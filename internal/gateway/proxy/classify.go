package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/errcode"
)

// Classifying upstream failures: the executable form of the decision table in
// docs/design/failover-and-cooldowns.md. There are four classes, and the class
// decides three things: whether this request rotates to another candidate,
// which level of circuit breaker records it, and what the client sees.

// FailureClass is the class of an upstream failure.
type FailureClass int

const (
	// ClassKey: this credential's problem (401, 403, 429). Try another
	// credential on the same provider, and another provider if there is none.
	ClassKey FailureClass = iota
	// ClassProvider: this provider's problem (5xx, timeout, connection error).
	// Try the next provider.
	ClassProvider
	// ClassRoute: this route is misconfigured -- a 404 means the model is not
	// on this provider. Mark the route unusable without cooling the provider.
	ClassRoute
	// ClassClient: the caller's problem (400, 413). Pass the response through
	// and do not retry.
	ClassClient
	// ClassTerminal: the first byte has gone out, so nothing can be retried.
	ClassTerminal
)

// Classification is the full verdict on one upstream failure.
type Classification struct {
	Class FailureClass
	// Err is what gets rendered to the client, and is only really used once
	// every retry has failed.
	Err *Error
	// CooldownHint comes from the upstream's Retry-After. On a 429 the cooldown
	// is the greater of this and the backoff rung.
	CooldownHint time.Duration
	// CountsTowardHealth false keeps the failure out of the provider's health
	// score. A 429 is a quota problem rather than a fault, and counting it
	// toward the error rate would open the circuit on a healthy provider.
	CountsTowardHealth bool
	// keyID records which provider credential was used, so key-level breaking
	// knows what to cool down.
	keyID pgtype.UUID
	// upstreamStatus is the HTTP status this verdict came from, zero for a
	// transport failure that never got one. It is kept so a failed hop can be
	// recorded with the status that caused it: "provider class" tells you a
	// circuit moved, while 429 against 503 tells you whether to call the
	// vendor or wait.
	upstreamStatus int
}

// String names the class for the attempt trail and the metric label. The set is
// closed and small, so the labels stay bounded.
func (c FailureClass) String() string {
	switch c {
	case ClassKey:
		return "key"
	case ClassProvider:
		return "provider"
	case ClassRoute:
		return "route"
	case ClassClient:
		return "client"
	case ClassTerminal:
		return "terminal"
	default:
		return "unknown"
	}
}

// ClassifyStatus classifies by the upstream's HTTP status and response body.
func ClassifyStatus(status int, body []byte, retryAfter string) Classification {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// The credential is not accepted: cool this one down on a single
		// occurrence, for a long time.
		return withStatus(status, Classification{
			Class:              ClassKey,
			Err:                NewError(errcode.GatewayAllProvidersFailed, "Upstream rejected the credential"),
			CountsTowardHealth: false, // recorded against the credential, not the provider's health
		})

	case status == http.StatusTooManyRequests:
		return withStatus(status, Classification{
			Class:              ClassKey,
			Err:                &Error{Code: errcode.GatewayRateLimited, Message: "Upstream rate limit reached", RetryAfter: time.Second},
			CooldownHint:       parseRetryAfter(retryAfter),
			CountsTowardHealth: false, // a quota problem, not a fault
		})

	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		// Nothing at this address for this model -- the route is a wrong fit
		// for the endpoint: try the next candidate and ask the probe worker
		// to look, but do not cool down the whole provider. 405 is the same
		// statement from an upstream that routes by method: the path exists,
		// the operation does not.
		return withStatus(status, Classification{
			Class: ClassRoute,
			Err:   NewError(errcode.GatewayModelNotFound, "Model not available on the selected upstream"),
		})

	case status == http.StatusRequestEntityTooLarge:
		return withStatus(status, Classification{
			Class: ClassClient,
			Err:   NewError(errcode.GatewayRequestTooLarge, "Request body exceeds the upstream limit"),
		})

	case status == http.StatusRequestTimeout:
		return withStatus(status, Classification{
			Class:              ClassProvider,
			Err:                NewError(errcode.GatewayUpstreamTimeout, "Upstream timed out"),
			CountsTowardHealth: true,
		})

	// Overloaded. Treated exactly as any other server-side failure: it counts
	// towards the provider's health and moves the same backoff ladder. There is
	// no separate short cooldown for this status.
	case status == statusOverloaded:
		return withStatus(status, Classification{
			Class:              ClassProvider,
			Err:                NewError(errcode.GatewayAllProvidersFailed, "Upstream is overloaded"),
			CountsTowardHealth: true,
		})

	case status >= 500:
		return withStatus(status, Classification{
			Class:              ClassProvider,
			Err:                NewError(errcode.GatewayAllProvidersFailed, "Upstream error"),
			CountsTowardHealth: true,
		})

	case status >= 400:
		// A 400 is a semantic error, including complaints about context length
		// and token limits. The upstream's own text is passed through, because
		// a developer needs those exact words to locate the bad parameter.
		return withStatus(status, Classification{
			Class: ClassClient,
			Err: &Error{
				Code: errcode.GatewayInvalidRequest, Message: "Invalid request",
				UpstreamMessage: truncateBody(body),
			},
		})
	}
	// 2xx and 3xx should never reach here.
	return withStatus(status, Classification{Class: ClassProvider, Err: NewError(errcode.GatewayInternal, "Unexpected upstream status"), CountsTowardHealth: true})
}

