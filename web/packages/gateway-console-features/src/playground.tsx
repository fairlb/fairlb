import { gatewayConsoleApi, type GatewayConsoleTypes, apiErrorMessage } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  PageHeader,
  Alert,
  Button,
  Card,
  CopyAction,
  Field,
  FormColumn,
  FormRow,
  InlineEmpty,
  Input,
  LoadingState,
  SectionHeading,
  Select,
  Textarea,
  formatNano,
  Forbidden,
} from "@fairlb/ui";
import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { chatBody, messagesBody, sseDelta, surfaceOf, type Surface } from "./playground-surface";
import { curlSnippet, gatewayOrigin } from "./playground-snippet";
import { OrgNotFound, useCanAccess, useConsoleTitle, useOrg } from "./host";

/**
 * The playground: a route of its own, not a card that expands inside the model
 * catalog.
 *
 * A catalog is a **looking-things-up** task and a playground is a
 * **doing-things** task. Sharing a page meant sharing one scroll position and one
 * document title, with the table still holding the screen below while you typed.
 * Split apart, choosing the model becomes this page's own control and `?model=` is
 * deep-linkable — which is exactly what each catalog row's "try it" uses.
 *
 * The playground **calls the data plane like anyone else**: the same key, the same
 * gates, the same billing. It therefore adds no backend of its own.
 *
 * **It speaks both surfaces.** Calling only `/v1/chat/completions` and filtering
 * models by `endpoints.includes("chat")` left every messages-only model untryable,
 * and rendered a dead `—` for them in the catalog's last column. The data plane had
 * `/v1/messages` all along; only the dispatch here was missing.
 */
export function PlaygroundPage() {
  const { t } = useI18n();
  const { orgId = "" } = useParams({ strict: false }) as { orgId?: string };
  const org = useOrg(orgId);
  const canAccess = useCanAccess();
  useConsoleTitle(org ? t("playTitle") : undefined);
  if (!org) return <OrgNotFound />;
  if (!canAccess(org, "/playground")) return <Forbidden />;
  // The `key` is a synchronous isolation boundary: changing organization remounts
  // the whole content tree, so form drafts, the conversation, errors, request ids
  // and the session key cannot flash into the new organization and then wait for an
  // effect to clean them up.
  return <PlaygroundDetail key={org.id} orgId={org.id} canManageKeys={canAccess(org, "/keys")} />;
}

function PlaygroundDetail({ orgId, canManageKeys }: { orgId: string; canManageKeys: boolean }) {
  const { t } = useI18n();
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as { model?: string };
  const models = gatewayConsoleApi.useListAvailableModels(orgId);
  // Tryable means it has a chat or a messages surface. The catalog also holds
  // embeddings and image models, which are not conversational — picking one would
  // produce a request that cannot be sent.
  const tryable = (models.data?.items ?? []).filter((m) => surfaceOf(m) !== null);
  // A model named in the URL is checked against the available list first: it may
  // have been retired since the link was made, and sending the request anyway
  // surfaces an upstream error nobody can interpret.
  const selected =
    search.model && tryable.some((m) => m.slug === search.model) ? search.model : tryable[0]?.slug;
  const model = tryable.find((m) => m.slug === selected);

  return (
    // A form plus its output: short lines, tall content, so it reads in a column.
    <FormColumn className="space-y-6">
      <PageHeader title={t("playTitle")} description={t("playgroundDesc")} />

      {models.isError ? (
        <Alert>{apiErrorMessage(models.error)}</Alert>
      ) : models.isPending ? (
        <Card>
          <LoadingState label={t("loading")} />
        </Card>
      ) : tryable.length === 0 ? (
        <Card>
          <InlineEmpty title={t("playNoModels")} description={t("playNoModelsHint")} />
        </Card>
      ) : (
        <>
          <Select
            label={t("playModelLabel")}
            className="max-w-md"
            value={selected}
            onValueChange={(v) => {
              // The select allows clearing, which calls back with null; there is no
              // "no model" state here, so ignore it.
              if (!v) return;
              void navigate({
                to: ".",
                search: (prev: Record<string, unknown>) => ({ ...prev, model: v }),
                replace: true,
              });
            }}
            items={tryable.map((m) => ({
              value: m.slug,
              label: m.display_name ? `${m.slug} · ${m.display_name}` : m.slug,
            }))}
          />
          {model && <Playground orgId={orgId} model={model} canManageKeys={canManageKeys} />}
        </>
      )}
    </FormColumn>
  );
}

