package proxy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

// byokChoice is this request's organization-credential decision; the zero value means
// no organization credential is used. See docs/design/upstream-credentials.md.
type byokChoice struct {
	KeyID   pgtype.UUID
	Secret  string
	BaseURL string // empty means use the provider's own endpoint
	// Fallback true lets a failed organization credential fall back to a shared one
	// and keep trying. It defaults to false: falling back silently means being
	// billed at full price, so it has to be the organization's explicit choice.
	//
	// Its scope is *this vendor*. A candidate at a platform the organization holds no
	// credential for is served on a shared one whatever this says, because there
	// was never a organization credential to fall back from -- refusing there would
	// mean one BYOK key stopped the org being routed anywhere else. So the
	// promise this switch makes is "do not retry my rejected key on yours", not
	// "never use yours".
	Fallback bool
}

// pinnedCredentialFor resolves exactly the credential recorded for a stateful
// resource. There is intentionally no fallback: another key at the same
// provider is a different upstream account and therefore a different resource
// namespace.
func (p *Pipeline) pinnedCredentialFor(
	ctx context.Context, route catalog.Route, orgID pgtype.UUID, in Request,
) (keyID pgtype.UUID, apiKey, baseURL string, byokKeyID pgtype.UUID, gerr *Error) {
	baseURL = route.BaseURL
	if in.PinnedOrgProviderKeyID.Valid {
		row, err := p.gw.GetActiveOrgProviderKeyByID(ctx, gwdb.GetActiveOrgProviderKeyByIDParams{
			ID: in.PinnedOrgProviderKeyID, OrgID: orgID,
		})
		if err != nil {
			if !db.IsNoRows(err) {
				slog.ErrorContext(ctx, "dataplane: reading pinned organization credential failed", "error", err)
			}
			return pgtype.UUID{}, "", "", pgtype.UUID{}, NewError(errcode.GatewayStateRouteUnavailable, "The resource's original credential is unavailable")
		}
		if row.Vendor != route.ProviderVendor {
			return pgtype.UUID{}, "", "", pgtype.UUID{}, NewError(errcode.GatewayStateRouteUnavailable, "The resource's original route no longer matches its credential")
		}
		plain, err := p.box.Open(row.SecretEnc, row.ID.Bytes[:])
		if err != nil {
			return pgtype.UUID{}, "", "", pgtype.UUID{}, NewError(errcode.GatewayStateRouteUnavailable, "The resource's original credential is unavailable")
		}
		if row.BaseUrl.Valid && row.BaseUrl.String != "" {
			baseURL = row.BaseUrl.String
		}
		return pgtype.UUID{}, string(plain), baseURL, row.ID, nil
	}

	if !in.PinnedProviderKeyID.Valid {
		return pgtype.UUID{}, "", "", pgtype.UUID{}, NewError(errcode.GatewayStateRouteUnavailable, "The resource's original credential is unavailable")
	}
	row, err := p.gw.GetProviderKeyByID(ctx, in.PinnedProviderKeyID)
	if err != nil || row.ProviderID != route.ProviderID || !p.breaker.KeyAvailable(ctx, row.ID) {
		if err != nil && !db.IsNoRows(err) {
			slog.ErrorContext(ctx, "dataplane: reading pinned provider credential failed", "error", err)
		}
		return pgtype.UUID{}, "", "", pgtype.UUID{}, NewError(errcode.GatewayStateRouteUnavailable, "The resource's original credential is unavailable")
	}
	plain, err := p.box.Open(row.SecretEnc, row.ID.Bytes[:])
	if err != nil {
		return pgtype.UUID{}, "", "", pgtype.UUID{}, NewError(errcode.GatewayStateRouteUnavailable, "The resource's original credential is unavailable")
	}
	return row.ID, string(plain), baseURL, pgtype.UUID{}, nil
}

func (c byokChoice) active() bool { return c.Secret != "" }

// byokChoices is what an org brings to this request: its usable credentials,
// by vendor. A candidate uses the entry matching its provider's vendor, or none.
type byokChoices map[string]byokChoice

// vendors lists the vendors the organization brings a usable credential for,
// which is what candidate selection needs to know before a credential is
// picked.
func (c byokChoices) vendors() []string {
	out := make([]string, 0, len(c))
	for vendor, choice := range c {
		if choice.active() {
			out = append(out, vendor)
		}
	}
	return out
}

// forVendor returns the credential to use for a candidate, and whether there is
// one.
func (c byokChoices) forVendor(vendor string) (byokChoice, bool) {
	choice, ok := c[vendor]
	return choice, ok && choice.active()
}

