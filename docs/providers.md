# Connecting upstream providers

Recipes for pointing this gateway at the upstreams people actually use, and an
honest note on what each one costs you in metering accuracy.

Read [design/same-protocol-passthrough.md](design/same-protocol-passthrough.md)
first if you have not. The one rule that shapes everything below: an
OpenAI-, Anthropic- and Gemini-shaped requests each go to an upstream speaking
that same native protocol. There is no protocol translation, so a model answers
on a protocol only through a provider that speaks it — and on every protocol
its providers speak, because a model is not bound to one.

## What you configure

Four objects, in this order:

1. **Provider** — one upstream machine. Which vendor it belongs to, its base URL,
   an optional header mapping, an optional transport profile. Which protocols it
   speaks follows from the vendor; only a custom upstream has to say.
2. **Provider key** — a credential for that provider. Stored encrypted.
3. **Model** — a name in *your* catalog. It owns no protocol: the same slug is
   reachable on whichever protocols its routes' providers speak.
4. **Route** — model plus provider plus the name that upstream calls the model.
   Nothing about which endpoints it serves: the route is probed on every
   endpoint of the provider's protocols, and what the probes find is what the
   catalogue publishes. Then a price, because an unpriced model is refused
   rather than served for free.

Everything below is about step 1: getting the request to leave in the shape the
upstream expects.

## Vendors

Every provider states which API platform it belongs to. Choosing one in the
create form fills in the base URL, the protocols and the transport profile —
for the recipes on this page you should not have to type any of it. The list
this build knows is also served at `GET /gateway/vendors`.

Where a vendor publishes several endpoints, the form asks which one; a
mainland-China host and an international one are different endpoints taking
different credentials, not a mirror pair. Where its address contains a
placeholder — a resource name, a region — the form says so and the save is
refused until it is replaced, because a placeholder that reaches the data plane
fails as a DNS error and reads like an outage.

That is all the choice does. **The stored provider record is what the data plane
reads**, so editing it afterwards is normal and a later release changing a preset
never reaches a provider that already exists. What the vendor decides at runtime
is narrower than it looks:

- which organization-supplied credential may serve this provider (a key for one vendor
  is never sent to another company's endpoint);
- which vendor's entry the bundled reference prices are read from, which is what
  lets a provider you named after yourself still resolve to a price;
- whether the discovery button has anything to call — two of the platforms below
  publish no model catalogue at all, and being told so is more useful than an
  empty list that reads as "this upstream serves no models".

Nothing on the data plane branches on the vendor. Addressing differences go
through the transport profile, and the request body is untouched by both.

Pick **Custom** for an upstream with no entry of its own. It constrains nothing
and prefills nothing, which is exactly right for a compatible endpoint the
registry has never heard of — the recipes at the end of this page are all that
shape.

## The transport profile

Most upstreams need nothing here. A provider whose endpoints look like
`<base>/v1/chat/completions` and who wants a bearer token is fully described by
its base URL alone.

The transport profile is the escape hatch for the rest. It is a JSON object on
the provider record with these keys:

| Key | What it does |
|---|---|
| `auth` | How the credential is presented. `bearer` (default for the OpenAI protocol), `x-api-key` (default for the Anthropic protocol), `x-goog-api-key` (default for the Gemini protocol), `header:<Name>` to send it as a bare value under a header of your choosing, or one of the two *derived* modes below. |
| `anthropic_version` | The Anthropic API version this provider should receive. Where it travels depends on `envelope`: a header on a direct endpoint, a body field on the two hosted ones. Without it the gateway sends the right default for whichever applies. |
| `query` | Query parameters appended to every outbound URL, e.g. `{"api-version": "2024-10-21"}`. |
| `path_overrides` | Replaces the path the gateway would otherwise use. Keys are the gateway's own upstream paths; `{model}` in a value is substituted with the upstream model name from the route. |
| `stream_path_overrides` | The same, for streamed requests only, for upstreams that put streaming at a different address instead of behind a body flag. A path with no entry here falls back to `path_overrides`. |
| `envelope` | `bedrock` or `vertex`: the body framing a hosted platform requires. See below. |
| `sigv4` | `{"region": "...", "service": "..."}` — where an AWS request is signed for. Required with `auth: "aws_sigv4"` and refused with anything else. `service` defaults to `bedrock`. **No credential goes here**; this object is returned with the rest of the provider record. |
| `connect_timeout_ms` | How long this provider gets to accept a TCP connection on a forwarded request. It bounds the connection only, not the answer, and it does not apply to the admin test and discovery buttons, which run under their own overall timeout. |

Unknown keys and unknown values are **rejected when you save**, not ignored. A
setting that is accepted and then silently does nothing is the worst of the
three possible behaviours. That applies inside `sigv4` too: a misspelled key
there is refused rather than stored.

### Derived credentials

Two auth modes compute the credential per request instead of copying a stored
one, and each expects the provider key to be a **JSON credential document**
rather than a bare token:

| Mode | The provider key holds |
|---|---|
| `aws_sigv4` | `{"access_key_id": "...", "secret_access_key": "...", "session_token": "..."}` — the session token only for temporary credentials. Each request is signed with Signature Version 4. |
| `gcp_service_account` | The service-account key file, verbatim, as downloaded. It is exchanged for an access token, which the gateway refreshes before it expires. |
| `kling_jwt` | `{"access_key": "...", "secret_key": "..."}`. The platform takes a token the caller signs, and the one it accepts is valid for half an hour, so the gateway signs a fresh one for every request. A token pasted here instead of the pair works until it expires and then answers 401 on every request. |

The credential is stored encrypted with the provider's other keys and is never
returned by any endpoint. Nothing secret belongs in the transport profile: that
object is handed back with the provider record, so a key placed there would be
sent to a browser.

### Envelopes

Two platforms publish the Anthropic Messages API with the request cut
differently: the model is a path segment rather than a body field, and
`anthropic_version` is a body field rather than a header. Declaring an envelope
tells the gateway which of the two shapes to send.

| Value | What changes |
|---|---|
| `bedrock` | The model leaves the body, `anthropic_version` enters it, and the `stream` flag is dropped — that platform chooses streaming by endpoint and refuses a body that also carries the flag. The streamed response arrives as a binary frame protocol, which the gateway unwraps back into the SSE events it contains before forwarding. |
| `vertex` | The model leaves the body and `anthropic_version` enters it. Streaming stays a body flag and the response is ordinary SSE. |

An envelope is **declared, never inferred from `auth`**. The two are
independent: the same signature also fronts an OpenAI-shaped endpoint that wants
no envelope at all, and a token can front anything.

This is the one place a profile reaches the request body, and the rule it obeys
is in [design/same-protocol-passthrough.md](design/same-protocol-passthrough.md):
nothing in a profile may *decide* anything about your request. A required field
whose value the platform fixes has exactly one place it can go, and moving it
there is not a judgement call. A field that would need reading your messages to
place is protocol translation, and this gateway does not do that.

Stored profiles use the same strict schema as writes. An unknown key, wrong
type, invalid authentication mode, or invalid envelope makes readiness fail. If
a running process sees a bad hot update, it keeps serving with the last profile
it validated and reports the catalog unhealthy until the stored value is fixed.

The paths you may override:

```text
/v1/chat/completions   /v1/responses    /v1/embeddings
/v1/messages           /v1/models       /v1/images/generations   /v1/images/edits
```

`/v1/models` is the one the model-discovery button uses, so an upstream that
keeps its catalog somewhere else needs that entry too — without it, discovery
comes back empty, and "this provider serves no models" is a conclusion rather
than an error, so nothing sends you looking.

## Metering fidelity: read this before choosing a compatibility layer

Several vendors publish an "OpenAI-compatible" endpoint alongside their native
API. Those endpoints are convenient and they are also where token accounting
goes to die.

What is typically lost:

- **`prompt_tokens_details` is empty.** Cached input tokens are reported as
  ordinary input tokens, so a cache read is billed to the caller at the full
  input rate. On a workload with a large reused system prompt this is not a
  rounding error.
- **Cache writes are not reported at all.** They are never counted, in either
  direction.
- **Reasoning tokens** are sometimes folded into the completion count and
  sometimes omitted.

The gateway reports what the upstream reports. It cannot recover a distinction
the upstream never made, and it will not invent one — a guessed cache split is
an invented number on a bill.

So the rule is: **reach a model through its vendor's own protocol when you can.**
Claude through `/v1/messages` on an Anthropic-protocol provider, not through
somebody's OpenAI-compatible bridge. Nothing refuses the bridge — a route for a
Claude model on an OpenAI-protocol relay is accepted, and it is how that model
answers on `/v1/chat/completions` at all — but the fidelity rating tells you
what the bill will and will not see. Where a compatibility layer is the only
route, the recipes below say so, and the cost is stated per recipe.

The two hosted platforms are worth a sentence here, because the choice is real
and it is not obvious. Both publish Claude through an OpenAI-compatible bridge
*and* through the Anthropic protocol. The bridges rate **Totals only**; the
native recipes rate **Full**, and they exist precisely so that the convenient
answer is not also the expensive one. They cost a longer profile to set up, and
that is the whole difference.

The recipes below are also the vendor presets: picking the matching vendor
fills in exactly the base URL and profile each section shows, and the sections
remain here because they explain *why* each setting is what it is — and because
an upstream that has drifted from its preset is still configured by hand.

Fidelity ratings used below:

| Rating | Meaning |
|---|---|
| **Full** | Native protocol. Cache reads, cache writes and reasoning tokens are reported and billed as such. |
| **Partial** | Totals are correct; the cache split is absent, so cache reads are billed at full input price. |
| **Totals only** | Input and output totals only. Assume no cache accounting at all. |

---

## OpenAI

| | |
|---|---|
| Vendor | `openai` |
| Protocol | `openai` |
| Base URL | `https://api.openai.com` |
| Transport profile | none |
| Endpoints | chat, responses, embeddings, images |
| Fidelity | **Full** |

The reference case. Nothing to configure beyond the base URL and a key.

## Anthropic

| | |
|---|---|
| Vendor | `anthropic` |
| Protocol | `anthropic` |
| Base URL | `https://api.anthropic.com` |
| Transport profile | none, unless you need to pin a version |
| Endpoints | messages |
| Fidelity | **Full** |

The gateway sends an `anthropic-version` header of its own choosing. To pin a
different one for this provider:

```json
{ "anthropic_version": "2023-06-01" }
```

The client's own `anthropic-version` header is never forwarded — no client
header is, which is what keeps the gateway invisible to the upstream. This
setting is how you control that header.

## Azure OpenAI — v1 endpoint

The shape to prefer. Azure's v1 surface takes OpenAI-shaped paths directly, so
only the credential presentation differs.

| | |
|---|---|
| Vendor | `azure-openai` |
| Protocol | `openai` |
| Base URL | `https://<resource>.openai.azure.com/openai` |
| Transport profile | `{ "auth": "header:api-key" }` |
| Upstream model name | your **deployment** name, set on the route |
| Fidelity | **Full** |

The base URL ends at `/openai` because the gateway appends `/v1/chat/completions`
itself, which lands on `https://<resource>.openai.azure.com/openai/v1/chat/completions`.

`auth: "header:api-key"` sends the key as `api-key: <key>` and no bearer header
is written at all. The equivalent can also be written as a plain header mapping —
`api-key: ${api_key}` plus an empty `Authorization` to delete it — and that form
worked before the transport profile existed. Prefer the `auth` key: it is one
setting instead of two, and it cannot be half-applied.

If you authenticate with a directory token instead of a resource key, leave
`auth` at its default and store the token as the provider key; it goes out as
`Authorization: Bearer <token>`. The gateway has no token exchange for this
directory — it renews credentials only for the derived modes above — and a
stored token here stops working when it expires. Practical with a long-lived
token, not with an hour-long one.

**Organization-supplied keys.** A organization-supplied credential replaces the key and, if
one was given, the base URL — it does not replace the transport profile, which
stays with the candidate being called. Those credentials are matched by vendor,
which is what keeps that safe: a organization bringing their own Azure subscription is
only ever served by providers whose vendor is Azure, so the profile they meet is
an Azure profile. This install does not expose that path at all — see
[design/upstream-credentials.md](design/upstream-credentials.md) for why — but
the rule is worth knowing before you assume a key alone is enough to redirect a
organization.

## Azure OpenAI — deployment-path endpoint

The older shape, where the deployment name is in the path and an API version is
mandatory.

| | |
|---|---|
| Vendor | `azure-openai` (edit the profile) |
| Protocol | `openai` |
| Base URL | `https://<resource>.openai.azure.com` |
| Fidelity | **Full** |

```json
{
  "auth": "header:api-key",
  "query": { "api-version": "2024-10-21" },
  "path_overrides": {
    "/v1/chat/completions": "/openai/deployments/{model}/chat/completions",
    "/v1/embeddings":       "/openai/deployments/{model}/embeddings",
    "/v1/models":           "/openai/models"
  }
}
```

`{model}` is the upstream model name on the route, which for Azure is the
deployment name. The `/v1/models` entry is what makes model discovery work; the
`api-version` parameter reaches it too, because `query` applies to every
outbound request rather than only to the ones the data plane sends.

## Google Gemini

The vendor's own protocol, which is what the `google` preset configures.

| | |
|---|---|
| Vendor | `google` |
| Protocol | `gemini` |
| Base URL | `https://generativelanguage.googleapis.com` |
| Transport profile | none |
| Endpoints | generate_content |
| Fidelity | **Full** |

Nothing to configure beyond the base URL and a key. The model travels in the
address on this protocol rather than in the body, and streaming is a different
method name rather than a body flag; both are handled for you, and a caller's
request is passed through untouched.

Callers reach it at `/v1beta/models/<model>:generateContent`, and at
`:streamGenerateContent?alt=sse` for a stream. Gemini APIs are mounted only
under `/v1beta`; `/v1` remains the OpenAI/Anthropic data plane.

## Google Gemini, via the OpenAI-compatible endpoint

The alternative, for a caller that would rather use an OpenAI SDK. Configure it
as a second provider record.

| | |
|---|---|
| Vendor | `google` |
| Protocol | `openai` |
| Base URL | `https://generativelanguage.googleapis.com/v1beta/openai` |
| Fidelity | **Partial** |

```json
{
  "path_overrides": {
    "/v1/chat/completions": "/chat/completions",
    "/v1/embeddings":       "/embeddings",
    "/v1/models":           "/models"
  }
}
```

The overrides are needed because this endpoint's paths do not carry a `/v1`
segment of their own — the version is already in the base URL. Without them the
gateway would ask for `/v1beta/openai/v1/chat/completions` and get a 404.

Gemini's context caching is not reported through this layer, so cached input is
billed as ordinary input. That is the whole reason the native protocol above is
what the preset selects: the convenient answer here is also the more expensive
one for whoever is paying per token.

## AWS Bedrock — Anthropic models, natively

The native shape. Requests are signed, and the Messages API arrives in Bedrock's
own envelope.

| | |
|---|---|
| Vendor | `aws-bedrock` |
| Protocol | `anthropic` |
| Base URL | `https://bedrock-runtime.<region>.amazonaws.com` |
| Provider key | `{"access_key_id": "...", "secret_access_key": "..."}` |
| Upstream model name | the Bedrock model id, e.g. `anthropic.claude-sonnet-4-20250514-v1:0` |
| Fidelity | **Full** |

```json
{
  "auth": "aws_sigv4",
  "envelope": "bedrock",
  "sigv4": { "region": "us-east-1" },
  "path_overrides": {
    "/v1/messages": "/model/{model}/invoke"
  },
  "stream_path_overrides": {
    "/v1/messages": "/model/{model}/invoke-with-response-stream"
  }
}
```

Fidelity is **Full** because this is the vendor's own protocol: the response is
the Anthropic Messages response, cache counters included, so a cache read is
billed as a cache read.

Two addresses rather than one, because Bedrock chooses streaming by endpoint. It
also refuses a body carrying `stream`, and the envelope removes it for you — so
a caller's `"stream": true` still works exactly as it does everywhere else.

The streamed reply is a binary frame protocol rather than SSE. The gateway
unwraps it back into the events it carries; clients see ordinary SSE and need to
know nothing about this.

The signing region has to be stated. It cannot be read off the hostname, because
a private endpoint or a proxy in front of the service has a name that says
nothing about the region behind it — and a signature computed for the wrong
region fails as an authentication error, which sends you to look at the key.

Use a **temporary credential** if your setup can produce one: add
`"session_token"` alongside the other two. It travels in its own header and is
covered by the signature.

Which models a region serves varies — check before assuming a model id is
available. There is no `/v1/models` here, so model discovery comes back empty
and routes are created by hand.

## AWS Bedrock — the OpenAI-compatible endpoint

Still the simpler answer for the OpenAI-protocol models Bedrock hosts.

| | |
|---|---|
| Vendor | Custom |
| Protocol | `openai` |
| Base URL | `https://bedrock-runtime.<region>.amazonaws.com/openai` |
| Transport profile | none |
| Why Custom | A different base URL and no envelope, so it is a second provider record rather than a second protocol on the native one. The `aws-bedrock` preset describes the signed Anthropic endpoint. |
| Upstream model name | the Bedrock model id, e.g. `openai.gpt-oss-20b-1:0` |
| Fidelity | **Totals only** |

This endpoint takes an ordinary bearer API key, so no signing is involved and no
profile is needed. It also accepts a signature, if you would rather manage one
credential than two: set `auth` and `sigv4` as above and leave `envelope` unset —
the envelope belongs to the native Messages endpoint, not to this one.

No `/v1/models` here either. Create the routes by hand.

## Google Vertex AI — Anthropic models, natively

| | |
|---|---|
| Vendor | `google-vertex` |
| Protocol | `anthropic` |
| Base URL | `https://<region>-aiplatform.googleapis.com` |
| Provider key | the service-account key file, verbatim |
| Upstream model name | the publisher model id, e.g. `claude-sonnet-4@20250514` |
| Fidelity | **Full** |

```json
{
  "auth": "gcp_service_account",
  "envelope": "vertex",
  "path_overrides": {
    "/v1/messages": "/v1/projects/<project>/locations/<region>/publishers/anthropic/models/{model}:rawPredict"
  },
  "stream_path_overrides": {
    "/v1/messages": "/v1/projects/<project>/locations/<region>/publishers/anthropic/models/{model}:streamRawPredict"
  }
}
```

The project and the region are written into the paths because that is where
Vertex puts them; there is no separate setting for either. The region also
appears in the hostname, and the two must agree.

The service account needs the Vertex AI user role. The gateway exchanges the key
for an access token, and **refreshes it** — the token lasts about an hour, so a
gateway that fetched one and stopped there would work for an hour and then answer
401 on every request.

Streaming is ordinary SSE at the `:streamRawPredict` address; the envelope sets
the body's `stream` flag to match the address it is being sent to.

Fidelity is **Full**: this is the Anthropic Messages response, with its cache
counters intact.

There is no model catalog at this address, so discovery comes back empty and
routes are created by hand.

## Google Vertex AI — Gemini models, natively

A separate provider record from the Claude-on-Vertex one above. The two are
different APIs at different addresses: that one carries the Anthropic Messages
envelope, and this one must not, so one record cannot serve both.

| | |
|---|---|
| Vendor | Custom |
| Protocol | `gemini` |
| Base URL | `https://<region>-aiplatform.googleapis.com` |
| Provider key | the service-account key file, verbatim |
| Upstream model name | the model id, e.g. `gemini-2.5-flash` |
| Fidelity | **Full** |

```json
{
  "auth": "gcp_service_account",
  "path_overrides": {
    "/v1beta/models/{model}:generateContent": "/v1/projects/<project>/locations/<region>/publishers/google/models/{model}:generateContent"
  },
  "stream_path_overrides": {
    "/v1beta/models/{model}:streamGenerateContent": "/v1/projects/<project>/locations/<region>/publishers/google/models/{model}:streamGenerateContent"
  }
}
```

The project and the region are written into the paths because that is where
Vertex puts them, and the region appears in the hostname too, where the two must
agree. `auth` is set because this endpoint takes an exchanged access token
rather than an API key; the gateway refreshes it.

There is no model catalogue at this address, so discovery comes back empty and
routes are created by hand.

## OpenAI-compatible upstreams that need no profile

Each of these speaks the `openai` protocol at `<base>/v1/...`, so the base URL is
the whole configuration. The key goes in as a bearer token. The ones with a
vendor entry of their own are marked; the rest are configured as **Custom**.

| Upstream | Vendor | Base URL | Fidelity | Notes |
|---|---|---|---|---|
| DeepSeek | `deepseek` | `https://api.deepseek.com` | **Partial** | Has reported its cache hits under field names of its own rather than under `prompt_tokens_details`. See the note below the table. Also serves the Anthropic protocol at `/anthropic`, which the preset maps for you. |
| Qwen (DashScope) | `alibaba` | `https://dashscope.aliyuncs.com/compatible-mode` | **Totals only** | Use `dashscope-intl.aliyuncs.com` outside mainland China. The `/compatible-mode` suffix is required; the gateway appends `/v1/...`. |
| Moonshot (Kimi) | `moonshot` | `https://api.moonshot.cn` | **Totals only** | `api.moonshot.ai` for the international endpoint. Anthropic protocol at `/anthropic`. |
| Volcengine Ark (Doubao) | `volcengine` | `https://ark.cn-beijing.volces.com/api/v3` | **Unknown** | The version segment is in the base URL, so the preset strips it from the paths. The upstream model name is a model id or an endpoint id. |
| Baidu Qianfan (ERNIE) | `baidu` | `https://qianfan.baidubce.com/v2` | **Unknown** | Same shape: the version is in the base URL. |
| Tencent Hunyuan | `tencent` | `https://api.hunyuan.cloud.tencent.com` | **Unknown** | |
| MiniMax | `minimax` | `https://api.minimax.io` | **Unknown** | `api.minimaxi.com` in mainland China. Anthropic protocol at `/anthropic`. |
| xAI | `xai` | `https://api.x.ai` | **Unknown** | |
| Mistral | `mistral` | `https://api.mistral.ai` | **Unknown** | |
| Groq | `groq` | `https://api.groq.com/openai` | **Totals only** | |
| OpenRouter | `openrouter` | `https://openrouter.ai/api` | **Totals only** | A relay in front of many vendors. It reports its own accounting, not the underlying vendor's, and the two do not always agree. |
| Together | Custom | `https://api.together.xyz` | **Totals only** | |
| Ollama | Custom | `http://<host>:11434` | **Totals only** | Ignores the credential; store any non-empty string as the key. |
| vLLM | Custom | `http://<host>:8000` | **Totals only** | The key is whatever the server was started with. Serves one model per process unless configured otherwise. |
| LM Studio | Custom | `http://<host>:1234` | **Totals only** | Local development. Same shape as vLLM. |

An **Unknown** rating means nobody has established what that endpoint reports,
not that it reports nothing. The way to settle it is the same as for the rest:
send one request and read the usage row.

The ratings above are a starting point, not a promise about a vendor's current
release. What the gateway reads is fixed and knowable: `prompt_tokens_details`
and `completion_tokens_details` on the OpenAI protocol, the two cache counters on
the Anthropic one. Anything an upstream reports under names of its own is not
seen, and any of these vendors may add the standard fields — or drop them — in a
release nobody tells you about. Before you rely on the cache accounting for a
workload where it is worth money, send one request and look at the usage row.

## Zhipu (BigModel / Z.ai)

| | |
|---|---|
| Vendor | `zhipu` |
| Dialects | `openai`, `anthropic` |
| Base URL | `https://open.bigmodel.cn` — `https://api.z.ai` internationally |
| Fidelity | **Totals only** on the OpenAI protocol |

```json
{
  "path_overrides": {
    "/v1/chat/completions": "/api/paas/v4/chat/completions",
    "/v1/embeddings":       "/api/paas/v4/embeddings",
    "/v1/models":           "/api/paas/v4/models",
    "/v1/messages":         "/api/anthropic/v1/messages"
  }
}
```

The base URL stops at the host so that one record reaches both protocols: the
OpenAI one lives under `/api/paas/v4` and the Anthropic one under
`/api/anthropic`, and neither carries the version segment the gateway sends on
its own — the same reason Gemini needs overrides.

Its two hosts are two different entries in the bundled price dataset, so this
vendor deliberately resolves prices by base URL rather than by name: whichever
host you chose is the one that answers.

## One provider, several protocols

A relay that exposes both `/v1/chat/completions` and `/v1/messages` behind one
base URL and one key is **one provider record with two protocols declared**, not
two records. Two records would give one machine two independent sets of
credentials, health state, cooldowns and concurrency budget, and a machine with
two health verdicts is a wrong model before it is an inconvenience.

The outbound authentication header follows the protocol of the *incoming*
request, not the provider record, so a two-protocol provider sends a bearer token
on chat and `x-api-key` on messages without any extra configuration. A transport
profile's `auth` setting overrides that for both.

## What the account will take

A provider record can state its upstream account's limits: requests per minute,
tokens per minute, and how many calls may be in flight at once. The first two
are optional — leave them blank if the account publishes no limit — while the
concurrency cap always has a value, because past some number every upstream
stops answering and a gateway with no opinion queues until something times out.

They sit on the provider and not on a route because that is the shape a quota
has: a key is rated across everything it serves, not per model. Where a
provider serves several models, they all draw on the same allowance, which is
what the upstream is doing too.

**Two accounts are two provider records.** This is the same rule as the section
above, pointed the other way: one machine with one account is one record even
when it speaks several protocols, and one machine with two accounts on different
quotas is two records — each with its own credentials, its own health verdict,
its own share of the traffic, and now its own allowance. Adding a second key to
the same record shares one allowance between two credentials, which is right
when the two keys are on one account and wrong when they are not.

A provider with nothing left this minute is skipped for that request and the
next candidate is tried, exactly as a cooling-down one is. It does not take a
smaller share of the traffic instead — see
[design/failover-and-cooldowns.md](design/failover-and-cooldowns.md).

## Checking your work

- **Test the connection** from the provider's page. It sends one real, minimal
  request and reports what came back. With `FAIRLB_PROBE_TRACE=true` it also
  shows the exact bytes that went out, including the final URL and every header
  — which is the fastest way to find a base URL that is one path segment wrong.
  That trace contains the credential in clear text, which is why it is off by
  default and belongs on a machine you are setting up rather than a live one.
- **Discover models** lists what the upstream says it serves. An empty list
  usually means `/v1/models` is somewhere else — see `path_overrides`.
- A 404 from the upstream is almost always the URL, and a 401 is almost always
  the `auth` form. Both are reported with the upstream's own message attached.
