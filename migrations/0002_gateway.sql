-- +goose Up
-- Gateway schema: providers, model catalog and routing, usage and pricing.
--
-- This file declares the complete product schema for an empty database. Every
-- statement creates an object used by the current application.
--
-- Note what that implies once a database exists: goose records which versions
-- have been applied and never re-runs them, so editing this file changes what a
-- *new* database gets and nothing else. Changing an existing deployment always
-- means adding a migration.

-- ===== Providers and credentials =====

-- Upstream endpoints. `vendor` says whose API platform this is and `protocols`
-- which API dialects it speaks; requests are forwarded within a protocol, never
-- translated across them.
--
-- `headers` is the provider-level header map applied to outbound requests after
-- the standard headers are set: each key is overwritten or appended, and an
-- empty string value removes that header. Values may contain the `${api_key}`
-- placeholder, substituted at request time with the decrypted credential, so
-- the credential itself still exists only encrypted in provider_keys and never
-- in plaintext in this table.
CREATE TABLE providers (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    slug       text NOT NULL UNIQUE,
    -- Which API platform this upstream belongs to, as a slug from the vendor
    -- registry in the catalog package. It is an identity, not behaviour: it
    -- decides which organization-supplied credential may be used for this provider,
    -- which reference-price scope its routes resolve against, whether model
    -- discovery has anything to call, and what an operator is shown. Nothing on
    -- the data plane branches on it.
    --
    -- The CHECK fixes only the shape. The set of valid vendors lives in code
    -- and is enforced when writing, because that list ships with the binary:
    -- making it a database enumeration would mean a schema change every time a
    -- platform is added, and would let a deployment carry a vendor its own code
    -- knows nothing about.
    vendor     text NOT NULL CHECK (
                   vendor ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND length(vendor) <= 40
               ),
    name       text NOT NULL DEFAULT '',
    base_url   text NOT NULL,
    enabled    boolean NOT NULL DEFAULT true,
    headers    jsonb NOT NULL DEFAULT '{}' CHECK (
                   jsonb_typeof(headers) = 'object'
                   AND NOT headers @? 'strict $.* ? (!(@.type() == "string"))'
               ),
    -- How to reach this upstream when base_url plus the dialect's defaults are
    -- not enough: how the credential is presented, query parameters every
    -- request needs, path shapes that differ from the standard ones, how long a
    -- connection may take to establish, and -- for the hosted platforms that
    -- publish a dialect with the model in the address rather than in the body --
    -- which envelope to use. The gateway validates the contents; the CHECK here
    -- only fixes the container, because a scalar or an array stored in this
    -- column would fail at request time on the data plane rather than at save
    -- time in front of the person who typed it.
    --
    -- *No credential ever goes in here.* This column is returned whole by the
    -- admin API alongside the rest of the provider record, so it holds only the
    -- non-secret half of an authentication setting -- the region a request is
    -- signed for, not the key it is signed with. Secrets live in provider_keys,
    -- encrypted, and no endpoint reads them back.
    --
    -- Empty is the normal case. Most upstreams are fully described by base_url.
    transport  jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(transport) = 'object'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- "disabled by health checks" and "disabled by a human" must be tellable
    -- apart: only the former may be re-enabled automatically when the provider
    -- recovers. Collapsing them means an operator takes a provider out of
    -- rotation and a probe puts it back five minutes later.
    auto_disabled boolean NOT NULL DEFAULT false,
    -- Procurement cost as a single scalar: cost = list price x this multiplier.
    -- It is deliberately not versioned. Versioning protects the numbers that
    -- appear on a customer's bill, and procurement cost never does; each
    -- request snapshots what it was charged, so changing this scalar cannot
    -- distort historical margin. The default of 10000 records cost at list
    -- price, which is the conservative lower bound on margin.
    cost_multiplier_bps integer NOT NULL DEFAULT 10000
        CHECK (cost_multiplier_bps BETWEEN 1 AND 100000),
    -- Which dialects this provider speaks. A relay that speaks several is one
    -- row, not one copy per dialect each carrying its own credentials and
    -- circuit-breaker state. This column plays no part in data-plane semantics
    -- — the outbound auth header follows the protocol of the incoming request —
    -- it only constrains configuration and filters candidates. What a route
    -- can actually serve on a protocol is what model_route_probes has observed.
    protocols  text[] NOT NULL,
    -- What this upstream account will take: requests and tokens per minute, and
    -- how many calls may be in flight at once. NULL means the two rate ceilings
    -- are not declared and nothing is measured against them.
    --
    -- They describe the account, not the model, because that is the shape the
    -- quota actually has: a key is rated at so many requests per minute across
    -- everything it serves. An upstream account with a different quota is a
    -- different account, and therefore a different row here -- which is also
    -- what gives it its own credentials, its own circuit and its own share of
    -- the traffic.
    --
    -- They are a filter, not a weight. A provider with no allowance left this
    -- minute is skipped exactly as a cooling-down one is, and the next
    -- candidate is tried; its configured share of the traffic never quietly
    -- erodes. See docs/design/failover-and-cooldowns.md.
    --
    -- max_concurrency has a value rather than a NULL because there is always
    -- some number of simultaneous calls beyond which an upstream stops
    -- answering, and a gateway with no opinion at all queues until something
    -- upstream times out. 64 is the default it used to carry in code.
    rate_limit_rpm  int CHECK (rate_limit_rpm > 0),
    rate_limit_tpm  int CHECK (rate_limit_tpm > 0),
    max_concurrency int NOT NULL DEFAULT 64 CHECK (max_concurrency > 0),
    -- Not redundant with the column type: this is what rejects the empty array.
    CONSTRAINT providers_protocols_check CHECK (
        cardinality(protocols) > 0
        AND protocols <@ ARRAY['openai', 'anthropic', 'gemini']::text[]
    )
);

-- Credentials the deployment itself holds for a provider. Circuit-breaker state
-- lives in memory; what is persisted here is only what has to survive a
-- restart or be visible to an operator.
CREATE TABLE provider_keys (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    provider_id    uuid NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    name           text NOT NULL DEFAULT '',
    secret_enc     bytea NOT NULL,           -- AES-256-GCM; the row id is the AAD, so a ciphertext cannot be moved between rows
    status         text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    last_error     text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- Display mask. The ciphertext is deliberately not readable back, so this
    -- is how an operator recognizes which credential a row holds. It is not
    -- part of the secret; disclosing it weakens nothing.
    secret_hint      text NOT NULL DEFAULT '',
    -- Result of the last connectivity test. It has to be visible in the list,
    -- otherwise finding out whether a credential still works means making a
    -- real upstream call, which costs money.
    last_verified_at timestamptz,
    UNIQUE (provider_id, name)
);
CREATE INDEX provider_keys_provider_idx ON provider_keys (provider_id) WHERE status = 'active';

