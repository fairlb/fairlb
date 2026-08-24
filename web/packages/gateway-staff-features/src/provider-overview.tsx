import { gatewayStaffApi, type GatewayStaffTypes } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  Button,
  Card,
  Checkbox,
  ConfirmDialog,
  CopyAction,
  Field,
  FormRow,
  Input,
  SectionHeading,
  Select,
  StatusBadge,
} from "@fairlb/ui";
import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { TestResult } from "./provider-panels";
import { type ProbeEndpoint, probeEndpointsFor, protocolLabel } from "./providers-shared";
import { useVendors, vendorBySlug, vendorLabel } from "./vendors";
import { ReadinessSteps } from "./readiness";

/** A link that switches faces within the detail page, routed the same way as every
 * other link in this package. */
function SectionLink({
  providerId,
  section,
  children,
}: {
  providerId: string;
  section: "models" | "keys";
  children: React.ReactNode;
}) {
  return (
    <Link
      to={
        section === "models"
          ? "/gateway/providers/$providerId/models"
          : "/gateway/providers/$providerId/keys"
      }
      params={{ providerId }}
      className="text-kumo-info underline-offset-4 hover:underline"
    >
      {children}
    </Link>
  );
}

/**
 * The default face of a provider's detail page.
 *
 * Clicking a provider's name used to land on its key table — a page of credentials
 * with a connectivity test underneath that really calls the upstream and really costs
 * money — while a model's detail page landed on its overview. Two detail pages in one
 * package facing opposite ways, and this was the one facing wrong.
 *
 * Each of the three blocks here has its own reason for being here:
 *
 * - **Metadata** came down out of the page header. The base URL was rendered as a
 *   copyable field in the header's description slot, which looks like a bordered
 *   input: a read-only fact wearing the clothes of a form control, and it pushed the
 *   header to five lines.
 * - **The readiness checklist** used to float above every detail face as a banner, while the
 *   same component is overview content on a model's detail page. One thing with two
 *   identities; it is content here too.
 * - **The connectivity test** came over from the keys face. What it answers is "can
 *   this provider be reached right now" — a diagnosis about **the provider**. That
 *   its result is stored on a key is an implementation detail; a probe has to use
 *   some key to make the call. Filing it by what it writes to would put every
 *   diagnosis on the credentials page. The checklist's second step asks exactly this
 *   question, and the two have to sit together to be read at a glance.
 */
export function ProviderOverviewPanel({
  provider,
}: {
  provider: GatewayStaffTypes.GatewayProvider;
}) {
  const { t } = useI18n();
  const vendors = useVendors().data?.items;
  const vendorDocs = vendorBySlug(provider.vendor, vendors)?.docs_url;

  return (
    <div className="space-y-6">
      <section className="space-y-3">
        <SectionHeading>{t("gwProviderIdentity")}</SectionHeading>
        <Card>
          <dl className="grid gap-x-8 gap-y-4 sm:grid-cols-2">
            <MetaRow label={t("gwColBaseUrl")}>
              {/* Plain text plus a copy button. Rendered as a copyable field, it
                  carried a border and an inset button inside a definition list whose
                  every other value is plain text — **a read-only fact looking like a
                  form control**. That is the same reason it was moved out of the page
                  header; moving it fixed the five-line header but not this. */}
              <span className="flex flex-wrap items-center gap-2">
                <span className="font-mono break-all">{provider.base_url}</span>
                <CopyAction text={provider.base_url} variant="ghost" />
              </span>
            </MetaRow>
            <MetaRow label={t("gwColVendor")}>
              <span className="flex flex-wrap items-center gap-2">
                <StatusBadge tone="neutral">{vendorLabel(provider.vendor, vendors)}</StatusBadge>
                {vendorDocs && (
                  <a
                    className="text-kumo-subtle underline"
                    href={vendorDocs}
                    target="_blank"
                    rel="noreferrer noopener"
                  >
                    {t("gwVendorDocs")}
                  </a>
                )}
              </span>
            </MetaRow>
            <MetaRow label={t("gwColProtocols")}>
              <span className="flex flex-wrap gap-2">
                {provider.protocols.map((protocol) => (
                  <StatusBadge key={protocol} tone="neutral">
                    {t(protocolLabel(protocol))}
                  </StatusBadge>
                ))}
              </span>
            </MetaRow>
            <MetaRow label={t("gwColKeys")}>
              <SectionLink providerId={provider.id} section="keys">
                {provider.key_count ?? 0}
              </SectionLink>
            </MetaRow>
            <MetaRow label={t("gwColRoutes")}>
              <SectionLink providerId={provider.id} section="models">
                {provider.route_count ?? 0}
              </SectionLink>
            </MetaRow>
          </dl>
        </Card>
      </section>

      <ProviderReadiness provider={provider} />

      <ConnectivityTest provider={provider} />
    </div>
  );
}

function MetaRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="grid gap-1">
      <dt className="text-base text-kumo-subtle">{label}</dt>
      <dd className="text-base">{children}</dd>
    </div>
  );
}

/**
 * The three-step guide shown after a provider has been created.
 *
 * Every step is decided from the same data that decides whether this provider can
 * actually serve a request — not from whether a form was filled in: it has a key, its
 * most recent connectivity test passed, and at least one enabled route points at it.
 * Once all three hold, nothing is rendered.
 */
function ProviderReadiness({ provider }: { provider: GatewayStaffTypes.GatewayProvider }) {
  const { t } = useI18n();
  // The same query key the key panel uses, so this is deduplicated and costs no
  // extra request.
  const keys = gatewayStaffApi.useListGatewayProviderKeys(provider.id);
  // Until the keys are in hand, "the connectivity test passed" is undecidable: an
  // empty key list would falsely mark that step as incomplete. Unlike the model side,
  // this returns null rather than a loading state — a guide that has not appeared yet
  // is just a temporarily missing block, whereas drawing it as "nothing done" tells a
  // lie about a provider that is in fact configured. A failed query is the same case:
  // its data is undefined too, it has merely settled.
  if (keys.isPending || keys.isError) return null;

  const hasKey = (provider.key_count ?? 0) > 0;
  // The verification timestamp is also set on failure, so the last error has to be
  // read alongside it — otherwise a *failed* connectivity test lights this step up,
  // when it is precisely the evidence that the provider cannot be reached.
  // 「有没有凭据验证过」由服务端在整个集合上答（ADR-0188）。此前是扫它手里那几行，
  // 而列表分页之后，验证过的那把若排在后面的页，这一步会显示成未完成——
  // 一个已经做完的事被画成没做。
  const verified = (keys.data?.verified_count ?? 0) > 0;
  const adopted = (provider.route_count ?? 0) > 0;

  const step = (section: "models" | "keys", label: string) => (
    <SectionLink providerId={provider.id} section={section}>
      {label}
    </SectionLink>
  );

  return (
    <ReadinessSteps
      title={t("gwProviderReadiness")}
      steps={[
        { key: "key", done: hasKey, label: step("keys", t("gwChecklistProviderKey")) },
        // The second step does not link away: the connectivity test is further down
        // this very page.
        { key: "test", done: verified, label: t("gwChecklistProviderTest") },
        { key: "route", done: adopted, label: step("models", t("gwChecklistProviderRoute")) },
      ]}
    />
  );
}

/**
 * The connectivity test, moved here from the keys face with its logic unchanged: it
 * still runs only on an explicit click, still asks for confirmation, and the full
 * trace still appears only where the server permits it.
 */
