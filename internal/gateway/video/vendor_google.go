package video

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func init() { register(googleMapper{}) }

// googleMapper reaches Veo through the Gemini API's long-running operations.
//
// # The shape, and where it differs from every other vendor
//
// Veo is addressed as `models/{model}:predictLongRunning`, and the job
// identifier that comes back is an *operation name* -- a path, not an opaque
// id. Polling is a GET of that path, which is why Poll builds its request from
// the identifier rather than substituting it into a fixed template.
//
// Duration is a string enum ("4", "6", "8") rather than a number. Audio is not
// a parameter at all: Veo 3.1 generates it natively, so the envelope for these
// models says `always` and nothing about audio is sent. Aspect ratio is limited
// to the two the API documents.
//
// Generated videos are kept on Google's servers for two days, which is why the
// reconciler takes custody as soon as a job completes rather than when a caller
// first asks for the bytes (ADR-0222).
type googleMapper struct{}

func (googleMapper) Vendor() string { return "google" }

func (googleMapper) Envelope(upstreamModel string) Envelope {
	e := Envelope{
		DurationsSeconds: []int{4, 6, 8},
		Resolutions:      []string{"720p", "1080p"},
		AspectRatios:     []string{"16:9", "9:16"},
		// Native audio, and no parameter to turn it off.
		Audio: AudioAlways,
		MaxN:  1,
		// Image-to-video, frame interpolation and up to three typed reference
		// images are all documented for 3.1.
		SupportsImageToVideo: true,
		SupportsLastFrame:    true,
		MaxReferenceImages:   3,
		// The Gemini API does not document negativePrompt for Veo 3.1 even
		// though Vertex accepts it. Declared false here: an operator whose
		// deployment does accept it can say so, and claiming it by default
		// would silently drop the field on the deployments that do not.
		SupportsNegativePrompt: false,
		// Not set: the published limit is 1024 *tokens*, and configuring a
		// token count as a character count would refuse valid prompts.
		MaxPromptChars: 0,
		Cancel:         CancelNever,
		MaxJobSeconds:  900,
		Source:         "declared",
	}
	if strings.Contains(upstreamModel, "4k") {
		e.Resolutions = append(e.Resolutions, "4k")
	}
	return e
}

// veoInstance is one generation request's inputs.
type veoInstance struct {
	Prompt    string        `json:"prompt,omitempty"`
	Image     *veoImage     `json:"image,omitempty"`
	LastFrame *veoImage     `json:"lastFrame,omitempty"`
	Reference []veoRefImage `json:"referenceImages,omitempty"`
}

