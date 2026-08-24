-- Persisting per-request usage and rolling it up.

-- Inserted in the same transaction as settlement: on a crash the ledger and the
-- usage row roll back together, so there is never "money taken, no record of
-- what for" or the reverse.
-- name: InsertUsageLog :exec
INSERT INTO usage_logs (
    org_id, api_key_id, request_id, surface, model_slug,
    provider_id, provider_key_id, org_provider_key_id, byok,
    route_attempts, stream, status, error_code, http_status,
    tokens_in, tokens_out, tokens_cached_read, tokens_cache_write,
    tokens_reasoning, usage_estimated,
    -- Everything needed to recompute the charge is stored on the row: the fx
    -- rate scalar plus a full pricing snapshot. charged_nano must be
    -- recomputable from this row alone, without joining any configuration
    -- table -- independent recomputation is what makes reverse reconciliation
    -- possible at all.
    upstream_cost_usd_nano, charged_nano, charged_currency,
    fx_rate,
    hold_id, end_user_id, ttft_ms, duration_ms,
    -- Tool usage and service tier are equally part of recomputation: both feed
    -- into the charge, so without them the amount cannot be re-derived from
    -- this row alone.
    tool_calls, service_tier,
    tokens_audio_in, tokens_audio_out, tokens_cache_write_5m, tokens_cache_write_1h,
    pricing_snapshot,
    -- Which route served it, and the hops that failed first.
    route_id, attempts
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    $10, $11, $12, $13, $14,
    $15, $16, $17, $18,
    $19, $20,
    $21, $22, $23,
    $24,
    $25, $26, $27, $28,
    sqlc.narg('tool_calls')::jsonb,
    sqlc.narg('service_tier')::text,
    sqlc.narg('tokens_audio_in')::integer,
    sqlc.narg('tokens_audio_out')::integer,
    sqlc.narg('tokens_cache_write_5m')::integer,
    sqlc.narg('tokens_cache_write_1h')::integer,
    sqlc.narg('pricing_snapshot')::jsonb,
    sqlc.narg('route_id')::uuid,
    sqlc.narg('attempts')::jsonb
);

-- Find the first aggregatable row instead of walking empty calendar ranges.
-- api_key_id is part of the rollup key; internal requests without a key are
-- intentionally detail-only and cannot be represented by this rollup grain.
-- name: FirstUsageForAggregation :one
SELECT min(created_at)::timestamptz
FROM usage_logs
WHERE created_at > @after::timestamptz
  AND created_at <= @until::timestamptz
  AND api_key_id IS NOT NULL;

