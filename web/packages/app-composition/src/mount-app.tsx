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
  appNameKey?: MessageKey;
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
  const namedApp = appNameKey ? (
    <AppNameProvider messageKey={appNameKey}>{app}</AppNameProvider>
  ) : (
    app
  );

  createRoot(root).render(
    <StrictMode>
      <ThemeProvider>
        <LocaleProvider>
          <QueryClientProvider client={queryClient}>
            <LinkProvider component={AppLink}>
              <Toasty toastManager={toastManager}>
                <RootErrorBoundary>
                  <Suspense fallback={<LoadingState label={loadingLabel} />}>{namedApp}</Suspense>
                </RootErrorBoundary>
              </Toasty>
            </LinkProvider>
          </QueryClientProvider>
        </LocaleProvider>
      </ThemeProvider>
    </StrictMode>,
  );
}
