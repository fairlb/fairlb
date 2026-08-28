package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/catalog/catalogtest"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// seedVideoModel creates a video-plane provider, a per-second priced model and
// one route carrying a capability envelope.
func (f *pipeFixture) seedVideoModel(t *testing.T, slug string, envelope string, nanoPerSecond int64) pgtype.UUID {
	return f.seedVideoModelAs(t, slug, envelope, "second", nanoPerSecond, "paid")
}

// seedVideoModelVendor seeds a model on a named vendor, for the cases where
// which vendor serves it is the point.
func (f *pipeFixture) seedVideoModelVendor(
	t *testing.T, vendor, slug, envelope string, nanoPerSecond int64,
) pgtype.UUID {
	return f.seedVideoModelFull(t, vendor, slug, envelope, "second", nanoPerSecond, "paid")
}

func (f *pipeFixture) seedVideoModelAs(
	t *testing.T, slug, envelope, unit string, nanoPerUnit int64, billingMode string,
) pgtype.UUID {
	return f.seedVideoModelFull(t, "google", slug, envelope, unit, nanoPerUnit, billingMode)
}

func (f *pipeFixture) seedVideoModelFull(
	t *testing.T, vendor, slug, envelope, unit string, nanoPerUnit int64, billingMode string,
) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	var provID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ('p-video-'||$2, $2, ARRAY['video'], $1) RETURNING id`,
		f.upstream.URL, vendor).Scan(&provID); err != nil {
		t.Fatal(err)
	}
	sealed, err := f.box.Seal([]byte("sk-upstream-secret"), provID.Bytes[:])
	if err != nil {
		t.Fatal(err)
	}
	var keyID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO provider_keys (provider_id, name, secret_enc) VALUES ($1, 'k', $2) RETURNING id`,
		provID, sealed).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	// The credential's associated data is its own row id, so it is inserted
	// first and the ciphertext written back afterwards.
	resealed, err := f.box.Seal([]byte("sk-upstream-secret"), keyID.Bytes[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE provider_keys SET secret_enc = $2 WHERE id = $1`, keyID, resealed); err != nil {
		t.Fatal(err)
	}

	var modelID pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO models (slug) VALUES ($1) RETURNING id`, slug).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	// A per-second model: four zero token buckets and pricing_family = units.
	// This row is unrepresentable before ADR-0220 relaxed the completeness
	// constraint, so the fixture is itself part of what is under test.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model_pricing (model_id, billing_mode, pricing_family,
			upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
			upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
			multiplier_bps, source_name)
		VALUES ($1, $2, 'units', 0, 0, 0, 0, 10000, 'test-fixture')`, modelID, billingMode); err != nil {
		t.Fatal(err)
	}
	if nanoPerUnit > 0 {
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO model_price_unit_rates (model_id, unit, nano_per_unit) VALUES ($1, $2, $3)`,
			modelID, unit, nanoPerUnit); err != nil {
			t.Fatal(err)
		}
	}
	routeID := catalogtest.SeedRoute(t, f.pool, modelID, provID, "veo-3.1", "video")
	if _, err := f.pool.Exec(ctx,
		`UPDATE model_routes SET video_envelope = $2::jsonb WHERE id = $1`, routeID, envelope); err != nil {
		t.Fatal(err)
	}
	return modelID
}

const klingEnvelope = `{"durations_seconds":[5,10],"resolutions":["720p","1080p"],
	"aspect_ratios":["16:9"],"audio":"never","max_n":1,"cancel":"queued_only",
	"supports_negative_prompt":true,"max_job_seconds":900}`

const veoEnvelope = `{"durations_seconds":[4,6,8],"resolutions":["720p","1080p"],
	"supports_negative_prompt":true,
	"aspect_ratios":["16:9"],"audio":"optional","max_n":1,"cancel":"never","max_job_seconds":900}`

func (f *pipeFixture) videoRouter(t *testing.T) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	f.pipeline.MountVideos(r)
	return r
}

func postVideo(t *testing.T, h http.Handler, key, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/videos", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "idem-"+t.Name())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	return rec, payload
}

// errorCode digs the code out of whichever envelope the surface rendered.
func errorCode(payload map[string]any) string {
	e, ok := payload["error"].(map[string]any)
	if !ok {
		return ""
	}
	if c, ok := e["code"].(string); ok {
		return c
	}
	return ""
}

