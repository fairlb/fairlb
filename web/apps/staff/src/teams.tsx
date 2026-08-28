import { apiErrorMessage, communityStaffApi, type CommunityStaffTypes } from "@fairlb/api-client";
import { useI18n } from "@fairlb/i18n";
import {
  Alert,
  Button,
  ConfirmDialog,
  DataTable,
  Field,
  FormDialog,
  InlineEmpty,
  Input,
  ListPage,
  LoadingState,
  PageHeader,
  RowActions,
  StatusBadge,
  useAdminTitle,
} from "@fairlb/ui";
import { LinkButton } from "@cloudflare/kumo/components/button";
import { useKumoToastManager } from "@cloudflare/kumo/components/toast";
import { useState } from "react";

/**
 * Teams: the groups this deployment issues keys in.
 *
 * # Why the page exists
 *
 * Two of the things an administrator most often wants to say are statements
 * about a group rather than about a key — "these people may use only this
 * model" and "this department may not exceed this many requests per minute".
 * Both are configured per team, so without a second team neither can be said at
 * all, however many keys are issued.
 *
 * # What it does not do
 *
 * It does not configure the access tier or the rate ceilings; it links to the
 * page that does. Those are the gateway's own settings for a team, they already
 * have a screen, and a second write path to the same values is how two screens
 * come to disagree about which one is in effect.
 *
 * It does not delete. Deleting a team takes its keys with it and leaves the
 * usage rows that name it pointing at nothing — those carry the team id without
 * a foreign key on purpose, so that what was consumed outlives the thing that
 * consumed it. Suspension serves the same intent, is reversible, and the data
 * plane already refuses on it.
 */
