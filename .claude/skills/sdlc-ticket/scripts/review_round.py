"""Decide the next action for one completed review round.

Ported from the upstream sdlc-ticket `contracts.py` review commands, including
the review-convergence rules (finding origin, delta inspection, round history,
security pass). It reads a round record and the project config. It does not
call a tracker, a model or git. See references/reviews.md for the procedure
that produces the record.

Commands:
    review        decide the next action for one record (one lens pair)
    sprint-round  decide the next action for one sprint round: the four area
                  records of one `sprint-review` round, taken together
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
STAGES = {"plan-review", "ai-review", "sprint-review"}
SPRINT_AREAS = {"implementation", "verification", "documentation", "retrospective"}
ORIGINS = {"introduced", "pre-existing", "unknown"}
INSPECTIONS = {"full", "delta"}
REVIEW_KEYS = {"severityMap", "guidance", "securityPass"}


def require(condition: Any, message: str) -> None:
    if not condition:
        raise ValueError(message)


def text(value: Any) -> bool:
    return isinstance(value, str) and bool(value.strip())


def model_identity(value: str) -> str:
    """Compare known runner spellings after the orchestrator verified the identity.

    Returns:
        A canonical spelling for known aliases, otherwise the supplied identity.

    """
    require(text(value), "actual model identity required")
    aliases = {
        "opus-5.5": "claude-opus-5-5",
        "sonnet-5.5": "claude-sonnet-5-5",
        "haiku-5.5": "claude-haiku-5-5",
    }
    name = value.strip().split(":", 1)[0]
    prefix, separator, suffix = name.partition("/")
    if separator and prefix in {"anthropic", "openai"}:
        name = suffix
    return aliases.get(name, name)


def validate_policy(config: Any) -> dict[str, Any]:
    require(isinstance(config, dict), "config must be an object")
    policy = config.get("review")
    require(isinstance(policy, dict), "config.review is required")
    unknown = policy.keys() - REVIEW_KEYS
    require(not unknown, f"unknown keys in config.review: {', '.join(sorted(unknown))}")
    mapping = policy.get("severityMap")
    require(
        isinstance(mapping, dict)
        and all(text(k) and v in SEVERITIES for k, v in mapping.items()),
        "invalid severity map",
    )
    if "guidance" in policy:
        require(text(policy["guidance"]), "review.guidance must be a nonempty path")
    if "securityPass" in policy:
        security = policy["securityPass"]
        require(isinstance(security, dict), "review.securityPass must be an object")
        unknown = security.keys() - {"enabled", "paths"}
        require(
            not unknown,
            f"unknown keys in review.securityPass: {', '.join(sorted(unknown))}",
        )
        require(
            type(security.get("enabled")) is bool,
            "securityPass enabled must be boolean",
        )
        paths = security.get("paths")
        require(
            not security["enabled"]
            or (isinstance(paths, list) and paths and all(text(p) for p in paths)),
            "securityPass needs nonempty path patterns when enabled",
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

    A `sprint-review` record also names its `area` on the record and on both lens
    results. It uses the same repeat threshold as `ai-review`.

    Raises:
        ValueError: the record is invalid or incomplete.

    """
    require(isinstance(record, dict), "review record must be an object")
    require(record.get("stage") in STAGES, "invalid review stage")
    material_change = record.get("materialChange", False)
    require(type(material_change) is bool, "materialChange must be boolean")
    if record["stage"] == "sprint-review":
        require(record.get("area") in SPRINT_AREAS, "sprint review area required")
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
    validate_inspection(record)
    findings = []
    items: list[dict[str, Any]] = []
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
            record["stage"] != "sprint-review" or output.get("area") == record["area"],
            "sprint review area mismatch",
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
            validate_finding_fields(item)
            findings.append((item["id"], level))
            items.append(item)
        require(output["verdict"] != "REVISE" or current, "REVISE requires findings")
        blocked = blocked or output["verdict"] == "BLOCKED"
    require(
        len({key for key, _ in findings}) == len(findings), "finding IDs must be unique"
    )
    validate_security_pass(record, {item["id"]: item for item in items}, policy)
    preexisting = {
        item["id"] for item in items if unchanged_preexisting(item, record["stage"])
    }
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
    if material_change or any(
        level in REPEAT_SEVERITIES
        and key not in confirmed_refutations
        and not (key in preexisting and dispositions.get(key) == "deferred")
        for key, level in findings
    ):
        return "STOP" if number == authorized_through else "REVIEW_AGAIN"
    return "HUMAN_REVIEW"