function ConnectivityTest({ provider }: { provider: GatewayStaffTypes.GatewayProvider }) {
  const { t } = useI18n();
  const keys = gatewayStaffApi.useListGatewayProviderKeys(provider.id);
  const test = gatewayStaffApi.useTestGatewayProvider();

  const [upstreamModel, setUpstreamModel] = useState("");
  const endpointItems = probeEndpointsFor(provider.protocols);
  const defaultEndpoint = endpointItems[0] ?? "chat";
  const [endpoint, setEndpoint] = useState<ProbeEndpoint>(defaultEndpoint);
  const [result, setResult] = useState<GatewayStaffTypes.GatewayProviderTestResult | null>(null);
  const [testing, setTesting] = useState(false);
  // The full trace contains the key in plaintext, and the server only offers it where
  // it is safe to. Whether this toggle appears at all is therefore **decided by the
  // server**, which says so in the response. Re-deriving the same rule in the client
  // meant two implementations of one predicate, and they disagreed: the toggle was
  // shown in environments where the server refused to produce a trace, so pressing it
  // did nothing.
  //
  // Before the response arrives the value is undefined, and `=== true` makes the
  // answer while unknown "no": the toggle does not flash into view and vanish again,
  // and it fails in the same direction the server's own gate does.
  const meta = gatewayStaffApi.useGetGatewayMeta();
  const traceAllowed = meta.data?.probe_trace_allowed === true;
  const [includeTrace, setIncludeTrace] = useState(false);

  return (
    <section className="space-y-3">
      <SectionHeading>{t("gwConnTest")}</SectionHeading>
      <p className="text-base text-kumo-subtle">
        {t("gwConnTestNote")}
        <span className="font-medium">{t("gwConnTestCost")}</span>
        {t("gwConnTestCost2")}
      </p>
      <Card className="space-y-2">
        {/* The test **sends a real upstream request and really costs money**, as the
            prose above says. Without a confirmation the warning and the action were
            two separate things, and only one of them was in the way. */}
        <FormRow
          as="form"
          className="sm:grid-cols-2 lg:grid-cols-[minmax(12rem,1fr)_minmax(10rem,1fr)_auto]"
          onSubmit={(e) => {
            e.preventDefault();
            setTesting(true);
          }}
        >
          <FormRow.Item>
            <Field label={t("gwUpstreamModelLabel")} htmlFor={`t-model-${provider.id}`}>
              <Input
                id={`t-model-${provider.id}`}
                value={upstreamModel}
                required
                onChange={(e) => setUpstreamModel(e.target.value)}
                placeholder="gpt-4o"
              />
            </Field>
          </FormRow.Item>
          {/* Which endpoint to probe is the operator's choice. Always probing chat
              completions made every embeddings- or image-only provider test red — and
              what failed there was the probe, not the provider. Only this protocol's
              endpoints are listed; the server rejects cross-protocol ones anyway. */}
          <FormRow.Item>
            <Field label={t("gwConnTestEndpoint")}>
              <Select
                value={endpoint}
                onValueChange={(v) => setEndpoint((v ?? defaultEndpoint) as ProbeEndpoint)}
                items={endpointItems.map((e) => ({ value: e, label: e }))}
              />
            </Field>
          </FormRow.Item>
          <FormRow.Actions>
            <Button type="submit" variant="outline" disabled={test.isPending || !upstreamModel}>
              {test.isPending ? t("gwTesting") : t("gwTest")}
            </Button>
          </FormRow.Actions>
        </FormRow>
        {traceAllowed && (
          <Checkbox
            checked={includeTrace}
            onCheckedChange={(v) => setIncludeTrace(v === true)}
            label={
              <span className="text-base text-kumo-subtle">
                {t("gwTraceInclude")} · {t("gwTraceHint")}
              </span>
            }
          />
        )}
        {/* The result component renders the full trace itself; not repeated here. */}
        {result && <TestResult result={result} />}
      </Card>
      <ConfirmDialog
        open={testing}
        onOpenChange={setTesting}
        destructive={false}
        title={t("gwConnTestConfirmTitle")}
        description={t("gwConnTestConfirmBody", { model: upstreamModel })}
        confirmLabel={t("gwTest")}
        pending={test.isPending}
        onConfirm={() =>
          test.mutate(
            {
              providerId: provider.id,
              data: {
                upstream_model: upstreamModel,
                endpoint,
                ...(includeTrace ? { include_trace: true } : {}),
              },
            },
            {
              onSuccess: (r) => {
                setResult(r);
                setTesting(false);
                // The checklist's second step reads the verification timestamp off
                // the key rows.
                void keys.refetch();
              },
            },
          )
        }
      />
    </section>
  );
}