type Turn = { role: "user" | "assistant"; content: string };

/** One storage key per organization, so keys never leak across them. */
const PLAYGROUND_KEY_PREFIX = "flb.playground.key";

function playgroundStorageKey(orgId: string): string {
  return `${PLAYGROUND_KEY_PREFIX}:${orgId}`;
}

function readPlaygroundKey(orgId: string): string {
  try {
    return window.sessionStorage.getItem(playgroundStorageKey(orgId)) ?? "";
  } catch {
    return "";
  }
}

function writePlaygroundKey(orgId: string, value: string): void {
  try {
    window.sessionStorage.setItem(playgroundStorageKey(orgId), value);
  } catch {
    // What was typed still works for this session; it simply will not be restored
    // on the next visit.
  }
}

/**
 * The key is held in session storage only: closing the tab discards it, it never
 * reaches local storage, and it is never sent back to this application's own API.
 *
 * **A real API key is required; the signed-in session must not stand in for one.**
 * If a session could drive these calls, the playground would bypass per-key billing
 * along with every per-key control — budgets, rate limits, the allowed-model list —
 * which is a path through the system that serves traffic and charges for none of it.
 */
function Playground({
  orgId,
  model,
  canManageKeys,
}: {
  orgId: string;
  model: GatewayConsoleTypes.AvailableModel;
  canManageKeys: boolean;
}) {
  const { t } = useI18n();
  const [apiKey, setApiKey] = useState(() => readPlaygroundKey(orgId));
  // The initial value is not a translated message: it is example input, not
  // interface text, and having it change with the language reads as "the thing I
  // just typed is gone".
  const [prompt, setPrompt] = useState("");
  const [system, setSystem] = useState("");
  // The messages surface **requires** max_tokens; the chat surface treats it as
  // optional. Defaulting it rather than leaving it blank, because blank makes every
  // messages-only model answer 400 on the first send, with an error that reads like
  // "the model is broken".
  const [maxTokens, setMaxTokens] = useState("1024");
  const [turns, setTurns] = useState<Turn[]>([]);
  const [streaming, setStreaming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [lastRequestId, setLastRequestId] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const surface = surfaceOf(model)!;
  const saveKey = (v: string) => {
    setApiKey(v);
    writePlaygroundKey(orgId, v);
  };

  // Leaving the page or switching organization unmounts this component. Aborting on
  // the way out stops an in-flight stream from the previous organization writing
  // into the new one, and frees the tab's connection.
  useEffect(() => () => abortRef.current?.abort(), []);

  const limit = Number(maxTokens);
  const validLimit = Number.isInteger(limit) && limit > 0;

  async function run() {
    if (!apiKey.trim()) {
      setError(t("playNeedKey"));
      return;
    }
    setError(null);
    setLastRequestId(null);
    const history: Turn[] = [...turns, { role: "user", content: prompt }];
    setTurns([...history, { role: "assistant", content: "" }]);
    setStreaming(true);
    const ctrl = new AbortController();
    abortRef.current = ctrl;

    const path = surface === "messages" ? "/v1/messages" : "/v1/chat/completions";
    const body =
      surface === "messages"
        ? messagesBody(model.slug, history, system, limit)
        : chatBody(model.slug, history, system, limit);

    try {
      // The same gateway the exported snippet targets. On a single host that is the
      // page's own origin, so this is an ordinary same-origin request; when the
      // surfaces are split across subdomains it goes cross-origin to the API host,
      // which the data plane permits for this origin and the page's own connect-src
      // policy allows.
      const res = await fetch(`${gatewayOrigin(window.location)}${path}`, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          authorization: `Bearer ${apiKey}`,
          // The gateway fills this in from the provider's configuration when it is
          // absent; sending it explicitly avoids depending on that default.
          ...(surface === "messages" ? { "anthropic-version": "2023-06-01" } : {}),
        },
        body: JSON.stringify(body),
        signal: ctrl.signal,
      });
      setLastRequestId(res.headers.get("x-request-id"));
      if (!res.ok || !res.body) {
        const text = await res.text();
        throw new Error(errorDetail(text) ?? t("playUpstreamStatus", { status: res.status }));
      }
      await consumeSSE(res.body, surface, (delta) => {
        setTurns((prev) => {
          const next = [...prev];
          const last = next[next.length - 1]!;
          next[next.length - 1] = { ...last, content: last.content + delta };
          return next;
        });
      });
      setPrompt("");
    } catch (e) {
      if ((e as Error).name !== "AbortError") setError((e as Error).message);
    } finally {
      setStreaming(false);
      abortRef.current = null;
    }
  }

  return (
    <>
      <Card className="space-y-3">
        <Field label={t("playApiKey")} htmlFor="pgkey">
          <Input
            id="pgkey"
            type="password"
            value={apiKey}
            placeholder="sk-flb-v1-…"
            autoComplete="off"
            onChange={(e) => saveKey(e.target.value)}
          />
        </Field>
        <p className="text-base text-kumo-subtle">
          {t("playKeyNote")}{" "}
          {/* Someone allowed to use the playground but not to manage keys can still
            paste one they already have; what they must not be handed is a link that
            can only ever land on a forbidden page. */}
          {canManageKeys && (
            <Link
              to="/orgs/$orgId/keys"
              params={{ orgId }}
              className="text-kumo-info hover:underline"
            >
              {t("playCreateKey")}
            </Link>
          )}
        </p>

        <Field label={t("playSystem")} htmlFor="pgsystem" hint={t("playSystemHint")}>
          <Textarea
            id="pgsystem"
            rows={2}
            value={system}
            onChange={(e) => setSystem(e.target.value)}
          />
        </Field>

        <FormRow className="sm:grid-cols-[12rem_minmax(0,1fr)]">
          <FormRow.Item>
            <Field label={t("playMaxTokens")} htmlFor="pgmax">
              <Input
                id="pgmax"
                inputMode="numeric"
                value={maxTokens}
                onChange={(e) => setMaxTokens(e.target.value)}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Item>
            {/* The surface is **decided by the model**, not offered as a choice. It
              is shown because it determines which endpoint the snippet below
              targets. */}
            <Field label={t("playSurfaceLabel")}>
              <p className="font-mono text-base text-kumo-subtle">
                POST {surface === "messages" ? "/v1/messages" : "/v1/chat/completions"}
              </p>
            </Field>
          </FormRow.Item>
        </FormRow>

        {turns.length > 0 && (
          <div className="max-h-96 space-y-2 overflow-y-auto rounded-md border border-kumo-line p-3">
            {turns.map((turn, i) => (
              <div key={i} className={turn.role === "user" ? "text-kumo-subtle" : ""}>
                <div className="text-base text-kumo-inactive">
                  {turn.role === "user" ? t("playYou") : model.slug}
                </div>
                <div className="text-base whitespace-pre-wrap">
                  {turn.content || (streaming && i === turns.length - 1 ? "…" : "")}
                </div>
              </div>
            ))}
          </div>
        )}

        <Field label={t("playMessage")} htmlFor="pgprompt">
          <Textarea
            id="pgprompt"
            rows={3}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
          />
        </Field>

        <div className="flex items-center gap-2">
          <Button onClick={run} disabled={streaming || !prompt.trim() || !validLimit}>
            {streaming ? t("playGenerating") : t("playSend")}
          </Button>
          {streaming && (
            <Button variant="outline" onClick={() => abortRef.current?.abort()}>
              {t("playStop")}
            </Button>
          )}
          {turns.length > 0 && !streaming && (
            <Button variant="ghost" onClick={() => setTurns([])}>
              {t("playClear")}
            </Button>
          )}
        </div>

        {error && <Alert>{error}</Alert>}
        {/* Show what the call actually cost, as evidence that it went through
            billing rather than running in some demonstration mode. */}
        {lastRequestId && !streaming && <ChargedFor orgId={orgId} requestId={lastRequestId} />}
      </Card>

      <CodeCard
        snippet={curlSnippet({
          origin: gatewayOrigin(window.location),
          surface,
          model: model.slug,
          system,
          maxTokens: validLimit ? limit : 1024,
          prompt: prompt || "Hello",
        })}
      />
    </>
  );
}

