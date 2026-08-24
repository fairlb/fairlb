// Package byok owns an organization's own upstream credentials.
//
// Every rule here is about the same asymmetry: a credential that looks
// configured but can never be used is worse than one that was refused, because
// the organization silently pays full price while their screen says otherwise.
// That is why a vendor this deployment does not route to is refused at entry,
// why fallback is off unless asked for, and why a failed test never marks a key
// invalid.
package byok

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/cursorpage"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
	"github.com/fairlb/fairlb/internal/gateway/upstreamprobe"
)

// testTimeout bounds one credential probe.
const testTimeout = 15 * time.Second

var (
	// ErrNotFound is "no such credential in this organization".
	ErrNotFound = fmt.Errorf("byok: not found")
	// ErrDuplicateName is a name this organization already uses.
	ErrDuplicateName = fmt.Errorf("byok: a key with this name already exists")
)

// InvalidError is a request the rules refuse, carrying the sentence that says
// why -- the sentence names which vendors are on offer, and that is the whole
// value of refusing at entry rather than storing a credential nothing can use.
type InvalidError struct{ Message string }

func (e InvalidError) Error() string { return "byok: " + e.Message }

// Key is one organization-supplied credential, never carrying the secret.
type Key struct {
	ID             uuid.UUID
	Vendor         string
	VendorLabel    string
	Name           string
	Status         string
	SecretHint     string
	BaseURL        string
	AllowFallback  bool
	CreatedAt      time.Time
	LastVerifiedAt time.Time
}

// Vendor is a platform a credential can usefully be supplied for: one this
// deployment has an enabled provider at.
type Vendor struct {
	Slug  string
	Label string
	// BaseURLHint is the endpoint that would be used when base_url is left
	// empty -- the answer to the question the form's next field asks.
	BaseURLHint    string
	ModelIDExample string
	KeyHint        string
}

// Create is a new credential.
type Create struct {
	Vendor        string
	Name          string
	BaseURL       *string
	Secret        string
	AllowFallback bool
}

// TestResult is one probe's outcome.
type TestResult struct {
	CheckedAt  time.Time
	Ok         bool
	LatencyMs  *int
	StatusCode *int
	Message    string
}

// Service reads and writes organization credentials. Bind it to the caller's
// org-scoped transaction: these rows are under row-level security, so they must
// run on the connection that set the org scope.
type Service struct {
	q  *gwdb.Queries
	hc *http.Client
}

func NewService(q *gwdb.Queries, hc *http.Client) *Service {
	if hc == nil {
		hc = &http.Client{Timeout: testTimeout}
	}
	return &Service{q: q, hc: hc}
}

func keyFrom(r gwdb.ListOrgProviderKeysRow) Key {
	k := Key{
		ID: uuid.UUID(r.ID.Bytes), Vendor: r.Vendor,
		VendorLabel: catalog.VendorLabel(r.Vendor), Name: r.Name,
		Status: r.Status, SecretHint: r.SecretHint,
		AllowFallback: r.AllowFallback, CreatedAt: r.CreatedAt.Time,
	}
	if r.BaseUrl.Valid {
		k.BaseURL = r.BaseUrl.String
	}
	if r.LastVerifiedAt.Valid {
		k.LastVerifiedAt = r.LastVerifiedAt.Time
	}
	return k
}

// Keys lists an organization's credentials, one page at a time.
//
// The page is keyed on (vendor, name) rather than on time -- see the query for
// why -- and the caller trims the probe row and mints the next cursor, because
// only the caller knows whether it is drawing a screen or exhausting the list.
func (s *Service) Keys(
	ctx context.Context, orgID pgtype.UUID, page cursorpage.KeyPage,
) ([]Key, error) {
	rows, err := s.q.ListOrgProviderKeys(ctx, gwdb.ListOrgProviderKeysParams{
		OrgID: orgID, HasCursor: page.HasKey(),
		CursorVendor: page.At(0), CursorName: page.At(1),
		Lim: page.ProbeLimit(),
	})
	if err != nil {
		return nil, fmt.Errorf("byok: list organization provider keys: %w", err)
	}
	out := make([]Key, 0, len(rows))
	for _, r := range rows {
		out = append(out, keyFrom(r))
	}
	return out, nil
}

