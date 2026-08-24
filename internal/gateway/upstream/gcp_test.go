package upstream_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/upstream"
)

// A service-account key file, generated per test so that each one gets a
// distinct credential -- the token cache is keyed on the credential, so tests
// sharing a key would share cached tokens and stop measuring what they claim
// to.
func serviceAccountJSON(t *testing.T, tokenURI string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	doc, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "a-project",
		"private_key_id": "kid-1",
		"private_key":    string(pemKey),
		"client_email":   "robot@a-project.iam.gserviceaccount.com",
		"token_uri":      tokenURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(doc)
}

// tokenServer stands in for the token endpoint and counts how often it is
// asked. The count is the whole point of these tests: "does it work once" and
// "does it keep working" are different questions, and only the second one is
// hard.
func tokenServer(t *testing.T, expiresIn int) (url string, calls *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := n.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The exchange has to be the two-legged assertion flow, or what is
		// being cached is not a service-account token at all.
		if got := r.Form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, "grant_type = "+got, http.StatusBadRequest)
			return
		}
		if r.Form.Get("assertion") == "" {
			http.Error(w, "no assertion", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"tok-%d","token_type":"Bearer","expires_in":%d}`, i, expiresIn)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

func present(t *testing.T, secret string, req *http.Request) error {
	t.Helper()
	return upstream.Present(context.Background(), upstream.Presentation{
		Mode: catalog.AuthGCPServiceAccount, Secret: secret,
		HTTP: req, Payload: []byte(`{}`), PayloadReadable: true,
	})
}

func newRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://up.test/v1/x", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// The key is exchanged for a token and the token goes out as a bearer.
func TestServiceAccountKeyBecomesABearerToken(t *testing.T) {
	uri, calls := tokenServer(t, 3600)
	req := newRequest(t)
	if err := present(t, serviceAccountJSON(t, uri), req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-1" {
		t.Fatalf("Authorization = %q", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("token endpoint called %d times, want 1", calls.Load())
	}
}

// A live token is reused across requests.
//
// Without this, every forwarded request would carry a round trip to the token
// endpoint in front of it, and the endpoint's own quota would be spent on
// re-fetching something that was already valid.
func TestALiveTokenIsReusedAcrossRequests(t *testing.T) {
	uri, calls := tokenServer(t, 3600)
	secret := serviceAccountJSON(t, uri)
	for i := range 5 {
		req := newRequest(t)
		if err := present(t, secret, req); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Fatalf("request %d: Authorization = %q, want the first token reused", i, got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("token endpoint called %d times for five requests, want 1", calls.Load())
	}
}

// An expiring token is exchanged again.
//
// This is the assertion the whole mode exists for. A token lasts an hour, so a
// build that fetches once and never again passes every functional test anyone
// runs on the day it ships, and starts answering 401 on every request an hour
// later -- with nothing in the change that caused it pointing at the clock.
func TestAnExpiringTokenIsRefreshed(t *testing.T) {
	// One second, against a source that treats a token as spent shortly before
	// its stated expiry: every call is therefore due for renewal.
	uri, calls := tokenServer(t, 1)
	secret := serviceAccountJSON(t, uri)

	req1 := newRequest(t)
	if err := present(t, secret, req1); err != nil {
		t.Fatal(err)
	}
	req2 := newRequest(t)
	if err := present(t, secret, req2); err != nil {
		t.Fatal(err)
	}

	first, second := req1.Header.Get("Authorization"), req2.Header.Get("Authorization")
	if first == second {
		t.Fatalf("an expired token was reused: both requests carried %q", first)
	}
	if calls.Load() != 2 {
		t.Fatalf("token endpoint called %d times, want 2", calls.Load())
	}
}

// A credential of the wrong shape is refused with a message that names the
// shape. The likeliest mistake here is pasting a different key file from the
// same console, and "invalid JSON" would be a lie about what happened.
func TestServiceAccountCredentialShapeIsChecked(t *testing.T) {
	for _, tc := range []struct{ name, secret, want string }{
		{"a bare token", "ya29.a0AfH6", "not valid JSON"},
		{
			"another kind of key file",
			`{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`,
			`"authorized_user"`,
		},
		{"a key with no private key", `{"type":"service_account","client_email":"a@b"}`, "private_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := present(t, tc.secret, newRequest(t))
			if err == nil {
				t.Fatal("this credential should have been refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say what is wrong: %v", err)
			}
		})
	}
}

// A token endpoint that refuses produces an error, not a request that goes out
// with no credential and comes back 401 blaming the provider.
func TestATokenEndpointFailureIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	req := newRequest(t)
	err := present(t, serviceAccountJSON(t, srv.URL), req)
	if err == nil {
		t.Fatal("a refused exchange must not produce a request that looks authenticated")
	}
	if req.Header.Get("Authorization") != "" {
		t.Errorf("no credential should have been written: %q", req.Header.Get("Authorization"))
	}
}
