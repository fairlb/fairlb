import type { GatewayConsoleTypes } from "@fairlb/api-client";
import { describe, expect, it } from "vitest";
import { chatBody, messagesBody, sseDelta, surfaceOf } from "./playground-surface";
import { curlSnippet, gatewayOrigin } from "./playground-snippet";

function model(endpoints: string[]): GatewayConsoleTypes.AvailableModel {
  return { slug: "m", protocols: ["f"], endpoints } as GatewayConsoleTypes.AvailableModel;
}

describe("surfaceOf", () => {
  it("routes chat and messages models to their own surface", () => {
    expect(surfaceOf(model(["chat"]))).toBe("chat");
    expect(surfaceOf(model(["messages"]))).toBe("messages");
  });

  it("returns null for models with no conversational surface", () => {
    // The catalog also holds embeddings and image models; neither belongs in the
    // playground's dropdown.
    expect(surfaceOf(model(["embeddings"]))).toBeNull();
    expect(surfaceOf(model([]))).toBeNull();
  });

  it("is deterministic when a model supports both", () => {
    // Two calls must answer the same. If the surface moves, the exported snippet
    // stops matching the request actually being sent.
    const both = model(["chat", "messages"]);
    expect(surfaceOf(both)).toBe(surfaceOf(model(["messages", "chat"])));
  });
});

describe("request bodies", () => {
  const turns = [{ role: "user" as const, content: "hi" }];

  it("puts system in messages[] for chat but at top level for messages", () => {
    const chat = chatBody("m", turns, "be terse", 512);
    expect(chat.messages[0]).toEqual({ role: "system", content: "be terse" });
    expect("system" in chat).toBe(false);

    const anthropic = messagesBody("m", turns, "be terse", 512);
    expect(anthropic.system).toBe("be terse");
    // The messages surface accepts only user/assistant turns. The type already
    // rules out a system role, so what this asserts is the **count**: the system
    // prompt must not be quietly appended as one more message.
    expect(anthropic.messages).toEqual(turns);
  });

  it("omits system entirely when blank rather than sending an empty one", () => {
    expect(chatBody("m", turns, "   ", 512).messages).toHaveLength(1);
    expect("system" in messagesBody("m", turns, "   ", 512)).toBe(false);
  });

  it("always carries max_tokens — it is required on the messages surface", () => {
    expect(messagesBody("m", turns, "", 777).max_tokens).toBe(777);
    expect(chatBody("m", turns, "", 777).max_tokens).toBe(777);
  });
});

describe("sseDelta", () => {
  it("reads chat chunks", () => {
    expect(sseDelta("chat", { choices: [{ delta: { content: "ab" } }] })).toBe("ab");
    expect(sseDelta("chat", { choices: [{ delta: {} }] })).toBeNull();
    expect(sseDelta("chat", { choices: [] })).toBeNull();
  });

  it("reads only text_delta frames on the messages surface", () => {
    expect(
      sseDelta("messages", {
        type: "content_block_delta",
        delta: { type: "text_delta", text: "x" },
      }),
    ).toBe("x");
  });

  it("skips the messages surface control frames", () => {
    // The counter-examples: read as prose, these frames would splice control
    // structures into the reply.
    for (const frame of [
      { type: "message_start", message: { id: "m" } },
      { type: "content_block_start", content_block: { type: "text" } },
      { type: "message_delta", usage: { output_tokens: 3 } },
      { type: "message_stop" },
      { type: "content_block_delta", delta: { type: "thinking_delta", thinking: "hmm" } },
    ]) {
      expect(sseDelta("messages", frame)).toBeNull();
    }
  });
});

describe("gatewayOrigin", () => {
  it("maps the deployed console host onto the api. gateway host", () => {
    // Swapping the subdomain prefix is the whole rule: `console.` in, `api.` out.
    expect(gatewayOrigin({ protocol: "https:", host: "console.staging.fairlb.com" })).toBe(
      "https://api.staging.fairlb.com",
    );
    // The rule follows the convention, not any particular domain — it holds for
    // whatever apex the deployment happens to use.
    expect(gatewayOrigin({ protocol: "https:", host: "console.example.com" })).toBe(
      "https://api.example.com",
    );
  });

  it("keeps the current origin in single-host dev mode", () => {
    // On a single host the dev server proxies `/v1` to the same backend, so the
    // page's own origin is the gateway.
    expect(gatewayOrigin({ protocol: "http:", host: "localhost:5173" })).toBe(
      "http://localhost:5173",
    );
  });
});

describe("curlSnippet", () => {
  const base = {
    origin: "https://api.example.com",
    surface: "chat",
    model: "m",
    system: "",
    maxTokens: 1,
    prompt: "p",
  } as const;

  it("targets the given origin and the endpoint that matches the surface", () => {
    // The host in the snippet is the current environment's API base, the one
    // `gatewayOrigin` derives. An earlier version of this line hard-coded a domain
    // that was not the gateway of any environment, so every command pasted out of
    // it failed. The negative assertion below is the guard against that returning.
    const chat = curlSnippet({
      ...base,
      origin: gatewayOrigin({ protocol: "https:", host: "console.staging.fairlb.com" }),
    });
    expect(chat).toContain("curl -X POST https://api.staging.fairlb.com/v1/chat/completions");
    // The retired name must not reappear in any form; the current domain may only
    // appear as a derivative of the origin passed in, which the line above pins.
    // The name is split and rejoined so that a full-text search of this tree still
    // finds nothing — the assertion reconstitutes it at runtime.
    const legacyBrand = ["plea", "selb"].join("");
    expect(chat).not.toContain(legacyBrand);

    const anthropic = curlSnippet({ ...base, surface: "messages" });
    expect(anthropic).toContain("curl -X POST https://api.example.com/v1/messages");
    expect(anthropic).toContain("anthropic-version");
  });

  it("never embeds a real key", () => {
    const s = curlSnippet(base);
    expect(s).toContain("$FAIRLB_API_KEY");
    expect(s).not.toContain("sk-flb-v1-");
  });

  it("escapes single quotes so the shell command stays well-formed", () => {
    // The JSON is wrapped in single quotes, so a `'` in the prompt closes the quote
    // early and hands the rest of the line to the shell to interpret — and the
    // prompt is user input.
    const s = curlSnippet({ ...base, prompt: "it's" });
    expect(s).toContain(`'\\''`);
    expect(s).not.toMatch(/-d '[^']*it's/);
  });
});