// resolveBYOK looks up every usable credential the org holds, keyed by vendor.
//
// *Once per request, not once per candidate*: rotation changes the candidate,
// and with it which credential applies, but not the set the org has. Reading
// the set once and choosing from it in memory keeps one database round trip
// where a per-candidate lookup would put one inside the rotation loop.
//
// Keyed by vendor rather than by dialect, because what the organization configured is
// "my account at this platform". Dozens of companies speak the OpenAI dialect,
// so a credential matched by dialect would be offered to whichever of them the
// routing reached -- sending one company's key to another company's endpoint.
//
// Any failure degrades to "no organization credential" rather than failing the
// request: not configured, configured but undecryptable, unreadable from the
// database -- in all of those, serving the request on a shared credential is
// more reasonable than refusing it. The only cost is that this request is
// billed at full price, and that is *visible*: both the invoice and the usage
// row's byok flag show it.
//
// The degradation is per credential, not per request: one key that will not
// decrypt does not take the org's other keys out of use, because they are
// separate credentials at separate platforms.
func (p *Pipeline) resolveBYOK(ctx context.Context, orgID pgtype.UUID) byokChoices {
	rows, err := p.gw.ListActiveBYOKForOrg(ctx, orgID)
	if err != nil || len(rows) == 0 {
		return nil // most orgs have none, which is normal, not an error
	}
	out := make(byokChoices, len(rows))
	for _, row := range rows {
		// The query orders by created_at within a vendor, so the first row for a
		// vendor is the oldest -- stability, not preference.
		if _, seen := out[row.Vendor]; seen {
			continue
		}
		plain, err := p.box.Open(row.SecretEnc, row.ID.Bytes[:])
		if err != nil {
			// Failing to decrypt means the ciphertext does not match the master
			// key -- has the encryption key been replaced? -- which is a
			// configuration accident, not the organization's fault.
			slog.ErrorContext(ctx, "dataplane: decrypting the organization credential failed; requests to this vendor bill as a shared credential",
				"org_provider_key_id", row.ID, "vendor", row.Vendor, "error", err)
			continue
		}
		out[row.Vendor] = byokChoice{
			KeyID:    row.ID,
			Secret:   string(plain),
			BaseURL:  row.BaseUrl.String,
			Fallback: row.AllowFallback,
		}
	}
	return out
}

// credentialFor picks the credential for the paths that do *not* fail over:
// streaming and images.
//
// Those two choose a single candidate and never rotate within the request,
// because there is no failing over once the first byte is out. So they need
// none of attemptOnce's per-hop fallback machinery and only have to answer one
// question: does this hop use the organization's credential or a shared one.
func (p *Pipeline) credentialFor(
	ctx context.Context, route catalog.Route, byok byokChoices,
) (keyID pgtype.UUID, apiKey, baseURL string, byokKeyID pgtype.UUID, gerr *Error) {
	baseURL = route.BaseURL
	// A organization credential only applies to candidates at the same vendor: what
	// the organization configured is "my account at this platform", and that says
	// nothing about another company that happens to speak the same dialect.
	if choice, ok := byok.forVendor(route.ProviderVendor); ok {
		if choice.BaseURL != "" {
			baseURL = choice.BaseURL
		}
		return pgtype.UUID{}, choice.Secret, baseURL, choice.KeyID, nil
	}
	keyID, apiKey, gerr = p.pickKey(ctx, route)
	return keyID, apiKey, baseURL, pgtype.UUID{}, gerr
}

// quoteFor prices the request, branching on whether a organization credential was
// used. list is the model's list price, which sets the invoice; cost is the
// upstream unit price of whoever actually served it, which sets the margin.
//
// It is one function because all three paths -- non-streaming, streaming,
// images -- make the same decision, and three copies would inevitably drift.
// The way that drift shows up is one path charging organization-credential requests
// at full price.
//
// The organization-credential path uses *list only*: the base of a service fee has to
// be a number both sides can recompute, and we do not know what the organization pays
// their upstream -- they have their own discount. The route cost is *our*
// purchase price and has nothing to do with theirs; using it as the base would
// make the same call cost different amounts depending on which provider it
// happened to route to.
func (p *Pipeline) quoteFor(
	list, cost catalog.PriceTable, tok catalog.Tokens,
	usedBYOK bool, rates catalog.Rates, byokFeeBps int64,
) (catalog.Quote, error) {
	if usedBYOK {
		// The discount rides along in rates: a customer on a better rate should
		// pay a lower service fee too, or switching to their own credential
		// would silently cost them their discount.
		return catalog.ComputeBYOK(list, tok, byokFeeBps, rates)
	}
	return catalog.Compute(list, cost, tok, rates)
}

