// Package proxy carries the dataplane pipeline: authentication, the policy
// gates, the injection layer and forwarding upstream.
package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/fairlb/fairlb/foundation/errcode"
)

// The dataplane does not answer with problem+json, which the management APIs
// use. An OpenAI surface answers with OpenAI's error structure and an Anthropic
// surface with Anthropic's, and `code` is a stable machine-readable code shared
// by both.

// Surface decides how an error is rendered.
type Surface string

const (
	SurfaceOpenAI    Surface = "openai"
	SurfaceAnthropic Surface = "anthropic"
	SurfaceGemini    Surface = "gemini"
)

// CodeStreamInterrupted is absent from the error-code registry: once the first
// byte has gone out the HTTP status is already 200, so this can only be
// conveyed as an SSE event inside the stream and has no status of its own.
const CodeStreamInterrupted = "gateway.stream_interrupted"

// errShape is how one code presents on each dialect's surface. The HTTP status
// comes from statusOf.
type errShape struct {
	openAIType    string
	anthropicType string
	// geminiStatus is the canonical error status this protocol names its
	// failures by. It is not derivable from the HTTP status: 400 covers both an
	// argument this API rejects and a request it refuses for another reason, and
	// clients switch on this string.
	geminiStatus string
}

// shapes is the executable form of the error catalogue. The HTTP status comes
// from the error-code registry, which is the single source of truth; only the
// per-dialect type differences are recorded here.
var shapes = map[string]errShape{
	errcode.GatewayInvalidApiKey:       {openAIType: "authentication_error", anthropicType: "authentication_error", geminiStatus: "UNAUTHENTICATED"},
	errcode.GatewayKeyRevoked:          {openAIType: "authentication_error", anthropicType: "authentication_error", geminiStatus: "UNAUTHENTICATED"},
	errcode.GatewayKeyExpired:          {openAIType: "authentication_error", anthropicType: "authentication_error", geminiStatus: "UNAUTHENTICATED"},
	errcode.GatewayOrgSuspended:        {openAIType: "permission_error", anthropicType: "permission_error", geminiStatus: "PERMISSION_DENIED"},
	errcode.GatewayInsufficientScope:   {openAIType: "permission_error", anthropicType: "permission_error", geminiStatus: "PERMISSION_DENIED"},
	errcode.GatewayInsufficientCredits: {openAIType: "insufficient_quota", anthropicType: "invalid_request_error", geminiStatus: "RESOURCE_EXHAUSTED"},
	errcode.GatewayKeyBudgetExceeded:   {openAIType: "insufficient_quota", anthropicType: "invalid_request_error", geminiStatus: "RESOURCE_EXHAUSTED"},
	errcode.GatewayRateLimited:         {openAIType: "rate_limit_error", anthropicType: "rate_limit_error", geminiStatus: "RESOURCE_EXHAUSTED"},
	errcode.GatewaySaturated:           {openAIType: "rate_limit_error", anthropicType: "overloaded_error", geminiStatus: "RESOURCE_EXHAUSTED"},
	errcode.GatewayModelNotFound:       {openAIType: "invalid_request_error", anthropicType: "not_found_error", geminiStatus: "NOT_FOUND"},
	errcode.GatewayResourceNotFound:    {openAIType: "invalid_request_error", anthropicType: "not_found_error", geminiStatus: "NOT_FOUND"},
	// A disabled admission tier belongs with a suspended org under
	// permission_error rather than under not_found: the first is an
	// account-level state blocking service, the second says "this model does
	// not exist for you". Filing it wrongly misleads the organization into thinking a
	// different model name would get around it.
	errcode.GatewayModelTierDisabled: {openAIType: "permission_error", anthropicType: "permission_error", geminiStatus: "PERMISSION_DENIED"},
	// A content block files under "invalid request" on both dialects; the
	// caller distinguishes the actual reason from the code.
	errcode.GatewayModelDisabled: {openAIType: "api_error", anthropicType: "overloaded_error", geminiStatus: "UNAVAILABLE"},
	// Unpriced means incomplete configuration and answers 503. It is kept apart
	// from model_disabled, which is a deliberate operator action: the first
	// tells the operator to go and add a price, the second needs nothing done.
	errcode.GatewayModelUnpriced:         {openAIType: "api_error", anthropicType: "api_error", geminiStatus: "FAILED_PRECONDITION"},
	errcode.GatewayRequestTooLarge:       {openAIType: "invalid_request_error", anthropicType: "invalid_request_error", geminiStatus: "INVALID_ARGUMENT"},
	errcode.GatewayInvalidRequest:        {openAIType: "invalid_request_error", anthropicType: "invalid_request_error", geminiStatus: "INVALID_ARGUMENT"},
	errcode.GatewayUpstreamTimeout:       {openAIType: "api_error", anthropicType: "api_error", geminiStatus: "DEADLINE_EXCEEDED"},
	errcode.GatewayAllProvidersFailed:    {openAIType: "api_error", anthropicType: "api_error", geminiStatus: "UNAVAILABLE"},
	errcode.GatewayStateRouteUnavailable: {openAIType: "api_error", anthropicType: "api_error", geminiStatus: "UNAVAILABLE"},
	errcode.GatewayInternal:              {openAIType: "api_error", anthropicType: "api_error", geminiStatus: "INTERNAL"},
}

