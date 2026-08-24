# Changelog

Notable changes, newest first. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

While the version is below 1.0, breaking changes can land in a minor release.
They are always listed here first. From 1.0 onward the API and the error codes
are stable, and breaking either needs a major version.

## [0.6.6] - 2026-08-24

Initial release.

A self-hosted LLM gateway: one endpoint in front of every provider you use,
speaking OpenAI, Anthropic and Gemini natively, with keys, budgets, failover,
usage accounting and an admin UI included. `docker compose up -d` and the
setup form is the whole install.

Migrations are applied automatically at start-up. Later release notes will
describe what changed and the supported upgrade path from this baseline.
