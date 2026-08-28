package video

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func init() { registerNative(volcengineNative{}) }

// volcengineNative answers as Ark's content-generation task API.
//
// The awkward part of this vendor is the same one its outbound mapper has to
// deal with, seen from the other side: **generation parameters ride inside the
// prompt text** as `--key value` commands. So reading a request here means
// parsing them back out of the text, and every one that decides a billed
// quantity has to be removed from the prompt before it is priced -- otherwise a
// caller could write `--duration 12`, be charged for the duration this gateway
// parsed, and receive whatever the upstream made of the text.
//
// The commands this surface understands are stripped and read; anything else
// stays in the text, because it is this vendor's own vocabulary and forwarding
// it verbatim is what this surface is for.
type volcengineNative struct{}

func (volcengineNative) Vendor() string { return "volcengine" }

func (volcengineNative) Routes() []NativeRoute {
	return []NativeRoute{
		{Method: "POST", Path: "/api/v3/contents/generations/tasks", Kind: NativeSubmit},
		{Method: "GET", Path: "/api/v3/contents/generations/tasks/{task_id}", Kind: NativePoll,
			IDName: "task_id"},
	}
}

type arkRequest struct {
	Model   string `json:"model"`
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Role     string `json:"role"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	} `json:"content"`
	// The scalar forms this API accepts beside the inline commands. Both are
	// read, and the scalar wins when a request carries the same axis twice --
	// stated here rather than left to whichever the parser saw last.
	Resolution string `json:"resolution"`
	Ratio      string `json:"ratio"`
	Duration   *int   `json:"duration"`
	Seed       *int64 `json:"seed"`
}

// arkPriced is every top-level key above. None of them may reach the
// passthrough: each decides what is generated, and the Request is what the job
// is priced from.
var arkPriced = []string{"model", "content", "resolution", "ratio", "duration", "seed"}

// arkCommand matches this vendor's inline parameter syntax.
var arkCommand = regexp.MustCompile(`--([a-z_]+)\s+(\S+)`)

func (volcengineNative) Decode(in NativeRequest) (Request, map[string]json.RawMessage, error) {
	var doc arkRequest
	if err := json.Unmarshal(in.Body, &doc); err != nil {
		return Request{}, nil, fmt.Errorf("the request body is not valid JSON: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(in.Body, &raw); err != nil {
		return Request{}, nil, fmt.Errorf("the request body is not a JSON object: %w", err)
	}
	for _, k := range arkPriced {
		delete(raw, k)
	}

	r := Request{Model: doc.Model, N: 1, Passthrough: raw}
	for _, item := range doc.Content {
		switch item.Type {
		case "text":
			prompt, commands := splitArkCommands(item.Text)
			r.Prompt = prompt
			applyArkCommands(&r, commands)
		case "image_url":
			if item.ImageURL == nil {
				continue
			}
			// This vendor names the roles the same way the normalised contract
			// does, which is the one place the two agree exactly -- so each is
			// mapped by name and an unfamiliar one is refused.
			//
			// Refused rather than defaulted to the first frame. A reference
			// image steers the subject without being a frame of the output;
			// animating it instead produces a materially different clip, and
			// the caller is billed for it either way.
			switch item.Role {
			case "first_frame", "":
				r.Image = item.ImageURL.URL
			case "last_frame":
				r.LastFrame = item.ImageURL.URL
			case "reference_image":
				r.ReferenceImages = append(r.ReferenceImages,
					ReferenceImage{URL: item.ImageURL.URL, Type: ReferenceAsset})
			default:
				return Request{}, nil, fmt.Errorf(
					"this gateway does not understand the image role %q", item.Role)
			}
		}
	}
	// The scalar fields win, because they are the unambiguous way to say each
	// of these and a request carrying both is a request that already disagrees
	// with itself.
	if doc.Resolution != "" {
		r.Resolution = strings.ToLower(doc.Resolution)
	}
	if doc.Ratio != "" {
		r.AspectRatio = doc.Ratio
	}
	if doc.Duration != nil {
		r.DurationSeconds = *doc.Duration
	}
	if doc.Seed != nil {
		r.Seed = doc.Seed
	}
	return r, raw, nil
}

// arkReadCommands is the set this gateway understands and prices. It is also
// the set that may be removed from the prompt.
var arkReadCommands = []string{"resolution", "ratio", "duration", "seed", "audio"}

// splitArkCommands separates the inline parameters this gateway reads from the
// prompt they are embedded in.
//
// **Only the commands that are read are removed.** Leaving a priced one in
// would send the upstream a second copy beside the one the outbound mapper
// writes from the priced Request, and where the two disagreed the upstream
// would obey the text while the bill followed the Request. But removing one
// this build does not read would delete it outright: it is not in the Request,
// so nothing puts it back, and this vendor's own knobs -- `--camerafixed`,
// `--watermark` -- travel exactly this way. Dropping a caller's parameter
// silently is the failure this whole plane refuses.
func splitArkCommands(text string) (prompt string, commands map[string]string) {
	commands = map[string]string{}
	prompt = arkCommand.ReplaceAllStringFunc(text, func(match string) string {
		m := arkCommand.FindStringSubmatch(match)
		if !slices.Contains(arkReadCommands, m[1]) {
			return match
		}
		commands[m[1]] = m[2]
		return ""
	})
	return strings.Join(strings.Fields(prompt), " "), commands
}

func applyArkCommands(r *Request, commands map[string]string) {
	// Every key here is one splitArkCommands chose to remove from the prompt,
	// so the two lists have to stay in step: a command removed and not applied
	// is a command deleted.
	for key, value := range commands {
		switch key {
		case "resolution":
			r.Resolution = strings.ToLower(value)
		case "ratio":
			r.AspectRatio = value
		case "duration":
			if n, err := strconv.Atoi(value); err == nil {
				r.DurationSeconds = n
			}
		case "seed":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				r.Seed = &n
			}
		case "audio":
			on := value == "true"
			r.Audio = &on
		}
	}
}

// arkStatusOf spells our status in this vendor's vocabulary.
func arkStatusOf(s Status) string {
	switch s {
	case StatusQueued:
		return "queued"
	case StatusInProgress:
		return "running"
	case StatusCompleted:
		return "succeeded"
	case StatusCanceled:
		return "cancelled"
	default:
		return "failed"
	}
}

func (volcengineNative) Render(_ NativeRoute, job NativeJob) (int, []byte, error) {
	doc := map[string]any{
		"id":         job.ID,
		"model":      job.Model,
		"status":     arkStatusOf(job.Status),
		"created_at": job.CreatedAt.Unix(),
		"updated_at": job.UpdatedAt.Unix(),
	}
	if job.Status == StatusCompleted && job.ContentURL != "" {
		doc["content"] = map[string]any{"video_url": job.ContentURL}
	}
	if job.ErrorMessage != "" {
		doc["error"] = map[string]any{"code": job.ErrorCode, "message": job.ErrorMessage}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return 0, nil, err
	}
	return 200, body, nil
}

// RenderError answers in this vendor's shape, which is an object under `error`
// with a string code -- the same shape its own refusals take.
func (volcengineNative) RenderError(_ NativeRoute, httpStatus int, code, message string) (int, []byte) {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return httpStatus, []byte(`{"error":{"code":"InternalServiceError","message":"internal error"}}`)
	}
	return httpStatus, body
}
