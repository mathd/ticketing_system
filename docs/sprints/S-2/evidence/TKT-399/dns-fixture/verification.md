# TKT-399 DNS fixture verification draft

This candidate changes only Access smoke fixtures, smoke assertions and the direct dependency declaration for the existing `golang.org/x/net v0.61.0`. `go.sum` is unchanged. Production code, the 500 ms step deadline, scanner behavior and earlier Exit evidence are unchanged.

The implementation started from `2c8b35bbad7f24a863e7ef7ef8b9c89613931174` on `TKT-399-dns-fixture`. The target worktree and primary checkout were clean. The governing amended plan has SHA-256 `b879ddcb39ca850fc0f968a56c3309b147c7f0e3c6a0b77fd254627a42fe9fc0`. This is an uncommitted candidate receipt, not a gate, review or ticket-completion verdict.

## Observed TDD

The responder, fixture observations and store wire assertions were added with the original resolver calls still in `beforeConnect`. The first real-database run exited 1. All 20 cases failed at `no-owned-wire-questions: operation did not reach the DNS producer`: four lookup sites, four dial-attempt sites and twelve mixed site/variant pairs. The assertion ran before classification. See [the red log](runs/tdd-red.log).

The three paths were then wired separately. Lookup passed at all four sites, followed by dial attempt at all four sites and all twelve mixed cases. The first red run is distinct from the retained original timing failure. It detects absent owned wire traffic without requiring a slow host resolver.

All commands ran from `/home/mathieu/sources/ticketing_system.TKT-399-dns-fixture`. Each shell sourced `/tmp/sdlc-S-2/dns-testdb-r3-20261020/environment.sh`. Both database variables were present. [checks.json](checks.json) records the complete commands, UTC start/end times, exit codes, log paths and SHA-256 digests.

| Run | Exit | Elapsed seconds | Evidence |
|---|---:|---:|---|
| Initial store wire assertions, old resolver calls | 1 | 5.714 | [Red](runs/tdd-red.log) |
| Lookup wiring | 0 | 2.026 | [Lookup](runs/lookup-green.log) |
| Dial-attempt wiring | 0 | 1.974 | [Attempt](runs/attempt-green.log) |
| Mixed wiring | 0 | 3.724 | [Mixed](runs/mixed-green.log) |
| Complete testdb smoke suite | 0 | 2.333 | [Helper](runs/helper-green.log) |
| Focused store suite and required controls | 0 | 19.456 | [Controls](runs/focused-controls.log) |
| Race smoke, testdb/store/api | 0 | 87.586 | [Race](runs/race-smoke.log) |
| Focused smoke lint, testdb/store | 0 | 3.934 | [Lint](runs/focused-lint.log) |
| Final DNS store suite and disarm control | 0 | 6.069 | [Final focused run](runs/final-focused.log) |

The final focused run includes the explicit missing-recovery and missing-socket assertions added after the first mixed pass. The race run also includes those assertions. Subsequent code-file edits only wrapped comments.

## What the checks establish

The private Go resolver decodes real DNS replies from an owned loopback TCP listener. The responder preserves the ID, opcode, recursion-desired bit and question, sets authoritative NXDOMAIN, and returns no answers. Protocol tests cover A/AAAA, EDNS0, unexpected names/classes/types, response messages, multiple questions, malformed and truncated frames. Observations are copies. Producer errors remain available after close.

Concurrent `LookupHost` tests require 24 wire questions for 12 calls, one A and one AAAA per call. Concurrent `DialContext` tests require both question types and a real DNS error on every call, without a count assumption because lookups can share a singleflight. Error assertions require the exact absolute name, `IsNotFound`, no temporary flag and no timeout flag.

The partial-frame test waits until the handler reads the length prefix. Close must join the handler within one second, close the client and listener, and empty the connection registry. The handler deadline remains ten seconds. The test also calls Close again.

Store tests require new decoded questions before classification. Mixed tests inspect the direct join under the actual outer `pgconn.ConnectError`. They require DNS, real 57P03 and the intended socket leaf before classification. Unsupported cases require a new blocked-dial entry and a timeout for that listener's exact address. They check opposite socket orders. pgx collects the DNS branch first in both variants. The successful lookup after disarm is a healthy-operation control only.

The focused control run exercised real 57P03, localhost recovery, 53300, wrong-password refusal, ECONNREFUSED, the independent 100 ms timeout, blocked-dial expiry and later-transaction negative controls. It also retained the successful transaction and late-callback controls. These selected tests did not skip.

The race command passed all three packages. Four opt-in representative-volume migration tests were not enabled. Their measurement variables were absent, so this receipt claims no measurement result. [environment-presence.json](environment-presence.json) records presence only, without credentials.

## Original failure evidence

[manifest.json](manifest.json) identifies the original failed gate on head `2c8b35bbad7f24a863e7ef7ef8b9c89613931174`, tree `3bec9e9116273f948bbf7e5fe9abcd02bcb2122c`, with exit 2. The full retained gate log has SHA-256 `6d4dcd9a86b2eea59dd182d4cbf1ded9da002aa7293e52624b8d67ba0492bf56`.

[Both original failure trees](original-failing-subtests.log) remain intact. The lookup case returned `step_expired` over a real DNS error. The mixed case returned `step_expired` with DNS and canceled socket branches. The [original probe](original-host-dns-probe.jsonl) includes a 549.209 ms lookup. This supports a broken external-DNS timing premise; it does not establish a production classification defect or account for every millisecond of either failed operation.

The original verdict, failure record, probe and diagnosis are preserved verbatim with digests. The diagnosis's old next-step instruction is historical and was superseded by D52. Earlier scanner receipts are unchanged.

## Pending verification and limits

No mutation has run. The parent must inspect and commit this candidate first. The external driver is `/tmp/sdlc-S-2/TKT-399-dns-implementation/mutate.py`; its ten owning selectors are in `mutation-selectors.json` beside it. It refuses dirty tracked files, an uncommitted candidate, changed candidate bytes and gate locks. It keeps byte copies, restores each independent mutant in `finally`, requires the named failing assertion, then requires eight restored passes. A survivor, skip or restored failure stops the driver. Its syntax and unique replacement targets were checked; its mutation execution remains unverified.

Full local and post-merge gates, independent code-review round 9, CI, merge, COS validation and ticket completion remain with the parent. Browser evidence was not rerun in this bounded fixture task. This draft does not replace the original four functional COS or their evidence.

This proof excludes the host resolver chain, UDP, search-list behavior, SERVFAIL classification and DNS timeout shapes. The planned SERVFAIL mutant is discrimination evidence only. `DNSError.Server` still identifies the configured resolv.conf server, not the private listener; neither its value nor error-message spelling is asserted. The resolver still reads system lookup configuration. Owned DNS removes upstream transport latency, not scheduler or database delays. The successful healthy lookup uses an IP literal and does not prove DNS isolation. Global resolver non-mutation and healthy-path wiring need structural review.
