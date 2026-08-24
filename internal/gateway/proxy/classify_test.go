package proxy_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

// An executable copy of the failure decision table in
// docs/gateway-errors.md §2, one assertion per row. The table is
// the contract: change the document and this must follow, and the other way
// round.
func TestClassifyDecisionTable(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		retryAfter  string
		wantClass   proxy.FailureClass
		wantCode    string
		wantRetry   bool // whether to rotate to another candidate
		wantsHealth bool // whether it counts toward provider health
	}{
		{
			name:   "400 semantic error -> client class, passed through, not retried",
			status: 400, body: `{"error":{"message":"invalid temperature"}}`,
			wantClass: proxy.ClassClient, wantCode: errcode.GatewayInvalidRequest,
			wantRetry: false, wantsHealth: false,
		},
		{
			name:   "400 about context length -> the same; another provider gives the same error",
			status: 400, body: `{"error":{"message":"context_length_exceeded"}}`,
			wantClass: proxy.ClassClient, wantCode: errcode.GatewayInvalidRequest,
			wantRetry: false, wantsHealth: false,
		},
		{
			name:      "401 -> credential class, provider health untouched",
			status:    401,
			wantClass: proxy.ClassKey, wantCode: errcode.GatewayAllProvidersFailed,
			wantRetry: true, wantsHealth: false,
		},
		{
			name:      "403 -> credential class",
			status:    403,
			wantClass: proxy.ClassKey, wantCode: errcode.GatewayAllProvidersFailed,
			wantRetry: true, wantsHealth: false,
		},
		{
			name:      "404 -> route class, a configuration error, no provider cooldown",
			status:    404,
			wantClass: proxy.ClassRoute, wantCode: errcode.GatewayModelNotFound,
			wantRetry: true, wantsHealth: false,
		},
		{
			// The same statement from an upstream that routes by method: the
			// path exists, the operation does not. The probe worker treats 405
			// as the definitive "nothing here" too, and the two classifiers of
			// that answer must agree or live traffic never asks for the probe.
			name:      "405 -> route class, like 404",
			status:    405,
			wantClass: proxy.ClassRoute, wantCode: errcode.GatewayModelNotFound,
			wantRetry: true, wantsHealth: false,
		},
		{
			name:      "408 -> provider class, counts toward health",
			status:    408,
			wantClass: proxy.ClassProvider, wantCode: errcode.GatewayUpstreamTimeout,
			wantRetry: true, wantsHealth: true,
		},
		{
			name:      "413 -> client class, passed through",
			status:    413,
			wantClass: proxy.ClassClient, wantCode: errcode.GatewayRequestTooLarge,
			wantRetry: false, wantsHealth: false,
		},
		{
			name:   "429 -> credential class, brief cooldown, no health effect since quota is not a fault",
			status: 429, retryAfter: "30",
			wantClass: proxy.ClassKey, wantCode: errcode.GatewayRateLimited,
			wantRetry: true, wantsHealth: false,
		},
		{
			name:      "500 -> provider class",
			status:    500,
			wantClass: proxy.ClassProvider, wantCode: errcode.GatewayAllProvidersFailed,
			wantRetry: true, wantsHealth: true,
		},
		{
			name:      "502 -> provider class",
			status:    502,
			wantClass: proxy.ClassProvider, wantCode: errcode.GatewayAllProvidersFailed,
			wantRetry: true, wantsHealth: true,
		},
		{
			name:      "503 -> provider class",
			status:    503,
			wantClass: proxy.ClassProvider, wantCode: errcode.GatewayAllProvidersFailed,
			wantRetry: true, wantsHealth: true,
		},
		{
			name:      "529 overloaded -> provider class, brief cooldown",
			status:    529,
			wantClass: proxy.ClassProvider, wantCode: errcode.GatewayAllProvidersFailed,
			wantRetry: true, wantsHealth: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cls := proxy.ClassifyStatus(c.status, []byte(c.body), c.retryAfter)
			if cls.Class != c.wantClass {
				t.Errorf("class = %v, want %v", cls.Class, c.wantClass)
			}
			if cls.Err == nil || cls.Err.Code != c.wantCode {
				t.Errorf("error code = %v, want %s", cls.Err, c.wantCode)
			}
			if cls.Retryable() != c.wantRetry {
				t.Errorf("retryable = %v, want %v", cls.Retryable(), c.wantRetry)
			}
			if cls.CountsTowardHealth != c.wantsHealth {
				t.Errorf("counts toward health = %v, want %v", cls.CountsTowardHealth, c.wantsHealth)
			}
		})
	}
}

// A 429's cooldown takes the greater of the upstream's Retry-After and the
// backoff rung: if the upstream says wait this long, wait at least that long.
func TestClassify429UsesRetryAfterHint(t *testing.T) {
	cls := proxy.ClassifyStatus(429, nil, "120")
	if cls.CooldownHint != 2*time.Minute {
		t.Fatalf("Retry-After should have been parsed: %v", cls.CooldownHint)
	}
	// Malformed or absent degrades to 0, and the backoff rung decides.
	if got := proxy.ClassifyStatus(429, nil, "").CooldownHint; got != 0 {
		t.Errorf("an absent Retry-After should give 0: %v", got)
	}
	if got := proxy.ClassifyStatus(429, nil, "Wed, 21 Oct 2026 07:28:00 GMT").CooldownHint; got != 0 {
		t.Errorf("the HTTP-date form is not parsed and should give 0: %v", got)
	}
}

// A 400 must pass the upstream's own text through: a developer needs those
// exact words to locate the bad parameter.
func TestClassifyPassesUpstreamMessage(t *testing.T) {
	body := `{"error":{"message":"max_tokens must be <= 4096"}}`
	cls := proxy.ClassifyStatus(400, []byte(body), "")
	if cls.Err.UpstreamMessage == "" {
		t.Fatal("a 400 must carry the upstream's own text")
	}
	if !strings.Contains(cls.Err.UpstreamMessage, "max_tokens must be <= 4096") {
		t.Errorf("the upstream text is incomplete: %q", cls.Err.UpstreamMessage)
	}
}

// Transport-level failures: a client hanging up is not counted as a failure,
// while timeouts and refused connections count at the provider level.
func TestClassifyTransportErrors(t *testing.T) {
	cancel := proxy.ClassifyTransportError(context.Background(), context.Canceled)
	if cancel.Class != proxy.ClassTerminal || cancel.CountsTowardHealth {
		t.Errorf("a client hanging up must not count as a failure: %+v", cancel)
	}

	timeout := proxy.ClassifyTransportError(context.Background(), &net.DNSError{IsTimeout: true})
	if timeout.Class != proxy.ClassProvider || !timeout.CountsTowardHealth {
		t.Errorf("a timeout should count at the provider level: %+v", timeout)
	}
	if timeout.Err.Code != errcode.GatewayUpstreamTimeout {
		t.Errorf("timeout error code: %s", timeout.Err.Code)
	}

	refused := proxy.ClassifyTransportError(context.Background(), errors.New("connection refused"))
	if refused.Class != proxy.ClassProvider || !refused.CountsTowardHealth {
		t.Errorf("a refused connection should count at the provider level: %+v", refused)
	}
}
