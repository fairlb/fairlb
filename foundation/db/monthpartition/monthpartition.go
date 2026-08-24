// Package monthpartition owns the mechanical part of monthly PostgreSQL
// partitions. Domain-specific checks and policies remain with each table owner.
package monthpartition

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func BoundsUTC(value time.Time) (start, end time.Time) {
	utc := value.UTC()
	start = time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// Ensure creates parent_YYYY_MM with explicit UTC bounds and returns the
// logical partition name for domain-specific follow-up work.
func Ensure(ctx context.Context, db Execer, parent string, value time.Time) (string, error) {
	start, end := BoundsUTC(value)
	partition := fmt.Sprintf("%s_%s", parent, start.Format("2006_01"))
	const tsLayout = "2006-01-02 15:04:05-07:00"
	ddl := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
		pgx.Identifier{partition}.Sanitize(),
		pgx.Identifier{parent}.Sanitize(),
		start.Format(tsLayout),
		end.Format(tsLayout),
	)
	_, err := db.Exec(ctx, ddl)
	return partition, err
}