export function CommunityTeamsPage() {
  const { t, formatDate } = useI18n();
  const toasts = useKumoToastManager();
  useAdminTitle(t("navTeams"));

  const teams = communityStaffApi.useCommunityListTeams();
  const create = communityStaffApi.useCommunityCreateTeam();
  const update = communityStaffApi.useCommunityUpdateTeam();

  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [renaming, setRenaming] = useState<CommunityStaffTypes.Team | null>(null);
  const [editName, setEditName] = useState("");
  // Suspending refuses every request the team's keys make, so it is confirmed
  // rather than firing from the click.
  const [toggling, setToggling] = useState<CommunityStaffTypes.Team | null>(null);

  const refresh = () => void teams.refetch();
  const items = teams.data?.items ?? [];
  const mutError = create.error ?? update.error;

  return (
    <ListPage
      header={
        <PageHeader
          title={t("navTeams")}
          description={t("teamsHint")}
          actions={<Button onClick={() => setCreating(true)}>{t("teamCreate")}</Button>}
        />
      }
      overlays={
        <>
          <FormDialog
            open={creating}
            onOpenChange={(next) => {
              setCreating(next);
              if (!next) {
                setName("");
                create.reset();
              }
            }}
            title={t("teamCreate")}
            error={create.isError ? apiErrorMessage(create.error) : undefined}
            submitLabel={t("teamCreate")}
            submitDisabled={name.trim() === ""}
            pending={create.isPending}
            onSubmit={() =>
              create.mutate(
                { data: { name: name.trim() } },
                {
                  onSuccess: () => {
                    toasts.add({ variant: "success", title: t("teamCreatedDone") });
                    setCreating(false);
                    setName("");
                    refresh();
                  },
                },
              )
            }
          >
            <Field label={t("teamName")} htmlFor="team-name">
              <Input
                id="team-name"
                value={name}
                autoFocus
                maxLength={100}
                onChange={(e) => setName(e.target.value)}
              />
            </Field>
          </FormDialog>

          <FormDialog
            open={renaming !== null}
            onOpenChange={(next) => {
              if (!next) {
                setRenaming(null);
                update.reset();
              }
            }}
            title={t("teamRename")}
            error={update.isError ? apiErrorMessage(update.error) : undefined}
            submitLabel={t("save")}
            submitDisabled={editName.trim() === ""}
            pending={update.isPending}
            onSubmit={() => {
              if (!renaming) return;
              update.mutate(
                { teamId: renaming.id, data: { name: editName.trim() } },
                {
                  onSuccess: () => {
                    toasts.add({ variant: "success", title: t("teamUpdatedDone") });
                    setRenaming(null);
                    refresh();
                  },
                },
              );
            }}
          >
            <Field label={t("teamName")} htmlFor="team-edit-name">
              <Input
                id="team-edit-name"
                value={editName}
                autoFocus
                maxLength={100}
                onChange={(e) => setEditName(e.target.value)}
              />
            </Field>
          </FormDialog>

          <ConfirmDialog
            open={toggling !== null}
            onOpenChange={(o) => !o && setToggling(null)}
            destructive={toggling?.status === "active"}
            title={
              toggling?.status === "active"
                ? t("teamSuspendConfirmTitle")
                : t("teamResumeConfirmTitle")
            }
            description={
              toggling?.status === "active"
                ? t("teamSuspendConfirmBody", { name: toggling?.name ?? "" })
                : t("teamResumeConfirmBody", { name: toggling?.name ?? "" })
            }
            confirmLabel={toggling?.status === "active" ? t("teamSuspend") : t("teamResume")}
            pending={update.isPending}
            onConfirm={() => {
              if (!toggling) return;
              update.mutate(
                {
                  teamId: toggling.id,
                  data: { status: toggling.status === "active" ? "suspended" : "active" },
                },
                {
                  onSuccess: () => {
                    toasts.add({ variant: "success", title: t("teamUpdatedDone") });
                    setToggling(null);
                    refresh();
                  },
                },
              );
            }}
          />
        </>
      }
    >
      {teams.isError && <Alert>{apiErrorMessage(teams.error)}</Alert>}
      {mutError && <Alert>{apiErrorMessage(mutError)}</Alert>}

      {teams.isPending ? (
        <LoadingState label={t("loading")} />
      ) : items.length === 0 ? (
        <InlineEmpty title={t("navTeams")} />
      ) : (
        <DataTable caption={t("navTeams")}>
          <DataTable.Header>
            <DataTable.Row>
              <DataTable.Head className="pr-3">{t("teamName")}</DataTable.Head>
              <DataTable.Head className="pr-3">{t("teamKeyCount")}</DataTable.Head>
              <DataTable.Head className="pr-3">{t("teamCreatedAt")}</DataTable.Head>
              <DataTable.Head>{""}</DataTable.Head>
            </DataTable.Row>
          </DataTable.Header>
          <DataTable.Body>
            {items.map((team) => (
              <DataTable.Row key={team.id}>
                <DataTable.Cell className="pr-3">
                  <span className="font-medium">{team.name}</span>
                  {team.is_default && (
                    <span className="ml-2">
                      <StatusBadge tone="neutral">{t("teamDefaultBadge")}</StatusBadge>
                    </span>
                  )}
                  {team.status === "suspended" && (
                    <span className="ml-2">
                      <StatusBadge tone="danger">{t("teamSuspended")}</StatusBadge>
                    </span>
                  )}
                  {team.is_default && (
                    <div className="text-kumo-subtle">{t("teamDefaultHint")}</div>
                  )}
                </DataTable.Cell>
                <DataTable.Cell className="pr-3 tabular-nums">{team.key_count}</DataTable.Cell>
                <DataTable.Cell className="pr-3">{formatDate(team.created_at)}</DataTable.Cell>
                <DataTable.Cell>
                  <RowActions align="start">
                    {/* A real link, not a button that navigates: middle-click
                          and copy-link-address both have to work. */}
                    <LinkButton variant="outline" href={`/orgs/${team.id}/access`}>
                      {t("teamAccess")}
                    </LinkButton>
                    <Button
                      variant="outline"
                      onClick={() => {
                        setEditName(team.name);
                        setRenaming(team);
                      }}
                    >
                      {t("teamRename")}
                    </Button>
                    {/* The first team has no suspend action at all rather than
                          a disabled one: the server refuses it, and offering a
                          control that cannot succeed is worse than not offering
                          it. */}
                    {!team.is_default && (
                      <Button variant="outline" onClick={() => setToggling(team)}>
                        {team.status === "suspended" ? t("teamResume") : t("teamSuspend")}
                      </Button>
                    )}
                  </RowActions>
                </DataTable.Cell>
              </DataTable.Row>
            ))}
          </DataTable.Body>
        </DataTable>
      )}
    </ListPage>
  );
}
