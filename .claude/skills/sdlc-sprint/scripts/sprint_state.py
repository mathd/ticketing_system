"""Check captured sprint facts from the sdlc board without contacting the board.

Ported from the upstream sdlc-sprint state checker and adapted to the board's
sprint design: identity is vault + sprint key, a member is accepted when it is
Done, and the human decision is a close approval that names the scope digest.
The script reads one normalized snapshot file. It never calls the board, the
vault or git. It cannot prove that a record in the snapshot is authentic.
Python 3.11 or newer.

Output and exit codes:
    progress   a JSON object on stdout, exit 0
    START      the start request is eligible, exit 0 (nothing was started)
    CLOSE      the close request is eligible, exit 0 (nothing was closed)
    STOP       the request is not eligible, exit 1; each reason goes to stderr
    FAIL: ...  the snapshot is invalid, exit 1
    usage      exit 2
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

STATES = {"planned", "active", "closed"}
SPRINT_KEY = re.compile(r"S-[0-9]+")
SHA256 = re.compile(r"[0-9a-f]{64}")
COMMIT = re.compile(r"[0-9a-f]{40}|[0-9a-f]{64}")


def require(condition: Any, message: str) -> None:
    if not condition:
        raise ValueError(message)


def present(value: Any) -> bool:
    return isinstance(value, str) and bool(value.strip())


def hex_text(value: Any, pattern: re.Pattern[str] = SHA256) -> bool:
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def key_list(value: Any) -> bool:
    return (
        isinstance(value, list)
        and all(present(key) for key in value)
        and len(set(value)) == len(value)
    )


def _iso(value: str) -> str:
    """The board's normalization before parsing (sprints.parse_instant)."""
    return value.strip().replace("Z", "+00:00")


def timestamp(value: Any) -> datetime:
    require(present(value), "timestamp required")
    parsed = datetime.fromisoformat(_iso(value))
    require(parsed.tzinfo is not None, "timestamps require an offset")
    return parsed


def utc(value: Any) -> str:
    """Return an instant in UTC with second precision, as the board's digest does.

    The last line is the design's expression. The checks before it refuse a
    value without an offset, because ``astimezone`` would silently use the
    local time zone of this machine.
    """
    timestamp(value)
    return (
        datetime.fromisoformat(_iso(value))
        .astimezone(timezone.utc)
        .isoformat(timespec="seconds")
    )


def scope_digest(
    vault: str,
    key: str,
    name: str | None,
    goal: str | None,
    start: str | None,
    end: str | None,
    member_keys: list[str],
) -> str:
    """Compute the board's scope digest from its canonical form (design section 1).

    Returns:
        The SHA-256 hex digest of the canonical JSON of the sprint identity,
        text, planned dates (in UTC) and sorted member keys.

    """
    canonical = {
        "vault": vault,
        "key": key,
        "name": name or "",
        "goal": goal or "",
        "startDate": utc(start) if start else None,
        "endDate": utc(end) if end else None,
        "members": sorted(member_keys),
    }
    return hashlib.sha256(
        json.dumps(
            canonical, sort_keys=True, separators=(",", ":"), ensure_ascii=False
        ).encode("utf-8")
    ).hexdigest()


def validate_brief(brief: Any) -> None:
    require(isinstance(brief, dict), "brief must be an object")
    require(
        hex_text(brief.get("digest")), "brief digest must be a SHA-256 hex string"
    )
    require(present(brief.get("sprint")), "brief must name its sprint")
    require(type(brief.get("release")) is bool, "brief release must be true or false")
    closure = brief.get("closureTask")
    require(closure is None or present(closure), "closureTask must be a key or null")
    if "continuation" in brief:
        require(isinstance(brief["continuation"], str) and brief["continuation"] in {"autonomous", "manual"}, "continuation must be autonomous or manual")
    if "review" in brief:
        require(isinstance(brief["review"], str) and brief["review"] in {"full", "none"}, "review must be full or none")
    require(key_list(brief.get("members")), "brief members must be unique keys")
    require(present(brief.get("scopeDigest")), "brief must carry its scope digest")
    require(present(brief.get("approvedBy")), "brief approver required")