-- Bring-your-own-key: credentials an organization supplies for itself, used
-- instead of the deployment's own when a request is routed to that vendor.
CREATE TABLE org_provider_keys (
    id               uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id           uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    -- Which vendor this credential is with. The organization is stating "I have an
    -- account at this platform", and that sentence has no meaning at the level
    -- of a protocol: the OpenAI dialect is spoken by dozens of companies, so a
    -- credential keyed by dialect would be offered to every one of them that the
    -- routing happened to reach. Same shape rule as providers.vendor; which
    -- slugs exist is decided in code.
    vendor           text NOT NULL CHECK (
                         vendor ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND length(vendor) <= 40
                     ),
    name             text NOT NULL,
    base_url         text,                   -- NULL = use the deployment's default endpoint for that vendor
    secret_enc       bytea NOT NULL,
    status           text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'invalid', 'disabled')),
    last_verified_at timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    -- Display mask, same shape as provider_keys.secret_hint: derived from the
    -- credential itself, so it identifies the row without revealing it.
    secret_hint      text NOT NULL DEFAULT '',
    -- Whether a failure with the organization's own credential may fall back to
    -- the deployment's. The default of false is deliberate: falling back
    -- silently means that request is billed at the full rate rather than at the
    -- service fee, and a surprising bill is worse than a failed request — the
    -- failure is visible immediately and can be fixed.
    allow_fallback   boolean NOT NULL DEFAULT false,
    UNIQUE (org_id, name)
);
ALTER TABLE org_provider_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY org_provider_keys_isolation ON org_provider_keys
    USING (org_id = current_setting('app.org_id')::uuid);

-- ===== Model catalog and routing =====

-- The catalog callers see. This table carries no prices: the single source of
-- price is model_pricing, and this table only describes what a model is.
--
-- A model owns no protocol. Which protocols it can be called on is decided by
-- its routes: every protocol a route's provider speaks is a protocol the model
-- is reachable on, and the same slug may be reached on /v1/chat/completions
-- through one provider and on /v1/messages through another (or the same one).
-- The gateway still never translates between protocols -- a request passes
-- through on the protocol it arrived on -- the model simply does not get to
-- pick which protocol that is.
--
-- `capabilities` is display metadata. What an endpoint can actually serve is
-- what model_route_probes has observed — a capability advertised here with no
-- verified route behind it only gets the caller a 404.
CREATE TABLE models (
    id                  uuid PRIMARY KEY DEFAULT uuidv7(),
    slug                text NOT NULL UNIQUE,          -- e.g. "openai/gpt-5.4"
    display_name        text NOT NULL DEFAULT '',
    context_window      integer NOT NULL DEFAULT 0,
    max_output_tokens   integer NOT NULL DEFAULT 0,    -- the output cap a pre-authorization estimate assumes
    capabilities        jsonb NOT NULL DEFAULT '{}',
    visibility          text NOT NULL DEFAULT 'public' CHECK (visibility IN ('public', 'beta', 'hidden')),
    enabled             boolean NOT NULL DEFAULT true,
    metadata            jsonb NOT NULL DEFAULT '{}',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

-- One route = one deployment of a model on one provider. A model may have many.
--
-- A route does not declare which endpoints it serves. That is a claim about the
-- upstream, and the upstream is the only thing that can answer it, so the
-- answer lives in model_route_probes as something observed rather than typed
-- in: the data plane tries any endpoint of a protocol the provider speaks
-- unless a probe has found it unsupported, and the catalog publishes only the
-- endpoints a probe has found working. The configuration therefore cannot
-- drift from the upstream, because it never restates it.
--
-- `headers` is the route-level header map; where a key also exists at provider
-- level, this one wins.
CREATE TABLE model_routes (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    model_id          uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    provider_id       uuid NOT NULL REFERENCES providers (id) ON DELETE CASCADE,
    provider_model_id text NOT NULL,               -- the model name upstream expects; substituted on the way out
    priority          integer NOT NULL DEFAULT 100,  -- lower first; ties are broken by weight
    weight            integer NOT NULL DEFAULT 1 CHECK (weight >= 0),
    enabled           boolean NOT NULL DEFAULT true,
    headers           jsonb NOT NULL DEFAULT '{}' CHECK (
                          jsonb_typeof(headers) = 'object'
                          AND NOT headers @? 'strict $.* ? (!(@.type() == "string"))'
                      ),
    created_at        timestamptz NOT NULL DEFAULT now(),
    -- Same as providers and models: knowing when a route last changed is what
    -- you want first when diagnosing a routing problem.
    updated_at        timestamptz NOT NULL DEFAULT now(),
    -- Limits belong to the route, not only to the model: the same model can
    -- have different limits on different upstreams, because a relay often
    -- narrows them by its own policy. NULL means "use the model's value".
    context_window    integer,
    max_output_tokens integer,
    -- An open set of upstream behavior flags. Today only
    -- `ignores_max_output_tokens` is read: some upstreams ignore the cap in the
    -- request and return more than was asked for, and a pre-authorization
    -- estimate must then not treat that cap as an upper bound on cost. jsonb
    -- rather than one column per flag, because each flag is read in one or two
    -- places and adding a column for it would cost more than it explains.
    quirks            jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(quirks) = 'object'),
    CONSTRAINT model_routes_caps_positive CHECK (
        coalesce(context_window, 1) > 0 AND coalesce(max_output_tokens, 1) > 0
    ),
    UNIQUE (model_id, provider_id, provider_model_id)
);
CREATE INDEX model_routes_model_idx ON model_routes (model_id) WHERE enabled;

-- A persisted upstream resource can only be addressed through the exact route
-- and exact credential that created it. The org-scoped primary key prevents
-- enumeration across organizations even if two providers issue the same id.
CREATE TABLE resource_affinities (
    org_id               uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    protocol             text NOT NULL CHECK (protocol IN ('openai', 'gemini')),
    resource_type        text NOT NULL CHECK (resource_type IN ('response', 'interaction')),
    upstream_id          text NOT NULL CHECK (upstream_id <> '' AND length(upstream_id) <= 512),
    model_id             uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    route_id             uuid REFERENCES model_routes (id) ON DELETE SET NULL,
    provider_id          uuid REFERENCES providers (id) ON DELETE SET NULL,
    provider_key_id      uuid REFERENCES provider_keys (id) ON DELETE SET NULL,
    org_provider_key_id  uuid REFERENCES org_provider_keys (id) ON DELETE SET NULL,
    expires_at           timestamptz NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, protocol, resource_type, upstream_id),
    CONSTRAINT resource_affinities_one_credential_check CHECK (
        NOT (provider_key_id IS NOT NULL AND org_provider_key_id IS NOT NULL)
    )
);

CREATE INDEX resource_affinities_expiry_idx ON resource_affinities (expires_at);
CREATE INDEX resource_affinities_route_idx ON resource_affinities (route_id);
CREATE TRIGGER resource_affinities_updated_at BEFORE UPDATE ON resource_affinities
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE resource_affinities ENABLE ROW LEVEL SECURITY;
CREATE POLICY resource_affinities_isolation ON resource_affinities
    USING (org_id = current_setting('app.org_id')::uuid);


