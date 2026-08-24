// Package discovery asks a provider what models it actually serves.
//
//
// It removes a class of configuration error that otherwise only shows up at run
// time: a mistyped provider_model_id is invisible until a real request reaches
// the provider, comes back 404, and the route gets marked broken. Discovery
// moves that check to configuration time.
//
// Three deliberate constraints:
//
//   - Read-only. Nothing here writes to the database.
//   - It never creates a model row on its own. Routing refuses an unpriced
//     model, so auto-creating rows would manufacture a batch of models destined
//     to answer 503 -- models that are invisible in the catalog and only fail
//     once a request reaches them.
//   - A match is a suggestion, nothing more. Upstream reports its own model
//     names, this system identifies models by slug, and there is no reliable
//     mechanical correspondence between the two.

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/fairlb/fairlb/foundation/strutil"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// ErrProviderNotFound is "no such provider".
var ErrProviderNotFound = fmt.Errorf("discovery: provider not found")

// State is what the local catalog knows about one upstream model.
type State string

const (
	// StateRouted is already being served.
	StateRouted State = "routed"
	// StateUnknown has no local model at all.
	StateUnknown State = "unknown"
	// StateUnpriced has a local model that routing would refuse.
	StateUnpriced State = "unpriced"
	// StateMappable has a local model ready to be wired.
	StateMappable State = "mappable"
)

// Model is one upstream model and what the local catalog makes of it.
type Model struct {
	UpstreamModel string
	State         State
	ModelID       uuid.UUID
	ModelSlug     string
}

// Result is one discovery run.
//
// Ok and Complete are separate bits on purpose. Ok says the fetch produced a
// conclusion; Complete says the conclusion covers everything upstream has.
// A client that only looks at Ok reads "all I saw" as "all there is", and then
// any catalog past the entry cap, past the page cap, or with a stalled cursor
// gets every unseen local route mislabelled "upstream no longer offers this".
type Result struct {
	CheckedAt  time.Time
	Ok         bool
	Complete   bool
	StatusCode *int
	// Message is aimed at the operator. An unreachable upstream is the *result*
	// of this probe, not a fault in the endpoint that ran it -- which is why
	// none of these is an error.
	Message string
	Models  []Model
}

// Service runs discovery.
type Service struct {
	q   *gwdb.Queries
	box *crypto.Box
	hc  *http.Client
}

func NewService(pool *pgxpool.Pool, box *crypto.Box) *Service {
	return &Service{
		q: gwdb.New(pool), box: box,
		hc: &http.Client{Timeout: discoverTimeout, Transport: httpx.UpstreamTransport()},
	}
}

// Discover asks the provider for its catalog and classifies what comes back.
func (s *Service) Discover(ctx context.Context, providerID uuid.UUID) (Result, error) {
	id := pgtype.UUID{Bytes: providerID, Valid: true}
	prov, err := s.q.GetProviderForAdmin(ctx, id)
	if err != nil {
		return Result{}, ErrProviderNotFound
	}
	out := Result{CheckedAt: time.Now().UTC(), Models: []Model{}}

	// Two platforms publish no catalogue at all. Asking them anyway returns a
	// 404 this would report as a failed fetch, or -- worse, where the address
	// answers with something -- an empty list, which reads as "the upstream
	// serves no models". That is a conclusion rather than an error, so nothing
	// sends anyone looking. The registry knows which vendors those are, and
	// saying so is more useful than any probe result.
	if v, ok := catalog.LookupVendor(prov.Vendor); ok && !v.ModelListing {
		out.Message = v.Label + " publishes no model catalogue, so there is nothing to " +
			"discover here. Create the routes by hand, using the model ids from the vendor's " +
			"own documentation."
		return out, nil
	}

	keys, err := s.q.GetProviderKeysForProvider(ctx, id)
	if err != nil || len(keys) == 0 {
		out.Message = "This provider has no usable credential"
		return out, nil
	}
	plain, err := s.box.Open(keys[0].SecretEnc, keys[0].ID.Bytes[:])
	if err != nil {
		out.Message = "Could not decrypt the credential. Has the master encryption key changed?"
		return out, nil
	}

	cat := s.fetchUpstreamModelIDs(ctx, prov, string(plain))
	out.StatusCode = cat.StatusCode
	if cat.Failure != "" {
		out.Message = cat.Failure
		return out, nil
	}
	out.Ok = true
	// An incomplete fetch is still Ok: whatever was retrieved classifies
	// correctly. But it has to be said out loud -- "upstream only has these"
	// and "these are all I managed to see" are completely different
	// conclusions for whoever is reading.
	out.Complete = cat.Incomplete == ""
	if cat.Incomplete != "" {
		out.Message = cat.Incomplete
	}
	if len(cat.IDs) == 0 {
		// "The upstream answered normally and reports no models at all" is the
		// state this feature most needs to raise: every route configured
		// locally now points at nothing. Classifying it as a failure would stop
		// the client from flagging any of them as gone.
		out.Message = "The upstream answered normally but reports no models at all"
		return out, nil
	}

	rows, err := s.q.ClassifyUpstreamModels(ctx, gwdb.ClassifyUpstreamModelsParams{
		UpstreamIds: cat.IDs, ProviderID: id,
	})
	if err != nil {
		return Result{}, fmt.Errorf("discovery: classify upstream models: %w", err)
	}
	for _, r := range rows {
		out.Models = append(out.Models, Model{
			UpstreamModel: r.UpstreamID,
			State:         stateOf(r),
			ModelID:       uuid.UUID(r.ModelID.Bytes),
			ModelSlug:     r.ModelSlug,
		})
	}
	return out, nil
}