/**
 * Exports the current call as a pastable curl command.
 *
 * The point of a playground is not to chat, it is to let someone **take the settings
 * they tuned away with them**; if the request has to be rewritten from the docs
 * afterwards, the page is a toy. The snippet tracks the form above live, including
 * the surface itself — the two have different body shapes.
 */
function CodeCard({ snippet }: { snippet: string }) {
  const { t } = useI18n();
  return (
    <Card className="space-y-3">
      <div className="flex items-center justify-between gap-2">
        <SectionHeading as="h3">{t("playCodeTitle")}</SectionHeading>
        <CopyAction text={snippet} copyLabel={t("playCopy")} copiedLabel={t("playCopied")} />
      </div>
      <p className="text-base text-kumo-subtle">{t("playCodeHint")}</p>
      {/* The code block scrolls horizontally on its own; the page never does. */}
      <pre className="overflow-x-auto rounded-md border border-kumo-line bg-kumo-recessed p-3 text-base">
        <code>{snippet}</code>
      </pre>
    </Card>
  );
}

/** Reads back the stored record of this request to show what was actually charged. */
function ChargedFor({ orgId, requestId }: { orgId: string; requestId: string }) {
  const { t } = useI18n();
  const q = gatewayConsoleApi.useGetRequestLog(orgId, requestId, {
    // Settlement happens after the response, so the first read may find nothing
    // yet — retry a few times with backoff.
    query: { retry: 4, retryDelay: (n: number) => 400 * 2 ** n },
  });
  if (!q.data) {
    return <p className="text-base text-kumo-inactive">{t("playRequestId", { id: requestId })}</p>;
  }
  const d = q.data;
  return (
    <p className="text-base text-kumo-subtle tabular-nums">
      {t("playCharged", {
        amount: formatNano(d.charged_nano),
        currency: d.charged_currency ?? "USD",
        in: d.tokens_in ?? 0,
        out: d.tokens_out ?? 0,
        ms: d.duration_ms ?? 0,
        id: requestId,
      })}
    </p>
  );
}

