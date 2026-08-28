package video

import (
	"fmt"
	"strings"
)

func init() { register(kuaishouMapper{}) }

// kuaishouMapper reaches Kling.
//
// # The three things this vendor does differently
//
// It is in the first batch precisely because it disagrees with the other two on
// every axis that matters, which is what makes it a real test of the interface
// rather than a third copy of the same shape:
//
//   - **it has no resolution field.** Quality is `mode`: std or pro. The
//     mapping from a requested resolution to a mode is real rather than a fudge
//     -- pro is how Kling delivers 1080p -- but it is a mapping, so it lives
//     here with this comment rather than being implied by a shared field name
//     (ADR-0218).
//   - **the end frame is gated on pro mode.** Which is why the envelope has a
//     separate SupportsLastFrame rather than folding it into image-to-video.
//
// And one thing it does that shapes the interface: **the query path is per task
// type.** A job created at /v1/videos/image2video is read back at
// /v1/videos/image2video/{id}, never at the text2video path. So the identifier
// this mapper hands back is "image2video/<id>" rather than a bare id, and Poll
// joins it onto the collection prefix -- the same shape Veo's operation name
// already forced (ADR-0219's fence: the mapper decides, the proxy delivers).
// Reading an image-to-video job at the text2video path answers 404 forever,
// which the job machinery reads as "the vendor has forgotten this job" and
// eventually voids the hold -- for a clip the upstream generated and charged
// its own account for.
type kuaishouMapper struct{}

func (kuaishouMapper) Vendor() string { return "kuaishou" }

func (kuaishouMapper) Envelope(string) Envelope {
	return Envelope{
		DurationsSeconds: []int{5, 10},
		// Expressed as resolutions because that is what a caller chooses. The
		// mapper turns them into the mode that produces them.
		Resolutions:            []string{"720p", "1080p"},
		AspectRatios:           []string{"16:9", "9:16", "1:1"},
		Audio:                  AudioNever,
		MaxN:                   1,
		SupportsImageToVideo:   true,
		SupportsLastFrame:      true,
		MaxReferenceImages:     0,
		SupportsNegativePrompt: true,
		MaxPromptChars:         2500,
		Cancel:                 CancelNever,
		MaxJobSeconds:          1800,
		Source:                 "declared",
	}
}

// modeFor maps a requested resolution onto Kling's quality mode.
//
// std is the 720p tier and pro the 1080p one. An unspecified resolution takes
// std: the cheaper tier is the safe default, because a caller who did not ask
// for the dearer one should not be billed for it.
func modeFor(resolution string) string {
	switch resolution {
	case "1080p", "4k":
		return "pro"
	default:
		return "std"
	}
}

// Submit ignores audioOn: this vendor produces no sound and its envelope says
// `audio: never`, so a request that reached here has already been refused if
// it asked for any.
func (m kuaishouMapper) Submit(r Request, upstreamModel string, _ bool) (Outbound, error) {
	doc := map[string]any{
		"model_name": upstreamModel,
		"prompt":     r.Prompt,
		"mode":       modeFor(r.Resolution),
		"duration":   fmt.Sprintf("%d", r.DurationSeconds),
	}
	if r.NegativePrompt != "" {
		doc["negative_prompt"] = r.NegativePrompt
	}
	if r.AspectRatio != "" {
		doc["aspect_ratio"] = r.AspectRatio
	}
	if r.Image != "" {
		doc["image"] = r.Image
	}
	if r.LastFrame != "" {
		if modeFor(r.Resolution) != "pro" {
			// The upstream gates the end frame on pro mode. Refusing here,
			// where the caller can still act on it, is better than a job that
			// fails minutes later -- and better than silently dropping the
			// frame and producing something they did not ask for.
			return Outbound{}, fmt.Errorf(
				"video: kuaishou: an end frame requires 1080p on this model (it is a pro-mode feature)")
		}
		doc["image_tail"] = r.LastFrame
	}
	mergePassthrough(doc, r.Passthrough)
	body, err := jsonBody(m.Vendor(), doc)
	if err != nil {
		return Outbound{}, err
	}
	return Outbound{Method: "POST", Path: klingCollection + klingVariant(r), Body: body}, nil
}

// klingCollection is the prefix every one of this vendor's video paths shares.
const klingCollection = "/v1/videos/"

// klingVariant is which of this vendor's two task types a request is. It
// decides the create path, and through the identifier it also decides the query
// and cancel paths, which is the whole reason it is derived from the request in
// one place rather than spelled twice.
func klingVariant(r Request) string {
	if r.Image != "" {
		return "image2video"
	}
	return "text2video"
}

type klingEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    *struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
		TaskMsg    string `json:"task_status_msg"`
		TaskResult *struct {
			Videos []struct {
				URL      string `json:"url"`
				Duration string `json:"duration"`
			} `json:"videos"`
		} `json:"task_result"`
	} `json:"data"`
}

