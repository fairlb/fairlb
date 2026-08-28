-- Asynchronous job rows: work that outlives the request that started it.
--
-- Every statement here is written so that a crash between any two of them
-- leaves a state the reconciler can finish. The money-bearing ones guard on
-- settlement_state rather than reading it first, because two concurrent pollers
-- would both pass a check-then-act.

-- name: CreateVideoJob :one
INSERT INTO gateway_async_jobs (
    org_id, api_key_id, kind, request_id, idempotency_key, request_fingerprint,
    model_id, model_slug, route_id, provider_id, provider_key_id, org_provider_key_id,
    byok, status, params, billing_units, hold_id, hold_nano, hold_expires_at,
    charged_currency, pricing_snapshot, end_user_id, max_job_seconds, next_poll_at, expires_at
) VALUES (
    @org_id, @api_key_id, 'video', @request_id, @idempotency_key, @request_fingerprint,
    @model_id, @model_slug, @route_id, @provider_id, @provider_key_id, @org_provider_key_id,
    @byok, 'queued', @params, @billing_units, @hold_id, @hold_nano, @hold_expires_at,
    @charged_currency, @pricing_snapshot, @end_user_id, @max_job_seconds, now(), @expires_at
)
RETURNING *;

-- name: GetVideoJobByIdempotencyKey :one
-- The replay path: a repeat of a key returns the job it already created.
SELECT * FROM gateway_async_jobs
WHERE org_id = @org_id AND kind = 'video' AND idempotency_key = @idempotency_key;

-- name: GetVideoJob :one
-- Org-scoped by the predicate, not only by RLS: a job id belonging to another
-- organization must be indistinguishable from one that does not exist.
SELECT * FROM gateway_async_jobs
WHERE id = @id AND org_id = @org_id AND kind = 'video';

-- name: GetVideoJobByAlias :one
-- The same job reached by its integer alias, which is what one vendor's
-- compatibility surface must hand out because that vendor's own schema types
-- the identifier as int64.
--
-- Scoped the same way and for the same reason: an alias from another
-- organization has to be indistinguishable from one that never existed.
-- Guessing one is easier than guessing a UUID, which is exactly why the
-- predicate carries the organization rather than trusting the identifier to be
-- unguessable.
SELECT * FROM gateway_async_jobs
WHERE native_alias = @native_alias AND org_id = @org_id AND kind = 'video';

-- name: ListVideoJobsForOrgFiltered :many
-- One organization's jobs, newest first, for both surfaces.
--
-- Keyset-ordered on gateway_async_jobs_org_idx (ADR-0185/0195). The data plane
-- names none of the filters and gets the same statement with them all empty --
-- one statement rather than two, because two would be one tested list endpoint
-- and one that quietly drifted.
SELECT * FROM gateway_async_jobs
WHERE org_id = @org_id AND kind = 'video'
  AND (@status::text = '' OR status = @status::text)
  AND (@model_slug::text = '' OR model_slug = @model_slug::text)
  AND (NOT @has_from::boolean OR created_at >= @from_ts::timestamptz)
  AND (NOT @has_to::boolean OR created_at < @to_ts::timestamptz)
  AND (NOT @has_cursor::boolean
       OR created_at < @cursor_created_at::timestamptz
       OR (created_at = @cursor_created_at::timestamptz AND id < @cursor_id))
ORDER BY created_at DESC, id DESC
LIMIT @lim;

-- name: MarkVideoJobSubmitted :exec
UPDATE gateway_async_jobs
   SET upstream_id = @upstream_id, upstream_status = @upstream_status,
       status = @status, submitted_at = now(), next_poll_at = now() + @poll_after::interval
 WHERE id = @id;

-- name: ClaimDueVideoJobs :many
-- One statement is the lease: the rows selected are the rows whose next poll is
-- pushed out, so a second replica running concurrently takes a different batch.
UPDATE gateway_async_jobs
   SET next_poll_at = now() + @lease::interval, last_polled_at = now(),
       poll_attempts = poll_attempts + 1
 WHERE id IN (
     SELECT id FROM gateway_async_jobs
      WHERE kind = 'video' AND status IN ('queued', 'in_progress')
        AND next_poll_at <= now()
      ORDER BY next_poll_at
      LIMIT @lim
      FOR UPDATE SKIP LOCKED
 )
