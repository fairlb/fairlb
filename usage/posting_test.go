package usage_test

import (
	"context"
	"testing"

	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/usage"
)

func TestAggregationCursorUsesUTCHourBoundary(t *testing.T) {
	pool := testpg.Start(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'Asia/Kathmandu'`); err != nil {
		t.Fatal(err)
	}

	cursor, err := usage.NewPostingStore(pool).WithTx(tx).AggregationCursor(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.Valid {
		t.Fatal("aggregation cursor is null")
	}
	utc := cursor.Time.UTC()
	if utc.Minute() != 0 || utc.Second() != 0 || utc.Nanosecond() != 0 {
		t.Fatalf("aggregation cursor %s is not a UTC hour boundary", utc)
	}
}
