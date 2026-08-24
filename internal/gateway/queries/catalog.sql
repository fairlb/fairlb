-- Catalog reads for the gateway: resolving a model on the request path.
-- In steady state a cache in front of these absorbs the traffic and the database
-- is only touched on a miss.

-- The usable candidates for a model across providers, filtered by the protocol the
-- request surface belongs to and by what has been observed about the endpoint.
--
-- A route is a candidate for an automatically probed endpoint unless a probe
-- has found the endpoint unsupported on it. No row, `unverified` and `failed`
-- all let the request through: the upstream is the authority on what it
-- serves, and a live request is the cheapest way to ask. A 404 from the
-- upstream is classified as a route problem by the data plane and enqueues a
-- probe, whose verdict -- not the request -- is what removes the candidate.
-- An endpoint the gateway refuses to probe on its own (images) is the other
-- way round: a candidate only once a verdict says `ok`, because nothing would
-- ever converge otherwise and every request would be the probe. The caller
-- says which kind the endpoint is (requires_verdict), from the same surface
-- table the seeder writes probe_mode from, so the rule does not depend on a
-- row having been seeded.
--
-- byok_vendors are the vendors this organization brings its own credential
-- for. An `unsupported` verdict was reached with the platform's shared
-- credential, and "404" from an upstream also means "your project has no
-- access"; the organization's own key may well have it, so for those vendors
-- the verdict does not exclude. An empty result here renders 404
-- model_not_found.
--
-- Protocol membership is decided by whether a provider *speaks the protocol*
-- (`= ANY (p.protocols)`), not by which protocol a provider "belongs to". An
-- aggregating relay that exposes chat, messages and responses at once is one
-- provider row, not two copies each carrying their own credentials and breaker
-- state.
--
-- The protocol column returned is the *request* protocol, not a column on the
-- provider: the candidates have already been filtered by that protocol, so for
-- this candidate it is the protocol we are about to speak upstream. With
-- multi-protocol providers the provider row has no single value to offer anyway,
-- and everything downstream (outbound auth headers, the same-protocol gate for
-- organization credentials) wants the request protocol. See
-- docs/design/same-protocol-passthrough.md.
-- name: ListRoutesForModel :many
SELECT r.id, r.model_id, r.provider_id, r.provider_model_id,
       r.priority, r.weight, r.headers,
       r.context_window, r.max_output_tokens, r.quirks,
       p.slug AS provider_slug, p.vendor AS provider_vendor,
       @protocol::text AS protocol, p.base_url,
       p.headers AS provider_headers, p.transport AS provider_transport,
       p.rate_limit_rpm AS provider_rate_limit_rpm,
       p.rate_limit_tpm AS provider_rate_limit_tpm,
       p.max_concurrency AS provider_max_concurrency
FROM model_routes r
JOIN providers p ON p.id = r.provider_id
WHERE r.model_id = $1
  AND r.enabled AND p.enabled
  AND @protocol::text = ANY (p.protocols)
  AND NOT EXISTS (
      SELECT 1 FROM model_route_probes pr
      WHERE pr.route_id = r.id AND pr.endpoint = @endpoint::text
        -- COALESCE: a NULL vendor list (a caller passing none) must read as
        -- "no exemption", not as an unknowable that lets the row through.
        AND pr.status = 'unsupported' AND NOT COALESCE(p.vendor = ANY (@byok_vendors::text[]), false)
  )
  AND (
      NOT @requires_verdict::boolean
      OR EXISTS (
          SELECT 1 FROM model_route_probes pr
          WHERE pr.route_id = r.id AND pr.endpoint = @endpoint::text AND pr.status = 'ok'
      )
  )
ORDER BY r.priority, r.id;

-- name: GetModelBySlug :one
SELECT * FROM models WHERE slug = $1 AND enabled;

