# Agent client configuration

FairLB keeps each vendor's native wire protocol. Replace
`https://gateway.example.com` and the example model slugs below with your
deployment and values returned by the relevant model catalog.

## Codex

Codex uses the Responses wire API. Put the key in the environment and add a
provider to `~/.codex/config.toml`:

```sh
export FAIRLB_API_KEY="fairlb-key"
```

```toml
model = "openai/gpt-5.4"
model_provider = "fairlb"

[model_providers.fairlb]
name = "FairLB"
base_url = "https://gateway.example.com/v1"
env_key = "FAIRLB_API_KEY"
wire_api = "responses"
```

The selected route must advertise `responses` and `responses_compact` for
long-session compaction. Current Codex releases no longer support a Chat
Completions wire provider. `GET /v1/models` is the OpenAI-shaped discovery
endpoint.

## Claude Code

Claude Code sends the native Anthropic Messages protocol. The auth-token form
uses `Authorization: Bearer`, which FairLB accepts; `ANTHROPIC_API_KEY` using
`x-api-key` is also accepted.

```sh
export ANTHROPIC_BASE_URL="https://gateway.example.com"
export ANTHROPIC_AUTH_TOKEN="fairlb-key"
claude --model "anthropic/claude-sonnet-4"
```

The selected route should advertise `messages` and
`messages_count_tokens`. Tool calls, content blocks, prompt caching, extended
thinking and streaming events remain in Anthropic's native shape.

## Gemini CLI

Use Gemini API-key authentication, keep the base URL at the host root, and use
the FairLB catalog slug as the model name:

```sh
export GEMINI_API_KEY="fairlb-key"
export GOOGLE_GEMINI_BASE_URL="https://gateway.example.com"
export GOOGLE_GENAI_API_VERSION="v1beta"
export GEMINI_MODEL="google/gemini-2.5-flash"
gemini
```

Gemini CLI discovers native metadata from `GET /v1beta/models`; the returned
`supportedGenerationMethods` is derived from the routes available to this key.
Streaming uses `:streamGenerateContent?alt=sse`. Do not append `/v1beta` to
`GOOGLE_GEMINI_BASE_URL`, because the SDK adds the API version.

## OpenCode

For Responses models, use the OpenAI provider package. This example keeps the
key out of the config file:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "fairlb": {
      "npm": "@ai-sdk/openai",
      "name": "FairLB Responses",
      "options": {
        "baseURL": "https://gateway.example.com/v1",
        "apiKey": "{env:FAIRLB_API_KEY}"
      },
      "models": {
        "openai/gpt-5.4": {
          "name": "GPT-5.4 via FairLB"
        }
      }
    }
  },
  "model": "fairlb/openai/gpt-5.4"
}
```

For a route intentionally using `/v1/chat/completions`, change `npm` to
`@ai-sdk/openai-compatible`. Available IDs can be copied from
`GET /v1/models`.

## Capability and pass-through rules

- A protocol name never enables every operation. Each route explicitly lists
  its verified operations, and new operations default to disabled.
- Token-count utilities are authenticated, model-admitted, rate-limited and
  audited, but do not create a consumption charge.
- Stored Responses and Interactions stay on the exact route and exact shared or
  organization credential that created them. A missing ID is 404; an unavailable
  original route is `503 gateway.state_route_unavailable`.
- Unknown fields, structured output, multimodal content, function/tool calls
  and remote MCP tool declarations are passed through in the native protocol.
- Realtime/audio, Files, Vector Stores, Containers, asynchronous Batch, video,
  top-level MCP/A2A and cross-protocol translation are not data-plane APIs.