-- Persisted cooldowns. The circuit breaker's decisions and counters live in
-- memory; this table exists so that "still cooling down" survives a process
-- restart. It is not consulted on the hot path.
CREATE TABLE cooldowns (
    scope      text NOT NULL CHECK (scope IN ('provider', 'provider_key')),
    ref_id     uuid NOT NULL,
    until      timestamptz NOT NULL,
    reason     text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, ref_id)
);

-- ===== Usage detail and rollups =====

-- One row per request: the largest write surface in the database.
--
-- RANGE-partitioned by month from the first day. Retrofitting partitioning onto
-- a table this size is painful enough that it has to be there from the start.
-- Old months are detached and archived, so reclaiming space is a DROP of a
-- partition rather than a DELETE across a table that only grows.
--
-- The absence of foreign keys is deliberate; org_id, api_key_id and hold_id are
-- recorded as they were, not as references. Three reasons: the hot path avoids
-- a referential check per row; rows this table points at are physically removed
-- on their own retention schedule, which would break those references; and
-- deleting an organization must not cascade away the billing record of what it
-- consumed.
--
-- The primary key includes the partition key because PostgreSQL requires it of
-- a partitioned table.
CREATE TABLE usage_logs (
    id                     uuid NOT NULL DEFAULT uuidv7(),
    created_at             timestamptz NOT NULL DEFAULT now(),
    org_id                 uuid NOT NULL,          -- recorded as it was, no FK
    api_key_id             uuid,                   -- recorded as it was; NULL = not an API-key request, e.g. an internal replay
    request_id             text NOT NULL,
    surface                text NOT NULL CHECK (surface IN (
                               'chat_completions', 'messages', 'messages_count_tokens',
                               'responses', 'responses_compact', 'responses_resources', 'responses_input_tokens',
                               'embeddings', 'images', 'generate_content', 'gemini_count_tokens',
                               'gemini_embed_content', 'gemini_batch_embed_contents', 'gemini_interactions'
                           )),
    model_slug             text NOT NULL,
    provider_id            uuid,
    provider_key_id        uuid,
    org_provider_key_id    uuid,
    -- Which route served the request. Two routes can point at the same
    -- provider with different upstream model names, so provider_id alone
    -- cannot say which configuration was exercised.
    route_id               uuid,
    byok                   boolean NOT NULL DEFAULT false,
    route_attempts         integer NOT NULL DEFAULT 1,
    -- The hops that failed before the one recorded above, in the order they
    -- were tried. Empty for a request that succeeded first time, which is
    -- almost all of them.
    --
    -- One request is one row here, always. Recording each attempt as its own
    -- row is the obvious alternative and it is a trap: this table is what
    -- budgets, invoices and rollups are computed from, so the moment one
    -- request can be several rows, every one of those computations has to
    -- start by asking which rows are real -- and the answer can only live in
    -- yet another column that each of them must remember to check. Keeping the
    -- trail inside the row leaves all of them correct by construction.
    --
    -- The winning hop is deliberately not repeated in here: it is already the
    -- row's own provider_id, route_id and provider_key_id.
    attempts               jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(attempts) = 'array'),
    stream                 boolean NOT NULL DEFAULT false,
    status                 text NOT NULL CHECK (status IN ('ok', 'upstream_error', 'client_error', 'canceled')),
    error_code             text NOT NULL DEFAULT '',
    http_status            integer NOT NULL DEFAULT 0,
    -- int4 is enough for the token count of one request; money is always
    -- bigint nano units, never floating point
    tokens_in              integer NOT NULL DEFAULT 0,
    tokens_out             integer NOT NULL DEFAULT 0,
    tokens_cached_read     integer NOT NULL DEFAULT 0,
    tokens_reasoning       integer NOT NULL DEFAULT 0,
    usage_estimated        boolean NOT NULL DEFAULT false,
    upstream_cost_usd_nano bigint NOT NULL DEFAULT 0,
    charged_nano           bigint NOT NULL DEFAULT 0,
    charged_currency       text NOT NULL DEFAULT 'USD',
    fx_rate                numeric,                -- snapshotted per request so the row recomputes on its own; 1 for USD
    hold_id                uuid,                   -- recorded as it was, no FK
    end_user_id            text NOT NULL DEFAULT '',
    ttft_ms                integer NOT NULL DEFAULT 0,
    duration_ms            integer NOT NULL DEFAULT 0,
    -- Cache writes are their own bucket because upstreams price them
    -- separately from input; folding them into input tokens would make the row
    -- impossible to recompute.
    tokens_cache_write     integer NOT NULL DEFAULT 0,
    -- Tool invocations, billed per call. Vendors do not agree on tool names, so
    -- the raw shape is stored as jsonb rather than normalized into a dimension
    -- table: fixing the set of names now would be inventing a domain.
    tool_calls             jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(tool_calls) = 'object'),
    -- The service tier this request was served at. Snapshotted for the same
    -- reason as fx_rate: charged_nano must be recomputable from this row alone,
    -- without joining any configuration table.
    service_tier           text NOT NULL DEFAULT '',
    -- These four are nullable on purpose: NULL means "not reported", which is
    -- a different fact from a reported zero.
    tokens_audio_in        integer CHECK (tokens_audio_in IS NULL OR tokens_audio_in >= 0),
    tokens_audio_out       integer CHECK (tokens_audio_out IS NULL OR tokens_audio_out >= 0),
    tokens_cache_write_5m  integer CHECK (tokens_cache_write_5m IS NULL OR tokens_cache_write_5m >= 0),
    tokens_cache_write_1h  integer CHECK (tokens_cache_write_1h IS NULL OR tokens_cache_write_1h >= 0),
    -- The single source of truth for recomputing this charge later: the full
    -- rate card, every multiplier that applied, and the FX rate.
    pricing_snapshot       jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (
                               jsonb_typeof(pricing_snapshot) = 'object'
                           ),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

-- BRIN carries time-range scans, which is what a table this size is mostly
-- read by. The btree indexes serve the three read paths that actually exist:
-- an organization's detail list, filtering that list by key or model, and
-- support looking a request up by its id. request_id is not unique here — a
-- UNIQUE constraint on a partitioned table must include the partition key —
-- uniqueness of a request is enforced where the request is authorized.
--
-- Aggregate queries always read the rollups instead. No further index belongs
-- on this table: it is the hottest write surface, and every index is paid for
-- inside the settlement transaction.
CREATE INDEX usage_logs_created_brin ON usage_logs USING brin (created_at);
CREATE INDEX usage_logs_org_idx ON usage_logs (org_id, created_at DESC);
CREATE INDEX usage_logs_org_key_idx ON usage_logs (org_id, api_key_id, created_at DESC);
CREATE INDEX usage_logs_request_idx ON usage_logs (request_id);

