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
