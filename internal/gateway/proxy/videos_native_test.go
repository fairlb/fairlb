package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/fairlb/fairlb/access/apikeys"
)

// The compatibility surfaces, exercised the way a switching client reaches
// them: that vendor's own path, that vendor's own body, that vendor's own
// credential header, and nothing else changed.

func (f *pipeFixture) nativeRouter(t *testing.T) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	f.pipeline.MountVideoNative(r)
	return r
}

// klingSubmitOnly is this vendor's upstream, answering its create call.
func klingSubmitOnly(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/videos/") {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"task_id":"t-1","task_status":"submitted"}}`))
	}
}

// seedKlingModel wires a model this gateway serves under Kling's own upstream
// name, which is what a switching client will send.
func (f *pipeFixture) seedKlingModel(t *testing.T, upstreamID string) {
	t.Helper()
	f.seedVideoModelVendor(t, "kuaishou", "kuaishou/kling-v2", klingEnvelope, 280_000_000)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE model_routes SET provider_model_id = $1`, upstreamID); err != nil {
		t.Fatal(err)
	}
}

// The whole promise, in one test: a request written the way Kling's own
// documentation writes it, sent to this gateway with only the base URL changed,
// comes back in Kling's own shape -- and the job underneath is ours, with a
// hold taken against the caller's balance.
func TestAKlingClientSwitchesByChangingTheBaseURL(t *testing.T) {
	f := newPipeFixture(t, klingSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedKlingModel(t, "kling-v2-master")

	rec := postNative(t, f.nativeRouter(t), "/kuaishou/v1/videos/text2video", plaintext,
		`{"model_name":"kling-v2-master","prompt":"a cat on a wall","mode":"pro",
		  "duration":"10","aspect_ratio":"16:9","cfg_scale":0.5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var doc struct {
		Code int `json:"code"`
		Data struct {
			TaskID     string `json:"task_id"`
			TaskStatus string `json:"task_status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the answer is not this vendor's shape: %v (%s)", err, rec.Body)
	}
	if doc.Code != 0 {
		t.Fatalf("code %d; this vendor puts refusals in a non-zero code: %s", doc.Code, rec.Body)
	}
	// Our identifier, in the vendor's task id. Opaque to any client, which is
	// what makes the substitution invisible.
	if !strings.HasPrefix(doc.Data.TaskID, "vid_") {
		t.Errorf("task_id is %q; it must be this gateway's job id", doc.Data.TaskID)
	}
	if doc.Data.TaskStatus == "" {
		t.Error("no task_status; a client polls on this field")
	}
	// The money side is the same as the normalised plane's: one hold, for the
	// ten seconds the request asked for.
	if holds, _, _ := f.settler.Counts(); holds != 1 {
		t.Errorf("took %d holds, want 1", holds)
	}
}

// Each vendor's SDK sends the credential in the header that vendor documents.
// The gateway accepts all three positions already -- that is what makes
// /v1/messages a drop-in -- and the compatibility surfaces inherit it. A client
// that had to move its key to a different header would be changing code.
func TestTheCompatibilitySurfaceTakesTheCredentialWhereTheSDKPutsIt(t *testing.T) {
	f := newPipeFixture(t, klingSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedKlingModel(t, "kling-v2-master")
	h := f.nativeRouter(t)

	body := `{"model_name":"kling-v2-master","prompt":"a cat","mode":"std","duration":"5"}`
	for _, header := range []struct{ name, value string }{
		{"Authorization", "Bearer " + plaintext},
		{"x-api-key", plaintext},
		{"x-goog-api-key", plaintext},
	} {
		t.Run(header.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost,
				"/kuaishou/v1/videos/text2video", strings.NewReader(body+" "))
			req.Header.Set(header.name, header.value)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("a key sent in %s was not accepted: %s", header.name, rec.Body)
			}
		})
	}
}

// A path this vendor publishes and this gateway does not serve gets an answer
// in that vendor's error shape, naming the path and pointing somewhere useful.
// A bare 404 would read as "your SDK is broken" (ADR-0157).
func TestAnUnservedVendorPathIsRefusedInThatVendorsShape(t *testing.T) {
	f := newPipeFixture(t, klingSubmitOnly(t))
	rec := postNative(t, f.nativeRouter(t), "/kuaishou/v1/videos/lip-sync", "no-key", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body)
	}
	var doc struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the refusal is not this vendor's shape: %v (%s)", err, rec.Body)
	}
	if doc.Code == 0 {
		t.Error("this vendor spells a refusal with a non-zero code")
	}
	if !strings.Contains(doc.Message, "lip-sync") || !strings.Contains(doc.Message, "/v1/videos") {
		t.Errorf("the refusal must name the path and say where to go instead, got %q", doc.Message)
	}
}

// The caller sends the vendor's model name; this gateway resolves by catalog
// slug. A name nothing here is wired to is refused with that name in it, rather
// than with a slug the caller has never seen.
func TestAnUnwiredVendorModelNameIsRefusedByName(t *testing.T) {
	f := newPipeFixture(t, klingSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedKlingModel(t, "kling-v2-master")

	rec := postNative(t, f.nativeRouter(t), "/kuaishou/v1/videos/text2video", plaintext,
		`{"model_name":"kling-v9-imaginary","prompt":"a cat","mode":"std","duration":"5"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("an unwired model name was accepted: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "kling-v9-imaginary") {
		t.Errorf("the refusal must name what the caller sent, got %s", rec.Body)
	}
	if holds, _, _ := f.settler.Counts(); holds != 0 {
		t.Error("a refused request took a hold")
	}
}

// The envelope refuses on this surface exactly as it does on the normalised
// one, and for the same reason: a twelve-second request against a model that
// tops out at ten must be an error, never a shortened clip billed for twelve.
func TestTheEnvelopeStillRefusesOnTheCompatibilitySurface(t *testing.T) {
	f := newPipeFixture(t, klingSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedKlingModel(t, "kling-v2-master")

	rec := postNative(t, f.nativeRouter(t), "/kuaishou/v1/videos/text2video", plaintext,
		`{"model_name":"kling-v2-master","prompt":"a cat","mode":"std","duration":"12"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("a duration outside the envelope was accepted: %s", rec.Body)
	}
	if holds, _, _ := f.settler.Counts(); holds != 0 {
		t.Error("a refused request took a hold")
	}
}

func postNative(t *testing.T, h http.Handler, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The second surface, to prove the mount is driven off the registry rather than
// off one vendor's shape: a Seedance client's own path, its own body with the
// parameters inline in the prompt, and its own response shape back.
func TestASeedanceClientSwitchesByChangingTheBaseURL(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/contents/generations/tasks") {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cgt-1","status":"queued"}`))
	})
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModelVendor(t, "volcengine", "bytedance/seedance-1.5", seedanceEnvelope, 150_000_000)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE model_routes SET provider_model_id = 'seedance-1-5-pro'`); err != nil {
		t.Fatal(err)
	}

	rec := postNative(t, f.nativeRouter(t), "/volcengine/api/v3/contents/generations/tasks", plaintext,
		`{"model":"seedance-1-5-pro","content":[{"type":"text",
		  "text":"a cat on a wall --resolution 720p --duration 8"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var doc struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the answer is not this vendor's shape: %v (%s)", err, rec.Body)
	}
	if !strings.HasPrefix(doc.ID, "vid_") {
		t.Errorf("id is %q; it must be this gateway's job id", doc.ID)
	}
	if doc.Status == "" {
		t.Error("no status; a client polls on this field")
	}
	if holds, _, _ := f.settler.Counts(); holds != 1 {
		t.Errorf("took %d holds, want 1", holds)
	}
}

const seedanceEnvelope = `{"durations_seconds":[4,8,12],"resolutions":["480p","720p","1080p"],
	"aspect_ratios":["16:9"],"audio":"optional","max_n":1,"cancel":"never","max_job_seconds":900}`

// Veo's identifier is an operation *name* rather than a bare id, and its client
// GETs that name as a path. So the submit answer has to carry ours inside a
// name-shaped string, and the read route has to find it at the end of one --
// which is the one thing the second batch of surfaces asked the route
// declaration for.
func TestAVeoClientSubmitsAndPollsByOperationName(t *testing.T) {
	f := newPipeFixture(t, veoSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)
	h := f.nativeRouter(t)

	rec := postNative(t, h, "/google/v1beta/models/veo-3.1:predictLongRunning", plaintext,
		`{"instances":[{"prompt":"a cat on a wall"}],
		  "parameters":{"aspectRatio":"16:9","resolution":"720p","durationSeconds":"8",
		  "personGeneration":"allow_adult"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var op struct {
		Name string `json:"name"`
		Done bool   `json:"done"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
		t.Fatalf("the answer is not this vendor's shape: %v (%s)", err, rec.Body)
	}
	if !strings.HasPrefix(op.Name, "models/") || !strings.Contains(op.Name, "/operations/") {
		t.Fatalf("name is %q; a client GETs this as a path", op.Name)
	}

	// Exactly what that client does next: GET the name it was given.
	req := httptest.NewRequest(http.MethodGet, "/google/v1beta/"+op.Name, nil)
	req.Header.Set("x-goog-api-key", plaintext)
	poll := httptest.NewRecorder()
	h.ServeHTTP(poll, req)
	if poll.Code != http.StatusOK {
		t.Fatalf("polling the name it was handed answered %d: %s", poll.Code, poll.Body)
	}
	var read struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(poll.Body.Bytes(), &read); err != nil || read.Name != op.Name {
		t.Errorf("polling returned %q for %q", read.Name, op.Name)
	}
}

// MiniMax's file route takes the integer alias, and an alias belonging to
// another organization has to be indistinguishable from one that never existed
// -- which matters more here than for a UUID, because an integer is guessable.
func TestTheMinimaxFileRouteTakesTheIntegerAliasAndIsScopedToTheCaller(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task_id":"t-1","base_resp":{"status_code":0,"status_msg":"ok"}}`))
	})
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModelVendor(t, "minimax", "minimax/hailuo-2.3", hailuoEnvelope, 80_000_000)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE model_routes SET provider_model_id = 'MiniMax-Hailuo-2.3'`); err != nil {
		t.Fatal(err)
	}
	h := f.nativeRouter(t)

	rec := postNative(t, h, "/minimax/v1/video_generation", plaintext,
		`{"model":"MiniMax-Hailuo-2.3","prompt":"a cat","duration":6,"resolution":"1080P"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	var alias int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT native_alias FROM gateway_async_jobs`).Scan(&alias); err != nil {
		t.Fatal(err)
	}
	if alias <= 0 {
		t.Fatalf("the job has no integer alias")
	}
	// The file route answers a record for a finished job and refuses for one
	// that produced nothing, so the job is driven to a terminal success first.
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE gateway_async_jobs SET status='completed', terminal_at=now(),
		        upstream_artifact_ref='https://upstream.test/v.mp4'`); err != nil {
		t.Fatal(err)
	}

	get := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			"/minimax/v1/files/retrieve?file_id="+strconv.FormatInt(alias, 10), nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if own := get(plaintext); own.Code != http.StatusOK {
		t.Fatalf("the owner could not read their own file record: %d %s", own.Code, own.Body)
	}

	// A second organization, asking for the same alias.
	otherKey, _, otherOrg := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, otherOrg, 1_000_000_000)
	other := get(otherKey)
	if other.Code == http.StatusOK {
		t.Fatal("another organization read a job by guessing its integer alias")
	}
}

const hailuoEnvelope = `{"durations_seconds":[6,10],"resolutions":["512p","768p","1080p"],
	"audio":"never","max_n":1,"cancel":"never","max_job_seconds":900,
	"supports_image_to_video":true}`

// A submit is authenticated before the vendor's model name is resolved against
// the catalogue.
//
// The normalised plane can decode first because decoding is pure. This surface
// cannot: resolving the name is a database query, and running it for an
// unauthenticated caller would both spend a query on them and answer "is this
// upstream name wired here" to anyone who asks. The observable property is that
// a bad key gets 401 rather than a verdict about the catalogue.
func TestACompatibilitySubmitIsAuthenticatedBeforeItTouchesTheCatalogue(t *testing.T) {
	f := newPipeFixture(t, klingSubmitOnly(t))
	f.seedKlingModel(t, "kling-v2-master")

	for _, name := range []string{"kling-v2-master", "kling-v9-imaginary"} {
		rec := postNative(t, f.nativeRouter(t), "/kuaishou/v1/videos/text2video", "not-a-key",
			`{"model_name":"`+name+`","prompt":"a cat","mode":"std","duration":"5"}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401: %s", name, rec.Code, rec.Body)
		}
		// A wired name and an unwired one must be indistinguishable to a caller
		// with no key, or the refusal itself is the answer they were after.
		if strings.Contains(rec.Body.String(), name) {
			t.Errorf("%s: the refusal names the model, which tells an unauthenticated "+
				"caller what this deployment wires: %s", name, rec.Body)
		}
	}
}

// A file record is not answered for a job that produced nothing. A success
// envelope with an empty download URL sends a client's retry loop after a file
// that will never exist.
func TestTheMinimaxFileRouteRefusesAJobWithNoVideo(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task_id":"t-1","base_resp":{"status_code":0,"status_msg":"ok"}}`))
	})
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModelVendor(t, "minimax", "minimax/hailuo-2.3", hailuoEnvelope, 80_000_000)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE model_routes SET provider_model_id = 'MiniMax-Hailuo-2.3'`); err != nil {
		t.Fatal(err)
	}
	h := f.nativeRouter(t)

	postNative(t, h, "/minimax/v1/video_generation", plaintext,
		`{"model":"MiniMax-Hailuo-2.3","prompt":"a cat","duration":6,"resolution":"1080P"}`)
	var alias int64
	if err := f.pool.QueryRow(context.Background(),
		`UPDATE gateway_async_jobs SET status='failed', terminal_at=now(),
		        error_message='content refused' RETURNING native_alias`).Scan(&alias); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/minimax/v1/files/retrieve?file_id="+strconv.FormatInt(alias, 10), nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("a failed job was handed a file record: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"status_code":0`) {
		t.Errorf("the refusal claims success in this vendor's envelope: %s", rec.Body)
	}
}

// A request that arrived in one vendor's shape must not be served by another
// vendor's route.
//
// The surface names its vendor implicitly, and that naming has to narrow the
// candidates -- the request carries that vendor's own parameters, and another
// upstream would be sent knobs that mean nothing there. The reachable way in is
// a slug-shaped model name, which resolveNativeModel takes at face value so
// that a client can send back what it was given.
func TestACompatibilitySurfaceCannotBorrowAnotherVendorsRoute(t *testing.T) {
	f := newPipeFixture(t, veoSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	// Wired on Google only. The request below arrives on Kling's surface.
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	rec := postNative(t, f.nativeRouter(t), "/kuaishou/v1/videos/text2video", plaintext,
		`{"model_name":"google/veo-3.1","prompt":"a cat","mode":"std","duration":"5",
		  "camera_control":{"type":"simple"}}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("a Kling-shaped request was served by a Google route: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "kuaishou") {
		t.Errorf("the refusal must say which upstream does not serve it, got %s", rec.Body)
	}
	if holds, _, _ := f.settler.Counts(); holds != 0 {
		t.Error("a refused request took a hold")
	}
}