// Error is the dataplane's error carrier: the code decides the status and each
// dialect's type, and the message is what the caller reads.
type Error struct {
	Code    string
	Message string
	// RetryAfter above zero writes a Retry-After header. A 429 always carries
	// one.
	RetryAfter time.Duration
	// UpstreamMessage preserves the upstream's own text; a developer needs those
	// exact words when a semantic 4xx is passed through.
	UpstreamMessage string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// NewError builds a dataplane error.
func NewError(code, message string) *Error { return &Error{Code: code, Message: message} }

// statusOf returns the HTTP status for a code. Statuses are maintained in one
// place, the error-code registry, and are not copied here -- two copies would
// drift apart.
func statusOf(code string) int {
	if d, ok := errcode.Lookup(code); ok {
		return d.Status
	}
	return http.StatusInternalServerError
}

// Write renders the error response for a surface.
//
// The gateway's own 5xx never exposes internal detail: the message is generic
// and X-Request-Id is what an investigation goes on. An upstream 4xx's own text,
// by contrast, *must* be passed through -- that is what a developer uses to
// locate the bad parameter.
func Write(w http.ResponseWriter, surface Surface, e *Error) {
	shape, known := shapes[e.Code]
	if !known {
		e = NewError(errcode.GatewayInternal, "Internal server error")
		shape = shapes[errcode.GatewayInternal]
	}
	status := statusOf(e.Code)

	msg := e.Message
	if e.UpstreamMessage != "" {
		msg = e.UpstreamMessage
	}
	if status >= 500 && e.UpstreamMessage == "" {
		msg = genericServerMessage
	}

	h := w.Header()
	h.Set("Content-Type", "application/json")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(e.RetryAfter.Round(time.Second).Seconds())))
	}
	w.WriteHeader(status)

	var body any
	switch surface {
	case SurfaceAnthropic:
		body = anthropicError{Type: "error", Error: anthropicErrorBody{
			Type: shape.anthropicType, Message: msg, Code: e.Code,
		}}
	case SurfaceGemini:
		status := shape.geminiStatus
		if status == "" {
			status = "UNKNOWN"
		}
		// The gateway's own code travels in the details, where this API puts
		// machine-readable specifics: dropping it would leave a caller with only
		// a status shared by every refusal of that kind, and support with
		// nothing to search on.
		body = geminiError{Error: geminiErrorBody{
			Code: statusOf(e.Code), Message: msg, Status: status,
			Details: []geminiErrorDetail{{
				Type: "type.googleapis.com/google.rpc.ErrorInfo", Reason: e.Code, Domain: errorDomain,
			}},
		}}
	default:
		body = openAIError{Error: openAIErrorBody{
			Message: msg, Type: shape.openAIType, Code: e.Code,
		}}
	}
	// HTML escaping is off: this is a pass-through proxy, so < > and & in the
	// upstream's own text should appear as they were. The default escaping
	// would render "max_tokens must be <= 4096" with an escape sequence --
	// lossless in meaning but a different shape from the upstream, and a
	// developer comparing the two would think the gateway had altered the
	// content.
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		slog.Error("dataplane: writing the error response failed", "code", e.Code, "error", err)
	}
}

// genericServerMessage is the outward text for a 5xx, which reveals no internal
// detail.
const genericServerMessage = "The upstream or gateway is temporarily unavailable; please retry"

type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Code    string  `json:"code"`
	Param   *string `json:"param"`
}

type anthropicError struct {
	Type  string             `json:"type"`
	Error anthropicErrorBody `json:"error"`
}

type anthropicErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code"`
}

// errorDomain names this gateway in a Gemini-shaped error's details, which is
// what that field is for: the reason string above is ours, not Google's, and a
// reader has to be able to tell whose vocabulary it is in.
const errorDomain = "fairlb"

type geminiError struct {
	Error geminiErrorBody `json:"error"`
}

type geminiErrorBody struct {
	Code    int                 `json:"code"`
	Message string              `json:"message"`
	Status  string              `json:"status"`
	Details []geminiErrorDetail `json:"details,omitempty"`
}

type geminiErrorDetail struct {
	Type   string `json:"@type"`
	Reason string `json:"reason"`
	Domain string `json:"domain"`
}
