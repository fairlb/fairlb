package video

import (
	"fmt"
	"strconv"
	"strings"
)

func init() { register(minimaxMapper{}) }

// minimaxMapper reaches Hailuo.
//
// # The one thing this vendor does that no other does
//
// **Its poll does not say where the bytes are.** It answers a `file_id`, and
// turning that into a URL is a second call to the file API. Every other vendor
// here hands back a link -- Google's own, or a presigned CDN one -- so the
// artifact was a single hop until this mapper.
//
// That is why Artifact carries Indirect and the interface has ResolveArtifact.
// ADR-0219 said adding a fourth vendor should be one file, and that if it were
// not, the interface had been shaped by the coincidences of the first three.
// This is that case, and the fix was the interface rather than a mapper that
// issues its own request -- which would have broken the fence that keeps these
// files pure.
//
// Two smaller differences, both shared with vendors already here:
//
//   - it answers 200 with a non-zero `base_resp.status_code` for a refusal, so
//     the HTTP status alone is not the verdict (Kling does this too);
//   - its resolutions are spelled with a capital P.
type minimaxMapper struct{}

func (minimaxMapper) Vendor() string { return "minimax" }

func (minimaxMapper) Envelope(upstreamModel string) Envelope {
	e := Envelope{
		DurationsSeconds: []int{6, 10},
		Resolutions:      []string{"512p", "768p", "1080p"},
		// Not declared: this API takes no aspect-ratio parameter. The shape
		// comes from the first frame, or from the model's default. An empty
		// list means "this axis is not constrained", which is the truth here --
		// declaring the three common ratios would refuse nothing and would
		// promise a choice the caller does not have.
		AspectRatios: nil,
		// The 2.3 family has no audio parameter and produces none.
		Audio:                  AudioNever,
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
	// The fast tier tops out below the flagship's ceiling.
	if strings.Contains(strings.ToLower(upstreamModel), "fast") {
		e.Resolutions = []string{"512p", "768p"}
	}
	return e
}

// minimaxResolution spells a normalised resolution the way this API does.
//
// A real mapping rather than a fudge -- 1080p and 1080P are the same tier -- but
// a mapping, so it lives in this vendor's file with this comment rather than
// being implied by a shared field name (ADR-0218).
func minimaxResolution(r string) string {
	if r == "" {
		return ""
	}
	return strings.ToUpper(r)
}

func (m minimaxMapper) Submit(r Request, upstreamModel string, _ bool) (Outbound, error) {
	doc := map[string]any{"model": upstreamModel}
	if r.Prompt != "" {
		doc["prompt"] = r.Prompt
	}
	if r.DurationSeconds > 0 {
		doc["duration"] = r.DurationSeconds
	}
	if res := minimaxResolution(r.Resolution); res != "" {
		doc["resolution"] = res
	}
	if r.Image != "" {
		// A public https URL or a data URL, both of which this API takes as
		// they are -- no fetch and no re-encoding on our side.
		doc["first_frame_image"] = r.Image
	}
	mergePassthrough(doc, r.Passthrough)
	// This API defaults prompt_optimizer to true, which rewrites the caller's
	// prompt before generating. Defaulted off here, because a normalised
	// request asked for the prompt it wrote and a silently rewritten one is not
	// that. Defaulted rather than forced: on this vendor's own compatibility
	// surface a caller can ask for the optimiser explicitly, and their own
	// choice about their own API outranks our default for ours.
	if _, chosen := doc["prompt_optimizer"]; !chosen {
		doc["prompt_optimizer"] = false
	}
	body, err := jsonBody(m.Vendor(), doc)
	if err != nil {
		return Outbound{}, err
	}
	return Outbound{Method: "POST", Path: "/v1/video_generation", Body: body}, nil
}

// minimaxBase is the envelope every response on this API carries. A non-zero
// code inside a 200 is how this vendor refuses.
type minimaxBase struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

type minimaxResp struct {
	TaskID  string       `json:"task_id"`
	Status  string       `json:"status"`
	FileID  any          `json:"file_id"`
	BaseRes *minimaxBase `json:"base_resp"`
}

type minimaxFile struct {
	File *struct {
		FileID      any    `json:"file_id"`
		DownloadURL string `json:"download_url"`
	} `json:"file"`
	BaseRes *minimaxBase `json:"base_resp"`
}

// minimaxID reads an identifier this API spells inconsistently: its own
// examples quote the task id and its OpenAPI declares file_id an int64.
// Decoding into `any` and rendering here accepts both rather than failing on
// whichever one an account's tier happens to return.
func minimaxID(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return ""
	}
}

// minimaxSensitiveContent is the code this API uses for a prompt its safety
// system refused. It is a terminal outcome with a reason rather than a
// transport failure, and the difference decides whether the job is polled again
// or the hold is released.
const minimaxSensitiveContent = 1026

func (m minimaxMapper) SubmitResult(_ Request, status int, body []byte) (string, error) {
	if !okStatus(status) {
		return "", fmt.Errorf("video: minimax: submit returned %d: %.256s", status, body)
	}
	var resp minimaxResp
	if err := jsonUnmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("video: minimax: decode submit response: %w", err)
	}
	// A refusal arrives as 200 with a non-zero code, so the HTTP status alone
	// is not the verdict. Reading only the status would take a refusal for a
	// submitted job and then poll a task id that does not exist.
	if resp.BaseRes != nil && resp.BaseRes.StatusCode != 0 {
		return "", fmt.Errorf("video: minimax: submit refused (%d): %s",
			resp.BaseRes.StatusCode, resp.BaseRes.StatusMsg)
	}
	if resp.TaskID == "" {
		return "", fmt.Errorf("video: minimax: submit returned no task id")
	}
	return resp.TaskID, nil
}

