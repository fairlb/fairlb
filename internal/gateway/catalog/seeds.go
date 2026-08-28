package catalog

import "regexp"

// The seeded catalog: the models an operator is most likely to want, with the
// identity and display metadata already spelled correctly.
//
// Why this exists. A model's catalog slug is `<creator>/<name>` and cannot be
// changed once created, while an upstream reports a bare name -- so every
// deployment used to begin by transcribing a table out of a runbook, by hand,
// into a form, sixteen times. Transcription is where characters get dropped,
// and a dropped character in a slug is permanent.
//
// What a seed carries, and what it deliberately does not:
//
//   - Identity and display metadata: slug, display name, the two token
//     windows, and the bare name the upstream answers to.
//   - No price. Prices come from the bundled reference dataset, which records
//     provenance the seed could not: which buckets the dataset does not price
//     and what the stored zero means at billing time, whether the value was
//     rounded, whether the model is priced in steps by prompt size. A price
//     copied to a second place is a price that will drift from the first, and
//     the operator's own verification is what turns either one into a rate we
//     are willing to bill against.
//
// Admission criteria, which decide whether a new entry belongs here:
//
//   - Current, on sale, and not fully superseded by a newer model from the
//     same vendor. Everything else remains creatable by hand.
//   - No dated snapshot names. They pin a slug to a date that stops being
//     true, and the slug cannot be changed afterwards.
//   - A model whose output is not text only once its usage *and* its price are
//     modelled end to end. Image models qualify now that both sides are
//     metered and priceable (BucketImageIn, BucketImageOut); audio, realtime
//     and embedding models still do not, because their tokens would be counted
//     against the text rate, and seeding a model that bills wrong turns one
//     mistake into every operator's mistake.
//   - Priceable from the bundled dataset, which TestSeedUpstreamIDsAreAllPriceable
//     enforces: a mistyped upstream id has to fail to match a third party's
//     spelling, and that test is the only thing standing between a typo here
//     and a catalog entry that looks right while its one-click pricing quietly
//     does nothing.
//
// # Why the video models are not here
//
// Not for want of a price any more. Per-unit rates now have a bundled prefill
// of their own (pricing/refdata/unit-rates.json), and the import reaches them
// through the same button.
//
// The reason is the criterion above, and it is worth stating because it is the
// one that stopped applying. That list is maintained in this repository and
// matched on a prefix, so keying this table to it would check our own spelling
// against our own spelling -- no external cross-check, and the typo the
// criterion exists to catch would sail through. Meanwhile the ids themselves
// are the part we are least able to spell: one vendor stamps a release date
// into them, another accepts an endpoint id instead.
//
// So video models are created from the upstream's own listing, where the id
// comes from the vendor rather than from a guess, and the per-unit prefill
// prices whatever was wired. That is the same outcome this table exists to
// produce, reached by the one route that cannot be wrong about the id.
//
// Checked 2026-08-24 against:
//
//   - https://developers.openai.com/api/docs/models
//   - https://platform.claude.com/docs/en/about-claude/models/overview
//   - https://ai.google.dev/gemini-api/docs/models
//
// The model ids themselves are cross-checked mechanically: TestSeedModels
// asserts every UpstreamID resolves to exactly one price in the bundled
// dataset, so a typo here fails the build rather than silently costing the
// operator the one-click pricing this table exists to enable.

