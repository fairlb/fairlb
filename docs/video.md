# Video

Video generation is a job, not a request. You submit one, it runs for a minute
or two, and you fetch the result afterwards.

That is why it is a separate API rather than another endpoint on the inference
data plane. The three protocols this gateway speaks natively — OpenAI,
Anthropic and Gemini — are protocols because their vendors published them and
others adopted them. No video vendor has published one the others speak, so
there is nothing to pass through faithfully. This gateway publishes its own
contract instead, and maps it onto each upstream.

**Already writing against one vendor's video API?** You do not have to learn
this one. Five of them are answered at their own paths under
`https://api.example.com/video/<vendor>` — change your base URL and your key,
and nothing else. See [Switching from a vendor's own API](#switching-from-a-vendors-own-api).

## Submitting

```bash
curl https://api.example.com/v1/videos \
  -H "Authorization: Bearer $FAIRLB_API_KEY" \
  -H "Idempotency-Key: $(uuidgen)" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "google/veo-3.1",
    "prompt": "a cat asleep on a sunlit windowsill",
    "duration_seconds": 8,
    "resolution": "720p",
    "aspect_ratio": "16:9"
  }'
```

```json
{
  "id": "vid_01a03c62-27e6-7b63-94f8-b526a686000d",
  "object": "video",
  "model": "google/veo-3.1",
  "status": "in_progress",
  "progress": 0,
  "duration_seconds": 8,
  "resolution": "720p",
  "created_at": 1756209600,
  "expires_at": 1756814400
}
```

**`Idempotency-Key` is required.** Submitting is a paid create, and a retry
without one would be charged as a second job. Repeat the same key and you get
the same job back; repeat it with a different body and the request is refused
rather than answered with somebody else's video.

## Parameters

The whole vocabulary, and it is closed — anything else is refused rather than
ignored:

| Field | Notes |
|---|---|
| `model` | A catalog slug, e.g. `google/veo-3.1` |
| `prompt` | What to generate |
| `negative_prompt` | What to keep out. Not every model accepts one |
| `duration_seconds` | The clip length, and the primary billing quantity |
| `resolution` | `480p`, `720p`, `1080p`, `4k` — the quality tier |
| `aspect_ratio` | `16:9`, `9:16`, `1:1`, and more on some models |
| `audio` | Absent means whatever the model does by default |
| `image` | First frame, for image-to-video |
| `last_frame` | Frame to interpolate towards |
| `reference_images` | `[{"url": "...", "type": "asset" \| "style"}]` |
| `seed`, `n` | |

**Values differ far more than field names do.** Duration is 4, 6 or 8 seconds
on one model, 5 or 10 on another, 4, 8 or 12 on a third. Aspect ratio is two
values on one and seven on another. There is no value here that roughly means
the same thing everywhere, which is why nothing is clamped: asking for twelve
seconds from a model that tops out at eight is an error, never a shortened clip
billed for twelve.

Ask what a model accepts rather than finding out by trial — on this API every
attempt is a real charge:

```bash
curl https://api.example.com/v1/videos/models \
  -H "Authorization: Bearer $FAIRLB_API_KEY"
```

Each entry carries the admissible durations, resolutions and aspect ratios,
whether it takes a negative prompt, an input image or an end frame, whether it
can be cancelled, and its rate card.

### The set above is the whole set

A parameter this API does not recognise is refused, not ignored. That is
deliberate and it is about the bill: a gateway that quietly dropped an
unrecognised field would generate a clip you did not ask for, charge you for it,
and leave you no way to find out.

So a knob belonging to exactly one upstream — a particular vendor's camera
control, guidance scale or safety policy — is not accepted here, and there is no
passthrough field for it either. Use the model's own admissible set, which
`GET /v1/videos/models` publishes.

## Polling and fetching

```bash
curl https://api.example.com/v1/videos/vid_... \
  -H "Authorization: Bearer $FAIRLB_API_KEY"
```

`status` is `queued`, `in_progress`, `completed`, `failed`, `canceled` or
`expired`. Polling is a plain read and never drives the upstream, so poll as
often as suits you; the gateway tracks the job on its own schedule regardless
of whether anyone is watching.

When it is `completed`:

```bash
curl https://api.example.com/v1/videos/vid_.../content \
  -H "Authorization: Bearer $FAIRLB_API_KEY" -o out.mp4
```

The bytes come from this gateway, never as a redirect to the upstream. Past its
retention window the request answers `410` with `gateway.artifact_gone`, which
is a normal outcome rather than a fault.

## Cancelling

`POST /v1/videos/{id}/cancel`, where the model supports it. It genuinely
differs: one vendor can stop a job at any point, another only while it is still
queued, a third not at all. `GET /v1/videos/models` reports which, and a model
that cannot be stopped says so with `gateway.job_not_cancelable` rather than
accepting a cancel that does nothing.

Whatever the vendor does, a cancelled job is not charged.

## What it costs

Per second of output, by resolution and whether there is audio — not per token.
The charge is a function of the parameters you sent, so it is known before the
upstream is called at all, and the amount reserved when you submit is the amount
settled when it finishes.

**A job that produces nothing is not charged.** That includes a refusal by the
upstream's own content policy, which is a normal outcome on this API rather than
an edge case. The reason comes back on the job:

```json
{
  "id": "vid_...", "status": "failed",
  "error": {"code": "gateway.video_content_rejected", "message": "..."}
}
```

## Errors

| Code | Meaning |
|---|---|
| `gateway.video_params_unsupported` | A parameter outside the contract, or a value this model does not accept. The message names what it does accept |
| `gateway.job_not_cancelable` | This model cannot stop this job |
| `gateway.job_not_ready` | Asked for the content of a job that has not produced any |
| `gateway.artifact_gone` | Past its retention window |
| `gateway.model_unpriced` | The operator has not finished pricing this model. Nothing is charged |

## Switching from a vendor's own API

If your code is already written against Veo, Kling, Seedance, Hailuo or Wan,
point it here instead:

| You were calling | Set your base URL to |
|---|---|
| Gemini API (Veo) | `https://api.example.com/video/google` |
| Kling open platform | `https://api.example.com/video/kuaishou` |
| Volcengine Ark (Seedance) | `https://api.example.com/video/volcengine` |
| MiniMax (Hailuo) | `https://api.example.com/video/minimax` |
| Model Studio (Wan) | `https://api.example.com/video/alibaba` |

Then put your FairLB key where you put that vendor's key. Whichever header
their SDK uses — `Authorization: Bearer`, `x-api-key`, `x-goog-api-key` — is
accepted, so that line does not change either.

Everything else stays as it is: their paths, their request bodies, their
response shapes, their status words, their error codes. Their own parameters
that this API does not have — camera control, a person-generation policy, a
prompt extender — are sent on as you wrote them.

**What changes, and what it means.** The job is this gateway's, not the
vendor's. So:

- **The identifier you get back is ours.** It sits exactly where that vendor's
  task id goes and it is an opaque string, the same as theirs. Where a vendor's
  schema says that identifier is an integer, you get an integer.
- **The download address is ours.** It sits where that vendor's video URL goes.
  Fetching it works the same way; the file is served by this deployment rather
  than by the upstream, and it lasts for the retention window above rather than
  for whatever the upstream happened to keep.
- **You get this gateway's model catalogue.** Send the model name that vendor
  uses; if this deployment has it wired, that is the model you get.
- **Repeating a request within a minute returns the job it already made.**
  Those APIs have no idempotency header, so one is derived from your request —
  which is what stops a network-layer retry becoming a second charge. The same
  request a minute later is a second job. Your `X-End-User-Id` is part of that
  key, so two of your users sending the same prompt at the same moment get two
  jobs. If you need exact control, use `POST /v1/videos` and send your own
  `Idempotency-Key`.
- **A field we do not recognise is forwarded, or the request is refused naming
  it.** It is never dropped: a parameter quietly discarded produces a clip you
  did not ask for and are billed for anyway. Vendor-specific knobs travel as you
  wrote them; the few places we refuse instead are noted in the API reference.
- **A path this gateway does not serve is refused in that vendor's error
  shape**, saying which path and where to go instead. It is not a bare 404.

**It is not a passthrough**, and it is worth being plain about why rather than
letting you find out. These APIs are all asynchronous. Forwarding your submit
would create a job on the upstream that this gateway has no record of — no hold
on your balance, no settlement, no usage line, and nothing to refund if it
fails. Forwarding the poll would hand you the upstream's own URL, which this
gateway does not return. So the shapes are theirs and the job is ours, and the
two places that touch are both opaque strings.

Admission, your balance, the exact hold taken before generation starts, the
refund on a failed job, the usage line and custody of the finished file all
work exactly as they do on `/v1/videos`.
