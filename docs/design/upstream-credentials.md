# Which upstream credential a request uses

Every request that leaves for an upstream carries one credential. Two things can
supply it, they have a fixed priority, and this install deliberately exposes
only one of them.

## The two sources

| Source | Belongs to | Where it is entered |
|---|---|---|
| Provider key | The provider record — the account this install has with the upstream | The provider's key list in the admin UI |
| Organization-supplied key | A organization, for one vendor | Not exposed here; see below |

The second exists because a multi-tenant deployment has customers who already
have their own contract with an upstream and would rather pay the upstream
directly. That is a real arrangement, and the gateway supports it: the organization
enters a key for one vendor, and requests from that organization that route to that
vendor go out under it.

## The priority rule

When a organization-supplied key exists for the vendor a candidate belongs to, it
wins. Concretely:

1. Resolve the organization's keys — all of them, once per request rather than once
   per candidate. They are a property of the organization, and candidate rotation
   changes which one applies but not the set.
2. A candidate uses the key whose vendor matches its provider's, and that key's
   own base URL overrides the provider's if it set one.
3. If there is no key for that vendor, or the organization enabled fallback and the
   key failed there, the provider's key pool is used.

## Several keys on one provider

The keys on a provider are a pool, not a primary and its standbys. Requests are
spread over them in turn: a counter per provider decides where the search for a
usable key starts, and keys that are cooling down are skipped.

Taking the first usable key every time would put the whole load on the oldest
one, which is wrong for the case several keys mostly exist for — sharing one
account's quota. Spreading also keeps every key warm, so a credential revoked
upstream is found out by ordinary traffic rather than at the moment the first
one finally trips and the second turns out to have been dead for a month.

It is not a fair scheduler and does not try to be. Nothing here measures what
any one key has actually consumed; even coverage is the goal, exact balance is
not.

**By vendor, not by protocol.** A organization supplying a key is stating "I have an
account at this platform", and that sentence has no meaning at the level of a
protocol: dozens of companies speak the OpenAI protocol. Matching by protocol
offered one company's credential to whichever of them routing happened to reach,
which sends a secret to a party it was never issued to. Matching by vendor
cannot: the candidate either belongs to that platform or it does not.

Rejection is scoped the same way. A key the upstream rejects is marked invalid
and, where the organization allowed the fallback, dropped for the rest of that
request — for **its own vendor only**. The organization's accounts elsewhere are
separate contracts, and a rejection at one says nothing about another.

The fallback switch is scoped the same way too, and it is worth stating because
the obvious reading is wider than the rule. Declining the fallback means "do not
retry my rejected key on yours". It does not mean "never serve me on yours": a
candidate at a platform the organization holds no key for has no organization credential to
fall back *from*, so it is served on a shared one and priced at full list
whatever the switch says. Reading it the wider way would make one supplied key
stop an org being routed to every other platform.

Anything that goes wrong while resolving the organization key — not configured,
configured but undecryptable, marked invalid — degrades to "do not use the
organization key" rather than failing the request. Failing would take a working
gateway down over a configuration mistake in one credential. The cost of
degrading is that the request is priced as an ordinary one, and that cost is
**visible**: the usage row records which way it went.

## Why this install has one place to enter credentials

The organization-supplied path is not served here. Requests to its endpoints answer
404 the same way an endpoint that does not exist answers.

The reason is the priority rule above. A single install has one operator, who is
also the only organization. Every upstream key they enter is already "their own key" —
there is no second party. Exposing both paths would not add a feature; it would
add **a second write path with an invisible priority**. The same person,
configuring from two pages, ends up with two keys where the one that takes
effect is the one on the page they look at less often. Debugging that means
knowing this document exists.

So the second path stays closed, and this is enforced by path rather than by
endpoint name, for two reasons:

- **The rejection has to happen before the request is bound and validated.** An
  endpoint that answers 400 for a malformed body has already told the caller
  that it exists. Refusing before parsing keeps 404 meaning 404.
- **Endpoints added later are covered automatically.** Matching on the path
  segment means a new endpoint under it is closed by default, and "closed by
  default" is the direction you want to be wrong in.

## What is not modelled

A organization-supplied key is bound to exactly one vendor, and only to a vendor this
install actually routes to. A key for a platform with no provider could never be
chosen by anything, so it would sit in the organization's list looking configured and
never take effect; it is refused when it is entered, which is the only place
that difference is visible to whoever is entering it.

Which protocols that vendor speaks is a separate question, answered by the
provider records. One key covers every candidate at its platform, whichever
protocol the request arrived on.
