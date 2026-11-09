# Browser form verification

Local Compose procedures. Run commands from the repository root. Configuration comes from
[configuration](../configuration.md); safety and approval rules remain in [AGENTS.md](../../AGENTS.md).
Load only the section needed for the operation. Historical examples retain their ticket IDs.

## The browser-submit gate (TKT-228)

`make browser` brings up a smoke-shaped stack on its own ports, runs every `test/browser/*.mjs`
spec against it through real Chrome, and tears it down. It is **not** part of `make check` — it
drives the host's browser, so CI cannot run it and a developer without Chrome must still be able
to pass the gate.

```
make browser              # up, run every spec, tear down
./scripts/browser.sh up   # leave the stack up for iterating
./scripts/browser.sh run  # re-run the specs against it
./scripts/browser.sh down
```

A spec gets `BASE` (the gateway URL) and `POSTGRES_CONTAINER` (for the operator-style `psql`
reads a mailbox or an audit check needs) from the script; it must never hardcode a port. Ports and
the Compose project name come from `scripts/stack-env.sh`, shared with `scripts/smoke.sh`. Project
identities are distinct by default for each checkout and stack kind. Ports use one of 40
deterministic slots; a rare slot collision fails at startup without letting either stack remove the
other's resources.
