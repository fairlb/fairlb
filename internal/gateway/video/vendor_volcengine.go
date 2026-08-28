package video

import (
	"fmt"
	"strings"
)

func init() { register(volcengineMapper{}) }

// volcengineMapper reaches Seedance through Volcano Engine's content-generation
// tasks API.
//
// # The shape, and where it differs
//
// This is the plain task-poll form: POST a task, get an id, GET the id until it
// reports a terminal state. Two things about it are worth stating because they
// are not shared with the other vendors:
//
//   - the input is a *content array* rather than a flat parameter object --
//     `[{type: "text", text: "..."}, {type: "image_url", ...}]`, the same shape
//     a chat message uses. Generation parameters ride along inside the text
//     item as `--key value` commands, which is this vendor's own convention.
//   - the artifact is a presigned CDN URL that expires, and it is *not*
//     credential-authenticated: fetching it with our key attached would leak
//     the key to a CDN.
type volcengineMapper struct{}

func (volcengineMapper) Vendor() string { return "volcengine" }

func (volcengineMapper) Envelope(string) Envelope {
	return Envelope{
		DurationsSeconds:       []int{4, 8, 12},
		Resolutions:            []string{"480p", "720p", "1080p"},
		AspectRatios:           []string{"16:9", "9:16", "1:1", "4:3", "3:4", "21:9"},
		Audio:                  AudioOptional,
		MaxN:                   1,
		SupportsImageToVideo:   true,
		SupportsLastFrame:      false,
		MaxReferenceImages:     0,
		SupportsNegativePrompt: false,
		MaxPromptChars:         2000,
		Cancel:                 CancelNever,
		MaxJobSeconds:          1800,
		Source:                 "declared",
	}
}

// promptCommands appends this vendor's inline parameter commands.
//
// They are part of the prompt text by that API's own design, not a hack: the
// text item carries `--resolution 1080p --duration 8` and so on. Building them
// here rather than letting a caller write them is the point -- a caller who
// wrote `--duration 12` in their prompt would get a clip this gateway priced at
// something else, which is why the normalised parameters are the only source.
func promptCommands(r Request, audioOn bool) string {
	var b strings.Builder
	b.WriteString(r.Prompt)
	if r.Resolution != "" {
		fmt.Fprintf(&b, " --resolution %s", r.Resolution)
	}
	if r.DurationSeconds > 0 {
		fmt.Fprintf(&b, " --duration %d", r.DurationSeconds)
	}
	if r.AspectRatio != "" {
		fmt.Fprintf(&b, " --ratio %s", r.AspectRatio)
	}
	if audioOn {
		b.WriteString(" --audio true")
	}
	if r.Seed != nil {
		fmt.Fprintf(&b, " --seed %d", *r.Seed)
	}
	return b.String()
}

func (m volcengineMapper) Submit(r Request, upstreamModel string, audioOn bool) (Outbound, error) {
	// audioOn, not r.Audio: this model's rate card prices a scored clip
	// differently from a silent one, and the value that was priced is the one
	// resolved against the route's stored envelope. Reading the tri-state field
	// here would send a silent clip for a job billed with sound.
	content := []map[string]any{
		{"type": "text", "text": promptCommands(r, audioOn)},
	}
	if r.Image != "" {
		content = append(content, map[string]any{
			"type": "image_url", "image_url": map[string]string{"url": r.Image},
		})
	}
	doc := map[string]any{"model": upstreamModel, "content": content}
	mergePassthrough(doc, r.Passthrough)
	body, err := jsonBody(m.Vendor(), doc)
	if err != nil {
		return Outbound{}, err
	}
	return Outbound{Method: "POST", Path: "/api/v3/contents/generations/tasks", Body: body}, nil
}

type volcTask struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Content *struct {
		VideoURL string `json:"video_url"`
	} `json:"content"`
}

func (m volcengineMapper) SubmitResult(_ Request, status int, body []byte) (string, error) {
	if !okStatus(status) {
		return "", fmt.Errorf("video: volcengine: submit returned %d: %.256s", status, body)
	}
	var task volcTask
	if err := jsonUnmarshal(body, &task); err != nil {
		return "", fmt.Errorf("video: volcengine: decode submit response: %w", err)
	}
	if task.ID == "" {
		return "", fmt.Errorf("video: volcengine: submit returned no task id")
	}
	return task.ID, nil
}

func (volcengineMapper) Poll(upstreamID string) (Outbound, error) {
	if upstreamID == "" {
		return Outbound{}, fmt.Errorf("video: volcengine: no task id to poll")
	}
	return Outbound{Method: "GET", Path: "/api/v3/contents/generations/tasks/" + upstreamID}, nil
}

func (m volcengineMapper) PollResult(status int, body []byte) (Poll, error) {
	if isNotFound(status) {
		return Poll{Status: StatusFailed, NotFound: true, UpstreamStatus: "not_found"}, nil
	}
	if !okStatus(status) {
		return Poll{}, fmt.Errorf("video: volcengine: poll returned %d: %.256s", status, body)
	}
	var task volcTask
	if err := jsonUnmarshal(body, &task); err != nil {
		return Poll{}, fmt.Errorf("video: volcengine: decode poll response: %w", err)
	}
	p := Poll{UpstreamStatus: task.Status}
	switch strings.ToLower(task.Status) {
	case "queued", "pending":
		p.Status = StatusQueued
	case "running", "in_progress", "processing":
		p.Status = StatusInProgress
	case "succeeded", "success", "done":
		p.Status = StatusCompleted
		p.Progress = 100
		if task.Content != nil {
			p.ArtifactRef = task.Content.VideoURL
		}
		if p.ArtifactRef == "" {
			// Reported success with nothing to fetch. Charging for it would
			// settle a bill for a video that does not exist.
			p.Status = StatusFailed
			p.ErrorCode = "gateway.video_content_rejected"
			p.ErrorMessage = "the upstream reported success without producing a video"
		}
		p.ContentType = "video/mp4"
	case "cancelled", "canceled":
		p.Status = StatusCanceled
	default:
		p.Status = StatusFailed
		p.ErrorCode = "gateway.video_content_rejected"
		p.ErrorMessage = "the upstream refused this request"
		if task.Error != nil {
			p.ErrorMessage = task.Error.Message
		}
	}
	return p, nil
}

func (volcengineMapper) CancelMode() CancelMode { return CancelNever }

func (m volcengineMapper) Cancel(string) (Outbound, error) {
	return Outbound{}, fmt.Errorf("video: volcengine: this model cannot be cancelled once submitted")
}

// ResolveArtifact: the poll already carries the CDN link.
func (volcengineMapper) ResolveArtifact(int, []byte) (Artifact, error) {
	return Artifact{}, ErrArtifactIsDirect
}

func (m volcengineMapper) Artifact(p Poll) (Artifact, error) {
	if p.ArtifactRef == "" {
		return Artifact{}, ErrNoArtifact
	}
	return Artifact{
		Request: Outbound{Method: "GET", URL: p.ArtifactRef},
		// A presigned CDN link, already carrying its own authorisation.
		// Attaching our credential would send the upstream key to a CDN.
		NeedsUpstreamCredential: false,
		ContentType:             orDefault(p.ContentType, "video/mp4"),
	}, nil
}