def progress(
    record: dict[str, Any], vault: str, sprint_key: str, done_status: str
) -> dict[str, Any]:
    """Validate a complete normalized snapshot.

    Returns:
        Counts, blockers, scope changes, history-gap count and the scope digest
        that this script recomputed and matched to the board's digest.

    """
    require(present(vault), "vault name required")
    require(
        isinstance(sprint_key, str) and SPRINT_KEY.fullmatch(sprint_key),
        "sprint key must look like S-3",
    )
    require(present(done_status), "done status required")
    require(record["vault"] == vault, "wrong vault")
    sprint = record["sprint"]
    require(sprint["key"] == sprint_key, "wrong sprint")
    require(sprint["state"] in STATES, "unknown sprint state")
    require(present(sprint.get("name")), "name required")
    for field in ("goal", "startDate", "endDate"):
        require(
            sprint.get(field) is None or isinstance(sprint[field], str),
            f"{field} must be text or null",
        )
    for field in ("startScope", "endScope"):
        require(
            sprint.get(field) is None or key_list(sprint[field]),
            f"{field} must be a list of member keys",
        )
    require(record.get("membershipComplete") is True, "sprint membership is incomplete")
    unreadable = record.get("unreadable")
    require(isinstance(unreadable, list), "unreadable must be a list")
    require(not unreadable, "member notes cannot be read: " + ", ".join(map(str, unreadable)))
    gaps = record.get("historyGaps")
    require(isinstance(gaps, list), "historyGaps must be a list")
    timestamp(record.get("observedAt"))
    members = record["members"]
    require(isinstance(members, list), "members must be a list")
    keys: list[str] = []
    for item in members:
        require(isinstance(item, dict), "member must be an object")
        require(
            present(item.get("key")) and item["key"] not in keys,
            "duplicate or missing member key",
        )
        require(present(item.get("status")), "member status required")
        require(type(item.get("blocked")) is bool, "blocker state must be explicit")
        keys.append(item["key"])
    computed = scope_digest(
        vault,
        sprint_key,
        sprint.get("name"),
        sprint.get("goal"),
        sprint.get("startDate"),
        sprint.get("endDate"),
        keys,
    )
    require(
        hex_text(record.get("scopeDigest")),
        "the board's scope digest must be a SHA-256 hex string",
    )
    require(record["scopeDigest"] == computed, "scope digest differs from the board's")
    brief = record.get("brief")
    if brief is not None:
        validate_brief(brief)
    accepted = [item for item in members if item["status"] == done_status]
    closure = brief.get("closureTask") if brief else None
    functional = [item for item in members if item["key"] != closure]
    start_scope, end_scope = sprint.get("startScope"), sprint.get("endScope")
    return {
        "vault": vault,
        "sprint": sprint_key,
        "state": sprint["state"],
        "accepted": len(accepted),
        "total": len(members),
        "remaining": len(members) - len(accepted),
        "blocked": sorted(
            item["key"]
            for item in members
            if item["blocked"] and item["status"] != done_status
        ),
        "added": sorted(set(keys) - set(brief["members"])) if brief else None,
        "removed": sorted(set(brief["members"]) - set(keys)) if brief else None,
        "sinceStart": (
            {
                "added": sorted(set(keys) - set(start_scope)),
                "removed": sorted(set(start_scope) - set(keys)),
            }
            if start_scope is not None and sprint["state"] != "planned"
            else None
        ),
        "functional": {
            "accepted": sum(item["status"] == done_status for item in functional),
            "total": len(functional),
        },
        "closure": next(
            (item["status"] for item in members if item["key"] == closure), None
        ),
        "historyGaps": len(gaps),
        "endScopeMismatch": (
            sorted(end_scope) != sorted(keys)
            if end_scope is not None and sprint["state"] == "closed"
            else None
        ),
        "scopeDigest": computed,
    }


def brief_reasons(
    record: dict[str, Any], counts: dict[str, Any], sprint_key: str
) -> list[str]:
    brief = record.get("brief")
    if brief is None:
        return ["no approved brief"]
    reasons = []
    if brief["sprint"] != sprint_key:
        reasons.append("brief names another sprint")
    if brief["scopeDigest"] != counts["scopeDigest"]:
        reasons.append("brief approval names another scope digest")
    if counts["added"] or counts["removed"]:
        reasons.append(
            "members differ from the approved brief (added: "
            + (", ".join(counts["added"]) or "none")
            + "; removed: "
            + (", ".join(counts["removed"]) or "none")
            + ")"
        )
    if "continuation" not in brief:
        reasons.append("brief needs an explicit continuation policy approved by the owner")
    if "review" not in brief:
        reasons.append("brief needs an explicit review policy approved by the owner")
    if brief.get("review") == "none":
        if not present(brief.get("reviewReason")):
            reasons.append("skipping review needs an owner-approved reason")
        if brief["release"]:
            reasons.append("tag publication requires full review")
        if brief.get("closureTask") is not None:
            reasons.append("a no-review brief cannot name a closure task")
    if brief.get("review") == "full" and brief.get("closureTask") not in {
        item["key"] for item in record["members"]
    }:
        reasons.append("closure task is not a member")
    return reasons