// The whole point of the refusal ordering: everything that can say no happens
// before anything that costs money. These three requests are refused at three
// different stages and none of them takes a hold.
func TestVideoAdmissionRefusesBeforeAnyHold(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no video request may reach an upstream during admission")
		w.WriteHeader(500)
	})
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)
	h := f.videoRouter(t)

	for _, tc := range []struct {
		name, body, wantCode string
		wantStatus           int
	}{
		{
			name:       "a parameter outside the normalised set",
			body:       `{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8,"style":"anime"}`,
			wantCode:   "gateway.video_params_unsupported",
			wantStatus: 400,
		},
		{
			name:       "a duration outside the envelope",
			body:       `{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":12}`,
			wantCode:   "gateway.video_params_unsupported",
			wantStatus: 400,
		},
		{
			name:       "a resolution outside the envelope",
			body:       `{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8,"resolution":"4k"}`,
			wantCode:   "gateway.video_params_unsupported",
			wantStatus: 400,
		},
		{
			name:       "an unknown model",
			body:       `{"model":"google/nope","prompt":"a cat","duration_seconds":8}`,
			wantCode:   "gateway.model_not_found",
			wantStatus: 404,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, payload := postVideo(t, h, plaintext, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			if got := errorCode(payload); got != tc.wantCode {
				t.Fatalf("error code %q, want %q (body %s)", got, tc.wantCode, rec.Body)
			}
			if holds, _, settles := f.settler.Counts(); holds != 0 || settles != 0 {
				t.Fatalf("a refused video request moved money: %d holds, %d settlements; "+
					"nothing above the submit may cost the caller anything", holds, settles)
			}
		})
	}
}

// The refusal must say what would have been accepted. A caller on a per-second
// plane cannot afford to find the admissible set by trial.
func TestVideoRefusalNamesTheAdmissibleSet(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	_, payload := postVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":12}`)
	msg, _ := payload["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "4, 6, 8") {
		t.Fatalf("the refusal must list what the model accepts, got %q", msg)
	}
}

// A `units` model whose rate rows are missing is the invariant no CHECK can
// hold. It must fail closed rather than serve a video for nothing.
func TestVideoModelWithNoUnitRateIsRefusedAsUnpriced(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 0) // priced as units, no unit rows

	rec, payload := postVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`)
	if rec.Code != 503 || errorCode(payload) != "gateway.model_unpriced" {
		t.Fatalf("an unpriced unit model must be a 503, got %d %s", rec.Code, rec.Body)
	}
}

// An admitted request takes a hold for the exact charge and becomes a job.
func TestAdmittedVideoRequestBecomesAJob(t *testing.T) {
	f := newPipeFixture(t, veoSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	rec, payload := postVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8,"resolution":"720p","aspect_ratio":"16:9"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("an in-envelope request should have become a job, got %d %s", rec.Code, rec.Body)
	}
	if payload["id"] == nil || payload["status"] != "in_progress" {
		t.Fatalf("the job was not returned to the caller: %v", payload)
	}
	// 8s at $0.40/s, held exactly, before the upstream was called.
	if holds := f.settler.Holds; len(holds) != 1 || holds[0].AmountNano != 3_200_000_000 {
		t.Fatalf("held %+v, want one hold of 3200000000 nano", holds)
	}
}

// veoSubmitOnly answers the create call and nothing else; a test that only
// cares about admission does not need a whole lifecycle.
func veoSubmitOnly(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ":predictLongRunning") {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"models/veo/operations/op-1"}`))
	}
}

// An unrecognised parameter is refused over HTTP, before admission has resolved
// the model and before anything is reserved.
//
// This replaced two tests about vendor_options. That field was the one door in
// the closed set, and it is gone: a knob belonging to one upstream is reached on
// that vendor's own compatibility surface, where it is a first-class parameter
// rather than a wrapped one. What has to stay true is the wall -- a parameter
// this gateway cannot price must never be dropped silently, because a dropped
// parameter produces a clip the caller did not ask for and is billed for.
func TestAnUnrecognisedParameterIsRefusedBeforeAnythingIsHeld(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)
	h := f.videoRouter(t)

	for _, field := range []string{"vendor_options", "personGeneration", "camera_control"} {
		rec, payload := postVideo(t, h, plaintext,
			`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8,"`+field+`":true}`)
		if rec.Code != 400 || errorCode(payload) != "gateway.video_params_unsupported" {
			t.Fatalf("%s: an unrecognised parameter must be refused, got %d %s", field, rec.Code, rec.Body)
		}
		msg, _ := payload["error"].(map[string]any)["message"].(string)
		if !strings.Contains(msg, field) {
			t.Errorf("%s: the refusal must name the offending field, got %q", field, msg)
		}
	}
	if holds, _, _ := f.settler.Counts(); holds != 0 {
		t.Fatal("a refused request took a hold")
	}
}

