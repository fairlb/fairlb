-- +goose Up
-- Core schema: the foundation every deployment needs.
--
-- # What lives here
--
-- Admin identity, organizations, settings, API keys and the audit log. These
-- are the tables the product segment has hard foreign keys into (`org_id ->
-- orgs`, `updated_by -> staff_users`), so they have to exist before it is
-- applied — that is the criterion for a table being in this file rather than
-- in a later segment.
--
-- Migration files are applied in numeric order, and the gap between this file's
-- number and the product segment's is deliberate: a deployment can add its own
-- segment in between without touching either of the two files it sits between.
--
-- # Only the columns every deployment needs
--
-- Product-specific fields live in extension tables owned by that product. The
-- core schema is never rewritten by a later migration segment.

-- Identity tables store email as citext, so lookups are case-insensitive
-- without every query having to remember to lower() both sides.
CREATE EXTENSION IF NOT EXISTS citext;

-- The single updated_at trigger function. Tables in later segments use it too,
-- which is why it has to be defined here.
-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- Idempotency keys: caches the first result of a POST that carries an
-- Idempotency-Key header, so a retry replays that answer instead of performing
-- the operation twice. `scope` is the authenticated principal, so one caller's
-- key can never collide with another's.
CREATE TABLE idempotency_keys (
    id               uuid PRIMARY KEY DEFAULT uuidv7(),
    scope            text NOT NULL,
    idempotency_key  text NOT NULL,
    request_hash     text NOT NULL,
    status           text NOT NULL DEFAULT 'in_flight' CHECK (status IN ('in_flight', 'completed')),
    response_status  int,
    response_headers jsonb,
    response_body    bytea,
    expires_at       timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope, idempotency_key)
);

CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

-- Administrator identity. A single-tenant deployment's admin account lives here
-- too: there is no separate operator identity to distinguish it from, and every
-- deployment serves the same `/api/staff/v1` routes off the same table. The
-- only difference is which role is filled in.
CREATE TABLE staff_users (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    email         citext NOT NULL UNIQUE,
    password_hash text NOT NULL,
    name          text NOT NULL DEFAULT '',
    role          text NOT NULL CHECK (role IN ('superadmin', 'operator')),
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE staff_sessions (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    staff_user_id uuid NOT NULL REFERENCES staff_users (id) ON DELETE CASCADE,
    token_hash    text NOT NULL UNIQUE,
    ip            inet,
    user_agent    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz
);

CREATE INDEX staff_sessions_staff_user_id_idx ON staff_sessions (staff_user_id) WHERE revoked_at IS NULL;
CREATE INDEX staff_sessions_expires_at_idx ON staff_sessions (expires_at);

-- Organizations. A single-tenant deployment collapses to one default org,
-- created on first start, and hides the concept from its UI entirely. The table
-- still exists because `org_id` and the row-level security policies below are
-- the same mechanism in every deployment — a single-tenant install runs the
-- real isolation code with one row rather than a stubbed-out version of it.
--
-- `status` belongs here for the same reason: the check that rejects writes for
-- any status not on the allowlist is one shared implementation, and it must
-- fail closed everywhere. Where every org is active, that check simply always
-- passes.
CREATE TABLE orgs (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    slug       text NOT NULL UNIQUE,
    name       text NOT NULL,
    kind       text NOT NULL CHECK (kind IN ('personal', 'team')),
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'pending_delete')),
    -- Settlement currency. It belongs to the organization rather than to any
    -- balance-keeping table: usage accounting and reporting are per-currency
    -- and must work in a deployment that keeps no balances at all.
    currency   text NOT NULL DEFAULT 'USD',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Runtime settings, keyed by a dotted namespace so each subsystem registers its
-- own keys. Reads go through a cache that is invalidated across processes by
-- LISTEN/NOTIFY, so a setting changed in one replica takes effect in all of
-- them without a restart.
CREATE TABLE settings (
    key         text PRIMARY KEY,
    -- Exactly one of value / secret_enc is present: a plain key stores its JSON
    -- value, a secret key stores nonce||ciphertext sealed under the master key
    -- with the key name as AAD. The registry decides which a key is; the
    -- constraint keeps a row from being both or neither.
    value       jsonb,
    secret_enc  bytea,
    -- Display mask of a secret, computed at write time. The ciphertext is
    -- deliberately not readable back through the settings surface; this is
    -- how an operator recognizes which credential is stored.
    secret_hint text,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  text NOT NULL DEFAULT 'system',
    CHECK ((value IS NOT NULL) <> (secret_enc IS NOT NULL)),
    CHECK ((secret_enc IS NULL) = (secret_hint IS NULL))
);

