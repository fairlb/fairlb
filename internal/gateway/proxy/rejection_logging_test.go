package proxy_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// A request refused at a gate still has to leave a trace.
//
// Failures inside prepare() used to be entirely silent: nothing in the usage
// log, nothing in the application log. Whether the same logical error was
// recorded therefore depended on which part of the pipeline caught it, and a
// support lookup by request id would find nothing.
//
// Every gate is checked here rather than just one: they sit at different points
// in prepare, and testing one proves nothing about the others.
func TestRejectedRequestsReachUsageLog(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scopes  []string
		setup   func(t *testing.T, f *pipeFixture, keyID pgtype.UUID)
		model   string
		wantErr string
	}{
		{
			// The first gate after authentication, and the one most worth
			// checking: the identity used to be assigned *after* the scope
			// check, so a scope failure could not even carry the org.
			name:    "insufficient scope",
			scopes:  []string{"management:read"},
			setup:   func(*testing.T, *pipeFixture, pgtype.UUID) {},
			model:   "openai/gate-probe",
			wantErr: errcode.GatewayInsufficientScope,
		},
		{
			name:    "model does not exist",
			setup:   func(*testing.T, *pipeFixture, pgtype.UUID) {},
			model:   "openai/does-not-exist",
			wantErr: errcode.GatewayModelNotFound,
		},
		{
			// Same code as the previous case but a *different gate* -- the
			// per-key guard rather than catalogue resolution. Testing one
			// proves nothing about the other.
			name: "excluded by the key model allowlist",
			setup: func(t *testing.T, f *pipeFixture, keyID pgtype.UUID) {
				if _, err := f.pool.Exec(context.Background(),
					`UPDATE api_keys SET allow_all_models = false, allowed_models = ARRAY['openai/other'] WHERE id = $1`,
					keyID); err != nil {
					t.Fatal(err)
				}
			},
			model:   "openai/gate-probe",
			wantErr: errcode.GatewayModelNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			})
			ctx := context.Background()
			plaintext, row, org := f.seedKey(t, apikeys.CreateInput{Scopes: tc.scopes})
			f.topup(t, org, 1_000_000_000)
			f.seedCatalog(t, "openai", "openai/gate-probe", "up", []string{"chat"})
			tc.setup(t, f, row.ID)

			_, gerr := f.pipeline.Run(ctx, proxy.Request{
				Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
				UpstreamPath: "/v1/chat/completions",
				Body:         []byte(`{"model":"` + tc.model + `","messages":[{"role":"user","content":"hi"}]}`),
				Credential:   plaintext, RequestID: "rej-" + tc.name,
			})
			if gerr == nil {
				t.Fatal("this request should have been refused")
			}
			if gerr.Code != tc.wantErr {
				t.Fatalf("error code = %s, want %s", gerr.Code, tc.wantErr)
			}

			// Recorded: a support lookup by request id must find it, with the
			// same error code the customer received.
			var status, code string
			if err := f.pool.QueryRow(ctx,
				`SELECT status, error_code FROM usage_logs WHERE request_id = $1`,
				"rej-"+tc.name).Scan(&status, &code); err != nil {
				t.Fatalf("the refused request left no trace, so a support lookup would find nothing: %v", err)
			}
			if code != tc.wantErr {
				t.Errorf("recorded error code = %s, which differs from the %s returned to the customer", code, tc.wantErr)
			}
			if status != "client_error" {
				t.Errorf("status = %q; a refusal at a gate belongs under client_error", status)
			}

			// And no money may be taken along the way: a refused request costs
			// nothing.
			var charged int64
			if err := f.pool.QueryRow(ctx,
				`SELECT charged_nano FROM usage_logs WHERE request_id = $1`,
				"rej-"+tc.name).Scan(&charged); err != nil {
				t.Fatal(err)
			}
			if charged != 0 {
				t.Errorf("a refused request must not be charged, charged_nano = %d", charged)
			}
		})
	}
}

// An authentication failure has no org, so it cannot go into the usage log
// where the column is NOT NULL -- but that must *not* make the pipeline error
// either; it only reaches the metrics. This pins down "skip quietly, do not
// blow up".
func TestAuthFailureSkipsUsageLogWithoutError(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {})
	ctx := context.Background()

	_, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"m","messages":[]}`),
		Credential:   "sk-flb-v1-nonexistent", RequestID: "auth-fail",
	})
	if gerr == nil || gerr.Code != errcode.GatewayInvalidApiKey {
		t.Fatalf("expected invalid_api_key: %v", gerr)
	}

	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM usage_logs WHERE request_id = $1`, "auth-fail").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("an authentication failure has no org and can neither be nor should be recorded, got %d rows", n)
	}
}