def validate_finding_fields(item: dict[str, Any]) -> None:
    """Check the optional origin, class, security and history fields of one finding."""
    origin = item.get("origin", "introduced")
    require(
        origin in ORIGINS, "finding origin must be introduced, pre-existing or unknown"
    )
    if origin == "pre-existing":
        require(
            text(item.get("originEvidence")),
            "pre-existing finding needs baseline evidence",
        )
        require(
            type(item.get("affectedByChange")) is bool,
            "pre-existing finding must state affectedByChange",
        )
    for key in ("bugClass", "priorFinding"):
        require(key not in item or text(item[key]), f"{key} must be nonempty text")
    require(
        type(item.get("security", False)) is bool, "finding security must be boolean"
    )


def unchanged_preexisting(item: dict[str, Any], stage: str) -> bool:
    """Return whether a deferred finding can skip the repeat round.

    Only an `ai-review` defect that the base revision already had, that this
    change does not reach or worsen, and that is explicitly not a security
    defect qualifies.

    Returns:
        True when the finding is exempt from the repeat threshold once deferred.

    """
    return (
        stage == "ai-review"
        and item.get("origin") == "pre-existing"
        and item.get("affectedByChange") is False
        and item.get("security") is False
    )


def artifact_base(artifact: str) -> str | None:
    prefix, separator, _ = artifact.partition("/head:")
    return (
        prefix.removeprefix("base:")
        if separator and prefix.startswith("base:")
        else None
    )


def validate_inspection(record: dict[str, Any]) -> None:
    """Check the shape of a delta inspection. The history check is separate."""
    mode = record.get("inspection", "full")
    require(mode in INSPECTIONS, "inspection must be full or delta")
    if mode == "full":
        require(
            "previousArtifact" not in record or text(record["previousArtifact"]),
            "previousArtifact must be nonempty text",
        )
        return
    require(record["stage"] == "ai-review", "delta inspection is for ai-review only")
    require(record["round"] >= 2, "delta inspection needs a complete earlier round")
    previous = record.get("previousArtifact")
    require(
        text(previous) and previous != record["artifact"],
        "delta inspection needs the previous reviewed artifact",
    )
    base = artifact_base(record["artifact"])
    require(
        base is not None and base == artifact_base(previous),
        "delta inspection needs the same known base; a changed baseline "
        "requires a full inspection",
    )


def validate_history(
    record: dict[str, Any], history: Any, policy: dict[str, Any]
) -> None:
    """Check a round against the retained records of the earlier rounds.

    A record cannot prove its own history. The orchestrator supplies the
    records of rounds 1 to N-1 of the same stage, read from the board.

    """
    require(isinstance(history, list), "history must be an array of round records")
    require(
        all(isinstance(item, dict) for item in history),
        "history records must be objects",
    )
    # Each earlier round must be a complete, valid record in its own right.
    # The security pass decision belongs to its own round, so a project that
    # enables it later does not invalidate older records.
    earlier_policy = {**policy, "securityPass": {"enabled": False}}
    for item in history:
        review_action(item, earlier_policy, max(MAX_REVIEW_ROUNDS, record["round"]))
    rounds = sorted(item["round"] for item in history)
    require(
        rounds == list(range(1, record["round"])),
        "history must hold each earlier round of this stage once",
    )
    chain = sorted(history, key=lambda item: item["round"]) + [record]
    require(
        all(item.get("stage") == record["stage"] for item in chain),
        "history stage mismatch",
    )
    require(
        chain[0].get("inspection", "full") == "full",
        "round one must be a full inspection",
    )
    known = set()
    for earlier, later in zip(chain, chain[1:]):
        for output in earlier.get("results", []):
            for item in output.get("findings", []):
                known.add(f"R{earlier['round']}:{item.get('id')}")
        if later.get("inspection", "full") == "delta":
            require(
                later.get("previousArtifact") == earlier.get("artifact"),
                "delta inspection does not start at the previous reviewed artifact",
            )
            require(
                earlier.get("materialChange", False) is False,
                "a material change requires a full inspection",
            )
    for output in record.get("results", []):
        for item in output.get("findings", []):
            prior = item.get("priorFinding")
            require(prior is None or prior in known, f"unknown prior finding: {prior}")


def validate_security_pass(
    record: dict[str, Any], findings: dict[str, Any], policy: dict[str, Any]
) -> None:
    """Check the separate security inspection when the project enables it."""
    enabled = policy.get("securityPass", {}).get("enabled", False)
    security = record.get("securityPass")
    if security is None:
        require(
            not enabled or record["stage"] != "ai-review",
            "securityPass decision required for ai-review",
        )
        return
    require(
        isinstance(security, dict) and type(security.get("required")) is bool,
        "securityPass must state required",
    )
    if not security["required"]:
        require(text(security.get("reason")), "securityPass skip needs a reason")
        return
    ids = security.get("findingIds")
    require(
        security.get("complete") is True
        and security.get("artifact") == record["artifact"]
        and text(security.get("reviewer"))
        and text(security.get("summary"))
        and isinstance(ids, list)
        and all(key in findings for key in ids),
        "security pass must be complete, revision-bound and merged into findings",
    )
    require(
        all(findings[key].get("security") is True for key in ids),
        "security pass findings must be marked security: true",
    )


