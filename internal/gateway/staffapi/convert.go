package gwstaffapi

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// Conversions between the generated contract types and the sqlc ones, kept in
// one place so that no handler has to unpack and repack pgtype values itself.

func uuidOf(u uuid.UUID) pgtype.UUID {
	var out pgtype.UUID
	out.Bytes, out.Valid = u, true
	return out
}

func derefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func visibilityOr(p *GatewayModelInputVisibility, def string) string {
	if p == nil {
		return def
	}
	return string(*p)
}

func visibilityPtr(p *GatewayModelInputVisibility) *string {
	if p == nil {
		return nil
	}
	s := string(*p)
	return &s
}
