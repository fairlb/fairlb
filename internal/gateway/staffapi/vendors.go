package gwstaffapi

import (
	"context"
	"encoding/json"
	"github.com/fairlb/fairlb/foundation/strutil"

	"github.com/fairlb/fairlb/internal/gateway/catalog"

	"github.com/fairlb/fairlb/foundation/httpx"
)

// The vendor registry, served to the admin interface.
//
// It reads no database. The list ships with the binary, which is what makes it
// safe to treat as a prefill source: every deployment of this version offers the
// same entries, and adding a platform needs no migration and no seed row.
//
// The response is a projection of catalog.Vendors() and nothing more. Any
// filtering here -- "only vendors with a provider", "only ones a key exists
// for" -- would make the create form's options depend on what is already
// configured, which is precisely backwards for the form whose job is to
// configure the first one.
func (s *Server) ListGatewayVendors(ctx context.Context, _ ListGatewayVendorsRequestObject) (ListGatewayVendorsResponseObject, error) {
	if _, err := httpx.RequireUserID(ctx); err != nil {
		return nil, err
	}
	all := catalog.Vendors()
	out := make([]GatewayVendor, 0, len(all))
	for _, v := range all {
		out = append(out, vendorOut(v))
	}
	return ListGatewayVendors200JSONResponse{Items: out}, nil
}

func vendorOut(v catalog.Vendor) GatewayVendor {
	out := GatewayVendor{
		Slug:             v.Slug,
		Label:            v.Label,
		Kind:             GatewayVendorKind(v.Kind),
		Protocols:        v.Protocols,
		DefaultProtocols: v.DefaultProtocols,
		KeyHint:          GatewayVendorKeyHint(v.KeyHint),
		ModelListing:     v.ModelListing,
		DocsUrl:          strutil.Ptr(v.DocsURL),
		ModelIdExample:   strutil.Ptr(v.ModelIDExample),
		RefdataProvider:  strutil.Ptr(v.RefdataProvider),
	}
	// Not omitted when empty: the field is required, and an absent array reads
	// to a client as "not sent" rather than as "this vendor prefills nothing".
	out.BaseUrls = make([]struct {
		Label    *string `json:"label,omitempty"`
		Template bool    `json:"template"`
		Url      string  `json:"url"`
	}, 0, len(v.BaseURLs))
	for _, b := range v.BaseURLs {
		out.BaseUrls = append(out.BaseUrls, struct {
			Label    *string `json:"label,omitempty"`
			Template bool    `json:"template"`
			Url      string  `json:"url"`
		}{Label: strutil.Ptr(b.Label), Template: b.Template, Url: b.URL})
	}
	if len(v.Fidelity) > 0 {
		f := make(map[string]GatewayVendorFidelity, len(v.Fidelity))
		for k, val := range v.Fidelity {
			f[k] = GatewayVendorFidelity(val)
		}
		out.Fidelity = &f
	}
	// The preset travels as the same object shape the provider record uses, so
	// the create form can hand what it received straight back on save.
	//
	// Marshalling cannot fail here -- Transport is a struct of strings and maps
	// of strings -- and a preset that would not survive the write path's own
	// validation is caught by TestVendorPresetsPassTheWritePathValidation
	// rather than by a runtime branch nobody can reach.
	if raw, err := json.Marshal(v.Transport); err == nil {
		out.Transport = transportOut(raw)
	}
	return out
}
