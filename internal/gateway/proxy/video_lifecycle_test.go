package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fairlb/fairlb/access/apikeys"
)

// veoUpstream fakes the long-running-operation shape: a submit that returns an
// operation name, then polls that report whatever the test sets.
type veoUpstream struct {
	pollBody atomic.Value // string
	submits  atomic.Int32
	polls    atomic.Int32
}

func (u *veoUpstream) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, ":predictLongRunning"):
			u.submits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"models/veo/operations/op-1"}`))
		case strings.Contains(r.URL.Path, "/operations/"):
			u.polls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			body, _ := u.pollBody.Load().(string)
			if body == "" {
				body = `{"name":"n","done":false}`
			}
			_, _ = w.Write([]byte(body))
		case strings.Contains(r.URL.Path, "v.mp4"):
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4BYTES"))
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(500)
		}
	}
}

func submitVideo(t *testing.T, h http.Handler, key, body, idem string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/videos", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", idem)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	return rec.Code, payload
}

// The whole happy path: an admitted request takes a hold for the exact charge,
// creates a job, and settles that exact amount when the upstream finishes.
func TestVideoJobSettlesTheExactAmountItHeld(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000) // $0.40/s

	code, payload := submitVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8,"resolution":"720p"}`, "idem-happy")
	if code != http.StatusAccepted {
		t.Fatalf("submit returned %d: %v", code, payload)
	}
	if payload["status"] != "in_progress" {
		t.Fatalf("a submitted job should be in progress: %v", payload)
	}
	// 8s at $0.40 = $3.20 exactly, held before the upstream was called.
	const want = 3_200_000_000
	holds := f.settler.Holds
	if len(holds) != 1 || holds[0].AmountNano != want {
		t.Fatalf("held %+v, want exactly one hold of %d nano -- the charge is exact on this plane", holds, want)
	}

	// The upstream finishes.
	up.pollBody.Store(`{"name":"n","done":true,"response":{"generateVideoResponse":` +
		`{"generatedSamples":[{"video":{"uri":"` + f.upstream.URL + `/v.mp4","mimeType":"video/mp4"}}]}}}`)
	f.runVideoScan(t)

	st, ok := f.settler.LastSettle()
	if !ok {
		t.Fatal("a completed video job was never settled")
	}
	if st.ActualNano != want {
		t.Fatalf("settled %d nano, want %d -- the hold is the charge on this plane", st.ActualNano, want)
	}

	var status string
	var charged int64
	if err := f.pool.QueryRow(ctx,
		`SELECT status, charged_nano FROM gateway_async_jobs WHERE org_id = $1`, org).
		Scan(&status, &charged); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || charged != want {
		t.Fatalf("job row: status=%q charged=%d", status, charged)
	}

	var logged int64
	var units int32
	var unit string
	if err := f.pool.QueryRow(ctx,
		`SELECT charged_nano, billed_units, billed_unit FROM usage_logs
		  WHERE org_id = $1 AND surface = 'video' AND status = 'ok'`, org).
		Scan(&logged, &units, &unit); err != nil {
		t.Fatalf("a settled video job must leave a usage row: %v", err)
	}
	if logged != want {
		t.Fatalf("usage row charged %d, want %d", logged, want)
	}
	// The quantity has to be a column rather than only a field inside the price
	// snapshot: "how many seconds did this organization generate" must be
	// summable, and a document cannot be summed.
	if units != 8 || unit != "second" {
		t.Fatalf("usage row recorded %d %q, want 8 seconds", units, unit)
	}
}

