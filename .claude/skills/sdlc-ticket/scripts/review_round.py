"""Decide the next action for one completed review round.

Ported from the py-moov sdlc-ticket `contracts.py review` command. It reads a
round record and the project config. It does not call a tracker, a model or git.
See references/reviews.md for the procedure that produces the record.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

SEVERITIES = {"Critical", "High", "Medium", "Low", "Nit"}
MAX_REVIEW_ROUNDS = 4
MATERIAL_SEVERITIES = {"Critical", "High", "Medium"}
REPEAT_SEVERITIES = {"Critical", "High"}
STAGES = {"plan-review", "ai-review"}


def require(condition: Any, message: str) -> None:
    if not condition:
        raise ValueError(message)


def text(value: Any) -> bool:
    return isinstance(value, str) and bool(value.strip())


def validate_policy(config: Any) -> dict[str, Any]:
    require(isinstance(config, dict), "config must be an object")
    policy = config.get("review")
    require(isinstance(policy, dict), "config.review is required")
    mapping = policy.get("severityMap")
    require(
        isinstance(mapping, dict)
        and all(text(k) and v in SEVERITIES for k, v in mapping.items()),
        "invalid severity map",
    )
    return policy


def severity(value: str, policy: dict[str, Any]) -> str:
    normalized = value if value in SEVERITIES else policy["severityMap"].get(value)
    require(normalized in SEVERITIES, f"unknown severity: {value}")
    return normalized


def review_action(
    record: Any, policy: dict[str, Any], authorized_through: int = MAX_REVIEW_ROUNDS
) -> str:
    """Return HUMAN_REVIEW, FIX, REVIEW_AGAIN or STOP for a round record.

    Raises:
        ValueError: the record is invalid or incomplete.

    """
    require(isinstance(record, dict), "review record must be an object")
    require(record.get("stage") in STAGES, "invalid review stage")
    material_change = record.get("materialChange", False)
    require(type(material_change) is bool, "materialChange must be boolean")
    number = record.get("round")
    require(
        type(authorized_through) is int
        and authorized_through >= MAX_REVIEW_ROUNDS
        and type(number) is int
        and 1 <= number <= authorized_through,
        "round outside configured budget",
    )
    require(
        text(record.get("artifact")) and text(record.get("reviewer")),
        "artifact and reviewer required",
    )
    results: Any = record.get("results")
    require(
        isinstance(results, list) and len(results) == 2,
        "initial review and audit required",
    )
    findings = []
    blocked = False
    for output, lens in zip(results, ("initial", "audit"), strict=True):
        require(isinstance(output, dict), "review output must be an object")
        require(
            output.get("complete") is True and text(output.get("summary")),
            "incomplete review output",
        )
        require(output.get("lens") == lens, "review lenses missing or out of order")
        requests = output.get("researchRequests", [])
        require(
            isinstance(requests, list) and not requests,
            "a complete result cannot carry open research requests",
        )
        require(
            all(
                output.get(k) == record[k]
                for k in ("artifact", "reviewer", "round", "stage")
            ),
            "review scope, identity, or revision mismatch",
        )
        require(
            output.get("verdict") in {"APPROVE", "REVISE", "BLOCKED"},
            "missing review verdict",
        )
        current = output.get("findings")
        require(isinstance(current, list), "findings must be a complete array")
        for item in current:
            require(
                isinstance(item, dict)
                and all(
                    text(item.get(k))
                    for k in ("id", "severity", "evidence", "impact", "correction")
                ),
                "incomplete finding",
            )
            level = severity(item["severity"], policy)
            require(
                not (output["verdict"] == "APPROVE" and level in MATERIAL_SEVERITIES),
                "approval contains material findings",
            )
            findings.append((item["id"], level))
        require(output["verdict"] != "REVISE" or current, "REVISE requires findings")
        blocked = blocked or output["verdict"] == "BLOCKED"
    require(
        len({key for key, _ in findings}) == len(findings), "finding IDs must be unique"
    )
    if blocked:
        return "STOP"
    dispositions = record.get("dispositions", {})
    evidence = record.get("dispositionEvidence", {})
    refutations = record.get("refutationChecks", {})
    require(isinstance(refutations, dict), "refutationChecks must be an object")
    finding_ids = {key for key, _ in findings}
    confirmed_refutations: set[str] = set()
    disputed = False
    for key, confirmation in refutations.items():
        require(
            key in finding_ids,
            "refutation check must name a rejected finding or its subsequent fix",
        )
        require(
            isinstance(confirmation, dict)
            and confirmation.get("artifact") == record["artifact"]
            and confirmation.get("reviewer") == record["reviewer"]
            and confirmation.get("complete") is True
            and type(confirmation.get("refuted")) is bool
            and text(confirmation.get("evidence")),
            "refutation check must identify the independent reviewer and this revision",
        )
        if confirmation["refuted"]:
            require(
                dispositions.get(key) == "rejected",
                "confirmed refutation must name a rejected finding",
            )
            confirmed_refutations.add(key)
        elif dispositions.get(key) == "rejected":
            disputed = True
    for key, _ in findings:
        if dispositions.get(key) not in {"fixed", "rejected", "deferred"} or not text(
            evidence.get(key)
        ):
            return "STOP" if number == authorized_through else "FIX"
    if disputed:
        return "STOP"
    if record.get("verificationPassed") is not True:
        return "FIX"
    repeat_levels = (
        MATERIAL_SEVERITIES if record["stage"] == "plan-review" else REPEAT_SEVERITIES
    )
    if material_change or any(
        level in repeat_levels and key not in confirmed_refutations
        for key, level in findings
    ):
        return "STOP" if number == authorized_through else "REVIEW_AGAIN"
    return "HUMAN_REVIEW"


def validate_review_target(
    record: dict[str, Any], artifact: str, reviewer: str, authors: list[str]
) -> None:
    require(
        text(artifact) and record.get("artifact") == artifact,
        "unexpected review artifact",
    )
    require(
        text(reviewer) and record.get("reviewer") == reviewer,
        "unexpected reviewer identity",
    )
    require(
        authors and all(text(author) for author in authors),
        "actual author identities required",
    )
    require(reviewer not in authors, "reviewer must be independent of all authors")


def load(path: str) -> Any:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    review = sub.add_parser("review")
    review.add_argument("path")
    review.add_argument("--config", required=True)
    review.add_argument("--artifact", required=True)
    review.add_argument("--reviewer", required=True)
    review.add_argument("--author", required=True, action="append")
    review.add_argument("--authorized-through", type=int, default=MAX_REVIEW_ROUNDS)
    review.add_argument(
        "--authorization",
        help="ticket comment holding the human's explicit authorization beyond round four",
    )
    args = parser.parse_args()
    try:
        data = load(args.path)
        validate_review_target(data, args.artifact, args.reviewer, args.author)
        require(
            args.authorized_through == MAX_REVIEW_ROUNDS or text(args.authorization),
            "human authorization reference required for extra rounds",
        )
        action = review_action(
            data, validate_policy(load(args.config)), args.authorized_through
        )
    except (ValueError, KeyError, TypeError, AttributeError, OSError) as exc:
        print(f"FAIL: {exc}")
        return 1
    print(action)
    return 0 if action == "HUMAN_REVIEW" else 1


if __name__ == "__main__":
    raise SystemExit(main())
