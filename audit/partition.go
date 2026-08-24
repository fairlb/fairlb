package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/db/monthpartition"
	"github.com/fairlb/fairlb/foundation/jobs"
)

// PartitionArgs drives the job that creates monthly audit partitions ahead of
// time. The migration creates the current and next month; this job keeps running
// ahead of the calendar, which is why the catch-all default partition normally
// stays empty.
type PartitionArgs struct{}

func (PartitionArgs) Kind() string { return "audit_partition_maintain" }

// PartitionWorker idempotently creates the upcoming partitions. The table names
// in the DDL are computed from the UTC start of a month and are never user
// input, which is why this does not go through the query generator.
type PartitionWorker struct {
	river.WorkerDefaults[PartitionArgs]
	pool *pgxpool.Pool
}

func NewPartitionWorker(pool *pgxpool.Pool) *PartitionWorker {
	return &PartitionWorker{pool: pool}
}

// partitionLookahead is how many months ahead of the current one to create, so
// that a skipped run of the periodic job does not leave a gap.
const partitionLookahead = 3

func (w *PartitionWorker) Work(ctx context.Context, _ *river.Job[PartitionArgs]) error {
	m0, _ := monthpartition.BoundsUTC(time.Now())
	for i := 0; i < partitionLookahead; i++ {
		if err := ensurePartition(ctx, w.pool, m0.AddDate(0, i, 0)); err != nil {
			return fmt.Errorf("audit: create partition ahead of time: %w", err)
		}
	}
	return nil
}

// ensurePartition idempotently creates the partition for monthStart's month,
// with explicit UTC bounds.
func ensurePartition(ctx context.Context, pool *pgxpool.Pool, monthStart time.Time) error {
	_, err := monthpartition.Ensure(ctx, pool, "audit_logs", monthStart)
	return err
}

// PartitionInterval is how often the job runs. The objects are monthly, so a
// daily check is ample and tolerates a missed run.
const PartitionInterval = 24 * time.Hour

// PartitionPeriodicJob builds the periodic partition job. The assembly point
// registers it, and it runs once on startup so a deploy never leaves a gap.
func PartitionPeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(
		PartitionInterval,
		func() (river.JobArgs, *river.InsertOpts) { return PartitionArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}