// Content-policy refusal is the common failure on this plane, not an edge case.
// It must void the hold and still leave a record of why.
func TestRefusedVideoJobIsVoidedAndStillExplainsItself(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	if code, payload := submitVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`, "idem-refused"); code != http.StatusAccepted {
		t.Fatalf("submit returned %d: %v", code, payload)
	}
	up.pollBody.Store(`{"name":"n","done":true,"error":{"code":3,"message":"blocked by policy","status":"INVALID_ARGUMENT"}}`)
	f.runVideoScan(t)

	if _, settled := f.settler.LastSettle(); settled {
		t.Fatal("a refused video job was settled; nothing was produced")
	}
	if _, voids, _ := f.settler.Counts(); voids == 0 {
		t.Fatal("a refused video job did not void its hold; the money stays reserved")
	}

	var status, errCode, errMessage string
	var charged int64
	if err := f.pool.QueryRow(ctx,
		`SELECT status, error_code, error_message, charged_nano FROM gateway_async_jobs WHERE org_id = $1`, org).
		Scan(&status, &errCode, &errMessage, &charged); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || charged != 0 {
		t.Fatalf("a refused job: status=%q charged=%d, want failed and 0", status, charged)
	}
	if !strings.Contains(errMessage, "blocked by policy") {
		t.Fatalf("the upstream's own reason must survive to the caller, got %q", errMessage)
	}

	// And it is visible where the organization looks.
	var rows int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM usage_logs WHERE org_id = $1 AND surface = 'video' AND charged_nano = 0`, org).
		Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("a refused video job left no usage row; the failure is invisible")
	}
}

// A retry of the same key returns the job that already exists rather than a
// second paid one.
func TestRetriedSubmitReturnsTheSameJob(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)
	h := f.videoRouter(t)
	body := `{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`

	_, first := submitVideo(t, h, plaintext, body, "idem-retry")
	_, second := submitVideo(t, h, plaintext, body, "idem-retry")
	if first["id"] != second["id"] {
		t.Fatalf("a retry created a second job: %v vs %v", first["id"], second["id"])
	}
	if n := up.submits.Load(); n != 1 {
		t.Fatalf("the upstream was asked to generate %d times for one logical submit", n)
	}
	if holds, _, _ := f.settler.Counts(); holds != 1 {
		t.Fatalf("a retry took %d holds; it must take one", holds)
	}
	var jobs int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM gateway_async_jobs WHERE org_id = $1`, org).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("%d job rows for one logical submit", jobs)
	}
}

// The same key with a different body must not replay somebody else's video.
func TestSameKeyDifferentRequestIsRefused(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)
	h := f.videoRouter(t)

	submitVideo(t, h, plaintext, `{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`, "idem-same")
	code, payload := submitVideo(t, h, plaintext,
		`{"model":"google/veo-3.1","prompt":"a dog","duration_seconds":8}`, "idem-same")
	if code != 400 {
		t.Fatalf("reusing a key for a different request returned %d: %v", code, payload)
	}
}

// A job the caller never polls still ends, and its money still moves. The
// reconciler owns that, not the caller.
func TestAJobNobodyPollsStillSettles(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	submitVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":4}`, "idem-nopoll")
	up.pollBody.Store(`{"name":"n","done":true,"response":{"generateVideoResponse":` +
		`{"generatedSamples":[{"video":{"uri":"` + f.upstream.URL + `/v.mp4"}}]}}}`)
	f.runVideoScan(t)

	var settlement string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT settlement_state FROM gateway_async_jobs WHERE org_id = $1`, org).Scan(&settlement); err != nil {
		t.Fatal(err)
	}
	if settlement != "settled" {
		t.Fatalf("settlement_state=%q; the reconciler must finish a job nobody watches", settlement)
	}
}

// A job whose reservation was reclaimed while it was still running settles late,
// through the one queue allowed to debit an expired reservation.
//
// This is also the path that proves a video usage row can be encoded for replay
// at all: the encoder refuses a row whose token dimensions are missing, and a
// video row has no token dimensions to report.
func TestAJobWhoseHoldWasReclaimedSettlesLateRatherThanSilently(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	submitVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`, "idem-orphan")

	// The reservation is gone: the sweeper reclaimed it while the upstream was
	// still working.
	if _, err := f.pool.Exec(ctx,
		`UPDATE gateway_async_jobs SET settlement_state = 'orphaned' WHERE org_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	up.pollBody.Store(`{"name":"n","done":true,"response":{"generateVideoResponse":` +
		`{"generatedSamples":[{"video":{"uri":"` + f.upstream.URL + `/v.mp4"}}]}}}`)
	f.runVideoScan(t)

	// It must not settle inline against a reservation that no longer exists.
	if _, settled := f.settler.LastSettle(); settled {
		t.Fatal("an orphaned job settled inline; only the late-debit path may charge an expired reservation")
	}
	var pending int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM gateway_unsettled WHERE request_id IN (
		     SELECT request_id FROM gateway_async_jobs WHERE org_id = $1)`, org).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending == 0 {
		t.Fatal("an orphaned job left nothing for the replay queue; the charge would be lost silently")
	}
}

