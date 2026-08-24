# Failover, circuit breaking and cooldowns

When an upstream misbehaves, three questions have to be answered in order: is
this the caller's problem or ours, can this request still be saved, and should
this upstream keep receiving traffic. They have different answers and mixing
them up produces a gateway that retries things it should pass through and keeps
sending traffic to something that is plainly down.

## Four classes of failure

| Class | This request | The upstream's standing |
|---|---|---|
| **Client** | Pass the response through unchanged, do not retry | Untouched — it answered correctly |
| **Key-level** | Try the next candidate, which reaches another key on the same provider only if that provider serves this model twice | That credential cools down |
| **Provider-level** | Try the next candidate route | Error count for the provider goes up |
| **Unretryable** | End the response and record what happened | Error count goes up |

Mapped onto what upstreams actually return:

| Upstream response | Class | Cooldown effect |
|---|---|---|
| 400, including context-length and token-limit complaints | Client | None |
| 401, 403 | Key-level | That credential cools down for a long time |
| 404 / 405 — nothing at this address for this model | Provider-level, and a configuration error | The next candidate is tried and a probe of that endpoint is requested; the provider is not punished |
| 408, connect timeout, connection reset | Provider-level | Error count for the provider |
| 413 | Client | None |
| 429 with `Retry-After` | Key-level | That credential cools down for at least the interval it asked for; the provider's health is untouched |
| 500, 502, 503 | Provider-level | Error count for the provider |
| 529 (overloaded) | Provider-level | Error count for the provider, exactly like any other server-side failure — there is no separate short cooldown for it |
| Timeout waiting for the first byte | Provider-level | Error count; failover is still safe here, nothing has been sent to the client |
| Stream breaks after the first byte | Unretryable | Error count |
| Client disconnects | Neither | Nothing counted; the usage row is marked cancelled and settled for what was produced |

Two entries in that table carry most of the weight.

**401 and 403 are about the credential, not the provider.** One expired key
among five should move traffic to the other four, not declare the provider down.
Punishing the provider for a credential problem takes out four working keys.

**429 is about the credential too, and it must not touch the provider's health
score.** Rate limits are the upstream working as intended. Counting them as
failures means a busy period looks like an outage, and the gateway responds by
routing away from a provider that is fine.

## Two levels, and why they are separate

Health is tracked for the individual **provider key** and for the **provider**
as a whole. The key level exists so that one bad credential does not cost you a
provider; the provider level exists so that a genuinely broken upstream stops
receiving traffic no matter how many keys it has.

Opening the breaker uses an exponential ladder with jitter:

| Parameter | Value |
|---|---|
| Backoff ladder | 30s, 1m, 4m, 10m, 30m, then held at 30m |
| Jitter | Plus or minus 20% |
| Provider opens after | 5 consecutive failures |
| Credential opens after | One 401 or 403 — for 30 minutes; a 429 cools down for what it asked for |
| Recovery | On expiry, one real request is let through. Success closes the breaker, failure moves one rung up the ladder |

Two details in there are not obvious:

- **A 401 goes straight to the long cooldown**, skipping the ladder. A wrong
  credential does not heal on its own, and a 30-second cooldown just means
  retrying an invalid key twice a minute forever.
- **The state outlives the cooldown.** The record is kept for the cooldown plus
  a memory window. If it expired exactly when the cooldown did, the ladder
  position would vanish with it, every open would be the first open, and the
  backoff would never leave its first rung.

Breaker state lives in the breaker driver — in memory by default, in Redis when
you have several replicas. There is a database table too, but only so a restart
does not begin with a clean slate; the hot path never reads it.

## Limits on failover

**Three routes per request.** At most 3 candidates are tried; beyond that the
request has already cost the caller more latency than a retry can win back.

**A global retry budget.** Retries are capped at 10% of total requests, measured
once at least 100 requests have been seen so a quiet gateway is not judged on
its first handful. Without
this, a wobbling upstream gets hit by three times its normal traffic exactly
when it is least able to take it, which converts a partial outage into a
complete one. The budget is what stops the gateway from amplifying a failure it
did not cause.

**No failover after the first byte.** Once part of a streamed response has
reached the client, there is nothing to fail over to: the client has already
received tokens that a second attempt would not produce again. The stream ends
with an error event and the usage is recorded for what was actually produced.