// KeyCursor is the opaque cursor pointing just past k.
//
// It lives next to Keys so the encoding and the ORDER BY are read together: a
// cursor built from different columns than the query sorts by produces a page
// that looks plausible and is wrong.
func KeyCursor(k Key) string { return cursorpage.EncodeKey(k.Vendor, k.Name) }

// KeyCursorParts is how many components a key cursor carries, for the transport
// to hand to ParseKeyPage.
const KeyCursorParts = 2

// Vendors describes the platforms a credential can usefully be supplied
// for: the ones this deployment has an enabled provider at.
//
// It travels with the list rather than at an address of its own, so the page
// showing the credentials and the form adding one cannot disagree about which
// platforms are on offer -- and so this deployment keeps exactly three console
// paths under the segment the Community build closes off wholesale.
//
// Each entry carries the endpoint that would be used when base_url is left
// empty. That number is the answer to the question the form's next field asks,
// and computing it here means the page never has to guess it.
func (s *Service) Vendors(ctx context.Context) ([]Vendor, error) {
	slugs, err := s.q.ListBYOKVendors(ctx)
	if err != nil {
		return nil, fmt.Errorf("byok: list the vendors this deployment routes to: %w", err)
	}
	out := make([]Vendor, 0, len(slugs))
	for _, slug := range slugs {
		entry := Vendor{Slug: slug, Label: catalog.VendorLabel(slug)}
		if def, dErr := s.q.DefaultUpstreamForVendor(ctx, slug); dErr == nil {
			entry.BaseURLHint = strings.TrimSpace(def.BaseUrl)
		}
		if v, ok := catalog.LookupVendor(slug); ok {
			entry.ModelIDExample = v.ModelIDExample
			entry.KeyHint = v.KeyHint
		}
		out = append(out, entry)
	}
	return out, nil
}

// Create stores a credential.
//
// Encryption needs the row id: the associated data is bound to it, so
// ciphertext moved to another row will not decrypt. The caller's transaction is
// what cleans up a failed seal -- rolling back leaves no row, and leaving an
// undecryptable credential behind would be worse than having none, because
// routing would keep picking it as a usable candidate and failing.
func (s *Service) Create(
	ctx context.Context, box *crypto.Box, orgID pgtype.UUID, in Create,
) (Key, error) {
	secret := strings.TrimSpace(in.Secret)
	name := strings.TrimSpace(in.Name)
	if secret == "" || name == "" {
		return Key{}, InvalidError{Message: "name and secret are required"}
	}
	vendor := strings.TrimSpace(in.Vendor)
	if err := s.checkVendor(ctx, vendor); err != nil {
		return Key{}, err
	}
	baseURL := pgtype.Text{}
	if in.BaseURL != nil && *in.BaseURL != "" {
		baseURL = pgtype.Text{String: *in.BaseURL, Valid: true}
	}
	row, err := s.q.CreateOrgProviderKey(ctx, gwdb.CreateOrgProviderKeyParams{
		OrgID: orgID, Vendor: vendor, Name: name, BaseUrl: baseURL,
		SecretHint: crypto.MaskSecret(secret),
		// Defaults to false: silently falling back to a shared credential means
		// being billed at full price, and that has to be the organization's
		// explicit choice.
		AllowFallback: in.AllowFallback,
	})
	if db.IsUniqueViolation(err) {
		return Key{}, ErrDuplicateName
	}
	if err != nil {
		return Key{}, fmt.Errorf("byok: create organization provider key: %w", err)
	}
	enc, err := box.Seal([]byte(secret), row.ID.Bytes[:])
	if err != nil {
		return Key{}, fmt.Errorf("byok: encrypt the secret: %w", err)
	}
	if err := s.q.SetOrgProviderKeySecret(ctx, gwdb.SetOrgProviderKeySecretParams{
		ID: row.ID, SecretEnc: enc, OrgID: orgID,
	}); err != nil {
		return Key{}, fmt.Errorf("byok: store the ciphertext: %w", err)
	}
	return keyFrom(gwdb.ListOrgProviderKeysRow(row)), nil
}

