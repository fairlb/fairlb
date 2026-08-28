package video

import (
	"encoding/json"
	"fmt"
	"strings"
)

func init() { registerNative(minimaxNative{}) }

// minimaxNative answers as Hailuo's video API.
//
// # The thing this vendor forces
//
// Its published schema types the file identifier as **int64**. A UUID rendered
// there breaks every generated client that parses the response into a typed
// model -- which would mean "change one base URL" was true of four vendors and
// not of this one. So a job carries an integer alias beside its id, and the two
// routes that hand an identifier out use whichever this vendor's schema says.
//
// Its two read routes also take the identifier as a **query parameter** rather
// than a path segment, which is the other reason a route says where its
// identifier lives instead of the handler assuming.
//
// The artifact route is answered here rather than by streaming bytes: on this
// vendor it returns a *file record* naming where the bytes are, and the client
// fetches that separately. So the record names this deployment's own address,
// and the second fetch lands on our content endpoint.
type minimaxNative struct{}

func (minimaxNative) Vendor() string { return "minimax" }

func (minimaxNative) Routes() []NativeRoute {
	return []NativeRoute{
		{Method: "POST", Path: "/v1/video_generation", Kind: NativeSubmit},
		{Method: "GET", Path: "/v1/query/video_generation", Kind: NativePoll,
			IDIn: NativeIDInQuery, IDName: "task_id"},
		{Method: "GET", Path: "/v1/files/retrieve", Kind: NativeArtifact,
			IDIn: NativeIDInQuery, IDName: "file_id", IDAlias: true},
	}
}

type hailuoInbound struct {
	Model           string `json:"model"`
	Prompt          string `json:"prompt"`
	FirstFrameImage string `json:"first_frame_image"`
	Duration        int    `json:"duration"`
	Resolution      string `json:"resolution"`
}

// hailuoPriced is this vendor's own spelling of every axis the job is billed
// on. None may reach the passthrough.
var hailuoPriced = []string{
	"model", "prompt", "first_frame_image", "duration", "resolution",
}

func (minimaxNative) Decode(in NativeRequest) (Request, map[string]json.RawMessage, error) {
	var doc hailuoInbound
	if err := json.Unmarshal(in.Body, &doc); err != nil {
		return Request{}, nil, fmt.Errorf("the request body is not valid JSON: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(in.Body, &raw); err != nil {
		return Request{}, nil, fmt.Errorf("the request body is not a JSON object: %w", err)
	}
	for _, k := range hailuoPriced {
		delete(raw, k)
	}
	return Request{
		Model:           doc.Model,
		Prompt:          doc.Prompt,
		Image:           doc.FirstFrameImage,
		DurationSeconds: doc.Duration,
		Resolution:      strings.ToLower(doc.Resolution),
		N:               1,
		Passthrough:     raw,
	}, raw, nil
}

// hailuoStatusOf spells our status in this vendor's vocabulary, which is
// capitalised words rather than lower-case ones.
func hailuoStatusOf(s Status) string {
	switch s {
	case StatusQueued:
		return "Queueing"
	case StatusInProgress:
		return "Processing"
	case StatusCompleted:
		return "Success"
	default:
		return "Fail"
	}
}

// hailuoOK is the success envelope every response on this API carries.
func hailuoOK() map[string]any {
	return map[string]any{"status_code": 0, "status_msg": "success"}
}

// ErrNativeNoArtifact is a file record asked for on a job that has produced
// nothing. Returned rather than rendered as a success with an empty URL: a
// client whose retry loop reaches the file route after a failed generation must
// be told there is no file, not handed one that does not exist.
var ErrNativeNoArtifact = fmt.Errorf("this job has not produced a video")

func (minimaxNative) Render(route NativeRoute, job NativeJob) (int, []byte, error) {
	var doc map[string]any
	if route.Kind == NativeArtifact {
		if job.Status != StatusCompleted || job.ContentURL == "" {
			return 0, nil, ErrNativeNoArtifact
		}
		// A file record, not bytes. The download URL is this deployment's own
		// address, so the client's next fetch lands here rather than upstream
		// (ADR-0222) -- and it is a URL either way, which is why the
		// substitution is invisible.
		doc = map[string]any{
			"file": map[string]any{
				"file_id":      job.Alias,
				"filename":     job.ID + ".mp4",
				"purpose":      "video_generation",
				"created_at":   job.CreatedAt.Unix(),
				"download_url": job.ContentURL,
			},
			"base_resp": hailuoOK(),
		}
	} else {
		doc = map[string]any{
			"task_id":   job.ID,
			"status":    hailuoStatusOf(job.Status),
			"base_resp": hailuoOK(),
		}
		if job.Status == StatusCompleted {
			// The integer alias, because this vendor's own schema types this
			// field as int64.
			doc["file_id"] = job.Alias
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return 0, nil, err
	}
	return 200, body, nil
}

// RenderError answers in this vendor's shape, which is a non-zero code inside
// base_resp. The HTTP status is still the real one, so a client reading either
// gets a correct answer.
func (minimaxNative) RenderError(_ NativeRoute, httpStatus int, code, message string) (int, []byte) {
	body, err := json.Marshal(map[string]any{
		"base_resp": map[string]any{
			"status_code": hailuoCodeOf(httpStatus), "status_msg": message,
		},
		"fairlb_error_code": code,
	})
	if err != nil {
		return httpStatus, []byte(`{"base_resp":{"status_code":1000,"status_msg":"internal error"}}`)
	}
	return httpStatus, body
}

// hailuoCodeOf maps an HTTP status onto this vendor's own numbering, which its
// clients already branch on.
func hailuoCodeOf(httpStatus int) int {
	switch httpStatus {
	case 401, 403:
		return 1004
	case 402:
		return 1008
	case 404:
		return 2013
	case 429:
		return 1002
	case 400, 422:
		return 2013
	default:
		return 1000
	}
}
