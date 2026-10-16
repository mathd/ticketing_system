# A Go patch release can break the linter

**2026-10-13 — TKT-523**

golangci-lint type-checks with the `go/types` of the Go version it was built with. A Go PATCH
release can change the export data format. A linter built before that release then cannot read the
packages, although the minor version is the same.

## How it was established

TKT-523 moved the repository to go1.27.2. The plan review executed the upgrade in a scratch copy
and ran `make lint-go` with golangci-lint v2.13.2. Every Go module failed with:

    export data version 5 is greater than maximum supported version 4

golangci-lint v2.14.0 reads it. The implementer reproduced both results. `make check-go-toolchain`
stayed green for the failing pair, because it compares minor versions only (the script header in
`scripts/check-go-toolchain.sh` records this).

## The rule

- A Go toolchain bump, including a patch bump, is not done until `make lint-go` passes with the
  pinned linter. Bump the linter in the same commit when it fails.
- Do not read a green `check-go-toolchain` as "the linter and Go agree". It catches a minor skew
  only. `lint-go` is the proof.
- A plan reviewer with execution tools found this by running the plan's commands in scratch. The
  read-only drafter could not see it.