// stateOf decides among the four states. The order is the priority: something
// already being served needs no further check on whether it is priced.
func stateOf(r gwdb.ClassifyUpstreamModelsRow) State {
	switch {
	case r.Routed:
		return StateRouted
	case !r.ModelID.Valid:
		return StateUnknown
	case !r.Priced:
		return StateUnpriced
	default:
		return StateMappable
	}
}

// discoverTimeout bounds fetching the upstream catalog. It runs synchronously
// from a screen somebody is looking at, so an unreachable base_url must not
// hang the request.
const discoverTimeout = 20 * time.Second

// maxUpstreamModels caps how many upstream entries are returned. An
// aggregator's catalog can run to thousands, which serves neither the UI nor
// the classification query that follows. Past the cap the list is truncated and
// the message says so, rather than dropping entries silently.
const maxUpstreamModels = 500

// maxDiscoverPages is the hard limit on pagination. Cursor pagination relies on
// the upstream's has_more to converge, and an upstream whose cursor does not
// advance would loop forever. The timeout bounds wall clock; this bound is what
// gives "why did this provider take twenty seconds" an explainable answer.
const maxDiscoverPages = 25

// discoverBodyLimit caps how much of an upstream response is read, so a
// runaway upstream cannot exhaust memory here.
const discoverBodyLimit = 4 << 20 // 4 MiB

// catalogDialect decides which dialect to list the catalogue in.
//
// A multi-dialect provider has no single right answer, so the *vendor's* own
// default wins when it declares one this provider speaks: the registry is where
// "what is this platform primarily reached through" is written down, and it is a
// better answer than any fixed preference.
//
// A fixed preference for openai was the rule while both dialects served their
// catalogue at the same address with the same cursor. That stopped being true
// when a third protocol arrived: Gemini's catalogue is at another path, behind
// another header, with another cursor and another response shape. For the vendor
// the registry ships as speaking both, the old rule sent discovery to the OpenAI
// address, where the answer is a 401 or -- worse -- a 200 this parser reads as
// an empty catalogue.
//
// Guessing wrong stays cheap: a failed fetch shows the upstream status code on
// the operator page, and the provider's header map can override any header.
func catalogDialect(vendor string, protocols []string) proxy.Protocol {
	if v, ok := catalog.LookupVendor(vendor); ok {
		for _, p := range v.DefaultProtocols {
			if slices.Contains(protocols, p) {
				return proxy.Protocol(p)
			}
		}
	}
	if len(protocols) == 0 || slices.Contains(protocols, string(proxy.ProtocolOpenAI)) {
		return proxy.ProtocolOpenAI
	}
	return proxy.Protocol(protocols[0])
}

// parseGeminiModelPage reads that protocol's catalogue shape.
//
// The names come back fully qualified ("models/gemini-2.5-flash") while a route
// names the model the way a request does, so the prefix is stripped. Leaving it
// on would classify every model as one this deployment does not have, which is
// the same conclusion as an empty catalogue and just as silent.
func parseGeminiModelPage(pg modelPage, raw []byte) modelPage {
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		pg.failure = "The upstream response is not the expected {\"models\":[{\"name\":…}]} shape: " + err.Error()
		return pg
	}
	pg.rawCount = len(body.Models)
	pg.ids = make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		if id := strings.TrimSpace(strings.TrimPrefix(m.Name, "models/")); id != "" {
			pg.ids = append(pg.ids, id)
		}
	}
	// This protocol's cursor is an opaque token rather than the last id, so
	// there is no falling back to one: an absent token means the last page.
	pg.nextCursor = strings.TrimSpace(body.NextPageToken)
	pg.hasMore = pg.nextCursor != ""
	return pg
}