def start_reasons(
    record: dict[str, Any], counts: dict[str, Any], sprint_key: str
) -> list[str]:
    sprint = record["sprint"]
    reasons = []
    if counts["state"] != "planned":
        reasons.append("sprint is not planned")
    reasons.extend(brief_reasons(record, counts, sprint_key))
    if not record["members"]:
        reasons.append("no member")
    if not present(sprint.get("goal")):
        reasons.append("goal required")
    brief = record.get("brief")
    if brief and brief.get("continuation") == "autonomous" and any(
        member["status"] in {"Backlog", "BLOCKED"} for member in record["members"]
    ):
        priority = record.get("priorityApproval")
        if not (
            isinstance(priority, dict)
            and hex_text(priority.get("digest"))
            and priority.get("sprint") == sprint_key
            and priority.get("briefDigest") == brief["digest"]
            and priority.get("scopeDigest") == counts["scopeDigest"]
            and key_list(priority.get("members"))
            and sorted(priority["members"]) == sorted(brief["members"])
            and present(priority.get("approvedBy"))
            and present(priority.get("approvedAt"))
        ):
            reasons.append("autonomous continuation needs exact owner priority approval for unready members")
        elif timestamp(priority["approvedAt"]) > timestamp(record["observedAt"]):
            reasons.append("observation precedes owner priority approval")
    start, end = sprint.get("startDate"), sprint.get("endDate")
    require(start and end, "startDate and endDate are required to start")
    require(timestamp(end) > timestamp(start), "endDate must follow startDate")
    request = record.get("startRequest")
    if request is None:
        reasons.append("no start request")
        return reasons
    require(isinstance(request, dict), "start request must be an object")
    require(
        hex_text(request.get("digest")),
        "start request digest must be a SHA-256 hex string",
    )
    require(present(request.get("requestedBy")), "start request requester required")
    if request.get("sprint") != sprint_key:
        reasons.append("start request names another sprint")
    brief = record.get("brief")
    if brief is not None and request.get("briefDigest") != brief["digest"]:
        reasons.append("start request names another brief")
    if request.get("scopeDigest") != counts["scopeDigest"]:
        reasons.append("start request names another scope digest")
    return reasons


def release_reasons(
    record: dict[str, Any], approval: dict[str, Any] | None
) -> list[str]:
    release = record.get("release")
    if not isinstance(release, dict):
        return ["release evidence missing"]
    publish_tag = record["brief"]["release"]
    for field, pattern in (("candidate", COMMIT), ("reportDigest", SHA256)):
        require(hex_text(release.get(field), pattern), f"invalid {field}")
    if publish_tag:
        require(hex_text(release.get("tagObject"), COMMIT), "invalid tagObject")
        require(present(release.get("tag")), "release tag required")
    reasons = []
    if release.get("ready") is not True:
        reasons.append("release is not ready")
    review = release.get("review")
    areas = {"implementation", "verification", "documentation", "retrospective"}
    if not isinstance(review, dict):
        reasons.append("full sprint review evidence missing")
    elif not (
        review.get("decision") == "HUMAN_REVIEW"
        and hex_text(review.get("packetDigest"))
        and type(review.get("round")) is int and review["round"] > 0
        and isinstance(review.get("areas"), dict)
        and set(review["areas"]) == areas
        and all(
            isinstance(area, dict)
            and area.get("decision") == "HUMAN_REVIEW"
            and type(area.get("round")) is int
            and area["round"] == review["round"]
            and area.get("packetDigest") == review["packetDigest"]
            and hex_text(area.get("recordDigest"))
            for area in review["areas"].values()
        )
    ):
        reasons.append("four completed review areas on one packet and round required")
    if isinstance(review, dict):
        reviewed = review.get("candidate")
        correction = review.get("correction")
        if not hex_text(reviewed, COMMIT):
            reasons.append("reviewed packet candidate required")
        elif reviewed != release["candidate"]:
            if not (
                isinstance(correction, dict)
                and correction.get("fromCandidate") == reviewed
                and correction.get("toCandidate") == release["candidate"]
                and hex_text(correction.get("diffDigest"))
                and hex_text(correction.get("recordDigest"))
                and correction.get("verificationPassed") is True
                and correction.get("materialChange") is False
                and correction.get("repeatThresholdFinding") is False
                and correction.get("gatePassed") is True
                and correction.get("gateCandidate") == release["candidate"]
            ):
                reasons.append("candidate differs from reviewed packet without verified bounded corrections")
    report = release.get("report")
    if not (
        isinstance(report, dict)
        and report.get("verified") is True
        and report.get("pointerVerified") is True
        and hex_text(report.get("commit"), COMMIT)
        and present(report.get("path"))
        and present(report.get("sourcePath"))
        and hex_text(report.get("sourceDigest"))
        and report.get("htmlDigest") == release["reportDigest"]
        and report.get("candidate") == release["candidate"]
        and isinstance(review, dict)
        and report.get("packetDigest") == review.get("packetDigest")
    ):
        reasons.append("verified committed report and current board pointer required")
    if approval is not None:
        for field in ("candidate", "reportDigest", "tag"):
            if approval.get(field) != release[field]:
                reasons.append(f"close approval names another {field}")
    if not publish_tag:
        if any(release.get(field) is not None for field in ("tag", "tagObject", "publication")):
            reasons.append("no-tag brief cannot claim tag publication")
        return reasons
    published = release.get("publication")
    if not (isinstance(published, dict) and published.get("verified") is True):
        reasons.append("tag publication is not verified")
    else:
        for field in ("candidate", "tag", "tagObject"):
            if published.get(field) != release[field]:
                reasons.append(f"published tag differs from the release: {field}")
    return reasons


