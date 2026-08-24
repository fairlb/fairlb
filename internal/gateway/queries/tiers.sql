-- Model tiers: the admission axis. A tier names a set of models an org is
-- allowed to reach; it is orthogonal to pricing, which is decided per model.

-- The list carries two counts: models in the tier and orgs assigned to it.
-- The counts are computed in SQL rather than fetched N+1, because the admin
-- page wants "how many models does this tier admit, how many organizations sit on it"
-- in one screen, and with tiers numbering in the tens a subquery is cheaper
-- than a round trip.
-- model_count is read together with allow_all_models, never on its own: with
-- allow_all_models true the count is zero by construction and says nothing, and
-- with it false a zero count means the tier admits nothing at all.
-- name: ListTiers :many
SELECT t.id, t.slug, t.name, t.description, t.allow_all_models, t.is_default, t.status,
       t.created_at, t.updated_at,
       (SELECT count(*) FROM model_tier_models m WHERE m.tier_id = t.id)::bigint AS model_count,
       (SELECT count(*) FROM org_gateway_settings s WHERE s.tier_id = t.id)::bigint AS org_count
FROM model_tiers t
-- 键是 (is_default, slug)：默认档／默认方案永远排在最前，其余按 slug。
-- 混合方向的元组比较不能直接写 `>`——is_default 降序意味着「之后」是它更小，
-- 或相等而 slug 更大。slug 唯一，所以两分量已是全序，不需要 id 兜底。
WHERE (NOT @has_cursor::boolean
       OR t.is_default < @cursor_is_default::boolean
       OR (t.is_default = @cursor_is_default::boolean AND t.slug > @cursor_slug::text))
  AND (@search::text = '' OR t.slug ILIKE '%' || @search::text || '%'
       OR t.name ILIKE '%' || @search::text || '%')
ORDER BY t.is_default DESC, t.slug
LIMIT @lim;

-- name: GetTier :one
SELECT id, slug, name, description, allow_all_models, is_default, status, created_at, updated_at
FROM model_tiers WHERE id = $1;

-- name: CreateTier :one
INSERT INTO model_tiers (slug, name, description, allow_all_models)
VALUES ($1, $2, $3, $4)
RETURNING id, slug, name, description, allow_all_models, is_default, status, created_at, updated_at;

-- Partial update: NULL means "leave this field alone". is_default is
-- deliberately absent -- moving the default is a two-row transaction
-- (ClearDefaultTier + MarkDefaultTier), and folding it into a partial update
-- would let a casual rename move the default as a side effect.
-- name: UpdateTier :one
UPDATE model_tiers SET
    name             = coalesce(sqlc.narg('name'), name),
    description      = coalesce(sqlc.narg('description'), description),
    status           = coalesce(sqlc.narg('status'), status),
    allow_all_models = coalesce(sqlc.narg('allow_all_models'), allow_all_models),
    updated_at       = now()
WHERE id = sqlc.arg('id')
RETURNING id, slug, name, description, allow_all_models, is_default, status, created_at, updated_at;

-- The two steps of moving the default. They are separate because a single
-- UPDATE cannot satisfy model_tiers_single_default_uk in its intermediate state
-- (a unique index takes effect within the statement).
-- Callers must run both in one transaction, otherwise there is a window with no
-- default tier at all.
-- name: ClearDefaultTier :exec
UPDATE model_tiers SET is_default = false, updated_at = now() WHERE is_default;

-- name: MarkDefaultTier :execrows
UPDATE model_tiers SET is_default = true, updated_at = now()
WHERE id = $1 AND status = 'active';

-- The default tier cannot be deleted (the application checks again; a DELETE
-- guard cannot be expressed as a CHECK). Tiers with members are caught by
-- ON DELETE RESTRICT on org_gateway_settings.tier_id.
-- name: DeleteTier :execrows
DELETE FROM model_tiers WHERE id = $1 AND NOT is_default;

-- ===== Models inside a tier =====

-- name: ListTierModels :many
SELECT m.id, m.slug, m.display_name, m.enabled, m.visibility
FROM model_tier_models tm
JOIN models m ON m.id = tm.model_id
WHERE tm.tier_id = $1
-- 有界而不是分页，且这是**整集编辑面的前提**（ADR-0189）：编辑面把这份清单当作
-- 完整的「已配置集」，缺一行，那一行在界面上就不存在——读者看到的不是「没勾」，
-- 是「没有这一项」，而保存时它会被算作未勾选。上界跟着目录走（同为 500）：
-- 一个档位允许的模型数、一个方案覆盖价的条数，都不可能超过目录本身。
ORDER BY m.slug
LIMIT 500;

