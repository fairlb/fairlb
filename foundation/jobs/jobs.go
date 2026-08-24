// Package jobs assembles the database-backed job queue.
//
// The core constraint: a job is enqueued in the same transaction as the business
// write that justifies it, using InsertTx with that transaction. Either both
// land or neither does.
package jobs

import (
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// ── Expected interval of each periodic job ───────────────────────────────
//
// A "last successful run" column of bare timestamps claims to be direct evidence
// that the periodic jobs are running, but the reader has to already know how
// often each job should run before a date can be judged healthy or alarming —
// the criterion lives in the reader's head rather than on the page. A job that
// has not run for three weeks and one that finished a minute ago look identical
// in that column.
//
// The intervals are registered here rather than derived, because the queue
// library's periodic job type exposes none of its fields: an assembled slice of
// them cannot be asked for its kind or its schedule.
//
// Registration and construction are the same call. Split into two steps, someone
// will eventually do only the first, and a missing registration shows up as that
// kind never being judged stale on the health page — and "no alarm" does not
// look like a defect.

var (
	scheduleMu sync.RWMutex
	schedules  = map[string]time.Duration{}
)

// Periodic builds a periodic job and registers its expected interval in the same
// call.
//
// ctor is invoked once here to learn the job's kind. Every ctor is a pure
// constructor with no side effects.
func Periodic(
	every time.Duration,
	ctor river.PeriodicJobConstructor,
	opts *river.PeriodicJobOpts,
) *river.PeriodicJob {
	args, _ := ctor()
	if args != nil {
		scheduleMu.Lock()
		schedules[args.Kind()] = every
		scheduleMu.Unlock()
	}
	//nolint:forbidigo // this function is the chokepoint that rule points at; it has to be able to construct one.
	return river.NewPeriodicJob(river.PeriodicInterval(every), ctor, opts)
}

// Schedules returns the registered kind-to-interval map.
//
// It is only populated after the assembly point has constructed the periodic
// jobs, so in a test process it is usually empty. A caller that cannot find an
// interval must degrade to making no judgement, not treat the absence as zero.
func Schedules() map[string]time.Duration {
	scheduleMu.RLock()
	defer scheduleMu.RUnlock()
	out := make(map[string]time.Duration, len(schedules))
	for k, v := range schedules {
		out[k] = v
	}
	return out
}

// NewInsertOnlyClient returns a client that can enqueue but never runs jobs.
// Separating enqueueing from execution lets a process with no workers still
// enqueue transactionally.
func NewInsertOnlyClient(pool *pgxpool.Pool) (*river.Client[pgx.Tx], error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{})
}

// WorkerConfig is the input for assembling a running client.
type WorkerConfig struct {
	// Workers is the registry each module adds its workers to.
	Workers *river.Workers
	// PeriodicJobs are the scheduled jobs, such as the sweepers.
	PeriodicJobs []*river.PeriodicJob
	// MaxWorkers is the default queue concurrency; 0 uses the library default.
	MaxWorkers int
}

// NewWorkerClient returns a client that executes jobs and schedules the periodic
// ones once started.
func NewWorkerClient(pool *pgxpool.Pool, cfg WorkerConfig) (*river.Client[pgx.Tx], error) {
	max := cfg.MaxWorkers
	if max <= 0 {
		max = 10
	}
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:       map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: max}},
		Workers:      cfg.Workers,
		PeriodicJobs: cfg.PeriodicJobs,
	})
}

// NewWorkers returns an empty worker registry for modules to add to.
func NewWorkers() *river.Workers { return river.NewWorkers() }
