package video

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func init() { registerNative(kuaishouNative{}) }

// kuaishouNative answers as Kling's own API.
//
// The vendor's paths verbatim, under this gateway's prefix for it. Two create
// paths and their two query paths, because on this vendor the task type is part
// of the address; and the collection GET, which is the one listing endpoint any
// of these five vendors publishes.
//
// The identifier a caller gets back is this gateway's job id. Kling's own is a
// string, so nothing has to be reshaped for it to fit.
type kuaishouNative struct{}

func (kuaishouNative) Vendor() string { return "kuaishou" }

func (kuaishouNative) Routes() []NativeRoute {
	return []NativeRoute{
		{Method: "POST", Path: "/v1/videos/text2video", Kind: NativeSubmit, Variant: "text2video"},
		{Method: "POST", Path: "/v1/videos/image2video", Kind: NativeSubmit, Variant: "image2video"},
		// This vendor also publishes a collection GET that pages its tasks.
		// It is deliberately not declared: listing is account management rather
		// than generation, this gateway's own GET /v1/videos already answers
		// it, and an undeclared path here is answered with a vendor-shaped
		// refusal that says so rather than with a bare 404.
		{Method: "GET", Path: "/v1/videos/text2video/{task_id}", Kind: NativePoll,
			Variant: "text2video", IDName: "task_id"},
		{Method: "GET", Path: "/v1/videos/image2video/{task_id}", Kind: NativePoll,
			Variant: "image2video", IDName: "task_id"},
	}
}

// klingRequest is this vendor's create body.
//
// Only the fields that decide what is generated and therefore what is charged
// are named. Everything else this vendor accepts -- camera control, guidance
// scale, the callback URL -- is left in the passthrough, which is the whole
// reason a caller comes to this surface rather than to /v1/videos.
type klingRequest struct {
	ModelName      string `json:"model_name"`
	Prompt         string `json:"prompt"`
	NegativePrompt string `json:"negative_prompt"`
	Mode           string `json:"mode"`
	// Duration is a string on this API. Its own examples send "5"; a client
	// that sends 5 is common enough that both are read.
	Duration    json.RawMessage `json:"duration"`
	AspectRatio string          `json:"aspect_ratio"`
	Image       string          `json:"image"`
	ImageTail   string          `json:"image_tail"`
}

// klingPriced is every field above, by this vendor's own spelling. A key in
// here must never reach the passthrough: it decides what is generated, the
// Request is what the job is priced from, and a parameter that changed one
// without the other is the wrong-bill failure this plane is arranged to
// prevent.
var klingPriced = []string{
	"model_name", "prompt", "negative_prompt", "mode",
	"duration", "aspect_ratio", "image", "image_tail",
}

func (kuaishouNative) Decode(in NativeRequest) (Request, map[string]json.RawMessage, error) {
	var doc klingRequest
	if err := json.Unmarshal(in.Body, &doc); err != nil {
		return Request{}, nil, fmt.Errorf("the request body is not valid JSON: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(in.Body, &raw); err != nil {
		return Request{}, nil, fmt.Errorf("the request body is not a JSON object: %w", err)
	}
	for _, k := range klingPriced {
		delete(raw, k)
	}

	seconds, err := klingDuration(doc.Duration)
	if err != nil {
		return Request{}, nil, err
	}
	r := Request{
		Model:           doc.ModelName,
		Prompt:          doc.Prompt,
		NegativePrompt:  doc.NegativePrompt,
		DurationSeconds: seconds,
		// This vendor has no resolution field; `mode` is how it asks for one.
		// Read back the way the outbound mapper writes it, so a caller who sent
		// pro is priced at the tier pro produces.
		Resolution:  klingResolutionOf(doc.Mode),
		AspectRatio: doc.AspectRatio,
		Image:       doc.Image,
		LastFrame:   doc.ImageTail,
		N:           1,
		Passthrough: raw,
	}
	if in.Route.Variant == "image2video" && r.Image == "" {
		return Request{}, nil, fmt.Errorf("image-to-video requires an image")
	}
	return r, raw, nil
}

// klingDuration reads a duration this API documents as a string and clients
// send both ways.
func klingDuration(raw json.RawMessage) (int, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		n, err := strconv.Atoi(strings.TrimSpace(asString))
		if err != nil {
			return 0, fmt.Errorf("duration %q is not a number of seconds", asString)
		}
		return n, nil
	}
	var asNumber int
	if err := json.Unmarshal(raw, &asNumber); err != nil {
		return 0, fmt.Errorf("duration must be a number of seconds")
	}
	return asNumber, nil
}

// klingResolutionOf inverts modeFor. An unset mode means this vendor's default,
// which is the cheaper tier -- and defaulting the other way would price a
// caller who said nothing at the dearer one.
func klingResolutionOf(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "pro") {
		return "1080p"
	}
	return "720p"
}

// klingStatusOf spells our status in this vendor's vocabulary. Its own words,
// not ours: a client switching over matches on these strings.
func klingStatusOf(s Status) string {
	switch s {
	case StatusQueued:
		return "submitted"
	case StatusInProgress:
		return "processing"
	case StatusCompleted:
		return "succeed"
	default:
		return "failed"
	}
}

type klingResult struct {
	Videos []klingVideo `json:"videos"`
}

type klingVideo struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Duration string `json:"duration"`
}

func (kuaishouNative) Render(route NativeRoute, job NativeJob) (int, []byte, error) {
	data := map[string]any{
		"task_id":         job.ID,
		"task_status":     klingStatusOf(job.Status),
		"task_status_msg": job.ErrorMessage,
		"created_at":      job.CreatedAt.UnixMilli(),
		"updated_at":      job.UpdatedAt.UnixMilli(),
	}
	if job.Status == StatusCompleted && job.ContentURL != "" {
		// The address is this deployment's own. The upstream's never leaves
		// here (ADR-0222), and a client cannot tell the difference: it was
		// always an opaque URL to fetch.
		data["task_result"] = klingResult{Videos: []klingVideo{{
			ID: job.ID, URL: job.ContentURL,
			Duration: strconv.Itoa(job.DurationSeconds),
		}}}
	}
	body, err := json.Marshal(map[string]any{
		"code": 0, "message": "SUCCEED", "request_id": job.ID, "data": data,
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, body, nil
}

// RenderError answers in this vendor's shape, which carries the refusal inside
// a 200-shaped envelope with a non-zero code. The HTTP status is still set to
// the real one: a client that reads only the status gets a correct answer, and
// one that reads only the envelope gets a correct answer too.
func (kuaishouNative) RenderError(_ NativeRoute, httpStatus int, code, message string) (int, []byte) {
	body, err := json.Marshal(map[string]any{
		"code": klingCodeOf(httpStatus), "message": message, "request_id": "",
		"data": map[string]any{"fairlb_error_code": code},
	})
	if err != nil {
		return httpStatus, []byte(`{"code":5000,"message":"internal error"}`)
	}
	return httpStatus, body
}

// klingCodeOf maps an HTTP status onto this vendor's own error numbering, so a
// client's existing handling of them keeps working. Its published families are
// 1000s for authentication, 1100s for account state and 1200s for the request.
func klingCodeOf(httpStatus int) int {
	switch {
	case httpStatus == 401 || httpStatus == 403:
		return 1000
	case httpStatus == 402:
		return 1101
	case httpStatus == 429:
		return 1302
	case httpStatus >= 400 && httpStatus < 500:
		return 1200
	default:
		return 5000
	}
}
