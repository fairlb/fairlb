// Package brand applies a deployment's brand to what the binary serves.
//
// It holds two things: the one brand value the backend itself needs -- the
// display name, for mail signatures and the issuer in an authenticator app --
// and the overlay that puts a mounted profile bundle in front of the SPA build
// embedded in the artifact (Serve, in serve.go).
//
// The brand used to be a build input for all four Web surfaces (ADR-0147), and
// the image build wrote the name in with `-ldflags -X`. That made a product line
// an image: the same source had to be built once per brand, and staging ran a
// different binary from the one production would run. Marketing is still built
// per brand -- a static site has no runtime to resolve one in -- but everything
// this binary serves resolves its brand at startup instead, from
// BRAND_PROFILE_DIR (ADR-0214).
package brand

// Name is the brand's display name as shown to people: mail signatures, the
// issuer in an authenticator app. The default is the default profile's name;
// Serve reports the name of the profile actually being served and the entry
// point assigns it here, once, before anything is served.
//
// **Read it where it is used, never into a package-level variable.** Package
// initialization runs before configuration is loaded, so a copy taken at init
// captures this default and keeps it forever -- and the failure is silent:
// every mail still sends, just signed with the wrong name.
//
// Machine-facing identifiers -- cookie names, key prefixes, module paths -- are
// not derived from it.
var Name = "FairLB"
