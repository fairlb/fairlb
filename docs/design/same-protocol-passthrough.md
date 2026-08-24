# Same-protocol pass-through

**The gateway never translates between protocols.** An OpenAI-shaped request is
served by an OpenAI-shaped upstream, an Anthropic-shaped request by an
Anthropic-shaped one, and a Gemini-shaped request by a Gemini-shaped one. There
is no request or response conversion between them.

## Protocols and surfaces

A **protocol** is a wire dialect: the shape of the authentication header plus
the request and response schemas. A **surface** is a concrete endpoint. One
protocol has several surfaces.

| Protocol | Surfaces | Upstreams that speak it |
|---|---|---|
| `openai` | `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, `/v1/images/generations` and `/edits` | OpenAI, any OpenAI-compatible endpoint or relay, vendor compatibility layers |
| `anthropic` | `/v1/messages` and `/v1/messages/count_tokens` | Anthropic, and the two hosted platforms that publish that API |
| `gemini` | `/v1beta/models`, native model methods, embeddings and `/v1beta/interactions` | Google's Gemini API, and Vertex AI |

The prefixes are intentionally not aliases. `/v1` carries OpenAI and Anthropic
surfaces; `/v1beta` carries Gemini surfaces. In particular, `GET /v1/models`
keeps its OpenAI shape while `GET /v1beta/models` keeps Gemini's shape.

Capabilities are per operation, not inherited from the protocol — and they are
**observed, not declared**. A route that serves Responses does not automatically
serve compaction, and a Gemini generateContent route does not automatically
serve embeddings or Interactions; but nobody types that in. A route is probed
on every endpoint of the protocols its provider speaks, and the probe verdicts
are the only record of what it serves. The catalogue publishes an endpoint once
a probe has found it working; the data plane tries an endpoint unless a probe
has found it unsupported. The gap between the two is deliberate: "callable but
unlisted" costs nothing, "listed but 404" is a support ticket.

A provider record may declare **several** protocols. Relays commonly expose two
or three protocols behind one base URL and one key, and modelling that as several
provider records would give you two copies of the credential, the health state,
the cooldown state and the concurrency budget for one machine — and "one machine
with two independent health verdicts" is a wrong model before it is an
inconvenience.

Declaring several protocols changes nothing about forwarding. The outbound
authentication header is decided by the **surface the request arrived on**, not
by the provider record, and errors are rendered in the same shape. A provider's
protocol set only participates in filtering candidates and in deciding which
endpoints its routes are probed on. Left unstated when the provider is created,
it is the vendor registry's default: the protocols the preset base URL and
transport profile are wired for, which for a first-party vendor is simply the
one it speaks.

## Why no translation

A faithful translation matrix is the single largest source of bugs in a gateway
like this. Every field that exists on one side and not the other becomes a
judgement call, and every judgement call becomes a report that the gateway
"changed my request". Skipping it removes that whole class of defect, and the
cost is one honest constraint: a model answers on a protocol only through a
provider that speaks it. Claude on `/v1/chat/completions` means a route on a
provider with an OpenAI-shaped surface, not a translation in the gateway.

The tempting shortcut is a vendor compatibility layer — Anthropic publishes an
OpenAI-compatible endpoint, so why not point an `openai` route at it and get
Claude on the OpenAI surface for free?

Because of what that layer drops. Its `prompt_tokens_details` and
`completion_tokens_details` are always empty and it does not support prompt
caching. For a gateway that meters tokens, that is not a small compatibility
gap:

- **Cache reads disappear.** Tokens served from cache are billed to the caller
  at the full input rate, because nothing reports that they were cached.
- **Cache writes disappear entirely.** They are never counted.

The vendor's own documentation positions that layer as being for testing and
capability comparison rather than production. Taking it at its word is cheaper
than discovering the same thing from a usage report.

## A model belongs to no protocol

A model is a catalogue entry — a slug, a display name, a price. It is reachable
on **every protocol its routes' providers speak**: the same Claude slug wired to
an OpenAI-protocol relay and to Anthropic itself answers on both
`/v1/chat/completions` and `/v1/messages`, each request
passing through on the protocol it arrived on. Nothing is translated; the model
simply does not get to choose which protocol that is. A route is accepted on
any provider, and whether the pairing is a good idea — a relay's OpenAI-shaped
surface for a Claude model may meter less faithfully than the native one — is
read off the vendor's fidelity rating, not refused by a rule.

A model may have **several routes** — different providers, different upstream
model names. Candidates are filtered by the protocol the request arrived on and
by what has been observed: a route is a candidate for an endpoint unless a probe
has found it unsupported there (the upstream answered 404 or 405 — twice, for an
endpoint that had been verified). A route that nobody has probed yet is tried;
a probe that failed inconclusively (a 5xx, a timeout, a body the relay would
not take) leaves the route in rotation, because those are the provider's or the
probe's problem, not a statement about the endpoint. Two exceptions: an
endpoint the gateway refuses to probe on its own (images — it costs a real
generation) is a candidate only once a verdict says ok, since nothing would
converge otherwise; and a verdict reached with the platform's shared credential
does not bind an organization bringing its own for that vendor, because an
upstream's 404 also means "your project has no access".

Live traffic never writes a verdict. When an upstream answers a live request
with 404, the data plane asks the probe worker to look at that endpoint on that
route, and the worker's verdict — taken with the shared credential and the same
request builder the data plane uses — is what removes the candidate. One live
404 cannot tell "unsupported" from "wrong upstream name" or "being rolled", and
a verdict written on its strength would take a working route out of rotation
over a blip. Verdicts age: a sweeper re-probes red ones daily, so an upstream
that withdrew a model and brought it back is found out without anyone clicking.

On a provider the platform holds no credential for — organizations bring their
own — the worker cannot look at all. Such a provider's automatically probed
endpoints are published while unverified: hiding what the platform cannot see
would list none of those models, and the organizations that can reach them are
the only callers anyway.

## What is still rewritten

Pass-through is not "proxy the bytes and hope". A thin injection layer remains,
and it is deliberately short:

| Rewrite | Why |
|---|---|
| Authentication header | `Bearer`, `x-api-key` plus a version header, or `x-goog-api-key` — one per protocol |
| Model name | The catalog name maps to whatever the upstream calls it. On the Gemini protocol it is a path segment rather than a body field, so the substitution happens in the address and the body is left alone |
| `stream_options.include_usage` | Added only on OpenAI Chat Completions, because otherwise a streamed Chat response reports no usage at all |
| Usage parsing | Each protocol reports token buckets differently, so normalisation is per protocol |

Everything else is relayed unchanged, streams byte for byte.

Stateful response and interaction identifiers add one routing constraint but
no protocol conversion. FairLB records the exact route and exact shared or BYOK
credential that created an upstream resource. `previous_*_id`, retrieve,
cancel and delete operations are pinned to that tuple. If it is unavailable the
request returns `gateway.state_route_unavailable`; it never fails over into a
different upstream account where the identifier could mean something else.

## Deliberately separate products

Realtime/audio, Gemini Live, Responses WebSocket, Files, Vector Stores,
Containers, asynchronous Batch, video and top-level MCP/A2A are not inference
surface aliases. They require different connection lifetimes, retained
resources, settlement and security policies, so they stay out of this data
plane until each has an explicit product contract. Native requests may still
carry function calls, structured output, multimodal content and remote MCP tool
declarations unchanged.

Exactly two of those four touch the request body — the model name and the usage
option — and that is the whole list of rewrites the gateway performs on its own.
A provider may carry a **transport profile** ([providers.md](../providers.md))
which changes the address, how the credential is presented, the version header
and the connect bound.

A profile may also declare an **envelope**, and one envelope does move a value
into the body, so the boundary is worth stating precisely. It is not "no byte of
the body may change". It is:

> **Nothing in a transport profile may decide anything about the caller's
> request.**

Two hosted platforms publish this same Anthropic API with the request cut
differently: the model is a path segment rather than a body field, and the
api-version that is a header everywhere else is a body field there. Moving a
value between the envelope and the body is not a judgement call — the field is
required, its value is fixed by the platform, and there is exactly one place it
can go. Nothing is dropped, because nothing fails to correspond; nothing is
invented, because every value came from somewhere.

Translation is the opposite: every field that exists on one side and not the
other is a decision, and a wrong decision about a token count is a wrong bill.
The test to apply to any future field here is whether making it work would
require reading a message, a parameter or a tool definition. If it would, it is
translation, and it belongs in a different program.

## Counting tokens is per protocol, and the difference is not cosmetic

The two protocols report the same quantities with opposite conventions:

```text
OpenAI       details are a subset of the parent count
             uncached input = prompt_tokens - cached - cache_write

Anthropic    the two cache counts are in addition to input_tokens
             uncached input = input_tokens

Gemini       the cached count is a subset of promptTokenCount, as OpenAI's is,
             but thinking tokens are *outside* candidatesTokenCount, as
             Anthropic's cache counts are outside input:
             uncached input = promptTokenCount - cachedContentTokenCount
                            + toolUsePromptTokenCount
             output         = candidatesTokenCount + thoughtsTokenCount
```

The third protocol matches neither neighbour, which is the point of writing all
three down. It also reports no cache *write*: context caching there is billed by
storage duration rather than by tokens written, so there is nothing to report,
and a number in that bucket would be one this gateway invented.

Normalising with the wrong convention does not produce an obviously broken
number; it produces a plausible one that is wrong by exactly the cached amount.
There is a guard for this: if the subtraction goes negative, the upstream is not
actually reporting in the convention its protocol implies — relays that rewrite
usage exist — and the totals are recomputed with the additive convention.

The guard does not clamp to zero. Clamping would hide the disagreement and give
those tokens away for free, which is the same failure with a friendlier shape.