// classifyUpstreamStatus adds the semantics that only exist after a request
// has been resolved. A 404 on an affinity-pinned request means the known
// resource disappeared; treating it as a missing model would rotate away from
// the only route allowed to serve that state and poison route health.
func classifyUpstreamStatus(in Request, status int, body []byte, retryAfter string) Classification {
	if status == http.StatusNotFound &&
		(in.PinnedProviderKeyID.Valid || in.PinnedOrgProviderKeyID.Valid) {
		return withStatus(status, Classification{
			Class: ClassClient,
			Err:   NewError(errcode.GatewayResourceNotFound, "Resource not found or expired"),
		})
	}
	return ClassifyStatus(status, body, retryAfter)
}

// withStatus pins the status the verdict came from. Applied at the return
// rather than by each branch so a new branch cannot forget it.
func withStatus(status int, c Classification) Classification {
	c.upstreamStatus = status
	return c
}

// statusOverloaded is the non-standard overload status some upstreams return,
// spelled out here because net/http has no constant for it.
const statusOverloaded = 529

// errFirstByteBudget is the cause attached when the gateway's own first-byte
// timer cancels an attempt.
//
// It has to be distinguishable from the client hanging up, and by the time the
// error surfaces both are plain context.Canceled. Without the cause they are
// the same error, and the client-hangup reading is the one that wins -- which
// would mean the gateway's own timeout got recorded as "the caller left": not
// counted against the upstream, not retried, and invisible in every reading
// anyone would think to check.
var errFirstByteBudget = errors.New("proxy: the first-byte budget expired")

// ClassifyTransportError classifies transport-level failures: connection
// refused, timeout, connection reset.
//
// ctx is the attempt's context, consulted only to tell our own cancellation
// apart from the caller's. Passing context.Background() is fine where no timer
// is involved.
func ClassifyTransportError(ctx context.Context, err error) Classification {
	if errors.Is(context.Cause(ctx), errFirstByteBudget) {
		// Our timer fired. This is an upstream that would not answer in time,
		// so it counts against the provider exactly as a timeout does.
		return Classification{
			Class:              ClassProvider,
			Err:                NewError(errcode.GatewayUpstreamTimeout, "Upstream timed out"),
			CountsTowardHealth: true,
		}
	}
	if errors.Is(err, context.Canceled) {
		// The client hung up: not counted as a failure and not held against
		// provider health.
		return Classification{Class: ClassTerminal, Err: nil, CountsTowardHealth: false}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return Classification{
			Class:              ClassProvider,
			Err:                NewError(errcode.GatewayUpstreamTimeout, "Upstream timed out"),
			CountsTowardHealth: true,
		}
	}
	return Classification{
		Class:              ClassProvider,
		Err:                NewError(errcode.GatewayUpstreamTimeout, "Upstream is unreachable"),
		CountsTowardHealth: true,
	}
}

// Retryable reports whether this class is worth trying another candidate for.
func (c Classification) Retryable() bool {
	switch c.Class {
	case ClassKey, ClassProvider, ClassRoute:
		return true
	default:
		return false
	}
}

// parseRetryAfter parses Retry-After in its seconds form. The HTTP-date form is
// rare enough to ignore.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// truncateBody truncates the upstream's own text, so that a huge response
// cannot blow up the logs and the response.
func truncateBody(body []byte) string {
	const maxLen = 2000
	if len(body) > maxLen {
		body = body[:maxLen]
	}
	return string(bytes.TrimSpace(body))
}
