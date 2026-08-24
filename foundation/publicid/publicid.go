// Package publicid implements the type-prefixed form of outward-facing ids.
// Internally they are time-ordered uuids; the API renders them as usr_…, org_…,
// ses_… and so on, and parsing checks the prefix.
package publicid

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// Prefixes are registered in one place so two entities cannot claim the same
// one; other layers register theirs alongside their own modules.
const (
	User       = "usr"
	Org        = "org"
	Session    = "ses"
	Invite     = "inv"
	Staff      = "stf"
	SignupCode = "sgc"
	Key        = "key"
	AuditLog   = "aud"
)

// Format renders a prefixed public id; an invalid uuid yields the empty string,
// which means the caller's data is wrong.
func Format(prefix string, id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%s_%x-%x-%x-%x-%x", prefix, b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Parse checks the prefix and returns the internal uuid; a malformed value is an
// error, which callers usually map to 404.
func Parse(prefix, s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	raw, ok := strings.CutPrefix(s, prefix+"_")
	if !ok {
		return u, fmt.Errorf("publicid: missing %s_ prefix", prefix)
	}
	if err := u.Scan(raw); err != nil {
		return u, fmt.Errorf("publicid: invalid uuid")
	}
	return u, nil
}

// UUIDString renders the bare internal uuid, for logs and scopes. It is not for
// outward use.
func UUIDString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// OrgMatches reports whether urlOrg -- the `org_` public id from a URL -- names
// the organization whose internal UUID string is principalOrg (what a credential
// carries). An unparseable urlOrg never matches.
func OrgMatches(principalOrg, urlOrg string) bool {
	id, err := Parse(Org, urlOrg)
	if err != nil {
		return false
	}
	return UUIDString(id) == principalOrg
}