// SeedModel is one catalog entry the operator can create without typing it.
type SeedModel struct {
	// Slug is the catalog identity, `<creator>/<name>`.
	Slug string
	// DisplayName is the vendor's own spelling. It does not go through the
	// message catalog, for the same reason Vendor.Label does not: a product
	// name is not translated.
	DisplayName string
	// ContextWindow and MaxOutputTokens are zero when the vendor publishes
	// neither, which is the case for image models. Zero here means "not
	// stated" and must not be presented as a limit of zero.
	ContextWindow   int32
	MaxOutputTokens int32
	// UpstreamID is the bare name the upstream answers to.
	UpstreamID string
	// Vendors are the vendors whose upstream spells this model *exactly*
	// UpstreamID.
	//
	// Platforms are deliberately absent even when they serve the model:
	// Bedrock spells Claude "anthropic.claude-...-v1:0" and Vertex spells it
	// "claude-...@date", so listing them would hand an operator an upstream
	// name we already know to be wrong. Those providers reach these models
	// from the model side of the wiring editor, by picking the catalog entry
	// this table created and typing the platform's own id -- which is the one
	// thing only they know.
	Vendors []string
	// ManualProbe marks a model reachable only on an endpoint the gateway
	// refuses to probe on its own (images). Such a model is not a routing
	// candidate until somebody probes it or records an operator verdict, so
	// creating one without saying this leaves a catalog entry that looks
	// finished and answers 404.
	ManualProbe bool
	// OutputModalities is what the model produces (ADR-0226). Empty means text,
	// which is what the column defaults to.
	//
	// It cannot be derived from the endpoints this model is reached on: Gemini
	// serves its image models on generate_content, the same endpoint as its
	// text models, so a seed that left this to inference would file every
	// Google image model under text.
	OutputModalities []Modality
}

// SeedModels returns the seeded catalog.
//
// Built per call rather than kept in a package variable: the entries carry
// slices, and a shared copy would let one caller's edit reach every other.
func SeedModels() []SeedModel {
	openai := []string{"openai"}
	anthropic := []string{"anthropic"}
	google := []string{"google"}
	return []SeedModel{
		// GPT. The 5.6 family covers flagship, workhorse and light; 5.4 stays
		// because clients still name it, and the codex model is a specialist
		// rather than a smaller version of anything.
		{
			Slug: "openai/gpt-5.6-sol", DisplayName: "GPT-5.6 Sol",
			ContextWindow: 1_050_000, MaxOutputTokens: 128_000,
			UpstreamID: "gpt-5.6-sol", Vendors: openai,
		},
		{
			Slug: "openai/gpt-5.6-terra", DisplayName: "GPT-5.6 Terra",
			ContextWindow: 1_050_000, MaxOutputTokens: 128_000,
			UpstreamID: "gpt-5.6-terra", Vendors: openai,
		},
		{
			Slug: "openai/gpt-5.6-luna", DisplayName: "GPT-5.6 Luna",
			ContextWindow: 1_050_000, MaxOutputTokens: 128_000,
			UpstreamID: "gpt-5.6-luna", Vendors: openai,
		},
		{
			Slug: "openai/gpt-5.5-pro", DisplayName: "GPT-5.5 Pro",
			ContextWindow: 1_050_000, MaxOutputTokens: 128_000,
			UpstreamID: "gpt-5.5-pro", Vendors: openai,
		},
		{
			Slug: "openai/gpt-5.4", DisplayName: "GPT-5.4",
			ContextWindow: 1_050_000, MaxOutputTokens: 128_000,
			UpstreamID: "gpt-5.4", Vendors: openai,
		},
		{
			Slug: "openai/gpt-5.3-codex", DisplayName: "GPT-5.3 Codex",
			ContextWindow: 400_000, MaxOutputTokens: 128_000,
			UpstreamID: "gpt-5.3-codex", Vendors: openai,
		},
		{
			// Both windows are zero because the vendor publishes neither: this
			// model is reached on the image endpoints, which take no
			// max_tokens and state no context limit. Inventing a number to
			// fill the columns would put a fabricated limit in front of a
			// pre-authorization estimate.
			Slug: "openai/gpt-image-2", DisplayName: "GPT Image 2",
			UpstreamID: "gpt-image-2", Vendors: openai, ManualProbe: true,
			OutputModalities: []Modality{ModalityImage},
		},

		// Claude.
		{
			Slug: "anthropic/claude-fable-5", DisplayName: "Claude Fable 5",
			ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
			UpstreamID: "claude-fable-5", Vendors: anthropic,
		},
		{
			Slug: "anthropic/claude-opus-5", DisplayName: "Claude Opus 5",
			ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
			UpstreamID: "claude-opus-5", Vendors: anthropic,
		},
		{
			Slug: "anthropic/claude-sonnet-5", DisplayName: "Claude Sonnet 5",
			ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
			UpstreamID: "claude-sonnet-5", Vendors: anthropic,
		},
		{
			Slug: "anthropic/claude-opus-4-8", DisplayName: "Claude Opus 4.8",
			ContextWindow: 1_000_000, MaxOutputTokens: 128_000,
			UpstreamID: "claude-opus-4-8", Vendors: anthropic,
		},
		{
			// The light tier of the previous generation: there is no Haiku 5.
			Slug: "anthropic/claude-haiku-4-5", DisplayName: "Claude Haiku 4.5",
			ContextWindow: 200_000, MaxOutputTokens: 64_000,
			UpstreamID: "claude-haiku-4-5", Vendors: anthropic,
		},

		// Gemini. The -preview suffix is kept rather than trimmed: Google ships
		// its Pro line under it for long stretches, and the trimmed name is not
		// a model the API answers to. A slug cannot be renamed later, so the
		// name that works is the one to store.
		{
			Slug: "google/gemini-3.1-pro-preview", DisplayName: "Gemini 3.1 Pro",
			ContextWindow: 1_048_576, MaxOutputTokens: 65_536,
			UpstreamID: "gemini-3.1-pro-preview", Vendors: google,
		},
		{
			Slug: "google/gemini-3.7-flash", DisplayName: "Gemini 3.7 Flash",
			ContextWindow: 1_048_576, MaxOutputTokens: 65_536,
			UpstreamID: "gemini-3.7-flash", Vendors: google,
		},
		{
			Slug: "google/gemini-3.5-flash", DisplayName: "Gemini 3.5 Flash",
			ContextWindow: 1_048_576, MaxOutputTokens: 65_536,
			UpstreamID: "gemini-3.5-flash", Vendors: google,
		},
		{
			Slug: "google/gemini-3.1-flash-lite", DisplayName: "Gemini 3.1 Flash Lite",
			ContextWindow: 1_048_576, MaxOutputTokens: 65_536,
			UpstreamID: "gemini-3.1-flash-lite", Vendors: google,
		},
		{
			Slug: "google/gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro",
			ContextWindow: 1_048_576, MaxOutputTokens: 65_536,
			UpstreamID: "gemini-2.5-pro", Vendors: google,
		},
		{
			Slug: "google/gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash",
			ContextWindow: 1_048_576, MaxOutputTokens: 65_536,
			UpstreamID: "gemini-2.5-flash", Vendors: google,
		},
		{
			// Google's image line, reached on generate_content like every other
			// Gemini model -- which is exactly why the modality is declared
			// here rather than inferred from the endpoint.
			//
			// Both windows are zero because the vendor publishes neither for
			// its image models, the same as gpt-image-2 above. It is not
			// ManualProbe: this model is reached on generate_content, which the
			// gateway probes for free, not on the images endpoint.
			Slug: "google/gemini-3.1-flash-image", DisplayName: "Gemini 3.1 Flash Image",
			UpstreamID: "gemini-3.1-flash-image", Vendors: google,
			OutputModalities: []Modality{ModalityText, ModalityImage},
		},
	}
}