// Delete removes a credential.
func (s *Service) Delete(ctx context.Context, orgID, keyID pgtype.UUID) error {
	n, err := s.q.DeleteOrgProviderKey(ctx, gwdb.DeleteOrgProviderKeyParams{ID: keyID, OrgID: orgID})
	if err != nil {
		return fmt.Errorf("byok: delete organization provider key: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Test sends one minimal request upstream using the credential.
//
// Success promotes the row to active. A failure -- including a 401 --
// deliberately leaves the status alone.
//
// This probe borrows the transport profile of whichever provider at this vendor
// sorts first by slug, so a deployment holding two of them (an EU and a US Azure
// resource, a mainland and an international endpoint) can address the wrong one
// and get a 401 for a credential that is perfectly good. Writing "invalid" there
// takes a working key out of routing and silently moves that organization to
// full price, which is a far worse outcome than a red test result. The
// authoritative rejection is the data plane's: it fires on a real forwarded
// request, where the profile is the candidate's own.
func (s *Service) Test(
	ctx context.Context, box *crypto.Box, orgID, keyID pgtype.UUID, model string,
) (TestResult, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return TestResult{}, InvalidError{Message: "upstream_model is required"}
	}
	row, err := s.q.GetOrgProviderKeySecret(ctx, gwdb.GetOrgProviderKeySecretParams{ID: keyID, OrgID: orgID})
	if err != nil {
		if db.IsNoRows(err) {
			return TestResult{}, ErrNotFound
		}
		return TestResult{}, fmt.Errorf("byok: fetch organization provider key: %w", err)
	}
	plain, err := box.Open(row.SecretEnc, row.ID.Bytes[:])
	if err != nil {
		return TestResult{
			CheckedAt: time.Now().UTC(),
			Message:   "Could not decrypt the stored secret. Has the master encryption key changed?",
		}, nil
	}
	baseURL, transport, protocol, err := s.upstreamFor(ctx, row.BaseUrl, row.Vendor)
	if err != nil {
		return TestResult{}, err
	}
	res := s.probe(ctx, proxy.Protocol(protocol), transport, baseURL, string(plain), model)
	if res.Ok {
		if err := s.q.SetOrgProviderKeyStatus(ctx, gwdb.SetOrgProviderKeyStatusParams{
			ID: row.ID, OrgID: orgID, Status: "active",
		}); err != nil {
			// Failing to record the attempt does not change the test's verdict.
			return res, nil
		}
	}
	return res, nil
}

// upstreamFor resolves how this credential should address its upstream: the
// endpoint, and the transport profile that says how to build a request for it.
//
// The endpoint is the organization's own if they supplied one, otherwise the
// deployment's default for that vendor. With neither available it errors rather
// than guessing. A wrong guess sends the request to a nonexistent or incorrect
// address, which surfaces as "cannot connect" while the real cause is missing
// configuration -- an error message that points the reader in the wrong
// direction.
//
// The profile always comes from the deployment's provider for that vendor, even
// when the endpoint is the organization's own. That is what the data plane does: it
// swaps in the organization's base URL and keeps the provider's profile, because the
// profile describes how that platform is addressed, and a organization pointing at
// their own account with the same platform is addressed the same way.
//
// Choosing by vendor rather than by dialect is what makes that true. The old
// criterion was "any provider speaking this dialect", which could hand an Azure
// credential the profile of a plain OpenAI provider -- a different credential
// header, so every request answered 401 and the organization was told their key was
// bad.
//
// The protocol comes back too, because the probe has to send a request in a
// dialect this endpoint actually speaks. For a multi-dialect provider it is the
// vendor's *own* default rather than the first row of the stored array: the
// write path stores `array_agg(DISTINCT f ORDER BY f)`, so that array is
// alphabetical, and taking its head would test every DeepSeek or Zhipu
// credential in the Anthropic dialect purely because "anthropic" sorts first.
func (s *Service) upstreamFor(
	ctx context.Context, own pgtype.Text, vendor string,
) (string, catalog.Transport, string, error) {
	def, defErr := s.q.DefaultUpstreamForVendor(ctx, vendor)

	baseURL := strings.TrimSpace(own.String)
	if !own.Valid || baseURL == "" {
		if defErr != nil || strings.TrimSpace(def.BaseUrl) == "" {
			return "", catalog.Transport{}, "", InvalidError{Message: "No default endpoint is configured " +
				"for this vendor; set base_url explicitly on this key"}
		}
		baseURL = strings.TrimSpace(def.BaseUrl)
	}

	// A deployment with no provider at this vendor is a normal state for a
	// organization who brought both their credential and their endpoint. There is no
	// profile to apply, and the zero value is the ordinary case; the dialect
	// then falls back to the vendor's own default.
	if defErr != nil {
		return baseURL, catalog.Transport{}, defaultProtocolFor(vendor), nil
	}
	// A profile this version cannot read is not a reason to refuse the test.
	// The read path already drops only what it cannot use, and reporting the
	// address the request will actually go to beats refusing to look.
	transport, _ := catalog.ParseTransport(def.Transport)
	return baseURL, transport, probeProtocol(vendor, def.Protocols), nil
}

// probeProtocol picks the dialect to test a credential in.
//
// The vendor's default wins when this provider declares it; otherwise the first
// dialect it does declare. Ordering the choice this way keeps the probe on the
// dialect the platform is primarily reached through, which is the one the
// organization's model belongs to in every case a single test can serve.
func probeProtocol(vendor string, declared []string) string {
	if v, ok := catalog.LookupVendor(vendor); ok {
		for _, p := range v.DefaultProtocols {
			if slices.Contains(declared, p) {
				return p
			}
		}
	}
	if len(declared) > 0 {
		return declared[0]
	}
	return defaultProtocolFor(vendor)
}

// defaultProtocolFor is the dialect to probe in when no provider row can answer.
// The registry knows what each platform publishes; for an unlisted one the
// OpenAI dialect is the overwhelmingly common shape.
func defaultProtocolFor(vendor string) string {
	if v, ok := catalog.LookupVendor(vendor); ok && len(v.DefaultProtocols) > 0 {
		return v.DefaultProtocols[0]
	}
	return catalog.ProtocolOpenAI
}

// checkVendor refuses a credential for a platform this deployment does not
// route to.
//
// Such a key can never take effect: no candidate belongs to that vendor, so
// nothing would ever be served by it, and it would sit in the organization's list
// looking configured. Refusing at the point of entry is the only place the
// difference between "configured" and "in use" is visible to whoever is typing.
func (s *Service) checkVendor(ctx context.Context, vendor string) error {
	if vendor == "" {
		return InvalidError{Message: "vendor is required"}
	}
	available, err := s.q.ListBYOKVendors(ctx)
	if err != nil {
		return fmt.Errorf("byok: list the vendors this deployment routes to: %w", err)
	}
	if slices.Contains(available, vendor) {
		return nil
	}
	if len(available) == 0 {
		return InvalidError{Message: "This deployment routes to no vendor you can supply a credential for"}
	}
	return InvalidError{Message: "This deployment does not route to " + catalog.VendorLabel(vendor) +
		"; a credential for it could never be used. Available: " + strings.Join(available, ", ")}
}

// probe sends one minimal real request: a one-word prompt with
// max_tokens=1. The point is to verify the credential and connectivity, not
// model quality, so the shape is chosen to spend as few tokens as possible --
// but it still lands on the organization's own upstream bill.
//
// The transport profile is applied here for the same reason it is applied on
// the request path: a test that builds a different request from the one the
// data plane builds reports its own failure as the credential's. An upstream
// that keeps its chat endpoint behind a deployment path answers the standard
// one with a 404, and the organization is told their key does not work while
// inference through the gateway is fine -- the most expensive shape a
// diagnostic can take, because it sends someone to re-issue a credential that
// was never the problem.
func (s *Service) probe(
	ctx context.Context, protocol proxy.Protocol, transport catalog.Transport,
	baseURL, apiKey, model string,
) TestResult {
	out := TestResult{CheckedAt: time.Now().UTC()}

	spec, ok := upstreamprobe.SpecForProtocol(protocol, model)
	if !ok {
		out.Message = "This build cannot test a credential for the " + string(protocol) + " protocol"
		return out
	}
	res := upstreamprobe.Run(ctx, upstreamprobe.Input{
		Client: s.hc, Spec: spec, BaseURL: baseURL, APIKey: apiKey,
		Model: model, Transport: transport, Timeout: testTimeout,
	})
	out.CheckedAt, out.Ok = res.CheckedAt, res.OK
	out.LatencyMs, out.StatusCode = res.LatencyMs, res.StatusCode
	out.Message = res.Message
	return out
}