// A model priced per generation must be priced, not answered "unpriced". The
// unit is read from the rate card; hardcoding seconds at the call site made
// every per-call model 503 on every request and page the operator.
func TestPerCallPricedModelIsServiceable(t *testing.T) {
	f := newPipeFixture(t, veoSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModelAs(t, "google/kling-like", veoEnvelope, "call", 250_000_000, "paid")

	rec, payload := postVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/kling-like","prompt":"a cat","duration_seconds":8}`)
	if got := errorCode(payload); got == "gateway.model_unpriced" {
		t.Fatalf("a per-call model reported itself unpriced: %v", payload)
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a per-call model should have priced and submitted, got %d %v", rec.Code, payload)
	}
	// One generation, not eight seconds of one.
	if holds := f.settler.Holds; len(holds) != 1 || holds[0].AmountNano != 250_000_000 {
		t.Fatalf("held %+v, want one hold of 250000000 nano for a single generation", holds)
	}
}

// Submitting is a paid create. Without a key a retry is charged twice, and
// nothing else on this path can tell one attempt from another -- request_id is
// minted per attempt, so it is unique by construction.
func TestSubmitRequiresAnIdempotencyKey(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	req := httptest.NewRequest(http.MethodPost, "/videos",
		strings.NewReader(`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`))
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	f.videoRouter(t).ServeHTTP(rec, req)

	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if rec.Code != 400 {
		t.Fatalf("a submit with no idempotency key must be refused, got %d %s", rec.Code, rec.Body)
	}
	// The refusal has to be in this plane's error shape, not problem+json --
	// which is one of the reasons the shared middleware could not carry this.
	if errorCode(payload) != "gateway.invalid_request" {
		t.Fatalf("the refusal must render in the OpenAI error shape, got %s", rec.Body)
	}
}

// A refusal that leaves no usage row is invisible to the org, to the metrics
// and to support -- and envelope refusals are the ones a video caller hits most.
func TestEnvelopeRefusalIsRecordedInTheUsageLog(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	postVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":12}`)

	var rows int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM usage_logs WHERE org_id = $1 AND surface = 'video'`, org).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("an envelope refusal left no usage row; it is invisible to the organization and to support")
	}
	if holds, _, settles := f.settler.Counts(); holds != 0 || settles != 0 {
		t.Fatalf("a recorded refusal must still not move money: %d holds, %d settlements", holds, settles)
	}
}

// A free model stops the charge, never the cost. Passing the free view of the
// rate card as both list and cost zeroed the margin for every free job.
func TestFreeVideoModelStillRecordsWhatItCost(t *testing.T) {
	f := newPipeFixture(t, veoSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModelAs(t, "google/veo-free", veoEnvelope, "second", 400_000_000, "free")

	// Submitting at all proves the quote succeeded; the cost split itself is
	// asserted on the pricing types, which is where the arithmetic is.
	rec, payload := postVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-free","prompt":"a cat","duration_seconds":8}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a free video model should have priced and submitted, got %d %v", rec.Code, payload)
	}
	if holds := f.settler.Holds; len(holds) != 1 || holds[0].AmountNano != 0 {
		t.Fatalf("a free model held %+v; the charge is zero", holds)
	}
}

// The surface and the price row must agree about how a model is billed.
//
// Without the cross-check, the units arm of the pricedness test exempted a
// unit-priced model on *every* surface -- so the same model reached on a token
// surface resolved with an all-zero rate card and billed nothing at all.
func TestUnitPricedModelIsNotReachableOnATokenSurface(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a unit-priced model must not reach an upstream through a token surface")
		w.WriteHeader(500)
	})
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 1_000_000_000)
	modelID := f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	// The misconfiguration this guards: the same unit-priced model also wired
	// to a provider that speaks a token protocol.
	var chatProvider pgtype.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO providers (slug, vendor, protocols, base_url)
		 VALUES ('p-chat', 'openai', ARRAY['openai'], $1) RETURNING id`, f.upstream.URL).Scan(&chatProvider); err != nil {
		t.Fatal(err)
	}
	catalogtest.SeedRoute(t, f.pool, modelID, chatProvider, "veo-3.1", "chat")

	_, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceChat, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: "/v1/chat/completions",
		Body:         []byte(`{"model":"google/veo-3.1","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext,
	})
	if gerr == nil {
		t.Fatal("a unit-priced model served a chat completion; its four token rates are zero, so it billed nothing")
	}
	if gerr.Code != "gateway.model_not_found" {
		t.Fatalf("a model priced one way and called the other is not available there; got %q", gerr.Code)
	}
}

// Capability discovery is the half that makes the normalised contract usable:
// on a plane where every attempt is a real charge, finding the admissible set
// by trial is not a way to learn anything.
func TestVideoModelsPublishesTheEnvelopeAndTheRates(t *testing.T) {
	f := newPipeFixture(t, veoSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	req := httptest.NewRequest(http.MethodGet, "/videos/models", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	f.videoRouter(t).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var payload struct {
		Data []struct {
			ID               string   `json:"id"`
			DurationsSeconds []int    `json:"durations_seconds"`
			Resolutions      []string `json:"resolutions"`
			Cancel           string   `json:"cancel"`
			Pricing          []struct {
				Unit        string `json:"unit"`
				NanoPerUnit int64  `json:"nano_per_unit"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 || payload.Data[0].ID != "google/veo-3.1" {
		t.Fatalf("catalog: %s", rec.Body)
	}
	m := payload.Data[0]
	// Exactly what the submit path validates against, so "listed here" and
	// "accepted there" cannot come apart.
	if len(m.DurationsSeconds) != 3 || m.DurationsSeconds[0] != 4 {
		t.Errorf("durations %v, want the route's declared set", m.DurationsSeconds)
	}
	if m.Cancel != "never" {
		t.Errorf("cancel %q; a client needs to know before it builds a stop button", m.Cancel)
	}
	if len(m.Pricing) != 1 || m.Pricing[0].NanoPerUnit != 400_000_000 || m.Pricing[0].Unit != "second" {
		t.Errorf("pricing %+v; the cost of a clip must be knowable before it is asked for", m.Pricing)
	}
}

// A model nobody has described is not listed. An empty envelope published as
// though it were a capability set reads as "accepts anything", which is the
// opposite of what it means.
func TestVideoModelsOmitsAModelWithNoDeclaredEnvelope(t *testing.T) {
	f := newPipeFixture(t, veoSubmitOnly(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-blank", `{}`, 400_000_000)

	req := httptest.NewRequest(http.MethodGet, "/videos/models", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	f.videoRouter(t).ServeHTTP(rec, req)

	var payload struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if len(payload.Data) != 0 {
		t.Fatalf("a model with no declared envelope was published: %s", rec.Body)
	}
}

// An artifact lives wherever the upstream put it. Joining that absolute address
// onto the provider's base URL produced a path that exists nowhere, so both the
// capture and the no-custody content path fetched 404s.
func TestArtifactURLIsUsedAsAnAddressNotAPath(t *testing.T) {
	var fetched []string
	f := newPipeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fetched = append(fetched, r.URL.Path)
		if strings.Contains(r.URL.Path, ":predictLongRunning") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"models/veo/operations/op-1"}`))
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("MP4BYTES"))
	})
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	rec, payload := postVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body)
	}
	id, _ := payload["id"].(string)

	// Complete the job with an artifact whose address is a path the provider
	// base URL does not contain.
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE gateway_async_jobs
		    SET status='completed', settlement_state='settled', terminal_at=now(),
		        upstream_artifact_ref = $2
		  WHERE org_id = $1`, org, f.upstream.URL+"/artifacts/final.mp4"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/videos/"+id+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	out := httptest.NewRecorder()
	f.videoRouter(t).ServeHTTP(out, req)
	if out.Code != 200 || out.Body.String() != "MP4BYTES" {
		t.Fatalf("content returned %d %q; the artifact address was not used verbatim",
			out.Code, out.Body.String())
	}
	last := fetched[len(fetched)-1]
	if last != "/artifacts/final.mp4" {
		t.Fatalf("the upstream was asked for %q, want /artifacts/final.mp4 -- an absolute "+
			"artifact address must not be joined onto the provider's base URL", last)
	}
}

// A presigned artifact link carries its own authorisation. Attaching ours would
// hand the upstream API key to whatever host the link names.
func TestArtifactFetchHonoursTheMappersCredentialDecision(t *testing.T) {
	var sawAuth []bool
	f := newPipeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "text2video") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_id":"t-1","task_status":"submitted"}}`))
			return
		}
		sawAuth = append(sawAuth, r.Header.Get("Authorization") != "")
		_, _ = w.Write([]byte("MP4BYTES"))
	})
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	// A vendor whose artifact link is presigned and needs no credential.
	f.seedVideoModelVendor(t, "kuaishou", "kuaishou/kling-v2", klingEnvelope, 400_000_000)

	rec, payload := postVideo(t, f.videoRouter(t), plaintext,
		`{"model":"kuaishou/kling-v2","prompt":"a cat","duration_seconds":5}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body)
	}
	id, _ := payload["id"].(string)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE gateway_async_jobs SET status='completed', settlement_state='settled',
		        terminal_at=now(), upstream_artifact_ref=$2 WHERE org_id=$1`,
		org, f.upstream.URL+"/cdn/final.mp4"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/videos/"+id+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	out := httptest.NewRecorder()
	f.videoRouter(t).ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("content: %d %s", out.Code, out.Body)
	}
	if len(sawAuth) == 0 {
		t.Fatal("the artifact was never fetched")
	}
	if sawAuth[len(sawAuth)-1] {
		t.Fatal("the upstream credential was attached to a presigned artifact link; " +
			"on a real deployment that sends the API key to a third-party CDN")
	}
}