// The credential is part of the pin. Without it the reconciler re-picks, and
// pickKey round-robins: the poll asks key B about a job that only exists under
// key A, gets a 404, and the finished job is expired and its charge voided.
func TestAJobRecordsTheCredentialItWasCreatedWith(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	submitVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`, "idem-cred")

	var providerKey, orgKey *string
	if err := f.pool.QueryRow(ctx,
		`SELECT provider_key_id::text, org_provider_key_id::text
		   FROM gateway_async_jobs WHERE org_id = $1`, org).Scan(&providerKey, &orgKey); err != nil {
		t.Fatal(err)
	}
	if providerKey == nil {
		t.Fatal("the job recorded no credential; every later poll would re-pick one and " +
			"ask the wrong account about this job")
	}
	if orgKey != nil {
		t.Fatal("a platform-served job recorded an organization credential")
	}
}

// Two concurrent submits under one key must produce one job, one upstream
// generation, and no stranded reservation.
//
// The handler's pre-check misses for both -- that is what "concurrent" means
// here -- so this is the path where the unique index is the only thing left,
// and where the losing attempt has already taken a reservation of its own.
//
// The collision is racy to provoke, so the test detects whether it actually
// happened (a losing attempt is visible as a second hold under one key) and
// says so rather than passing vacuously when it did not. A test that silently
// exercises nothing is worse than no test: it reports the branch as covered.
func TestConcurrentSubmitsUnderOneKeyGenerateOnceAndStrandNothing(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)
	h := f.videoRouter(t)
	body := `{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`

	collided := false
	for round := range 40 {
		key := fmt.Sprintf("idem-race-%d", round)
		holdsBefore, voidsBefore, _ := f.settler.Counts()
		submitsBefore := up.submits.Load()

		const racers = 6
		ids := make([]string, racers)
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i := range racers {
			wg.Go(func() {
				<-gate
				_, payload := submitVideo(t, h, plaintext, body, key)
				ids[i], _ = payload["id"].(string)
			})
		}
		close(gate)
		wg.Wait()

		holds, voids, _ := f.settler.Counts()
		roundHolds := holds - holdsBefore
		if roundHolds < 2 {
			continue // no collision this round; the pre-check absorbed the retries
		}
		collided = true

		if n := up.submits.Load() - submitsBefore; n != 1 {
			t.Fatalf("%d upstream generations for one logical submit; a losing racer "+
				"submitted again and that clip is billed by the vendor", n)
		}
		// Every reservation a losing attempt took is released here, not left
		// for the timeout sweeper hours later.
		if got := roundHolds - (voids - voidsBefore); got != 1 {
			t.Fatalf("%d holds and %d voids in this round: %d reservations stranded",
				roundHolds, voids-voidsBefore, got)
		}
		for i, id := range ids {
			if id != "" && id != ids[0] {
				t.Fatalf("racer %d got job %q, racer 0 got %q", i, id, ids[0])
			}
		}
		var rows int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM gateway_async_jobs WHERE org_id = $1 AND idempotency_key = $2`,
			org, key).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("%d job rows for one idempotency key", rows)
		}
		break
	}
	if !collided {
		t.Skip("no insert collision was provoked in 40 rounds; this machine serialises the " +
			"racers, so the conflict branch was not exercised")
	}
}

