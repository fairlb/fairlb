# Changelog

Notable changes, newest first. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

While the version is below 1.0, breaking changes can land in a minor release.
They are always listed here first. From 1.0 onward the API and the error codes
are stable, and breaking either needs a major version.

## [0.7.0] - 2026-08-28

### Added

- Asynchronous video generation with a durable job lifecycle, idempotent
  submission, artifact custody, cancellation, reconciliation and usage
  settlement.
- Native video surfaces for Kling, Seedance, Veo, MiniMax Hailuo and Wan, plus
  a provider-neutral FairLB video API.
- Image-aware token accounting, per-image prices and per-route output limits.
- Per-unit pricing for seconds, calls and images, including resolution, audio,
  quality variant and service-tier dimensions.
- Model output modalities, persisted provider discovery results and admin UI
  workflows for configuring and operating image and video routes.
- Runtime brand profiles, so one image can be branded at deployment time
  without rebuilding it.

### Changed

- Catalog identity is now enforced as `<creator>/<model>` and model modality is
  separate from protocol reachability.
- Provider and route probing now covers image and video surfaces and records
  in-flight probes without discarding the last verdict.
- Usage logs and hourly rollups record image token dimensions and unit-based
  consumption explicitly.

### Fixed

- Image generation is settled from the number of outputs actually returned,
  including streamed responses, instead of assuming one image.
- Video reservations, late settlement and repair queues are guarded against
  duplicate charging, expired holds and partially completed jobs.
- Multipart requests rewrite model names consistently with JSON requests.

### Removed

- The text-only Playground and its capability have been removed.
- Data-plane CORS handling has been removed; browser clients should call a
  same-origin backend rather than expose gateway credentials cross-origin.

### Upgrade notes

This is a pre-launch baseline reset. Existing v0.6.6 development databases are
not upgraded in place: remove the PostgreSQL data volume and start v0.7.0 with
a fresh database. Do not do this to a database containing data you need to
keep; export that data first.

## [0.6.6] - 2026-08-24

Initial release.

A self-hosted LLM gateway: one endpoint in front of every provider you use,
speaking OpenAI, Anthropic and Gemini natively, with keys, budgets, failover,
usage accounting and an admin UI included. `docker compose up -d` and the
setup form is the whole install.

Migrations are applied automatically at start-up. Later release notes will
describe what changed and the supported upgrade path from this baseline.