-- Aggregate in PostgreSQL. This keeps the watermark transaction bounded and
-- avoids materializing every request in Go followed by one write per bucket.
-- The upsert remains set-valued: rewinding a watermark and replaying the same
-- complete hour converges on the same row rather than adding it twice.
-- name: AggregateUsageRollups :execrows
INSERT INTO gateway_usage_rollups (
    org_id, bucket_start, granularity, api_key_id, model_slug, provider_id,
    requests, tokens_in, tokens_out, tokens_cached_read, tokens_cache_write,
    tokens_reasoning, tokens_audio_in, tokens_audio_out,
    tokens_cache_write_5m, tokens_cache_write_1h,
    charged_nano, upstream_cost_usd_nano, errors,
    lat_le_100, lat_le_250, lat_le_500, lat_le_1000, lat_le_2500, lat_le_5000,
    lat_le_10000, lat_le_30000, lat_le_60000, lat_le_120000,
    duration_ms_sum, lat_count
)
SELECT
    org_id,
    date_trunc('hour', created_at, 'UTC'),
    'hour',
    api_key_id,
    model_slug,
    COALESCE(provider_id, '00000000-0000-0000-0000-000000000000'::uuid),
    count(*)::bigint,
    COALESCE(sum(tokens_in), 0)::bigint,
    COALESCE(sum(tokens_out), 0)::bigint,
    COALESCE(sum(tokens_cached_read), 0)::bigint,
    COALESCE(sum(tokens_cache_write), 0)::bigint,
    COALESCE(sum(tokens_reasoning), 0)::bigint,
    COALESCE(sum(tokens_audio_in), 0)::bigint,
    COALESCE(sum(tokens_audio_out), 0)::bigint,
    COALESCE(sum(tokens_cache_write_5m), 0)::bigint,
    COALESCE(sum(tokens_cache_write_1h), 0)::bigint,
    COALESCE(sum(charged_nano), 0)::bigint,
    COALESCE(sum(upstream_cost_usd_nano), 0)::bigint,
    count(*) FILTER (WHERE status = 'upstream_error')::bigint,
    count(*) FILTER (WHERE status = 'ok' AND duration_ms <= 100)::bigint,
    count(*) FILTER (WHERE status = 'ok' AND duration_ms <= 250)::bigint,
    count(*) FILTER (WHERE status = 'ok' AND duration_ms <= 500)::bigint,
    count(*) FILTER (WHERE status = 'ok' AND duration_ms <= 1000)::bigint,
    count(*) FILTER (WHERE status = 'ok' AND duration_ms <= 2500)::bigint,
    count(*) FILTER (WHERE status = 'ok' AND duration_ms <= 5000)::bigint,
    count(*) FILTER (WHERE status = 'ok' AND duration_ms <= 10000)::bigint,
    count(*) FILTER (WHERE status = 'ok' AND duration_ms <= 30000)::bigint,
    count(*) FILTER (WHERE status = 'ok' AND duration_ms <= 60000)::bigint,
    count(*) FILTER (WHERE status = 'ok' AND duration_ms <= 120000)::bigint,
    COALESCE(sum(duration_ms) FILTER (WHERE status = 'ok'), 0)::bigint,
    count(*) FILTER (WHERE status = 'ok')::bigint
FROM usage_logs
WHERE created_at > @after::timestamptz
  AND created_at <= @until::timestamptz
  AND api_key_id IS NOT NULL
GROUP BY org_id, date_trunc('hour', created_at, 'UTC'), api_key_id, model_slug,
         COALESCE(provider_id, '00000000-0000-0000-0000-000000000000'::uuid)
ON CONFLICT (org_id, bucket_start, granularity, api_key_id, model_slug, provider_id)
    DO UPDATE SET
        requests = excluded.requests,
        tokens_in = excluded.tokens_in,
        tokens_out = excluded.tokens_out,
        tokens_cached_read = excluded.tokens_cached_read,
        tokens_cache_write = excluded.tokens_cache_write,
        tokens_reasoning = excluded.tokens_reasoning,
        tokens_audio_in = excluded.tokens_audio_in,
        tokens_audio_out = excluded.tokens_audio_out,
        tokens_cache_write_5m = excluded.tokens_cache_write_5m,
        tokens_cache_write_1h = excluded.tokens_cache_write_1h,
        charged_nano = excluded.charged_nano,
        upstream_cost_usd_nano = excluded.upstream_cost_usd_nano,
        errors = excluded.errors,
        lat_le_100 = excluded.lat_le_100,
        lat_le_250 = excluded.lat_le_250,
        lat_le_500 = excluded.lat_le_500,
        lat_le_1000 = excluded.lat_le_1000,
        lat_le_2500 = excluded.lat_le_2500,
        lat_le_5000 = excluded.lat_le_5000,
        lat_le_10000 = excluded.lat_le_10000,
        lat_le_30000 = excluded.lat_le_30000,
        lat_le_60000 = excluded.lat_le_60000,
        lat_le_120000 = excluded.lat_le_120000,
        duration_ms_sum = excluded.duration_ms_sum,
        lat_count = excluded.lat_count;

