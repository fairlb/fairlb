# FairLB documentation

FairLB is a multimodal AI model gateway you run yourself: native OpenAI,
Anthropic and Gemini APIs for text and images, its own asynchronous job API for
video, in front of several upstream providers -- with failover, per-key budgets
and usage accounting.

## Start here

| Page | What it answers |
|---|---|
| [agent-clients.md](agent-clients.md) | Pointing Codex, Claude Code and the Gemini CLI at this gateway |
| [images.md](images.md) | Generating and editing images on the native protocols, and the two ways they are priced |
| [video.md](video.md) | Submitting a video job, polling it, fetching the result, what it costs, and how to switch from a vendor's own video API |
| [architecture.md](architecture.md) | What the process does with a request, and what it stores |
| [providers.md](providers.md) | Connecting each upstream, and what each one costs in metering accuracy |
| [configuration.md](configuration.md) | Every environment variable, what it does, what it defaults to |
| [deployment.md](deployment.md) | Running it behind a proxy, backing it up |
| [development.md](development.md) | Building it, generating code, running the tests |

## Why a decision was made

The pages under [design/](design/) are the long-form arguments behind choices
that look arbitrary until you know what the alternative cost. They are written
for someone deciding whether this design fits their situation, and for anyone
about to change it.

| Page | The question it settles |
|---|---|
| [design/same-protocol-passthrough.md](design/same-protocol-passthrough.md) | Why native protocols are passed through instead of translated |
| [design/upstream-credentials.md](design/upstream-credentials.md) | Which upstream credential a request uses, and why there is one place to enter them |
| [design/key-budgets.md](design/key-budgets.md) | Why a key budget refuses the request rather than letting it through |
| [design/reference-prices.md](design/reference-prices.md) | Where the bundled prices come from, and why nothing ever syncs them for you |
| [design/failover-and-cooldowns.md](design/failover-and-cooldowns.md) | How failing upstreams are detected, retried, and taken out of rotation |
| [design/idempotency.md](design/idempotency.md) | What `Idempotency-Key` guarantees, and what it deliberately does not |
| [design/video-jobs.md](design/video-jobs.md) | Why the video plane normalises parameters when the inference plane refuses to, and how a job's money is kept straight |

## Conventions in these pages

Money is stored as an integer number of nano-units plus a currency, never as a
float. Timestamps are `timestamptz` and everything reasons in UTC. Prices are
quoted per million tokens, as the providers quote them.
