package catalogadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/db"
	fdb "github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

const (
	// MultiplierBpsMin and MultiplierBpsMax bound a provider's cost multiplier.
	// 10000 is cost; below 1 or above 100000 the number stops being a markup and
	// starts being a typo with a bill attached.
	MultiplierBpsMin = 1
	MultiplierBpsMax = 100_000
)

// OutOfRangeError is a well-formed value the schema will not hold.
//
// Separate from InvalidError because the transport answers it differently: the
// request parsed and its fields are the right shape, so it is 422 rather than
// 400. The provider cost multiplier has answered 422 since it existed, and a
// refactor is not a reason to change what an operator's tooling sees.
type OutOfRangeError struct{ Message string }

func (e OutOfRangeError) Error() string { return "catalogadmin: " + e.Message }

// Provider is one configured upstream.
//
// KeyCount and RouteCount are only filled by the reads that ask for them; the
// write path returns the row it just wrote, which does not carry them. They are
// counts of other tables, not columns of this one.
type Provider struct {
	ID                uuid.UUID
	Slug              string
	Vendor            string
	Protocols         []string
	Name              string
	BaseURL           string
	Enabled           bool
	AutoDisabled      bool
	Headers           map[string]string
	Transport         map[string]any
	CostMultiplierBps int32
	RateLimitRPM      *int32
	RateLimitTPM      *int32
	MaxConcurrency    int32
	KeyCount          int64
	RouteCount        int64
}

// ProviderCreate is a new provider. Every field the database has no default for
// is required, and CreateProvider says which one is missing rather than letting
// a NOT NULL violation answer for it.
type ProviderCreate struct {
	Slug              string
	Vendor            string
	Protocols         []string
	BaseURL           string
	Name              string
	Headers           *map[string]string
	Transport         *map[string]any
	CostMultiplierBps *int32
	RateLimitRPM      *int32
	RateLimitTPM      *int32
	MaxConcurrency    *int32
}

// ProviderPatch is a partial update: a nil field is "leave this alone".
type ProviderPatch struct {
	Vendor            *string
	Protocols         *[]string
	BaseURL           *string
	Name              *string
	Enabled           *bool
	Headers           *map[string]string
	Transport         *map[string]any
	CostMultiplierBps *int32
	RateLimitRPM      *int32
	RateLimitTPM      *int32
	MaxConcurrency    *int32
	// ClearRateLimitRPM and ClearRateLimitTPM remove a ceiling. Absent and
	// "set back to no limit" are different intentions and a partial update
	// cannot express the second one with a nil.
	ClearRateLimitRPM bool
	ClearRateLimitTPM bool
}

// Providers lists configured upstreams, one page at a time, with the counts the
// admin list shows next to each one.
//
// Keyed on slug, which is what the list is ordered by and — being UNIQUE —
// already a total order. search matches slug or display name, and it runs in the
// database because the pickers built on this list have to find a provider in the
// whole registry (ADR-0187).
func (s *Service) Providers(
	ctx context.Context, search string, page cursorpage.KeyPage,
) ([]Provider, error) {
	rows, err := s.q.ListProvidersForAdmin(ctx, gwdb.ListProvidersForAdminParams{
		Search: fdb.SearchTerm(search), HasCursor: page.HasKey(), CursorSlug: page.At(0),
		Lim: page.ProbeLimit(),
	})
	if err != nil {
		return nil, fmt.Errorf("catalogadmin: list providers: %w", err)
	}
	out := make([]Provider, 0, len(rows))
	for _, r := range rows {
		out = append(out, providerFrom(r))
	}
	return out, nil
}

// ProviderCursor points just past p. SlugCursorParts is what the transport hands
// to ParseKeyPage.
func ProviderCursor(p Provider) string { return cursorpage.EncodeKey(p.Slug) }

// SlugCursorParts is the component count for every slug-keyed catalog list here.
const SlugCursorParts = 1

