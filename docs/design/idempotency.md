# Idempotency keys

Send `Idempotency-Key: <something unique>` with a POST to the admin or console
API and the result of the first attempt is what every retry with that key gets
back — the same status, the same body, without the work running twice.

## What it does

| Situation | Answer |
|---|---|
| First time this key is seen | The request runs; its response is stored |
| Same key, same request, first attempt finished | The stored response is replayed |
| Same key, **different** request body | 422 — the key is already committed to a different request |
| Same key, first attempt still running | 409 |
| Same key, first attempt started but has been silent for 60s | The retry takes over and runs |

Keys are scoped to the authenticated caller, so two callers cannot collide on
the same string, and they are kept for 24 hours.

"Same request" means the same fingerprint: a hash over the request, not just its
body. Comparing only the key would let a client accidentally receive the answer
to a different question.

## The details that matter when it goes wrong

**The takeover exists because processes die.** Without it, a request whose
handler crashed halfway would hold its key until the 24-hour expiry, and every
retry would get 409 until then — a stuck key for a day because a container was
restarted at the wrong moment. Sixty seconds of silence is treated as death, and
the retry takes over. If both end up finishing, each caller gets its own
response and only the first completion is stored.

**Replay does not include every header.** `Set-Cookie` is dropped, because
replaying it would resurrect a session credential that may since have expired.
The request id and rate-limit headers are dropped too, because a replayed
response reporting the original request's id and the original moment's remaining
quota is stale information dressed as current.

**There is a size limit.** Request bodies over 1 MiB with an idempotency key are
refused outright rather than truncated, because a truncated body silently
changes the request. Responses over the limit are not stored, and the key is
released so a retry re-runs the work instead of waiting on a promise that cannot
be kept.

**Only POST, and only with the header.** No key means no bookkeeping, which is
the right default: most requests do not need it, and every stored key is a row
with a lifetime.

## What it deliberately does not cover

**The data plane is not idempotent.** `/v1/chat/completions` and the other
inference endpoints do not participate. A completion is not a resource being
created — it is a paid, non-deterministic call whose whole value is that it runs
— and a client retrying one has generally decided it wants another answer, not
the previous one repeated. Storing responses there would also mean storing model
output, which is not something a gateway should keep by default.

**It is not a distributed lock.** It guarantees that one key produces one
outcome, not that two different keys doing conflicting things are serialised.
Two clients creating the same-named object with different keys is a uniqueness
question, and it belongs to the constraint that enforces it.
