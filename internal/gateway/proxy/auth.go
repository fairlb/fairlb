package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/access/keyfmt"
	"github.com/fairlb/fairlb/access/organizations"
	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/drivers/cache"
	"github.com/fairlb/fairlb/foundation/errcode"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// Dataplane authentication. Parsing the key and checking its state happen here
// rather than in the shared management-plane authenticator, because that one
// renders its failures as problem+json while the dataplane must answer with the
// error shape native to the surface it is imitating. The org is looked up from
// the key and then passed explicitly down the pipeline.

// keyCacheTTL is how long a key snapshot is cached. Revocation cuts through
// immediately via the invalidation callback; the TTL only bounds how stale a
// snapshot can get if that callback is lost.
const keyCacheTTL = 60 * time.Second

// KeyCacheKey builds the cache key from a key hash. The revocation callback
// injected at the assembly point clears it by the same name.
func KeyCacheKey(keyHash string) string { return "gw:key:" + keyHash }

// Identity is the snapshot of who is calling, passed explicitly down the
// pipeline.
type Identity struct {
	KeyID   pgtype.UUID
	OrgID   pgtype.UUID
	KeyHash string
	Scopes  []string
	// ModelTierID and ModelTierStatus are the org's *effective* admission
	// tier. With none configured the query falls back to the default tier, so
	// this is always populated -- unless the database has no default tier at
	// all, in which case TierID is invalid and the decision fails closed (see
	// Guard.CheckTier).
	ModelTierID     pgtype.UUID
	ModelTierStatus string
	// ExpiresAt has to be exported: Identity is serialised into the cache, and
	// an unexported field would not survive the JSON round trip. Losing the
	// expiry instant would let an expired key keep working for as long as the
	// snapshot is cached.
	ExpiresAt time.Time
	// AllowAllModels and AllowedModels are the key's own model gate. With
	// AllowAllModels the key adds no restriction of its own and reaches
	// whatever its org's tier allows; otherwise AllowedModels is the exact set,
	// and an empty set refuses everything.
	AllowAllModels     bool
	AllowedModels      []string
	SpendLimitNano     int64
	SpendLimitInterval string // total | monthly | daily; empty means no spend limit
	RateLimitRPM       int
	RateLimitTPM       int
	// OrgRateLimitRPM and OrgRateLimitTPM are the whole organization's
	// ceilings, checked before the key's own. Zero means no ceiling.
	OrgRateLimitRPM int
	OrgRateLimitTPM int
	TotalSpentNano  int64
	WalletCurrency  string
}

// Authenticator resolves a bearer key into an Identity.
type Authenticator struct {
	keys *apikeys.Store
	orgs *organizations.Store
	// gw reads the admission tier and the discount. They are deliberately not
	// folded into dbgen's GetOrgForDataplane: that query belongs to the core
	// schema, and joining a gateway table into it would point a dependency in
	// the direction it is never allowed to go.
	gw    *gwdb.Queries
	cache cache.Store // nil means read the database every time
}

func NewAuthenticator(keys *apikeys.Store, orgs *organizations.Store, gw *gwdb.Queries, c cache.Store) *Authenticator {
	return &Authenticator{keys: keys, orgs: orgs, gw: gw, cache: c}
}

// CredentialOf extracts the plaintext virtual key from a request.
//
// The two headers are equivalent, because whichever surface the dataplane
// imitates it has to accept the credential form that surface's official SDK
// sends by default: the Anthropic SDK constructed with `api_key=` sends
// `x-api-key`, so honouring only Authorization would void the drop-in promise
// of /v1/messages. This is deliberately *not* decided per protocol -- GET
// /v1/models is a shared endpoint (see models_handler.go) where there is no
// protocol to consult.
//
// Authorization wins: the standard authentication header of RFC 7235 outranks a
// vendor convention. But it only counts if it actually *carries* a bearer
// credential -- something like `Basic xxx` supplies nothing usable, and then
// x-api-key is used and "wins" does not arise.
//
// When both are present and disagree, the first is taken silently rather than
// answering 400: having both an API key and an auth token left over in the
// environment is extremely common, and a 401 for it would leave the user with
// no way to diagnose themselves.
func CredentialOf(r *http.Request) string {
	if tok, ok := bearerToken(r.Header.Get(hdrAuthorization)); ok {
		return tok
	}
	if k := strings.TrimSpace(r.Header.Get(hdrAPIKey)); k != "" {
		return k
	}
	// The third form, for the same reason as the second: the Google SDK
	// constructed with an API key sends x-goog-api-key, and a gateway that only
	// honoured the other two would fail every request from an unmodified client
	// while claiming to serve that protocol.
	//
	// The `?key=` query form is deliberately *not* accepted. A credential in a
	// URL is a credential in access logs, proxy logs and browser history, and
	// accepting it here would make those copies this deployment's doing.
	return strings.TrimSpace(r.Header.Get(hdrGoogAPIKey))
}

// Authenticate validates the plaintext credential that CredentialOf pulled out
// of the request headers.
//
// It takes the credential rather than the raw header because header names are
// knowledge belonging to the HTTP boundary, which the pipeline should not have.
//
// Every failure uses the same protocol of 401 codes and does not distinguish
// "malformed" from "no such key": both mean the credential is invalid, and
// splitting them only hands a brute-force enumerator a feedback signal.
// Revoked and expired do get their own codes: to a legitimate holder those are
// actionable (go to the console and issue a new one).
func (a *Authenticator) Authenticate(ctx context.Context, credential string) (Identity, *Error) {
	// Local format check -- prefix, version segment, checksum tail. A typo or a
	// forged string is stopped before any database lookup, and shares a code
	// with "no such key": the presence of a checksum must not give an
	// enumerator any "the format was right" feedback.
	if !keyfmt.Valid(credential) {
		return Identity{}, NewError(errcode.GatewayInvalidApiKey, "Missing or malformed API key")
	}
	hash := crypto.HashToken(credential)

	id, err := a.lookup(ctx, hash)
	if err != nil {
		return Identity{}, err
	}
	return id, nil
}

