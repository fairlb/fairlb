package video

import (
	"fmt"
	"strings"
)

func init() { register(alibabaMapper{}) }

// alibabaMapper reaches Wan through Model Studio's asynchronous task API.
//
// # Where this vendor differs
//
//   - **asynchrony is a request header**, not a property of the endpoint:
//     without `X-DashScope-Async: enable` the same path blocks. It is sent on
//     the create call and nowhere else.
//   - **duration is a range, not an enum.** Two to fifteen seconds, any integer.
//     The envelope enumerates it anyway, because Envelope.DurationsSeconds is a
//     closed set and adding a min/max axis beside it would fork Union, Validate
//     and the operator's price grid to save a few lines here.
//   - **the input images are a typed list**, `media: [{type: first_frame}, …]`,
//     which is the closest thing to this contract's own three image roles that
//     any upstream publishes.
//   - **its status words are upper case**, and one of them, UNKNOWN, means the
//     task id has aged out rather than that anything went wrong.
//
// The base URL is not the one this vendor's text models use. Its
// OpenAI-compatible surface lives under /compatible-mode and this API does not,
// so a Wan channel is a second provider record -- the same shape Vertex and
// Bedrock already need. The recipe is in the documentation.
type alibabaMapper struct{}

func (alibabaMapper) Vendor() string { return "alibaba" }

// wanDurations is the two-to-fifteen-second range, enumerated.
//
// Written out rather than expressed as bounds because the one axis that carries
// an admissible *set* everywhere else would otherwise carry a range here, and a
// reader of GET /v1/videos/models would have to know which kind each model is.
// Fourteen entries is a cheap price for one shape.
func wanDurations() []int {
	out := make([]int, 0, 14)
	for d := 2; d <= 15; d++ {
		out = append(out, d)
	}
	return out
}

// wanIsImageToVideo reads the task type out of the model id, which is how this
// vendor names them: wan2.7-t2v-… and wan2.7-i2v-….
func wanIsImageToVideo(upstreamModel string) bool {
	return strings.Contains(strings.ToLower(upstreamModel), "i2v")
}

func (alibabaMapper) Envelope(upstreamModel string) Envelope {
	e := Envelope{
		DurationsSeconds:       wanDurations(),
		Resolutions:            []string{"720p", "1080p"},
		AspectRatios:           []string{"16:9", "9:16", "1:1", "4:3", "3:4"},
		Audio:                  AudioNever,
		MaxN:                   1,
		SupportsImageToVideo:   true,
		SupportsLastFrame:      true,
		MaxReferenceImages:     0,
		SupportsNegativePrompt: true,
		// Not set: this vendor publishes no character limit for the prompt, and
		// inventing one here would refuse requests the upstream would accept.
		MaxPromptChars: 0,
		Cancel:         CancelNever,
		MaxJobSeconds:  1800,
		Source:         "declared",
	}
	if wanIsImageToVideo(upstreamModel) {
		// The image-to-video models take no ratio: the shape comes from the
		// first frame. Declared empty rather than copied from the text model,
		// because an accepted-then-ignored aspect ratio is a clip the caller
		// did not ask for.
		e.AspectRatios = nil
	}
	return e
}

// wanResolution spells a normalised resolution the way this API does. Same kind
// of mapping as Kling's mode: real, one line, and living in the vendor's file
// rather than implied by a shared field name (ADR-0218).
func wanResolution(r string) string {
	if r == "" {
		return ""
	}
	return strings.ToUpper(r)
}

type wanMedia struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

func (m alibabaMapper) Submit(r Request, upstreamModel string, _ bool) (Outbound, error) {
	input := map[string]any{}
	if r.Prompt != "" {
		input["prompt"] = r.Prompt
	}
	if r.NegativePrompt != "" {
		input["negative_prompt"] = r.NegativePrompt
	}
	var media []wanMedia
	if r.Image != "" {
		media = append(media, wanMedia{Type: "first_frame", URL: r.Image})
	}
	if r.LastFrame != "" {
		media = append(media, wanMedia{Type: "last_frame", URL: r.LastFrame})
	}
	if len(media) > 0 {
		input["media"] = media
	}

	params := map[string]any{}
	if res := wanResolution(r.Resolution); res != "" {
		params["resolution"] = res
	}
	if r.DurationSeconds > 0 {
		params["duration"] = r.DurationSeconds
	}
	// The image-to-video models reject a ratio rather than ignoring it, and the
	// envelope has already refused one for them, so this only ever fires on the
	// text models.
	if r.AspectRatio != "" && !wanIsImageToVideo(upstreamModel) {
		params["ratio"] = r.AspectRatio
	}
	if r.Seed != nil {
		params["seed"] = *r.Seed
	}

	doc := map[string]any{"model": upstreamModel, "input": input, "parameters": params}
	// Merged at the body level, because a compatibility surface's passthrough
	// for this vendor is body-shaped: its unrecognised fields live under
	// `input` as well as under `parameters`, and mergePassthrough folds each
	// sub-object into the one already built rather than replacing it.
	mergePassthrough(doc, r.Passthrough)
	// Defaulted rather than inherited: this vendor's own default for the
	// watermark has moved before, and a clip that comes back stamped when
	// nobody asked for one is a clip the caller did not order. Defaulted rather
	// than forced, for the same reason the prompt optimiser is: on this
	// vendor's own surface the caller may ask for a watermark, and that is
	// their choice about their own API.
	if _, chosen := params["watermark"]; !chosen {
		params["watermark"] = false
	}
	body, err := jsonBody(m.Vendor(), doc)
	if err != nil {
		return Outbound{}, err
	}
	return Outbound{
		Method: "POST",
		Path:   "/api/v1/services/aigc/video-generation/video-synthesis",
		Body:   body,
		// Without this the same path blocks until the clip is finished, which
		// on this plane means a request that outlives every timeout between
		// here and the caller.
		Headers: map[string]string{"X-DashScope-Async": "enable"},
	}, nil
}

