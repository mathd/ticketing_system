# A proxy can fail while the guarantee holds — and pass while it is gone

**TKT-232 (PR #191) — 2026-08-09**

## What happened

`TestMigrationsRanBeforeServicesStarted` asserts ADR-022's core guarantee: a service never starts
against an unmigrated schema. Compose enforces that as a `depends_on` **condition**. The test
checked it by comparing two container timestamps:

```go
finished, _ := time.Parse(..., inspect(t, job, "{{.State.FinishedAt}}"))
started,  _ := time.Parse(..., inspect(t, srv, "{{.State.StartedAt}}"))
if !finished.Before(started) { t.Fatalf(...) }
```

Under a loaded gate this inverted by **519ms** on one of five services and failed. Nothing was
wrong with the system.

**The interesting half is the other direction.** A proxy that can fail while the guarantee holds
can usually also pass while the guarantee is gone — and that half is silent. Verified here: delete
the `depends_on` condition entirely and, on an unloaded machine, the job still *usually* finishes
before the service starts. The old assertion goes green with the guarantee removed.

So the flaky-looking failure was the visible symptom of a test that was weak in both directions.

## What to do instead

Assert the mechanism the system actually uses. Compose records the resolved edge on the container:

```
$ docker inspect -f '{{index .Config.Labels "com.docker.compose.depends_on"}}' <svc>
svc-migrate:service_completed_successfully:false,nats-init:service_completed_successfully:false
```

That fails in exactly one case — the edge is missing or weakened — which is the case the ADR wants
caught. No clock, so nothing to be flaky.

**Ask what else carries the same condition.** `nats-init` also uses
`service_completed_successfully`, so searching the label for that string passes with the migrate
edge *deleted outright*. The assertion has to match the specific `<service>-migrate` entry. A
substring check would have been the same defect wearing a different hat.

## The part that was nearly missed: what the encoding does not carry

Review pass 2 found the real hole. Compose supports `depends_on.required`, and **it is not encoded
in the label**:

```
required: false  ->  svc-migrate:service_completed_successfully:false
(correct)        ->  svc-migrate:service_completed_successfully:false   # byte-identical
```

`required: false` is not cosmetic. Compose **skips** a failed optional dependency and starts the
service anyway:

```
Container svc-migrate-1  Skipped: optional dependency "svc-migrate"
                         didn't complete successfully: exit 1
Container svc-1          Started
```

That is precisely the violation the assertion exists to catch, reachable while the label still
reads correct. The fix reads `required` from the merged `docker compose config`, because the
container cannot carry it.

**The general shape: when you switch from a proxy to "the real mechanism", check that the encoding
you read carries every field the mechanism honours.** Reading a *closer* source is not the same as
reading a *complete* one, and the fields it silently drops are invisible by construction.

## Two traps found while fixing it

**A "simplification" that silently changed the answer.** Dropping the explicit `-f` list —
`docker compose -p <project> config` — looked equivalent. It is not: it re-discovers the default
compose file and ignores the overrides the stack was created with.

```
with explicit -f flags:  {"condition": "...", "required": false}
with bare -p:            {"condition": "...", "required": true}
```

It would have reported the base file's edge and missed the weakening. Caught by testing the
simplification against the case it was supposed to preserve, not by reading it.

**A plausible drift check that does not check the drift.** A reviewer proposed comparing
`com.docker.compose.config-hash` against a pre-`up` snapshot. The hash is **byte-identical** with
and without `required: false` (a command change does move it, so the probe was sound) — blind to
the one field in question. Recorded in ADR-022 so it is not re-proposed.

## Cost of the caching mistake, for the record

The first mutation run reported `PASS`. Go had served a **cached** result — the test binary and
env were unchanged, and Go cannot see that container state changed underneath. `ok mutcheck
(cached)` is one word away from a real pass. Every mutation run against external state needs
`-count=1`; without it the mutation check certifies nothing while looking exactly like it did.

Related: [three ways one test could not fail](2026-08-06-three-ways-one-test-could-not-fail.md),
[a fixture too small cannot show the negative](2026-08-03-a-fixture-too-small-cannot-show-the-negative.md),
[say what a check establishes](2026-08-03-say-what-a-check-establishes.md).