def sprint_round_action(
    records: Any,
    policy: dict[str, Any],
    artifact: str,
    reviewers: dict[str, str],
    authors: list[str],
    authorized_through: int = MAX_REVIEW_ROUNDS,
) -> str:
    """Validate all four independent area pairs of one sprint round.

    Returns:
        The most restrictive decision for the complete shared round, in the order
        STOP, FIX, REVIEW_AGAIN, HUMAN_REVIEW.

    """
    require(
        isinstance(records, list) and len(records) == len(SPRINT_AREAS),
        "four sprint review records required",
    )
    require(set(reviewers) == SPRINT_AREAS, "four expected sprint reviewers required")
    require(
        all(isinstance(record, dict) for record in records),
        "sprint records must be objects",
    )
    require(
        {record.get("area") for record in records} == SPRINT_AREAS,
        "missing or duplicate sprint area",
    )
    require(
        len({record.get("round") for record in records}) == 1, "sprint round mismatch"
    )
    decisions = []
    for record in records:
        require(record.get("stage") == "sprint-review", "sprint review stage required")
        area = record["area"]
        validate_review_target(record, artifact, reviewers[area], authors, area)
        decisions.append(review_action(record, policy, authorized_through))
    return next(
        decision
        for decision in ("STOP", "FIX", "REVIEW_AGAIN", "HUMAN_REVIEW")
        if decision in decisions
    )


def validate_review_target(
    record: dict[str, Any],
    artifact: str,
    reviewer: str,
    authors: list[str],
    area: str | None = None,
) -> None:
    if record.get("stage") == "sprint-review":
        require(
            area in SPRINT_AREAS and record.get("area") == area,
            "unexpected sprint review area",
        )
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
    author_ids = {model_identity(author) for author in authors}
    require(
        model_identity(reviewer) not in author_ids,
        "reviewer must be independent of all authors",
    )
    security = record.get("securityPass")
    if isinstance(security, dict) and security.get("required") is True:
        require(
            text(security.get("reviewer"))
            and model_identity(security["reviewer"]) not in author_ids,
            "security reviewer must be independent of all authors",
        )


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
    review.add_argument(
        "--area",
        choices=sorted(SPRINT_AREAS),
        help="required for a sprint-review record; the area the record must name",
    )
    review.add_argument(
        "--history",
        help="JSON array of this stage's earlier round records; required for delta",
    )
    sprint_round = sub.add_parser("sprint-round")
    sprint_round.add_argument("path", help="JSON array of the four area records")
    sprint_round.add_argument("--config", required=True)
    sprint_round.add_argument("--artifact", required=True)
    sprint_round.add_argument(
        "--reviewer",
        required=True,
        action="append",
        help="AREA=actual-model-identity, once per area",
    )
    sprint_round.add_argument("--author", required=True, action="append")
    for command in (review, sprint_round):
        command.add_argument(
            "--authorized-through", type=int, default=MAX_REVIEW_ROUNDS
        )
        command.add_argument(
            "--authorization",
            help="board comment holding the human's explicit authorization beyond round four",
        )
    args = parser.parse_args()
    try:
        data = load(args.path)
        if args.action == "review":
            validate_review_target(
                data, args.artifact, args.reviewer, args.author, args.area
            )
        require(
            args.authorized_through == MAX_REVIEW_ROUNDS or text(args.authorization),
            "human authorization reference required for extra rounds",
        )
        policy = validate_policy(load(args.config))
        if args.action == "review":
            require(
                data.get("inspection", "full") != "delta" or args.history,
                "delta inspection needs --history",
            )
            action = review_action(data, policy, args.authorized_through)
            if args.history:
                validate_history(data, load(args.history), policy)
        else:
            pairs = [value.split("=", 1) for value in args.reviewer]
            require(
                len(pairs) == len(SPRINT_AREAS)
                and all(len(pair) == 2 for pair in pairs),
                "four AREA=reviewer bindings required",
            )
            action = sprint_round_action(
                data,
                policy,
                args.artifact,
                dict(pairs),
                args.author,
                args.authorized_through,
            )
    except (ValueError, KeyError, TypeError, AttributeError, OSError) as exc:
        print(f"FAIL: {exc}")
        return 1
    print(action)
    return 0 if action == "HUMAN_REVIEW" else 1


if __name__ == "__main__":
    raise SystemExit(main())
