package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Health serves liveness through Up, startup readiness followed by latched
// liveness through StartupThenLiveness, and continuous dependency checks
// through Healthz. It also carries the drain signal for graceful shutdown.
type Health struct {
	draining atomic.Bool
	ready    atomic.Bool
	checks   map[string]func(context.Context) error
}

// NewHealth builds a Health from a set of named dependency checks. Each check is
// responsible for its own cost: probes are polled frequently.
func NewHealth(checks map[string]func(context.Context) error) *Health {
	return &Health{checks: checks}
}

// SetDraining starts shutdown draining: /up begins answering 503.
func (h *Health) SetDraining() { h.draining.Store(true) }

// Up answers 200 unless the process is draining.
//
// It reads no dependency: this is the liveness half, and a liveness probe that
// fails on a dependency incident removes traffic from a process that is fine.
// The paragraph that used to be here -- "reports startup readiness and, after
// the first successful dependency check, latched liveness" -- described
// StartupThenLiveness, one function down, and had never described this body.
func (h *Health) Up(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("draining"))
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// StartupThenLiveness holds traffic until every startup dependency succeeds.
// It then latches success so that later dependency incidents are reported by
// Healthz without turning liveness into a cascading traffic-removal signal.
func (h *Health) StartupThenLiveness(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("draining"))
		return
	}
	if !h.ready.Load() {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if status, _ := h.runChecks(ctx); status != http.StatusOK {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		h.ready.Store(true)
	}
	_, _ = w.Write([]byte("ok"))
}

// Healthz runs each dependency check; any failure makes the whole response 503.
// The body carries only component names and ok/failed so that nothing internal
// leaks; the error details go to the structured log.
func (h *Health) Healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	status, results := h.runChecks(ctx)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": map[bool]string{true: "ok", false: "degraded"}[status == http.StatusOK],
		"checks": results,
	})
}

func (h *Health) runChecks(ctx context.Context) (int, map[string]string) {
	status := http.StatusOK
	results := make(map[string]string, len(h.checks))
	for name, check := range h.checks {
		if err := check(ctx); err != nil {
			status = http.StatusServiceUnavailable
			results[name] = "failed"
			slog.WarnContext(ctx, "health check failed", "check", name, "error", err)
		} else {
			results[name] = "ok"
		}
	}
	return status, results
}