-- The public catalog behind GET /v1/models: only public, enabled models that
-- have at least one usable route -- a model listed with nobody to serve it just
-- hands the caller a 404.
-- A NULL tier_id means no admission filtering; the anonymous catalog is the
-- list price seen from the default tier, not any particular caller's usable
-- set. When a tier is given, the criterion must match what the dataplane
-- resolves, because "visible in the catalog, 404 on call" is exactly the kind of
-- inconsistency that turns into a support ticket.
-- The tier predicate is the same one the dataplane applies: a tier either
-- admits everything or admits exactly what it lists.
-- The pricing plan is resolved in the same statement as the prices, so a price
-- change activating between two separate queries cannot produce a blend of old
-- and new that never existed.
-- With an org_id that org's own assignment wins, falling back to the default
-- plan. With no org_id the default plan is still resolved, because the
-- anonymous catalog is *defined* as the default plan's price: it is what a
-- reader who signs up right now will actually be charged. Leaving the plan
-- unresolved here would publish a list price nobody is on the moment the
-- operator discounts the default plan -- and in the mark-up direction it would
-- publish a price lower than the one charged.
-- name: ListPublicModels :many
WITH selected_plan AS (
    SELECT a.pricing_plan_id
      FROM org_pricing_plan_assignments a
      JOIN pricing_plans p ON p.id = a.pricing_plan_id AND p.status = 'active'
     WHERE sqlc.narg('org_id')::uuid IS NOT NULL
       AND a.org_id = sqlc.narg('org_id')::uuid AND NOT a.inherit_default
    UNION ALL
    SELECT p.id
      FROM pricing_plans p
     WHERE p.is_default AND p.status = 'active'
       AND NOT EXISTS (
           SELECT 1 FROM org_pricing_plan_assignments a
            WHERE sqlc.narg('org_id')::uuid IS NOT NULL
              AND a.org_id = sqlc.narg('org_id')::uuid AND NOT a.inherit_default
       )
    LIMIT 1
)
SELECT m.*,
       -- What the catalog publishes is stated once, in the
       -- model_published_endpoints view: what a probe has seen working, plus
       -- what the platform has no credential to look at. Unverified endpoints
       -- elsewhere are callable but not listed: a listing the caller cannot
       -- act on is the failure this query exists to prevent, whereas an
       -- omission costs nothing. The protocols are those of the published
       -- endpoints, not the providers' declared sets: a provider may declare
       -- a protocol its preset base URL does not serve, and the catalog must
       -- not repeat the claim.
       COALESCE((SELECT array_agg(DISTINCT v.endpoint ORDER BY v.endpoint)
                 FROM model_published_endpoints v WHERE v.model_id = m.id), '{}')::text[] AS endpoints,
       COALESCE((SELECT array_agg(DISTINCT v.protocol ORDER BY v.protocol)
                 FROM model_published_endpoints v WHERE v.model_id = m.id), '{}')::text[] AS protocols,
       -- Price lives in exactly one place: the model_pricing row is the current
       -- price, and models carries no price columns at all.
       mp.model_id AS priced_model_id,
       -- Currency comes from model_pricing (a CHECK pins it to USD).
       COALESCE(mp.currency, 'USD')::text AS price_currency,
       mp.billing_mode AS current_billing_mode,
       mp.upstream_in_nano_per_mtok AS current_upstream_in_nano_per_mtok,
       mp.upstream_out_nano_per_mtok AS current_upstream_out_nano_per_mtok,
       mp.upstream_cache_read_nano_per_mtok AS current_upstream_cache_read_nano_per_mtok,
       mp.upstream_cache_write_nano_per_mtok AS current_upstream_cache_write_nano_per_mtok,
       mp.multiplier_bps AS current_model_multiplier_bps,
       mp.updated_at AS current_model_price_effective_at,
       -- Where the upstream rate came from and whether a person confirmed it.
       -- verified_at NULL means "a reference dataset suggested this, nobody
       -- checked it" -- see docs/design/reference-prices.md. A catalog that
       -- publishes a rate without saying which of the two it is invites the
       -- reader to assume the stronger one.
       mp.source_name AS current_price_source_name,
       mp.source_url AS current_price_source_url,
       mp.verified_at AS current_price_verified_at,
       pl.id AS current_pricing_plan_id,
       pl.default_multiplier_bps AS current_plan_default_multiplier_bps,
       po.multiplier_bps AS current_plan_override_multiplier_bps
FROM models m
LEFT JOIN model_pricing mp ON mp.model_id = m.id
LEFT JOIN selected_plan sp ON true
LEFT JOIN pricing_plans pl ON pl.id = sp.pricing_plan_id
LEFT JOIN pricing_plan_model_overrides po
    ON po.pricing_plan_id = sp.pricing_plan_id AND po.model_id = m.id
WHERE m.enabled AND m.visibility = 'public'
  AND EXISTS (
      SELECT 1 FROM model_routes r
      JOIN providers p ON p.id = r.provider_id
      WHERE r.model_id = m.id AND r.enabled AND p.enabled
  )
  AND (
      sqlc.narg('tier_id')::uuid IS NULL
      OR EXISTS (
          SELECT 1 FROM model_tiers t
          WHERE t.id = sqlc.narg('tier_id')::uuid AND t.allow_all_models
      )
      OR EXISTS (
          SELECT 1 FROM model_tier_models b
          WHERE b.tier_id = sqlc.narg('tier_id')::uuid AND b.model_id = m.id
      )
  )
ORDER BY m.slug;

-- name: GetProviderKeysForProvider :many
-- The source of truth for per-key cooldown is cooldowns(scope='provider_key');
-- this table deliberately has no cooldown status column of its own.
SELECT id, provider_id, name, secret_enc
FROM provider_keys
WHERE provider_id = $1 AND status = 'active'
ORDER BY id;

-- ===== Cooldown state (persisted so it survives a restart; the decision itself
-- is made in memory). See docs/design/failover-and-cooldowns.md. =====

-- name: ListActiveCooldowns :many
SELECT scope, ref_id, until, reason FROM cooldowns WHERE until > now();

-- name: UpsertCooldown :exec
INSERT INTO cooldowns (scope, ref_id, until, reason)
VALUES ($1, $2, $3, $4)
ON CONFLICT (scope, ref_id)
    DO UPDATE SET until = excluded.until, reason = excluded.reason, updated_at = now();

-- name: DeleteCooldown :exec
DELETE FROM cooldowns WHERE scope = $1 AND ref_id = $2;

-- Liveness probe candidates: every provider, including disabled ones -- the
-- probe is also what decides whether a provider can be brought back.
-- protocols is not selected: the probe calls GET /v1/models, which is the same
-- regardless of protocol.
-- name: ListProvidersForProbe :many
-- transport comes along because the probe builds a real URL: an upstream whose
-- catalog lives at another path, or which requires a query parameter, answers
-- 404 to the standard one. Probing without it would auto-disable a healthy
-- provider on a schedule.
SELECT id, slug, base_url, enabled, auto_disabled, transport FROM providers ORDER BY slug;

-- Persist the probe's verdict: auto-disable or auto-recover, flagged so it can
-- be told apart from a manual change.
-- name: SetProviderAutoDisabled :exec
UPDATE providers SET enabled = false, auto_disabled = true, updated_at = now() WHERE id = $1;

-- name: SetProviderAutoEnabled :exec
UPDATE providers SET enabled = true, auto_disabled = false, updated_at = now()
WHERE id = $1 AND auto_disabled;

-- ===== Admin console =====