-- Virtual API keys: what callers authenticate with on the data plane. The
-- limits declared here (model access, spend, request and token rates) are
-- enforced on every request before it is forwarded upstream.
CREATE TABLE api_keys (
    id                   uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id               uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name                 text NOT NULL,
    prefix               text NOT NULL,  -- first 8 characters, for display
    key_hash             text NOT NULL UNIQUE,  -- SHA-256; the plaintext is returned once, at creation
    scopes               text[] NOT NULL DEFAULT '{inference}',
    -- Which models this key may call, as two columns rather than one.
    --
    -- The pair exists because "every model" and "no model at all" are both
    -- real answers, and a bare list cannot hold them both: an empty list has to
    -- mean one of them, and whichever it means, the other becomes
    -- inexpressible. Reading emptiness as "unrestricted" is the dangerous
    -- direction -- clearing the last entry would then silently grant
    -- everything -- so the intent is stated instead of inferred.
    --
    -- allow_all_models = true is the default: a key restricts nothing of its
    -- own and reaches whatever its organization's admission tier allows. With
    -- it false, allowed_models is the exact set, and an empty set really does
    -- refuse every model.
    --
    -- The CHECK keeps the stored state unambiguous: a non-empty list always
    -- means the key is restricted, so nobody reading this table has to consult
    -- a second column before believing the first. The write path clears the
    -- list when the switch is turned on.
    allow_all_models     boolean NOT NULL DEFAULT true,
    allowed_models       text[] NOT NULL DEFAULT '{}',
    CONSTRAINT api_keys_allow_all_has_no_list CHECK (
        NOT allow_all_models OR cardinality(allowed_models) = 0
    ),
    spend_limit_nano     bigint CHECK (spend_limit_nano > 0),
    spend_limit_interval text CHECK (spend_limit_interval IN ('total', 'monthly', 'daily')),
    -- Request and token ceilings per minute; NULL means no ceiling. They are
    -- the key's own share, checked after the organization's own limits, so a
    -- key can only ever be narrower than the organization it belongs to.
    rate_limit_rpm       int CHECK (rate_limit_rpm > 0),
    rate_limit_tpm       int CHECK (rate_limit_tpm > 0),
    tags                 jsonb NOT NULL DEFAULT '{}',
    total_spent_nano     bigint NOT NULL DEFAULT 0 CHECK (total_spent_nano >= 0),
    status               text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    last_used_at         timestamptz,  -- written when the data plane loads the identity; a cache hit does not write
    expires_at           timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX api_keys_org_active_name_uniq ON api_keys (org_id, name) WHERE status = 'active';
CREATE INDEX api_keys_org_active_idx ON api_keys (org_id) WHERE status = 'active';
CREATE INDEX api_keys_org_created_idx ON api_keys (org_id, created_at DESC, id DESC);

-- Running total per key per day, which is what a daily spend limit is checked
-- against. A deployment that keeps no balances still settles into this table,
-- so the spend-limit check on the data plane keeps working there.
CREATE TABLE api_key_daily_spend (
    api_key_id uuid NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    day        date NOT NULL,
    spent_nano bigint NOT NULL DEFAULT 0 CHECK (spent_nano >= 0),
    PRIMARY KEY (api_key_id, day)
);

-- Watermarks for incremental aggregation: each rollup job records how far it
-- has consumed so the next run picks up exactly where the last one stopped.
CREATE TABLE posting_watermarks (
    key        text PRIMARY KEY,
    watermark  timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Audit log, RANGE-partitioned by month so that dropping old data is a DETACH
-- rather than a DELETE over a table that only ever grows.
CREATE TABLE audit_logs (
    id          uuid NOT NULL DEFAULT uuidv7(),
    actor_type  text NOT NULL CHECK (actor_type IN ('user', 'staff', 'system')),
    actor_id    uuid,                       -- NULL when anonymous, e.g. a failed sign-in attempt
    org_id      uuid,                       -- recorded as it was, no FK; NULL = not scoped to an org
    action      text NOT NULL,
    target_type text NOT NULL DEFAULT '',
    target_id   text NOT NULL DEFAULT '',   -- text so non-uuid targets fit too, e.g. a settings key
    meta        jsonb NOT NULL DEFAULT '{}',
    ip          inet,
    request_id  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE INDEX audit_logs_org_idx ON audit_logs (org_id, created_at DESC);
CREATE INDEX audit_logs_actor_idx ON audit_logs (actor_type, actor_id, created_at DESC);
-- 运营台的审计视图**默认不带任何过滤**（每个条件都是可选的），排序恒为
-- (created_at DESC, id DESC)。上面两条索引都不以 created_at 打头，所以那个
-- 默认页是全扫加排序。这一条让它变成索引扫描。
CREATE INDEX audit_logs_recent_idx ON audit_logs (created_at DESC, id DESC);

-- Catch-all partition: a row for a month nobody pre-created lands here, so an
-- audit insert can never fail for want of a partition. A periodic job keeps
-- creating months ahead of time, so in a healthy system this stays empty —
-- and if it is not empty, that job has stopped.
CREATE TABLE audit_logs_default PARTITION OF audit_logs DEFAULT;

-- This month and the next, created at deploy time; from then on the periodic
-- job stays ahead. Partition names come from the UTC month so they do not
-- depend on the session time zone, and the bounds are explicit UTC timestamps
-- for the same reason.
-- +goose StatementBegin
DO $$
DECLARE
    b0 timestamp := date_trunc('month', now() AT TIME ZONE 'UTC');
    b1 timestamp := b0 + interval '1 month';
    b2 timestamp := b0 + interval '2 month';
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS audit_logs_%s PARTITION OF audit_logs FOR VALUES FROM (%L) TO (%L)',
        to_char(b0, 'YYYY_MM'), b0 AT TIME ZONE 'UTC', b1 AT TIME ZONE 'UTC');
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS audit_logs_%s PARTITION OF audit_logs FOR VALUES FROM (%L) TO (%L)',
        to_char(b1, 'YYYY_MM'), b1 AT TIME ZONE 'UTC', b2 AT TIME ZONE 'UTC');
END $$;
-- +goose StatementEnd

CREATE TRIGGER idempotency_keys_set_updated_at
    BEFORE UPDATE ON idempotency_keys
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER staff_users_set_updated_at BEFORE UPDATE ON staff_users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER orgs_set_updated_at BEFORE UPDATE ON orgs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER settings_set_updated_at BEFORE UPDATE ON settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER api_keys_set_updated_at BEFORE UPDATE ON api_keys
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER posting_watermarks_set_updated_at BEFORE UPDATE ON posting_watermarks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The application role. Request paths that act on behalf of an organization
-- `SET LOCAL ROLE` to it and are therefore subject to the row-level security
-- policies below; system paths keep the connection role, which owns the tables
-- and so is not subject to them unless FORCE ROW LEVEL SECURITY is set.
--
-- It is created here rather than in a later segment because tables in later
-- segments need these grants too, and not every deployment applies them.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'fairlb_app') THEN
        CREATE ROLE fairlb_app NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO fairlb_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO fairlb_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO fairlb_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO fairlb_app;
