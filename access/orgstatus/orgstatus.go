// Package orgstatus is the single place the org write gate is decided.
//
// # Why it is its own package
//
// The write gates live in several packages, and putting the decision in any one
// of them would create a dependency edge for the sake of a single function. The
// decision itself depends only on two leaf packages.
//
// # Why it denies by default
//
// Before this existed, each gate wrote its own `== "suspended"` check. The
// consequence was that a newly added status was writable everywhere for a long
// time without anyone noticing: an org marked for deletion still accepted
// renames, invitations, role changes, new keys and top-ups, and only the data
// plane refused it.
//
// The root cause is that the shape of that check defaults to permitting. Add a
// status value and every site has to remember to handle it; miss one and it
// silently permits. This is the other way round: any status not explicitly on
// the allowlist is refused, so a new status that is not added here is refused
// until someone comes and adds it. The cost is having to edit this one place —
// which is exactly the pause worth forcing.
package orgstatus

import (
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
)

// RequireWritable decides whether an org's current status allows writes.
//
// Each outcome maps to a registered error code:
//   - active: allowed
//   - suspended: 403 — a suspended org can still be read, but not written
//   - pending_delete: 409, the same code and wording used elsewhere for
//     "this organization is being deleted". It is not a permission problem; the
//     resource is in a lifecycle stage that does not accept changes.
//
// The callers are the write gates. Do not scatter calls elsewhere, and in
// particular do not put it on a read path: reads stay allowed.
func RequireWritable(status string) error {
	switch status {
	case "active":
		return nil
	case "suspended":
		return httpx.ErrCode(errcode.CommonOrgSuspended)
	case "pending_delete":
		return httpx.ErrCodeDetail(errcode.CommonConflict, "This organization is being deleted")
	default:
		// An unknown status is not writable. Refusing a new status that should
		// have been allowed is noticed immediately; permitting one that should
		// have been refused is not noticed at all.
		return httpx.ErrCodeDetail(errcode.CommonConflict, "This organization's status does not allow write operations")
	}
}