-- Latency histogram. Summing the columns merges buckets across time, model and
-- key; the quantile itself is interpolated in Go from the cumulative counts.
-- Computing quantiles in SQL would need either percentile_cont (which wants the
-- full sample set, and the rollups do not keep it) or a long chain of CASEs --
-- the first is impossible, the second unreadable.
-- name: UsageLatencyHistogram :one
-- The denominator is lat_count (successfully served requests), not requests:
-- rejected and failed requests do not represent upstream latency and would
-- bias the distribution toward zero.
SELECT COALESCE(SUM(lat_count), 0)::bigint AS samples,
       COALESCE(SUM(lat_le_100), 0)::bigint AS le_100,
       COALESCE(SUM(lat_le_250), 0)::bigint AS le_250,
       COALESCE(SUM(lat_le_500), 0)::bigint AS le_500,
       COALESCE(SUM(lat_le_1000), 0)::bigint AS le_1000,
       COALESCE(SUM(lat_le_2500), 0)::bigint AS le_2500,
       COALESCE(SUM(lat_le_5000), 0)::bigint AS le_5000,
       COALESCE(SUM(lat_le_10000), 0)::bigint AS le_10000,
       COALESCE(SUM(lat_le_30000), 0)::bigint AS le_30000,
       COALESCE(SUM(lat_le_60000), 0)::bigint AS le_60000,
       COALESCE(SUM(lat_le_120000), 0)::bigint AS le_120000,
       COALESCE(SUM(duration_ms_sum), 0)::bigint AS duration_ms_sum
FROM gateway_usage_rollups
WHERE org_id = $1 AND bucket_start >= @from_ts::timestamptz AND bucket_start < @to_ts::timestamptz
  AND (sqlc.narg('api_key_id')::uuid IS NULL OR api_key_id = sqlc.narg('api_key_id')::uuid);

-- Margin data source for periodic reports. Consumed through an injected
-- interface rather than by querying these tables directly from another layer.
-- name: SumUpstreamCostByRange :one
SELECT COALESCE(SUM(upstream_cost_usd_nano), 0)::bigint AS upstream_cost_usd_nano,
       COALESCE(SUM(charged_nano), 0)::bigint AS charged_nano
FROM gateway_usage_rollups
WHERE org_id = $1 AND bucket_start >= @from_ts::timestamptz AND bucket_start < @to_ts::timestamptz;

-- name: RecordUnsettled :exec
-- Written on a separate connection: it must not join the transaction that has
-- already failed. On repeated failures for the same request_id the first reason
-- is kept and the attempt count incremented -- the first reason is the one worth
-- having when diagnosing.
INSERT INTO gateway_unsettled (request_id, org_id, charged_nano, currency, reason, payload)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (request_id) DO UPDATE
    SET attempts = gateway_unsettled.attempts + 1,
        last_attempt_at = now();

