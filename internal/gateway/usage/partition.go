// Package usage persists the gateway's usage records, maintains their
// partitions, and wires them into the metering framework.
package usage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/db/monthpartition"
	"github.com/fairlb/fairlb/foundation/jobs"
)

// PartitionArgs is the job that pre-creates monthly partitions for usage_logs
// and gateway_usage_rollups.
// The migrations create the current and next month; this job stays ahead of
// that, which is why both tables' default partitions are normally empty.
type PartitionArgs struct{}

func (PartitionArgs) Kind() string { return "gateway_partition_maintain" }

// partitionedTables are the tables needing monthly partitions. usage_logs is
// partitioned by write time and gateway_usage_rollups by the hour bucket it
// belongs to, but the boundary rule and the cadence are the same, so one job
// covers both.
var partitionedTables = []string{"usage_logs", "gateway_usage_rollups"}

// PartitionWorker idempotently pre-creates the coming months' partitions.
// The table names in the DDL are computed from the UTC month start and a fixed
// table name -- no user input is involved, which is why this does not go
// through the query generator.
type PartitionWorker struct {
	river.WorkerDefaults[PartitionArgs]
	pool *pgxpool.Pool
}

func NewPartitionWorker(pool *pgxpool.Pool) *PartitionWorker {
	return &PartitionWorker{pool: pool}
}

// partitionLookahead is how many months ahead of the current one to create, so
// the job stays ahead even if a scheduled run is missed.
const partitionLookahead = 3

func (w *PartitionWorker) Work(ctx context.Context, _ *river.Job[PartitionArgs]) error {
	m0, _ := monthpartition.BoundsUTC(time.Now())
	for _, table := range partitionedTables {
		for i := range partitionLookahead {
			if err := ensurePartition(ctx, w.pool, table, m0.AddDate(0, i, 0)); err != nil {
				return fmt.Errorf("gateway: pre-create %s partition: %w", table, err)
			}
		}
	}
	return w.warnOnDefaultPartition(ctx)
}

// warnOnDefaultPartition: a non-empty default partition means pre-creation
// failed at some point and rows landed there.
// It does not affect correctness -- routing rows to the default keeps the
// service available -- but archiving works by detaching partitions, so rows
// mixed into the default would be missed. That has to surface early.
func (w *PartitionWorker) warnOnDefaultPartition(ctx context.Context) error {
	var usageRows, rollupRows int64
	err := w.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_logs_default),
		(SELECT count(*) FROM gateway_usage_rollups_default)`).Scan(&usageRows, &rollupRows)
	if err != nil {
		return fmt.Errorf("gateway: inspect default partition: %w", err)
	}
	if usageRows > 0 || rollupRows > 0 {
		slog.ErrorContext(ctx, "default partition is not empty: pre-creation failed at some point and archiving will miss rows",
			"usage_logs_default", usageRows, "rollups_default", rollupRows)
	}
	return nil
}

// ensurePartition idempotently creates the partition for monthStart's month,
// with explicitly UTC boundaries.
func ensurePartition(ctx context.Context, pool *pgxpool.Pool, table string, monthStart time.Time) error {
	partition, err := monthpartition.Ensure(ctx, pool, table, monthStart)
	if err != nil {
		return err
	}
	return applyPartitionRLS(ctx, pool, partition)
}

// applyPartitionRLS gives a freshly created partition the same isolation policy
// as its parent.
//
// A row-level security policy attached to a partitioned parent does not descend
// to its partitions: access through the parent is constrained, while querying a
// partition directly returns every organization's rows. No user-facing path queries
// by partition name today, but the day someone adds "only scan this month's
// partition" for performance, isolation fails silently and without error. So
// the bypass route is made safe in itself -- a defence should not rest on
// "nobody would write that".
func applyPartitionRLS(ctx context.Context, pool *pgxpool.Pool, partition string) error {
	if _, err := pool.Exec(ctx,
		fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY", partition)); err != nil {
		return fmt.Errorf("usage: enable row-level security on partition: %w", err)
	}
	// The policy may already exist; idempotent pre-creation reaches this more
	// than once.
	_, err := pool.Exec(ctx, fmt.Sprintf(
		`DO $$ BEGIN
		    IF NOT EXISTS (SELECT FROM pg_policies WHERE tablename = '%s' AND policyname = '%s_isolation') THEN
		        CREATE POLICY %s_isolation ON %s USING (org_id = current_setting('app.org_id')::uuid);
		    END IF;
		 END $$`, partition, partition, partition, partition))
	if err != nil {
		return fmt.Errorf("usage: create policy on partition: %w", err)
	}
	return nil
}

// PartitionInterval is how often partitions are pre-created. The objects are
// monthly, so a daily check is ample and tolerates a missed run.
const PartitionInterval = 24 * time.Hour

// PartitionPeriodicJob builds the periodic pre-creation job, registered at the
// assembly point. It runs on start so a deploy immediately reconciles.
func PartitionPeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(
		PartitionInterval,
		func() (river.JobArgs, *river.InsertOpts) { return PartitionArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}
