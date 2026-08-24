import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";
import { apiErrorMessage } from "./error-message";
import { ApiError } from "./mutator";

/**
 * createAppQueryClient builds the QueryClient every app in this workspace uses.
 *
 * - Every failed mutation reaches `notifyError`. Hanging it on the
 *   MutationCache is what drives the count of silently failing mutations to
 *   zero: a page that forgets to render `isError` otherwise shows nothing at
 *   all. It coexists with an in-page alert — a toast is transient, an alert
 *   stays, and the two answer different questions.
 * - A failed query only toasts when data is already on screen, i.e. a
 *   background refresh failed. A failed first load is rendered in place by the
 *   page itself and must not be reported twice. A 401 never toasts: that is an
 *   expired session, and the route guard takes the user to sign in.
 * - Retries skip every 4xx (retrying a 404 or a 403 only delays the error) and
 *   allow at most two attempts for 5xx and network errors.
 */
export function createAppQueryClient(notifyError: (message: string) => void): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: (count, err) => !(err instanceof ApiError && err.status < 500) && count < 2,
        staleTime: 30_000,
      },
    },
    mutationCache: new MutationCache({
      onError: (err) => notifyError(apiErrorMessage(err)),
    }),
    queryCache: new QueryCache({
      onError: (err, query) => {
        if (err instanceof ApiError && err.status === 401) return;
        // Background probes opt out of the toast: they are not user-initiated,
        // and they are expected to fail briefly while the server is being
        // restarted. Without this, every restart pops a toast in every open
        // tab and says nothing the user can act on.
        if (query.meta?.silent) return;
        if (query.state.data !== undefined) notifyError(apiErrorMessage(err));
      },
    }),
  });
}
