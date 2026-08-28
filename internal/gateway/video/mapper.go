package video

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Mapper is the whole contract for reaching one vendor's video API.
//
// It produces shapes and reads shapes; it never delivers anything. The base
// URL, the credential, the breaker and the retry budget are all applied by the
// proxy layer from the provider's transport profile, exactly as they are on the
// inference plane. That separation is what holds ADR-0219's fence in place: a
// mapper that could reach a connection pool would eventually issue a request
// from inside itself.
//
// Adding a vendor is therefore one file and one registry entry. If a new vendor
// ever needs the interface widened, that is the signal the interface was shaped
// by the coincidences of the vendors already in it.
type Mapper interface {
	// Vendor is the registry slug this mapper serves.
	Vendor() string

	// Envelope is this vendor's published capability set for one upstream
	// model, offered as a prefill for the operator. It is not consulted at
	// request time -- the stored envelope is (ADR-0221) -- for the same reason
	// the vendor registry is a prefill and the saved provider record is the
	// truth: from the moment somebody presses save, the configuration is theirs.
	Envelope(upstreamModel string) Envelope

	// Submit renders a normalised request as this vendor's create call.
	//
	// audioOn is passed in rather than re-derived from r.Audio, which is
	// tri-state on the wire: the caller has already resolved it against the
	// route's stored envelope, and that resolved value is the one the job was
	// priced on. A mapper that decided for itself would send a silent clip for
	// a request billed with sound the moment an operator declared a model
	// `audio: always` -- exactly the substitution Envelope.Validate exists to
	// prevent.
	Submit(r Request, upstreamModel string, audioOn bool) (Outbound, error)
	// SubmitResult extracts the vendor's own job identifier.
	//
	// It sees the request because on some vendors the identifier alone is not
	// enough to find the job again: Kling publishes a separate query path per
	// task type, so its mapper returns "image2video/<id>" and Poll joins.
	// Google's identifier is already a path for the same reason.
	SubmitResult(r Request, status int, body []byte) (upstreamID string, err error)

	// Poll renders the status call for a job.
	Poll(upstreamID string) (Outbound, error)
	// PollResult normalises a status response. The vendors spell success in as
	// many ways as there are vendors; normalising here is what keeps the job
	// machinery working in five states rather than in each vendor's vocabulary.
	PollResult(status int, body []byte) (Poll, error)

	// CancelMode says how far this vendor can stop a job that already exists.
	CancelMode() CancelMode
	// Cancel renders the cancel call. Only called when CancelMode allows it.
	Cancel(upstreamID string) (Outbound, error)

	// Artifact says how to fetch the bytes of a finished job.
	Artifact(p Poll) (Artifact, error)
	// ResolveArtifact reads the document an Indirect artifact request
	// returned. Only called when the previous Artifact said Indirect, and a
	// vendor that never does may refuse it.
	ResolveArtifact(status int, body []byte) (Artifact, error)
}

// Outbound is a request shape: everything except where it goes and who is
// making it. The proxy layer supplies the base URL, the credential and the
// transport profile.
type Outbound struct {
	Method string
	// Path is appended to the provider's base URL. It may be a full path
	// beginning with a slash; the transport profile's overrides still apply.
	Path string
	// URL is an absolute address that replaces the provider's entirely. An
	// artifact lives wherever the upstream put it -- its own CDN, an object
	// store -- and that is not a path under the API's base URL.
	URL   string
	Query map[string]string
	// Body is nil for a GET.
	Body    []byte
	Headers map[string]string
}

// Status is the normalised lifecycle of a job, and the only vocabulary the job
// machinery knows.
type Status string

const (
	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
	StatusCanceled   Status = "canceled"
)

// Terminal reports whether a status is final.
func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCanceled
}

// Poll is one normalised status reading.
type Poll struct {
	Status Status
	// UpstreamStatus is the vendor's own word, kept verbatim. When the
	// normalisation above is the thing in doubt, this is what settles it.
	UpstreamStatus string
	Progress       int
	// ErrorCode and ErrorMessage carry a terminal failure's reason. Content
	// policy refusals land here, and they are the common failure on this plane
	// rather than an edge case, so the text is kept as written rather than
	// flattened into a code.
	ErrorCode    string
	ErrorMessage string
	// NotFound distinguishes "this vendor has never heard of that job" from a
	// transport failure. One 404 is not proof -- see the job worker.
	NotFound bool
	// ArtifactRef is what this vendor gave us that addresses the finished
	// bytes. Usually a URL; on MiniMax it is a file id that Artifact turns into
	// a second call. Named for neither because only the mapper that produced it
	// interprets it -- and because a file id stored in a field called "url" is
	// the kind of name that goes wrong later.
	//
	// Never returned to a caller: it names the upstream and often carries its
	// credential (ADR-0222). Empty on a completed job means the upstream
	// reported success with nothing to fetch, which every mapper turns into a
	// failure rather than a charge for a video that does not exist.
	ArtifactRef    string
	ArtifactExpiry time.Time
	ContentType    string
}