type veoImage struct {
	BytesBase64 string `json:"bytesBase64Encoded,omitempty"`
	GCSURI      string `json:"gcsUri,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

type veoRefImage struct {
	Image         veoImage `json:"image"`
	ReferenceType string   `json:"referenceType"`
}

type veoParameters struct {
	AspectRatio      string `json:"aspectRatio,omitempty"`
	Resolution       string `json:"resolution,omitempty"`
	DurationSeconds  string `json:"durationSeconds,omitempty"`
	NegativePrompt   string `json:"negativePrompt,omitempty"`
	PersonGeneration string `json:"personGeneration,omitempty"`
	NumberOfVideos   int    `json:"numberOfVideos,omitempty"`
	Seed             *int64 `json:"seed,omitempty"`
}

// Submit ignores audioOn: Veo 3.1 generates sound natively and publishes no
// parameter for it, which is why these models declare `audio: always`. The
// argument is accepted and unused rather than absent so that the one mapper
// where audio is not a choice says so here instead of by omission.
func (m googleMapper) Submit(r Request, upstreamModel string, _ bool) (Outbound, error) {
	inst := veoInstance{Prompt: r.Prompt}
	if r.Image != "" {
		img, err := veoImageOf(r.Image)
		if err != nil {
			return Outbound{}, err
		}
		inst.Image = &img
	}
	if r.LastFrame != "" {
		img, err := veoImageOf(r.LastFrame)
		if err != nil {
			return Outbound{}, err
		}
		inst.LastFrame = &img
	}
	for _, ref := range r.ReferenceImages {
		img, err := veoImageOf(ref.URL)
		if err != nil {
			return Outbound{}, err
		}
		// The normalised roles map onto this API's own two.
		kind := "asset"
		if ref.Type == ReferenceStyle {
			kind = "style"
		}
		inst.Reference = append(inst.Reference, veoRefImage{Image: img, ReferenceType: kind})
	}

	params := veoParameters{
		AspectRatio:     r.AspectRatio,
		Resolution:      r.Resolution,
		DurationSeconds: strconv.Itoa(r.DurationSeconds),
		NegativePrompt:  r.NegativePrompt,
		NumberOfVideos:  r.N,
		Seed:            r.Seed,
	}

	doc := map[string]any{
		"instances":  []veoInstance{inst},
		"parameters": params,
	}
	if len(r.Passthrough) > 0 {
		// The parameter block becomes a map so that a compatibility surface's
		// own knobs can sit beside the priced ones without being able to
		// replace them, and the merge runs at the body level because that
		// passthrough is body-shaped.
		doc["parameters"] = veoParameterMap(params)
		mergePassthrough(doc, r.Passthrough)
	}
	body, err := jsonBody(m.Vendor(), doc)
	if err != nil {
		return Outbound{}, err
	}
	return Outbound{
		Method: "POST",
		Path:   "/v1beta/models/" + upstreamModel + ":predictLongRunning",
		Body:   body,
	}, nil
}

// veoParameterMap renders the typed parameter block as a map, so that a
// compatibility surface's vendor parameters can be merged beside it without
// being able to replace one of these.
func veoParameterMap(p veoParameters) map[string]any {
	out := map[string]any{}
	if p.AspectRatio != "" {
		out["aspectRatio"] = p.AspectRatio
	}
	if p.Resolution != "" {
		out["resolution"] = p.Resolution
	}
	if p.DurationSeconds != "" {
		out["durationSeconds"] = p.DurationSeconds
	}
	if p.NegativePrompt != "" {
		out["negativePrompt"] = p.NegativePrompt
	}
	if p.PersonGeneration != "" {
		out["personGeneration"] = p.PersonGeneration
	}
	if p.NumberOfVideos != 0 {
		out["numberOfVideos"] = p.NumberOfVideos
	}
	if p.Seed != nil {
		out["seed"] = *p.Seed
	}
	return out
}

func veoImageOf(ref string) (veoImage, error) {
	switch {
	case strings.HasPrefix(ref, "gs://"):
		return veoImage{GCSURI: ref}, nil
	case strings.HasPrefix(ref, "data:"):
		mime, payload, ok := splitDataURL(ref)
		if !ok {
			return veoImage{}, fmt.Errorf("video: google: malformed data URL")
		}
		return veoImage{BytesBase64: payload, MIMEType: mime}, nil
	default:
		// This API takes bytes or a GCS URI; it will not fetch an arbitrary
		// https URL. Refusing is the honest answer -- silently dropping the
		// image would generate a text-to-video clip and bill for it.
		return veoImage{}, fmt.Errorf(
			"video: google: image references must be a data URL or a gs:// URI, not %.32q", ref)
	}
}

func splitDataURL(s string) (mime, payload string, ok bool) {
	rest, found := strings.CutPrefix(s, "data:")
	if !found {
		return "", "", false
	}
	head, body, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	mime, _, _ = strings.Cut(head, ";")
	return mime, body, true
}

// veoOperation is the long-running operation envelope.
type veoOperation struct {
	Name     string     `json:"name"`
	Done     bool       `json:"done"`
	Error    *veoStatus `json:"error"`
	Response *struct {
		GenerateVideoResponse *struct {
			GeneratedSamples []struct {
				Video struct {
					URI      string `json:"uri"`
					MIMEType string `json:"mimeType"`
				} `json:"video"`
			} `json:"generatedSamples"`
			RaiMediaFilteredReasons []string `json:"raiMediaFilteredReasons"`
		} `json:"generateVideoResponse"`
	} `json:"response"`
}

type veoStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func (m googleMapper) SubmitResult(_ Request, status int, body []byte) (string, error) {
	if !okStatus(status) {
		return "", fmt.Errorf("video: google: submit returned %d: %.256s", status, body)
	}
	var op veoOperation
	if err := jsonUnmarshal(body, &op); err != nil {
		return "", fmt.Errorf("video: google: decode submit response: %w", err)
	}
	if op.Name == "" {
		return "", fmt.Errorf("video: google: submit returned no operation name")
	}
	return op.Name, nil
}

func (m googleMapper) Poll(upstreamID string) (Outbound, error) {
	if upstreamID == "" {
		return Outbound{}, fmt.Errorf("video: google: no operation name to poll")
	}
	// The identifier is itself a path ("models/…/operations/…"), so it is
	// appended rather than substituted.
	return Outbound{Method: "GET", Path: "/v1beta/" + strings.TrimPrefix(upstreamID, "/")}, nil
}

func (m googleMapper) PollResult(status int, body []byte) (Poll, error) {
	if isNotFound(status) {
		return Poll{Status: StatusFailed, NotFound: true, UpstreamStatus: "NOT_FOUND"}, nil
	}
	if !okStatus(status) {
		return Poll{}, fmt.Errorf("video: google: poll returned %d: %.256s", status, body)
	}
	var op veoOperation
	if err := jsonUnmarshal(body, &op); err != nil {
		return Poll{}, fmt.Errorf("video: google: decode poll response: %w", err)
	}
	if !op.Done {
		return Poll{Status: StatusInProgress, UpstreamStatus: "RUNNING"}, nil
	}
	if op.Error != nil && (op.Error.Code != 0 || op.Error.Message != "") {
		return Poll{
			Status: StatusFailed, UpstreamStatus: op.Error.Status,
			ErrorCode: "gateway.video_content_rejected", ErrorMessage: op.Error.Message,
		}, nil
	}
	resp := op.Response
	if resp == nil || resp.GenerateVideoResponse == nil || len(resp.GenerateVideoResponse.GeneratedSamples) == 0 {
		// Done, no error, no sample: this is what a safety filter looks like on
		// this API. Reporting it as a failure with the filter's own reason is
		// the difference between a caller who knows why and one who does not.
		reason := "the upstream produced no video and gave no reason"
		if resp != nil && resp.GenerateVideoResponse != nil &&
			len(resp.GenerateVideoResponse.RaiMediaFilteredReasons) > 0 {
			reason = strings.Join(resp.GenerateVideoResponse.RaiMediaFilteredReasons, "; ")
		}
		return Poll{
			Status: StatusFailed, UpstreamStatus: "DONE_EMPTY",
			ErrorCode: "gateway.video_content_rejected", ErrorMessage: reason,
		}, nil
	}
	sample := resp.GenerateVideoResponse.GeneratedSamples[0].Video
	return Poll{
		Status: StatusCompleted, UpstreamStatus: "DONE", Progress: 100,
		ArtifactRef: sample.URI, ContentType: orDefault(sample.MIMEType, "video/mp4"),
		// Google keeps a generated video for two days.
		ArtifactExpiry: time.Time{},
	}, nil
}

// CancelMode: the Gemini API documents no cancel for predictLongRunning.
// Saying so is the point -- a cancel that silently does nothing is worse than
// one that refuses (ADR-0221).
func (googleMapper) CancelMode() CancelMode { return CancelNever }

func (m googleMapper) Cancel(string) (Outbound, error) {
	return Outbound{}, fmt.Errorf("video: google: this model cannot be cancelled once submitted")
}

// ResolveArtifact: Google's file URI is fetched directly.
func (googleMapper) ResolveArtifact(int, []byte) (Artifact, error) {
	return Artifact{}, ErrArtifactIsDirect
}

func (m googleMapper) Artifact(p Poll) (Artifact, error) {
	if p.ArtifactRef == "" {
		return Artifact{}, ErrNoArtifact
	}
	return Artifact{
		Request: Outbound{Method: "GET", URL: p.ArtifactRef},
		// The file URI is served by Google and authenticated as the upstream
		// account, so the credential has to travel with the fetch.
		NeedsUpstreamCredential: true,
		ContentType:             orDefault(p.ContentType, "video/mp4"),
	}, nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