-- name: ListProvidersForAdmin :many
SELECT p.id, p.slug, p.vendor, p.protocols, p.name, p.base_url, p.enabled, p.auto_disabled, p.headers,
       p.transport, p.cost_multiplier_bps,
       p.rate_limit_rpm, p.rate_limit_tpm, p.max_concurrency,
       (SELECT count(*) FROM provider_keys k WHERE k.provider_id = p.id AND k.status = 'active')::bigint AS key_count,
       -- Only enabled routes are counted: a disabled route carries no traffic,
       -- and counting it as "some model is using this provider" would light up
       -- the provider's readiness checklist green while nothing actually routes
       -- through it. Same criterion the model enable gate uses.
       (SELECT count(*) FROM model_routes r WHERE r.provider_id = p.id AND r.enabled)::bigint AS route_count
FROM providers p
-- Matching base_url too, because the list page's own search box always has:
-- "which provider points at api.openai.com" is a question operators actually
-- ask, and moving the filter server side must not quietly drop a field it
-- used to cover.
WHERE (@search::text = ''
       OR p.slug ILIKE '%' || @search::text || '%'
       OR p.name ILIKE '%' || @search::text || '%'
       OR p.base_url ILIKE '%' || @search::text || '%')
  -- No tiebreaker: providers.slug is UNIQUE, so slug alone is already a total
  -- order. The cursor packs exactly what the ORDER BY sorts by.
  AND (NOT @has_cursor::boolean OR p.slug > @cursor_slug::text)
ORDER BY p.slug
LIMIT @lim;

-- Single-row read for the detail page. The column list is a literal copy of
-- ListProvidersForAdmin: one missing column means one thing the detail page
-- fails to render, and no gate can see a divergence between the two.
-- Distinct from GetProviderForAdmin, which serves the connectivity test and
-- model discovery and only needs identity plus base_url.
-- name: GetProviderDetailForAdmin :one
SELECT p.id, p.slug, p.vendor, p.protocols, p.name, p.base_url, p.enabled, p.auto_disabled, p.headers,
       p.transport, p.cost_multiplier_bps,
       p.rate_limit_rpm, p.rate_limit_tpm, p.max_concurrency,
       (SELECT count(*) FROM provider_keys k WHERE k.provider_id = p.id AND k.status = 'active')::bigint AS key_count,
       (SELECT count(*) FROM model_routes r WHERE r.provider_id = p.id AND r.enabled)::bigint AS route_count
FROM providers p WHERE p.id = $1;

-- protocols is normalised on write (sorted and deduplicated) so the stored form
-- is canonical and comparison and display do not shift with the order the
-- operator happened to tick the boxes in. A CHECK constraint cannot do this
-- (subqueries are not allowed in CHECK), so both write paths array_agg it
-- themselves.
-- name: CreateProvider :one
INSERT INTO providers (slug, vendor, protocols, base_url, name, headers, transport,
                       cost_multiplier_bps, rate_limit_rpm, rate_limit_tpm, max_concurrency)
VALUES (sqlc.arg('slug'), sqlc.arg('vendor'),
        (SELECT array_agg(DISTINCT f ORDER BY f) FROM unnest(sqlc.arg('protocols')::text[]) AS f),
        sqlc.arg('base_url'), sqlc.arg('name'),
        COALESCE(sqlc.narg('headers'), '{}'::jsonb),
        COALESCE(sqlc.narg('transport'), '{}'::jsonb),
        COALESCE(sqlc.narg('cost_multiplier_bps'), 10000),
        sqlc.narg('rate_limit_rpm'), sqlc.narg('rate_limit_tpm'),
        COALESCE(sqlc.narg('max_concurrency'), 64))
RETURNING id, slug, vendor, protocols, name, base_url, enabled, auto_disabled, headers, transport,
          cost_multiplier_bps, rate_limit_rpm, rate_limit_tpm, max_concurrency;

-- Partial update: NULL means the field is unchanged. Enabling by hand also
-- clears auto_disabled -- an explicit operator action takes the provider over,
-- and the probe stops flipping it automatically from then on.
-- name: UpdateProvider :one
UPDATE providers SET
    name = COALESCE(sqlc.narg('name'), name),
    vendor = COALESCE(sqlc.narg('vendor'), vendor),
    protocols = COALESCE(
        (SELECT array_agg(DISTINCT f ORDER BY f) FROM unnest(sqlc.narg('protocols')::text[]) AS f),
        protocols),
    base_url = COALESCE(sqlc.narg('base_url'), base_url),
    enabled = COALESCE(sqlc.narg('enabled'), enabled),
    headers = COALESCE(sqlc.narg('headers'), headers),
    transport = COALESCE(sqlc.narg('transport'), transport),
    cost_multiplier_bps = COALESCE(sqlc.narg('cost_multiplier_bps'), cost_multiplier_bps),
    -- The two ceilings take a clear flag each, for the reason spelled out on
    -- UpdateAPIKeyControls: a nullable parameter cannot tell "leave it alone"
    -- from "remove it", and removing a ceiling has to be sayable.
    rate_limit_rpm = CASE WHEN @clear_rate_limit_rpm::boolean THEN NULL
                          WHEN sqlc.narg('rate_limit_rpm')::integer IS NOT NULL
                            THEN sqlc.narg('rate_limit_rpm')::integer
                          ELSE rate_limit_rpm END,
    rate_limit_tpm = CASE WHEN @clear_rate_limit_tpm::boolean THEN NULL
                          WHEN sqlc.narg('rate_limit_tpm')::integer IS NOT NULL
                            THEN sqlc.narg('rate_limit_tpm')::integer
                          ELSE rate_limit_tpm END,
    max_concurrency = COALESCE(sqlc.narg('max_concurrency'), max_concurrency),
    auto_disabled = CASE WHEN sqlc.narg('enabled') IS NOT NULL THEN false ELSE auto_disabled END,
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING id, slug, vendor, protocols, name, base_url, enabled, auto_disabled, headers, transport,
          cost_multiplier_bps, rate_limit_rpm, rate_limit_tpm, max_concurrency;

