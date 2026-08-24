package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/foundation/drivers/ratelimit"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/publicid"
)

// The admission gates: model allowlist, spend budget (total, monthly, daily),
// and the request and token rates. Their order follows their cost -- purely
// in-memory checks first, the budget that needs a database read next, and rate
// limiting last, because it has a side effect: it consumes quota.
//
// Rates are measured at two levels for every request, the organization's and
// the key's, and the organization's goes first: when the whole organization is
// at its ceiling there is no reason to spend one of the key's own requests
// finding that out.

// rateWindow is the measurement window for RPM and TPM.
const rateWindow = time.Minute

// Guard applies the per-key gates.
type Guard struct {
	keys    *apikeys.Store
	limiter ratelimit.Limiter
}

func NewGuard(keys *apikeys.Store, l ratelimit.Limiter) *Guard {
	return &Guard{keys: keys, limiter: l}
}

// CheckModel enforces the key's own model gate.
//
// Two states, said explicitly rather than inferred from the size of a list: the
// key restricts nothing of its own, or it names the models it may call. An
// empty allowlist is therefore a real answer -- it refuses everything -- and
// not a synonym for "unrestricted". Inferring the second from an empty list is
// what turns "the last model was removed from this key" into "this key may now
// call anything".
func (g *Guard) CheckModel(id Identity, modelSlug string) *Error {
	if id.AllowAllModels {
		return nil
	}
	for _, m := range id.AllowedModels {
		if m == modelSlug {
			return nil
		}
	}
	// Same code as "no such model": do not confirm the existence of a model the
	// caller is not allowed to use.
	return NewError(errcode.GatewayModelNotFound, "Model not found or unavailable")
}

// CheckTier verifies that the org's admission tier is usable at all, and fails
// closed.
//
// Both cases refuse, and both use model_tier_disabled rather than
// model_not_found:
//   - the tier is disabled;
//   - the tier cannot be resolved at all. A migration seeds one row that can be
//     neither deleted nor disabled, so reaching this means somebody changed the
//     database behind the application.
//
// It deliberately does *not* fall back to the default tier: falling back would
// mean the organization keeps spending under an admission policy and a price list
// they do not know about. The code is separate from model_not_found because
// here *every* model is refused -- reusing 404 would show the organization "all the
// models suddenly vanished" and leave them guessing, whereas a 403 with one
// sentence tells them who to ask.
func (g *Guard) CheckTier(id Identity) *Error {
	if !id.ModelTierID.Valid {
		// No default tier is a data-integrity problem on the operator's side
		// and has to be shouted about: it refuses every request in the
		// deployment while giving the organization no visible reason.
		slog.Error("dataplane: no usable model tier (the default tier is missing); refusing to serve", "org_id", id.OrgID)
		return NewError(errcode.GatewayModelTierDisabled, "Your model access tier is unavailable; contact support")
	}
	if id.ModelTierStatus != "active" {
		return NewError(errcode.GatewayModelTierDisabled, "Your model access tier is disabled; contact support")
	}
	return nil
}

// CheckBudget enforces the key's spend limit. The three periods read different
// sources: daily and monthly sum the per-day table, total reads a denormalised
// running column. See docs/design/key-budgets.md.
func (g *Guard) CheckBudget(ctx context.Context, id Identity) *Error {
	if id.SpendLimitNano <= 0 || id.SpendLimitInterval == "" {
		return nil
	}
	var spent int64
	switch id.SpendLimitInterval {
	case "total":
		spent = id.TotalSpentNano
	case "daily", "monthly":
		since := periodStart(id.SpendLimitInterval, time.Now().UTC())
		sum, err := g.keys.SpendSince(ctx, id.KeyID, since)
		if err != nil {
			slog.ErrorContext(ctx, "dataplane: reading API key spend failed", "error", err)
			return NewError(errcode.GatewayInternal, "Budget check failed")
		}
		spent = sum
	default:
		slog.ErrorContext(ctx, "dataplane: unknown budget period", "interval", id.SpendLimitInterval)
		return nil
	}
	if spent >= id.SpendLimitNano {
		// A separate code from insufficient credits: the budget is the key's own
		// gate and the customer can raise it or use another key, which is a
		// different situation from the organization being out of money.
		return NewError(errcode.GatewayKeyBudgetExceeded, "API key budget exhausted")
	}
	return nil
}