// upstreamCatalog is the outcome of one catalog fetch.
//
// Incomplete and Failure are deliberately separate. Failure means there is no
// conclusion at all (bad credential, no such endpoint, wrong shape). Incomplete
// means the conclusion is valid but partial (pagination was cut short, the
// entry cap was reached). Collapsing them into one field would force the caller
// to choose between refusing to display anything and silently treating a
// partial answer as complete, when the right handling is to display it and mark
// it partial.
type upstreamCatalog struct {
	IDs        []string
	StatusCode *int
	Incomplete string
	Failure    string
}

// fetchUpstreamModelIDs fetches the upstream catalog and extracts the model
// ids, following cursor pagination.
//
// Failure is an explanation aimed at the operator, not an error: not being able
// to reach the upstream is the result of this probe, not a fault in this
// endpoint. Expressing it as 4xx/5xx would force clients to distinguish "the
// probe ran and did not pass" from "the probe endpoint itself is broken", for
// the same reason the connectivity probe returns 200 either way.
//
// Pagination has to be followed. Anthropic's GET /v1/models is cursor
// paginated (after_id/before_id with has_more/first_id/last_id, and a small
// default page), so reading only the first page reports every model from page
// two onward as "upstream does not have it" -- the exact opposite of what this
// feature is for, and silently so.
//
// A second page is only ever requested when the upstream itself says has_more,
// and the first request is byte-for-byte the plain endpoint with no query
// parameters. OpenAI-protocol providers therefore see no change at all: they do
// not report has_more, so the loop exits after one pass. Only an upstream that
// has explicitly entered the cursor protocol gets paged, so there is no risk of
// an extra parameter provoking a 400 from a strict proxy.
func (s *Service) fetchUpstreamModelIDs(
	ctx context.Context, prov gwdb.GetProviderForAdminRow, apiKey string,
) upstreamCatalog {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	var headers map[string]string
	if err := json.Unmarshal(prov.Headers, &headers); err != nil {
		headers = nil
	}

	var out upstreamCatalog
	seen := make(map[string]struct{})
	cursor := ""
	for page := 0; ; page++ {
		pg := s.fetchModelPage(ctx, prov, apiKey, headers, cursor)
		out.StatusCode = pg.statusCode
		if pg.failure != "" {
			if page == 0 {
				out.Failure = pg.failure
				return out
			}
			// Earlier pages are already in hand: return what there is and say
			// where it stopped, rather than discarding the whole round.
			out.Incomplete = fmt.Sprintf("Read the first %d page(s), then stopped: %s", page, pg.failure)
			return out
		}
		// There is deliberately no "empty catalog means failure" rule here. A
		// 200 with data:[] is successfully enumerating zero entries, not a
		// failed fetch. Treating it as a failure makes the client see no
		// upstream names at all, so nothing gets flagged as "configured here,
		// no longer offered upstream" -- which is precisely the case that most
		// needs raising, because it is what a fully withdrawn upstream looks
		// like. The judgement belongs to ok, complete and the number of models
		// together; emptiness must not quietly become its own kind of failure.

		fresh := 0
		for _, id := range pg.ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out.IDs = append(out.IDs, id)
			fresh++
		}
		// The rawCount > 0 term matters: "reported nothing" and "reported
		// entries whose ids are all empty" are two different things. The first
		// is a legitimately empty catalog and takes the success path above;
		// the second is a malformed response, and the operator needs to know
		// which one it was. Without that term, an empty data array falls into
		// this branch and the empty catalog just allowed through is classified
		// as a failure after all.
		if page == 0 && pg.rawCount > 0 && len(out.IDs) == 0 {
			out.Failure = "The upstream catalog contains no non-empty id"
			return out
		}

		switch {
		case !pg.hasMore:
			return out // the only exit that counts as complete
		case len(out.IDs) >= maxUpstreamModels:
			out.IDs = out.IDs[:maxUpstreamModels]
			out.Incomplete = fmt.Sprintf(
				"The upstream has more models; reached the display limit of %d", maxUpstreamModels)
			return out
		case page+1 >= maxDiscoverPages:
			out.Incomplete = fmt.Sprintf(
				"The upstream has more models; reached the page limit of %d (%d read in total)",
				maxDiscoverPages, len(out.IDs))
			return out
		case pg.nextCursor == "" || fresh == 0:
			// The upstream says there is more but gave no usable cursor, or
			// gave one that does not advance. Continuing would loop forever,
			// so stop and say so. This counts as incomplete rather than
			// failed, because what was read is still a valid conclusion.
			out.Incomplete = fmt.Sprintf(
				"The upstream reports more models but gave no usable pagination cursor (%d read in total)", len(out.IDs))
			return out
		}
		cursor = pg.nextCursor
	}
}