-- name: ListModelsForAdmin :many
SELECT m.id, m.slug, m.display_name, m.enabled, m.visibility,
       -- The edit dialog has to prefill current values. These two columns were
       -- once write-only, which meant "settable at creation, unchangeable
       -- afterwards" -- changing a display name took a hand-written API call.
       m.context_window, m.max_output_tokens,
       -- metadata comes along with the list for the same reason: the mapping
       -- editor prefills from it, and without it every open would need a second
       -- single-model query.
       m.metadata,
       -- Published endpoints, from the same view the public catalog reads.
       COALESCE((SELECT array_agg(DISTINCT v.endpoint ORDER BY v.endpoint)
                 FROM model_published_endpoints v WHERE v.model_id = m.id), '{}')::text[] AS endpoints,
       -- The protocols the model is *configured* on: every protocol spoken by
       -- a provider that has an enabled route for it. This is the configuration
       -- view, for the catalog filter; the public catalog publishes the narrower
       -- verified set.
       COALESCE((SELECT array_agg(DISTINCT proto ORDER BY proto)
                 FROM model_routes r JOIN providers p ON p.id = r.provider_id, unnest(p.protocols) proto
                 WHERE r.model_id = m.id AND r.enabled AND p.enabled), '{}')::text[] AS protocols,
       (SELECT count(*) FROM model_routes r2 WHERE r2.model_id = m.id)::bigint AS route_count
FROM models m
WHERE (@search::text = ''
       OR m.slug ILIKE '%' || @search::text || '%'
       OR m.display_name ILIKE '%' || @search::text || '%')
-- Still capped rather than paginated, and the order is deliberate (ADR-0187):
-- four consumers of this list are whole-set editors that need every configured
-- model to stay visible and ticked. Search comes first so those editors have
-- somewhere to move to; the cursor comes after they have moved.
-- This one stays a cap rather than a probe: it also feeds the plain catalog
-- page, which must keep rendering past the bound. The two route lists below are
-- editor-only and probe one row over so the editor can refuse instead.
ORDER BY m.slug
LIMIT 500;

-- Single-row read for the detail page; the column list is a literal copy of
-- ListModelsForAdmin.
-- Distinct from GetModelBySlug, which serves the dataplane, keys on slug and
-- requires enabled. The admin page must be able to open a disabled model --
-- otherwise disabling one makes it permanently unreachable.
-- name: GetModelForAdmin :one
SELECT m.id, m.slug, m.display_name, m.enabled, m.visibility,
       m.context_window, m.max_output_tokens,
       m.metadata,
       COALESCE((SELECT array_agg(DISTINCT v.endpoint ORDER BY v.endpoint)
                 FROM model_published_endpoints v WHERE v.model_id = m.id), '{}')::text[] AS endpoints,
       COALESCE((SELECT array_agg(DISTINCT proto ORDER BY proto)
                 FROM model_routes r JOIN providers p ON p.id = r.provider_id, unnest(p.protocols) proto
                 WHERE r.model_id = m.id AND r.enabled AND p.enabled), '{}')::text[] AS protocols,
       (SELECT count(*) FROM model_routes r2 WHERE r2.model_id = m.id)::bigint AS route_count
FROM models m WHERE m.id = $1;

-- Counts for the provider- and model-level kill switches shown on the health
-- page.
-- The frontend must not derive these by counting the catalog lists: those lists
-- are capped (200 and 500 above), so counting them yields "how many are disabled
-- on the first page" while the card claims "N total, M disabled". The day a cap
-- actually bites, both numbers go silently wrong with no signal at all. A count
-- and the thing it counts have to come from the same place.
-- name: CatalogSwitchCounts :one
SELECT (SELECT count(*) FROM providers)::bigint                     AS providers_total,
       (SELECT count(*) FROM providers WHERE NOT enabled)::bigint   AS providers_disabled,
       (SELECT count(*) FROM models)::bigint                        AS models_total,
       (SELECT count(*) FROM models WHERE NOT enabled)::bigint      AS models_disabled;

-- Per-provider latency histogram over the last hour, for the health dashboard.
--
-- The join is on provider_id directly, not routed through model_routes and
-- models. The older form went providers -> routes -> models -> rollups keyed by
-- model_slug, which meant:
--   a model served by two providers  -> each provider showed that model's
--                                       entire request and error volume
--   two routes on the same provider  -> the same data counted twice
-- In other words the "error rate for this provider" was neither this provider's
-- nor any real number at all.
--
-- Same source and same window as ProviderHealthLastHour (both read the last
-- hour of the rollups), so "error rate" and "latency" describe the same set of
-- requests. Taken from different windows the two would contradict each other
-- during an incident review with no way to tell which to believe.
--
-- The quantile is not computed in SQL: the rollups store a cumulative histogram
-- rather than raw samples, so percentile_cont has nothing to work with. The
-- columns are summed here and interpolated in Go.
-- name: ProviderLatencyLastHour :many
SELECT p.id,
       COALESCE(SUM(ru.lat_count), 0)::bigint       AS samples,
       COALESCE(SUM(ru.lat_le_100), 0)::bigint      AS le_100,
       COALESCE(SUM(ru.lat_le_250), 0)::bigint      AS le_250,
       COALESCE(SUM(ru.lat_le_500), 0)::bigint      AS le_500,
       COALESCE(SUM(ru.lat_le_1000), 0)::bigint     AS le_1000,
       COALESCE(SUM(ru.lat_le_2500), 0)::bigint     AS le_2500,
       COALESCE(SUM(ru.lat_le_5000), 0)::bigint     AS le_5000,
       COALESCE(SUM(ru.lat_le_10000), 0)::bigint    AS le_10000,
       COALESCE(SUM(ru.lat_le_30000), 0)::bigint    AS le_30000,
       COALESCE(SUM(ru.lat_le_60000), 0)::bigint    AS le_60000,
       COALESCE(SUM(ru.lat_le_120000), 0)::bigint   AS le_120000,
       COALESCE(SUM(ru.duration_ms_sum), 0)::bigint AS duration_ms_sum
