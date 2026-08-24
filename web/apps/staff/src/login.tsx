import { communityStaffApi } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import { Alert, AuthShell, Button, CredentialInput, Field, Input, useAppName } from "@fairlb/ui";
import { useNavigate } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { apiErrorMessage } from "@fairlb/api-client";
import { useAdminTitle } from "@fairlb/ui";
import { HOME_PATH } from "./registry";

/**
 * Sign-in: email and password, one step.
 *
 * No second factor, because this build's account table has no column for one —
 * signing in successfully returns 204 and a session cookie, with nothing in
 * between. No environment badge either; there are no deployment tiers here.
 *
 * A fresh instance is redirected to the setup page. That check reads the
 * server's answer rather than guessing: on an instance with no administrator
 * this form cannot be passed by anyone, and it looks exactly like a forgotten
 * password.
 */
export function LoginPage() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const login = communityStaffApi.useCommunityLogin();
  const meta = communityStaffApi.useCommunityMeta();
  const appName = useAppName();
  const title = t("staffSignIn");
  useAdminTitle(title);

  // Send the visitor to /setup when no administrator exists yet. The answer
  // comes from the server rather than being guessed: on a brand-new deployment
  // nobody can get through this form, and failing here looks exactly like a
  // forgotten password.
  //
  // Only an explicit "available" counts. The field is undefined until the
  // request comes back, and treating "not complete" as "you may create an
  // account" would flash the wizard at every visitor on every load.
  if (meta.data?.setup_state === "available") {
    void navigate({ to: "/setup", replace: true });
    return null;
  }

  const submit = (e: FormEvent) => {
    e.preventDefault();
    login.mutate(
      { data: { email, password } },
      { onSuccess: () => void navigate({ to: HOME_PATH }) },
    );
  };

  return (
    <AuthShell appName={appName} title={title} description={t("staffAuthSubtitle")}>
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
        <Field label={t("password")} htmlFor="password">
          <CredentialInput id="password" value={password} onValueChange={setPassword} required />
        </Field>
        {login.isError && <Alert>{apiErrorMessage(login.error)}</Alert>}
        <Button type="submit" className="w-full" loading={login.isPending}>
          {t("signIn")}
        </Button>
      </form>
    </AuthShell>
  );
}
