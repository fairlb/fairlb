# Images

Image generation goes through the same native protocols as text. There is no
separate image API to learn, and no translation layer between your client and
the upstream.

That is worth stating because video works the other way, and the two look
similar from a distance. Video has its own contract precisely because no vendor
published one the others speak. Images are the opposite case: OpenAI's image
endpoints are implemented well beyond OpenAI, and Google reaches its image
models on the same Gemini endpoint as its text ones. Passing requests through
unchanged reaches the field, so that is what happens.

## Which endpoint

Two, and which one a model is on is a property of the model, not a choice you
make.

| Reached on | Reported as | Models |
|---|---|---|
| `POST /v1/images/generations` | `images` | Models served in OpenAI's image shape — including vendors other than OpenAI whose own API uses it |
| `POST /v1/images/edits` | `images_edits` | The subset of those that also serve a separate edits endpoint |
| `POST /v1beta/models/{model}:generateContent` | `generate_content` | Google's image models, which return image parts alongside text |

The two image paths are reported separately because they are separately true.
Several vendors serve generations and have no edits endpoint at all; they take
an input image on the generations call instead.

`GET /v1/models` reports all of it, per model: `fairlb.endpoints` says which
endpoints have been verified on it, and `fairlb.output_modalities` says what it
produces. The second is not derivable from the first — a Gemini image model and
a Gemini text model are reached identically — so filter on
`output_modalities` when you want "the image models".

## Generating

```bash
curl https://api.example.com/v1/images/generations \
  -H "Authorization: Bearer $FAIRLB_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openai/gpt-image-2",
    "prompt": "a cat asleep on a sunlit windowsill",
    "n": 1,
    "size": "1024x1024"
  }'
```

The request body is forwarded unchanged apart from the model name, which is
rewritten to whatever the chosen upstream calls it. A parameter this gateway
has never heard of reaches the upstream exactly as you wrote it.

## What you are charged

Image models are sold two different ways, and the catalogue tells you which
before you call:

- **Per token.** A generated image is reported as output tokens, priced well
  above text output. `pricing.billing_unit` is `token` and the four
  `*_per_mtok` rates apply.
- **Per image.** `pricing.billing_unit` is `image`, the token rates are
  **absent** rather than zero, and `pricing.unit_rates` carries the rate card:
  one row per case the price varies on, which for these models is the size and
  the quality tier.

Both live on the same endpoint. Read `billing_unit` first — for a per-image
model there is no token price to read, and treating its absence as zero would
tell you an image is free.

A per-image charge is **the number of images the response contains**, at the
rate for the size and quality you asked for. It is counted from the response
rather than from your request, because on several of these models the request
does not say: Seedream has no `n` at all, and a request that names no number
can come back with fifteen.

What happens before the call is the reservation, and it covers the most that
model can return in one response. So the amount reserved and the amount charged
are ordinarily different numbers, and the second is the one you pay. A size or
quality the model's rate card does not price is refused before anything is
reserved, rather than being served at some neighbouring row's rate.

**A per-image model cannot be streamed.** `"stream": true` is refused on it,
before anything is reserved. The images arrive in one response either way; what
a stream cannot give is a count of them that every vendor spells the same, and
this gateway does not bill you on a number it had to guess. Token-billed image
models stream normally.

## Editing

```bash
curl https://api.example.com/v1/images/edits \
  -H "Authorization: Bearer $FAIRLB_API_KEY" \
  -F model=openai/gpt-image-2 \
  -F image=@room.png \
  -F prompt="repaint the walls green"
```

This endpoint takes multipart rather than JSON, so the body is a stream with the
image in it. The gateway reads the small fields ahead — the model, and the ones
a per-image rate is looked up by — rewrites the model name to whatever the
chosen upstream calls it, and forwards the rest of the stream unread. Your
upload is never buffered and never touched.

Not every vendor has a separate edits endpoint. Several take an input image on
the generations call instead; for those, `fairlb.endpoints` lists `images` and
not `images_edits`. Read it before you call — that is what it is for.

## Where the image lives

`response_format` decides, and the difference is worth knowing before you build
on it.

- **`b64_json`** returns the bytes in the response. They are yours from that
  moment; nothing expires.
- **`url`** returns *the upstream's* address, forwarded to you exactly as it was
  given. How long it stays valid is that vendor's decision, not this gateway's —
  a day is common, and some vendors say nothing at all. If you need the image
  later, fetch it now.

gpt-image only ever returns base64, so on that model the question does not
arise.

This is the one place images and video differ in a way you can see. A video job's
artifact is served from this gateway, with a retention you can read; an image
response is passed through byte for byte, and passing it through is what makes
the rest of this page true — your request reaches the upstream unchanged, and
its answer reaches you unchanged. Rewriting the URLs in it would mean this
gateway had stored your images, which is a promise it does not make on this
endpoint.
