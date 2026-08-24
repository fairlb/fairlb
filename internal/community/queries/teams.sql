-- Teams: the organizations of a self-hosted deployment.
--
-- These live in their own query set rather than in the shared one, and the
-- reason is what the shared set would then contain. A hosted deployment owns
-- an organization's whole lifecycle -- a profile row, a deletion window, the
-- membership that authorizes writing it -- and a plain "insert an org" next to
-- those is an insert that skips all of them. Shared code is code both
-- deployments should call, and this is code only one of them should.

-- The teams this instance serves, with how many keys each has.
--
-- Oldest first: the team created at first start is always the first row, so
-- the list does not reorder underneath somebody as teams are added.
--
-- The key count is only the active ones. A revoked key is history, and
-- counting it would make a team that has issued and revoked one key look the
-- same as a team that has one working key.
-- name: ListTeams :many
SELECT o.id, o.name, o.status, o.created_at,
       (SELECT count(*) FROM api_keys k
        WHERE k.org_id = o.id AND k.status = 'active')::bigint AS key_count
FROM orgs o
ORDER BY o.created_at, o.id
LIMIT 200;

-- name: GetTeam :one
SELECT id, name, status, created_at FROM orgs WHERE id = $1;

-- A team is an org with a generated slug.
--
-- The slug is generated here rather than derived from the name, and is never
-- shown or edited. A hosted deployment addresses organizations by it; giving
-- this deployment a second, human-edited identifier for the same row would
-- invite the two to disagree. Deriving it from the name would also make two
-- teams called the same thing collide on a value the operator never typed and
-- cannot see, and that error has nowhere sensible to point.
--
-- A collision here raises the unique violation rather than passing silently,
-- which is the right direction: 48 bits makes it vanishingly unlikely, and if
-- it ever happens the operator sees an error instead of a team quietly landing
-- in another team's row.
-- name: CreateTeam :one
INSERT INTO orgs (slug, name, kind)
VALUES ('team-' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 12),
        sqlc.arg('name'), 'team')
RETURNING id, name, status, created_at;

-- Partial update: NULL leaves the field alone.
--
-- `status` is limited to active and suspended by the application. The column
-- also accepts pending_delete, which belongs to a deletion flow this
-- deployment does not have -- writing it here would leave a team in a state
-- nothing ever advances out of.
-- name: UpdateTeam :one
UPDATE orgs SET
    name       = coalesce(sqlc.narg('name'), name),
    status     = coalesce(sqlc.narg('status'), status),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING id, name, status, created_at;

-- Suspending a team must reach the data plane now, not when the identity cache
-- expires. Cache keys are built from the key hash, so invalidating by team
-- means looking that team's hashes up first.
--
-- Only active keys: a revoked one has its own invalidation path and should not
-- be re-entering the cache anyway.
-- name: ListActiveKeyHashesForTeam :many
SELECT key_hash FROM api_keys WHERE org_id = $1 AND status = 'active';

-- Which team a key belongs to.
--
-- Key operations here address a key by id with no team in the path, while the
-- shared key service scopes every write by organisation. This is the lookup
-- that joins the two: without it, editing a key in any team but the first would
-- answer "not found", which reads as "no such key".
-- name: GetKeyTeam :one
SELECT org_id FROM api_keys WHERE id = $1;
