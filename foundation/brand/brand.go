// Package brand is the backend's copy of the one brand value the Go side needs:
// the display name. The four Web surfaces take theirs from BrandProfileV1 at
// build time (ADR-0147); the backend used to carry the default name as two
// constants — the transactional mail signature and the TOTP issuer — which a
// white-label build could not reach. Now the image build reads the profile's
// name and sets it here with -ldflags -X, so mail and authenticator apps say
// the same name as the pages.
package brand

// Name is the brand's display name as shown to people: mail signatures, the
// issuer in an authenticator app. It is a variable, not a constant, so the
// linker can set it (`-X github.com/fairlb/fairlb/foundation/brand.Name=…`);
// the default is the default profile's name. Machine-facing identifiers —
// cookie names, key prefixes, module paths — are not derived from it.
var Name = "FairLB"