// Artifact says how to fetch a finished job's bytes.
type Artifact struct {
	// Request is the call that returns the bytes.
	Request Outbound
	// NeedsUpstreamCredential is true where the download is authenticated as
	// the upstream account -- Google's file URIs are, a presigned CDN link is
	// not. It decides whether the proxy layer attaches the credential, and
	// getting it wrong either leaks a key to a CDN or fails the fetch.
	NeedsUpstreamCredential bool
	ContentType             string
	// Indirect means Request does not return bytes: it returns a document
	// naming where the bytes are. The caller hands that body to
	// ResolveArtifact and follows the artifact it gets back.
	//
	// One hop only, and the caller enforces it. MiniMax is the vendor that
	// needs it -- its poll yields a file id, and turning that into a URL is a
	// second call -- and a vendor needing a third would be the signal this
	// shape is wrong again rather than a reason to loop.
	Indirect bool
}

// ErrArtifactIsDirect is what a vendor whose artifact is a single hop answers
// when asked to resolve one. Returning an error rather than a zero Artifact
// keeps a caller that reached here by mistake from fetching nothing and
// recording it as custody taken.
var ErrArtifactIsDirect = fmt.Errorf("video: this vendor's artifact is fetched in one hop")

// ErrNoArtifact means a poll reported completion without anything to fetch.
var ErrNoArtifact = fmt.Errorf("video: the upstream reported completion with no artifact")

// registry is the set of vendors this build can reach.
//
// A map rather than a switch so that the set can be enumerated -- the operator
// UI has to be able to say which vendors are wireable, and a switch cannot be
// asked.
var registry = map[string]Mapper{}

func register(m Mapper) {
	if _, dup := registry[m.Vendor()]; dup {
		panic("video: two mappers registered for vendor " + m.Vendor())
	}
	registry[m.Vendor()] = m
}

// MapperFor returns the mapper for a vendor.
func MapperFor(vendor string) (Mapper, bool) {
	m, ok := registry[vendor]
	return m, ok
}

// jsonUnmarshal is the one decode every mapper shares, so that a vendor's
// response shape is read the same way everywhere.
func jsonUnmarshal(body []byte, v any) error { return json.Unmarshal(body, v) }

// mergePassthrough folds a compatibility surface's vendor parameters into an
// outbound body.
//
// A key already in the document is never replaced. The surface has already
// stripped every parameter that decides a billed quantity, so this is only
// belt to that braces -- but it is the belt that matters: a passthrough key
// reaching in and changing the duration would generate one clip and charge for
// another, which is the failure the whole per-unit design is arranged around.
func mergePassthrough(doc map[string]any, extra map[string]json.RawMessage) {
	for k, v := range extra {
		if existing, priced := doc[k]; priced {
			// Two objects under the same key are two halves of one thing --
			// this vendor's `parameters` block holds both priced and
			// unrecognised knobs -- so they are merged rather than one of them
			// winning whole. Anything else present already is priced and stands.
			if nested, ok := existing.(map[string]any); ok {
				mergePassthroughNested(nested, v)
			}
			continue
		}
		doc[k] = v
	}
}

// mergePassthroughNested folds an unrecognised sub-object into one the mapper
// has already built, under the same rule: a key the mapper set is priced and is
// never replaced.
func mergePassthroughNested(doc map[string]any, raw json.RawMessage) {
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extra); err != nil {
		// Not an object, so there is nothing to merge into. The mapper's own
		// value stands, which is the same outcome a collision produces.
		return
	}
	mergePassthrough(doc, extra)
}

// jsonBody marshals a request body, with the vendor named in any failure so a
// mapper bug is attributable.
func jsonBody(vendor string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("video: %s: encode request: %w", vendor, err)
	}
	return b, nil
}

// httpStatusOK is the one status check every mapper shares.
func okStatus(status int) bool { return status >= 200 && status < 300 }

// notFound is shared because "the vendor has forgotten this job" is decided the
// same way everywhere, and it is load-bearing: it is what eventually voids a
// hold for a job nobody will ever finish.
func isNotFound(status int) bool { return status == http.StatusNotFound }
