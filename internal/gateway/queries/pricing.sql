-- Reads and writes for usage-based pricing. The truth about a price is its
-- current value: one row per model, one row per plan. There is no version
-- state machine and no publish workflow.
--
-- Two properties hold instead:
--   1. a price is pinned when the request starts -- by the pricing snapshot
--      transaction in the request pipeline, not by a version id;
--   2. recomputing history reads only the pricing snapshot stored on the usage
--      log row, never a join against current configuration.

-- ===== Resolution at request start =====

-- name: ResolveUsagePricing :one
-- One read for everything: the model's current price, the plan in effect for
-- this org, and any per-model override that plan carries.
-- Runs inside the pipeline's snapshot transaction -- reading once inside the
-- transaction *is* the pin.
WITH selected_plan AS (
    SELECT p.id AS pricing_plan_id
    FROM org_pricing_plan_assignments a
    JOIN pricing_plans p ON p.id = a.pricing_plan_id AND p.status = 'active'
    WHERE a.org_id = sqlc.arg('org_id') AND NOT a.inherit_default
    UNION ALL
    -- With no explicit assignment (or one that explicitly says "follow the
    -- default"), inherit the deployment's default plan.
    SELECT p.id
    FROM pricing_plans p
    WHERE p.is_default AND p.status = 'active'
      AND NOT EXISTS (
          SELECT 1 FROM org_pricing_plan_assignments a
          WHERE a.org_id = sqlc.arg('org_id') AND NOT a.inherit_default
      )
    LIMIT 1
)
SELECT m.id AS model_id, m.slug AS model_slug,
       mp.billing_mode,
       mp.upstream_in_nano_per_mtok, mp.upstream_out_nano_per_mtok,
       mp.upstream_cache_read_nano_per_mtok, mp.upstream_cache_write_nano_per_mtok,
       mp.multiplier_bps AS model_multiplier_bps,
       sp.pricing_plan_id,
       COALESCE(po.multiplier_bps, pl.default_multiplier_bps)::integer AS plan_multiplier_bps,
       (po.multiplier_bps IS NOT NULL)::boolean AS plan_model_override
FROM models m
JOIN model_pricing mp ON mp.model_id = m.id
JOIN selected_plan sp ON true
JOIN pricing_plans pl ON pl.id = sp.pricing_plan_id
LEFT JOIN pricing_plan_model_overrides po
    ON po.pricing_plan_id = sp.pricing_plan_id AND po.model_id = m.id
WHERE m.slug = sqlc.arg('model_slug') AND m.enabled;

-- name: GetRouteCostMultiplier :one
-- Cost is a single scalar: cost = list price x provider.cost_multiplier_bps.
-- Usage snapshots the scalar per request, so changing it later does not distort
-- historical margin.
SELECT p.cost_multiplier_bps
FROM model_routes r
JOIN providers p ON p.id = r.provider_id
WHERE r.id = sqlc.arg('route_id');

-- ===== Model price (one row per model) =====

-- name: GetModelPricing :one
SELECT * FROM model_pricing WHERE model_id = $1;

-- name: UpsertModelPricing :one
-- Saving takes effect immediately. `reason` is required, enforced by the write
-- endpoint, and written to the audit log: "who changed this, when, and why" is
-- the audit log's job, not a publish workflow's.
INSERT INTO model_pricing (
    model_id, billing_mode,
    upstream_in_nano_per_mtok, upstream_out_nano_per_mtok,
    upstream_cache_read_nano_per_mtok, upstream_cache_write_nano_per_mtok,
    multiplier_bps, source_name, source_url, verified_at, provenance, reason, updated_by
) VALUES (
    sqlc.arg('model_id'), sqlc.arg('billing_mode'),
    sqlc.narg('upstream_in'), sqlc.narg('upstream_out'),
    sqlc.narg('upstream_cache_read'), sqlc.narg('upstream_cache_write'),
    sqlc.arg('multiplier_bps'), sqlc.arg('source_name'), sqlc.arg('source_url'),
    sqlc.narg('verified_at'), sqlc.arg('provenance'), sqlc.arg('reason'),
    sqlc.narg('updated_by')
)
ON CONFLICT (model_id) DO UPDATE SET
    billing_mode = EXCLUDED.billing_mode,
    upstream_in_nano_per_mtok = EXCLUDED.upstream_in_nano_per_mtok,
    upstream_out_nano_per_mtok = EXCLUDED.upstream_out_nano_per_mtok,
    upstream_cache_read_nano_per_mtok = EXCLUDED.upstream_cache_read_nano_per_mtok,
    upstream_cache_write_nano_per_mtok = EXCLUDED.upstream_cache_write_nano_per_mtok,
    multiplier_bps = EXCLUDED.multiplier_bps,
    source_name = EXCLUDED.source_name,
    source_url = EXCLUDED.source_url,
    verified_at = EXCLUDED.verified_at,
    provenance = EXCLUDED.provenance,
    reason = EXCLUDED.reason,
    updated_by = EXCLUDED.updated_by
RETURNING *;

-- ===== Advanced pricing dimensions =====

-- name: ListModelPriceDimensionRates :many
-- Ordering carries min_input_tokens so that the bands of one bucket arrive in a
-- fixed order rather than whatever the heap returns.
SELECT * FROM model_price_dimension_rates
WHERE model_id = $1 ORDER BY bucket, service_tier, variant, min_input_tokens;

-- name: UpsertModelPriceDimensionRate :one
INSERT INTO model_price_dimension_rates (
    model_id, bucket, service_tier, variant, min_input_tokens, nano_per_mtok
) VALUES ($1, $2, $3, $4, $5, $6)
-- The conflict target has to carry min_input_tokens too: it is part of the
-- primary key, and leaving it out would collapse two context bands of the same
-- bucket onto one row.
ON CONFLICT (model_id, bucket, service_tier, variant, min_input_tokens)
DO UPDATE SET nano_per_mtok = EXCLUDED.nano_per_mtok
RETURNING *;

-- name: ListModelPriceToolRates :many
SELECT * FROM model_price_tool_rates WHERE model_id = $1 ORDER BY tool;

-- name: UpsertModelPriceToolRate :one
INSERT INTO model_price_tool_rates (model_id, tool, nano_per_call)
VALUES ($1, $2, $3)
ON CONFLICT (model_id, tool) DO UPDATE SET nano_per_call = EXCLUDED.nano_per_call
RETURNING *;

-- name: ListPricingPlans :many
-- The two branches of org_count count different things on purpose: the default
-- plan must also include orgs with no explicit assignment, because those orgs
-- really are on it.
SELECT p.*,
       CASE WHEN p.is_default THEN
           (SELECT count(*) FROM orgs o
             WHERE o.status <> 'deleted'
               AND NOT EXISTS (
                   SELECT 1 FROM org_pricing_plan_assignments a
                    WHERE a.org_id = o.id AND NOT a.inherit_default
                      AND a.pricing_plan_id <> p.id
               ))
       ELSE
           (SELECT count(*) FROM org_pricing_plan_assignments a
              JOIN orgs o ON o.id = a.org_id AND o.status <> 'deleted'
             WHERE a.pricing_plan_id = p.id AND NOT a.inherit_default)
       END::bigint AS org_count
FROM pricing_plans p
-- 键是 (is_default, slug)：默认档／默认方案永远排在最前，其余按 slug。
-- 混合方向的元组比较不能直接写 `>`——is_default 降序意味着「之后」是它更小，
-- 或相等而 slug 更大。slug 唯一，所以两分量已是全序，不需要 id 兜底。
-- @only_id lets the single-plan read reuse this query instead of a second one:
-- org_count is an aggregate only computed here, and two queries that disagree
-- about what a plan is would be two answers to the same question.
WHERE (NOT @has_id::boolean OR p.id = @only_id)
  AND (NOT @has_cursor::boolean
       OR p.is_default < @cursor_is_default::boolean
       OR (p.is_default = @cursor_is_default::boolean AND p.slug > @cursor_slug::text))
  AND (@search::text = '' OR p.slug ILIKE '%' || @search::text || '%'
       OR p.name ILIKE '%' || @search::text || '%')
ORDER BY p.is_default DESC, p.slug
LIMIT @lim;

-- name: GetPricingPlan :one
SELECT * FROM pricing_plans WHERE id = $1;

-- name: CreatePricingPlan :one
INSERT INTO pricing_plans (slug, name, description, default_multiplier_bps)
VALUES ($1, $2, $3, sqlc.arg('default_multiplier_bps')) RETURNING *;

-- name: UpdatePricingPlan :one
-- Saving takes effect immediately. The ETag is still here: it guards against
-- concurrent overwrite, which has nothing to do with versioning.
UPDATE pricing_plans SET name = sqlc.arg('name'), description = sqlc.arg('description'),
       status = sqlc.arg('status'),
       default_multiplier_bps = sqlc.arg('default_multiplier_bps'),
       reason = sqlc.arg('reason'), updated_by = sqlc.narg('updated_by'),
       etag = uuidv7()
WHERE id = sqlc.arg('id') AND etag = sqlc.arg('etag') RETURNING *;

-- name: DeletePricingPlan :execrows
DELETE FROM pricing_plans p
WHERE p.id = sqlc.arg('id') AND p.etag = sqlc.arg('etag') AND NOT p.is_default
  AND NOT EXISTS (SELECT 1 FROM org_pricing_plan_assignments a
                  WHERE a.pricing_plan_id = p.id AND NOT a.inherit_default);

-- name: ListPricingPlanModelOverrides :many
SELECT o.*, m.slug AS model_slug
FROM pricing_plan_model_overrides o
JOIN models m ON m.id = o.model_id
WHERE o.pricing_plan_id = $1
-- 有界而不是分页，且这是**整集编辑面的前提**（ADR-0189）：编辑面把这份清单当作
-- 完整的「已配置集」，缺一行，那一行在界面上就不存在——读者看到的不是「没勾」，
-- 是「没有这一项」，而保存时它会被算作未勾选。上界跟着目录走（同为 500）：
-- 一个方案覆盖价的条数不可能超过目录本身。
ORDER BY m.slug
LIMIT 500;

-- name: UpsertPricingPlanModelOverride :one
INSERT INTO pricing_plan_model_overrides (pricing_plan_id, model_id, multiplier_bps)
VALUES ($1, $2, $3)
ON CONFLICT (pricing_plan_id, model_id) DO UPDATE SET multiplier_bps = EXCLUDED.multiplier_bps
RETURNING *;

-- name: ClearPricingPlanModelOverrides :execrows
-- Used for whole-set replacement: the overrides are a set, so per-field dirty
-- checking does not apply to them.
DELETE FROM pricing_plan_model_overrides WHERE pricing_plan_id = $1;

-- ===== Organization assignment (one row per org) =====

-- name: GetEffectivePricingPlanForOrg :one
WITH selected AS (
    SELECT p.*
    FROM org_pricing_plan_assignments a
    JOIN pricing_plans p ON p.id = a.pricing_plan_id AND p.status = 'active'
    WHERE a.org_id = sqlc.arg('org_id') AND NOT a.inherit_default
    UNION ALL
    SELECT p.* FROM pricing_plans p
    WHERE p.is_default AND p.status = 'active'
      AND NOT EXISTS (SELECT 1 FROM org_pricing_plan_assignments a
                      WHERE a.org_id = sqlc.arg('org_id') AND NOT a.inherit_default)
    LIMIT 1
)
SELECT s.id AS pricing_plan_id, s.slug, s.name, s.is_default, s.default_multiplier_bps
FROM selected s;

-- name: UpsertOrgPricingPlanAssignment :one
-- One row per org. `inherit_default = true` explicitly declares "follow the
-- default plan": semantically the same as having no row, but it keeps a reason
-- and an audit subject.
INSERT INTO org_pricing_plan_assignments (
    org_id, pricing_plan_id, inherit_default, reason, updated_by
) VALUES ($1, $2, $3, $4, sqlc.narg('updated_by'))
ON CONFLICT (org_id) DO UPDATE SET
    pricing_plan_id = EXCLUDED.pricing_plan_id,
    inherit_default = EXCLUDED.inherit_default,
    reason = EXCLUDED.reason,
    updated_by = EXCLUDED.updated_by,
    updated_at = now()
RETURNING *;

-- ===== Reference-price import =====
--
-- The two reads below serve the operator-triggered import that fills empty
-- price rows in from a bundled reference dataset. They are reads only: the
-- write goes through the same path as the admin editor, so the completeness
-- rule, the risk assessment and the cache invalidation cannot be bypassed by
-- coming in through a different door.

-- name: ListModelsForPriceImport :many
-- Every model, with the state of its price row.
--
-- The import decides three things per model -- is there a price at all, has a
-- human checked it, and does the reference rate differ from what is stored --
-- and all three are answered here, so the decision is taken against one
-- consistent view of the catalogue rather than a read per model.
--
-- The explicit casts are not decoration: through a LEFT JOIN these columns can
-- all be null, and generating the NOT NULL ones as non-nullable would panic on
-- scan for every model that has no price yet, which is precisely the set this
-- import exists for.
SELECT m.id AS model_id, m.slug AS model_slug,
       (mp.model_id IS NOT NULL)::boolean AS priced,
       mp.verified_at,
       coalesce(mp.billing_mode, '')::text        AS billing_mode,
       coalesce(mp.multiplier_bps, 10000)::integer AS multiplier_bps,
       mp.upstream_in_nano_per_mtok, mp.upstream_out_nano_per_mtok,
       mp.upstream_cache_read_nano_per_mtok, mp.upstream_cache_write_nano_per_mtok
FROM models m
LEFT JOIN model_pricing mp ON mp.model_id = m.id
ORDER BY m.slug;

-- name: ListUsableRoutesForPriceImport :many
-- Where the import reads an upstream model id from: enabled routes on enabled
-- providers, the same set the catalogue calls usable.
--
-- Restricted to that set on purpose. A model nothing can serve stays out of the
-- catalogue whatever its price says, so pricing it would produce a row that
-- changes nothing an operator can see; and a disabled route pointing at a
-- different upstream name is a reading that would only manufacture
-- disagreements between routes.
SELECT r.model_id, r.provider_model_id, p.slug AS provider_slug, p.vendor AS provider_vendor, p.base_url
FROM model_routes r
JOIN providers p ON p.id = r.provider_id
WHERE r.enabled AND p.enabled
ORDER BY r.model_id, r.priority, p.slug;
