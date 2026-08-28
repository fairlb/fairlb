# Video jobs

Everything in this file follows from one difference: on the inference data plane
a request *is* the unit of work and the unit of billing, and on the video plane
it is not. A video is submitted, it runs for a minute or two, and the film is
fetched afterwards — possibly days later, possibly never.

This page is for people changing the code. The customer-facing description is
[../video.md](../video.md).

## Why this plane translates when the other one refuses to

[same-protocol-passthrough.md](same-protocol-passthrough.md) says the gateway
never translates. That rule is intact, and it is worth being precise about what
it governs, because "video normalises parameters" reads like an exception to it.

The rule governs **a plane where the caller has already chosen a dialect**. An
OpenAI-shaped request is written in OpenAI's fields; rewriting it into
Anthropic's is the gateway deciding what the caller meant to say. No video
vendor has published a dialect the others speak — the leading models are a
long-running operation on one side and half a dozen private shapes on the other
— so on this plane there is no second dialect to be unfaithful to. The caller
chose ours because ours is the only one there is.

Two properties keep that safe, and both have to hold:

- **The vocabulary is closed and enumerable.** Chat parameters are open-ended
  (content blocks, tool definitions, cache control, reasoning configuration) and
  no list of them is ever finished. The video set is eleven fields, chosen
  against three published vendor contracts rather than guessed at.
- **The bill does not depend on the translation.** A chat charge is computed
  from the token counts an upstream reports, so a mistranslation is a wrong
  bill. A video charge is a pure function of the request's own parameters, known
  before anything is sent. A mistranslation here fails to produce a request; it
  cannot produce a wrong charge.

The boundary is structural, not a matter of discipline: `RewriteRequest` is
still the only function that changes an inference request body, and it still
does only its four things. The video mappers live in their own package that
imports nothing but the catalogue, so a mapper cannot reach a connection pool
and quietly become a second data plane.

## Money, and the two columns

The price is exact at admission. Duration, resolution, audio and `n` are all in
the request, so the amount held **is** the amount settled — none of the
"reserve high, refund on settle" reasoning that applies to token billing applies
here, and copying a comment about it into this path would be wrong.

`status` and `settlement_state` are two columns on `gateway_async_jobs`, and
collapsing them is the path on which a finished job is charged twice. They come
apart in states that really happen:

| `status` | `settlement_state` | what it is |
|---|---|---|
| `completed` | `settled` | the ordinary ending |
| `completed` | `held` | the settlement transaction failed; awaiting replay |
| `completed` | `orphaned` | a sweeper reclaimed the reservation first |
| `failed` / `canceled` | `voided` | produced nothing, charged nothing |

The idempotency guard for settling is the row count from
`WHERE settlement_state = 'held'`. Repeated polls, concurrent replicas and job
retries all converge on one charge through that one predicate.

**A job that produced no film is charged nothing**, and it still writes a usage
row at zero. Hiding those rows would make "why did my video fail" unanswerable,
and content refusals are the ordinary failure here rather than an edge case.

## Pinning

A job row records which route, which provider and which *credential* created it,
and none of those is ever re-picked. An upstream job id means nothing on a
different upstream account: asking key B about a job that exists only under key
A returns a 404, and so does asking with a platform key about a job created on
an organization's own credential. Both look like "the job disappeared", and both
end with a completed job's charge being voided.

That is why the pin is five columns rather than a route id, and why it does not
reuse `resource_affinities`: that table's garbage collection deletes expired
rows knowing nothing about settlement, and its primary key contains the upstream
id — which a job row cannot have, because the row must exist *before* the submit
(the row is the record that a hold was taken).

## The capability envelope

Declared, not observed — the one deliberate exception to "capability is
observed", and the reason is that the observation is unaffordable: answering
"does this model take a twelve-second clip" costs a twelve-second clip.

What makes the exception safe is that **neither direction of drift produces a
wrong bill**. Declaring too little refuses the request at admission having spent
nothing; declaring too much ends in an upstream refusal, a terminal `failed`
job, and a voided hold. The `endpoints` column this repository deleted had no
such property — over-declaring there put live traffic on a route that could
never work.

It lives on the **route**, because one model deployed on two channels can accept
different duration steps. Admission validates against the union across candidate
routes, and then the envelope narrows the candidates. Price never varies by
route, so narrowing can change who serves a job but never what it costs.