type wanTask struct {
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Output    *struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
		VideoURL   string `json:"video_url"`
		Code       string `json:"code"`
		Message    string `json:"message"`
	} `json:"output"`
}

func (m alibabaMapper) SubmitResult(_ Request, status int, body []byte) (string, error) {
	if !okStatus(status) {
		return "", fmt.Errorf("video: alibaba: submit returned %d: %.256s", status, body)
	}
	var task wanTask
	if err := jsonUnmarshal(body, &task); err != nil {
		return "", fmt.Errorf("video: alibaba: decode submit response: %w", err)
	}
	if task.Code != "" {
		return "", fmt.Errorf("video: alibaba: submit refused (%s): %s", task.Code, task.Message)
	}
	if task.Output == nil || task.Output.TaskID == "" {
		return "", fmt.Errorf("video: alibaba: submit returned no task id")
	}
	return task.Output.TaskID, nil
}

func (alibabaMapper) Poll(upstreamID string) (Outbound, error) {
	if upstreamID == "" {
		return Outbound{}, fmt.Errorf("video: alibaba: no task id to poll")
	}
	return Outbound{Method: "GET", Path: "/api/v1/tasks/" + upstreamID}, nil
}

func (m alibabaMapper) PollResult(status int, body []byte) (Poll, error) {
	if isNotFound(status) {
		return Poll{Status: StatusFailed, NotFound: true, UpstreamStatus: "not_found"}, nil
	}
	if !okStatus(status) {
		return Poll{}, fmt.Errorf("video: alibaba: poll returned %d: %.256s", status, body)
	}
	var task wanTask
	if err := jsonUnmarshal(body, &task); err != nil {
		return Poll{}, fmt.Errorf("video: alibaba: decode poll response: %w", err)
	}
	if task.Output == nil {
		return Poll{Status: StatusFailed, UpstreamStatus: "unknown",
			ErrorCode:    "gateway.video_content_rejected",
			ErrorMessage: orDefault(task.Message, "the upstream returned no task")}, nil
	}
	p := Poll{UpstreamStatus: task.Output.TaskStatus}
	switch strings.ToUpper(task.Output.TaskStatus) {
	case "PENDING":
		p.Status = StatusQueued
	case "RUNNING":
		p.Status = StatusInProgress
	case "SUCCEEDED":
		p.Status = StatusCompleted
		p.Progress = 100
		p.ContentType = "video/mp4"
		p.ArtifactRef = task.Output.VideoURL
		if p.ArtifactRef == "" {
			// Reported success with nothing to fetch. Charging for it would
			// settle a bill for a video that does not exist.
			p.Status = StatusFailed
			p.ErrorCode = "gateway.video_content_rejected"
			p.ErrorMessage = "the upstream reported success without producing a video"
		}
	case "CANCELED", "CANCELLED":
		p.Status = StatusCanceled
	case "UNKNOWN":
		// The task id has aged out -- this vendor keeps one for a day. That is
		// the same fact a 404 carries elsewhere, so it is reported the same
		// way: one reading is not proof, and the job worker needs two.
		p.Status = StatusFailed
		p.NotFound = true
	default: // "FAILED" and anything unrecognised
		p.Status = StatusFailed
		p.ErrorCode = "gateway.video_content_rejected"
		p.ErrorMessage = orDefault(task.Output.Message, "the upstream refused this request")
	}
	return p, nil
}

// CancelMode: this vendor's task-management reference documents querying but no
// cancel for a generation task. Saying so beats a stop button that always fails
// (ADR-0221).
func (alibabaMapper) CancelMode() CancelMode { return CancelNever }

func (alibabaMapper) Cancel(string) (Outbound, error) {
	return Outbound{}, fmt.Errorf("video: alibaba: this model cannot be cancelled once submitted")
}

// ResolveArtifact: the poll already carries the link.
func (alibabaMapper) ResolveArtifact(int, []byte) (Artifact, error) {
	return Artifact{}, ErrArtifactIsDirect
}

func (alibabaMapper) Artifact(p Poll) (Artifact, error) {
	if p.ArtifactRef == "" {
		return Artifact{}, ErrNoArtifact
	}
	return Artifact{
		Request: Outbound{Method: "GET", URL: p.ArtifactRef},
		// A signed link this vendor publishes as valid for a day. It carries
		// its own authorisation; attaching ours would send the upstream key to
		// a CDN.
		NeedsUpstreamCredential: false,
		ContentType:             orDefault(p.ContentType, "video/mp4"),
	}, nil
}
