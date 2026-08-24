package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// The executable contract of the error catalogue: exactly where each code lands
// on both dialects' surfaces, in HTTP status and in error.type. Table driven,
// and the table is the machine-readable copy of that catalogue.
func TestErrorShapesBothSurfaces(t *testing.T) {
	cases := []struct {
		code          string
		status        int
		openAIType    string
		anthropicType string
	}{
		{errcode.GatewayInvalidApiKey, 401, "authentication_error", "authentication_error"},
		{errcode.GatewayKeyRevoked, 401, "authentication_error", "authentication_error"},
		{errcode.GatewayKeyExpired, 401, "authentication_error", "authentication_error"},
		{errcode.GatewayOrgSuspended, 403, "permission_error", "permission_error"},
		{errcode.GatewayInsufficientScope, 403, "permission_error", "permission_error"},
		{errcode.GatewayInsufficientCredits, 402, "insufficient_quota", "invalid_request_error"},
		{errcode.GatewayKeyBudgetExceeded, 402, "insufficient_quota", "invalid_request_error"},
		{errcode.GatewayRateLimited, 429, "rate_limit_error", "rate_limit_error"},
		{errcode.GatewaySaturated, 429, "rate_limit_error", "overloaded_error"},
		{errcode.GatewayModelNotFound, 404, "invalid_request_error", "not_found_error"},
		{errcode.GatewayModelDisabled, 503, "api_error", "overloaded_error"},
		{errcode.GatewayRequestTooLarge, 413, "invalid_request_error", "invalid_request_error"},
		{errcode.GatewayInvalidRequest, 400, "invalid_request_error", "invalid_request_error"},
		{errcode.GatewayUpstreamTimeout, 502, "api_error", "api_error"},
		{errcode.GatewayAllProvidersFailed, 502, "api_error", "api_error"},
		{errcode.GatewayInternal, 500, "api_error", "api_error"},
	}

	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			// The OpenAI surface.
			rec := httptest.NewRecorder()
			proxy.Write(rec, proxy.SurfaceOpenAI, proxy.NewError(c.code, "msg"))
			if rec.Code != c.status {
				t.Errorf("OpenAI status = %d, want %d", rec.Code, c.status)
			}
			var oa struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &oa); err != nil {
				t.Fatalf("parsing the OpenAI structure: %v (%s)", err, rec.Body.String())
			}
			if oa.Error.Type != c.openAIType || oa.Error.Code != c.code {
				t.Errorf("OpenAI type/code = %s/%s, want %s/%s",
					oa.Error.Type, oa.Error.Code, c.openAIType, c.code)
			}

			// The Anthropic surface: type=error at the top level, with the error
			// body nested under error.
			rec = httptest.NewRecorder()
			proxy.Write(rec, proxy.SurfaceAnthropic, proxy.NewError(c.code, "msg"))
			if rec.Code != c.status {
				t.Errorf("Anthropic status = %d, want %d", rec.Code, c.status)
			}
			var an struct {
				Type  string `json:"type"`
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &an); err != nil {
				t.Fatalf("parsing the Anthropic structure: %v (%s)", err, rec.Body.String())
			}
			if an.Type != "error" {
				t.Errorf("Anthropic top-level type = %q, want error", an.Type)
			}
			if an.Error.Type != c.anthropicType || an.Error.Code != c.code {
				t.Errorf("Anthropic type/code = %s/%s, want %s/%s",
					an.Error.Type, an.Error.Code, c.anthropicType, c.code)
			}
		})
	}
}

// A 429 always carries Retry-After.
func TestRetryAfterHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	proxy.Write(rec, proxy.SurfaceOpenAI, &proxy.Error{
		Code: errcode.GatewayRateLimited, Message: "slow down", RetryAfter: 3500 * time.Millisecond,
	})
	if got := rec.Header().Get("Retry-After"); got != "4" {
		t.Fatalf("Retry-After = %q, want 4 (seconds, rounded)", got)
	}
}

// The gateway's own 5xx leaks no internal detail; an upstream 4xx's own text
// must be passed through.
func TestServerErrorHidesDetailButPassesUpstream4xx(t *testing.T) {
	rec := httptest.NewRecorder()
	proxy.Write(rec, proxy.SurfaceOpenAI,
		proxy.NewError(errcode.GatewayInternal, "pgx: connection refused on 10.0.0.7:5432"))
	if body := rec.Body.String(); strings.Contains(body, "10.0.0.7") || strings.Contains(body, "pgx") {
		t.Fatalf("a 5xx must not leak internal detail: %s", body)
	}

	rec = httptest.NewRecorder()
	proxy.Write(rec, proxy.SurfaceOpenAI, &proxy.Error{
		Code:            errcode.GatewayInvalidRequest,
		Message:         "Invalid request",
		UpstreamMessage: "max_tokens must be <= 4096",
	})
	// Decoded, the meaning is the same.
	var oa struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &oa); err != nil {
		t.Fatal(err)
	}
	if oa.Error.Message != "max_tokens must be <= 4096" {
		t.Fatalf("an upstream 4xx's own text must be passed through: %q", oa.Error.Message)
	}
	// And at the byte level it matches the upstream: the default HTML escaping
	// would rewrite the comparison operator, and a developer comparing the two
	// outputs would think the gateway had altered the content.
	if !strings.Contains(rec.Body.String(), "<= 4096") {
		t.Fatalf("< > and & must not be HTML-escaped: %s", rec.Body.String())
	}
}

// An unregistered code collapses to internal and must never render an error
// body with an empty type.
func TestUnknownCodeFallsBackToInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	proxy.Write(rec, proxy.SurfaceOpenAI, proxy.NewError("gateway.not_a_real_code", "x"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	var oa struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &oa)
	if oa.Error.Type != "api_error" || oa.Error.Code != errcode.GatewayInternal {
		t.Fatalf("should collapse to internal: %+v", oa.Error)
	}
}

// Every gateway code in the registry must have a rendering shape: the registry
// and the rendering table must not drift apart.
func TestEveryRegisteredGatewayCodeRenders(t *testing.T) {
	for code, def := range errcode.All() {
		if len(code) < 8 || code[:8] != "gateway." {
			continue
		}
		rec := httptest.NewRecorder()
		proxy.Write(rec, proxy.SurfaceOpenAI, proxy.NewError(code, "x"))
		if rec.Code != def.Status {
			t.Errorf("%s renders status %d but the registry says %d; a code missing from the rendering table falls to 500",
				code, rec.Code, def.Status)
		}
	}
}
