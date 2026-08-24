package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// ShutdownConfig describes the host-owned HTTP servers' shared stop budget.
type ShutdownConfig struct {
	Health      *Health
	DrainGrace  time.Duration
	Timeout     time.Duration
	HTTPServers []*http.Server
}

// GracefulShutdown marks the process as draining, waits for traffic removal,
// then closes every server concurrently under one timeout budget.
func GracefulShutdown(cfg ShutdownConfig) error {
	if cfg.Health != nil {
		cfg.Health.SetDraining()
	}
	if cfg.DrainGrace > 0 {
		slog.Info("draining before shutdown", "grace", cfg.DrainGrace, "then", cfg.Timeout)
		timer := time.NewTimer(cfg.DrainGrace)
		<-timer.C
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, len(cfg.HTTPServers))
	for _, server := range cfg.HTTPServers {
		if server == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := server.Shutdown(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	var failures []error
	var timedOut bool
	for err := range errs {
		if errors.Is(err, context.DeadlineExceeded) {
			timedOut = true
			continue
		}
		failures = append(failures, err)
	}
	if timedOut {
		slog.Warn("shutdown window elapsed with requests still in flight; they were cut off",
			"window", cfg.Timeout)
	}
	if err := errors.Join(failures...); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
