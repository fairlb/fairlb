/**
 * The global mutator orval generates every client against: it decodes JSON and
 * turns a non-2xx response into a thrown `ApiError` carrying the RFC 9457
 * problem+json body.
 */

export interface Problem {
  type?: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
  code: string;
  request_id?: string;
}

export class ApiError extends Error {
  readonly status: number;
  readonly problem: Problem | undefined;

  constructor(status: number, problem?: Problem) {
    super(problem?.title ?? `HTTP ${status}`);
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
  }
}

// Orval's fetch mutator returns the decoded body, which is the right default for
// almost every endpoint. Versioned pricing is the exception: its optimistic
// concurrency token lives in the response ETag header and must be replayed in
// If-Match. Keep the last token by URL so generated query/mutation hooks remain
// usable without inventing a second HTTP client just for pricing.
const responseEtags = new Map<string, string>();

export function getResponseETag(...urls: (string | undefined)[]): string | undefined {
  for (const url of urls) {
    if (!url) continue;
    const etag = responseEtags.get(url);
    if (etag) return etag;
  }
  return undefined;
}

export async function customFetch<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, {
    ...init,
    headers: { Accept: "application/json", ...init?.headers },
  });

  if (!res.ok) {
    let problem: Problem | undefined;
    try {
      problem = (await res.json()) as Problem;
    } catch {
      problem = undefined;
    }
    throw new ApiError(res.status, problem);
  }
  const etag = res.headers.get("etag");
  if (etag) responseEtags.set(url, etag);

  // 204 is not the only status that comes back without a body: a 201 or 202
  // that only acknowledges the request has an empty body too. `Response.json()`
  // throws `SyntaxError: Unexpected end of JSON input` on an empty body for
  // *any* status, so keying the decision on a list of statuses turns every such
  // response into a failure after the request already succeeded — the UI shows
  // a parse error for an action the server carried out.
  //
  // So the test is on the thing being tested: whether the body has anything in
  // it. A status allowlist would need editing every time an endpoint starts
  // answering empty, and nobody comes back to edit it.
  const text = await res.text();
  if (text.trim() === "") return undefined as T;
  return JSON.parse(text) as T;
}