def close_reasons(
    record: dict[str, Any], counts: dict[str, Any], sprint_key: str, done_status: str
) -> list[str]:
    reasons = []
    if counts["state"] != "active":
        reasons.append("sprint is not active")
    unfinished = sorted(
        item["key"] for item in record["members"] if item["status"] != done_status
    )
    if unfinished:
        reasons.append("members not Done: " + ", ".join(unfinished))
    reasons.extend(brief_reasons(record, counts, sprint_key))
    approval = record.get("closeApproval")
    if approval is None:
        reasons.append("no close approval")
    else:
        require(isinstance(approval, dict), "close approval must be an object")
        require(
            hex_text(approval.get("digest")),
            "close approval digest must be a SHA-256 hex string",
        )
        require(present(approval.get("approvedBy")), "close approval approver required")
        approved_at = timestamp(approval.get("approvedAt"))
        if approval.get("sprint") != sprint_key:
            reasons.append("close approval names another sprint")
        brief = record.get("brief")
        if brief is not None and approval.get("briefDigest") != brief["digest"]:
            reasons.append("close approval names another brief")
        if approval.get("scopeDigest") != counts["scopeDigest"]:
            reasons.append("close approval names another scope digest")
        if approval.get("closeSprint") is not True:
            reasons.append("close approval does not permit closing the sprint")
        if timestamp(record["observedAt"]) < approved_at:
            reasons.append("observation precedes the close approval")
    brief = record.get("brief")
    if brief is not None and brief.get("review") == "full":
        reasons.extend(release_reasons(record, approval))
    if brief is not None and brief.get("review") == "none" and isinstance(approval, dict):
        if any(approval.get(field) is not None for field in ("candidate", "reportDigest", "tag")):
            reasons.append("no-review approval cannot name candidate, report or tag")
    return reasons


def decide(
    record: dict[str, Any],
    intent: str,
    vault: str,
    sprint_key: str,
    done_status: str,
) -> tuple[str, list[str]]:
    """Evaluate a requested lifecycle operation. No external action occurs.

    Every rule is evaluated, so the reasons list names all of them.

    Returns:
        START or CLOSE with no reasons when eligible, otherwise STOP with reasons.

    """
    require(intent in {"start", "close"}, "unknown intent")
    counts = progress(record, vault, sprint_key, done_status)
    reasons = (
        start_reasons(record, counts, sprint_key)
        if intent == "start"
        else close_reasons(record, counts, sprint_key, done_status)
    )
    if reasons:
        return "STOP", reasons
    return intent.upper(), []


def action(
    record: dict[str, Any],
    intent: str,
    vault: str,
    sprint_key: str,
    done_status: str,
) -> str:
    """Return START, CLOSE or STOP for a requested lifecycle operation.

    Returns:
        The decision only. Use ``decide`` to read the reasons.

    """
    return decide(record, intent, vault, sprint_key, done_status)[0]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("intent", choices=("progress", "start", "close"))
    parser.add_argument("snapshot", type=Path)
    parser.add_argument("--vault", required=True)
    parser.add_argument("--sprint", required=True)
    parser.add_argument("--done-status", required=True)
    args = parser.parse_args()
    try:
        record = json.loads(args.snapshot.read_text(encoding="utf-8"))
        if args.intent == "progress":
            print(json.dumps(progress(record, args.vault, args.sprint, args.done_status)))
            return 0
        decision, reasons = decide(
            record, args.intent, args.vault, args.sprint, args.done_status
        )
    except KeyError as exc:
        print(f"FAIL: missing field {exc}")
        return 1
    except (ValueError, TypeError, AttributeError, OSError) as exc:
        print(f"FAIL: {exc}")
        return 1
    print(decision)
    for reason in reasons:
        print(f"STOP: {reason}", file=sys.stderr)
    return 1 if decision == "STOP" else 0


if __name__ == "__main__":
    raise SystemExit(main())