-- Whole-set replacement: clear, then insert. Simpler than diffing out an
-- add-set and a remove-set, and what the admin page submits is already the full
-- set after checking boxes -- a diff-based implementation needs extra state and
-- drifts from the UI easily.
-- name: ClearTierModels :exec
DELETE FROM model_tier_models WHERE tier_id = $1;

-- Bulk insert via unnest: one round trip instead of one per model.
-- ON CONFLICT DO NOTHING makes duplicate selections and concurrent replays
-- idempotent.
-- name: AddTierModels :exec
INSERT INTO model_tier_models (tier_id, model_id)
SELECT sqlc.arg('tier_id'), unnest(sqlc.arg('model_ids')::uuid[])
ON CONFLICT DO NOTHING;

-- ===== Binding on the org side =====

-- What this org has been granted: its *effective* tier plus its own rate
-- ceilings.
--
-- Both a missing row and a NULL tier_id fall back to the default tier, so this
-- is not a plain SELECT: `FROM (SELECT 1)` guarantees exactly one row, the two
-- LEFT JOINs pick up "the explicitly assigned tier" and "the default tier", and
-- COALESCE decides which one wins. Doing the fallback in SQL rather than in Go
-- keeps the read path to a single round trip and keeps "effective tier" defined
-- in exactly one place.
--
-- The rate ceilings have no such fallback: a missing row simply means no
-- ceiling, and NULL says the same thing, so they are read straight off the row.
--
-- The two explicit-flag columns let the caller distinguish "configured on
-- purpose" from "following the default" -- that difference is what the admin
-- page needs to show, rather than a value with no visible provenance.
-- name: GetOrgGatewaySettings :one
SELECT (s.tier_id IS NOT NULL)::boolean          AS tier_explicit,
       (s.org_id IS NOT NULL)::boolean           AS row_exists,
       COALESCE(t.id, dt.id)                     AS tier_id,
       COALESCE(t.slug, dt.slug)::text           AS tier_slug,
       COALESCE(t.name, dt.name)::text           AS tier_name,
       COALESCE(t.status, dt.status)::text       AS tier_status,
       COALESCE(t.allow_all_models, dt.allow_all_models)::boolean AS tier_allow_all_models,
       s.rate_limit_rpm,
       s.rate_limit_tpm
FROM (SELECT 1) AS anchor
LEFT JOIN org_gateway_settings s ON s.org_id = sqlc.arg('org_id')::uuid
LEFT JOIN model_tiers t          ON t.id = s.tier_id
-- No join condition = a cross product against the single default row
-- (model_tiers_single_default_uk guarantees there is only one).
LEFT JOIN model_tiers dt         ON dt.is_default;

-- Whole-row replacement (PUT semantics). Deliberately not a partial update: a
-- partial update needs a sentinel parameter to express "clear tier_id" (a
-- nullable field cannot be expressed with coalesce), and that sentinel pattern
-- is where the existing complexity around per-model markup overrides comes
-- from. Whole-row replacement leaves nothing in the SQL but excluded.*.
-- name: PutOrgGatewaySettings :one
INSERT INTO org_gateway_settings (org_id, tier_id, rate_limit_rpm, rate_limit_tpm)
VALUES (sqlc.arg('org_id'), sqlc.narg('tier_id'),
        sqlc.narg('rate_limit_rpm'), sqlc.narg('rate_limit_tpm'))
ON CONFLICT (org_id) DO UPDATE SET
    tier_id        = excluded.tier_id,
    rate_limit_rpm = excluded.rate_limit_rpm,
    rate_limit_tpm = excluded.rate_limit_tpm,
    updated_at     = now()
RETURNING org_id, tier_id, rate_limit_rpm, rate_limit_tpm, updated_at;

-- ===== Dataplane =====

