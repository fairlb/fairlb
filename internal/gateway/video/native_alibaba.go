package video

import (
	"encoding/json"
	"fmt"
	"strings"
)

func init() { registerNative(alibabaNative{}) }

// alibabaNative answers as Model Studio's asynchronous task API.
//
// Its inbound shape splits what the others keep flat: content in `input`,
// generation settings in `parameters`. The typed `media` list in `input` is the
// closest thing to this contract's three image roles that any of these vendors
// publishes, so it round-trips exactly.
//
// The `X-DashScope-Async: enable` header its clients send is accepted and
// ignored. Every job here is asynchronous, so there is nothing for it to
// switch, and refusing a header a client must send would break the one thing
// this surface exists to preserve.
type alibabaNative struct{}

func (alibabaNative) Vendor() string { return "alibaba" }

func (alibabaNative) Routes() []NativeRoute {
	return []NativeRoute{
		{Method: "POST", Path: "/api/v1/services/aigc/video-generation/video-synthesis",
			Kind: NativeSubmit},
		{Method: "GET", Path: "/api/v1/tasks/{task_id}", Kind: NativePoll, IDName: "task_id"},
	}
}

type wanInbound struct {
	Model string `json:"model"`
	Input struct {
		Prompt         string `json:"prompt"`
		NegativePrompt string `json:"negative_prompt"`
		Media          []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"media"`
		// The older single-image form, still what a good deal of client code
		// sends. Read as the first frame, which is what it means.
		ImgURL string `json:"img_url"`
	} `json:"input"`
	Parameters map[string]any `json:"parameters"`
}

// wanPricedParameters is this vendor's own spelling of every axis the job is
// billed on, inside `parameters`. None may reach the passthrough.
var wanPricedParameters = []string{"resolution", "ratio", "duration", "seed"}

// wanPricedInput is the same list for `input`, whose contents decide what is
// generated.
var wanPricedInput = []string{"prompt", "negative_prompt", "media", "img_url"}

func (alibabaNative) Decode(in NativeRequest) (Request, map[string]json.RawMessage, error) {
	var doc wanInbound
	if err := json.Unmarshal(in.Body, &doc); err != nil {
		return Request{}, nil, fmt.Errorf("the request body is not valid JSON: %w", err)
	}

	// Body-shaped, not parameters-shaped. Building it from one sub-object was
	// how `input.audio_url` and every other unrecognised field outside
	// `parameters` came to be dropped without a word: the mapper writes a fresh
	// `input`, so anything not carried here is simply gone.
	passthrough, err := wanPassthrough(in.Body)
	if err != nil {
		return Request{}, nil, err
	}

	r := Request{
		Model:          doc.Model,
		Prompt:         doc.Input.Prompt,
		NegativePrompt: doc.Input.NegativePrompt,
		Image:          doc.Input.ImgURL,
		N:              1,
		Passthrough:    passthrough,
	}
	for _, m := range doc.Input.Media {
		switch m.Type {
		case "first_frame":
			r.Image = m.URL
		case "last_frame":
			r.LastFrame = m.URL
		}
	}
	if s, ok := doc.Parameters["resolution"].(string); ok {
		r.Resolution = strings.ToLower(s)
	}
	if s, ok := doc.Parameters["ratio"].(string); ok {
		r.AspectRatio = s
	}
	if n := veoNumber(doc.Parameters["duration"]); n != 0 {
		r.DurationSeconds = n
	}
	if n := veoNumber(doc.Parameters["seed"]); n != 0 {
		seed := int64(n)
		r.Seed = &seed
	}
	return r, passthrough, nil
}

// wanPassthrough is every field of the request this gateway did not read,
// keeping the body's own shape so the mapper can put each one back where the
// caller wrote it.
func wanPassthrough(body []byte) (map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("the request body is not a JSON object: %w", err)
	}
	delete(top, "model")
	for key, priced := range map[string][]string{
		"input": wanPricedInput, "parameters": wanPricedParameters,
	} {
		raw, present := top[key]
		if !present {
			continue
		}
		var sub map[string]json.RawMessage
		if err := json.Unmarshal(raw, &sub); err != nil {
			continue
		}
		for _, k := range priced {
			delete(sub, k)
		}
		if len(sub) == 0 {
			delete(top, key)
			continue
		}
		encoded, err := json.Marshal(sub)
		if err != nil {
			return nil, fmt.Errorf("re-encode %s: %w", key, err)
		}
		top[key] = encoded
	}
	return top, nil
}

// wanStatusOf spells our status in this vendor's vocabulary, which is upper
// case.
func wanStatusOf(s Status) string {
	switch s {
	case StatusQueued:
		return "PENDING"
	case StatusInProgress:
		return "RUNNING"
	case StatusCompleted:
		return "SUCCEEDED"
	case StatusCanceled:
		return "CANCELED"
	default:
		return "FAILED"
	}
}

func (alibabaNative) Render(_ NativeRoute, job NativeJob) (int, []byte, error) {
	output := map[string]any{
		"task_id":     job.ID,
		"task_status": wanStatusOf(job.Status),
	}
	if job.Status == StatusCompleted && job.ContentURL != "" {
		output["video_url"] = job.ContentURL
	}
	if job.ErrorMessage != "" {
		output["code"] = orDefault(job.ErrorCode, "InvalidParameter")
		output["message"] = job.ErrorMessage
	}
	body, err := json.Marshal(map[string]any{
		"output": output, "request_id": job.ID,
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, body, nil
}

// RenderError answers in this vendor's shape, which puts a string code and a
// message at the top level beside the request id.
func (alibabaNative) RenderError(_ NativeRoute, httpStatus int, code, message string) (int, []byte) {
	body, err := json.Marshal(map[string]any{
		"code": wanCodeOf(httpStatus), "message": message,
		"request_id": "", "fairlb_error_code": code,
	})
	if err != nil {
		return httpStatus, []byte(`{"code":"InternalError","message":"internal error"}`)
	}
	return httpStatus, body
}

// wanCodeOf maps an HTTP status onto this vendor's own string codes, which its
// clients branch on.
func wanCodeOf(httpStatus int) string {
	switch httpStatus {
	case 401:
		return "InvalidApiKey"
	case 402, 403:
		return "Forbidden.Unpurchased"
	case 404:
		return "InvalidParameter"
	case 429:
		return "Throttling.RateQuota"
	case 400, 422:
		return "InvalidParameter"
	default:
		return "InternalError"
	}
}
