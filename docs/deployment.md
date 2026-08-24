# Deployment

What you need: Docker. The compose file brings its own PostgreSQL, and pointing
at a database you already run works too — 17 or newer. Redis is optional and
only becomes useful with more than one replica.

Every setting is an environment variable; the full list is in
[configuration.md](configuration.md).

## Compose

The gateway and a database, in the [`docker-compose.yml`](../docker-compose.yml)
at the root of this repository:

```bash
curl -fsSLO https://raw.githubusercontent.com/fairlb/fairlb/main/docker-compose.yml
docker compose up -d
```

Open it in a browser and fill in the setup form; that creates the first
administrator and signs you in. Migrations run on start, so there is no separate
migrate step and no second command.

Four things in that file worth knowing before you edit it:

- **The database port is not published.** Nothing outside the compose network
  needs it, and a Postgres listening on a public host is found by scanners
  within hours. Add a mapping yourself if you want to connect a client to it.
- **The health check runs the binary**, not `curl`. The image is distroless —
  no shell, no curl — which is the right default for a service holding provider
  credentials, and the cost of it is that the probe has to be a subcommand.
- **`SECRET_KEY` is deliberately unset** and `/data` is a named volume; see the
  next section for why that combination and not the other one.
- **Settings live in [`.env.example`](../.env.example)** — copy it to `.env` and
  uncomment what you need. `PORT` and `FAIRLB_IMAGE` in there are read by
  compose itself rather than by the gateway, which is why they do not appear in
  [configuration.md](configuration.md).

## The master key, and why the volume is not optional

Upstream credentials are encrypted at rest with a master key. You can supply it
as `SECRET_KEY` (64 hex characters), and if you do not, one is generated on
first start and written to `FAIRLB_DATA_DIR` — `/data` by default — with mode
0600.

**If that file is lost, every stored upstream credential becomes undecryptable.**
Not "resets to empty" — unreadable, while the rows are still there. So:

- Mount `/data` on a named volume or a host path. A container recreated without
  it comes back with a new key and cannot read what the old one wrote.
- Do not generate a fresh key on every start. A `docker run` with
  `SECRET_KEY="$(openssl rand -hex 32)"` in the command line looks careful and
  is the same mistake with more steps.
- Running more than one replica means supplying `SECRET_KEY` through the
  environment, because each replica would otherwise generate its own.
- Back the key up with the database, and store it somewhere the database backup
  is not. See below.

The image runs as a non-root user, so the mounted path has to be writable by it.
A named volume handles this; a bind mount to a host directory owned by root does
not, and the failure is a startup error saying so rather than a silent fallback.

## The first administrator

Two ways, and you pick by whether a human is present.

**In a browser.** With no administrator and no credentials in the environment,
`/setup` is open and the start-up log prints the link. Completing it creates the
account, signs you in, and closes the wizard permanently.

If the instance is reachable from the internet before you get to it, set
`FAIRLB_SETUP_TOKEN` and the wizard will ask for it.

**From the environment.** Set `FAIRLB_ADMIN_EMAIL` and `FAIRLB_ADMIN_PASSWORD`
(or `FAIRLB_ADMIN_PASSWORD_FILE`, which reads a mounted secret) and the account
is created on first start. Both must be set; one alone is rejected at startup.
Subsequent starts do nothing, so leaving them in your
compose file is fine.

There is also a command line path — `fairlb admin create` and
`fairlb admin reset-password` — which take the password from
`FAIRLB_ADMIN_PASSWORD` or from stdin, never as an argument. An argument would
land in your shell history and in `ps` output for every user on the machine.

## Behind a reverse proxy

Three settings and two proxy behaviours.

```text
PUBLIC_URL       https://gateway.example.com
TRUST_PROXY      true
TRUST_PROXY_HOPS 1
```

`PUBLIC_URL` decides whether session cookies carry `Secure`, and it is the
same-origin reference for the CSRF check. Get its scheme wrong and one of two
things happens: an `http` value behind TLS means cookies without `Secure`, and
an `https` value served over plain HTTP means a cookie the browser will not send
back — a login that appears to succeed and then does not.

`TRUST_PROXY` makes the gateway believe `X-Forwarded-For` and
`X-Forwarded-Proto`. Do not set it when the gateway is directly reachable:
without a proxy in front, anyone can claim any client address, and the rate
limiter keys on that address.

`TRUST_PROXY_HOPS` is how many proxies are actually in front. The client address
is taken that many entries from the right of `X-Forwarded-For`. Setting it
higher than the real chain hands the rate-limit key to a field the client
controls; setting it lower attributes everything to your own proxy.

On the proxy itself:

- **Turn response buffering off** for `/v1` and `/v1beta`. A buffering proxy holds a streamed
  completion until it finishes, which turns streaming into a slow non-streaming
  response. In nginx that is `proxy_buffering off`.
- **Raise the read timeout.** Image generation is allowed 300 seconds and long
  completions can run for minutes. A 60-second proxy timeout will cut them off,
  and the error the caller sees will come from your proxy, not from here.

Operational endpoints listen on a **separate address** (`INTERNAL_ADDR`, default
`:9091`) and carry `/readyz` and `/metrics`. Do not publish that port, and do
not route it through the same proxy as the public one.

## Installing

Start from a new, empty PostgreSQL database (or a new Compose volume).
Migrations are applied automatically at start-up; there is nothing to run by
hand. This release is the baseline: later release notes will describe what
changed and the supported upgrade path. Backups are mandatory before any
future upgrade; see below.

## Backup and restore

Two things to back up, and they are useless separately:

```bash
# 1. the database
docker compose exec -T db pg_dump -U fairlb --format=custom fairlb > fairlb.dump

# 2. the master key (skip if you supply SECRET_KEY yourself)
docker compose cp fairlb:/data/secret.key ./secret.key
```

The key comes out with `cp` rather than `exec cat` because the gateway image has
no `cat` — it has no userland at all beyond the binary.

A database backup **without** the master key restores every provider, model,
route, price and usage row — and not one usable upstream credential. Verify this
is not the state you are in before you need the backup: restore into a scratch
database, start the gateway against it with the key you saved, and send one real
request.

Restoring:

```bash
docker compose exec -T db createdb -U fairlb fairlb_restored
docker compose exec -T db pg_restore -U fairlb --dbname=fairlb_restored < fairlb.dump
```

Then start the gateway with `DATABASE_URL` pointing at the restored database and
the same master key as the source.

Keep the key and the dump in different places. Storing them together means one
compromised backup gives away both the data and the credentials that unlock it.

## Running more than one replica

Everything except one thing already works: replicas share the database, and they
tolerate each other during start-up.

The one thing is the four infrastructure drivers, which default to in-process
implementations:

```text
DRIVER_CACHE=redis
DRIVER_RATELIMIT=redis
DRIVER_BREAKER=redis
DRIVER_LOCK=redis
REDIS_URL=redis://redis:6379
```

With the in-process defaults on N replicas, rate limits apply N times over, a
key revoked on one replica keeps working on the others until its cache entry
expires, and each replica forms its own opinion about which upstreams are
healthy. None of that is a crash; all of it is wrong quietly.

Also supply `SECRET_KEY` from the environment rather than the data directory, so
that every replica agrees on it.

## Metrics

`/metrics` on the internal address is Prometheus format and includes Go runtime
and process collectors alongside the gateway's own.

The two probes next to it answer different questions, and a load balancer wants
both:

| Probe | 200 when | 503 when |
|---|---|---|
| `/healthz` | The process is up and taking traffic | Shutdown has begun — stop sending new requests here |
| `/readyz` | The dependency checks pass | The database is unreachable |

`/healthz` is also served on the public address, so a proxy that can only reach
the public port still has something to poll.

## Stopping

Stopping is deliberately not instant, because a gateway's requests are long.
On `SIGTERM` the gateway does two things in order:

1. `/healthz` starts answering 503 while both listeners keep serving normally,
   for `DRAIN_GRACE_SECONDS` (0 by default). Set a positive value only when a
   proxy needs time to stop routing new work. This is the window whatever routes
   traffic here has to notice and stop sending more.
2. New connections are refused and requests already running get
   `SHUTDOWN_TIMEOUT_SECONDS` (320 seconds by default) to finish. Whatever is still
   running when that elapses is cut off mid-response, and the log says so.

The second number has a floor, and it is your own traffic: an instance serving
image generation has requests that run for several minutes, and stopping it on
the default cuts them off on every upgrade. What the client sees then is a
connection that ended mid-response, which is indistinguishable from a network
fault — so the gateway logs a warning naming this setting on its way out, and
that warning is the only place the cause is written down.

Both numbers are the application's half of the arrangement. Whatever supervises
the process kills it after a grace period of its own, and the shorter of the two
is the one that decides:

| Supervisor | Its own setting | Default |
|---|---|---|
| Compose | `stop_grace_period` on the service | 10s, and `330s` in the file shipped here |
| Kubernetes | `terminationGracePeriodSeconds` on the pod | 30s |
| systemd | `TimeoutStopSec` | 90s |

Raising `SHUTDOWN_TIMEOUT_SECONDS` past that number changes nothing on its own —
the container is killed first, which looks exactly like the setting being
ignored.