ALTER TABLE usage_logs ENABLE ROW LEVEL SECURITY;
CREATE POLICY usage_logs_isolation ON usage_logs
    USING (org_id = current_setting('app.org_id')::uuid);

-- Catch-all partition. A missing partition makes the INSERT fail, and this
-- INSERT shares a transaction with settlement — so a missing partition would
-- take the whole data plane down. Availability wins over archive tidiness here;
-- a non-empty default partition is reported by the nightly check instead.
CREATE TABLE usage_logs_default PARTITION OF usage_logs DEFAULT;

-- Hourly rollups. Only the hour grain is materialized; a daily view is summed
-- from hours at query time, which is what keeps a day boundary from being
-- wrong in every time zone but one.
--
-- Every dimension column is NOT NULL, and only the finest grain
-- (org x key x model x hour) is stored — coarser grains are aggregated at query
-- time. A primary key cannot contain NULL, and using a sentinel value to mean
-- "all" would create two different ways to sum the same numbers.
--
-- Retained for years, hence partitioned by month as well: otherwise deleting by
-- age eventually means a DELETE over hundreds of millions of rows.
CREATE TABLE gateway_usage_rollups (
    org_id                 uuid NOT NULL,
    bucket_start           timestamptz NOT NULL,
    granularity            text NOT NULL DEFAULT 'hour' CHECK (granularity IN ('hour')),
    api_key_id             uuid NOT NULL,
    model_slug             text NOT NULL,
    requests               bigint NOT NULL DEFAULT 0,
    tokens_in              bigint NOT NULL DEFAULT 0,
    tokens_out             bigint NOT NULL DEFAULT 0,
    tokens_cached_read     bigint NOT NULL DEFAULT 0,
    tokens_cache_write     bigint NOT NULL DEFAULT 0,
    tokens_reasoning       bigint NOT NULL DEFAULT 0,
    tokens_audio_in        bigint NOT NULL DEFAULT 0,
    tokens_audio_out       bigint NOT NULL DEFAULT 0,
    tokens_cache_write_5m  bigint NOT NULL DEFAULT 0,
    tokens_cache_write_1h  bigint NOT NULL DEFAULT 0,
    charged_nano           bigint NOT NULL DEFAULT 0,
    upstream_cost_usd_nano bigint NOT NULL DEFAULT 0,
    errors                 bigint NOT NULL DEFAULT 0,
    -- Latency histogram, stored as cumulative counts. The bucket bounds cover
    -- the real distribution of these requests: under 100ms is a cache hit or a
    -- very short completion, over 10s is a long completion or an upstream
    -- problem.
    lat_le_100      bigint NOT NULL DEFAULT 0,
    lat_le_250      bigint NOT NULL DEFAULT 0,
    lat_le_500      bigint NOT NULL DEFAULT 0,
    lat_le_1000     bigint NOT NULL DEFAULT 0,
    lat_le_2500     bigint NOT NULL DEFAULT 0,
    lat_le_5000     bigint NOT NULL DEFAULT 0,
    lat_le_10000    bigint NOT NULL DEFAULT 0,
    -- Total duration, for the mean. A mean is only meaningful next to
    -- percentiles: p50 alone does not show how heavy the tail is.
    duration_ms_sum bigint NOT NULL DEFAULT 0,
    -- Total number of samples (the +Inf bucket), deliberately decoupled from
    -- `requests`. The denominator of a percentile has to be the number of
    -- requests whose duration was actually observed; counting rows that carry
    -- no latency sample pushes the computed p50 past the top bucket.
    lat_count       bigint NOT NULL DEFAULT 0,
    -- The provider dimension. It is in the primary key, which is what makes
    -- "margin and health per provider" a real split rather than an apportioned
    -- guess.
    provider_id     uuid NOT NULL,
    lat_le_30000    bigint NOT NULL DEFAULT 0,
    lat_le_60000    bigint NOT NULL DEFAULT 0,
    lat_le_120000   bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, bucket_start, granularity, api_key_id, model_slug, provider_id)
) PARTITION BY RANGE (bucket_start);

ALTER TABLE gateway_usage_rollups ENABLE ROW LEVEL SECURITY;
CREATE POLICY gateway_usage_rollups_isolation ON gateway_usage_rollups
    USING (org_id = current_setting('app.org_id')::uuid);

CREATE TABLE gateway_usage_rollups_default PARTITION OF gateway_usage_rollups DEFAULT;

-- This month and the next, created at deploy time; from then on a background
-- job stays ahead. Bounds are explicit UTC so they do not depend on the session
-- time zone.
-- +goose StatementBegin
DO $$
DECLARE
    b0 timestamp := date_trunc('month', now() AT TIME ZONE 'UTC');
    b1 timestamp := b0 + interval '1 month';
    b2 timestamp := b0 + interval '2 month';
    t  text;
BEGIN
    FOREACH t IN ARRAY ARRAY['usage_logs', 'gateway_usage_rollups'] LOOP
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
            t || '_' || to_char(b0, 'YYYY_MM'), t, b0 AT TIME ZONE 'UTC', b1 AT TIME ZONE 'UTC');
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
            t || '_' || to_char(b1, 'YYYY_MM'), t, b1 AT TIME ZONE 'UTC', b2 AT TIME ZONE 'UTC');
    END LOOP;
END $$;
-- +goose StatementEnd

CREATE TRIGGER providers_updated_at BEFORE UPDATE ON providers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER models_updated_at BEFORE UPDATE ON models
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Requests whose settlement failed to reach the database.
--
-- Without this table such a request leaves no recoverable trace: settlement and
-- the usage row share one transaction, and when that transaction fails there is
-- nothing to reconcile against. A consistency check between balances and the
-- ledger cannot find it either — nothing was written on either side, so the two
-- agree perfectly. The pre-authorization is then swept as expired and refunded,
-- and the books look clean while the service was given away for free.
--
-- `payload` holds the JSON of the parameters the usage insert would have taken,
-- so a retry replays it verbatim instead of this table having to repeat every
-- column of usage_logs.

CREATE TABLE gateway_unsettled (
    request_id      text PRIMARY KEY,   -- keyed by request, so recording the same failure twice is a no-op
    org_id          uuid NOT NULL,
    charged_nano    bigint NOT NULL,    -- denormalized so alerts and reconciliation can sum without parsing jsonb
    currency        text NOT NULL,
    reason          text NOT NULL,      -- why it failed the first time
    attempts        integer NOT NULL DEFAULT 0,
    payload         jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_attempt_at timestamptz,
    resolved_at     timestamptz,        -- the replay succeeded
    abandoned_at    timestamptz         -- out of retries, alerted, needs a human
);

-- The pending set: this is all the retry worker scans each round.
CREATE INDEX gateway_unsettled_pending_idx ON gateway_unsettled (created_at)
    WHERE resolved_at IS NULL AND abandoned_at IS NULL;

COMMENT ON TABLE gateway_unsettled IS
    'Requests whose settlement failed to reach the database. A worker replays '
    'them; past the retry limit a row is abandoned and alerted for a human';

