# Contributing to FairLB

## The unusual bit: how code gets here

Development happens in a private repository; this repository receives **export
snapshots**. So the git history here is a series of releases rather than a
commit-by-commit record.

**Your pull requests are still welcome and still get merged** — the path is just
slightly different:

1. You open a PR here, as usual.
2. A maintainer reviews it here — discussion, review, changes all happen in your PR.
3. Once it's ready, the patch is applied in the development repository **with
   your authorship preserved** (`git am` keeps you as the commit author).
4. It reaches this repository in the next export, and your PR is closed with a
   pointer to the release that contains it.

Why this shape: the hosted service (FairLB Cloud) shares most of its code with
this project, and keeping one repository as the single source of truth is what
makes a small team able to maintain both. The trade-off is visible here in the
history — we'd rather be upfront about it than pretend otherwise.

If this project grows a steady group of contributors, the arrangement can flip
(public repository as the primary one). That's a decision waiting on reality,
not a closed door.

## Before you open a PR

- **Sign off your commits** (`git commit -s`). That's the
  [Developer Certificate of Origin](https://developercertificate.org/) — you're
  stating you wrote the patch or have the right to contribute it. CI checks this
  on every pull request; if you forget, `git rebase --signoff origin/main` fixes
  the whole branch at once.
- **There is no CLA.** You keep the copyright to what you write, and it stays
  under Apache-2.0 like the rest of the project. The trade-off is ours, not
  yours: we give up the ability to relicense your contribution unilaterally.
- Run the checks: `make verify` runs everything CI runs. It needs Docker (tests
  spin up real PostgreSQL) and Node/pnpm for the frontend.

## What makes a good PR

- **One thing per PR.** Mixed changes are hard to review and hard to revert.
- **Explain why, not just what.** The diff shows what changed; the description
  should say what problem it solves and what you considered.
- **Tests that would fail without the fix.** A test that passes before and after
  proves nothing.
- Existing code comments explain *why* things are the way they are — please keep
  that habit. If you find one that's wrong, fixing it is a real contribution.

## Reporting bugs

Include: what you did, what you expected, what happened, and the version
(`fairlb version`). If it involves a specific model provider, say which one —
upstream behaviour varies more than you'd think.

Security issues go through [SECURITY.md](SECURITY.md), not the issue tracker.
