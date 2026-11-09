# Catalog rule validation

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## Finding misconfigured rule currencies (TKT-243)

A price or fee rule whose `currency` differs from the currency of a ticket type it applies to is
invalid configuration, not a rule that quietly does not apply: resolution **fails** on it
(ADR-036 §2). Price resolution filters by **channel before** it checks currency (TKT-237, and that
order is deliberate — the alternative made one misconfigured `pos` rule return 500 for every
`reseller` and public request), so such a rule is **invisible until a sale arrives on its channel**,
and then it fails closed at the worst possible moment.

`catalog validate-rules` finds them first, without needing a sale on each channel:

```bash
docker compose exec catalog /app validate-rules
# catalog validate-rules: 1 currency mismatch pair(s)
#   price rule_id=… organizer_id=… ticket_type_id=… scope=venue/… channel=pos \
#     rule_currency=USD ticket_currency=EUR window=[unbounded,unbounded)
```

It needs only the service's usual `DATABASE_URL`. It **reports and decides nothing**: exit code is
`0` whether or not it finds anything, and it performs no writes. Findings are for a human to act on,
so do not wire it into a gate — a failing build is not the intended answer to a misconfigured rule.

**One finding is one `(rule, ticket type)` pair, not one rule.** A rule attaches to one of five
scope levels with no foreign key (ADR-036 §3), so a venue-scoped rule covers every ticket type under
that venue, each with its own currency — one rule can be correct for one ticket type and wrong for
another at the same time. That asymmetry is also why write-time validation cannot do this job
(ADR-036 §4 step 1).

**Two things it deliberately does not filter**, because filtering them would reintroduce the blind
spot it exists to close:

- **channel** — it never joins the `channels` registry. The registry is a lookup, not a constraint:
  a code that was never registered sells exactly like one that was, so consulting it would miss the
  rules most likely to be wrong.
- **`effective_from`** — a rule whose window has not opened yet is precisely what is being hunted.
  It will price the moment it opens and nothing today would notice.

**One thing it deliberately excludes.** Rules whose window has already **closed** are not reported.
They are inert and **unrecoverable**: `currency` is immutable and `effective_until` can only be
shortened, so no write can rescue such a row (ADR-036 §4 step 1, ADR-046 §8). An operator handed
that finding could do nothing with it, and a report that accumulates unfixable rows forever teaches
people to ignore the fixable ones beside it.

**Split schedules are not swept** — they carry no currency at all (shares are basis points, which
are currency-independent; ADR-047), so they cannot mismatch.

**What this does and does not constrain** (ADR-021 — name the adversary). It is an **operator aid
under honest-writer assumptions**. It reads the same tables a writer with catalog database access
writes, so it is not an integrity control and proves nothing against someone who can insert or alter
rules directly. It catches configuration mistakes, not tampering.
