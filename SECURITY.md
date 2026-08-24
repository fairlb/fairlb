# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

Use GitHub's [private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository (Security → Report a vulnerability). That channel is private
and reaches the maintainers directly.

You should get an acknowledgement within a few days. If you don't, the report
may have gone astray — please follow up through the same channel.

## Scope

In scope: the gateway itself — authentication, key handling, organization isolation,
the data plane, the admin API.

Out of scope: vulnerabilities in upstream model providers, and issues that
require an attacker to already hold the database credentials or `SECRET_KEY`
(with those, the system is compromised by definition).

## Handling

Reports are triaged privately. Fixes land in the private development repository
and reach this repository in the next export, together with a security advisory.
We'll credit reporters who want credit.