// Provider reads one upstream.
//
// It reads its own row rather than picking an entry out of the capped list,
// which would silently miss anything past the cap.
func (s *Service) Provider(ctx context.Context, id uuid.UUID) (Provider, error) {
	row, err := s.q.GetProviderDetailForAdmin(ctx, pgID(id))
	if err != nil {
		return Provider{}, ErrNotFound
	}
	// The two queries return identical columns, so this converts the struct
	// instead of writing a second mapping: the moment the two column sets
	// diverge, this conversion stops compiling.
	return providerFrom(gwdb.ListProvidersForAdminRow(row)), nil
}

// CreateProvider adds an upstream.
func (s *Service) CreateProvider(ctx context.Context, in ProviderCreate) (Provider, error) {
	if in.Slug == "" || in.Vendor == "" || in.BaseURL == "" {
		return Provider{}, invalid("slug, vendor and base_url are required")
	}
	protocols, err := DefaultProtocols(in.Vendor, in.Protocols)
	if err != nil {
		return Provider{}, err
	}
	if err := CheckVendorSpeaks(in.Vendor, protocols); err != nil {
		return Provider{}, err
	}
	if err := CheckBaseURLIsFinished(in.BaseURL); err != nil {
		return Provider{}, err
	}
	transport, err := encodeTransport(in.Transport)
	if err != nil {
		return Provider{}, err
	}
	if err := checkTransportRules(transport, protocols); err != nil {
		return Provider{}, err
	}
	if err := CheckMultiplierBps(in.CostMultiplierBps); err != nil {
		return Provider{}, err
	}
	row, err := s.q.CreateProvider(ctx, gwdb.CreateProviderParams{
		Slug: in.Slug, Vendor: in.Vendor, Protocols: protocols, BaseUrl: in.BaseURL,
		Name: in.Name, Headers: encodeHeaders(in.Headers),
		Transport:         transport,
		CostMultiplierBps: int4(in.CostMultiplierBps),
		RateLimitRpm:      int4(in.RateLimitRPM),
		RateLimitTpm:      int4(in.RateLimitTPM),
		MaxConcurrency:    int4(in.MaxConcurrency),
	})
	if err != nil {
		if db.IsUniqueViolation(err) {
			return Provider{}, ConflictError{Message: "That slug is already taken"}
		}
		return Provider{}, fmt.Errorf("catalogadmin: create provider: %w", err)
	}
	s.invalidate(ctx)
	return providerFromWriteRow(providerWriteRow(row)), nil
}