// byokKeyIDIfUsed is the organization credential id for a hop, or the zero value when
// that hop did not use one. It exists so "which credential served" and "did a
// organization credential serve" can never disagree: both read the same field.
func byokKeyIDIfUsed(used bool, choice byokChoice) pgtype.UUID {
	if !used {
		return pgtype.UUID{}
	}
	return choice.KeyID
}

// byokKeyOf gives the organization credential id that belongs in the usage row.
//
// It goes by *whether the hop that succeeded used one*, not by the choice made
// at the start of the request: with fallback allowed, an earlier hop can be
// rejected on the organization's credential and a later one succeed on a shared one.
// That request is billed at full price, and recording the organization credential id
// would make reconciliation read full-price revenue as service-fee revenue.
// It also has to come from the hop rather than from a request-level choice:
// with credentials per vendor there is no single "the organization's key" for a
// request whose candidates span platforms, and the one that served is the only
// one the usage row may name.
func byokKeyOf(res upstreamResult) pgtype.UUID { return res.byokKeyID }

// sharedKeyOf is the mirror of byokKeyOf: the credential from *our* pool that
// served the request, empty when the organization's own was used. The two are
// mutually exclusive by construction, and keeping them in separate columns is
// what lets a query ask "which of my credentials is failing" without first
// having to ask whose credential it was.
func sharedKeyOf(res upstreamResult) pgtype.UUID {
	return sharedKeyIfUsed(res.byok, res.keyID)
}

// sharedKeyIfUsed is the direct form, for the paths that have no
// upstreamResult.
func sharedKeyIfUsed(usedBYOK bool, keyID pgtype.UUID) pgtype.UUID {
	if usedBYOK {
		return pgtype.UUID{}
	}
	return keyID
}

// byokRejected decides whether an upstream response means "this credential
// cannot be used".
//
// Only 401 and 403 count. Timeouts, 429s, 5xx and a wrong model name all leave
// the status *alone*: none of them says the credential is bad, and counting
// them would let one upstream blip mark a organization's credential invalid. The
// connectivity test in the console applies the same rule.
func byokRejected(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// markBYOKInvalid marks the credential invalid and notifies the organization.
//
// Once invalid, routing stops choosing it (the lookup only returns active
// ones), so later requests fall back to a shared credential -- *at full price*.
// That is why the notification is not optional: the organization has to know their
// credential was dropped, or they will discover the price change at the next
// invoice.
func (p *Pipeline) markBYOKInvalid(ctx context.Context, orgID, keyID pgtype.UUID, status int) {
	if err := p.gw.SetOrgProviderKeyStatus(ctx, gwdb.SetOrgProviderKeyStatusParams{
		ID: keyID, OrgID: orgID, Status: "invalid",
	}); err != nil {
		slog.ErrorContext(ctx, "dataplane: marking the organization credential invalid failed", "id", keyID, "error", err)
		return
	}
	slog.WarnContext(ctx, "dataplane: organization credential rejected upstream; marked invalid",
		"org_provider_key_id", keyID, "upstream_status", status)
	if p.notifyBYOK != nil {
		p.notifyBYOK(ctx, orgID, keyID, status)
	}
}

// fallbackQuote is the quote used when billing cannot be computed: settle at the
// held amount.
//
// It is one function because all three paths -- non-streaming, streaming,
// images -- have such a fallback, and they each used to write the Quote literal
// out by hand. Miss a field there and you lose a piece of the snapshot; with
// the discount added there are four of them, and the fields most easily missed
// are exactly the ones that make a usage row recomputable.
func fallbackQuote(estNano int64, rates catalog.Rates) catalog.Quote {
	modelMultiplier := rates.ModelMultiplierBps
	if modelMultiplier == 0 {
		modelMultiplier = 10000
	}
	planMultiplier := rates.PlanMultiplierBps
	if planMultiplier == 0 {
		planMultiplier = 10000
	}
	procurementMultiplier := rates.ProcurementMultiplierBps
	if procurementMultiplier == 0 {
		procurementMultiplier = 10000
	}
	return catalog.Quote{
		ChargedNano:              estNano,
		ModelMultiplierBps:       modelMultiplier,
		PlanMultiplierBps:        planMultiplier,
		ProcurementMultiplierBps: procurementMultiplier,
		FXRate:                   rates.FXRate,
	}
}
