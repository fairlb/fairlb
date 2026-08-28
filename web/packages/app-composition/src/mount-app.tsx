import { createKumoToastManager, Toasty } from "@cloudflare/kumo/components/toast";
import { LinkProvider } from "@cloudflare/kumo/utils";
import { createAppQueryClient } from "@fairlb/api-client";
import { LocaleProvider, type MessageKey } from "@fairlb/i18n";
import {
  AppLink,
  AppNameProvider,
  ErrorState,
  LoadingState,
  ThemeProvider,
  installStaleBuildDetection,
} from "@fairlb/ui";
import { QueryClientProvider } from "@tanstack/react-query";
import { Component, StrictMode, Suspense, type ErrorInfo, type ReactNode } from "react";
import { createRoot } from "react-dom/client";

export interface MountAppOptions {
  root: HTMLElement;
  app: ReactNode;
  /**
   * The message key holding this application's display name.
   *
   * Required, and that is the fix for a real defect rather than a tightening for
   * its own sake: it used to be optional, two of the three shells omitted it,
   * and they read correctly only because the context's fallback happens to be
   * the operations surface's name. A fourth shell would have been named after
   * the operations console with nothing reporting it. `useAppName` is documented
   * as the single source for the sidebar brand, the authentication pages and the
   * document title, and a single source that a caller may silently decline is
   * not one.
   */
  appNameKey: MessageKey;
  loadingLabel?: string;
}

class RootErrorBoundary extends Component<{ children: ReactNode }, { error?: Error }> {
  state: { error?: Error } = {};

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("application render failed", error, info.componentStack);
  }

  render() {
    return this.state.error ? (
      <ErrorState message={this.state.error.message} />
    ) : (
      this.props.children
    );
  }
}

/** Mount the provider stack shared by every FairLB browser application. */
export function mountApp({ root, app, appNameKey, loadingLabel = "Loading…" }: MountAppOptions) {
  installStaleBuildDetection();
  const toastManager = createKumoToastManager();
  const queryClient = createAppQueryClient((message) =>
    toastManager.add({ variant: "error", title: message }),
  );
  createRoot(root).render(
    <StrictMode>
      <ThemeProvider>
        <LocaleProvider>
          <QueryClientProvider client={queryClient}>
            <LinkProvider component={AppLink}>
              <Toasty toastManager={toastManager}>
                <RootErrorBoundary>
                  <Suspense fallback={<LoadingState label={loadingLabel} />}>
                    <AppNameProvider messageKey={appNameKey}>{app}</AppNameProvider>
                  </Suspense>
                </RootErrorBoundary>
              </Toasty>
            </LinkProvider>
          </QueryClientProvider>
        </LocaleProvider>
      </ThemeProvider>
    </StrictMode>,
  );
}