// UpdateProvider partially updates an upstream.
//
// Enabling one by hand clears the auto_disabled flag (done in SQL): an explicit
// operator action takes ownership of that provider, after which health checks no
// longer flip it on or off by themselves. Without that, a background job would
// quietly overwrite a human decision.
func (s *Service) UpdateProvider(ctx context.Context, id uuid.UUID, in ProviderPatch) (Provider, error) {
	var protocols []string
	// protocolsChanged says whether the stored set actually moves. A client
	// echoing the current set (the contract lets it) must not realign and
	// re-probe every route on the provider for nothing.
	protocolsChanged := false
	if in.Protocols != nil {
		var err error
		if protocols, err = ValidProtocols(*in.Protocols); err != nil {
			return Provider{}, err
		}
	}
	// The vendor and the dialects constrain each other, so a partial update of
	// either has to be checked against the stored value of the other -- moving a
	// provider to a vendor that does not speak its current dialects is the same
	// mistake as declaring those dialects at creation, and it arrives by a route
	// that touches neither field the check is named after.
	if in.Vendor != nil || in.Protocols != nil {
		cur, err := s.q.GetProviderForAdmin(ctx, pgID(id))
		if err != nil {
			return Provider{}, ErrNotFound
		}
		vendor := cur.Vendor
		if in.Vendor != nil {
			vendor = *in.Vendor
		}
		declared := cur.Protocols
		if protocols != nil {
			declared = protocols
			protocolsChanged = !sameSet(cur.Protocols, protocols)
		}
		if err := CheckVendorSpeaks(vendor, declared); err != nil {
			return Provider{}, err
		}
	}
	if in.BaseURL != nil {
		if err := CheckBaseURLIsFinished(*in.BaseURL); err != nil {
			return Provider{}, err
		}
	}
	transport, err := encodeTransport(in.Transport)
	if err != nil {
		return Provider{}, err
	}
	if len(transport) > 0 {
		// Against the dialects this provider will have once this update lands,
		// which is the stored set unless this same request replaces it.
		declared := protocols
		if declared == nil {
			cur, cErr := s.q.GetProviderForAdmin(ctx, pgID(id))
			if cErr != nil {
				return Provider{}, ErrNotFound
			}
			declared = cur.Protocols
		}
		if err := checkTransportRules(transport, declared); err != nil {
			return Provider{}, err
		}
	}
	if err := CheckMultiplierBps(in.CostMultiplierBps); err != nil {
		return Provider{}, err
	}
	// A protocol change realigns the probe rows of every route on this
	// provider in the same transaction as the update: a protocol it stops
	// speaking takes its rows with it, one it starts speaking gets rows seeded
	// and probed. Committing one without the other leaves a record that
	// disagrees with itself.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Provider{}, fmt.Errorf("catalogadmin: begin provider update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := s.q.WithTx(tx).UpdateProvider(ctx, gwdb.UpdateProviderParams{
		ID:                pgID(id),
		Name:              textOrNull(in.Name),
		Vendor:            textOrNull(in.Vendor),
		Protocols:         protocols,
		BaseUrl:           textOrNull(in.BaseURL),
		Enabled:           boolOrNull(in.Enabled),
		Headers:           encodeHeadersOrNull(in.Headers),
		Transport:         transport,
		CostMultiplierBps: int4(in.CostMultiplierBps),
		RateLimitRpm:      int4(in.RateLimitRPM),
		RateLimitTpm:      int4(in.RateLimitTPM),
		MaxConcurrency:    int4(in.MaxConcurrency),
		ClearRateLimitRpm: in.ClearRateLimitRPM,
		ClearRateLimitTpm: in.ClearRateLimitTPM,
	})
	if err != nil {
		return Provider{}, fmt.Errorf("catalogadmin: update provider: %w", err)
	}
	if protocolsChanged {
		if err := s.probes.ReseedProvider(ctx, tx, pgID(id), protocols); err != nil {
			return Provider{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Provider{}, fmt.Errorf("catalogadmin: commit provider update: %w", err)
	}
	s.invalidate(ctx)
	return providerFromWriteRow(providerWriteRow(row)), nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// DefaultProtocols resolves the protocol set a new provider is created with.
//
// Left empty, it is the vendor's default -- the protocols the registry's
// preset base URL and transport profile are wired for, which for most vendors
// is simply "the protocol it speaks". That is the operator's whole answer for
// a known vendor: picking OpenAI has already said openai. A custom upstream
// has no registry entry to answer for it and must say so itself.
func DefaultProtocols(vendor string, declared []string) ([]string, error) {
	if len(declared) > 0 {
		return ValidProtocols(declared)
	}
	v, ok := catalog.LookupVendor(vendor)
	if !ok {
		return nil, invalid("Unknown vendor: %s. Pick one from GET /gateway/vendors, or %s "+
			"for an upstream this build has no entry for.", vendor, catalog.VendorCustom)
	}
	if len(v.DefaultProtocols) == 0 {
		return nil, invalid("protocols must be given for a %s provider: there is no registry "+
			"entry to say which it speaks", vendor)
	}
	return ValidProtocols(v.DefaultProtocols)
}

// ValidProtocols validates the set of protocol dialects and converts it to
// strings. The database CHECK is the last line of defence; this is the one that
// can say what was wrong.
func ValidProtocols(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, invalid("protocols must not be empty; a provider has to speak at least one")
	}
	out := make([]string, 0, len(in))
	for _, f := range in {
		if _, ok := catalog.ProtocolEndpoints(f); !ok {
			return nil, invalid("Unknown protocol dialect: %s", f)
		}
		out = append(out, f)
	}
	return out, nil
}

// CheckVendorSpeaks refuses a provider whose vendor does not publish the
// dialects it declares.
//
// The registry is the authority on this, not the database: a dialect a platform
// does not serve produces routes that save without complaint, never become
// candidates, and show as configured. Catching it here means catching it while
// the person who chose it is still looking at the form.
func CheckVendorSpeaks(vendor string, protocols []string) error {
	v, ok := catalog.LookupVendor(vendor)
	if !ok {
		return invalid("Unknown vendor: %s. Pick one from GET /gateway/vendors, or %s "+
			"for an upstream this build has no entry for.", vendor, catalog.VendorCustom)
	}
	if err := v.CheckProtocols(protocols); err != nil {
		return InvalidError{Message: err.Error()}
	}
	return nil
}

// CheckBaseURLIsFinished refuses a base URL still carrying a preset's
// placeholder.
//
// Several vendors' endpoints contain a resource name or a region that has no
// default -- the registry marks those, and the create form asks for them. One
// that reaches the data plane unreplaced fails as a DNS error, which reads like
// the upstream being down rather than like a form nobody finished.
func CheckBaseURLIsFinished(baseURL string) error {
	if strings.ContainsAny(baseURL, "{}") {
		return invalid("The base URL still contains a placeholder from the vendor preset: %s. "+
			"Replace it with this deployment's own value.", baseURL)
	}
	return nil
}

// CheckMultiplierBps bounds a cost multiplier. A nil value is "not being set".
func CheckMultiplierBps(v *int32) error {
	if v == nil {
		return nil
	}
	if *v < MultiplierBpsMin || *v > MultiplierBpsMax {
		return OutOfRangeError{Message: fmt.Sprintf(
			"cost_multiplier_bps must be between %d and %d", MultiplierBpsMin, MultiplierBpsMax)}
	}
	return nil
}

// checkTransportRules holds the two rules that need the provider's declared
// dialects, which ValidateTransport cannot see.
//
// They live here rather than in ValidateTransport because that function
// validates a profile in isolation and is also the *read* path, which must stay
// lenient: a stored profile that a later rule would reject has to keep serving,
// or one tightened rule takes the data plane down on deploy.
func checkTransportRules(raw []byte, protocols []string) error {
	if len(raw) == 0 {
		return nil
	}
	transport, err := catalog.ParseTransport(raw)
	if err != nil {
		return nil // encodeTransport already refused anything unparseable
	}
	if err := checkTransportIsFinished(transport); err != nil {
		return err
	}
	return checkEnvelopeFitsProtocols(protocols, transport)
}

// checkTransportIsFinished refuses a profile still carrying a preset's
// placeholders.
//
// The sibling of CheckBaseURLIsFinished, and needed for the same reason: the
// hosted-platform presets ship a project and a region inside their path
// overrides, and only `{model}` is ever substituted. One left unreplaced reaches
// the upstream percent-escaped, 404s every request, is classified as the
// provider failing and opens its circuit -- an outage's shape for a form nobody
// finished. Guarding the base URL and not this left the placeholders that
// actually ship unguarded.
func checkTransportIsFinished(transport catalog.Transport) error {
	for _, table := range []map[string]string{transport.PathOverrides, transport.StreamPathOverrides} {
		for key, dest := range table {
			if left := unsubstitutedPlaceholder(dest); left != "" {
				return invalid("The path override for %s still contains a placeholder from "+
					"the vendor preset: %s. Replace it with this deployment's own value.", key, left)
			}
		}
	}
	return nil
}

// unsubstitutedPlaceholder returns the first `{...}` that is not one the
// gateway fills in, or "" when the value is finished. Model and state-resource
// identifiers are both substituted per request.
func unsubstitutedPlaceholder(dest string) string {
	rest := dest
	for {
		open := strings.Index(rest, "{")
		if open < 0 {
			return ""
		}
		shut := strings.Index(rest[open:], "}")
		if shut < 0 {
			return rest[open:]
		}
		token := rest[open : open+shut+1]
		if token != "{model}" && token != "{resource}" {
			return token
		}
		rest = rest[open+shut+1:]
	}
}

// checkEnvelopeFitsProtocols refuses an envelope on a provider that does not
// speak the dialect the envelope re-cuts.
//
// Both envelopes are re-cuts of the Anthropic Messages request: they move the
// model out of the body and an api-version into it. Declared on an OpenAI or
// Gemini provider the profile saves, and then every forwarded request has its
// model deleted and an anthropic_version added -- the upstream answers 400 and
// the failure is attributed to it. The registry's own presets were already held
// to this rule by a test; the path where an operator types a profile was not.
func checkEnvelopeFitsProtocols(protocols []string, transport catalog.Transport) error {
	if transport.BodyEnvelope() == catalog.EnvelopeNone {
		return nil
	}
	for _, p := range protocols {
		if p != catalog.ProtocolAnthropic {
			return invalid("The %s envelope re-cuts an Anthropic Messages request, so it cannot "+
				"be declared on a provider that speaks %s. That platform's other dialects belong "+
				"to a separate provider record.", transport.BodyEnvelope(), p)
		}
	}
	return nil
}

// encodeTransport validates a transport profile on its way in and returns the
// bytes to store; nil means "leave this alone", which is what a partial update
// omitting the field asks for.
//
// Validation is strict here and nowhere else. A profile the gateway cannot act
// on has to be refused in front of the person who typed it: rejected later it
// would be a data-plane failure attributed to the upstream, and accepted
// silently it would be a setting that saves and does nothing.
func encodeTransport(m *map[string]any) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	raw, err := json.Marshal(*m)
	if err != nil {
		return nil, invalid("The transport profile could not be encoded: %v", err)
	}
	if _, err := catalog.ValidateTransport(raw); err != nil {
		return nil, InvalidError{Message: err.Error()}
	}
	return raw, nil
}