// CheckRate enforces the request and token rates, at both levels. With
// estimatedTokens above zero it consumes that much of each token allowance.
//
// It has a side effect -- consuming quota -- so it must be called only after
// every other gate has passed, or a request that the allowlist was going to
// refuse would still eat into the customer's allowance.
//
// The organization's ceilings are checked before the key's. Both are checked;
// neither substitutes for the other. A key can only ever be the narrower of the
// two, because its consumption also counts against the organization.
//
// Each refusal names the level it came from. "Rate limit exceeded" with no
// subject leaves a organization unable to tell "this key is small" from "the whole
// account is at its ceiling", and those have different fixes -- one they can
// make themselves, one they have to ask for.
func (g *Guard) CheckRate(ctx context.Context, id Identity, estimatedTokens int64) *Error {
	if g.limiter == nil {
		return nil
	}
	orgID := uuidStr(id.OrgID)
	keyID := uuidStr(id.KeyID)

	if gerr := g.checkRequests(ctx, "gw:rpm:org:"+orgID, id.OrgRateLimitRPM,
		"Organization request rate limit exceeded"); gerr != nil {
		return gerr
	}
	if gerr := g.checkTokens(ctx, "gw:tpm:org:"+orgID, id.OrgRateLimitTPM, estimatedTokens,
		"Organization token rate limit exceeded"); gerr != nil {
		return gerr
	}
	if gerr := g.checkRequests(ctx, "gw:rpm:"+keyID, id.RateLimitRPM,
		"Request rate limit exceeded"); gerr != nil {
		return gerr
	}
	return g.checkTokens(ctx, "gw:tpm:"+keyID, id.RateLimitTPM, estimatedTokens,
		"Token rate limit exceeded")
}

// checkRequests consumes one request from a per-minute allowance. A limit of
// zero or less means the allowance is not declared and nothing is measured.
func (g *Guard) checkRequests(ctx context.Context, bucket string, limit int, message string) *Error {
	if limit <= 0 {
		return nil
	}
	res, err := g.limiter.Allow(ctx, bucket, limit, rateWindow)
	if err != nil {
		// A broken rate-limiter driver does not block the request: for a
		// capacity gate, availability wins, and unauthorised access is
		// stopped by the security gates that ran earlier.
		slog.ErrorContext(ctx, "dataplane: request rate-limit check failed; letting the request through",
			"bucket", bucket, "error", err)
		return nil
	}
	if res.Allowed {
		return nil
	}
	return &Error{Code: errcode.GatewayRateLimited, Message: message, RetryAfter: res.RetryAfter}
}

// checkTokens consumes the request's estimated input tokens from a per-minute
// token allowance.
func (g *Guard) checkTokens(ctx context.Context, bucket string, limit int, estimatedTokens int64, message string) *Error {
	if limit <= 0 || estimatedTokens <= 0 {
		return nil
	}
	n := int(min(estimatedTokens, int64(limit)+1)) // an over-limit request can never pass, and this cannot overflow
	res, err := g.limiter.AllowN(ctx, bucket, n, limit, rateWindow)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: token rate-limit check failed; letting the request through",
			"bucket", bucket, "error", err)
		return nil
	}
	if res.Allowed {
		return nil
	}
	return &Error{Code: errcode.GatewayRateLimited, Message: message, RetryAfter: res.RetryAfter}
}

// periodStart returns the first day of the budget period; the per-day table is
// filtered at day granularity.
func periodStart(interval string, now time.Time) pgtype.Date {
	var d time.Time
	if interval == "monthly" {
		d = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	} else { // daily
		d = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	}
	return pgtype.Date{Time: d, Valid: true}
}

// uuidStr is this package's shorthand for publicid.UUIDString; the pipeline uses
// it wherever a UUID has to reach a string field. One name per package, and the
// same name in every package -- there used to be four (uuidStr / uuidString /
// uuidText / uuidHex, the last of which does not even describe what it returns),
// and two of them lived in this package at once.
var uuidStr = publicid.UUIDString
