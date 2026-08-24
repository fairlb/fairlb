# Key budgets, and why they fail closed

A key can carry a spend limit: an amount plus a period of `daily`, `monthly` or
`total`. When the limit is reached the gateway answers 402 and no upstream
request is made.

## Where the number comes from

The three periods read different places, for a reason worth stating.

| Period | Read from | Why |
|---|---|---|
| `daily`, `monthly` | Rolling sum over the per-key daily spend table, from the start of the period | A period boundary must be a query, not a reset job. A job that has to run at midnight is a job that can fail at midnight |
| `total` | A denormalised running total on the key | Summing a key's whole history on every request is a cost that grows with the key's age |

Both are written in the **same transaction that records the usage row**. That is
what makes the number trustworthy: a request cannot be recorded as having
happened without its spend being counted, and vice versa, because there is no
window between the two.

## Fail closed

If the spend cannot be read — the query errors, the database is unhappy — the
request is **refused**, with a 500 rather than a 402. It is not allowed through
on the grounds that the limit "probably" is not reached yet.

This is the opposite of what the rate limiter does, and the difference is
deliberate:

| Check | On driver failure | Why |
|---|---|---|
| Spend limit | Refuse | The failure mode of allowing is unbounded spend on somebody's upstream account, discovered on a bill |
| RPM and TPM | Allow | These are capacity controls. Their failure mode is a burst; the security controls that keep a stranger out are elsewhere and still ran |
| Model access tier | Refuse | Allowing means serving under an access policy and price list nobody chose |

The rule underneath: **fail closed when the thing being protected is money or
authorisation, fail open when it is capacity.** Getting this backwards in either
direction is a defect that only shows up on the bad day.

## A distinct code, not a generic 402

"This key is over its limit" and "this account has run out of money" are
different situations with different fixes, so they are different codes even
though both answer 402. The first is fixed by raising the key's limit or using
another key, by the person holding the key; the second is not, and telling them
apart is the difference between a caller who can unblock themselves and one who
files a ticket.

Only the first can occur in a self-hosted install — there is no balance to run
out of here, see below. The code stays distinct anyway, because the client
libraries that consume it are the same ones either way.

Either way the request is uncharged: nothing was sent upstream, so there is
nothing to charge for.

## Order of the checks

The checks run in a fixed order, and both ends of it are deliberate:

```text
access tier  ->  model allowlist  ->  spend limit  ->  team RPM / TPM  ->  key RPM / TPM
```

The access tier goes first because an unavailable tier refuses *every* model. If
the allowlist check ran first, the caller would be told the model does not
exist, and from where they sit that is indistinguishable from the catalog having
been emptied. One specific reason beats a correct but unusable one.

Rate limiting goes last because consuming quota is a write. A request that is
going to be refused by the allowlist must not first eat a slice of the caller's
per-minute budget — the refusal is not their fault and should not cost them
anything.

The team's ceilings are measured before the key's, for the same reason applied
one level up: a request the team-wide ceiling is going to refuse must not spend
one of the key's own requests on the way to being refused. Both are measured;
neither replaces the other, so a key is never wider than the team it belongs to.

Each refusal names which ceiling it hit. "Rate limit exceeded" with no subject
leaves the caller unable to tell "this key is small" from "the whole team is at
its ceiling", and those have different fixes.

## What is enforced and what is not

A single install has no wallet, no prepaid balance and no ledger; those belong
to a service that collects money on your behalf, and this one does not. Spend
limits are not billing — they are an operational guard rail. A runaway script
burning your own upstream credit hurts more than one burning somebody else's, so
the limit stays, and the per-key spend accumulation that feeds it stays with it.
