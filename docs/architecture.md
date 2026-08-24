# Architecture

One process, one database. The binary serves the data plane, the admin API and
the admin UI; PostgreSQL holds everything that survives a restart. Redis is
optional and only matters once you run more than one replica.

```text
  OpenAI / Anthropic / Gemini SDKs      browser
             |                             |
             v                             v
     /v1/...  and  /v1beta/...        /  and  /api/...
             \                           /
              \                         /
               +------- fairlb --------+          PostgreSQL
                      |                                ^
                      +-------- upstreams              |
                        (OpenAI, Anthropic, Gemini,    |
                         compatible relays) -----------+
```

## The surfaces

Everything is served from one host and split by path. A hosted multi-tenant
deployment splits these across subdomains; a single install behind a single
domain does not need that, so the isolation here is by path and by
authentication domain, not by host name.

| Path | Who calls it | Authentication |
|---|---|---|
| `/v1/...` | Your applications and any OpenAI or Anthropic SDK | API key, `Authorization: Bearer` or `x-api-key` |
| `/v1beta/...` | The Gemini SDKs and the Gemini CLI, which speak the Gemini-native API | The same API key, also accepted as `x-goog-api-key` |
| `/api/staff/v1/...` | The admin UI: providers, models, routes, prices | Signed-in session cookie |
| `/api/v1/...` | The admin UI: usage, request logs, available models | The same session cookie |
| `/` | The admin UI itself | None (the UI asks for the session) |

Liveness, readiness and metrics listen on a **separate address**
(`INTERNAL_ADDR`), so that `/metrics` is not reachable from wherever the gateway
is. Nothing publishes that port by default.

| Endpoint | Answers |
|---|---|
| `/healthz` | Is the process alive |
| `/readyz` | Can it serve — it checks the database |
| `/metrics` | Prometheus metrics, including Go runtime and process collectors |

The split matters exactly when the database is unreachable: an orchestrator
needs to tell "still running, cannot serve" apart from "crashed", and one
endpoint cannot say both.

## What happens to a request

```text
credential header  ->  key lookup (cached)  ->  key and org state
  -> per-team then per-key checks: model access, budget, RPM, TPM
  -> model resolution: is this model enabled and visible to this key
  -> price lookup, snapshotted for the whole request
  -> candidate routes for this model, in priority order
  -> upstream call: header rewrite, model name mapping, byte-level stream relay
  -> usage extraction (estimated if the upstream omits it)
  -> settlement in one transaction, then batched insert into the usage log
```

Two properties of that list are load-bearing:

- **Prices are read once, at the start.** Everything the request is charged at
  is read inside one snapshot transaction before the upstream call goes out.
  Changing a price while a stream is in flight does not retroactively reprice
  it, because that request already read its copy.
- **Refusals happen before the upstream call.** A key over budget, a model the
  key may not use, a rate limit — all of these are decided while nothing has
  been spent. See [design/key-budgets.md](design/key-budgets.md) for why the
  budget check refuses rather than allows when it cannot decide.

Requests pass through. The gateway parses what routing and accounting need —
the model name, the stream flag, the usage block on the way back — and forwards
the rest of the body untouched. Streaming responses are relayed byte for byte.

## Choosing an upstream

A model is served by one or more **routes**. A route says: this model, on this
provider, under this upstream model name, at this priority and weight.

Selection walks the routes in ascending priority, and inside one priority group
picks randomly weighted by the route weight. A route whose provider or provider
key is cooling down is skipped, and so is one whose provider has no allowance
left this minute. If the attempt fails in a way that can be retried, the next
candidate is tried, up to three routes per request.

Health and capacity both enter as filters, not as weights: a provider is either
in the running or it is out, and while it is in the running its share of traffic
is exactly what you configured. There is no scoring that quietly bleeds traffic
away from an upstream that has been a little slow — an upstream is either good
enough to use or it is out, and which one it is can be read off the cooldown
state instead of inferred from a changing traffic split.