// LookupSeed finds the seed a given vendor serves under a given upstream name.
//
// Both halves are required. The same bare name means different things on
// different platforms, and a seed matched on the name alone would answer for a
// provider whose upstream spells it some other way entirely.
func LookupSeed(vendor, upstreamID string) (SeedModel, bool) {
	if vendor == "" || upstreamID == "" {
		return SeedModel{}, false
	}
	for _, m := range SeedModels() {
		if m.UpstreamID != upstreamID {
			continue
		}
		for _, v := range m.Vendors {
			if v == vendor {
				return m, true
			}
		}
	}
	return SeedModel{}, false
}

// modelSlugShape mirrors the models_slug_shape constraint in the migration.
//
// The database is the authority and every write path relies on it; this copy
// exists for the two jobs a constraint cannot do. It lets discovery decide
// whether a name it is about to *suggest* would be accepted -- offering a
// prefill that fails on save is worse than offering none -- and it lets the
// seed table be checked without a database. It is deliberately the only copy
// in Go, and TestModelSlugMirrorAgreesWithTheDatabase asserts the two verdicts
// never diverge.
var modelSlugShape = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*/[a-z0-9]+([._-][a-z0-9]+)*$`)

// ValidModelSlug reports whether the database would accept this slug.
func ValidModelSlug(s string) bool { return modelSlugShape.MatchString(s) }