-- Give every partition the same row-level security policy as its parent.
--
-- A policy attached to a partitioned parent does not descend to its partitions:
-- reads through the parent are constrained, but `SELECT ... FROM
-- usage_logs_2026_07` returns every organization's rows. Verified directly —
-- one row through the parent, two straight from the partition.
--
-- Nothing exploitable reaches this today, because no user-facing path names a
-- partition. It is a trap rather than a hole: the first person who adds "read
-- only this month's partition" for performance turns organization isolation off, and
-- nothing errors. So each partition carries the policy too, which makes the
-- bypass safe as well. Partitions created later get it from the job that
-- creates them.

-- +goose StatementBegin
DO $$
DECLARE part record;
BEGIN
    FOR part IN
        SELECT c.relname, pg_get_expr(c.relpartbound, c.oid) IS NOT NULL AS is_part
        FROM pg_class c
        JOIN pg_inherits i ON i.inhrelid = c.oid
        JOIN pg_class p ON p.oid = i.inhparent
        WHERE p.relname IN ('usage_logs', 'gateway_usage_rollups')
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', part.relname);
        EXECUTE format(
            'CREATE POLICY %I ON %I USING (org_id = current_setting(''app.org_id'')::uuid)',
            part.relname || '_isolation', part.relname);
    END LOOP;
END
$$;
-- +goose StatementEnd

-- Why latency is stored as a histogram rather than as per-bucket percentiles.
--
-- Percentiles cannot be averaged: the mean of 24 hourly p95 values is not the
-- p95 of that day, it is a number with no statistical meaning. Cumulative
-- histograms (the Prometheus `le` shape) can be summed column by column and the
-- percentile computed afterwards, so merging across time, model, key or
-- provider all stay correct.
--
-- The cost is precision bounded by the bucket edges — a value inside
-- (1000, 2500] can only be interpolated. In exchange, aggregation is correct
-- and the storage cost is fixed. Exact percentiles would need either every
-- sample kept (that is usage_logs itself, and sorting it is far too slow for a
-- dashboard) or a t-digest, whose complexity is out of proportion to what a
-- dashboard needs.
--
-- A row whose histogram is all zeros while `requests` is greater than zero
-- means "there were requests but no latency samples". Queries fall back to "no
-- data" for those rather than reporting a fabricated 0ms percentile.
COMMENT ON COLUMN gateway_usage_rollups.lat_le_100 IS
    'Cumulative count of requests with duration_ms <= 100. All buckets zero '
    'while requests > 0 means the row carries no latency samples at all';

-- The total number of samples in the histogram (the `+Inf` bucket).
--
-- It exists as its own column rather than reusing `requests` because the two
-- are not the same number: a row can have requests and no latency samples, and
-- counting those rows in the denominator pushes every percentile upward. In one
-- measured window 60 rows had samples and 54 did not; including all 114 pushed
-- p50 past the top bucket bound, while the 60 rows with samples had a p50 of
-- 500ms.
--
-- With an explicit count the denominator is exactly "requests whose duration
-- was observed", and rows without samples drop out of latency statistics
-- entirely.
COMMENT ON COLUMN gateway_usage_rollups.lat_count IS
    'Total samples in the latency histogram (the +Inf bucket). Deliberately '
    'decoupled from requests: a row can have requests and no latency samples';