A provider says what its upstream account will take: requests per minute, tokens
per minute, and how many calls may be in flight at once. Those belong to the
account rather than to a model, because that is the shape the quota has — a key
is rated across everything it serves. An account with a different quota is a
different account, and therefore a second provider record, which is also what
gives it its own credentials, its own circuit and its own share of the traffic.

Skipping a provider for either reason costs the request nothing: it has not
called anything, so it has not used one of its three attempts. Several keys on
one provider are used in turn rather than in order, so they share that account's
quota instead of standing by behind the oldest one.

Two rules limit how much a bad upstream can cost you:

- Once the first byte has reached the client, there is no failover. The response
  has already started; the only honest option is to end it and record what
  happened.
- Retries have a budget. A gateway that retries every failure turns a partial
  upstream outage into a full one by multiplying the traffic against it.

Health is tracked at two levels — the individual provider key and the provider
as a whole — with an exponential backoff ladder and jitter, and a single probe
request on expiry. [design/failover-and-cooldowns.md](design/failover-and-cooldowns.md)
covers which upstream failure means which, and why the two levels are separate.

Protocols are not translated. An OpenAI-shaped request goes to an OpenAI-shaped
upstream, an Anthropic-shaped request to an Anthropic-shaped one, a Gemini-shaped
request (the `/v1beta` surface) to a Gemini-shaped one; the
[design note](design/same-protocol-passthrough.md) explains why the alternative is
worse than it sounds.

## Keys, budgets and limits

API keys look like `sk-flb-v1-` followed by random characters and a checksum.
Only a hash is stored; the plaintext is shown once, at creation. The checksum
lets an obviously mistyped key be rejected before touching the database, and
gives secret-scanning tools something they can match without false positives.

Each key carries its own controls:

| Control | Effect |
|---|---|
| Model access | Everything its team may reach, or exactly the models you list. A key that lists none reaches none |
| Spend limit | Daily, monthly or total, enforced before the upstream call |
| RPM and TPM | Request-per-minute and token-per-minute ceilings |
| Expiry | Past it the key stops authenticating, without anyone having to revoke it |
| Labels | Free-form tags that come back on usage rows, so you can slice spend by app or environment |

## Teams

Keys are issued in a team. One team exists from first start and is where a key
lands when you do not name one, so a deployment that never wants to think about
this never has to.

A team carries what a group is allowed, rather than what one key is allowed:

| Setting | Effect |
|---|---|
| Access tier | Which models the team may call at all. A tier either allows every model or allows exactly the ones it lists |
| RPM and TPM | Ceilings for the whole team. Every request is measured against these *and* against its own key's, so a key is never wider than its team |

That is the difference between the two levels, and it is why both exist: a key's
ceiling cannot cap a team, because keys are created by whoever runs the team and
five keys each under the limit add up to five times the limit.

"Only this model, for these people" is therefore two steps: make a tier holding
that model, put the team on it, and issue that group its keys there.

A team is suspended rather than deleted. Suspending refuses every request its
keys make, immediately, and keeps the keys — so resuming restores exactly what
was there. Deleting would take the keys with it and leave the usage rows that
name the team pointing at nothing; those rows record what was consumed, and they
outlive the things they describe on purpose. The first team cannot be suspended,
since it is the one keys fall back to.

Note what "no models" means, because two opposite settings used to look
identical. A tier that allows every model and a tier that allows none are both
tiers with an empty list; what tells them apart is the switch, not the count. The
same is true of a key. If a list is empty and the switch is off, nothing is
allowed — which is a real thing to want, and used to be unsayable.

Revocation invalidates the data plane's key cache immediately. Without that
step the API would answer "revoked" while a leaked key kept working until its
cache entry expired.

## Prices and usage

Prices are entered per model as four numbers per million tokens: input, output,
cache read, cache write. Some models need more than four — audio, per-request
tool charges, cache writes with a longer time to live — and those are optional
overrides on top of the four.

A missing price and a zero price are different things. `NULL` means "nobody has
priced this yet" and a paid model with any of the four missing cannot be
enabled. `0` means "this component is free". A model that is free in its
entirety says so explicitly, rather than being inferred from four zeros — an
inference that would silently start giving away a model the moment somebody
cleared a price by accident.

