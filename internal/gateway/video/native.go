package video

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Vendor compatibility surfaces: this gateway answering in one vendor's own
// shape, so that a caller who already wrote against that vendor moves here by
// changing a base URL.
//
// # What this is, and what it is not
//
// It is **not** passthrough. The inference data plane's promise is byte-level:
// an OpenAI-shaped request reaches an OpenAI-shaped upstream unchanged. That
// cannot be done here, and the reasons are structural rather than a matter of
// effort:
//
//   - every video API on this plane is asynchronous, so a forwarded submit
//     would create a job upstream that this gateway holds no row for -- no
//     hold, no settlement, no usage line. ADR-0220 exists to make that
//     impossible.
//   - a forwarded poll answers with the upstream's own URL, which ADR-0222
//     forbids returning: it names the upstream and usually carries its
//     credential.
//   - an upstream job id only means anything on the account that created it
//     (ADR-0220's pinning), so a bare id could not be resolved back to a route.
//
// So the shapes are the vendor's and the job is ours. Our job identifier
// occupies the vendor's task id, our /content address occupies the vendor's
// video URL, and everything between admission and settlement is the same code
// the normalised plane runs. For the caller that is still zero code changed --
// both of those are opaque strings to any client.
//
// # Why this is a second interface rather than more methods on Mapper
//
// Mapper's methods are all outbound: this gateway as a client of that vendor.
// These are inbound: this gateway impersonating it. Merging them would force a
// vendor that only needs routing to implement an inbound contract, and
// "routable but not switch-to-able" is a legitimate state -- the reverse is
// not, and a test refuses it.

// NativeKind is what one inbound route does. Declared per route rather than
// inferred from the method, because two of a vendor's GETs can mean different
// things: one lists tasks and one reads a single task.
type NativeKind string

const (
	NativeSubmit   NativeKind = "submit"
	NativePoll     NativeKind = "poll"
	NativeList     NativeKind = "list"
	NativeCancel   NativeKind = "cancel"
	NativeDelete   NativeKind = "delete"
	NativeArtifact NativeKind = "artifact"
)

// NativeRoute is one inbound path of a vendor's own API that this gateway
// answers.
//
// Declared rather than discovered. A path this gateway cannot name is a path it
// cannot price, and answering an unnamed one would mean either forwarding
// something unbilled or returning a shape nobody decided on.
type NativeRoute struct {
	Method string
	// Path is a chi pattern relative to this vendor's prefix, and it is the
	// vendor's own path verbatim -- that is the whole point: the caller's SDK
	// appends it to the base URL exactly as it always did.
	Path string
	Kind NativeKind
	// Variant separates routes of the same kind. Kling submits text-to-video
	// and image-to-video at two paths, and which one was used decides how the
	// job is read back.
	Variant string
	// IDIn and IDName say where a read route carries the job identifier.
	//
	// Declared rather than assumed to be a path segment, because it is not:
	// three of these vendors put it in the path and one passes it as a query
	// parameter. This was the one thing the second batch of surfaces asked the
	// interface for, and it is a field on a route rather than a method on the
	// surface because it describes an address, not a decision.
	//
	// IDIn is "path", "query" or "path_tail"; empty means "path", which is the
	// shape most of them have. "path_tail" is the last segment of a wildcard
	// match, which is what a vendor whose identifier is itself a path needs.
	IDIn   string
	IDName string
	// IDAlias says this route's identifier is the job's integer alias rather
	// than its id. One vendor's published schema types its file identifier as
	// int64, and a UUID rendered there breaks every generated client.
	IDAlias bool
}

// Identifier locations.
const (
	NativeIDInPath  = "path"
	NativeIDInQuery = "query"
	// NativeIDInPathTail is the last segment of a wildcard match. Veo's job
	// identifier is an operation *name* -- "models/<model>/operations/<id>" --
	// and its client GETs that name as a path, so the route is a wildcard and
	// the id is the end of it.
	NativeIDInPathTail = "path_tail"
)

