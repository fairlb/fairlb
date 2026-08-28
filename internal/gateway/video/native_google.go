package video

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

func init() { registerNative(googleNative{}) }

// googleNative answers as the Gemini API's long-running video operations.
//
// # The two things this vendor does differently on the way in
//
//   - **the model is in the path, with the action after a colon**:
//     `models/veo-3.1-generate-preview:predictLongRunning`. chi matches
//     segments, so the route is a wildcard and the tail is split here.
//   - **the job identifier is itself a path.** What comes back is an operation
//     *name*, `models/<model>/operations/<id>`, and the client GETs that name.
//     So the read route is a wildcard too and the id is its last segment --
//     which is why NativeRoute can say where an identifier lives rather than
//     assuming a path parameter.
//
// Its images are bytes or a GCS URI rather than a URL, which round-trips
// exactly: read into the data URL this contract uses, and written back out as
// bytes by the outbound mapper.
type googleNative struct{}

func (googleNative) Vendor() string { return "google" }

func (googleNative) Routes() []NativeRoute {
	return []NativeRoute{
		{Method: "POST", Path: "/v1beta/models/*", Kind: NativeSubmit},
		{Method: "GET", Path: "/v1beta/models/*", Kind: NativePoll,
			IDIn: NativeIDInPathTail, IDName: "*"},
	}
}

type veoInboundImage struct {
	BytesBase64 string `json:"bytesBase64Encoded"`
	GCSURI      string `json:"gcsUri"`
	MIMEType    string `json:"mimeType"`
}

type veoInboundInstance struct {
	Prompt    string           `json:"prompt"`
	Image     *veoInboundImage `json:"image"`
	LastFrame *veoInboundImage `json:"lastFrame"`
	Reference []struct {
		Image         veoInboundImage `json:"image"`
		ReferenceType string          `json:"referenceType"`
	} `json:"referenceImages"`
}

// veoInstanceFields is every key an instance may carry. An instance is a fixed
// shape on this vendor -- `parameters` is where its knobs go -- so a key that
// is not one of these is refused rather than forwarded or dropped.
var veoInstanceFields = []string{"prompt", "image", "lastFrame", "referenceImages"}

type veoInbound struct {
	Instances  []veoInboundInstance `json:"instances"`
	Parameters map[string]any       `json:"parameters"`
}

// veoPricedParameters is this vendor's own spelling of every axis the job is
// billed on. None may reach the passthrough.
var veoPricedParameters = []string{
	"aspectRatio", "resolution", "durationSeconds", "negativePrompt", "numberOfVideos", "seed",
}

func (googleNative) Decode(in NativeRequest) (Request, map[string]json.RawMessage, error) {
	// "veo-3.1-generate-preview:predictLongRunning" -- the action after the
	// colon is this vendor's own convention and is not part of the model name.
	model, action, found := strings.Cut(in.Path["*"], ":")
	if !found || action != "predictLongRunning" {
		return Request{}, nil, ErrNativeRouteUnsupported{Path: "models/" + in.Path["*"]}
	}

	var doc veoInbound
	if err := json.Unmarshal(in.Body, &doc); err != nil {
		return Request{}, nil, fmt.Errorf("the request body is not valid JSON: %w", err)
	}
	if len(doc.Instances) == 0 {
		return Request{}, nil, fmt.Errorf("the request carries no instance to generate from")
	}
	if len(doc.Instances) > 1 {
		// This gateway prices one generation per job. Silently dropping the
		// rest would produce one clip for a request that asked for several,
		// billed as one -- the wrong direction, but still not what was asked.
		return Request{}, nil, fmt.Errorf(
			"this gateway takes one instance per request; send them as separate calls")
	}
	inst := doc.Instances[0]

	// Body-shaped, not parameters-shaped. Built from one sub-object, every
	// unrecognised field outside `parameters` was dropped without a word.
	passthrough, err := veoPassthrough(in.Body)
	if err != nil {
		return Request{}, nil, err
	}

	r := Request{
		Model:       model,
		Prompt:      inst.Prompt,
		N:           1,
		Passthrough: passthrough,
	}
	if s, ok := doc.Parameters["negativePrompt"].(string); ok {
		r.NegativePrompt = s
	}
	if s, ok := doc.Parameters["aspectRatio"].(string); ok {
		r.AspectRatio = s
	}
	if s, ok := doc.Parameters["resolution"].(string); ok {
		r.Resolution = strings.ToLower(s)
	}
	if n := veoNumber(doc.Parameters["durationSeconds"]); n != 0 {
		r.DurationSeconds = n
	}
	if n := veoNumber(doc.Parameters["numberOfVideos"]); n > 0 {
		r.N = n
	}
	if n := veoNumber(doc.Parameters["seed"]); n != 0 {
		seed := int64(n)
		r.Seed = &seed
	}
	r.Image = veoInboundRef(inst.Image)
	r.LastFrame = veoInboundRef(inst.LastFrame)
	for _, ref := range inst.Reference {
		kind := ReferenceAsset
		if strings.EqualFold(ref.ReferenceType, "style") {
			kind = ReferenceStyle
		}
		if url := veoInboundRef(&ref.Image); url != "" {
			r.ReferenceImages = append(r.ReferenceImages, ReferenceImage{URL: url, Type: kind})
		}
	}
	return r, passthrough, nil
}