-- The org's gateway settings, read while loading a request identity: the
-- effective tier and the two rate ceilings.
--
-- Same shape as GetOrgGatewaySettings but only the columns the hot path needs.
-- Deliberately not merged into the core org lookup: that query lives in the
-- shared query set and is compiled into builds that do not carry the gateway
-- schema, so joining a gateway table into it would make those builds hit a
-- table that does not exist at runtime. The extra round trip is amortised by
-- the identity cache -- it happens only on load.
-- name: GetOrgSettingsForDataplane :one
SELECT COALESCE(t.id, dt.id)               AS tier_id,
       COALESCE(t.status, dt.status)::text AS tier_status,
       s.rate_limit_rpm,
       s.rate_limit_tpm
FROM (SELECT 1) AS anchor
LEFT JOIN org_gateway_settings s ON s.org_id = sqlc.arg('org_id')::uuid
LEFT JOIN model_tiers t          ON t.id = s.tier_id
LEFT JOIN model_tiers dt         ON dt.is_default;

-- Does this tier admit this model? Either it admits everything, or it admits
-- exactly what it lists -- and a tier that lists nothing admits nothing. The
-- answer therefore has to read the tier row itself, not just count membership:
-- inferring "unrestricted" from an empty membership is what made a cleared
-- list silently grant the whole catalogue.
--
-- Selecting FROM model_tiers means a tier that no longer exists returns no row
-- rather than a permissive default. The caller treats that as "not admitted"
-- (see catalog.Resolve).
-- Every column is table-qualified: without the alias sqlc's analyser reads
-- `tier_id = sqlc.arg('tier_id')` as an ambiguous column reference. PostgreSQL
-- itself resolves it fine, but code generation fails.
-- name: TierAllowsModel :one
SELECT (
    t.allow_all_models
    OR EXISTS (
        SELECT 1 FROM model_tier_models m
        WHERE m.tier_id = t.id AND m.model_id = sqlc.arg('model_id')
    )
)::boolean AS allowed
FROM model_tiers t
WHERE t.id = sqlc.arg('tier_id');

-- Which of these model slugs is this org *not* allowed to reach?
--
-- It answers with the offending slugs rather than a verdict, because whoever
-- asked has to be told which ones: "one of these is not available" leaves a
-- person comparing two lists by hand.
--
-- The criterion is deliberately the same three-part one the dataplane applies
-- -- enabled, public, admitted by the tier -- so a key allowlist can never be
-- saved promising a model that would answer 404 when called. It is not the
-- same *statement*, though, and cannot be: this one runs over a set of slugs
-- and the dataplane resolves exactly one.
--
-- With no tier resolvable at all (no default row) every slug comes back
-- rejected: the comparison against a NULL tier is not true, and refusing to
-- save is the right direction when the deployment cannot say what is allowed.
-- name: ModelsNotAdmittedForOrg :many
WITH tier AS (
    SELECT COALESCE(t.id, dt.id)                   AS id,
           COALESCE(t.allow_all_models, dt.allow_all_models) AS allow_all_models
    FROM (SELECT 1) AS anchor
    LEFT JOIN org_gateway_settings s ON s.org_id = sqlc.arg('org_id')::uuid
    LEFT JOIN model_tiers t          ON t.id = s.tier_id
    LEFT JOIN model_tiers dt         ON dt.is_default
)
SELECT candidate.slug::text AS slug
FROM unnest(sqlc.arg('slugs')::text[]) AS candidate(slug)
WHERE NOT EXISTS (
    SELECT 1
    FROM models m, tier
    WHERE m.slug = candidate.slug
      AND m.enabled
      AND m.visibility = 'public'
      AND (
          tier.allow_all_models
          OR EXISTS (
              SELECT 1 FROM model_tier_models mt
              WHERE mt.tier_id = tier.id AND mt.model_id = m.id
          )
      )
);

-- Changing an org's tier must invalidate the dataplane key cache immediately,
-- because the cached identity carries the tier with it.
-- Cache keys are built from the key hash, so invalidating by org means first
-- looking up that org's hashes.
-- Only active keys: a revoked key has its own invalidation path and should not
-- be re-entering the cache anyway.
-- name: ListActiveKeyHashesForOrg :many
SELECT key_hash FROM api_keys WHERE org_id = $1 AND status = 'active';

-- Reference check before deleting a tier: report the member count so the
-- operator knows how many orgs have to be migrated, instead of just "cannot
-- delete" -- the ON DELETE RESTRICT error does not carry that number.
-- name: CountOrgsOnTier :one
SELECT count(*)::bigint FROM org_gateway_settings WHERE tier_id = $1;
