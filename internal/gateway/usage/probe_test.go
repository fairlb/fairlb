package usage_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/alerttest"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/usage"
)

func seedProvider(t *testing.T, pool *pgxpool.Pool, slug, baseURL string, enabled, autoDisabled bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO providers (slug, vendor, protocols, base_url, enabled, auto_disabled)
		 VALUES ($1, 'custom', ARRAY['openai'], $2, $3, $4)`, slug, baseURL, enabled, autoDisabled); err != nil {
		t.Fatal(err)
	}
}

func providerState(t *testing.T, pool *pgxpool.Pool, slug string) (enabled, autoDisabled bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT enabled, auto_disabled FROM providers WHERE slug = $1`, slug).
		Scan(&enabled, &autoDisabled); err != nil {
		t.Fatal(err)
	}
	return
}

// A failed probe auto-disables a provider; a recovered one auto-enables it.
func TestProbeAutoDisableAndRecover(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	q := gwdb.New(pool)

	healthy := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)

	seedProvider(t, pool, "p-flaky", srv.URL, true, false)
	al := &alerttest.Recorder{}
	w := usage.NewProbeWorker(pool, q, al)

	// Healthy: nothing changes.
	if err := w.Work(ctx, &river.Job[usage.ProbeArgs]{}); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := providerState(t, pool, "p-flaky"); !enabled {
		t.Fatal("a healthy provider must not be disabled")
	}

	// Down: auto-disable and alert.
	healthy = false
	if err := w.Work(ctx, &river.Job[usage.ProbeArgs]{}); err != nil {
		t.Fatal(err)
	}
	enabled, autoDisabled := providerState(t, pool, "p-flaky")
	if enabled || !autoDisabled {
		t.Fatalf("a failed probe should auto-disable and flag the source: enabled=%v auto=%v", enabled, autoDisabled)
	}
	if len(al.Subjects()) != 1 {
		t.Errorf("expected exactly one alert: %v", al.Subjects())
	}

	// Recovered: auto-enable.
	healthy = true
	if err := w.Work(ctx, &river.Job[usage.ProbeArgs]{}); err != nil {
		t.Fatal(err)
	}
	enabled, autoDisabled = providerState(t, pool, "p-flaky")
	if !enabled || autoDisabled {
		t.Fatalf("a recovered probe should auto-enable and clear the flag: enabled=%v auto=%v", enabled, autoDisabled)
	}
	if len(al.Subjects()) != 2 {
		t.Errorf("recovery should alert too: %v", al.Subjects())
	}
}

// A manually disabled provider must not be auto-enabled when the probe
// recovers. Otherwise an operator takes a provider out of service and the probe
// puts it back five minutes later, making the manual action meaningless.
func TestProbeDoesNotResurrectManuallyDisabled(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)

	// enabled=false with auto_disabled=false means a manual disable.
	seedProvider(t, pool, "p-manual", srv.URL, false, false)

	w := usage.NewProbeWorker(pool, gwdb.New(pool), nil)
	if err := w.Work(ctx, &river.Job[usage.ProbeArgs]{}); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := providerState(t, pool, "p-manual"); enabled {
		t.Fatal("a manually disabled provider must not be auto-enabled by the probe")
	}
}

// A 401 or 403 means the service is alive and the probe simply carried no
// credentials -- still healthy, and must not be disabled by mistake.
func TestProbeTreatsAuthErrorAsHealthy(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	seedProvider(t, pool, "p-auth", srv.URL, true, false)
	w := usage.NewProbeWorker(pool, gwdb.New(pool), nil)
	if err := w.Work(ctx, &river.Job[usage.ProbeArgs]{}); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := providerState(t, pool, "p-auth"); !enabled {
		t.Fatal("a 401 means the service is alive and must not disable the provider")
	}
}
