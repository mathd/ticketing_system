#!/usr/bin/env python3
"""Run named probe mutations, capture stdout, and compare six restored runs."""

import hashlib
import os
from pathlib import Path
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[3]
EVIDENCE = Path(__file__).resolve().parent
SOURCE = EVIDENCE / "probe.go"
CAPTURE = EVIDENCE / "probe.stdout"
MUTATIONS = [
    (
        "offline expiry",
        "return !now.Before(c.NBF.Add(-skew)) && now.Before(c.EXP.Add(skew))",
        "return !now.Before(c.NBF.Add(-skew))",
        ["FAIL offline old exp+5s:"],
    ),
    (
        "refusal gate actuation",
        'return "expired_credential", false',
        'return "expired_credential", true',
        ["FAIL expired local evaluation keeps gate closed:"],
    ),
    (
        "unknown decision routing",
        '\tif o.Decision != "admitted" && !knownLocalRefusal(o.Decision) {\n\t\treturn "unknown_decision"\n\t}\n',
        "",
        [
            "FAIL unknown decision rejected before ledger mutation:",
            "FAIL unknown decision leaves ledger unchanged:",
        ],
    ),
    (
        "local clock branch",
        '\tif !clockTrusted(evidence) {\n\t\treturn "unknown_clock", false\n\t}\n',
        "",
        ["FAIL missing clock local evaluation decision:"],
    ),
    (
        "early credential reason",
        'return "not_yet_valid", false',
        'return "expired_credential", false',
        ["FAIL early signed credential at T-6 local evaluation decision:"],
    ),
]


def run(source: Path, cache: Path) -> subprocess.CompletedProcess[bytes]:
    env = os.environ.copy()
    env["GOCACHE"] = str(cache)
    return subprocess.run(
        ["go", "run", str(source)],
        cwd=ROOT,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def main() -> None:
    original = SOURCE.read_text()
    with tempfile.TemporaryDirectory(prefix="tkt356-validation-") as temp_name:
        temp = Path(temp_name)
        cache = temp / "gocache"
        baseline = run(SOURCE, cache)
        if baseline.returncode != 0:
            raise SystemExit(f"baseline failed with exit {baseline.returncode}\n{baseline.stdout.decode()}")
        CAPTURE.write_bytes(baseline.stdout)
        print(
            f"baseline exit=0 bytes={len(baseline.stdout)} lines={baseline.stdout.count(bytes([10]))} "
            f"sha256={digest(baseline.stdout)}"
        )

        for index, (name, old, new, expected_failures) in enumerate(MUTATIONS):
            if original.count(old) != 1:
                raise SystemExit(f"{name}: expected one mutation anchor, found {original.count(old)}")
            mutant = temp / f"mutant-{index}.go"
            mutant.write_text(original.replace(old, new, 1))
            formatted = subprocess.run(["gofmt", "-w", str(mutant)], check=False)
            if formatted.returncode != 0:
                raise SystemExit(f"{name}: gofmt failed")
            result = run(mutant, cache)
            output = result.stdout.decode()
            if result.returncode == 0 or any(marker not in output for marker in expected_failures):
                raise SystemExit(
                    f"{name}: expected named assertions {expected_failures!r} to fail; "
                    f"exit={result.returncode}\n{output}"
                )
            failures = [line for line in output.splitlines() if line.startswith("FAIL ")]
            if any(not any(marker in line for line in failures) for marker in expected_failures):
                raise SystemExit(f"{name}: named assertion was not a failure: {failures}")
            print(f"mutation {name}: exit={result.returncode}; assertions red: {' | '.join(failures)}")

        for index in range(1, 7):
            result = run(SOURCE, cache)
            if result.returncode != 0:
                raise SystemExit(f"restored run {index} failed with exit {result.returncode}\n{result.stdout.decode()}")
            if result.stdout != baseline.stdout:
                raise SystemExit(f"restored run {index} differs from captured baseline")
            print(
                f"restored run {index}: exit=0 bytes={len(result.stdout)} "
                f"lines={result.stdout.count(bytes([10]))} sha256={digest(result.stdout)} exact-match=yes"
            )

    if SOURCE.read_text() != original:
        raise SystemExit("probe source changed during validation")


if __name__ == "__main__":
    main()