// encodeHeaders encodes the header map as jsonb, defaulting to an empty object.
func encodeHeaders(m *map[string]string) []byte {
	if m == nil {
		return []byte(`{}`)
	}
	b, err := json.Marshal(*m)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

// encodeHeadersOrNull is for partial updates: nil means "leave this alone".
func encodeHeadersOrNull(m *map[string]string) []byte {
	if m == nil {
		return nil
	}
	return encodeHeaders(m)
}

// providerWriteRow is the column set the write queries return: the provider's
// own columns, without the counts of other tables that the reads join in.
type providerWriteRow struct {
	ID                pgtype.UUID
	Slug              string
	Vendor            string
	Protocols         []string
	Name              string
	BaseUrl           string
	Enabled           bool
	AutoDisabled      bool
	Headers           []byte
	Transport         []byte
	CostMultiplierBps int32
	RateLimitRpm      pgtype.Int4
	RateLimitTpm      pgtype.Int4
	MaxConcurrency    int32
}

func providerFrom(r gwdb.ListProvidersForAdminRow) Provider {
	out := providerFromWriteRow(providerWriteRow{
		ID: r.ID, Slug: r.Slug, Vendor: r.Vendor, Protocols: r.Protocols,
		Name: r.Name, BaseUrl: r.BaseUrl, Enabled: r.Enabled, AutoDisabled: r.AutoDisabled,
		Headers: r.Headers, Transport: r.Transport, CostMultiplierBps: r.CostMultiplierBps,
		RateLimitRpm: r.RateLimitRpm, RateLimitTpm: r.RateLimitTpm,
		MaxConcurrency: r.MaxConcurrency,
	})
	out.KeyCount, out.RouteCount = r.KeyCount, r.RouteCount
	return out
}

func providerFromWriteRow(r providerWriteRow) Provider {
	return Provider{
		ID: r.ID.Bytes, Slug: r.Slug, Vendor: r.Vendor, Protocols: r.Protocols,
		Name: r.Name, BaseURL: r.BaseUrl, Enabled: r.Enabled, AutoDisabled: r.AutoDisabled,
		Headers: decodeHeaders(r.Headers), Transport: decodeJSONObject(r.Transport),
		CostMultiplierBps: r.CostMultiplierBps,
		RateLimitRPM:      int32Ptr(r.RateLimitRpm), RateLimitTPM: int32Ptr(r.RateLimitTpm),
		MaxConcurrency: r.MaxConcurrency,
	}
}

// decodeJSONObject renders a stored jsonb object back out. An empty or
// unreadable one comes back nil rather than as an empty map: absent means
// "nothing configured", and a map that is present but says nothing is a third
// state nobody needs. Unreadable reads as absent on purpose -- these columns
// carry configuration and display, and one bad row must not fail a whole list.
func decodeJSONObject(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}
