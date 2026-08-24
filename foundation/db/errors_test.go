package db

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDatabaseErrorClassificationFollowsWrappedErrors(t *testing.T) {
	if !IsNoRows(fmt.Errorf("lookup: %w", pgx.ErrNoRows)) {
		t.Fatal("wrapped ErrNoRows was not recognized")
	}
	err := fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505"})
	if SQLState(err) != "23505" || !IsUniqueViolation(err) {
		t.Fatalf("wrapped SQLSTATE was not recognized: %q", SQLState(err))
	}
	if IsForeignKeyViolation(err) || IsCheckViolation(err) {
		t.Fatal("a unique violation matched an unrelated constraint class")
	}
}
