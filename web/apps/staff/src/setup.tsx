import { communityStaffApi } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import { Alert, AuthShell, Button, Field, Input, useAppName } from "@fairlb/ui";
import { useNavigate } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { apiErrorMessage } from "@fairlb/api-client";
import { useAdminTitle } from "@fairlb/ui";
import { HOME_PATH } from "./registry";

/**
 * First-run setup: create the administrator that owns this instance.
 *
 * One pane, not three. The hosted build's wizard also enrols a second factor
 * and picks a signup gate; neither exists here — there is no TOTP column on
 * this build's account table, and there is nobody to sign up.
 *
 * Creating the account signs the person in, so installing does not end on a
 * sign-in form asking for a password they have not typed anywhere yet.
 */
export function SetupPage() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const meta = communityStaffApi.useCommunityMeta();
  const setup = communityStaffApi.useCommunitySetup();
  const appName = useAppName();
  const title = t("setupTitle");
  useAdminTitle(title);

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [token, setToken] = useState("");

  // Allow-list, not deny-list: only an explicit "available" keeps someone on
  // this page. While /meta is still loading the state is undefined, and
  // treating "not complete" as "go ahead" would flash the form at someone who
  // is about to be redirected away from it.
  const state = meta.data?.setup_state;
  if (state === "complete") {
    void navigate({ to: "/login", replace: true });
    return null;
  }

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setup.mutate(
      { data: { email, password, ...(token ? { token } : {}) } },
      { onSuccess: () => void navigate({ to: HOME_PATH }) },
    );
  };

  return (
    <AuthShell appName={appName} title={title} description={t("setupSubtitle")}>
      <form onSubmit={submit} className="space-y-4">
        <Field label={t("email")} htmlFor="email">
          <Input
            id="email"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="email"
            required
          />
        </Field>
        <Field label={t("password")} htmlFor="password" hint={t("setupPasswordHint")}>
          <Input
            id="password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="new-password"
            minLength={12}
            required
          />
        </Field>
        {/* Only rendered when the server says a token is configured; the field
            cannot be guessed into existence, and asking for one that is not
            required would read as a missing credential. */}
        {meta.data?.setup_requires_token ? (
          <Field label={t("setupToken")} htmlFor="setup-token" hint={t("setupTokenHint")}>
            <Input
              id="setup-token"
              type="password"
              value={token}
              onChange={(e) => setToken(e.target.value)}
              required
            />
          </Field>
        ) : null}
        {setup.isError && <Alert>{apiErrorMessage(setup.error)}</Alert>}
        <Button type="submit" className="w-full" loading={setup.isPending}>
          {t("setupSubmit")}
        </Button>
      </form>
    </AuthShell>
  );
}