// IDFrom reads this route's job identifier out of an inbound request.
func (r NativeRoute) IDFrom(in NativeRequest) string {
	name := r.IDName
	if name == "" {
		return ""
	}
	switch r.IDIn {
	case NativeIDInQuery:
		return in.Query[name]
	case NativeIDInPathTail:
		tail := in.Path[name]
		if i := strings.LastIndex(tail, "/"); i >= 0 {
			return tail[i+1:]
		}
		return tail
	default:
		return in.Path[name]
	}
}

// NativeRequest is one inbound call, already split into the parts a surface
// reads. It never carries headers: credentials and content types are the HTTP
// boundary's business, and a surface that could read them would eventually
// authenticate something.
type NativeRequest struct {
	Route NativeRoute
	Body  []byte
	Path  map[string]string
	Query map[string]string
}

// NativeJob is one job as a surface needs to see it.
//
// Plain data rather than the database row, for the reason the artifact port
// gives: letting a generated type cross this seam welds the storage model into
// every vendor file.
type NativeJob struct {
	// ID is this gateway's job identifier, which occupies the vendor's task id.
	ID string
	// Alias is the same job as an integer, for the one vendor whose published
	// schema types its identifier as int64. Zero where it was not needed.
	Alias int64
	// Model is the upstream model id the job was submitted with, not the
	// catalog slug: the caller sent the vendor's own name and expects it back.
	Model          string
	Status         Status
	UpstreamStatus string
	Progress       int
	ErrorCode      string
	ErrorMessage   string
	// ContentURL is this deployment's own address for the bytes. The upstream's
	// URL never appears here (ADR-0222).
	ContentURL      string
	DurationSeconds int
	Resolution      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NativeSurface is one vendor's inbound shape.
type NativeSurface interface {
	Vendor() string
	Routes() []NativeRoute
	// Decode reads one of this vendor's own request bodies into the normalised
	// request, plus the parameters that belong to this vendor alone.
	//
	// The passthrough is the part that has to be got right. Anything that
	// changes a billed quantity -- duration, resolution, audio, how many clips
	// -- must be read into the Request under this vendor's own spelling and
	// must not be forwarded, because the Request is what the job is priced on.
	// Everything else is this vendor's business and travels verbatim.
	Decode(in NativeRequest) (r Request, passthrough map[string]json.RawMessage, err error)
	// Render writes one job back in this vendor's own response shape.
	Render(route NativeRoute, job NativeJob) (status int, body []byte, err error)
	// RenderError writes a refusal in this vendor's own error shape. An SDK
	// that cannot parse our errors is an SDK whose retry logic is blind.
	RenderError(route NativeRoute, httpStatus int, code, message string) (status int, body []byte)
}

// ErrNativeRouteUnsupported is what a surface answers for one of its vendor's
// paths that this gateway does not serve. Named rather than a bare 404: a
// caller whose SDK reached a lip-sync endpoint deserves to be told that this
// gateway does not offer it, not to be told the path does not exist.
type ErrNativeRouteUnsupported struct{ Path string }

func (e ErrNativeRouteUnsupported) Error() string {
	return fmt.Sprintf("this gateway does not offer %s", e.Path)
}

// nativeRegistry is the set of vendors this build can be reached *as*.
var nativeRegistry = map[string]NativeSurface{}

func registerNative(s NativeSurface) {
	if _, dup := nativeRegistry[s.Vendor()]; dup {
		panic("video: two compatibility surfaces registered for vendor " + s.Vendor())
	}
	nativeRegistry[s.Vendor()] = s
}

// NativeSurfaceFor returns the compatibility surface for a vendor.
func NativeSurfaceFor(vendor string) (NativeSurface, bool) {
	s, ok := nativeRegistry[vendor]
	return s, ok
}

// NativeVendors lists every vendor this build publishes a compatibility surface
// for, sorted.
//
// Unlike the mapper registry this one has a production reader: the HTTP layer
// walks it to mount the routes. A vendor added here becomes reachable by
// mounting, with no second list to keep in step.
func NativeVendors() []string {
	out := make([]string, 0, len(nativeRegistry))
	for v := range nativeRegistry {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}
