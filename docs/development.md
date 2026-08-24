# Development

Go for the gateway, TypeScript and React for the admin UI, PostgreSQL for
everything that persists. If you are here to send a patch, read
[CONTRIBUTING.md](../CONTRIBUTING.md) first — this project accepts them through
a slightly unusual path.

## Prerequisites

| Tool | Why |
|---|---|
| Go, the version in `go.mod` | The gateway |
| Node 24 and pnpm (via corepack) | The admin UI and the code generation it owns |
| Docker | The tests start a real PostgreSQL; nothing is mocked at the database boundary |

## Running it locally

```bash
export DATABASE_URL='postgres://fairlb:fairlb@localhost:5432/fairlb?sslmode=disable'
export FAIRLB_DATA_DIR=./bin/data
make dev
```

That is the API on `:8080`. Migrations run on start, and with no administrator
yet the log prints a link to the setup page.

`FAIRLB_DATA_DIR` is worth setting because its default, `/data`, is a path
inside the container and not one you can write to on your own machine. It is
where the generated master key is kept, so start-up fails rather than carrying
on without one — pointing it at an ignored directory in your checkout gives you
a key that survives restarts, which is what makes stored provider credentials
readable tomorrow.

For UI work, run the admin app from Vite instead of the embedded copy:

```bash
cd web && pnpm --filter @fairlb/staff dev
```

It serves on `:5175` and proxies `/api` to `:8080`, so the Go process does not
have to be restarted for a frontend change. Opening `:8080` directly without a
build of the UI shows a note saying so, rather than a 404 — "no page here" and
"you are running the API without the UI" are answers to different questions.

## Build tags

Ordinary commands need none — `go build ./...` and `go test ./...` work as they
come, and so does your editor. That is deliberate: a repository that only
compiles with a flag greets every newcomer with a screen of red in their
language server, and nothing on screen says why.

Two opt-in tags exist:

| Tag | Effect |
|---|---|
| `webembed` | Bakes `web/apps/staff/dist` into the binary. The container image is built with it, so self-hosting is one file |
| `live` | Opts in to tests that talk to real upstream providers. Off by default; they need credentials and they cost money |

## The gate

```bash
make verify
```

It runs the numbered steps the Makefile prints (lint, build, configuration docs,
generated-code drift, the Compose environment contract, tests, brand assets,
markdown rendering, frontend); `make help` lists each one as its own target. The CI
workflow is a thin shell around it rather than its own list of steps — a second
set of criteria in a workflow file drifts away from the first, and it always
drifts toward "looks like it passed".

A second workflow brings the compose stack up and uses it: sign in, create a
key, call the data plane. That one answers a question `make verify` cannot —
whether somebody who runs the documented command ends up with something that
works. It has been possible for every test to pass while the answer was no.

Run it before opening a pull request. If a step is slow to reproduce locally,
run that step alone rather than skipping the gate:

```bash
make lint
make test
make web-typecheck
```

## Generated code

Some files are produced by a generator and must not be hand-edited. `make
generate` rebuilds these, and their output is committed:

| Output | Produced from |
|---|---|
| `foundation/errcode/errcode.gen.go` | `api/errors-core.yaml`, the error registry |
| `web/packages/api-client/src/errors.gen.ts` | the same file |
| `internal/gateway/db/`, `internal/community/db/` | the SQL owned by those modules under their `queries/` directories, through sqlc |
| `internal/{gateway,community}/*/api.gen.go` | the OpenAPI specs under `api/`, through oapi-codegen |
| `web/packages/api-client/src/gen/` | the same specs, through orval |

The drift step of `make verify` regenerates all of it and compares the result
against what is committed, so "regenerated but not committed" and "never
regenerated" both stop the gate. They look identical to it, which is why the
message names both.

## Tests

```bash
make test
```

The database-touching tests bring up PostgreSQL in a container and apply the
real migrations, so what they exercise is the schema that ships. The first run
pulls an image; later runs reuse it.

Frontend tests run from the same place as the rest of the frontend tooling:

```bash
cd web && pnpm -r test
```

Two conventions worth knowing before you write one:

- **A test asserts a mechanism, not a wording.** If it can pass while the
  feature is broken and only fail when a sentence changes, it is testing the
  sentence.
- **Anything suspicious gets a probe before it gets a claim.** Verify that a
  test fails when the behaviour it describes is removed. A test that cannot fail
  is indistinguishable from one that passes.

## Layout

```text
cmd/fairlb/            entry point: wiring, router, commands
gateway/               the gateway module's public surface: NewModule, Mount, workers
internal/gateway/      the product: catalog, routing, proxy, pricing, usage
internal/community/    the single-install pieces: config, setup, sessions
access/ audit/ settings/ usage/
                       API keys, audit log, settings registry, metering
foundation/            infrastructure: HTTP middleware, drivers, crypto, money, db
migrations/            SQL, applied in order at start-up
api/                   OpenAPI specs and the error registry — the contract
web/apps/staff/        the admin UI
web/packages/          UI components, feature modules, generated API client
docs/                  this documentation
```

The split between `gateway` and `foundation` is a dependency direction, not a
filing convention: `foundation` knows nothing about the gateway, and code moves
down into it only when a second caller needs it. `gateway/` is the one door into
`internal/gateway`: the entry point assembles the module through it and never
reaches past it.

## Style

Comments explain **why**, not what. The what is in the code directly underneath
and it does not need restating; the reason a surprising line is the way it is
exists nowhere else, and it is what the next reader — often the author, months
later — actually needs.

Money is `int64` nanos plus a currency, never a float. Times are `timestamptz`
and reasoned about in UTC. Both rules exist because the alternative fails
rarely, quietly, and in the direction of the number being wrong.
