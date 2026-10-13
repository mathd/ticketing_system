# A version outside the advisory range is not a fix

**2026-10-13 — TKT-504**

A scanner reports a package when its version is in the advisory's affected range. When an upgrade
moves the package out of that range, the scanner stops reporting it. That does not prove that the
vulnerable code is gone. The range can be wrong or incomplete, and the advisory can list no patched
version at all.

## How it was established

GHSA-ch52-4w7c-c8xp (CVE-2026-93748) concerns the `max-stale` handling in `http-cache-semantics`.
It lists affected versions through 4.2.0 and no patched version. `pnpm audit` shows ">=4.2.1" as
patched, but it derives that value from the affected range; the advisory does not state it.

After the upgrade to Astro 7.3.8, the lockfile resolved `http-cache-semantics` 4.3.0. `pnpm audit`
and `repository-scan` stopped reporting it. The TKT-504 code review then executed a probe
against 4.3.0: with `max-stale` in the request, the cache policy returned the cached response, with
its `Set-Cookie` headers, although the response had a zero cache lifetime. The 4.3.0 code is still
vulnerable. (The probe output is in the TKT-504 review thread; this file does not re-run it.)

## The rule

- Before you call a finding resolved by an upgrade, read or execute the vulnerable code path in the
  new version. A silent scanner is not evidence.
- If the code is still vulnerable, record it as an accepted risk with its premise and a guard
  (`docs/conventions/security.md` has the TKT-504 entry as the model).
- Do not add an allowlist entry for a finding that no scanner reports. It has nothing to suppress,
  and it would hide the finding if the advisory range is widened later (TKT-504, D44).
