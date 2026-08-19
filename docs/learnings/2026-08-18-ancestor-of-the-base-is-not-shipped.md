# "Ancestor of the review base" is not "shipped"

**TKT-259, ai-review pass 3.** Two of three findings — one graded `[high]` — were correct reasoning
from a false premise about **deployment state**, and both would have cost a real forward migration.

## What happened

The third review pass was scoped to the fix diff, `--base 996bc04d`. It observed that migration
`0022` was introduced in `505a3532`, an **ancestor of that base**, and concluded:

- `[high]` rows parked or charged by the earlier implementation are stranded, because the narrowed
  claim predicate excludes them and there is no unpark command — *"add a forward data migration that
  clears `reversal_attempts`, `reversal_last_error` and `reversal_parked_at`"*;
- `[medium]` editing `0022` in place leaves already-upgraded databases on the old index while fresh
  ones get the new predicate — *"restore 0022 unchanged and add 0023"*.

Both are **correct and necessary** if `0022` has ever run anywhere. It has not: `505a3532` existed
only on the unmerged feature branch. No database had applied the migration, so there were no legacy
rows to normalise and no environments to diverge.

```
git merge-base --is-ancestor 505a3532 origin/main   # -> false
```

One command, and the two findings evaporate.

## Why the reviewer could not know

A review scoped to a diff sees **commit topology**, not **deployment state**. "Ancestor of the base
commit" and "has run in an environment" look identical from inside a diff, and the reviewer chose the
conservative reading — which is the right instinct. The information that settles it lives outside the
diff entirely: what is on the default branch, and what has been deployed from it.

This generalises past migrations. Any finding of the form *"this breaks existing data / existing
deployments / existing consumers"* carries an unstated premise that the thing has shipped.

## The rule

**When a finding turns on whether something has already shipped, resolve the premise before accepting
or refuting it** — and resolve it against the default branch, not the local checkout:

```
git merge-base --is-ancestor <sha> origin/main
git log --oneline origin/main -1 -- <path>
```

Two corollaries:

1. **Record the refutation, don't wave it away.** A refuted finding that would become correct after
   merge is worth a sentence in the closeout, because the reviewer's remediation is exactly what a
   *later* change to that file will need. For `0022`: it must not be edited in place once it is on
   `main`.
2. **Refuted is not the same as overridden.** A gateless run's `overrides:` list should distinguish
   them. Overriding a finding means proceeding against a reviewer's stated objection; refuting one
   means the objection rested on a fact that is checkably false. Conflating the two makes the
   override list either alarming or useless.
