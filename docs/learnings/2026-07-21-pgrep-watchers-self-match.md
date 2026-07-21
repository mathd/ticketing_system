# A `pgrep`-for-the-command-name watcher self-matches its own command line

Date: 2026-07-21 · From TKT-106 (PR #83) · Status: process learning; extends the TKT-101 "background gate exit code lies" family

## What happened

TKT-106 ran the local gate (`make check`) as a backgrounded job and needed to know when it finished.
A watcher was written to poll for the process by name:

```bash
until ! pgrep -f "make check" >/dev/null 2>&1 && ! pgrep -f "smoke.sh" >/dev/null 2>&1; do sleep 5; done
echo "make check finished"
```

It reported "still running" for minutes **after `make check` had already exited cleanly**. The
reason: the watcher's *own* shell command line contains the literal string `make check`, and a second
sibling watcher's command line also contained it. `pgrep -f "make check"` matches against the full
command line of every process — so it matched the watcher processes themselves (and each other),
never returning empty. The gate was long done; the poll could never observe it done.

The authoritative signal was an explicit exit-code sentinel, which read `MAKE_CHECK_EXIT=0`:

```bash
nohup bash -c 'make check > "$0" 2>&1; echo "MAKE_CHECK_EXIT=$?" > "$1"' "$LOG" "$DONE" &
# then wait on the sentinel file, and read PASS/FAIL from it + a log-body scan
```

## The rule

- **Never judge a backgrounded gate by `pgrep -f "<command name>"`.** `-f` matches the whole command
  line, so any watcher, wrapper, or sibling poll that *mentions* the command name matches too — the
  predicate `! pgrep -f "make check"` can be permanently false while nothing real is running. It is a
  false "still running", which is indistinguishable from a genuinely hung gate.
- **Use an explicit exit-code sentinel.** Run the gate as the sole command, capture `$?` to a sentinel
  file (`make check > log 2>&1; echo "EXIT=$?" > done`), wait on the sentinel's existence, then read
  PASS/FAIL from the sentinel plus a log-body scan (`grep -E 'violates|--- FAIL|make: \*\*\*|Error [0-9]+'`).
  The exit code + log body are authoritative; process-liveness polling is not.
- This extends the same failure family as TKT-101 (a background wrapper reported exit 0 across runs
  the log showed failing): **the truth of a background gate is in the log body and a captured exit
  code, never in a liveness heuristic.** If you must check liveness, match the PID you launched
  (`kill -0 "$pid"`), not the command name.

Related: [prove tests fail](./2026-07-15-prove-tests-fail.md) (green is not evidence); the sdlc-ticket
skill's Building gate-mechanics rule carries the one-line agent-facing version.