Each request also records the candidates that failed before the one that
served it — which route, which credential, what the upstream answered and how
long it took — on the request's own usage row. One request stays one row: a
table where a single request can appear several times would make every budget,
invoice and rollup start by asking which of those rows are real.

**Streamed requests fail over too, up to the first byte.** The moment the first
frame is written is a fact rather than an estimate — the response header is
withheld until there is a complete frame to send — so a stream whose upstream
refuses the connection, or answers 503, or rejects the credential is served by
the next candidate, and the caller sees one clean response.

**A first-byte timeout does not fail over, though.** The first-byte budget is
one budget for the whole request, not one per attempt, so a candidate that spent
it leaves nothing for the next one to run under. Giving each attempt its own
full budget is what makes a slow request outlast the proxies in front of the
gateway, and an error from one of those tells the caller nothing about which
upstream was slow. So failover recovers from candidates that fail *fast*; a
candidate that is merely silent ends the request.

**Backpressure instead of a queue.** Each provider has a concurrency limit;
when it is full that candidate is skipped and the next one is tried, and if
every candidate is full the gateway answers 429 with `Retry-After` immediately
rather than queueing. A queue only moves the latency to a place the caller
cannot see, and by the time a queued request runs, the caller has usually timed
out anyway. A stream holds its slot until the first byte, not for the whole
response: holding it for the duration would quietly turn a backpressure valve
into a cap on how many streams one provider may serve at once, which is a
different limit and not one anybody asked for.

**Declared capacity is a filter, like health.** A provider may also state what
its upstream account will take per minute, in requests and in tokens. A provider
with nothing left this minute is out of the running exactly as a cooling-down
one is: skipped, next candidate tried. It never becomes a weight — an upstream
whose share of traffic quietly shrinks is far harder to reason about than one
that is plainly out, and the reason it is out can be read rather than inferred.

**Being skipped costs the request nothing.** Neither a full concurrency slot nor
an exhausted minute counts as one of the three attempts, and neither draws on
the global retry budget. Nothing was sent upstream, so nothing was tested; a
busy provider must not be able to spend a request's whole failover allowance
without a single call having been made.

## Timeouts

| Stage | Budget |
|---|---|
| Connect | 5s |
| First byte | 60s |
| Idle within a stream | 30s |
| Whole request, non-streaming | 120s |
| Image endpoints | 300s |

The connect budget is the one stage a provider may override
(`transport.connect_timeout_ms`, 50–60000ms) — a relay on the other side of the
world legitimately needs more than the default, and nothing else does.

Connecting is bounded separately from waiting for an answer, and much more
tightly, because the two mean different things: an upstream that is thinking is
legitimately slow, while one whose address will not accept a connection is not
about to start. Keeping them apart is what lets a dead candidate be abandoned in
seconds while a slow one is given a full minute.

The first-byte budget is a **single budget spanning two waits**: waiting for the
upstream's response headers, and then waiting for the first event of the stream.
The second wait gets whatever the first did not use. Giving each its own 60
seconds would allow 120 in the worst case, which is past the point where the
proxies in front of you start returning their own errors — and an error from
your proxy tells the caller nothing about which upstream was slow.

## What learns, and what does not

All of the above is learned from **real traffic**. There is no background job
probing your upstreams on a schedule, which has one consequence worth knowing:
an upstream that recovers while no traffic is being routed to it stays cooled
down until the next request arrives to find out. On a busy gateway that is
milliseconds; on an idle one it is until you use it.

Two things you can do by hand, from the admin UI:

- **Test a provider** — one real call against its base URL and a credential,
  which is how you tell "the key is wrong" from "the model name is wrong"
  without reading logs.
- **Override what a route is known to serve** — a route declares nothing; the
  probe worker finds out, per endpoint, and its verdicts are what candidate
  selection reads (a route is skipped for an endpoint the upstream answered 404
  or 405 to). A 404 on live traffic is classified by the table above as a
  configuration error rather than an outage: that candidate is skipped for the
  rest of the request, the provider's health is left alone, and the data plane
  **asks for a probe** of that endpoint rather than writing a verdict itself —
  the gateway cannot tell a wrong model name from a model the upstream has
  temporarily withdrawn, and one response is not enough to decide. The worker
  decides; the sweeper re-checks red verdicts daily; and the operator can write
  a verdict of their own per endpoint, which the worker then leaves alone.
