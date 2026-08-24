# Reference prices: prefilled, never synchronised

A fresh install has no models, no routes and no prices. That is deliberate — a
gateway cannot guess which upstreams you have accounts with — but it leaves one
step that is pure transcription: for every model you add, four rates copied from
a vendor's price page into a form. Four numbers, per model, retyped.

Transcription is where digits get dropped, and a dropped digit here is a billing
error you find on an invoice rather than in a log.

So the binary ships a snapshot of a public price dataset, and one command fills
the empty rates in from it:

```
fairlb pricing import
```

What that is, and — just as importantly — what it is not, is the subject of this
page.

## What it is not: a price feed

Nothing about this runs on its own. No timer, no background job, no network
call. The request path never consults the dataset; it reads the same
`model_pricing` rows it always did.

That restraint is the point rather than an omission. A gateway that bills by the
token has to be able to answer "why was this request charged that much" with a
number somebody stands behind. A feed that can move on its own cannot answer it:
the rate that produced last Tuesday's invoice may no longer exist anywhere, and
nobody decided to change it. Prices reach this deployment when its operator
decides they should.

What the import removes is only the typing.

## What keeps it honest

**Every imported row is unverified.** The price row has a `verified_at` field
that the console's editor fills in when a person confirms a rate against the
vendor's own list. The import leaves it `NULL`, always — including when it is
run from the console by somebody signed in, because pressing a button is not
the act that field records. "A dataset suggested this" and "somebody checked
this" stay two different facts. The models list marks the difference with an
"unverified price" badge on the row, and the model's own page shows it as an
empty checked-on date.

**A price a person has checked is never overwritten.** Not by a repeat run, not
by `--force`. `--force` exists to replace prices *nobody* has verified; a
verified row is a human decision, and a bulk writer does not get to overrule it.
Those rows are reported as skipped, with the date they were checked.

**Replacing a rate replaces the rates that hang off it.** A model can carry
advanced rates — a dearer rate above a prompt-size threshold, a cheaper one for
batch, a per-call rate for a tool — and those are entered in proportion to the
four base rates beside them. When `--force` writes new base rates it clears them,
and says how many it cleared, both in its output and on the row. Left in place
they would keep multiplying against a number that no longer exists: a base rate
moved from 99 to 3 per million tokens leaves a long-prompt rate still charging
198, a step of sixty-six times where two was meant. The dataset has no opinion
about any of those dimensions, so there is nothing to put back — re-enter them
against the new base. A run that writes no base rate, which is every run that
reports a model as kept or unchanged, leaves them alone.

**Every row records where it came from.** Alongside the rates: the dataset, the
snapshot date, the digest of the exact file, the entry that matched, and how it
matched — an exact model name or a prefix rule.

**A bucket the dataset does not price is stored as zero, and zero means free.**
Both the run's output and the row itself name those buckets and say so in those
terms. Usually it is right: an embedding model has no output tokens to charge
for. But cached input is counted separately from ordinary input, not folded into
it, so a zero on a cache bucket is not "billed as input" — those tokens are
billed at nothing at all, and a cache-heavy workload can be most of a request.
The dataset is also not uniform about it: the same model can be priced with a
cache rate under one vendor's entry and without one under another's. If a model
you are importing supports prompt caching, that is the pair of numbers to check
first.

**Every model it declined to price is named, with the reason.** A silently
missing price is the exact failure this command exists to remove; producing new
ones would be an odd way to finish.

## How a model is matched

The key is the upstream model id on the model's route — the string this gateway
actually sends upstream — narrowed to one vendor first, by the provider's name
when it matches the dataset's, and otherwise by its base URL.

Nothing here guesses:

- An exact name in the dataset beats a prefix or substring rule.
- If two routes for one model resolve to different rates, that is two upstream
  costs for one stored number. The disagreement is reported and nothing is
  written.
- Matching is case-sensitive, because the id being matched is the exact string
  sent upstream, and vendors do ship model names that differ only in case.

## What it will not import

Some shapes cannot be stored as a single rate, and the import says so rather
than storing an approximation:

| Shape | What happens |
|---|---|
| A rate that differs by time of day | Refused, and the model is named. One stored rate cannot hold two prices for the same tokens. |
| A rate that steps up for larger inputs | The base step is stored and the row records that the higher steps were dropped. Check the model if you send long contexts. |
| A model with no route on an enabled provider | Skipped: there is no upstream name to look up, and pricing it would not make it appear in the catalogue anyway. |
| An entry with no input rate | Skipped. Every other bucket missing means "not charged for"; input missing means the entry does not price the model. |

Rates are converted through their decimal text, never through a floating-point
number. The dataset does contain values such as `0.08333333333333334` — a
float artefact of one twelfth — and anything below the stored precision is
rounded, with the affected buckets recorded on the row.

## Running it

```
# print what would happen to every model, and write nothing
fairlb pricing import --dry-run

# fill in models that have no price at all
fairlb pricing import

# also replace prices that nobody has verified
fairlb pricing import --force

# use your own file instead of the bundled snapshot
fairlb pricing import --file /data/prices.json
cat prices.json | fairlb pricing import --file -
```

The image has no shell, so `--file` has to name a path readable inside the
container; `--file -` reads standard input, which is usually easier.

A dry run is not a second opinion computed by other means. It does the whole
job — validating each price, assessing its risks, writing the row, letting
every column constraint run — inside a transaction it then throws away. So what
it prints is what a real run reaches, refusals included. A preview that
predicted the outcome separately would agree at first and drift later, and the
direction of that drift is always "the preview looked fine".

Order matters for the result you are after: a model appears in `/v1/models` only
once it has both a price and at least one enabled route on an enabled provider.
Add the provider and its route first, then import — the other way round the
import has no upstream name to match on.

Afterwards, the models list marks every rate the import wrote as an unverified
price. That badge is the whole of what the import claims for itself; confirming
a rate on the model's page — which fills in its checked-on date and clears the
badge — is the step that turns a reference into something you have agreed to
charge against.

## From the console

The same run is available to a signed-in operator, and the console uses it in
two places.

`POST /gateway/pricing/import` takes `force`, `dry_run` and an optional
`model_ids`, and needs the same role saving a price does — the highest one. It
differs from the command line in exactly one way, and that difference is why it
exists: a request arrives with somebody behind it, so it leaves an audit record
and names that operator on every row it writes. A command running inside a
container has no identity to record, and inventing one would be worse than the
blank it writes instead.

`model_ids` is what keeps it from being a bigger button than it looks. The
provider page's "models served" dialog offers, on each row that will still be
unpriced after saving, to fill its price in from the reference; ticking those
prices exactly those models and nothing else. The order is fixed by the
matching: prices are filled in after the routes are stored, because until then
there is no upstream name to match on — and for a row that creates its own
catalogue entry, no model either.

One timing note: the command writes to the database, and a running gateway
reads its catalogue through a 60-second cache. The command tells that gateway
to drop its copy, so the new prices take effect immediately; if it cannot —
it says so — the previous prices are served for up to a minute. That window
only matters when a price was *replaced* rather than filled in, because filling
one in replaces a refusal rather than a number.

## Licence

The bundled dataset is `genai-prices`, published by Pydantic Services Inc. under
the MIT licence. Its licence text and a record of which revision was taken, when,
and what it hashes to are stored next to the data in the source tree.