// Deleting a running job would strand its reservation and leave the upstream
// generating a clip nothing will reconcile.
func TestDeletingARunningJobIsRefused(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	_, payload := submitVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`, "idem-del")
	id, _ := payload["id"].(string)

	req := httptest.NewRequest(http.MethodDelete, "/videos/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	f.videoRouter(t).ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("deleting a running job returned %d: %s", rec.Code, rec.Body)
	}
	var rows int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM gateway_async_jobs WHERE org_id = $1`, org).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatal("a refused delete removed the row anyway, stranding its reservation")
	}
}

// Being terminal is not enough. A job whose charge has not settled or voided is
// the only row that points at its reservation, and deleting it puts that money
// out of reach: a `held` hold waits for the generic sweeper to void it, so a
// delivered video goes uncharged, and a `protected` one is never swept at all
// (ProtectHold pushes the expiry to infinity precisely so it cannot be) -- the
// customer's balance stays reserved with nothing left that could release it.
//
// It is also the row the operator's repair queue reads, so deleting it is how
// that queue loses the ability to see the problem.
func TestDeletingATerminalJobWhoseMoneyHasNotMovedIsRefused(t *testing.T) {
	for _, state := range []string{"held", "protected"} {
		t.Run(state, func(t *testing.T) {
			up := &veoUpstream{}
			f := newPipeFixture(t, up.handler(t))
			plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
			f.topup(t, org, 100_000_000_000)
			f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

			_, payload := submitVideo(t, f.videoRouter(t), plaintext,
				`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`, "idem-del-"+state)
			id, _ := payload["id"].(string)

			// Terminal to the caller, unmoved on the money -- the state the
			// schema describes as a settlement that failed and awaits replay.
			if _, err := f.pool.Exec(context.Background(),
				`UPDATE gateway_async_jobs
				    SET status = 'completed', terminal_at = now(), settlement_state = $2
				  WHERE org_id = $1`, org, state); err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodDelete, "/videos/"+id, nil)
			req.Header.Set("Authorization", "Bearer "+plaintext)
			rec := httptest.NewRecorder()
			f.videoRouter(t).ServeHTTP(rec, req)
			if rec.Code != 409 {
				t.Fatalf("deleting a %s job returned %d: %s", state, rec.Code, rec.Body)
			}
			var rows int
			if err := f.pool.QueryRow(context.Background(),
				`SELECT count(*) FROM gateway_async_jobs WHERE org_id = $1`, org).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 1 {
				t.Fatalf("a %s job was deleted anyway, stranding its reservation", state)
			}
		})
	}
}

