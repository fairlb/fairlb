package db

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func IsUniqueViolation(err error) bool     { return SQLState(err) == "23505" }
func IsForeignKeyViolation(err error) bool { return SQLState(err) == "23503" }
func IsCheckViolation(err error) bool      { return SQLState(err) == "23514" }

// ConstraintName is the constraint a violation names, or "" when the error is
// not a Postgres error at all.
//
// A check violation on its own says only "some CHECK on this table refused the
// row", and a table typically has several. Translating that into one specific
// message means knowing which -- so the constraints worth translating are
// given explicit names in the migration rather than left to Postgres to name
// after the column, which would silently change meaning if a second check on
// the same column were ever added.
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}
