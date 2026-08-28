-- Per-request log listing for the console.
--
-- Reads usage_logs, not the rollups: the log view wants individual request rows,
-- and the rollups have already aggregated them away. Anything aggregate goes
-- through the rollups instead; this file only filters and paginates.

-- name: ListRequestLogs :many
-- Keyset pagination, newest first.
--
-- The cursor is the composite (created_at, id), not a bare timestamp: a bare
-- timestamp drops every other row sharing the last row's instant, and under
-- concurrent gateway writes several rows per millisecond is the normal case,
-- not a theoretical one.
-- The predicate is written as `created_at <=` boundary first (that form can use
-- the index condition on usage_logs_org_idx), then a second term to discard the
-- rows on the boundary that were already returned. Writing it as the row
-- comparison (created_at, id) < (a, b) would instead lose the index -- there is
-- no index in that column order.
SELECT id::text AS id, request_id, created_at, model_slug, surface, status,
       http_status, error_code, stream, tokens_in, tokens_out, billed_units, billed_unit, charged_nano,
       duration_ms, api_key_id, end_user_id
FROM usage_logs
WHERE org_id = @org_id
  AND created_at >= @from_ts::timestamptz
  AND created_at < @to_ts::timestamptz
  AND (sqlc.narg('api_key_id')::uuid IS NULL OR api_key_id = sqlc.narg('api_key_id')::uuid)
  AND (sqlc.narg('model')::text IS NULL OR model_slug = sqlc.narg('model')::text)
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('end_user_id')::text IS NULL OR end_user_id = sqlc.narg('end_user_id')::text)
  AND (
    sqlc.narg('cursor_ts')::timestamptz IS NULL
    OR (created_at <= sqlc.narg('cursor_ts')::timestamptz
        AND (created_at < sqlc.narg('cursor_ts')::timestamptz
             OR id < sqlc.narg('cursor_id')::uuid))
  )
ORDER BY created_at DESC, id DESC
LIMIT @lim;

-- name: GetRequestLog :one
-- Fetch a single log by request_id. request_id is not unique in this table (a
-- UNIQUE constraint on a partitioned table must include the partition key);
-- uniqueness is enforced by credit_holds instead. This returns the newest row,
-- which is also the one you want to look at when a request was replayed.
SELECT l.id::text AS id, l.request_id, l.created_at, l.model_slug, l.surface, l.status,
       l.http_status, l.error_code, l.stream, l.tokens_in, l.tokens_out,
       l.billed_units, l.billed_unit, l.charged_nano,
       l.duration_ms, l.api_key_id, l.end_user_id, l.route_attempts, l.ttft_ms,
       l.tokens_cached_read, l.tokens_cache_write, l.tokens_reasoning,
       l.usage_estimated, l.charged_currency, l.byok,
       COALESCE(p.slug, '')::text AS provider_slug
FROM usage_logs l
LEFT JOIN providers p ON p.id = l.provider_id
WHERE l.org_id = @org_id AND l.request_id = @request_id
ORDER BY l.created_at DESC
LIMIT 1;