// Once the money has moved there is nothing left to strand, so the row goes.
func TestDeletingASettledJobIsAllowed(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	_, payload := submitVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`, "idem-del-settled")
	id, _ := payload["id"].(string)
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE gateway_async_jobs
		    SET status = 'completed', terminal_at = now(), settlement_state = 'settled'
		  WHERE org_id = $1`, org); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/videos/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rec := httptest.NewRecorder()
	f.videoRouter(t).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("deleting a settled job returned %d: %s", rec.Code, rec.Body)
	}
	var rows int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM gateway_async_jobs WHERE org_id = $1`, org).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("a settled job survived its own deletion")
	}
}

// Retention may take the film. It may not take the row while the row is the only
// thing pointing at a reservation.
//
// This is the path the by-hand guard does not cover: `Delete` refuses such a job,
// but the sweep runs unattended on a timer, and deleting the row there would
// strand the hold exactly the same way -- `protected` is never swept at all, so
// nothing would ever release it -- while emptying the operator's repair queue by
// destroying its contents rather than by anyone repairing them.
func TestRetentionKeepsAnExpiredJobWhoseMoneyNeverMoved(t *testing.T) {
	for _, state := range []string{"held", "protected"} {
		t.Run(state, func(t *testing.T) {
			up := &veoUpstream{}
			f := newPipeFixture(t, up.handler(t))
			ctx := context.Background()
			plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
			f.topup(t, org, 100_000_000_000)
			f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

			submitVideo(t, f.videoRouter(t), plaintext,
				`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`, "idem-ret-"+state)

			// Terminal, retention passed, and the charge never landed.
			if _, err := f.pool.Exec(ctx,
				`UPDATE gateway_async_jobs
				    SET status='completed', terminal_at=now(), settlement_state=$2,
				        artifact_key='k/'||id::text, expires_at = now() - interval '1 hour'
				  WHERE org_id = $1`, org, state); err != nil {
				t.Fatal(err)
			}
			f.runVideoSweep(t)

			var rows int
			var artifact string
			if err := f.pool.QueryRow(ctx,
				`SELECT count(*), coalesce(max(artifact_key), '') FROM gateway_async_jobs WHERE org_id = $1`,
				org).Scan(&rows, &artifact); err != nil {
				t.Fatal(err)
			}
			if rows != 1 {
				t.Fatalf("retention deleted a %s job, stranding its reservation with nothing left to release it", state)
			}
			// The film is gone -- that part of retention still happens -- and the
			// key is cleared so the next pass does not re-delete a dead object.
			if artifact != "" {
				t.Errorf("the artifact reference survived retention: %q", artifact)
			}

			// Idempotent: a second pass must not thrash on the same row.
			f.runVideoSweep(t)
			if err := f.pool.QueryRow(ctx,
				`SELECT count(*) FROM gateway_async_jobs WHERE org_id = $1`, org).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 1 {
				t.Fatalf("a second sweep removed the %s job", state)
			}
		})
	}
}

// Once the money has moved there is nothing left to strand, so retention takes
// the row as it always did.
func TestRetentionDeletesAnExpiredJobThatSettled(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	submitVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`, "idem-ret-settled")
	if _, err := f.pool.Exec(ctx,
		`UPDATE gateway_async_jobs
		    SET status='completed', terminal_at=now(), settlement_state='settled',
		        expires_at = now() - interval '1 hour'
		  WHERE org_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	f.runVideoSweep(t)

	var rows int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM gateway_async_jobs WHERE org_id = $1`, org).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("retention left a settled job behind; the row has no reason to survive its artifact")
	}
}

// The sweeper is what finishes a job nothing else will: a hold taken for a
// submit that never reached an upstream.
func TestSweepVoidsAHoldTakenForAJobThatWasNeverSubmitted(t *testing.T) {
	up := &veoUpstream{}
	f := newPipeFixture(t, up.handler(t))
	ctx := context.Background()
	plaintext, _, org := f.seedKey(t, apikeys.CreateInput{})
	f.topup(t, org, 100_000_000_000)
	f.seedVideoModel(t, "google/veo-3.1", veoEnvelope, 400_000_000)

	submitVideo(t, f.videoRouter(t), plaintext,
		`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`, "idem-sweep")

	// The shape a crash between the hold and the submit leaves behind.
	if _, err := f.pool.Exec(ctx,
		`UPDATE gateway_async_jobs
		    SET status='queued', upstream_id='', submitted_at=NULL,
		        created_at = now() - interval '10 minutes'
		  WHERE org_id = $1`, org); err != nil {
		t.Fatal(err)
	}
	f.runVideoSweep(t)

	var status, settlement string
	if err := f.pool.QueryRow(ctx,
		`SELECT status, settlement_state FROM gateway_async_jobs WHERE org_id = $1`, org).
		Scan(&status, &settlement); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || settlement != "voided" {
		t.Fatalf("status=%q settlement=%q; a hold taken for work that never started "+
			"must be released, not left for the billing timeout", status, settlement)
	}
}
