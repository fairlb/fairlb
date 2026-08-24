import { chatBody, messagesBody, type Surface } from "./playground-surface";

/**
 * Exports the call the playground is about to make as a pastable curl command.
 *
 * The point of a playground is not to chat, it is to let someone **take the
 * settings they just tuned away with them**. Without this step the page is a toy:
 * you would still have to rewrite the request from the docs afterwards.
 *
 * The key is always a placeholder. The real one lives only in session storage and
 * **must not be baked into a piece of text that gets pasted into a chat window or a
 * support ticket without a second thought**.
 */
const KEY_PLACEHOLDER = "$FAIRLB_API_KEY";

/**
 * The origin of the data plane, derived from where the page itself is being served.
 * No domain is hard-coded anywhere.
 *
 * When the surfaces are split across `console.` / `api.` subdomains of one apex,
 * swapping this page's own prefix for `api.` *is* the gateway. With no such prefix
 * we are on a single host — the development setup, where the same server answers
 * both — and the same origin is the gateway; that is also the path the playground's
 * real calls take.
 *
 * Injecting this at build time does not work: the built assets are embedded in a
 * single binary and one build serves whatever domain it is deployed under, so the
 * domain is knowable only at runtime.
 *
 * Under a deployment that does not follow that convention this degrades to pointing
 * at the console's own host, which the data plane will not answer — wrong, but
 * visibly wrong. A hard-coded domain would instead point quietly at somewhere that
 * does not exist.
 */
export function gatewayOrigin(loc: { protocol: string; host: string }): string {
  const host = loc.host.startsWith("console.")
    ? `api.${loc.host.slice("console.".length)}`
    : loc.host;
  return `${loc.protocol}//${host}`;
}

export function curlSnippet(opts: {
  origin: string;
  surface: Surface;
  model: string;
  system: string;
  maxTokens: number;
  prompt: string;
}): string {
  const { origin, surface, model, system, maxTokens, prompt } = opts;
  const path = surface === "messages" ? "/v1/messages" : "/v1/chat/completions";
  const turns = [{ role: "user" as const, content: prompt }];
  // The snippet does not stream. Whoever pastes it is usually looking at the
  // response shape first, and a streamed response in a terminal is a wall of
  // server-sent-event frames rather than something to read.
  const body =
    surface === "messages"
      ? { ...messagesBody(model, turns, system, maxTokens), stream: false }
      : { ...chatBody(model, turns, system, maxTokens), stream: false };

  const headers = [
    `-H 'content-type: application/json'`,
    `-H "authorization: Bearer ${KEY_PLACEHOLDER}"`,
    ...(surface === "messages" ? [`-H 'anthropic-version: 2023-06-01'`] : []),
  ];

  return [
    `curl -X POST ${origin}${path} \\`,
    ...headers.map((h) => `  ${h} \\`),
    `  -d '${JSON.stringify(body, null, 2).replace(/'/g, `'\\''`)}'`,
  ].join("\n");
}