RETURNING *;

-- name: MarkVideoJobProgress :exec
UPDATE gateway_async_jobs
   SET status = @status, upstream_status = @upstream_status, progress = @progress,
       next_poll_at = now() + @poll_after::interval
 WHERE id = @id AND status IN ('queued', 'in_progress');

-- name: MarkVideoJobNotFound :one
UPDATE gateway_async_jobs
   SET not_found_count = not_found_count + 1, next_poll_at = now() + @poll_after::interval
 WHERE id = @id
RETURNING not_found_count;

-- name: MarkVideoJobTerminal :execrows
-- The terminal transition and nothing else. Settlement is a separate statement
-- in the same transaction, guarded on settlement_state, so that a crash between
-- the two leaves a row the sweeper can still finish.
UPDATE gateway_async_jobs
   SET status = @status, upstream_status = @upstream_status, terminal_at = now(),
       error_code = @error_code, error_message = @error_message,
       upstream_artifact_ref = @upstream_artifact_ref,
       upstream_artifact_expires_at = @upstream_artifact_expires_at,
       progress = CASE WHEN @status::text = 'completed' THEN 100 ELSE progress END,
       next_poll_at = NULL
 WHERE id = @id AND status IN ('queued', 'in_progress');

-- name: SettleVideoJob :execrows
-- The idempotency guard for money. A duplicate poll, a racing replica and a
-- job retry all converge here, and only one of them updates a row.
--
-- `orphaned` is in the list because that job still owes a charge -- its
-- reservation was reclaimed while it was running, and it settles late through
-- the replay queue. Leaving it out made that whole branch unreachable: the
-- guard matched nothing, the transaction rolled back, and the charge was lost
-- silently.
UPDATE gateway_async_jobs
   SET settlement_state = 'settled', charged_nano = @charged_nano
 WHERE id = @id AND settlement_state IN ('held', 'protected', 'orphaned');

-- name: VoidVideoJob :execrows
UPDATE gateway_async_jobs
   SET settlement_state = 'voided', charged_nano = 0
 WHERE id = @id AND settlement_state IN ('held', 'protected');

-- name: ProtectVideoJobHold :exec
UPDATE gateway_async_jobs
   SET settlement_state = 'protected', hold_expires_at = NULL
 WHERE id = @id AND settlement_state = 'held';

-- name: OrphanVideoJobHold :exec
UPDATE gateway_async_jobs SET settlement_state = 'orphaned'
 WHERE id = @id AND settlement_state IN ('held', 'protected');

-- name: RecordVideoJobArtifact :exec
UPDATE gateway_async_jobs
   SET artifact_key = @artifact_key, artifact_bytes = @artifact_bytes,
       artifact_content_type = @artifact_content_type, artifact_fetched_at = now()
 WHERE id = @id;

-- name: ListUnsubmittedVideoJobs :many
-- A hold was taken and nothing was ever submitted: the process died between the
-- two. Bounded by age so a job still inside its submit call is not swept.
SELECT * FROM gateway_async_jobs
WHERE kind = 'video' AND status = 'queued' AND upstream_id = '' AND submitted_at IS NULL
  AND created_at < now() - @older_than::interval
LIMIT @lim;

-- name: ListStaleVideoJobs :many
-- Never reached a terminal state within twice its model's own ceiling.
--
-- Per job rather than one interval for all of them: the ceiling is a property
-- of the model, and a single global bound either expires a long render that was
-- about to succeed or leaves a dead short job holding a reservation for hours.
SELECT * FROM gateway_async_jobs
WHERE kind = 'video' AND status IN ('queued', 'in_progress')
  AND created_at < now() - make_interval(secs => max_job_seconds * 2)
LIMIT @lim;