/** Reads the event stream chunk by chunk, handing each text delta to the callback. */
async function consumeSSE(
  body: ReadableStream<Uint8Array>,
  surface: Surface,
  onDelta: (s: string) => void,
) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    // Split on event boundaries, not on lines: one data field can arrive across
    // several reads.
    let sep: number;
    while ((sep = buf.indexOf("\n\n")) !== -1) {
      const event = buf.slice(0, sep);
      buf = buf.slice(sep + 2);
      for (const line of event.split("\n")) {
        if (!line.startsWith("data:")) continue;
        const payload = line.slice(5).trim();
        // `[DONE]` is the chat surface's terminator; the messages surface ends with
        // a message_stop event and never sends this one.
        if (payload === "[DONE]") return;
        try {
          const delta = sseDelta(surface, JSON.parse(payload));
          if (delta) onDelta(delta);
        } catch {
          // Keep-alives and other non-JSON frames are simply ignored.
        }
      }
    }
  }
}

/**
 * Extracts one readable sentence from an error response.
 *
 * Three shapes have to be recognised: the data plane answers the chat surface in
 * **OpenAI's native shape** and the messages surface in **Anthropic's** — both put
 * the sentence at `error.message` — while errors raised before the request reaches a
 * provider come back as a problem document, with `detail` and `title`.
 *
 * Parsing only the problem document made **every data-plane error unreadable**:
 * `error.message` was dropped and the page fell back to "upstream returned 402",
 * when what that 402 actually said was "you are out of credit" — a sentence that was
 * in the response body the whole time.
 */
function errorDetail(body: string): string | null {
  try {
    const p = JSON.parse(body);
    if (p?.error?.message) return String(p.error.message);
    return p.detail || p.title || null;
  } catch {
    return null;
  }
}
