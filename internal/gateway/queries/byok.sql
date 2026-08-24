-- Organization-supplied upstream credentials (BYOK).
--
-- Every query here carries an explicit org predicate. These are called from the
-- console, where the org comes from a URL parameter -- user-controlled input --
-- so they must run inside an org-scoped transaction: row-level security is the
-- third line of defence, the explicit predicate is the second, and both are
-- required. See docs/design/upstream-credentials.md.

-- name: ListOrgProviderKeys :many
-- Deliberately does not return secret_enc: once ciphertext leaves the crypto
-- boundary it can be carried off by logs, serialisation or error messages.
--
-- Ordered by (vendor, name) rather than by time, and paginated on that same
-- key. This is a configuration screen, not a feed: an organization holding
-- azure-eu, azure-us and two OpenAI accounts reads it by platform, and ordering
-- it newest-first to suit a created_at cursor would rearrange the page to serve
-- the pagination rather than the reader.
--
-- No id tiebreaker, and that is a property of the schema rather than an
-- omission: `UNIQUE (org_id, name)` makes name alone unique within an
-- organization, so (vendor, name) is already a total order.
SELECT id, org_id, vendor, name, base_url, status,
       secret_hint, allow_fallback, last_verified_at, created_at
FROM org_provider_keys
WHERE org_id = @org_id
  AND (NOT @has_cursor::boolean
       OR (vendor, name) > (@cursor_vendor::text, @cursor_name::text))
ORDER BY vendor, name
LIMIT @lim;

-- name: CreateOrgProviderKey :one
-- secret_enc is inserted as a placeholder and back-filled once the row id is
-- known, because the id is used as the AEAD associated data. The two steps are
-- deliberate: binding the AAD to the row id makes the ciphertext unusable if it
-- is copied into another row.
INSERT INTO org_provider_keys (org_id, vendor, name, base_url, secret_enc, secret_hint, allow_fallback)
VALUES ($1, $2, $3, sqlc.narg('base_url'), '\x00'::bytea, $4, @allow_fallback)
RETURNING id, org_id, vendor, name, base_url, status,
          secret_hint, allow_fallback, last_verified_at, created_at;

-- name: SetOrgProviderKeySecret :exec
UPDATE org_provider_keys SET secret_enc = $2 WHERE id = $1 AND org_id = $3;

-- name: DeleteOrgProviderKey :execrows
DELETE FROM org_provider_keys WHERE id = $1 AND org_id = $2;

-- name: GetOrgProviderKeySecret :one
-- Fetch the ciphertext for decryption (connectivity test and dataplane
-- routing). The org predicate means a caller holding someone else's key id
-- still reads nothing.
SELECT id, org_id, vendor, base_url, secret_enc, status
FROM org_provider_keys
WHERE id = $1 AND org_id = $2;

-- name: SetOrgProviderKeyStatus :exec
-- Shared by the connectivity test and the dataplane: a successful verification
-- sets status to active and stamps the time, a 401/403 from upstream sets it to
-- invalid.
UPDATE org_provider_keys
SET status = $3,
    last_verified_at = CASE WHEN $3 = 'active' THEN now() ELSE last_verified_at END
WHERE id = $1 AND org_id = $2;

-- name: DefaultUpstreamForVendor :one
-- Resolve the default endpoint for a vendor when a BYOK row leaves base_url
-- empty (empty means "use the deployment's default endpoint for this vendor").
-- Picks the enabled provider with the lowest slug so the result is
-- deterministic; if there is none the row comes back empty and the caller asks
-- the organization to fill in base_url explicitly -- guessing an endpoint is worse
-- than an error.
-- Membership is by vendor rather than by protocol, because the endpoint has to be
-- one this credential is actually valid at: every provider speaking a protocol
-- was the old criterion, and it would hand a organization's key the address of a
-- different company that happens to speak the same protocol.
-- base_url and transport come back together, from one row, because they are one
-- answer: the address of an upstream is the endpoint plus the profile that says
-- how to address it. Fetching them separately would let a connectivity test
-- reach one provider's endpoint carrying another provider's path override --
-- and the profile is now certainly the right one, since it belongs to the same
-- platform the credential does.
SELECT base_url, transport, protocols FROM providers
WHERE vendor = sqlc.arg('vendor')::text AND enabled
ORDER BY slug
LIMIT 1;

-- name: ListActiveBYOKForOrg :many
-- Every usable BYOK credential an org holds (dataplane hot path).
--
-- All of them in one query rather than one query per vendor: a model's
-- candidates can span several platforms, and the credential that applies is
-- decided per candidate. Asking per candidate would put a database round trip
-- inside the rotation loop; asking once per request and choosing in memory keeps
-- it at one.
--
-- Deliberately not inside an org-scoped transaction: here the org comes from an
-- authenticated API key rather than user-controlled input, so the explicit
-- predicate is the whole requirement, and the hot path should not open an extra
-- transaction per request.
-- Only active rows: invalid (upstream rejected it) and disabled (the organization
-- turned it off) both stay out of routing.
-- With several credentials for one vendor the oldest wins -- stability is all
-- that is needed, and there is no rotation here. Rotation is a property of a
-- shared credential pool; a organization's own quota does not need us spreading load
-- across their keys. The ordering is what makes "the oldest" well defined for
-- the caller collapsing this list per vendor.
SELECT id, vendor, base_url, secret_enc, allow_fallback
FROM org_provider_keys
WHERE org_id = @org_id AND status = 'active'
ORDER BY vendor, created_at;

-- name: ListBYOKVendors :many
-- The vendors this deployment actually routes to, which is the set a organization may
-- usefully supply a credential for.
--
-- A key for a vendor with no enabled provider can never apply to anything: there
-- is no candidate for it to serve, so it would sit in the organization's list looking
-- configured and never take effect. Refusing it at the point of entry is the
-- only place that difference is visible.
--
-- The custom vendor is excluded: it means "an upstream this build has no entry
-- for", which is not an identity a organization can hold an account with.
SELECT DISTINCT vendor FROM providers
WHERE enabled AND vendor <> 'custom'
ORDER BY vendor;