FROM providers p
LEFT JOIN gateway_usage_rollups ru
       ON ru.provider_id = p.id AND ru.bucket_start >= now() - interval '1 hour'
GROUP BY p.id
ORDER BY p.id;

-- name: ProviderHealthLastHour :many
SELECT p.id, p.slug,
       COALESCE(SUM(ru.requests), 0)::bigint AS requests,
       COALESCE(SUM(ru.errors), 0)::bigint AS errors
FROM providers p
LEFT JOIN gateway_usage_rollups ru
       ON ru.provider_id = p.id AND ru.bucket_start >= now() - interval '1 hour'
GROUP BY p.id, p.slug
ORDER BY p.slug;

-- ===== Admin write surface =====
-- Every write to the catalog goes through here. The hot read path never touches
-- these queries (it reads the cache), so clarity wins over cleverness.

-- name: ListProviderKeysForAdmin :many
-- secret_enc is deliberately not returned: ciphertext only ever goes in, never
-- out, and what the API layer never receives it cannot leak.
--
-- cooldown_until comes from the same place the router uses: a tripped key
-- writes cooldowns(scope='provider_key'), which is also what the breaker behind
-- key selection consults (and what is read back on restart). Without a cooldown
-- column the admin page would show a key as active while the router has already
-- taken it out of rotation.
SELECT k.id, k.provider_id, k.name, k.status, k.secret_hint,
       k.last_verified_at, k.last_error, k.created_at,
       (SELECT c.until FROM cooldowns c
        WHERE c.scope = 'provider_key' AND c.ref_id = k.id AND c.until > now()) AS cooldown_until
