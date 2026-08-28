package gwstaffapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/access/organizations/orgtest"
	"github.com/fairlb/fairlb/foundation/publicid"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// The settlement repair queue: jobs that reached a terminal state while their
// reservation never moved.
//
// `gateway_async_jobs_stuck_money_idx` was created with a comment calling this
// "the operator's repair queue" and then had no reader for its whole life -- the
// state was absent from the API, the console and the alerts at the same time.
// Every row in it is a customer either overcharged or not charged at all, so the
// cases that matter here are as much about what must *not* be counted: a false
// alarm on this dashboard teaches the operator to ignore it.
func TestHealthReportsJobsWhoseMoneyNeverMoved(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()

	read := func() *gwstaffapi.GatewayStuckMoney {
		t.Helper()
		res, err := s.GetGatewayHealth(ctx, gwstaffapi.GetGatewayHealthRequestObject{})
		if err != nil {
			t.Fatal(err)
		}
		return res.(gwstaffapi.GetGatewayHealth200JSONResponse).StuckMoney
	}

	// An empty queue must still be reported. "Nothing is stuck" and "we could
	// not tell" are different sentences, and absent is reserved for the second.
	if clear := read(); clear == nil {
		t.Fatal("an empty repair queue came back absent -- absent is reserved for a failed read, and the interface renders it as 'could not tell'")
	} else if clear.Jobs != 0 {
		t.Fatalf("nothing was inserted yet, want 0 stuck jobs, got %d", clear.Jobs)
	} else if clear.OldestTerminalAt != nil {
		t.Errorf("with nothing stuck there is no oldest, got %v", clear.OldestTerminalAt)
	}

	org := publicid.UUIDString(orgtest.Create(t, pool, orgtest.Seed{Name: "Stuck"}))
	var model string
	if err := pool.QueryRow(ctx,
		`INSERT INTO models (slug, output_modalities) VALUES ($1, ARRAY['video']) RETURNING id`,
		"acme/vid-"+uuid.NewString()[:8]).Scan(&model); err != nil {
		t.Fatal(err)
	}

	job := func(status, settlement string, terminalAt *time.Time) {
		t.Helper()
		id := uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO gateway_async_jobs
			   (org_id, kind, request_id, idempotency_key, request_fingerprint,
			    model_id, model_slug, status, settlement_state, params, hold_nano,
			    max_job_seconds, terminal_at, expires_at)
			 VALUES ($1, 'video', $2, $3, 'fp', $4, 'acme/vid', $5, $6, '{}', 1000,
			         600, $7, now() + interval '1 day')`,
			org, "req-"+id, "idem-"+id, model, status, settlement, terminalAt); err != nil {
			t.Fatal(err)
		}
	}

	older := time.Now().Add(-72 * time.Hour).UTC()
	newer := time.Now().Add(-2 * time.Hour).UTC()

	// The two that are genuinely stuck. `protected` counts as much as `held`:
	// it is a hold the sweeper was told not to reclaim, which is precisely the
	// state that outlives everything else if nobody settles it.
	job("completed", "held", &older)
	job("failed", "protected", &newer)

	// The three that must not count, one per way the money did move -- or has
	// not been asked to yet.
	job("completed", "settled", &newer) // charged, as it should be
	job("canceled", "voided", &newer)   // refunded, as it should be
	job("in_progress", "held", nil)     // still running; a hold in flight is not stuck

	got := read()
	if got == nil {
		t.Fatal("the repair queue came back absent after rows were inserted")
	}
	if got.Jobs != 2 {
		t.Errorf("want the 2 terminal-but-unsettled jobs, got %d -- a settled, voided or in-flight job counted here is a false alarm that teaches the operator to ignore the queue", got.Jobs)
	}
	if got.OldestTerminalAt == nil {
		t.Fatal("with rows stuck there must be an oldest -- it is what separates a live incident from one stranded row")
	}
	if delta := got.OldestTerminalAt.Sub(older); delta > time.Second || delta < -time.Second {
		t.Errorf("oldest should be the earliest terminal_at (%v), got %v", older, *got.OldestTerminalAt)
	}
}