-- The same trigger as providers and models. `UpdateRoute` assigns updated_at
-- explicitly; this is the second layer, so that a write which does not go
-- through it — a migration, a manual statement — cannot leave a stale
-- timestamp behind.
CREATE TRIGGER model_routes_updated_at BEFORE UPDATE ON model_routes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Model tiers: which models an organization may use. Nothing else — not
-- pricing, not routing.
--
-- They are deliberately not called "groups". One name that implies three
-- responsibilities is exactly the mistake this separation avoids: access,
-- pricing and routing are three independent axes, and a single "group" column
-- quietly ties them together.
CREATE TABLE model_tiers (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    slug        text NOT NULL UNIQUE,
    name        text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    -- Whether this tier admits every model in the catalogue. With it false the
    -- tier admits exactly what model_tier_models lists, and listing nothing
    -- really does admit nothing.
    --
    -- It defaults to false because creating a tier is an act of restricting:
    -- an operator who has just made one and not yet chosen its models has not
    -- said "everything", and treating the gap as a grant is the one direction
    -- that fails open. The seeded default tier sets it explicitly, because for
    -- that one row the opposite is true -- it is where an unconfigured
    -- organization lands, and it must not deny service to a deployment nobody
    -- has configured yet.
    --
    -- Turning it on clears the membership rows in the same transaction (see
    -- staffapi/tiers.go), so a non-empty membership always means a restricted
    -- tier -- the same rule as api_keys.allow_all_models, which cannot be a
    -- CHECK here only because the two sides live in different tables.
    allow_all_models boolean NOT NULL DEFAULT false,
    is_default  boolean NOT NULL DEFAULT false,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    -- The default tier cannot be disabled: it is where a new organization
    -- lands, so disabling it denies service to everyone without an explicit
    -- tier. "Cannot be deleted" is enforced in the application, since a CHECK
    -- constraint cannot express a DELETE.
    CONSTRAINT model_tiers_default_active CHECK (NOT (is_default AND status = 'disabled')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- Exactly one tier may be the default. Changing which one means clearing the
-- old flag and setting the new one in the same transaction.
CREATE UNIQUE INDEX model_tiers_single_default_uk ON model_tiers (is_default) WHERE is_default;

CREATE TRIGGER model_tiers_updated_at BEFORE UPDATE ON model_tiers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Which models are in a tier. Membership is consulted only when the tier's
-- allow_all_models is false; with it true this table is empty by construction.
--
-- Emptiness therefore has one meaning and only one: this tier admits no model.
-- "Every model" is said in the other column, where it can be read without
-- counting rows -- and where adding a model to the catalogue does not silently
-- change what an existing tier admits.
CREATE TABLE model_tier_models (
    tier_id  uuid NOT NULL REFERENCES model_tiers (id) ON DELETE CASCADE,
    model_id uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    PRIMARY KEY (tier_id, model_id)
);

-- The reverse lookup: which tiers include a given model. Needed to display it,
-- and to check references before deleting a model.
CREATE INDEX model_tier_models_model_idx ON model_tier_models (model_id);

-- What the deployment has agreed to give one organization: which models it may
-- reach, and how fast. One row per organization, and the whole agreement in one
-- place so that reading it takes one query and changing it takes one write.
--
-- This is its own table rather than extra columns on `orgs`, so that the whole
-- concept lives in one place while referential integrity is still enforced by
-- the database — tier_id is a real foreign key, not free text. Pricing is
-- assigned separately, in org_pricing_plan_assignments.
--
-- Note what it deliberately is *not*: a reusable group that decides three
-- things at once. The tier is the only shared, named object here; the two rate
-- ceilings belong to this organization alone. Putting them on the tier instead
-- would make one customer's traffic spend another customer's allowance,
-- because a tier is shared by every organization assigned to it.
CREATE TABLE org_gateway_settings (
    org_id       uuid PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
    -- NULL means the default tier, which is also what a missing row means.
    --
    -- ON DELETE RESTRICT: members must be moved explicitly before a tier can be
    -- deleted, rather than being silently returned to the default tier — which
    -- would change what models they can reach without anyone deciding to.
    tier_id      uuid REFERENCES model_tiers (id) ON DELETE RESTRICT,
    -- Requests and tokens per minute for the whole organization; NULL means no
    -- ceiling. Every request is measured against these *and* against its own
    -- key's ceilings, so a key is always the narrower of the two.
    --
    -- The organization is the level that carries them because it is the level
    -- the agreement is with. Splitting an organization's allowance across its
    -- keys instead would cap no organization at all: keys are created by the
    -- organization, and n keys each under the limit add up to n times the limit.
    rate_limit_rpm int CHECK (rate_limit_rpm > 0),
    rate_limit_tpm int CHECK (rate_limit_tpm > 0),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER org_gateway_settings_updated_at BEFORE UPDATE ON org_gateway_settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The reverse lookup: how many organizations are on a tier. Displayed in the
-- admin UI, and checked before a tier is deleted.
CREATE INDEX org_gateway_settings_tier_idx ON org_gateway_settings (tier_id) WHERE tier_id IS NOT NULL;

-- Today this table is only written by administrators and read by the data
-- plane, both of which run on paths that are not subject to row-level security.
-- The policy is enabled anyway, because a request from the organization itself
-- ("which models can I use?") is all but certain to arrive, and that query will
-- then be isolated the moment it is written. Adding row-level security later,
-- once the table has data, costs a migration and a review instead.
ALTER TABLE org_gateway_settings ENABLE ROW LEVEL SECURITY;
CREATE POLICY org_gateway_settings_isolation ON org_gateway_settings
    USING (org_id = current_setting('app.org_id')::uuid);

-- The default tier. It restricts nothing (allow_all_models), and it has to
-- exist: a missing org_gateway_settings row falls back to the default tier, and
-- without one that fallback has nowhere to land.
INSERT INTO model_tiers (slug, name, description, allow_all_models, is_default)
VALUES (
    'default',
    'Default',
    'Where normally registered organizations land; it admits every model until an operator narrows it.',
    true,
    true
);


-- Per-provider aggregation reads this index. `org_id` is deliberately not the
-- leading column: the health view is read across all organizations at once.
--
-- Without the provider dimension in the rollup, "error rate for this provider"
-- could only be joined through `model_slug`, and that apportions rather than
-- splits: a model served by two providers would show each provider the model's
-- entire request and error count, and two routes on one provider would count
-- the same data twice. The resulting number belongs to no provider at all.
CREATE INDEX gateway_usage_rollups_provider_idx
    ON gateway_usage_rollups (provider_id, bucket_start DESC);

-- What is known about one route on one endpoint. This table is the only
-- record of what a route can serve: the data plane and the catalog both read
-- it, and nothing in the configuration restates it.
--
-- One row per (route, endpoint of a protocol the provider speaks), seeded when
-- the route is created or the provider's protocol set widens, and removed when
-- the protocol set narrows. `protocol` is stored so that reads can filter rows
-- by the provider's current protocols in SQL; it is a function of `endpoint`
-- and is written by the seeder from the same table the data plane routes with.
--
-- Two different questions are answered by `status`, with two different
-- thresholds, on purpose:
--
--   * Is this route a candidate for this endpoint? -- for an endpoint the
--     gateway probes on its own (`probe_mode = 'auto'`): yes unless the row
--     says `unsupported`. A row that does not exist, or says `unverified` or
--     `failed`, lets the request through: the upstream is the authority and
--     the request is the cheapest way to ask it. For an endpoint the gateway
--     refuses to probe on its own (`manual`, images): only once a verdict says
--     `ok` -- what cannot be observed is opt-in, not tried on live traffic.
--   * Does the catalog publish this endpoint? -- if some enabled route on an
--     enabled provider says `ok`; or, on a provider the platform holds no
--     credential for (organizations bring their own), if an auto-probed
--     endpoint is merely `unverified` -- the platform has no way to look, and
--     hiding what it cannot see would unlist every BYOK-only model. "Callable
--     but unlisted" costs nothing; "listed but 404" is a support ticket. The
--     rule lives once, in the model_published_endpoints view below.
--
-- `unsupported` is reserved for a definitive answer: the upstream returned 404
-- or 405 to a probe -- twice, when it had said `ok` before. One 404 cannot tell
-- "unsupported" from "being rolled" or "withdrawn for an hour", so a verified
-- endpoint keeps its verdict on the first and loses it on a confirming second
-- (see SaveRouteProbe). A 400 or 422 is `failed` -- the probe's minimal body
-- may simply not suit that relay, and a route that works must not be taken
-- out of rotation by a probe's guess. 5xx, timeouts, 401/403/429 are `failed`
-- too: those are the provider's or the credential's problem, and the circuit
-- breakers already own them. A `failed` verdict never downgrades an `ok` one,
-- so an upstream incident does not churn the catalog; the failure's status
-- and message are still recorded on the row, and the admin page shows them.
--
-- `source` records who wrote the verdict. Only the probe worker and the
-- operator do; live traffic never writes a verdict, it enqueues a probe. An
-- `operator` row is the operator's override and is not overwritten by the
-- worker -- it is how an endpoint that is never probed automatically (images)
-- gets published, and how an operator says "do not send this here". A change
-- of upstream model name resets every row, the operator's included: the
-- override was about the old name.
CREATE TABLE model_route_probes (
    route_id   uuid NOT NULL REFERENCES model_routes (id) ON DELETE CASCADE,
    endpoint   text NOT NULL CHECK (
                   endpoint IN (
                       'chat', 'messages', 'messages_count_tokens',
                       'responses', 'responses_compact', 'responses_input_tokens',
                       'embeddings', 'images', 'generate_content', 'gemini_count_tokens',
                       'gemini_embed_content', 'gemini_batch_embed_contents', 'gemini_interactions'
                   )
               ),
    protocol   text NOT NULL CHECK (protocol IN ('openai', 'anthropic', 'gemini')),
    -- How this endpoint's verdict is reached: `auto` by the worker on its own,
    -- `manual` only when an operator asks (it costs money). Written by the
    -- seeder from the surface table in code, stored so that SQL -- the
    -- candidate query, the catalog view, the sweeper -- can read it without
    -- spelling an endpoint's name.
    probe_mode text NOT NULL CHECK (probe_mode IN ('auto', 'manual')),
    -- `unverified` is a real state, not the absence of a row: some endpoints
    -- are not probed automatically, and they need somewhere to record "known
    -- to be unverified, trigger it by hand".
    status     text NOT NULL DEFAULT 'unverified' CHECK (
                   status IN ('unverified', 'ok', 'unsupported', 'failed')
               ),
    source     text NOT NULL DEFAULT 'probe' CHECK (source IN ('probe', 'operator')),
    checked_at timestamptz,
    latency_ms integer,
    status_code integer,
    -- The upstream's own message, truncated. It is the only thing that tells
    -- "wrong credential" from "wrong model name" from "endpoint not
    -- supported", so it is stored verbatim rather than rewritten.
    error      text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (route_id, endpoint)
);

CREATE TRIGGER model_route_probes_updated_at BEFORE UPDATE ON model_route_probes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- "Which configurations are failing?" is the question asked most often, so it
-- gets its own partial index.
CREATE INDEX model_route_probes_failed_idx ON model_route_probes (status)
    WHERE status IN ('failed', 'unsupported');

-- The sweeper re-probes rows whose verdict has aged, so that an upstream that
-- withdrew a model and brought it back is found out without anyone clicking.
-- Only rows it can advance: automatically probed, not the operator's.
CREATE INDEX model_route_probes_sweep_idx ON model_route_probes (status, checked_at)
    WHERE source <> 'operator' AND probe_mode = 'auto';

-- What the catalog publishes, stated once. Every reader of "which endpoints
-- does this model serve" -- the public catalog, the staff listing, the Gemini
-- catalog -- selects from here, so that the rule cannot drift between them.
--
-- An endpoint is published when a probe (or the operator) found it working on
-- an enabled route of an enabled provider that still speaks its protocol. On
-- a provider the platform holds no active credential for, the worker cannot
-- look, so an automatically probed endpoint is published while unverified:
-- organizations bringing their own key are the only ones who can reach it,
-- and a catalog that hides what the platform cannot see would list none of
-- their models. A manual endpoint is never published on such a provider --
-- it is not a candidate either.
CREATE VIEW model_published_endpoints AS
SELECT r.model_id, pr.endpoint, pr.protocol
FROM model_routes r
JOIN providers p ON p.id = r.provider_id
JOIN model_route_probes pr ON pr.route_id = r.id
WHERE r.enabled AND p.enabled
  AND pr.protocol = ANY (p.protocols)
  AND (
      pr.status = 'ok'
      OR (
          pr.status = 'unverified' AND pr.probe_mode = 'auto'
          AND NOT EXISTS (
              SELECT 1 FROM provider_keys k
              WHERE k.provider_id = p.id AND k.status = 'active'
          )
      )
  );


CREATE TABLE pricing_plans (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    slug        text NOT NULL UNIQUE,
    name        text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    etag        uuid NOT NULL DEFAULT uuidv7(),
    is_default  boolean NOT NULL DEFAULT false,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    -- The plan's default multiplier (10000 = list price; may discount or mark
    -- up). A per-model override replaces this value rather than compounding
    -- with it.
    default_multiplier_bps integer NOT NULL DEFAULT 10000
        CHECK (default_multiplier_bps BETWEEN 1 AND 100000),
    reason      text NOT NULL DEFAULT '',
    updated_by  uuid REFERENCES staff_users (id) ON DELETE SET NULL,
    CONSTRAINT pricing_plans_default_active_ck CHECK (NOT (is_default AND status = 'disabled'))
);
CREATE UNIQUE INDEX pricing_plans_one_default_uk ON pricing_plans (is_default) WHERE is_default;
CREATE TRIGGER pricing_plans_updated_at BEFORE UPDATE ON pricing_plans
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- Requests that could not be settled because a price was missing.
--
-- This is a separate table from the general retry queue, not a `kind` column on
-- it, and the separation is physical on purpose. A row in the general queue can
-- be replayed automatically: its amount is already computed. A row here cannot
-- — until the price is filled in there is no real charged amount, and
-- `reserved_nano` is only what was set aside up front.
--
-- Only the ordinary queue has automatic settlement authority. Keeping the
-- missing-price queue physically separate makes it impossible for that worker
-- to turn a reservation into a confirmed bill before pricing is resolved.
CREATE TABLE gateway_pricing_unsettled (
    request_id      text PRIMARY KEY,
    org_id          uuid NOT NULL,
    reserved_nano   bigint NOT NULL CHECK (reserved_nano >= 0),
    currency        text NOT NULL,
    reason          text NOT NULL,
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    payload         jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_attempt_at timestamptz,
    resolved_at     timestamptz,
    abandoned_at    timestamptz
);
CREATE INDEX gateway_pricing_unsettled_pending_idx ON gateway_pricing_unsettled (created_at)
    WHERE resolved_at IS NULL AND abandoned_at IS NULL;
COMMENT ON TABLE gateway_pricing_unsettled IS
    'Requests held back because a price was missing. reserved_nano is what was '
    'set aside up front, not a final charge, so the generic retry worker must '
    'never settle these rows automatically';

-- The default pricing plan, inherited by any organization with no explicit
-- assignment.
INSERT INTO pricing_plans (slug, name, description, is_default)
VALUES ('default', 'Default pricing plan', 'Inherited when no plan is explicitly assigned.', true);


-- Keyset index for listing organizations.
--
-- Including `id` is not redundant: keyset pagination compares the row value
-- `(created_at, id)` against the cursor, and an index on `created_at` alone
-- degenerates as soon as several organizations share a millisecond.
CREATE INDEX IF NOT EXISTS orgs_created_at_id_desc_idx ON orgs (created_at DESC, id DESC);

-- ===== Model prices: one row per model =====

CREATE TABLE model_pricing (
    model_id                            uuid PRIMARY KEY REFERENCES models (id) ON DELETE CASCADE,
    billing_mode                        text NOT NULL CHECK (billing_mode IN ('paid', 'free')),
    upstream_in_nano_per_mtok           bigint CHECK (upstream_in_nano_per_mtok BETWEEN 0 AND 92233720368547758),
    upstream_out_nano_per_mtok          bigint CHECK (upstream_out_nano_per_mtok BETWEEN 0 AND 92233720368547758),
    upstream_cache_read_nano_per_mtok   bigint CHECK (upstream_cache_read_nano_per_mtok BETWEEN 0 AND 92233720368547758),
    upstream_cache_write_nano_per_mtok  bigint CHECK (upstream_cache_write_nano_per_mtok BETWEEN 0 AND 92233720368547758),
    currency                            text NOT NULL DEFAULT 'USD' CHECK (currency = 'USD'),
    -- The upper bound of 100000 is x10, the same limit the application
    -- validates against; a lower bound of 1 keeps "free" out of this column.
    multiplier_bps                      integer NOT NULL DEFAULT 10000
                                            CHECK (multiplier_bps BETWEEN 1 AND 100000),
    source_name                         text NOT NULL DEFAULT '',
    source_url                          text NOT NULL DEFAULT '',
    verified_at                         timestamptz,
    provenance                          jsonb NOT NULL DEFAULT '{}'::jsonb
                                            CHECK (jsonb_typeof(provenance) = 'object'),
    reason                              text NOT NULL DEFAULT '',
    updated_by                          uuid REFERENCES staff_users (id) ON DELETE SET NULL,
    created_at                          timestamptz NOT NULL DEFAULT now(),
    updated_at                          timestamptz NOT NULL DEFAULT now(),
    -- This constraint is about ledger correctness, not about editing workflow.
    -- There is no draft state, so there is nothing to exempt: every row that
    -- exists must be a price the gateway can actually charge against.
    CONSTRAINT model_pricing_complete_ck CHECK (
        upstream_in_nano_per_mtok IS NOT NULL
        AND upstream_out_nano_per_mtok IS NOT NULL
        AND upstream_cache_read_nano_per_mtok IS NOT NULL
        AND upstream_cache_write_nano_per_mtok IS NOT NULL
        -- A free model must say so with billing_mode = 'free'. Allowing a
        -- 'paid' row with all four buckets at zero would make "deliberately
        -- free" and "price never filled in" the same state again.
        AND (billing_mode = 'free'
             OR upstream_in_nano_per_mtok > 0
             OR upstream_out_nano_per_mtok > 0
             OR upstream_cache_read_nano_per_mtok > 0
             OR upstream_cache_write_nano_per_mtok > 0)
    )
);
CREATE TRIGGER model_pricing_updated_at BEFORE UPDATE ON model_pricing
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ===== Advanced dimensions and tool rates, keyed by model =====

CREATE TABLE model_price_dimension_rates (
    model_id      uuid NOT NULL REFERENCES model_pricing (model_id) ON DELETE CASCADE,
    bucket        text NOT NULL CHECK (
                      bucket IN ('in', 'out', 'cache_read', 'cache_write', 'audio_in', 'audio_out')
                  ),
    service_tier  text NOT NULL DEFAULT 'standard' CHECK (
                      service_tier IN ('standard', 'priority', 'batch')
                  ),
    variant       text NOT NULL DEFAULT '',
    -- The context-length band this rate belongs to: it applies when the whole
    -- prompt (non-cached input plus both cache buckets) is at least this many
    -- tokens. Several upstreams charge more above a prompt-size threshold --
    -- Claude Sonnet 4/4.5 raise both the input and the output price above 200K,
    -- Gemini 2.5 Pro does the same -- and without this axis such requests are
    -- billed at the short-prompt rate, which undercharges.
    --
    -- The bound is inclusive, so bands tile the axis as [min, next min) with no
    -- gap and no overlap, and the default 0 matches every request including one
    -- with no input at all. An upstream rule phrased as "above 200K" is
    -- therefore configured as 200001, not 200000.
    min_input_tokens integer NOT NULL DEFAULT 0 CHECK (min_input_tokens >= 0),
    nano_per_mtok bigint NOT NULL CHECK (nano_per_mtok BETWEEN 0 AND 92233720368547758),
    PRIMARY KEY (model_id, bucket, service_tier, variant, min_input_tokens)
);

CREATE TABLE model_price_tool_rates (
    model_id      uuid NOT NULL REFERENCES model_pricing (model_id) ON DELETE CASCADE,
    tool          text NOT NULL,
    nano_per_call bigint NOT NULL CHECK (nano_per_call BETWEEN 0 AND 92233720368547758),
    PRIMARY KEY (model_id, tool)
);

-- ===== Pricing plans: per-model exceptions to the plan's default =====


CREATE TABLE pricing_plan_model_overrides (
    pricing_plan_id uuid NOT NULL REFERENCES pricing_plans (id) ON DELETE CASCADE,
    model_id        uuid NOT NULL REFERENCES models (id) ON DELETE CASCADE,
    multiplier_bps  integer NOT NULL CHECK (multiplier_bps BETWEEN 1 AND 100000),
    PRIMARY KEY (pricing_plan_id, model_id)
);
CREATE INDEX pricing_plan_model_overrides_model_idx
    ON pricing_plan_model_overrides (model_id);

-- ===== Plan assignment: one row per organization =====

CREATE TABLE org_pricing_plan_assignments (
    org_id          uuid PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
    pricing_plan_id uuid NOT NULL REFERENCES pricing_plans (id) ON DELETE RESTRICT,
    -- true records an explicit decision to follow the default plan. It has the
    -- same effect as having no row at all, but it leaves a reason and an audit
    -- trail behind — "we chose the default" and "nobody has looked at this"
    -- are different facts.
    inherit_default boolean NOT NULL DEFAULT false,
    reason          text NOT NULL DEFAULT '',
    updated_by      uuid REFERENCES staff_users (id) ON DELETE SET NULL,
    updated_at      timestamptz NOT NULL DEFAULT now()
);
-- Isolated per organization, like every other table holding org-scoped rows.
ALTER TABLE org_pricing_plan_assignments ENABLE ROW LEVEL SECURITY;
CREATE POLICY org_pricing_plan_assignments_isolation ON org_pricing_plan_assignments
    USING (org_id = current_setting('app.org_id')::uuid);

-- There is exactly one source of price: model_pricing. No versions, no release
-- objects — saving takes effect immediately, and recomputing a past charge
-- reads usage_logs.pricing_snapshot rather than joining today's configuration.
-- That is why neither `models` nor `usage_logs` carries a pricing column.

-- ── 外键列的索引 ────────────────────────────────────────────────────────────
--
-- 判据：**父表的行会被删，而未加索引的外键列会把那次删除变成子表的一次顺序扫描**
-- ——而且是在持有排他锁的时候。PostgreSQL 不为外键自动建索引，所以这一条得手写。
--
-- 逐条量过（ADR-0172）：全仓 68 个外键列，24 个没有以它打头的索引，且这 24 个
-- 所在的子表**都会随使用增长**。真正的高写表（usage_logs、gateway_usage_rollups）
-- 没有裸外键，所以这批索引不落在热写路径上。
--
-- 反过来也成立：不是每个外键都该有索引。加索引的代价是每次写都要维护它，
-- 「所有外键一律加索引」是把一条判据换成一句口号。

CREATE INDEX model_pricing_updated_by_idx ON model_pricing (updated_by);
CREATE INDEX model_routes_provider_idx ON model_routes (provider_id);
CREATE INDEX org_pricing_plan_assignments_plan_idx ON org_pricing_plan_assignments (pricing_plan_id);
CREATE INDEX org_pricing_plan_assignments_updated_by_idx ON org_pricing_plan_assignments (updated_by);
CREATE INDEX pricing_plans_updated_by_idx ON pricing_plans (updated_by);
CREATE INDEX resource_affinities_model_idx ON resource_affinities (model_id);
CREATE INDEX resource_affinities_org_provider_key_idx ON resource_affinities (org_provider_key_id);
CREATE INDEX resource_affinities_provider_idx ON resource_affinities (provider_id);
CREATE INDEX resource_affinities_provider_key_idx ON resource_affinities (provider_key_id);
