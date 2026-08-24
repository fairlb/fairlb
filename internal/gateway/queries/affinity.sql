-- name: UpsertResourceAffinity :exec
INSERT INTO resource_affinities (
    org_id, protocol, resource_type, upstream_id, model_id, route_id,
    provider_id, provider_key_id, org_provider_key_id, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.narg('provider_key_id'),
          sqlc.narg('org_provider_key_id'), $8)
ON CONFLICT (org_id, protocol, resource_type, upstream_id) DO UPDATE SET
    model_id = excluded.model_id,
    route_id = excluded.route_id,
    provider_id = excluded.provider_id,
    provider_key_id = excluded.provider_key_id,
    org_provider_key_id = excluded.org_provider_key_id,
    expires_at = excluded.expires_at,
    updated_at = now();

-- name: GetResourceAffinity :one
SELECT a.*, m.slug AS model_slug
  FROM resource_affinities a
  JOIN models m ON m.id = a.model_id
 WHERE a.org_id = $1
   AND a.protocol = $2
   AND a.resource_type = $3
   AND a.upstream_id = $4
   AND a.expires_at > now();

-- name: DeleteResourceAffinity :exec
DELETE FROM resource_affinities
 WHERE org_id = $1 AND protocol = $2 AND resource_type = $3 AND upstream_id = $4;

-- name: DeleteExpiredResourceAffinities :execrows
DELETE FROM resource_affinities WHERE expires_at <= now();

-- name: GetProviderKeyByID :one
SELECT id, provider_id, name, secret_enc
  FROM provider_keys
 WHERE id = $1 AND status = 'active';

-- name: GetActiveOrgProviderKeyByID :one
SELECT id, org_id, vendor, base_url, secret_enc, allow_fallback
  FROM org_provider_keys
 WHERE id = $1 AND org_id = $2 AND status = 'active';
