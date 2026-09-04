#!/usr/bin/env bash
# Every Go version this repository declares must agree with the one it is being
# built and linted with. That is more than two numbers, so this reads all of them
# rather than a representative one: the active toolchain, the Go golangci-lint
# was built with, go.work, every workspace go.mod, the production builder image,
# and each workflow's go-version pin.
#
# Why the linter leg: it type-checks with the go/types from its own build, so a
# linter built with Go 1.26 cannot read a package the local Go 1.27 stdlib pulls
# in. It panics with `file requires newer Go version go1.27 (application built
# with go1.26)` and 50 lines of stack, which reads as a code defect and is not.
#
# Why every declared pin: a partial upgrade is the dangerous shape. Local and CI
# agreeing with each other while the modules or the builder image declare an
# older Go lets CI accept standard-library APIs the shipped binary cannot
# compile. An earlier version of this script checked go.work alone while its
# message claimed all of them agreed — a check whose diagnostic is wider than its
# evidence is worse than none, because it is quoted as proof.
#
# Patch differences are fine — Go's type checker changes on minor bumps, and
# Go 1.26.6 against a linter built with Go 1.26.2 is a supported pairing.
set -euo pipefail

minor() { cut -d. -f1,2 <<<"$1"; }

# Each source is parsed by its own shape; a generic "first version-looking token"
# would read golangci-lint's OWN version (2.13.2) as a Go version.
anchors() { # emits "<label>\t<version>" per declared pin
  local root="$1" f v setup_count version_count versions
  v="$(awk '$1=="go"{print $2; exit}' "$root/go.work" 2>/dev/null || true)"
  printf 'go.work\t%s\n' "$v"
  while IFS= read -r f; do
    v="$(awk '$1=="go"{print $2; exit}' "$root/$f" 2>/dev/null || true)"
    printf '%s\t%s\n' "$f" "$v"
  done < <(git -C "$root" ls-files '*go.mod')
  v="$(sed -nE 's/^FROM golang:([0-9]+\.[0-9]+\.[0-9]+).*/\1/p' "$root/build/go.Dockerfile" | head -1)"
  printf 'build/go.Dockerfile\t%s\n' "$v"
  while IFS= read -r f; do
    setup_count="$(grep -Ec '^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]+actions/setup-go@' "$root/$f" || true)"
    [ "$setup_count" -gt 0 ] || continue
    versions="$(sed -nE 's/.*go-version: "([0-9]+\.[0-9]+(\.[0-9]+)?)".*/\1/p' "$root/$f")"
    version_count="$(printf '%s\n' "$versions" | awk 'NF { n++ } END { print n + 0 }')"
    if [ "$version_count" -ne "$setup_count" ]; then
      printf '%s\t\n' "$f"
      continue
    fi
    while IFS= read -r v; do
      [ -n "$v" ] && printf '%s\t%s\n' "$f" "$v"
    done <<<"$versions"
  done < <(git -C "$root" ls-files '.github/workflows/*.yml' '.github/workflows/*.yaml')
}

# Reads "<label>\t<version>" lines on stdin. Pure: the self-test feeds it fixtures.
converge() {
  local line label version m ref="" refl="" bad="" active="" linter=""
  while IFS=$'\t' read -r label version; do
    [ -n "$label" ] || continue
    if [ -z "$version" ]; then
      echo "cannot read a Go version from $label" >&2
      return 2
    fi
    m="$(minor "$version")"
    case "$label" in
      "active Go") active="$m" ;;
      golangci-lint) linter="$m" ;;
    esac
    if [ -z "$ref" ]; then ref="$m"; refl="$label"
    elif [ "$m" != "$ref" ]; then bad="$bad$label ($version)"$'\n'
    fi
  done
  [ -n "$ref" ] || { echo "no Go versions to compare" >&2; return 2; }

  # The linter/toolchain pair has one specific remedy, so say that one plainly.
  if [ -n "$active" ] && [ -n "$linter" ] && [ "$active" != "$linter" ]; then
    echo "golangci-lint was built with Go $linter; active Go is $active. Use Go $linter.x or install a Go $active-compatible linter." >&2
    return 1
  fi
  if [ -n "$bad" ]; then
    echo "Go versions disagree. $refl declares $ref; these do not:" >&2
    printf '%s' "$bad" >&2
    echo "Complete the upgrade across every pin, or move them all back." >&2
    return 1
  fi
}

check_anchors() {
  local root="$1" active="$2" linter="$3"
  {
    printf 'active Go\t%s\n' "$active"
    printf 'golangci-lint\t%s\n' "$linter"
    anchors "$root"
  } | converge
}