`source` separates `observed` (a vendor's own capability endpoint answered) from
`declared` (a person typed it). Nothing in this build observes one, so every
stored envelope is `declared` — the staff API refuses to store anything else,
and the interface says which it is rather than leaving the reader to assume. The
vendor mappers' default envelopes are a **prefill**, not runtime truth: from the
moment somebody saves one, the person who saved it is the one answering for it.

### Cancel has exactly one source

How far a job can be stopped is a field of the envelope, and everything reads it
from there: `GET /v1/videos/models` publishes it, the console decides whether to
offer a stop button from it, and the cancel endpoint enforces it. `VideoCancelMode`
is the one function that answers, and unset reads as `never`.

It is worth saying because there is an obvious second source and it is wrong.
Each vendor mapper carries a `CancelMode()` — the default this build recorded
from that vendor's contract — and reading it at cancel time made the two
disagree in both directions: a route declaring `anytime` was refused while the
catalogue advertised it, and a route declaring `never` was cancelled anyway
while the catalogue said it could not be. The mapper's value belongs to the
prefill endpoint the envelope editor calls, and nowhere else.

The mapper keeps one veto, and it is a different question: `Cancel()` returns an
error when the vendor published no cancel at all, so a declaration the upstream
cannot carry out is refused rather than sent and silently ignored.

## Endpoint reachability is still observed

The envelope says what a model accepts. Whether a route serves video at all is
still a probe verdict, and that probe is `manual`: one video probe generates a
real clip, one to two orders of magnitude dearer than an image probe. It never
enters the periodic sweep and is never run when a route is created.

Because it is asked for by hand and answers slowly, a probe in flight is a fact
the interface has to be able to state — see
[ADR-0224](../../../docs/adr/0224-a-probe-in-flight-is-a-column-not-a-verdict.md)
for why that is a column of its own and not a fifth `status` value.

## Artifacts

`GET /v1/videos/{id}/content` serves bytes from this deployment. No upstream URL
is ever returned or redirected to: one vendor's link needs that vendor's
credentials to fetch at all, the rest expire somewhere between an hour and a
week, and every one of them names the upstream and can be neither renewed nor
revoked from here.

Storage is a port on the gateway module, shaped like the settlement port and for
the same reason — object storage is a whole subsystem, not something every
deployment has:

- **With an object store**, the reconciler fetches the bytes the moment a job
  reaches terminal success, not lazily on first read. Upstream windows as short
  as an hour exist, and "come back for it in a few days" is a normal use.
- **Without one**, the bytes are proxied from the upstream on read, using the
  credential the job pinned.

The two behave identically from outside. What differs is the promise: with
custody it is "available for the whole retention window", without it is
"available for as long as the upstream still has it". Saying "you can always
fetch it" would be the same lie as an unimplemented feature in the interface,
only told at runtime.

Past its window the answer is `410`, which is a normal ending rather than a
fault, and which the console shows as "no longer kept".

## Where to look

| What | Where |
|---|---|
| Normalised vocabulary | `internal/gateway/video/params.go` |
| Capability envelope | `internal/gateway/video/envelope.go` |
| One vendor's outbound mapping | `internal/gateway/video/vendor_*.go` |
| One vendor's inbound compatibility surface | `internal/gateway/video/native_*.go` |
| Admission, submit, pinning | `internal/gateway/proxy/videos.go`, `video_submit.go` |
| The compatibility surfaces' HTTP layer | `internal/gateway/proxy/videos_native.go` |
| Job operations shared by every surface | `internal/gateway/proxy/video_jobs.go` |
| Reconciler and sweeper | `internal/gateway/proxy/video_worker.go` |
| Per-unit pricing, and its bundled prefill | `internal/gateway/catalog/unit_price.go`, `internal/gateway/pricing/refdata/unitrates.go` |
| Wire contracts | `api/videos.yaml`, `api/videos-native.yaml` |

## The vendor compatibility surfaces

Five vendors' own APIs, answered at `/video/<vendor>`, so that a caller already
written against one of them switches by changing a base URL (ADR-0228).

Two things about them decide most of the code:

**It is not a passthrough.** Not for want of trying: every one of these APIs is
asynchronous, so forwarding a submit creates an upstream job this gateway holds
no row for -- no hold, no settlement, no usage line -- and forwarding a poll
returns the upstream's own URL, which never leaves here. So the shapes are the
vendor's and the job is ours: our identifier occupies their task id, our
`/content` occupies their video URL. Both are opaque strings to any client,
which is the whole reason the substitution costs the caller nothing.

**Inbound is a separate interface.** `Mapper`'s methods are all outbound -- this
gateway as a client of that vendor. `NativeSurface`'s are inbound -- this
gateway impersonating it. A vendor may have the first and not the second
("routable, not switch-to-able"); the reverse is refused by a test, because a
surface with nowhere to send the job is a promise that cannot be reached.

Two rules keep them honest, and both are the ones `vendor_options` used to
carry.

**A priced axis is read, never forwarded.** On a vendor's own surface *their*
parameters are first-class, so the only thing between a caller and a back-door
duration is the surface reading every priced axis into the `Request`. One table
per vendor names them, and a test walks all five.

**Everything else is forwarded, or the request is refused naming it — never
dropped.** The second half of that took a review to find: three of the five
surfaces built their passthrough from one sub-object of the body and lost
whatever fell outside it, and the test suite only checked that priced keys were
*absent* from the passthrough, never that unpriced ones survived. The passthrough
is body-shaped now, `mergePassthrough` folds sub-objects into the ones the mapper
built, and `TestNoSurfaceSilentlyDropsAFieldTheCallerWrote` plants a made-up
field in every place a body has room for one.

**The surface's vendor narrows the candidates.** A request that arrived in one
vendor's shape carries that vendor's parameters, so serving it from another's
route would send them somewhere they mean nothing. `admitAndSubmitVideo` takes a
vendor for this; `/v1/videos` passes none, because there every route of a slug
serves the same model.