func (m kuaishouMapper) SubmitResult(r Request, status int, body []byte) (string, error) {
	if !okStatus(status) {
		return "", fmt.Errorf("video: kuaishou: submit returned %d: %.256s", status, body)
	}
	var env klingEnvelope
	if err := jsonUnmarshal(body, &env); err != nil {
		return "", fmt.Errorf("video: kuaishou: decode submit response: %w", err)
	}
	// This API answers 200 with a non-zero code for a refusal, so the HTTP
	// status alone is not the verdict.
	if env.Code != 0 || env.Data == nil || env.Data.TaskID == "" {
		return "", fmt.Errorf("video: kuaishou: submit refused: %s", env.Message)
	}
	// The task type travels with the id because the query path needs it and the
	// job row stores nothing else about how the job was created.
	return klingVariant(r) + "/" + env.Data.TaskID, nil
}

func (kuaishouMapper) Poll(upstreamID string) (Outbound, error) {
	if err := checkKlingID(upstreamID); err != nil {
		return Outbound{}, fmt.Errorf("video: kuaishou: %w to poll", err)
	}
	// The identifier is "<task type>/<id>", so it is joined onto the collection
	// rather than substituted into one task type's template.
	return Outbound{Method: "GET", Path: klingCollection + upstreamID}, nil
}

// checkKlingID refuses an identifier that cannot address a job. An id with no
// task type would be sent to /v1/videos/<id>, which is not an endpoint this
// vendor publishes, and the 404 would read as a forgotten job.
func checkKlingID(upstreamID string) error {
	if upstreamID == "" {
		return fmt.Errorf("no task id")
	}
	variant, id, ok := strings.Cut(upstreamID, "/")
	if !ok || id == "" || (variant != "text2video" && variant != "image2video") {
		return fmt.Errorf("task id %q does not name a task type", upstreamID)
	}
	return nil
}

func (m kuaishouMapper) PollResult(status int, body []byte) (Poll, error) {
	if isNotFound(status) {
		return Poll{Status: StatusFailed, NotFound: true, UpstreamStatus: "not_found"}, nil
	}
	if !okStatus(status) {
		return Poll{}, fmt.Errorf("video: kuaishou: poll returned %d: %.256s", status, body)
	}
	var env klingEnvelope
	if err := jsonUnmarshal(body, &env); err != nil {
		return Poll{}, fmt.Errorf("video: kuaishou: decode poll response: %w", err)
	}
	if env.Data == nil {
		return Poll{Status: StatusFailed, UpstreamStatus: "unknown",
			ErrorCode: "gateway.video_content_rejected", ErrorMessage: env.Message}, nil
	}
	p := Poll{UpstreamStatus: env.Data.TaskStatus}
	switch strings.ToLower(env.Data.TaskStatus) {
	case "submitted":
		p.Status = StatusQueued
	case "processing":
		p.Status = StatusInProgress
	case "succeed":
		p.Status = StatusCompleted
		p.Progress = 100
		p.ContentType = "video/mp4"
		if env.Data.TaskResult != nil && len(env.Data.TaskResult.Videos) > 0 {
			p.ArtifactRef = env.Data.TaskResult.Videos[0].URL
		}
		if p.ArtifactRef == "" {
			p.Status = StatusFailed
			p.ErrorCode = "gateway.video_content_rejected"
			p.ErrorMessage = "the upstream reported success without producing a video"
		}
	default: // "failed" and anything unrecognised
		p.Status = StatusFailed
		p.ErrorCode = "gateway.video_content_rejected"
		p.ErrorMessage = orDefault(env.Data.TaskMsg, env.Message)
	}
	return p, nil
}

// CancelMode: none.
//
// This was CancelQueuedOnly with a DELETE, and that was wrong. The DELETE is
// published by a third-party relay that fronts this vendor, not by the vendor's
// own open platform, whose reference documents create and query per task type
// and nothing else. Sending it would have answered 404 over a healthy
// connection, which VideoJobs.Cancel correctly reads as "already generating" --
// so the visible symptom of the mistake was a cancel button that always failed
// with the wrong reason.
//
// Recorded rather than quietly corrected because ADR-0219 named "cancel only
// while queued" as one of the three ways this vendor was supposed to stretch
// the interface, and one of those three was never real. CancelQueuedOnly stays
// in the type: the envelope is the operator's to declare, and this only says
// what the bundled prefill may promise (ADR-0221).
func (kuaishouMapper) CancelMode() CancelMode { return CancelNever }

func (kuaishouMapper) Cancel(string) (Outbound, error) {
	return Outbound{}, fmt.Errorf(
		"video: kuaishou: this vendor publishes no cancel; a job cannot be stopped once submitted")
}

// ResolveArtifact: the poll already carries the CDN link.
func (kuaishouMapper) ResolveArtifact(int, []byte) (Artifact, error) {
	return Artifact{}, ErrArtifactIsDirect
}

func (kuaishouMapper) Artifact(p Poll) (Artifact, error) {
	if p.ArtifactRef == "" {
		return Artifact{}, ErrNoArtifact
	}
	return Artifact{
		Request:                 Outbound{Method: "GET", URL: p.ArtifactRef},
		NeedsUpstreamCredential: false,
		ContentType:             orDefault(p.ContentType, "video/mp4"),
	}, nil
}
