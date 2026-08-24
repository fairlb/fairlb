package usage

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/fairlb/fairlb/foundation/jobs"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

type AffinityGCArgs struct{}

func (AffinityGCArgs) Kind() string { return "gateway_resource_affinity_gc" }

type AffinityGCWorker struct {
	river.WorkerDefaults[AffinityGCArgs]
	q *gwdb.Queries
}

func NewAffinityGCWorker(pool *pgxpool.Pool) *AffinityGCWorker {
	return &AffinityGCWorker{q: gwdb.New(pool)}
}

func (w *AffinityGCWorker) Work(ctx context.Context, _ *river.Job[AffinityGCArgs]) error {
	deleted, err := w.q.DeleteExpiredResourceAffinities(ctx)
	if err == nil && deleted > 0 {
		slog.InfoContext(ctx, "expired gateway resource affinities deleted", "rows", deleted)
	}
	return err
}

func AffinityGCPeriodicJob() *river.PeriodicJob {
	return jobs.Periodic(
		24*time.Hour,
		func() (river.JobArgs, *river.InsertOpts) { return AffinityGCArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}
