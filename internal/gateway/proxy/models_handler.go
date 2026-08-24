package proxy

import (
	"log/slog"
	"net/http"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// The authenticated GET /v1/models.
//
// *It lives in proxy rather than in catalog*, for three reasons:
//  1. The dependency direction is already fixed -- proxy depends on catalog,
//     because the pipeline calls its resolver -- so catalog cannot import
//     proxy. Giving catalog this handler would mean inverting an
//     authentication interface, and it still would not have the per-dialect
//     error renderers.
//  2. Consistency: every other authenticated dataplane endpoint's handler is in
//     this package.
//  3. Correct errors: an authenticated endpoint has to be able to answer 401
//     and 403, which needs this package's per-dialect rendering table. The
//     catalog only has a stub covering its own failures.
//
// Rendering still reuses the catalog's model-list writer: the two catalogue
// endpoints must produce exactly the same response shape -- a client should be
// able to point at a different base URL and carry on -- and a second copy of the
// rendering would drift sooner or later.
//
// The catalogue is the set you can call, so it applies the *same three-way
// narrowing* the dataplane resolver does: globally available (the query already
// requires public, enabled, and at least one usable route), intersected with the
// admission tier, intersected with the key's allowlist. Drop any one of the
// three and a organization sees a model in the catalogue that is certain to 404 when
// called.
func ModelsHandler(auth *Authenticator, guard *Guard, cat *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Errors render on the OpenAI surface: /v1/models is an
		// OpenAI-compatible endpoint, and unlike the proxying endpoints its
		// surface is not decided by the request path.
		const surface = SurfaceOpenAI

		id, gerr := auth.Authenticate(ctx, CredentialOf(r))
		if gerr != nil {
			Write(w, surface, gerr)
			return
		}
		// An unusable admission tier -- disabled, or unresolvable -- fails
		// closed, the same way the dataplane does. It does not return an empty
		// list: an empty list reads as "this deployment has no models", when
		// what is really true is that this account's admission configuration is
		// not in effect, and the two call for entirely different responses.
		if gerr := guard.CheckTier(id); gerr != nil {
			Write(w, surface, gerr)
			return
		}

		models, err := cat.ModelsForOrg(ctx, id.ModelTierID, id.OrgID)
		if err != nil {
			slog.ErrorContext(ctx, "dataplane: reading the organization catalogue failed", "error", err)
			Write(w, surface, NewError(errcode.GatewayInternal, "Internal server error"))
			return
		}

		// Unit prices render at *what this organization actually pays*: the markup
		// plus their own discount. Showing list price instead would leave a
		// discounted customer seeing one price in the catalogue and another on
		// the invoice.
		cat.WriteModelList(ctx, w, filterByKeyAllowlist(id, models), catalog.Rates{})
	}
}

// filterByKeyAllowlist applies the innermost of the three intersections, the
// key's own model gate.
//
// Its semantics match Guard.CheckModel exactly: unrestricted lists everything,
// otherwise the exact slugs and nothing else, with no wildcards -- and a key
// restricted to an empty set lists nothing, because it can call nothing. The
// two must agree: the catalogue saying yes while the gate says no is a support
// ticket, and the reverse is a model the organization is paying for and cannot find.
func filterByKeyAllowlist(id Identity, models []catalog.PublicModel) []catalog.PublicModel {
	if id.AllowAllModels {
		return models
	}
	set := make(map[string]struct{}, len(id.AllowedModels))
	for _, m := range id.AllowedModels {
		set[m] = struct{}{}
	}
	out := make([]catalog.PublicModel, 0, len(models))
	for _, m := range models {
		if _, ok := set[m.Slug]; ok {
			out = append(out, m)
		}
	}
	return out
}