-- Let the connection role (migrations and the service share one DATABASE_URL)
-- SET ROLE to the application role.
GRANT fairlb_app TO current_user;

-- Org-scoped policies. If `app.org_id` was never set, current_setting raises
-- rather than matching nothing — the failure mode is an error, not a silently
-- unfiltered read.
ALTER TABLE orgs ENABLE ROW LEVEL SECURITY;
CREATE POLICY orgs_isolation ON orgs
    USING (id = current_setting('app.org_id')::uuid);

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY api_keys_isolation ON api_keys
    USING (org_id = current_setting('app.org_id')::uuid);

-- No org_id column here, so the scope comes indirectly through the owning key.
-- The api_keys policy applies inside the subquery as well, so both tables end
-- up filtered by the same condition.
ALTER TABLE api_key_daily_spend ENABLE ROW LEVEL SECURITY;
CREATE POLICY api_key_daily_spend_isolation ON api_key_daily_spend
    USING (api_key_id IN (SELECT id FROM api_keys
                          WHERE org_id = current_setting('app.org_id')::uuid));

-- Row-level security is enabled in the same migration that creates the table,
-- never in a follow-up: a table that exists for a while without a policy is a
-- table that was readable across orgs for a while. Rows with no org are
-- invisible to org-scoped paths; administrative reads run as the connection
-- role and see everything.
ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
CREATE POLICY audit_logs_isolation ON audit_logs
    USING (org_id = current_setting('app.org_id')::uuid);
