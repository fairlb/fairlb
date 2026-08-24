package db

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Pool saturation, exposed as observable gauges read off pgxpool's own counters.
//
// *Why this is attached inside Connect rather than offered as a call the
// entrypoints make.* A second assembly point is a thing one of the two products
// eventually forgets, and the symptom of forgetting is not an error -- it is a
// dashboard that is simply empty for that build, which reads exactly like a
// healthy idle system. The pool is created in one place, so its instrumentation
// is attached in that same place and there is nothing left to remember.
//
// *One registration per process, not per pool.* The callback is registered once
// and reads whichever pool was created most recently. Registering per pool
// instead would retain every pool the process ever opened -- the callback
// closure holds it, nothing ever unregisters, and pool.Close() does not help --
// which in the test suite means one leaked pool per test database.
//
// It also removes an ambiguity rather than hiding one. These series carry no
// pool-identifying attribute, so two simultaneously registered callbacks would
// report the same instrument with the same attributes in one collection cycle;
// the SDK resolves that by keeping one of them, not by summing, so the reading
// would silently describe an arbitrary pool with nothing to say which. With a
// single registration the answer is defined: it is the process's pool.
//
// Every entrypoint runs exactly one long-lived pool anyway -- the Cloud binary's
// three Connect calls belong to three mutually exclusive subcommands (migrate,
// staff, serve). Should a deployment ever need two concurrent pools observed
// separately, this needs a pool name attribute and a registration per pool with
// a matching release; that is a deliberate change, not a discovery.
//
// *Ordering note.* Both entrypoints call Connect before o11y.Setup installs the
// real meter provider. That is fine and not an accident: instruments taken from
// the global provider before a provider is registered are delegated to the real
// one once it arrives.
var (
	observedPool   atomic.Pointer[pgxpool.Pool]
	observePoolReg sync.Once
)

func observePool(pool *pgxpool.Pool) {
	observedPool.Store(pool)
	observePoolReg.Do(registerPoolMetrics)
}

func registerPoolMetrics() {
	meter := otel.Meter("fairlb/db")

	conns, err := meter.Int64ObservableGauge(
		"db_pool_connections",
		metric.WithDescription("Connections in the pgx pool, split by state"),
	)
	if err != nil {
		slog.Error("db: the pool connection gauge could not be created; this metric will be missing", "error", err)
		return
	}
	maxConns, err := meter.Int64ObservableGauge(
		"db_pool_connections_max",
		metric.WithDescription("The pool's configured ceiling, so saturation can be read without knowing the deployment's configuration"),
	)
	if err != nil {
		slog.Error("db: the pool ceiling gauge could not be created; this metric will be missing", "error", err)
		return
	}
	// The one number that answers "is the pool too small". Depth and wait time
	// are both needed: a pool that is briefly full is fine, a pool that makes
	// callers queue is the thing that turns into request latency nobody can
	// attribute, because from the handler's side it looks like a slow query.
	emptyAcquires, err := meter.Int64ObservableCounter(
		"db_pool_empty_acquires_total",
		metric.WithDescription("Acquisitions that found no free connection and had to wait for one"),
	)
	if err != nil {
		slog.Error("db: the empty-acquire counter could not be created; this metric will be missing", "error", err)
		return
	}
	acquireWait, err := meter.Float64ObservableCounter(
		"db_pool_acquire_wait_seconds_total",
		metric.WithDescription("Cumulative time callers spent waiting for a pooled connection"),
		metric.WithUnit("s"),
	)
	if err != nil {
		slog.Error("db: the acquire-wait counter could not be created; this metric will be missing", "error", err)
		return
	}

	if _, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		pool := observedPool.Load()
		if pool == nil {
			return nil
		}
		s := pool.Stat()
		o.ObserveInt64(conns, int64(s.AcquiredConns()), metric.WithAttributes(attribute.String("state", "acquired")))
		o.ObserveInt64(conns, int64(s.IdleConns()), metric.WithAttributes(attribute.String("state", "idle")))
		o.ObserveInt64(maxConns, int64(s.MaxConns()))
		o.ObserveInt64(emptyAcquires, s.EmptyAcquireCount())
		o.ObserveFloat64(acquireWait, s.AcquireDuration().Seconds())
		return nil
	}, conns, maxConns, emptyAcquires, acquireWait); err != nil {
		slog.Error("db: the pool metrics callback could not be registered; these metrics will be missing", "error", err)
	}
}