// modelPage is one page of the catalog.
type modelPage struct {
	ids        []string
	rawCount   int // raw data length: "empty catalog" vs "ids all blank"
	hasMore    bool
	nextCursor string
	statusCode *int
	failure    string
}

// fetchModelPage fetches one page. With an empty cursor it hits the plain
// endpoint, with no query parameters at all.
func (s *Service) fetchModelPage(
	ctx context.Context, prov gwdb.GetProviderForAdminRow,
	apiKey string, headers map[string]string, cursor string,
) modelPage {
	var pg modelPage
	// Both protocols expose the catalog at the same path in the same shape
	// ({"data":[{"id":...}]}), so the request is built without branching on
	// protocol. The dialect difference in the auth header is handled by
	// proxy.BuildRequest.
	//
	// The transport profile is applied here as well, and it matters more here
	// than anywhere else: an upstream that keeps its catalog at another path,
	// or requires a query parameter to answer at all, returns nothing to a
	// request built without it -- and "this provider serves no models" is a
	// conclusion, not an error, so nobody would go looking.
	transport, err := catalog.ParseTransport(prov.Transport)
	if err != nil {
		pg.failure = "The provider's transport profile could not be read: " + err.Error()
		return pg
	}
	// The method and the cursor are handed to the builder rather than written
	// onto the finished request.
	//
	// They used to be applied afterwards, which read fine while every
	// credential was a header copied verbatim. It stops being fine the moment a
	// provider's credential is a signature: the method and the query string are
	// both covered by it, so editing either one afterwards leaves a signature
	// describing a request that no longer exists. The upstream then answers 403
	// and blames the credential, which is the wrong place to look.
	//
	// The cursor is *added* to whatever the transport profile contributes rather
	// than replacing it. Assigning would drop a mandatory parameter from the
	// second page onwards, so the first page would come back and every page
	// after it would fail -- a shape that reads like upstream pagination being
	// broken.
	protocol := catalogDialect(prov.Vendor, prov.Protocols)
	gemini := protocol == proxy.ProtocolGemini
	path := catalog.PathModels
	cursorParam := "after_id"
	if gemini {
		// A catalogue of its own, at its own address, with its own cursor
		// parameter. Nothing about it is derivable from the other two.
		path = catalog.PathGeminiModels
		cursorParam = "pageToken"
	}
	var extraQuery map[string]string
	if cursor != "" {
		extraQuery = map[string]string{cursorParam: cursor}
	}
	// With a nil body the builder sets ContentLength to 0 and Body to
	// http.NoBody, so this really is a bodyless GET rather than a malformed
	// request carrying an empty body.
	httpReq, err := proxy.BuildRequest(ctx, proxy.Target{
		Protocol: protocol, BaseURL: prov.BaseUrl,
		APIKey: apiKey, Path: path, Headers: headers,
		Transport: transport, Method: http.MethodGet, ExtraQuery: extraQuery,
	}, nil)
	if err != nil {
		pg.failure = "Could not build the request: " + err.Error()
		return pg
	}

	resp, err := s.hc.Do(httpReq)
	if err != nil {
		pg.failure = "Could not reach the upstream: " + err.Error()
		return pg
	}
	defer func() { _ = resp.Body.Close() }()

	code := resp.StatusCode
	pg.statusCode = &code
	raw, err := io.ReadAll(io.LimitReader(resp.Body, discoverBodyLimit))
	if err != nil {
		pg.failure = "Could not read the upstream response: " + err.Error()
		return pg
	}
	if resp.StatusCode >= 400 {
		// The upstream's own message is the most useful thing for telling a
		// bad credential from an unsupported endpoint from a quota problem.
		pg.failure = fmt.Sprintf("The upstream returned %d: %s",
			resp.StatusCode, strutil.Ellipsize(strings.TrimSpace(string(raw)), 2<<10))
		return pg
	}

	if gemini {
		return parseGeminiModelPage(pg, raw)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		pg.failure = "The upstream response is not the expected {\"data\":[{\"id\":…}]} shape: " + err.Error()
		return pg
	}
	pg.rawCount = len(body.Data)
	pg.hasMore = body.HasMore
	pg.ids = make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			pg.ids = append(pg.ids, id)
		}
	}
	// Prefer the upstream's own last_id as the cursor; fall back to the last id
	// on this page, which covers compatibility layers that report has_more
	// without a last_id.
	pg.nextCursor = strings.TrimSpace(body.LastID)
	if pg.nextCursor == "" && len(pg.ids) > 0 {
		pg.nextCursor = pg.ids[len(pg.ids)-1]
	}
	return pg
}