func (minimaxMapper) Poll(upstreamID string) (Outbound, error) {
	if upstreamID == "" {
		return Outbound{}, fmt.Errorf("video: minimax: no task id to poll")
	}
	return Outbound{
		Method: "GET", Path: "/v1/query/video_generation",
		Query: map[string]string{"task_id": upstreamID},
	}, nil
}

func (m minimaxMapper) PollResult(status int, body []byte) (Poll, error) {
	if isNotFound(status) {
		return Poll{Status: StatusFailed, NotFound: true, UpstreamStatus: "not_found"}, nil
	}
	if !okStatus(status) {
		return Poll{}, fmt.Errorf("video: minimax: poll returned %d: %.256s", status, body)
	}
	var resp minimaxResp
	if err := jsonUnmarshal(body, &resp); err != nil {
		return Poll{}, fmt.Errorf("video: minimax: decode poll response: %w", err)
	}
	if resp.BaseRes != nil && resp.BaseRes.StatusCode != 0 {
		if resp.BaseRes.StatusCode == minimaxSensitiveContent {
			return Poll{
				Status: StatusFailed, UpstreamStatus: "content_rejected",
				ErrorCode: "gateway.video_content_rejected", ErrorMessage: resp.BaseRes.StatusMsg,
			}, nil
		}
		// Anything else comes back as an error rather than as a terminal state.
		// A rate limit or an expired key is not a verdict on the job, and
		// calling it failed here would void the hold for a clip the upstream is
		// still making. The model's job ceiling is what ends a poll that never
		// resolves, and it ends it in the caller's favour.
		return Poll{}, fmt.Errorf("video: minimax: poll returned code %d: %s",
			resp.BaseRes.StatusCode, resp.BaseRes.StatusMsg)
	}
	p := Poll{UpstreamStatus: resp.Status}
	switch strings.ToLower(resp.Status) {
	case "preparing", "queueing":
		p.Status = StatusQueued
	case "processing":
		p.Status = StatusInProgress
	case "success":
		p.Status = StatusCompleted
		p.Progress = 100
		p.ContentType = "video/mp4"
		// A file id, not a URL. Artifact turns it into one.
		p.ArtifactRef = minimaxID(resp.FileID)
		if p.ArtifactRef == "" {
			// Reported success with nothing to fetch. Charging for it would
			// settle a bill for a video that does not exist.
			p.Status = StatusFailed
			p.ErrorCode = "gateway.video_content_rejected"
			p.ErrorMessage = "the upstream reported success without producing a video"
		}
	default: // "fail" and anything unrecognised
		p.Status = StatusFailed
		p.ErrorCode = "gateway.video_content_rejected"
		p.ErrorMessage = "the upstream refused this request"
	}
	return p, nil
}

func (minimaxMapper) CancelMode() CancelMode { return CancelNever }

func (minimaxMapper) Cancel(string) (Outbound, error) {
	return Outbound{}, fmt.Errorf("video: minimax: this model cannot be cancelled once submitted")
}

// Artifact is the first of two hops: the file id has to be exchanged for a URL.
// Authenticated as the upstream account, because it is a call to this vendor's
// own API rather than to a link it handed out.
func (minimaxMapper) Artifact(p Poll) (Artifact, error) {
	if p.ArtifactRef == "" {
		return Artifact{}, ErrNoArtifact
	}
	return Artifact{
		Request: Outbound{
			Method: "GET", Path: "/v1/files/retrieve",
			Query: map[string]string{"file_id": p.ArtifactRef},
		},
		NeedsUpstreamCredential: true,
		ContentType:             orDefault(p.ContentType, "video/mp4"),
		Indirect:                true,
	}, nil
}

// ResolveArtifact reads the file record and returns the fetch that actually
// yields bytes. The link is signed and short-lived (this vendor publishes nine
// hours) and carries its own authorisation -- attaching ours would send the
// upstream key to a CDN.
func (m minimaxMapper) ResolveArtifact(status int, body []byte) (Artifact, error) {
	if !okStatus(status) {
		return Artifact{}, fmt.Errorf("video: minimax: retrieving the file returned %d: %.256s", status, body)
	}
	var file minimaxFile
	if err := jsonUnmarshal(body, &file); err != nil {
		return Artifact{}, fmt.Errorf("video: minimax: decode file response: %w", err)
	}
	if file.BaseRes != nil && file.BaseRes.StatusCode != 0 {
		return Artifact{}, fmt.Errorf("video: minimax: retrieving the file refused (%d): %s",
			file.BaseRes.StatusCode, file.BaseRes.StatusMsg)
	}
	if file.File == nil || file.File.DownloadURL == "" {
		return Artifact{}, ErrNoArtifact
	}
	return Artifact{
		Request:                 Outbound{Method: "GET", URL: file.File.DownloadURL},
		NeedsUpstreamCredential: false,
		ContentType:             "video/mp4",
	}, nil
}