// lookup fetches the key snapshot through the cache and checks its state.
func (a *Authenticator) lookup(ctx context.Context, hash string) (Identity, *Error) {
	if a.cache != nil {
		if raw, ok, err := a.cache.Get(ctx, KeyCacheKey(hash)); err == nil && ok {
			var id Identity
			if json.Unmarshal(raw, &id) == nil {
				// What is cached is a snapshot that passed the checks; the
				// expiry instant still has to be re-evaluated every time.
				if gerr := checkExpiry(id); gerr != nil {
					return Identity{}, gerr
				}
				return id, nil
			}
		}
	}

	row, err := a.keys.RecordByHash(ctx, hash)
	if err != nil {
		if db.IsNoRows(err) {
			return Identity{}, NewError(errcode.GatewayInvalidApiKey, "Invalid API key")
		}
		slog.ErrorContext(ctx, "dataplane: API key lookup failed", "error", err)
		return Identity{}, NewError(errcode.GatewayInternal, "Authentication failed")
	}
	if row.Status == "revoked" {
		return Identity{}, NewError(errcode.GatewayKeyRevoked, "API key has been revoked")
	}

	org, err := a.orgs.Dataplane(ctx, row.OrgID)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: organization lookup failed", "error", err)
		return Identity{}, NewError(errcode.GatewayInternal, "Authentication failed")
	}
	if org.Status != "active" {
		// Suspended and pending deletion are both refusals to the dataplane.
		return Identity{}, NewError(errcode.GatewayOrgSuspended, "Organization is suspended")
	}

	// The org's gateway settings -- admission tier and rate ceilings -- cost one
	// more round trip, but only on a cache miss: the Identity is cached with a
	// TTL.
	orgSettings, err := a.gw.GetOrgSettingsForDataplane(ctx, row.OrgID)
	if err != nil {
		slog.ErrorContext(ctx, "dataplane: organization gateway settings lookup failed", "error", err)
		return Identity{}, NewError(errcode.GatewayInternal, "Authentication failed")
	}

	id := Identity{
		KeyID: row.ID, OrgID: row.OrgID, KeyHash: hash,
		Scopes:             row.Scopes,
		ModelTierID:        orgSettings.TierID,
		ModelTierStatus:    orgSettings.TierStatus,
		AllowAllModels:     row.AllowAllModels,
		AllowedModels:      row.AllowedModels,
		RateLimitRPM:       intOrZero(row.RateLimitRpm),
		RateLimitTPM:       intOrZero(row.RateLimitTpm),
		OrgRateLimitRPM:    intOrZero(orgSettings.RateLimitRpm),
		OrgRateLimitTPM:    intOrZero(orgSettings.RateLimitTpm),
		TotalSpentNano:     row.TotalSpentNano,
		WalletCurrency:     org.Currency,
		SpendLimitNano:     int64OrZero(row.SpendLimitNano),
		SpendLimitInterval: textOrEmpty(row.SpendLimitInterval),
	}
	if row.ExpiresAt.Valid {
		id.ExpiresAt = row.ExpiresAt.Time
	}
	if gerr := checkExpiry(id); gerr != nil {
		return Identity{}, gerr
	}

	if a.cache != nil {
		if raw, mErr := json.Marshal(id); mErr == nil {
			if err := a.cache.Set(ctx, KeyCacheKey(hash), raw, keyCacheTTL); err != nil {
				slog.WarnContext(ctx, "dataplane: caching the API key snapshot failed (this request is unaffected)", "error", err)
			}
		}
	}
	// Recording last use is throttled by riding on the cache load: it is
	// written only on a miss, when the database was really consulted, never on
	// a hit -- so at most once per key per keyCacheTTL. (With no cache
	// configured, which is a local-development shape, it writes every time.)
	// It is best effort: a failure is logged and does not affect the
	// authentication result.
	if err := a.keys.TouchLastUsed(ctx, row.ID); err != nil {
		slog.WarnContext(ctx, "dataplane: recording API key last use failed (not blocking)", "error", err)
	}

	return id, nil
}

// checkExpiry decides whether the key has expired. Expiry moves with the clock,
// so it cannot be evaluated once on the way into the cache -- every snapshot
// taken back out has to be judged again.
func checkExpiry(id Identity) *Error {
	if !id.ExpiresAt.IsZero() && time.Now().After(id.ExpiresAt) {
		return NewError(errcode.GatewayKeyExpired, "API key has expired")
	}
	return nil
}

// RequireScope checks that the key carries the inference scope.
func RequireScope(id Identity, scope string) *Error {
	for _, s := range id.Scopes {
		if s == scope {
			return nil
		}
	}
	return NewError(errcode.GatewayInsufficientScope, "API key lacks the "+scope+" scope")
}

// bearerToken extracts a bearer token. The scheme name is case-insensitive per
// RFC 7235.
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(prefix):])
	return tok, tok != ""
}

func intOrZero(v pgtype.Int4) int {
	if !v.Valid {
		return 0
	}
	return int(v.Int32)
}

func int64OrZero(v pgtype.Int8) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func textOrEmpty(v pgtype.Text) string {
	if !v.Valid {
		return ""
	}
	return v.String
}