-- name: RecordUnsettledPricing :exec
-- Advanced usage happened but the pinned pricing had no rate for it: snapshot
-- the usage and the pricing context here instead of handing it to the generic
-- retry worker. reserved_nano is the funds currently reserved, not a confirmed
-- customer charge.
--
-- Protecting the reservation is deliberately NOT part of this statement. It
-- used to be: a CTE here also did `UPDATE credit_holds SET expires_at =
-- 'infinity'`, reaching straight into the accounting layer's internal
-- structure. That is a cross-layer dependency no import-based guard can see --
-- dependency linters and `go list -deps` only know about Go imports, not table
-- names inside SQL. And where that table does not exist, the UPDATE touches
-- zero rows and the `WHERE EXISTS` finds nothing, so the whole handling path
-- silently does nothing at all. Protection now goes through the settler
-- interface: the caller invokes it first and only records this row once it
-- succeeds, which lets each build decide for itself what "protect" means.
INSERT INTO gateway_pricing_unsettled (
    request_id, org_id, reserved_nano, currency, reason, payload
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (request_id) DO UPDATE
    SET attempts = gateway_pricing_unsettled.attempts + 1,
        last_attempt_at = now();

-- name: ListUnsettledPending :many
SELECT request_id, org_id, charged_nano, currency, reason, attempts, payload, created_at
FROM gateway_unsettled
WHERE resolved_at IS NULL AND abandoned_at IS NULL
ORDER BY created_at
LIMIT $1;

-- name: MarkUnsettledResolved :exec
UPDATE gateway_unsettled SET resolved_at = now(), last_attempt_at = now()
WHERE request_id = $1;

-- name: BumpUnsettledAttempt :exec
UPDATE gateway_unsettled SET attempts = attempts + 1, last_attempt_at = now(), reason = $2
WHERE request_id = $1;

-- name: AbandonUnsettled :exec
UPDATE gateway_unsettled SET abandoned_at = now(), last_attempt_at = now(), reason = $2
WHERE request_id = $1;

-- ===== Reverse reconciliation: service delivered vs revenue booked =====
--
-- The nightly ledger invariants ask "is the accounting internally consistent" --
-- balances vs entries vs reservations. A request that was never settled at all
-- is perfectly consistent under that criterion and therefore invisible to it.
-- The queries below ask the opposite question: was everything we served
-- actually charged for?

-- name: ReconZeroCharged :many
-- Successful requests that were charged nothing. This surfaces unpriced models,
-- a missing rate for one token bucket, and any future misconfiguration that
-- would silently drive a charge to zero.
-- Free models are excluded -- being free is an explicit configuration choice.
SELECT u.model_slug, u.org_id, count(*)::bigint AS requests
FROM usage_logs u
WHERE u.created_at > $1
  AND u.status = 'ok'
  AND u.charged_nano = 0
  -- Only this row's own snapshot counts. Whether the model was free is a fact
  -- about the moment of the request; explaining an old charge with the model's
  -- current state rewrites history, and does so worst of all right after a
  -- paid/free switch.
  --
  -- Rows whose snapshot has no billing_mode (an empty snapshot left by a
  -- failure) are treated as paid, i.e. they DO get reported. Falling back to
  -- "treat as free and skip" would be the wrong direction: this is a
  -- reconciliation query, and a row we cannot classify should surface for a
  -- human, not quietly vanish from the result.
  AND COALESCE(NULLIF(u.pricing_snapshot ->> 'billing_mode', ''), 'paid') <> 'free'
GROUP BY u.model_slug, u.org_id
ORDER BY requests DESC;

-- name: ReconUnsettledBacklog :one
-- The deferred-charge backlog is the direct measure of "served but not yet
-- collected". abandoned is reported separately: those have exhausted retries
-- and need a human.
SELECT
    count(*) FILTER (WHERE resolved_at IS NULL AND abandoned_at IS NULL)::bigint AS pending,
    COALESCE(SUM(charged_nano) FILTER (WHERE resolved_at IS NULL AND abandoned_at IS NULL), 0)::bigint AS pending_nano,
    count(*) FILTER (WHERE abandoned_at IS NOT NULL)::bigint AS abandoned,
    COALESCE(SUM(charged_nano) FILTER (WHERE abandoned_at IS NOT NULL), 0)::bigint AS abandoned_nano
FROM gateway_unsettled;

-- name: ReconPricingUnsettledBacklog :one
-- Requests missing a rate for an advanced dimension cannot be replayed
-- automatically -- someone has to establish the price first -- so even a
-- backlog of one is worth alerting on.
SELECT
    count(*) FILTER (WHERE resolved_at IS NULL AND abandoned_at IS NULL)::bigint AS pending,
    COALESCE(SUM(reserved_nano) FILTER (WHERE resolved_at IS NULL AND abandoned_at IS NULL), 0)::bigint AS reserved_nano
FROM gateway_pricing_unsettled;

-- name: ReconEstimatedShare :one
-- The share of settled revenue that rests on estimated token counts. The plan
-- has always been "adopt a real tokenizer once this share crosses a threshold",
-- and a threshold with nothing measuring it is not a plan -- this query is the
-- measurement.
SELECT
    COALESCE(SUM(charged_nano) FILTER (WHERE usage_estimated), 0)::bigint AS estimated_nano,
    COALESCE(SUM(charged_nano), 0)::bigint AS total_nano
FROM usage_logs
WHERE created_at > $1 AND status = 'ok';

-- name: DetectSpendAnomalies :many
-- Per-org spend anomaly detection: this hour's spend against the org's median
-- hourly spend over the past 7 days.
--
-- Median rather than mean: one spike drags a mean up far enough that the next
-- real anomaly no longer clears the bar.
-- Only hours with spend enter the median. Counting every calendar hour would
-- give a organization who only uses the service during business hours a median of
-- zero, and then every single request looks anomalous.
--
-- Both a relative and an absolute condition must hold. A new org's baseline is
-- zero, so a pure multiple alerts on their very first spend; a pure absolute
-- threshold buries a large organization's anomaly under their normal volume.
WITH current_hour AS (
    SELECT org_id, SUM(charged_nano)::bigint AS spend
    FROM gateway_usage_rollups
    WHERE bucket_start = date_trunc('hour', now() AT TIME ZONE 'UTC')
    GROUP BY org_id
),
baseline AS (
    SELECT org_id,
           percentile_cont(0.5) WITHIN GROUP (ORDER BY hourly)::bigint AS median_spend
    FROM (
        SELECT org_id, bucket_start, SUM(charged_nano) AS hourly
        FROM gateway_usage_rollups
        WHERE bucket_start >= date_trunc('hour', now() AT TIME ZONE 'UTC') - interval '7 days'
          AND bucket_start < date_trunc('hour', now() AT TIME ZONE 'UTC')
        GROUP BY org_id, bucket_start
        HAVING SUM(charged_nano) > 0
    ) h
    GROUP BY org_id
)
SELECT c.org_id, c.spend::bigint AS current_spend,
       COALESCE(b.median_spend, 0)::bigint AS baseline_spend
FROM current_hour c
LEFT JOIN baseline b USING (org_id)
WHERE c.spend >= sqlc.arg('floor_nano')::bigint
  AND c.spend > COALESCE(b.median_spend, 0) * sqlc.arg('multiplier')::bigint
ORDER BY c.spend DESC;

-- name: MarginByCurrency :many
-- Upstream cost and charged amount aggregated by currency: the margin data
-- source for periodic reports.
--
-- The currency comes from orgs. The rollups carry no currency dimension, and
-- orgs.currency has no update path once the org is created, so joining the
-- current value is the same as joining the historical one.
-- If changing an org's currency ever becomes possible this join stops being
-- correct, and the rollups will need a currency column of their own.
SELECT o.currency,
       COALESCE(SUM(r.upstream_cost_usd_nano), 0)::bigint AS upstream_usd_nano,
       COALESCE(SUM(r.charged_nano), 0)::bigint AS charged_nano
FROM gateway_usage_rollups r
JOIN orgs o ON o.id = r.org_id
WHERE r.bucket_start >= @from_ts::timestamptz AND r.bucket_start < @to_ts::timestamptz
GROUP BY o.currency;

-- ===== Console usage queries =====
--
-- All of them read the rollups, not usage_logs: the rollups exist so charts
-- open instantly, and they are retained for years while the raw rows are kept
-- for 90 days. The primary key leads with (org_id, bucket_start, ...), and the
-- WHERE and GROUP BY clauses below all use that order.

-- name: GetOrgCurrency :one
-- Runs inside the same org-scoped transaction as the other usage queries.
-- It must not reach back to the pool from inside the transaction callback: on a
-- small pool, acquiring a second connection while holding one deadlocks against
-- itself.
SELECT currency FROM orgs WHERE id = $1;

-- Time series are zero-filled. Returning only the buckets that have rollup rows
-- lets the chart draw a straight line between two isolated points, which reads
-- as "steady linear growth" when the truth is seven days with no calls at all.
-- Aggregating first and then LEFT JOINing a generate_series is far cheaper than
-- scanning once per bucket: one index range scan plus one hash join.
--
-- The start is date_trunc(from): when `from` lands mid-bucket, that bucket is
-- still labelled with its own start, matching how the aggregate labels it.
-- The end is `to - 1us`, so no extra bucket is generated exactly at `to` --
-- the interval is half-open.

-- name: UsageSeriesByDay :many
-- Day buckets are cut in the timezone given by @tz, not in UTC.
--
-- bucket_start is a timestamptz and `date_trunc('day', ...)` truncates in UTC by
-- default, while the console renders axis labels in the browser's local zone.
-- The mismatch shows up as misaligned day boundaries: at UTC+8, UTC midnight is
-- 08:00 local, so a "daily" bar spans two local calendar days and the date label
-- can be off by a full day.
-- The server normalises an empty string to 'UTC', so the behaviour then is the
-- same as truncating in UTC.
--
-- Use the function form `timezone(tz, ts)`, not the operator form
-- `ts AT TIME ZONE tz`. They are semantically identical, but sqlc parses the
-- operator form by treating both operands of AT TIME ZONE as new named
-- parameters, and emits an absurdly named pgtype.Interval parameter for it --
-- which compiles, and is wrong.
--
-- Buckets are generated and compared as local naive timestamps (timezone(tz, ts)
-- yields a timestamp) and converted back to timestamptz on the way out, so
-- "add one day" across a DST boundary means a civil day rather than 24 hours.
WITH bounds AS (
    SELECT @from_ts::timestamptz AS f, @to_ts::timestamptz AS t,
           interval '1 day' AS step,
           -- Subtract 1us from the end, not a whole step. The interval is
           -- half-open, and subtracting a step drops the entire current
           -- partial bucket -- the symptom is "the request I just made is
           -- missing from the chart" while the totals still count it, two
           -- numbers that disagree.
           interval '1 microsecond' AS eps
),
agg AS (
    SELECT date_trunc('day', timezone(@tz::text, bucket_start)) AS bucket,
           SUM(requests) AS requests,
           SUM(tokens_in) AS tokens_in,
           SUM(tokens_out) AS tokens_out,
           SUM(charged_nano) AS charged_nano,
           SUM(errors) AS errors
    FROM gateway_usage_rollups
    WHERE org_id = $1 AND bucket_start >= @from_ts::timestamptz AND bucket_start < @to_ts::timestamptz
      AND (sqlc.narg('api_key_id')::uuid IS NULL OR api_key_id = sqlc.narg('api_key_id')::uuid)
    GROUP BY 1
)
SELECT timezone(@tz::text, g.bucket)::timestamptz AS bucket,
       COALESCE(a.requests, 0)::bigint AS requests,
       COALESCE(a.tokens_in, 0)::bigint AS tokens_in,
       COALESCE(a.tokens_out, 0)::bigint AS tokens_out,
       COALESCE(a.charged_nano, 0)::bigint AS charged_nano,
       COALESCE(a.errors, 0)::bigint AS errors
FROM bounds b,
     generate_series(date_trunc('day', timezone(@tz::text, b.f)),
                     timezone(@tz::text, b.t - b.eps), b.step) AS g(bucket)
LEFT JOIN agg a ON a.bucket = g.bucket
ORDER BY g.bucket;

-- name: UsageSeriesByHour :many
WITH bounds AS (
    SELECT @from_ts::timestamptz AS f, @to_ts::timestamptz AS t,
           interval '1 hour' AS step,
           -- Subtract 1us from the end, not a whole step. The interval is
           -- half-open, and subtracting a step drops the entire current
           -- partial bucket -- the symptom is "the request I just made is
           -- missing from the chart" while the totals still count it, two
           -- numbers that disagree.
           interval '1 microsecond' AS eps
),
agg AS (
    SELECT bucket_start AS bucket,
           SUM(requests) AS requests,
           SUM(tokens_in) AS tokens_in,
           SUM(tokens_out) AS tokens_out,
           SUM(charged_nano) AS charged_nano,
           SUM(errors) AS errors
    FROM gateway_usage_rollups
    WHERE org_id = $1 AND bucket_start >= @from_ts::timestamptz AND bucket_start < @to_ts::timestamptz
      AND (sqlc.narg('api_key_id')::uuid IS NULL OR api_key_id = sqlc.narg('api_key_id')::uuid)
    GROUP BY 1
)
SELECT g.bucket::timestamptz AS bucket,
       COALESCE(a.requests, 0)::bigint AS requests,
       COALESCE(a.tokens_in, 0)::bigint AS tokens_in,
       COALESCE(a.tokens_out, 0)::bigint AS tokens_out,
       COALESCE(a.charged_nano, 0)::bigint AS charged_nano,
       COALESCE(a.errors, 0)::bigint AS errors
FROM bounds b,
     generate_series(date_trunc('hour', b.f), b.t - b.eps, b.step) AS g(bucket)
LEFT JOIN agg a ON a.bucket = g.bucket
ORDER BY g.bucket;

-- The two group-by queries must apply the same `api_key_id` filter as the
-- series and totals queries.
--
-- They once did not carry the predicate: a caller asked, per the contract, for
-- "usage of this one key" and got a filtered total alongside unfiltered groups.
-- Summing the groups then does not match the total.

-- name: UsageGroupByModel :many
-- The api_key_id predicate has to be literally the same as in UsageSeriesBy*
-- and UsageTotals: all three make up one usage response, and the filter used to
-- reach only two of them. Passing an api_key_id together with grouping by model
-- returned "this key's totals plus the whole org's model distribution", with no
-- error -- and a self-contradictory report is much harder to notice than a 500.
--
-- It also returns the model's display name, symmetrically with the group-by-key
-- query below: with only the slug the leaderboard shows bare identifiers, while
-- the very same component grouped by key shows human key names -- two different
-- levels of readability in one place.
SELECT r.model_slug AS key,
       COALESCE(MAX(m.display_name), '')::text AS label,
       COALESCE(SUM(r.requests), 0)::bigint AS requests,
       COALESCE(SUM(r.tokens_in), 0)::bigint AS tokens_in,
       COALESCE(SUM(r.tokens_out), 0)::bigint AS tokens_out,
       COALESCE(SUM(r.charged_nano), 0)::bigint AS charged_nano
FROM gateway_usage_rollups r
LEFT JOIN models m ON m.slug = r.model_slug
WHERE r.org_id = $1 AND r.bucket_start >= @from_ts::timestamptz AND r.bucket_start < @to_ts::timestamptz
  AND (sqlc.narg('api_key_id')::uuid IS NULL OR r.api_key_id = sqlc.narg('api_key_id')::uuid)
GROUP BY r.model_slug ORDER BY charged_nano DESC;

-- name: UsageGroupByKey :many
-- Returns the key's name too: with only the uuid the console would have to make
-- a second query to find out which key it is looking at.
SELECT r.api_key_id AS key,
       COALESCE(MAX(k.name), '')::text AS label,
       COALESCE(SUM(r.requests), 0)::bigint AS requests,
       COALESCE(SUM(r.tokens_in), 0)::bigint AS tokens_in,
       COALESCE(SUM(r.tokens_out), 0)::bigint AS tokens_out,
       COALESCE(SUM(r.charged_nano), 0)::bigint AS charged_nano
FROM gateway_usage_rollups r
LEFT JOIN api_keys k ON k.id = r.api_key_id
WHERE r.org_id = $1 AND r.bucket_start >= @from_ts::timestamptz AND r.bucket_start < @to_ts::timestamptz
  AND (sqlc.narg('api_key_id')::uuid IS NULL OR r.api_key_id = sqlc.narg('api_key_id')::uuid)
GROUP BY r.api_key_id ORDER BY charged_nano DESC;

-- name: UsageTotals :one
SELECT COALESCE(SUM(requests), 0)::bigint AS requests,
       COALESCE(SUM(tokens_in), 0)::bigint AS tokens_in,
       COALESCE(SUM(tokens_out), 0)::bigint AS tokens_out,
       COALESCE(SUM(charged_nano), 0)::bigint AS charged_nano,
       COALESCE(SUM(errors), 0)::bigint AS errors
FROM gateway_usage_rollups
WHERE org_id = $1 AND bucket_start >= @from_ts::timestamptz AND bucket_start < @to_ts::timestamptz
  AND (sqlc.narg('api_key_id')::uuid IS NULL OR api_key_id = sqlc.narg('api_key_id')::uuid);

-- name: HasSuccessfulRequest :one
SELECT EXISTS (
    SELECT 1 FROM usage_logs WHERE org_id = $1 AND status = 'ok'
)::boolean AS has_success;