-- name: ListExpiredVideoJobs :many
SELECT * FROM gateway_async_jobs
WHERE kind = 'video' AND expires_at <= now()
LIMIT @lim;

-- name: CountStuckMoneyJobs :one
-- Terminal jobs whose money never moved: the operator's repair queue.
--
-- The predicate is `gateway_async_jobs_stuck_money_idx`'s, verbatim. That index
-- was created with a comment calling this "the operator's repair queue" and then
-- had no reader at all -- the state was absent from the API, the console and the
-- alerts at once, which is the one shape in which a money bug keeps its own
-- secret. A row here is a customer either overcharged or not charged.
--
-- It does not filter on `kind`, and neither does the index. A second kind of
-- asynchronous job would reach terminal with an unmoved reservation the same
-- way, and a queue that silently covered only one of them would be worse than
-- no queue: it would read as "nothing stuck".
--
-- Two numbers rather than one, because a count alone cannot be triaged:
-- `oldest_terminal_at` separates a live incident from a single row stranded a
-- month ago.
--
-- It deliberately does **not** sum `hold_nano`. A hold is denominated in its
-- organisation's wallet currency, `wallets.currency` is per-organisation, and
-- that table is Cloud's -- this query ships in Community too, where there are
-- no wallets at all. So the sum would be either a cross-currency addition or a
-- join this module is not allowed to write. A total that is wrong is worse here
-- than no total: this readout exists to be believed.
SELECT count(*)::bigint              AS jobs,
       min(terminal_at)::timestamptz AS oldest_terminal_at
FROM gateway_async_jobs
WHERE settlement_state IN ('held', 'protected')
  AND status IN ('completed', 'failed', 'canceled', 'expired');

-- name: ClearVideoJobArtifact :exec
-- Forget the stored object without forgetting the job.
--
-- Retention is about the media, and a row whose money has not moved may not be
-- deleted (see videoDeleteRefusal): it is the only thing pointing at an
-- outstanding reservation, and it is what the operator's repair queue reads. So
-- the sweep drops the artifact and keeps the row.
--
-- The key is cleared rather than merely deleted upstream, because the sweep
-- selects on `expires_at` and would otherwise pick the same row up every pass
-- and re-issue a delete for an object that is already gone.
UPDATE gateway_async_jobs
   SET artifact_key = '', artifact_bytes = 0, artifact_content_type = '', artifact_fetched_at = NULL
 WHERE id = @id;

-- name: DeleteVideoJob :exec
DELETE FROM gateway_async_jobs WHERE id = @id;

-- name: GetVideoJobRoute :one
-- The route a job was pinned to, loaded by id and without any admission check.
--
-- Polling needs two things: where to send the request and which credential
-- created the upstream job. It does not need to re-decide whether the model is
-- still admissible -- doing that made an ordinary catalog edit (hiding a model,
-- renaming an upstream model, which resets probe verdicts) turn every in-flight
-- job into a failed one while the upstream kept generating.
--
-- It carries `video_envelope` because how far a job can be stopped is declared
-- there, and the catalogue publishes it from there: reading it from anywhere
-- else makes the two disagree (ADR-0221).
SELECT r.id, r.provider_model_id, r.headers AS route_headers, r.video_envelope,
       p.id AS provider_id, p.slug AS provider_slug, p.vendor, p.base_url,
       p.headers AS provider_headers, p.transport AS provider_transport
FROM model_routes r JOIN providers p ON p.id = r.provider_id
WHERE r.id = $1;

-- name: ListVideoRouteEnvelopes :many
-- The declared envelope of several routes at once.
--
-- The console's job list needs one field off each row's route -- how far that
-- job can still be stopped -- and asking per row turned a page of a hundred
-- jobs into a hundred and one queries for a field most of those rows never
-- render.
SELECT id, video_envelope FROM model_routes WHERE id = ANY(@ids::uuid[]);

-- name: GetVideoJobProviderKey :one
-- The exact shared credential a job was created with.
SELECT id, secret_enc FROM provider_keys WHERE id = $1;