If a completed request cannot be priced, it is not recorded as free. It lands in
a queue for a human, because the alternative is a number that is quietly wrong.

Entering the first set of rates by hand is the one part of setup that is pure
transcription, so the binary carries a snapshot of a public price dataset and
`fairlb pricing import` fills empty rates in from it. It runs only when you run
it, and every rate it writes is marked unverified until a person confirms it —
see [design/reference-prices.md](design/reference-prices.md).

Every request writes one usage row: the token counts by bucket, the price
applied, latency, which provider served it, how many candidates were tried, and
the status. Rows are batched and the tables are partitioned by time. Rollups are
computed forward from a watermark, so restarting mid-way recomputes from the
last completed point rather than from the beginning.

One row per request, not one per attempt. The count of candidates tried is
therefore a number and not a list: if a request failed over twice, the row names
the provider that finally served it and says three attempts were made, but not
which two came before. That is a deliberate trade — the usage table is what
budgets, invoices and rollups are computed from, and a table where one request
can be several rows makes every one of those computations depend on a flag that
says which rows are real.

When an upstream returns no usage block at all — some compatible relays do not,
and an interrupted stream cannot — token counts are estimated and the row is
flagged as estimated. The flag also travels in the response, so a client can
tell a measured number from an estimated one.

The estimate is a character heuristic, not a tokenizer: it takes the larger of
bytes/4 and characters/2, which errs high on Latin text and is roughly right on
dense scripts. That is adequate for its main job, which is sizing the amount of
balance a request reserves before it runs. It is worth knowing that in the
uncommon case where an upstream reports nothing at all, this approximation is
what the request is charged on — if you see many estimated rows for one
provider, that provider's usage reporting is the thing to look at.

## Pluggable infrastructure

Four things need shared state once there is more than one replica: the cache,
rate limit counters, circuit breaker state, and distributed locks. Each is
behind an interface with two implementations, chosen by environment variable.

| Driver | `memory` | `redis` |
|---|---|---|
| Cache | Per-process, invalidations broadcast in-process | Shared, invalidations reach every replica |
| Rate limit | Per-process counters, so N replicas allow N times the limit | One shared counter |
| Breaker | Per-process health view | Shared health view |
| Lock | Process-local | Shared |

One replica on the memory drivers is a complete, correct install; that is the
default and it needs no Redis. More than one replica on the memory drivers is
the configuration to avoid: the limits still work, they just apply per process.

Circuit breaker state also has a database table, but only so that a restart does
not start with a clean slate; the hot path never queries it.

## What is stored

| Area | Tables |
|---|---|
| Catalog | `providers`, `provider_keys`, `models`, `model_routes`, `model_route_probes` |
| Prices | `model_pricing`, `model_price_dimension_rates`, `model_price_tool_rates`, `pricing_plans` |
| Access | `api_keys`, `model_tiers`, `model_tier_models`, `org_gateway_settings` |
| Accounting | `usage_logs`, `gateway_usage_rollups`, `api_key_daily_spend`, `posting_watermarks` |
| Operations | `cooldowns`, `settings`, `audit_logs`, `idempotency_keys` |
| Identity | `staff_users`, `staff_sessions`, `orgs` |

Every row that belongs to a organization carries an organisation id and row-level
security policies key off it. That column is what a team is: a single install
starts with one, created on first start, and can have as many as it needs.
Keeping the column rather than dropping it is what lets the same schema and the
same queries serve both shapes — and it is why teams cost no new concept here.

Migrations run automatically when the process starts. A separate migrate step is
a step somebody eventually forgets, and the symptom is a service that starts,
looks healthy, and has no tables.

## Where administrative writes are recorded

Every write through the admin API is recorded in `audit_logs` with the actor,
the target, and the change. This is also the price history: prices are saved
directly rather than staged in versions, so "who changed this, when, and to
what" lives in the audit log, and rolling a price back means reading the old
value out of it.