FROM provider_keys k
WHERE k.provider_id = @provider_id
  -- Keyset on (created_at, id): two keys added in one rotation script share a
  -- created_at, and a cursor on that alone loses one at the page boundary.
  AND (sqlc.narg('cursor_created_at')::timestamptz IS NULL
       OR (k.created_at, k.id) > (sqlc.narg('cursor_created_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY k.created_at, k.id
LIMIT @lim;

-- How many of this upstream's credentials have ever verified, over the whole set
-- rather than over a page.
--
-- The readiness checklist asks "has any credential been verified", and it used to
-- answer by scanning the list it had already fetched. Once that list paginates,
-- a verified key sitting on a later page reads as "none verified" — a checklist
-- step shown incomplete while the thing it checks is done. A count from the
-- database cannot be wrong that way.
-- name: CountVerifiedProviderKeys :one
SELECT count(*)::bigint
FROM provider_keys k
WHERE k.provider_id = @provider_id
  AND k.last_verified_at IS NOT NULL
  AND k.last_error = '';

-- Carries a provider_id predicate, like DeleteProviderKey: a tampered path
-- parameter fails silently instead of modifying another provider's key.
-- The safe rotation order is add new -> verify new -> disable old. Without an
-- update statement the only available move was to delete the old key first,
-- which is the most dangerous order there is.
-- name: UpdateProviderKey :one
UPDATE provider_keys
SET name   = COALESCE(sqlc.narg('name')::text, name),
    status = COALESCE(sqlc.narg('status')::text, status)
WHERE id = sqlc.arg('id') AND provider_id = sqlc.arg('provider_id')
RETURNING id, provider_id, name, status, secret_hint, last_verified_at, last_error;

-- name: GetProviderKeyForProvider :one
-- Used when the connectivity test probes a specific key_id. Ownership must be
-- verified here: without it, passing another provider's key_id would send that
-- key to this provider's base_url.
SELECT id, provider_id, name, secret_enc, status
FROM provider_keys
WHERE id = $1 AND provider_id = $2;

-- name: CreateProviderKey :one
INSERT INTO provider_keys (provider_id, name, secret_enc, secret_hint)
VALUES ($1, $2, $3, $4)
RETURNING id, provider_id, name, status, secret_hint, last_verified_at, last_error;

-- name: SetProviderKeySecret :exec
-- Back-fill the ciphertext: the AEAD associated data is bound to the row id, so
-- the row has to exist before its secret can be encrypted.
UPDATE provider_keys SET secret_enc = $2 WHERE id = $1;

-- name: DeleteProviderKey :execrows
-- Carries a provider_id predicate to prevent cross-provider deletion: a
-- tampered path parameter fails silently rather than deleting another
-- provider's key.
DELETE FROM provider_keys WHERE id = $1 AND provider_id = $2;

-- name: MarkProviderKeyVerified :exec
-- Records the outcome of a connectivity test. A failure does not disable the
-- key: the probe worker is the only authority for automatic disabling, and one
-- failed manual test may just mean the upstream model name was typed wrong.
UPDATE provider_keys SET last_verified_at = now(), last_error = $2 WHERE id = $1;

-- name: GetProviderForAdmin :one
-- Serves the connectivity test and model discovery, so it carries transport as
-- well as headers: both of those build a real outbound request, and one built
-- without the transport profile would report "cannot reach the upstream" for a
-- provider the data plane reaches without trouble -- the diagnosis tool
-- disagreeing with the thing it diagnoses.
SELECT id, slug, vendor, protocols, name, base_url, enabled, auto_disabled, headers, transport
FROM providers WHERE id = $1;

-- Existence check before creating a route, and the protocol set the route's
-- probe rows are seeded from. A cross join with two equality conditions: if
-- either side does not exist the result is zero rows, which is how the caller
-- tells "not found" apart from anything else. There is no protocol to compare
-- on the model side: a model is reachable on whatever its providers speak.
-- name: RouteParties :one
SELECT m.id AS model_id, p.protocols AS provider_protocols
FROM models m, providers p
WHERE m.id = @model_id AND p.id = @provider_id;

-- name: CreateModel :one
-- Pricing does not live in this table: the model_pricing row is the price.
-- When a model is created with a price, the price goes through
-- `PUT /gateway/models/{id}/pricing` as a second call.
INSERT INTO models (
    slug, display_name, enabled, visibility,
    context_window, max_output_tokens
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, slug, display_name, enabled, visibility;

-- name: UpdateModel :one
-- Partial update: NULL means the field is unchanged (an unset pgtype value is
-- NULL).
UPDATE models SET
    display_name = coalesce(sqlc.narg('display_name'), display_name),
    enabled      = coalesce(sqlc.narg('enabled'), enabled),
    visibility   = coalesce(sqlc.narg('visibility'), visibility),
    context_window    = coalesce(sqlc.narg('context_window'), context_window),
    max_output_tokens = coalesce(sqlc.narg('max_output_tokens'), max_output_tokens),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING id, slug, display_name, enabled, visibility;

-- name: ListRoutesForAdmin :many
SELECT r.id, r.model_id, m.slug AS model_slug,
       r.provider_id, p.slug AS provider_slug, p.protocols AS provider_protocols,
       r.provider_model_id, r.priority, r.weight, r.enabled, r.headers,
       r.context_window, r.max_output_tokens, r.quirks
FROM model_routes r
JOIN providers p ON p.id = r.provider_id
JOIN models m ON m.id = r.model_id
WHERE r.model_id = @model_id
-- 有界而不是分页，且这是**整集编辑面的前提**：模型侧的接线弹窗按这份清单算出
-- 「要建哪些、要删哪些」，清单缺一行，那一行就会被算作「未勾选」而删掉。
-- 一个模型的路由数受限于已配置的供应商数（那是运维手工建的），所以上界在这里
-- 是安全的；真有部署撞上它，该做的是把那个弹窗改成分批，不是给这里加游标。
-- 多取一行：第 501 行出现时 Go 侧拒绝打开编辑面，而不是把截断的清单交出去。
ORDER BY r.priority, p.slug
LIMIT 501;

-- The same rows read from the provider side, for the Models panel on a provider's
-- detail page. Identical columns to ListRoutesForAdmin, only the WHERE axis and
-- the sort key differ: that panel is read model by model, so it sorts by model
-- slug rather than by priority -- priorities on different models of the same
-- provider are not comparable, and sorting by them is effectively random.
-- name: ListRoutesForProviderAdmin :many
SELECT r.id, r.model_id, m.slug AS model_slug,
       r.provider_id, p.slug AS provider_slug, p.protocols AS provider_protocols,
       r.provider_model_id, r.priority, r.weight, r.enabled, r.headers,
       r.context_window, r.max_output_tokens, r.quirks
FROM model_routes r
JOIN providers p ON p.id = r.provider_id
JOIN models m ON m.id = r.model_id
WHERE r.provider_id = @provider_id
-- 同 ListRoutesForAdmin：供应商侧的接线弹窗也是整集编辑，缺行即删行。
ORDER BY m.slug
LIMIT 501;

-- Probe results read from the provider side; same as ListRouteProbes with the
-- axis swapped.
-- name: ListRouteProbesForProvider :many
SELECT pr.route_id, pr.endpoint, pr.probe_mode, pr.status, pr.source, pr.checked_at, pr.latency_ms, pr.status_code, pr.error
FROM model_route_probes pr
JOIN model_routes r ON r.id = pr.route_id
JOIN providers p ON p.id = r.provider_id
WHERE r.provider_id = $1 AND pr.protocol = ANY (p.protocols)
ORDER BY pr.route_id, pr.endpoint;

-- name: CreateRoute :one
INSERT INTO model_routes (
    model_id, provider_id, provider_model_id, priority, weight, enabled, headers,
    context_window, max_output_tokens, quirks
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id, model_id, provider_id, provider_model_id, priority, weight, enabled, headers,
          context_window, max_output_tokens, quirks;

-- name: UpdateRoute :one
UPDATE model_routes SET
    provider_model_id = coalesce(sqlc.narg('provider_model_id'), provider_model_id),
    priority  = coalesce(sqlc.narg('priority'), priority),
    weight    = coalesce(sqlc.narg('weight'), weight),
    enabled   = coalesce(sqlc.narg('enabled'), enabled),
    headers   = coalesce(sqlc.narg('headers'), headers),
    context_window    = coalesce(sqlc.narg('context_window'), context_window),
    max_output_tokens = coalesce(sqlc.narg('max_output_tokens'), max_output_tokens),
    quirks            = coalesce(sqlc.narg('quirks'), quirks),
    updated_at = now()
WHERE id = sqlc.arg('id') AND model_id = sqlc.arg('model_id')
RETURNING id, model_id, provider_id, provider_model_id, priority, weight, enabled, headers,
          context_window, max_output_tokens, quirks;

-- name: DeleteRoute :execrows
DELETE FROM model_routes WHERE id = $1 AND model_id = $2;

-- ===== Upstream model discovery =====

-- Classify the model ids an upstream reports, so the admin page can show the
-- four-state diff view.
--
-- Matching is a heuristic suggestion, never an authoritative verdict. Upstream
-- reports its own model name (`gpt-4o`); identity here is a slug
-- (`openai/gpt-4o`), and there is no reliable mechanical correspondence between
-- the two. So the criterion is fixed at "the slug either equals the upstream
-- name or ends in /<upstream name>", and a hit is only a candidate
-- -- a route is created after a human confirms it.
--
-- LATERAL with LIMIT 1 rather than a plain LEFT JOIN: one upstream name can
-- match several local slugs (`openai/gpt-4o` and `vendorx/gpt-4o`), and a plain
-- join would turn one upstream model into several rows, which reads in the UI
-- as upstream reporting duplicates. The ordering makes an exact match win.
-- name: ClassifyUpstreamModels :many
-- Both explicit type annotations are required: sqlc cannot infer the type of
-- the unnest inside the CTE, nor the nullability of the LEFT JOINed columns.
-- The former would generate interface{}, and the latter would generate the
-- nullable slug as a non-nullable string, which panics on scan.
WITH ids AS (SELECT DISTINCT unnest(sqlc.arg('upstream_ids')::text[])::text AS upstream_id)
SELECT i.upstream_id::text                AS upstream_id,
       m.id                               AS model_id,
       COALESCE(m.slug, '')::text         AS model_slug,
       -- The "is it priced" criterion reads model_pricing, word for word the
       -- same as the readiness check in the admin write path.
       --
       -- It used to read a free flag and four price columns on models. Those
       -- five columns had long been dead, and price now lives in exactly one
       -- place. The general lesson: a value has as many places to be read wrong
       -- as it has places to be stored. Collapsing it to one place removes the
       -- habitat for this class of defect, instead of relying on every reader
       -- remembering which copy is the real one.
       --
       -- The criterion itself: the row says free explicitly, or all four token
       -- buckets are present and not all zero. "All present" rather than "any
       -- non-zero" -- partial NULLs mean incomplete data, not a cheap price.
       -- (A CHECK constraint says the same thing on the write side; this is the
       -- read side of one sentence.)
       --
       -- No COALESCE wrapper: EXISTS is never NULL, and when the LATERAL misses
       -- (m.id IS NULL) `mp.model_id = m.id` is never true, so it is plainly
       -- false. The Go side checks for a missing model id first and reports
       -- "unknown", so the four-state precedence is unaffected.
       EXISTS (
           SELECT 1 FROM model_pricing mp
           WHERE mp.model_id = m.id
             AND (mp.billing_mode = 'free'
                  OR (mp.upstream_in_nano_per_mtok IS NOT NULL
                      AND mp.upstream_out_nano_per_mtok IS NOT NULL
                      AND mp.upstream_cache_read_nano_per_mtok IS NOT NULL
                      AND mp.upstream_cache_write_nano_per_mtok IS NOT NULL
                      AND (mp.upstream_in_nano_per_mtok <> 0
                           OR mp.upstream_out_nano_per_mtok <> 0
                           OR mp.upstream_cache_read_nano_per_mtok <> 0
                           OR mp.upstream_cache_write_nano_per_mtok <> 0)))
       )::boolean AS priced,
       EXISTS (
           SELECT 1 FROM model_routes r
           WHERE r.provider_id = sqlc.arg('provider_id')
             AND r.provider_model_id = i.upstream_id
       )::boolean AS routed
FROM ids i
LEFT JOIN LATERAL (
    -- Only the two columns the decision needs. Narrowing is not about bytes:
    -- `mm.*` would drag every column on models into scope, and the next person
    -- writing a criterion against one of them would meet no resistance at all.
    -- That is exactly how the defect described above grew.
    SELECT mm.id, mm.slug FROM models mm
    -- No protocol predicate: a model owns none, and any provider can carry any
    -- model on the protocols it speaks.
    WHERE mm.slug = i.upstream_id OR mm.slug LIKE '%/' || i.upstream_id
    ORDER BY (mm.slug = i.upstream_id) DESC, mm.slug
    LIMIT 1
) m ON true
ORDER BY i.upstream_id;

-- ===== Route probes =====
--
-- One row per (route, endpoint): the only record of what a route serves. The
-- data plane reads it to skip unsupported endpoints and the catalog reads it to
-- publish verified ones; see the table comment in the migration for the two
-- thresholds.

-- Create the row in the unverified state when a route is created or a provider
-- starts speaking a protocol, so the admin page can show "not checked yet"
-- rather than nothing at all.
-- name: SeedRouteProbe :exec
INSERT INTO model_route_probes (route_id, endpoint, protocol, probe_mode) VALUES ($1, $2, $3, $4)
ON CONFLICT (route_id, endpoint) DO NOTHING;

-- The probe worker's verdict. Three guards live in the upsert rather than in
-- the caller, because every writer must obey them:
--   * an operator's override is never overwritten by the worker;
--   * `failed` never downgrades `ok` -- it is an inconclusive answer (5xx,
--     timeout, quota, credential, or a body the upstream would not take), and
--     letting it flip a verified endpoint would churn the public catalog on
--     every upstream incident;
--   * `unsupported` downgrades `ok` only on a confirming second sample: one
--     404 cannot tell "gone" from "being rolled", so the first keeps the
--     verdict and records the 404, and the next 404 within an hour flips it.
--     The worker schedules that second look itself (see the returned status).
-- In every case the timestamp, status and message of the latest probe are
-- recorded, so the admin page shows what happened even while the verdict
-- stands.
-- name: SaveRouteProbe :one
INSERT INTO model_route_probes (
    route_id, endpoint, protocol, probe_mode, status, source, checked_at, latency_ms, status_code, error
) VALUES ($1, $2, $3, $4, $5, 'probe', now(), $6, $7, $8)
ON CONFLICT (route_id, endpoint) DO UPDATE SET
    status = CASE
        WHEN excluded.status = 'failed' AND model_route_probes.status = 'ok' THEN 'ok'
        WHEN excluded.status = 'unsupported' AND model_route_probes.status = 'ok'
             AND NOT (model_route_probes.status_code IN (404, 405)
                      AND model_route_probes.checked_at > now() - interval '1 hour') THEN 'ok'
        ELSE excluded.status
    END,
    checked_at = excluded.checked_at,
    latency_ms = excluded.latency_ms, status_code = excluded.status_code,
    error = excluded.error, updated_at = now()
WHERE model_route_probes.source <> 'operator'
RETURNING status;

-- The operator's override: a verdict typed in, marked as such so the worker
-- leaves it alone. Rows are only ever for endpoints of a protocol the provider
-- speaks, which the caller checks; the protocol column is written from the
-- endpoint so that the row is filtered like any other.
-- name: SetRouteProbeOverride :exec
INSERT INTO model_route_probes (route_id, endpoint, protocol, probe_mode, status, source, checked_at, error)
VALUES ($1, $2, $3, $4, $5, 'operator', now(), '')
ON CONFLICT (route_id, endpoint) DO UPDATE SET
    status = excluded.status, source = 'operator', checked_at = now(),
    latency_ms = NULL, status_code = NULL, error = '', updated_at = now();

-- Clearing the override hands the row back to the worker, unverified, so the
-- next probe decides.
-- name: ClearRouteProbeOverride :exec
UPDATE model_route_probes
SET status = 'unverified', source = 'probe', checked_at = NULL, latency_ms = NULL,
    status_code = NULL, error = '', updated_at = now()
WHERE route_id = $1 AND endpoint = $2;

-- Only endpoints of a protocol the provider still speaks: rows are deleted when
-- a provider's protocol set narrows, but the read side filters as well so that
-- the two never disagree.
-- name: ListRouteProbes :many
SELECT pr.route_id, pr.endpoint, pr.probe_mode, pr.status, pr.source, pr.checked_at, pr.latency_ms, pr.status_code, pr.error
FROM model_route_probes pr
JOIN model_routes r ON r.id = pr.route_id
JOIN providers p ON p.id = r.provider_id
WHERE r.model_id = $1 AND pr.protocol = ANY (p.protocols)
ORDER BY pr.route_id, pr.endpoint;

-- One route under one model, with the protocol set its endpoints are drawn
-- from: the operator's per-endpoint writes check both before touching a row.
-- name: RouteUnderModel :one
SELECT r.id, p.protocols AS provider_protocols
FROM model_routes r JOIN providers p ON p.id = r.provider_id
WHERE r.id = @route_id AND r.model_id = @model_id;

-- name: GetRouteProbe :one
SELECT pr.route_id, pr.endpoint, pr.probe_mode, pr.status, pr.source, pr.checked_at, pr.latency_ms, pr.status_code, pr.error
FROM model_route_probes pr
WHERE pr.route_id = $1 AND pr.endpoint = $2;

-- When a provider stops speaking a protocol, the rows for that protocol's
-- endpoints go with it on every route of the provider.
-- name: DeleteRouteProbesOutsideProtocols :exec
DELETE FROM model_route_probes pr
USING model_routes r
WHERE r.id = pr.route_id AND r.provider_id = @provider_id
  AND NOT (pr.protocol = ANY (@protocols::text[]));

-- name: CountActiveProviderKeys :one
SELECT count(*)::bigint FROM provider_keys WHERE provider_id = $1 AND status = 'active';

-- Every route of a provider, for seeding probe rows when the provider gains a
-- protocol or its first credential.
-- name: ListRouteIDsForProvider :many
SELECT id FROM model_routes WHERE provider_id = $1 ORDER BY id;

-- The sweeper's work list: verdicts that have aged, on routes and providers
-- that are still enabled, excluding operator overrides -- and only rows a
-- probe can actually advance. Rows it cannot must not be here at all, because
-- the batch is bounded and a row that is due forever would occupy it forever:
-- manual endpoints are never probed on the sweeper's own initiative, and a
-- provider with no active credential gives the worker nothing to probe with.
-- Unverified rows are included after a short grace period, because a route
-- whose creation-time job was lost sits at unverified with nothing else coming
-- for it. Oldest first, so that whatever does not fit this hour is reached the
-- next.
-- name: ListRouteProbesDueForReprobe :many
SELECT pr.route_id, pr.endpoint
FROM model_route_probes pr
JOIN model_routes r ON r.id = pr.route_id
JOIN providers p ON p.id = r.provider_id
WHERE pr.source <> 'operator'
  AND pr.probe_mode = 'auto'
  AND r.enabled AND p.enabled
  AND pr.protocol = ANY (p.protocols)
  AND EXISTS (SELECT 1 FROM provider_keys k WHERE k.provider_id = p.id AND k.status = 'active')
  AND (
      (pr.status = 'unverified' AND pr.updated_at < now() - @unverified_after::interval)
      OR (pr.status IN ('failed', 'unsupported') AND pr.checked_at < now() - @verdict_after::interval)
  )
ORDER BY COALESCE(pr.checked_at, pr.updated_at), pr.route_id, pr.endpoint
LIMIT @max_rows;

-- The context a probe job needs: the route, the provider, and the provider's
-- first usable credential.
-- Which protocol a probe request speaks is decided by the endpoint being probed,
-- not by a column on the provider: a multi-protocol provider has no single
-- protocol to read, while probing messages must send that protocol's auth header
-- and probing chat must send a bearer token. protocols decides which endpoints
-- the route is probed on at all.
-- name: RouteForProbe :one
-- p.transport is selected because the probe has to build the same request the
-- data plane builds. Without it the worker probed with the default profile
-- while the operator's button probed with the provider's real one -- two greens
-- for the same route that could contradict each other, which is exactly what
-- sharing one probe implementation is supposed to prevent.
SELECT r.id, r.provider_model_id,
       p.id AS provider_id, p.protocols, p.base_url, p.headers AS provider_headers,
       p.transport AS provider_transport,
       r.headers AS route_headers
FROM model_routes r JOIN providers p ON p.id = r.provider_id
WHERE r.id = $1;

-- Changing the upstream model name invalidates every previous verdict, the
-- operator's included. Keeping one would be worse than having none: a stale
-- green reads as a current one, and an override was about the old name.
-- name: ResetRouteProbes :exec
UPDATE model_route_probes
SET status = 'unverified', source = 'probe', checked_at = NULL, latency_ms = NULL,
    status_code = NULL, error = '', updated_at = now()
WHERE route_id = $1;
