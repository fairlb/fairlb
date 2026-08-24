import type { GatewayConsoleTypes } from "@fairlb/api-client";

/**
 * Which chat surface the playground speaks, and the three places the two differ.
 *
 * The data plane exposes two conversational endpoints: the OpenAI-compatible
 * `/v1/chat/completions` and the Anthropic-compatible `/v1/messages`. A playground
 * that only spoke the first one and filtered models by `endpoints.includes("chat")`
 * could try none of the models that only offer the second, and the catalog's "try
 * it" column rendered a dead `—` for every one of them.
 *
 * The two surfaces differ in exactly three places, collected here so they can be
 * tested without a browser:
 *   1. request body shape (`system` as a top-level field or as a message)
 *   2. whether `max_tokens` is required or optional
 *   3. streaming frame shape (`choices[].delta.content` or `content_block_delta`)
 */
export type Surface = "chat" | "messages";

export type Turn = { role: "user" | "assistant"; content: string };

/**
 * Which surface a model should be tried on; null when it offers neither, meaning
 * it cannot be tried at all.
 *
 * When a model offers both, chat wins. **The point is determinism**, not that one
 * is better: a model that landed on a different surface between two renders would
 * make the code snippet the page hands out contradict itself.
 */
export function surfaceOf(m: GatewayConsoleTypes.AvailableModel): Surface | null {
  if (m.endpoints.includes("chat")) return "chat";
  if (m.endpoints.includes("messages")) return "messages";
  return null;
}

/** The OpenAI-compatible request body: the system prompt is one of the messages. */
export function chatBody(model: string, turns: Turn[], system: string, maxTokens: number) {
  const messages: { role: string; content: string }[] = [];
  if (system.trim()) messages.push({ role: "system", content: system });
  for (const t of turns) messages.push({ role: t.role, content: t.content });
  return { model, stream: true, max_tokens: maxTokens, messages };
}

/**
 * The Anthropic-compatible request body: the system prompt is a **top-level field**,
 * not a message role.
 *
 * `max_tokens` is required on this surface, and omitting it earns a 400 whose text
 * reads like "the model is broken" rather than "you left out a field".
 */
export function messagesBody(model: string, turns: Turn[], system: string, maxTokens: number) {
  return {
    model,
    stream: true,
    max_tokens: maxTokens,
    ...(system.trim() ? { system } : {}),
    messages: turns.map((t) => ({ role: t.role, content: t.content })),
  };
}

/**
 * The text increment carried by one streamed frame; null for frames that carry none.
 *
 * On the chat surface every frame is a chunk, and the text sits at
 * `choices[0].delta.content`. The messages surface is an **event stream**, where
 * only `content_block_delta` frames whose `delta.type` is `text_delta` carry text —
 * `message_start`, `message_delta` and `message_stop` must be skipped, or control
 * frames get concatenated into the reply as if they were prose.
 */
export function sseDelta(surface: Surface, frame: unknown): string | null {
  const f = frame as Record<string, unknown>;
  if (surface === "chat") {
    const choices = f?.choices as { delta?: { content?: unknown } }[] | undefined;
    const content = choices?.[0]?.delta?.content;
    return typeof content === "string" ? content : null;
  }
  if (f?.type !== "content_block_delta") return null;
  const delta = f.delta as { type?: unknown; text?: unknown } | undefined;
  if (delta?.type !== "text_delta") return null;
  return typeof delta.text === "string" ? delta.text : null;
}