// veoPassthrough is every field of the request this gateway did not read,
// keeping the body's own shape so the mapper can put each one back where the
// caller wrote it.
//
// An unrecognised key *inside an instance* is refused rather than carried.
// `parameters` is this vendor's extension point and grows; an instance is a
// fixed shape, so a key there is a typo or a field this build has not learned
// -- and either way "I do not know what you meant" is a better answer than a
// clip generated without it.
func veoPassthrough(body []byte) (map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("the request body is not a JSON object: %w", err)
	}
	var instances []map[string]json.RawMessage
	if raw, present := top["instances"]; present {
		if err := json.Unmarshal(raw, &instances); err != nil {
			return nil, fmt.Errorf("instances must be a list of objects: %w", err)
		}
	}
	for _, inst := range instances {
		for k := range inst {
			if !slices.Contains(veoInstanceFields, k) {
				return nil, fmt.Errorf(
					"this gateway does not understand the instance field %q; "+
						"a generation setting belongs in parameters", k)
			}
		}
	}
	delete(top, "instances")

	if raw, present := top["parameters"]; present {
		var params map[string]json.RawMessage
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, fmt.Errorf("parameters must be an object: %w", err)
		}
		for _, k := range veoPricedParameters {
			delete(params, k)
		}
		if len(params) == 0 {
			delete(top, "parameters")
		} else {
			encoded, err := json.Marshal(params)
			if err != nil {
				return nil, fmt.Errorf("re-encode parameters: %w", err)
			}
			top["parameters"] = encoded
		}
	}
	return top, nil
}

// veoNumber reads a value this API documents as a string enum in one place and
// a number in another. Both are accepted rather than picking one and refusing
// requests its own examples produce.
func veoNumber(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// veoInboundRef turns this vendor's image shape into the reference this
// contract carries, which the outbound mapper turns straight back. Bytes become
// the data URL it already knows how to read, and a GCS URI passes through as
// itself.
func veoInboundRef(img *veoInboundImage) string {
	switch {
	case img == nil:
		return ""
	case img.GCSURI != "":
		return img.GCSURI
	case img.BytesBase64 != "":
		mime := img.MIMEType
		if mime == "" {
			mime = "image/png"
		}
		return "data:" + mime + ";base64," + img.BytesBase64
	default:
		return ""
	}
}

// veoOperationName is the identifier this vendor's clients read and then GET.
//
// The model segment is the name half of the catalog slug, not the slug itself.
// A slug always carries a creator prefix and a slash, and a slash there would
// give the name five segments where this vendor's own has four -- so a client
// that splits it to recover the model, or validates it against the documented
// shape, would break. Polling still finds the job either way: the read route
// matches on the last segment.
func veoOperationName(job NativeJob) string {
	model := job.Model
	if _, name, found := strings.Cut(model, "/"); found {
		model = name
	}
	if model == "" {
		model = "video"
	}
	return "models/" + model + "/operations/" + job.ID
}

func (googleNative) Render(_ NativeRoute, job NativeJob) (int, []byte, error) {
	doc := map[string]any{"name": veoOperationName(job)}
	switch {
	case !job.Status.Terminal():
		doc["done"] = false
	case job.Status == StatusCompleted && job.ContentURL != "":
		doc["done"] = true
		doc["response"] = map[string]any{
			"@type": "type.googleapis.com/google.ai.generativelanguage.v1beta.PredictLongRunningResponse",
			"generateVideoResponse": map[string]any{
				"generatedSamples": []any{map[string]any{
					"video": map[string]any{"uri": job.ContentURL, "mimeType": "video/mp4"},
				}},
			},
		}
	default:
		doc["done"] = true
		// This API's terminal failure is a google.rpc.Status. Code 3 is
		// INVALID_ARGUMENT, which is what a content refusal arrives as; a
		// client's error handling reads the number.
		doc["error"] = map[string]any{
			"code": 3, "status": "INVALID_ARGUMENT",
			"message": orDefault(job.ErrorMessage, "the upstream produced no video"),
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return 0, nil, err
	}
	return 200, body, nil
}

// RenderError answers in this vendor's error shape: a google.rpc.Status under
// `error`, which is what its clients parse on a non-2xx.
func (googleNative) RenderError(_ NativeRoute, httpStatus int, code, message string) (int, []byte) {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"code": httpStatus, "message": message, "status": veoStatusName(httpStatus),
			"details": []any{map[string]any{"reason": code}},
		},
	})
	if err != nil {
		return httpStatus, []byte(`{"error":{"code":500,"message":"internal error","status":"INTERNAL"}}`)
	}
	return httpStatus, body
}

func veoStatusName(httpStatus int) string {
	switch httpStatus {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 402, 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 503:
		return "UNAVAILABLE"
	default:
		return "INTERNAL"
	}
}