selftest() {
  local fail=0 fixture output got expected actual
  case_() { # case_ <expected-rc> <label> <lines...>
    set +e; printf '%b' "$3" | converge >/dev/null 2>&1; local got=$?; set -e
    if [ "$got" != "$1" ]; then echo "FAIL: $2 (expected rc $1, got $got)" >&2; fail=1; fi
  }
  fixture_case() { # fixture_case <expected-rc> <label> <required diagnostic>
    set +e
    output="$(check_anchors "$fixture" 1.27.0 1.27.4 2>&1)"
    got=$?
    set -e
    if [ "$got" != "$1" ]; then
      echo "FAIL: $2 (expected rc $1, got $got)" >&2
      printf '%s\n' "$output" | sed 's/^/  /' >&2
      fail=1
    elif [ -n "$3" ] && ! grep -Fq "$3" <<<"$output"; then
      echo "FAIL: $2 did not name $3" >&2
      printf '%s\n' "$output" | sed 's/^/  /' >&2
      fail=1
    fi
  }
  write_fixture() {
    mkdir -p "$fixture/app" "$fixture/build" "$fixture/.github/workflows"
    printf 'go 1.27.0\n\nuse ./app\n' > "$fixture/go.work"
    printf 'module example.com/app\n\ngo 1.27.2\n' > "$fixture/app/go.mod"
    printf 'FROM golang:1.27.3 AS build\n' > "$fixture/build/go.Dockerfile"
    printf '%s\n' \
      'jobs:' \
      '  check:' \
      '    steps:' \
      '      - name: Set up Go' \
      '        uses: actions/setup-go@v6' \
      '        with:' \
      '          go-version: "1.27.1"' > "$fixture/.github/workflows/check.yaml"
  }

  fixture="$(mktemp -d)"
  git -C "$fixture" init -q
  write_fixture
  git -C "$fixture" add go.work app/go.mod build/go.Dockerfile .github/workflows/check.yaml

  expected=$'go.work\t1.27.0\napp/go.mod\t1.27.2\nbuild/go.Dockerfile\t1.27.3\n.github/workflows/check.yaml\t1.27.1'
  actual="$(anchors "$fixture")"
  if [ "$actual" != "$expected" ]; then
    echo "FAIL: matching fixture did not discover the exact anchor inventory" >&2
    diff <(printf '%s\n' "$expected") <(printf '%s\n' "$actual") >&2 || true
    fail=1
  fi
  fixture_case 0 "matching repository anchors agree" ""

  write_fixture
  sed -i 's/1.27.1/1.26.6/' "$fixture/.github/workflows/check.yaml"
  fixture_case 1 "a discovered workflow mismatch is refused" ".github/workflows/check.yaml (1.26.6)"

  write_fixture
  printf 'module example.com/app\n' > "$fixture/app/go.mod"
  fixture_case 2 "a tracked go.mod without a Go directive is refused" "cannot read a Go version from app/go.mod"

  write_fixture
  printf 'FROM golang:latest AS build\n' > "$fixture/build/go.Dockerfile"
  fixture_case 2 "a malformed builder version is refused" "cannot read a Go version from build/go.Dockerfile"

  write_fixture
  sed -i '/go-version:/d' "$fixture/.github/workflows/check.yaml"
  fixture_case 2 "setup-go without a version is refused" "cannot read a Go version from .github/workflows/check.yaml"

  rm -rf "$fixture"

  ALL='active Go\t1.27.0\ngolangci-lint\t1.27.0\ngo.work\t1.27.0\nshared/go/go.mod\t1.27.2\nbuild/go.Dockerfile\t1.27.0\n.github/workflows/check.yaml\t1.27.0\n'
  case_ 0 "patch differences across every pin are fine" "$ALL"
  case_ 1 "linter behind the toolchain is refused" \
    'active Go\t1.27.0\ngolangci-lint\t1.26.2\ngo.work\t1.27.0\n'
  case_ 1 "1.9 against 1.10 is refused" \
    'active Go\t1.9.7\ngolangci-lint\t1.10.1\ngo.work\t1.9.7\n'
  case_ 2 "a pin that cannot be read is an error" \
    'active Go\t1.27.0\ngolangci-lint\t1.27.0\ngo.work\t\n'
  case_ 2 "nothing to compare is an error" ''
  [ "$fail" -eq 0 ] || { echo "check-go-toolchain: comparison regression" >&2; return 1; }
}

if [ "${1:-}" = "--selftest" ]; then
  selftest
  exit
fi

LINTER="${1:?usage: check-go-toolchain.sh <path to golangci-lint> | --selftest}"
ROOT="$(git rev-parse --show-toplevel)"
ACTIVE_VERSION="$(go env GOVERSION | sed 's/^go//')"
LINTER_VERSION="$("$LINTER" --version | sed -nE 's/.*built with go([0-9.]+).*/\1/p')"
check_anchors "$ROOT" "$ACTIVE_VERSION" "$LINTER_VERSION"
echo "go toolchain: every declared pin, the active Go and the linter's build agree on $(minor "$ACTIVE_VERSION")"
