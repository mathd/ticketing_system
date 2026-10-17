"""Validate captured sprint evidence and render an offline, localized report."""

from __future__ import annotations

import argparse
import hashlib
import html
import json
import math
import re
from collections import defaultdict
from datetime import datetime
from decimal import Decimal
from pathlib import Path
from string import Template
from typing import Any
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
AREAS = ("implementation", "verification", "documentation", "retrospective")
DECISIONS = ("HUMAN_REVIEW", "FIX", "REVIEW_AGAIN", "STOP")
SPRINT_KEY = re.compile(r"S-[0-9]+")

# The template and both languages share one maintained copy source.
COPY = json.loads((ROOT / "assets/report.labels.json").read_text(encoding="utf-8"))
LABELS = {language: copy["labels"] for language, copy in COPY.items()}
ENUMS = {language: copy["enums"] for language, copy in COPY.items()}


def require(condition: Any, message: str) -> None:
    if not condition:
        raise ValueError(message)


def text(value: Any) -> bool:
    return isinstance(value, str) and bool(value.strip())


def fields(value: Any, required: str, optional: str = "") -> None:
    require(
        isinstance(value, dict)
        and set(required.split())
        <= set(value)
        <= set((required + " " + optional).split()),
        f"expected fields: {required}; optional: {optional}",
    )


def strings(value: dict[str, Any], names: str, *, nullable: bool = False) -> None:
    for name in names.split():
        require(
            (nullable and value[name] is None) or text(value[name]),
            f"text required: {name}",
        )


def choice(value: Any, values: str | tuple[str, ...]) -> None:
    require(
        isinstance(value, str)
        and value in (values.split() if isinstance(values, str) else values),
        f"invalid enum: {value}",
    )


def timestamp(value: Any) -> datetime:
    require(text(value), "timestamp required")
    result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    require(result.tzinfo is not None, "timestamp requires timezone")
    return result


def digest(value: Any, *, commit: bool = False) -> None:
    require(
        isinstance(value, str)
        and re.fullmatch(
            r"[0-9a-f]{40}|[0-9a-f]{64}" if commit else r"[0-9a-f]{64}", value
        ),
        "invalid digest",
    )


def url(value: Any) -> None:
    require(
        text(value) and not any(char.isspace() or ord(char) < 32 for char in value),
        "invalid evidence URL",
    )
    parsed = urlsplit(value)
    require(
        parsed.scheme in {"http", "https"}
        and parsed.hostname
        and not parsed.username
        and not parsed.password,
        "only absolute HTTP(S) evidence URLs are allowed",
    )


def board_ref(value: Any) -> None:
    """Check a reference to a board comment: the note key and the body digest.

    A board comment has no ID. The key names the sprint note or the ticket, and
    the digest is the SHA-256 of the comment body as the board returns it.
    """
    fields(value, "key digest")
    strings(value, "key")
    digest(value["digest"])


def ref_text(ref: dict[str, str]) -> str:
    return ref["key"] + " sha256:" + ref["digest"]


def evidence(items: Any) -> None:
    require(isinstance(items, list), "evidence array required")
    for item in items:
        fields(item, "label url", "ref")
        strings(item, "label")
        if item["url"] is not None:
            url(item["url"])
        if item.get("ref") is not None:
            board_ref(item["ref"])
        require(
            item["url"] is None or item.get("ref") is None,
            "an evidence item holds a URL or a board reference, not both",
        )


def linked_evidence(items: list[dict[str, Any]]) -> bool:
    """Check that captured evidence includes a retrievable receipt.

    Returns:
        True if at least one validated item has an HTTP(S) URL or a board reference.

    """
    return any(
        item["url"] is not None or item.get("ref") is not None for item in items
    )


def unique(items: Any, key: str) -> None:
    require(
        isinstance(items, list)
        and all(isinstance(item, dict) and text(item.get(key)) for item in items),
        f"array with {key} required",
    )
    require(len({item[key] for item in items}) == len(items), f"duplicate {key}")


def validate_identity(data: dict[str, Any]) -> None:
    fields(
        data,
        "schemaVersion report project sprint release outcome scope areas checks measurements risks followups retrospective decision",
    )
    require(
        type(data["schemaVersion"]) is int and data["schemaVersion"] == 1,
        "unsupported schemaVersion",
    )
    report = data["report"]
    fields(report, "id version language generatedAt preparedBy", "fictional")
    strings(report, "id preparedBy")
    choice(report["language"], ("fr", "en"))
    require(
        type(report["version"]) is int
        and report["version"] > 0
        and type(report.get("fictional", False)) is bool,
        "invalid report identity",
    )
    timestamp(report["generatedAt"])
    fields(data["project"], "key name")
    strings(data["project"], "key name")
    sprint = data["sprint"]
    fields(
        sprint,
        "vault key name goal state startDate endDate scopeDigest closureTask",
    )
    strings(sprint, "vault name goal closureTask")
    require(
        isinstance(sprint["key"], str) and SPRINT_KEY.fullmatch(sprint["key"]),
        "sprint key must look like S-3",
    )
    choice(sprint["state"], "planned active closed")
    require(
        timestamp(sprint["endDate"]) > timestamp(sprint["startDate"]),
        "invalid sprint dates",
    )
    digest(sprint["scopeDigest"])
    release = data["release"]
    fields(release, "repository base candidate tag")
    url(release["repository"])
    fields(release["base"], "ref sha")
    strings(release["base"], "ref", nullable=True)
    fields(release["candidate"], "sha url")
    url(release["candidate"]["url"])
    for key in ("base", "candidate"):
        digest(release[key]["sha"], commit=True)
    strings(release, "tag")
    fields(data["outcome"], "readiness headline summary")
    strings(data["outcome"], "headline summary")
    choice(data["outcome"]["readiness"], "ready incomplete blocked")


def validate_reviews(data: dict[str, Any]) -> None:
    fields(data["areas"], " ".join(AREAS))
    for area in data["areas"].values():
        fields(area, "status decision round reviewers findings summary evidence")
        choice(area["status"], "passed failed incomplete")
        strings(area, "summary")
        evidence(area["evidence"])
        if area["decision"] is not None:
            choice(area["decision"], DECISIONS)
        expected = (
            "passed"
            if area["decision"] == "HUMAN_REVIEW"
            else "failed"
            if area["decision"] == "STOP"
            else "incomplete"
        )
        require(area["status"] == expected, "area status contradicts review decision")
        if area["round"] is not None:
            require(
                type(area["round"]) is int and area["round"] > 0,
                "positive review round required",
            )
        require(
            isinstance(area["reviewers"], list)
            and all(
                text(name) and name != data["report"]["preparedBy"]
                for name in area["reviewers"]
            ),
            "independent review identities required",
        )
        if area["findings"] is not None:
            fields(area["findings"], "raised resolved deferred")
            counts = area["findings"]
            require(
                all(type(value) is int and value >= 0 for value in counts.values())
                and counts["resolved"] + counts["deferred"] <= counts["raised"],
                "invalid finding counts",
            )
        if area["decision"] is not None:
            require(
                area["round"] is not None
                and area["reviewers"]
                and area["findings"] is not None,
                "completed decision requires captured review metadata",
            )
        if area["status"] == "passed":
            require(
                linked_evidence(area["evidence"])
                and counts["raised"] == counts["resolved"] + counts["deferred"],
                "passed area requires complete review evidence",
            )
    require(
        len(
            {
                area["round"]
                for area in data["areas"].values()
                if area["round"] is not None
            }
        )
        <= 1,
        "areas must share one round",
    )


def validate_scope(data: dict[str, Any]) -> None:
    fields(data["scope"], "summary items")
    strings(data["scope"], "summary")
    unique(data["scope"]["items"], "key")
    for item in data["scope"]["items"]:
        fields(item, "key title origin outcome destination note")
        strings(item, "key title")
        strings(item, "note destination", nullable=True)
        choice(item["origin"], "planned added")
        choice(item["outcome"], "delivered deferred removed")
        require(
            item["key"] != data["sprint"]["closureTask"],
            "scope counts functional stories only",
        )
        if item["outcome"] != "delivered":
            require(text(item["note"]), "excluded work requires code disposition")
        if item["outcome"] == "deferred":
            require(
                item["destination"] is not None, "deferred work requires destination"
            )


def validate_checks(data: dict[str, Any]) -> None:
    unique(data["checks"], "id")
    require(
        any(check.get("required") is True for check in data["checks"]),
        "at least one required release check is needed",
    )
    for check in data["checks"]:
        fields(check, "id name kind required status command detail evidence cost")
        strings(check, "id name detail")
        strings(check, "command", nullable=True)
        choice(check["kind"], "gate unit integration e2e llm_eval manual other")
        choice(check["status"], "passed failed not_run not_applicable")
        require(type(check["required"]) is bool, "required must be boolean")
        evidence(check["evidence"])
        if check["status"] in {"passed", "failed"}:
            require(
                linked_evidence(check["evidence"]),
                "executed check requires linked run evidence",
            )
        if check["cost"] is not None:
            cost = check["cost"]
            fields(cost, "amount currency", "budget calls model")
            require(
                all(
                    type(cost[key]) in {int, float}
                    and math.isfinite(cost[key])
                    and cost[key] >= 0
                    for key in ("amount", "budget")
                    if key in cost
                ),
                "invalid cost or budget",
            )
            require(
                isinstance(cost["currency"], str)
                and re.fullmatch(r"[A-Z]{3}", cost["currency"]),
                "three-letter currency required",
            )
            if "calls" in cost:
                require(
                    type(cost["calls"]) is int and cost["calls"] >= 0,
                    "nonnegative call count required",
                )
            if "model" in cost:
                strings(cost, "model")
        if check["kind"] == "llm_eval" and check["status"] in {"passed", "failed"}:
            require(
                isinstance(check["cost"], dict) and text(check["cost"].get("model")),
                "real LLM execution requires cost and model",
            )
    require(isinstance(data["measurements"], list), "measurements array required")
    for metric in data["measurements"]:
        fields(metric, "metric scope baseline candidate tool observation")
        strings(metric, "metric scope observation")
        strings(metric, "baseline candidate tool", nullable=True)


def validate_learning(data: dict[str, Any]) -> None:
    unique(data["followups"], "key")
    followups = {item["key"] for item in data["followups"]}
    for item in data["followups"]:
        fields(item, "key title reason")
        strings(item, "key title reason")
    unique(data["risks"], "id")
    for risk in data["risks"]:
        fields(
            risk, "id severity area title status blocking resolution followup evidence"
        )
        strings(risk, "id title")
        strings(risk, "resolution followup", nullable=True)
        choice(risk["severity"], "Critical High Medium Low Nit")
        choice(risk["area"], AREAS)
        choice(risk["status"], "open resolved deferred")
        require(type(risk["blocking"]) is bool, "blocking must be boolean")
        evidence(risk["evidence"])
        if risk["status"] == "resolved":
            require(
                text(risk["resolution"]) and linked_evidence(risk["evidence"]),
                "resolved finding requires evidence",
            )
        if risk["status"] == "deferred":
            require(
                not risk["blocking"] and risk["followup"] in followups,
                "deferred finding requires a nonblocking listed follow-up",
            )
    retro = data["retrospective"]
    fields(retro, "summary candidates")
    strings(retro, "summary")
    unique(retro["candidates"], "id")
    for item in retro["candidates"]:
        fields(
            item,
            "id type observation proposal recommendation humanChoice destination evidence",
        )
        strings(item, "id observation proposal")
        strings(item, "destination", nullable=True)
        choice(item["type"], "process skill adr")
        choice(item["recommendation"], "adopt reject trial no_action")
        choice(item["humanChoice"], "pending adopt reject trial no_action")
        evidence(item["evidence"])
        if item["humanChoice"] in {"adopt", "trial"}:
            require(
                item["destination"] is not None and linked_evidence(item["evidence"]),
                "adopted improvement requires human decision evidence and destination",
            )


def blockers(data: dict[str, Any]) -> list[dict[str, Any]]:
    """Return unresolved release blockers, independent of the readiness label.

    Returns:
        Open or deferred explicit blockers and all unresolved Critical/High risks.

    """
    return [
        risk
        for risk in data["risks"]
        if risk["status"] != "resolved"
        and (risk["blocking"] or risk["severity"] in {"Critical", "High"})
    ]


def validate_decision(data: dict[str, Any]) -> None:
    decision = data["decision"]
    fields(decision, "approval tagPublication")
    approval = decision["approval"]
    fields(
        approval,
        "status approver decidedAt ref sprint scopeDigest candidate reportDigest reportUrl tag note",
    )
    choice(approval["status"], "not_requested pending approved rejected")
    strings(
        approval,
        "approver sprint scopeDigest candidate reportDigest tag note",
        nullable=True,
    )
    if approval["ref"] is not None:
        board_ref(approval["ref"])
    if approval["reportUrl"] is not None:
        url(approval["reportUrl"])
    if approval["status"] in {"approved", "rejected"}:
        require(
            all(text(approval[key]) for key in ("approver", "decidedAt"))
            and approval["ref"] is not None
            and approval["reportUrl"],
            "human decision requires a board reference and exact report evidence",
        )
        timestamp(approval["decidedAt"])
        digest(approval["reportDigest"])
        digest(approval["scopeDigest"])
        require(
            approval["sprint"] == data["sprint"]["key"]
            and approval["scopeDigest"] == data["sprint"]["scopeDigest"]
            and approval["candidate"] == data["release"]["candidate"]["sha"]
            and approval["tag"] == data["release"]["tag"],
            "human decision targets must match sprint, scope digest, candidate and tag",
        )
    else:
        require(
            all(
                approval[key] is None
                for key in (
                    "approver",
                    "decidedAt",
                    "sprint",
                    "scopeDigest",
                    "candidate",
                    "reportDigest",
                    "reportUrl",
                    "tag",
                )
            ),
            "undecided approval cannot contain decision targets",
        )
    if approval["status"] == "approved":
        require(
            data["outcome"]["readiness"] == "ready",
            "approved release must be technically ready",
        )
    publication = decision["tagPublication"]
    fields(publication, "status object verifiedAt tag candidate evidence")
    choice(publication["status"], "not_published published")
    evidence(publication["evidence"])
    if publication["status"] == "published":
        digest(publication["object"], commit=True)
        require(
            timestamp(publication["verifiedAt"]) >= timestamp(approval["decidedAt"]),
            "publication must follow human approval",
        )
        require(
            approval["status"] == "approved"
            and publication["tag"] == data["release"]["tag"]
            and publication["candidate"] == data["release"]["candidate"]["sha"]
            and linked_evidence(publication["evidence"]),
            "published tag requires exact human approval and linked publication evidence",
        )
    else:
        require(
            all(
                publication[key] is None
                for key in ("object", "verifiedAt", "tag", "candidate")
            )
            and publication["evidence"] == [],
            "unpublished tag cannot claim verification",
        )
    require(
        data["sprint"]["state"] != "closed" or publication["status"] == "published",
        "closed sprint requires verified publication",
    )


def validate(data: dict[str, Any]) -> None:
    validate_identity(data)
    validate_reviews(data)
    validate_scope(data)
    validate_checks(data)
    validate_learning(data)
    validate_decision(data)
    failed = (
        any(area["status"] == "failed" for area in data["areas"].values())
        or any(
            check["required"] and check["status"] == "failed"
            for check in data["checks"]
        )
        or bool(blockers(data))
    )
    incomplete = any(
        area["status"] == "incomplete" for area in data["areas"].values()
    ) or any(
        check["required"] and check["status"] == "not_run" for check in data["checks"]
    )
    expected = "blocked" if failed else "incomplete" if incomplete else "ready"
    require(
        data["outcome"]["readiness"] == expected,
        "readiness contradicts review, required checks or release blockers",
    )


def esc(value: Any) -> str:
    """Escape a plain value for HTML text or a quoted attribute.

    Returns:
        Escaped text. None becomes an empty string.

    """
    return html.escape("" if value is None else str(value), quote=True)


def paragraphs(value: str) -> str:
    """Render plain paragraphs without interpreting Markdown or HTML.

    Returns:
        Escaped paragraph elements.

    """
    return "".join(
        "<p>" + esc(part).replace("\n", "<br>") + "</p>"
        for part in value.replace("\r\n", "\n").replace("\r", "\n").split("\n\n")
    )


def link(label: str, target: str) -> str:
    """Construct an escaped evidence link after checking its scheme.

    Returns:
        An HTML anchor with no executable URL or unescaped attribute.

    """
    url(target)
    return f'<a href="{esc(target)}" rel="noreferrer">{esc(label)}</a>'


def evidence_html(items: list[dict[str, Any]]) -> str:
    """Render evidence links and board references in the template's list style.

    Returns:
        An evidence list, empty when no evidence was recorded.

    """
    def entry(item: dict[str, Any]) -> str:
        if item["url"]:
            return link(item["label"], item["url"])
        if item.get("ref"):
            return esc(item["label"]) + " <code>" + esc(ref_text(item["ref"])) + "</code>"
        return esc(item["label"])

    return (
        '<ul class="evidence">'
        + "".join("<li>" + entry(item) + "</li>" for item in items)
        + "</ul>"
        if items
        else ""
    )


def table_rows(
    rows: list[list[str]],
    empty: str,
    columns: int,
    numeric: tuple[int, ...] = (),
    *,
    headers: tuple[str, ...] = (),
    row_attrs: list[str] | None = None,
    cell_classes: dict[int, str] | None = None,
) -> str:
    """Wrap already escaped or constructed cells in table rows.

    Args:
        rows: Cells that are already escaped or built from escaped data.
        empty: Plain text for the empty-state row.
        columns: Column count of the table.
        numeric: Indexes of right-aligned amount columns.
        headers: Plain column labels, repeated on each cell for narrow screens.
        row_attrs: Constructed attributes for each row, in row order.
        cell_classes: Extra class name for each column index.

    Returns:
        Table rows or one explicit empty-state row.

    """

    def cell(index: int, value: str) -> str:
        classes = " ".join(
            name
            for name in (
                "num" if index in numeric else "",
                (cell_classes or {}).get(index, ""),
            )
            if name
        )
        attrs = (f' class="{classes}"' if classes else "") + (
            f' data-label="{esc(headers[index])}"' if index < len(headers) else ""
        )
        return f"<td{attrs}>{value}</td>"

    return (
        "".join(
            "<tr"
            + (row_attrs[number] if row_attrs else "")
            + ">"
            + "".join(cell(index, value) for index, value in enumerate(row))
            + "</tr>"
            for number, row in enumerate(rows)
        )
        or f'<tr class="is-empty"><td colspan="{columns}">{esc(empty)}</td></tr>'
    )


def money(value: int | float | Decimal, currency: str, language: str) -> str:
    """Format measured costs without rounding a nonzero charge to zero.

    Returns:
        A localized amount, with at least two decimal places and its currency.

    """
    amount = format(Decimal(str(value)), "f")
    whole, _, fraction = amount.partition(".")
    amount = (
        whole + LABELS[language]["decimal_mark"] + fraction.rstrip("0").ljust(2, "0")
    )
    return LABELS[language]["money"].format(amount=amount, currency=currency)


def cost_text(data: dict[str, Any]) -> str:
    """Sum recorded costs without mixing currencies.

    Returns:
        Localized currency totals or the explicit empty-cost label.

    """
    totals: dict[str, Decimal] = defaultdict(Decimal)
    for check in data["checks"]:
        if check["cost"] is not None:
            totals[check["cost"]["currency"]] += Decimal(str(check["cost"]["amount"]))
    return (
        " + ".join(
            money(amount, currency, data["report"]["language"])
            for currency, amount in sorted(totals.items())
        )
        or LABELS[data["report"]["language"]]["none"]
    )


def tone(value: str, *, blocking: bool = False) -> str:
    """Map a validated enum value to one status tone.

    Returns:
        A tone name that the template styles.

    """
    return (
        "bad"
        if blocking or value in {"failed", "rejected", "blocked", "Critical", "High"}
        else "good"
        if value
        in {
            "passed",
            "approved",
            "published",
            "resolved",
            "delivered",
            "closed",
            "adopt",
        }
        else "warn"
        if value in {"incomplete", "pending", "not_run", "open", "Medium"}
        else "info"
        if value in {"trial", "active"}
        else "muted"
    )


def ticks(tones: list[str]) -> str:
    """Draw one mark per counted item. The adjacent text carries the count.

    Returns:
        Hidden marks, or an empty string for no item or more than 30 items.

    """
    return (
        '<span class="ticks" aria-hidden="true">'
        + "".join(f'<i class="tick tick--{name}"></i>' for name in tones)
        + "</span>"
        if 0 < len(tones) <= 30
        else ""
    )


def code_link(value: str, target: str) -> str:
    """Construct a checked link whose label is set as code.

    Returns:
        An HTML anchor around an escaped code label.

    """
    url(target)
    return f'<a href="{esc(target)}" rel="noreferrer"><code>{esc(value)}</code></a>'


def gate(
    target: str, name: str, value: str, note: str, marks: str = "", *, bad: bool = False
) -> str:
    """Construct one sign-off sheet cell from escaped parts.

    Returns:
        A list item that links to the section with the evidence.

    """
    return (
        f'<li class="gate{" is-bad" if bad else ""}"><a href="{target}">'
        f'<span class="gate-name">{name}</span><span class="gate-value">{value}</span>'
        f'<span class="gate-sub">{note}</span>{marks}</a></li>'
    )


def hold(target: str, ident: str, title: str, meta: str) -> str:
    """Construct one item of the holds list from escaped parts.

    Returns:
        A list item that links to the blocking or missing evidence.

    """
    return (
        f'<li class="hold"><a class="hold-id" href="{target}">{ident}</a>'
        f'<span class="hold-title">{title}</span><span class="hold-meta">{meta}</span></li>'
    )


def findings(counts: dict[str, int] | None, unknown: str) -> str:
    """Show finding counts as text, with one hidden mark per finding.

    Returns:
        Marks and the raised / resolved / deferred counts, or the escaped unknown label.

    """
    if not counts:
        return esc(unknown)
    unsettled = counts["raised"] - counts["resolved"] - counts["deferred"]
    return (
        '<span class="fticks" aria-hidden="true">'
        + '<i class="tick tick--good"></i>' * counts["resolved"]
        + '<i class="tick tick--warn"></i>' * counts["deferred"]
        + '<i class="tick tick--open"></i>' * unsettled
        + "</span>"
        if 0 < counts["raised"] <= 30
        else ""
    ) + " / ".join(str(counts[key]) for key in ("raised", "resolved", "deferred"))


def render(
    data: dict[str, Any], *, source_bytes: bytes | None = None
) -> tuple[str, str]:
    """Validate and render captured facts without fetching external evidence.

    Returns:
        HTML and source SHA-256. CLI hashes exact bytes; direct calls default to canonical JSON.

    """
    validate(data)
    if source_bytes is not None:
        require(
            json.loads(source_bytes, object_pairs_hook=unique_object) == data,
            "source bytes differ from rendered data",
        )
    labels, enums = (
        LABELS[data["report"]["language"]],
        ENUMS[data["report"]["language"]],
    )
    source_digest = hashlib.sha256(
        source_bytes
        if source_bytes is not None
        else json.dumps(
            data,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
            allow_nan=False,
        ).encode()
    ).hexdigest()

    def enum(value: str) -> str:
        return esc(enums.get(value, value))

    def chip(value: str, *, blocking: bool = False) -> str:
        return f'<span class="chip chip--{tone(value, blocking=blocking)}">{enum(value)}</span>'

    def optional(value: Any) -> str:
        return esc(labels["unknown"] if value is None else value)

    def code(value: Any) -> str:
        return (
            "<code>" + esc(value) + "</code>" if value is not None else optional(None)
        )

    def sub(value: str) -> str:
        return '<span class="sub">' + value + "</span>" if value else ""

    def key_code(value: str | None) -> str:
        return code(value) if value else ""

    def board_ref_html(ref: dict[str, str] | None) -> str:
        return code(ref_text(ref)) if ref else optional(None)

    required = [check for check in data["checks"] if check["required"]]
    passed = sum(check["status"] == "passed" for check in required)
    excluded = sum(check["status"] == "not_applicable" for check in required)
    report, sprint, release = data["report"], data["sprint"], data["release"]
    approval, publication = (
        data["decision"]["approval"],
        data["decision"]["tagPublication"],
    )
    values = {"l_" + key: esc(value) for key, value in labels.items()}
    values.update(
        {
            "lang": report["language"],
            "schema_version": "1",
            "document_title": esc(sprint["name"] + " | " + labels["report_kind"]),
            "fictional_html": '<p class="fictional"><span>'
            + esc(labels["fictional"])
            + "</span></p>"
            if report.get("fictional", False)
            else "",
            "project_name": esc(data["project"]["name"]),
            "sprint_name": esc(sprint["name"]),
            "sprint_goal": esc(sprint["goal"]),
            "sprint_key_html": code(sprint["key"]),
            "vault_name": esc(sprint["vault"]),
            "scope_digest": esc(sprint["scopeDigest"]),
            "sprint_state_label": enum(sprint["state"]),
            "sprint_dates": esc(
                labels["date_range"].format(
                    start=sprint["startDate"][:10], end=sprint["endDate"][:10]
                )
            ),
            "report_version": esc(f"{report['id']} · v{report['version']}"),
            "readiness": data["outcome"]["readiness"],
            "readiness_label": enum(data["outcome"]["readiness"]),
            "outcome_headline": esc(data["outcome"]["headline"]),
            "outcome_summary_html": paragraphs(data["outcome"]["summary"]),
            "figure_scope": f"{sum(item['outcome'] == 'delivered' for item in data['scope']['items'])} / {len(data['scope']['items'])}",
            "figure_checks": f"{passed} / {len(required) - excluded}",
            "figure_blockers": str(len(blockers(data))),
            "figure_cost": esc(cost_text(data)),
            "approval_chip_html": chip(approval["status"])
            + (
                " " + esc(labels["approval_prior_note"])
                if approval["status"] in {"approved", "rejected"}
                else ""
            ),
            "tag_chip_html": chip(publication["status"]),
            "sprint_state_chip_html": chip(sprint["state"]),
            "closure_task_html": code(sprint["closureTask"]),
            "repository_link_html": link(release["repository"], release["repository"]),
            "base_html": (
                code(release["base"]["ref"]) + " " if release["base"]["ref"] else ""
            )
            + code(release["base"]["sha"][:12]),
            "base_full_html": (
                code(release["base"]["ref"]) + " " if release["base"]["ref"] else ""
            )
            + code(release["base"]["sha"]),
            "candidate_html": link(
                release["candidate"]["sha"], release["candidate"]["url"]
            ),
            "candidate_code_html": code_link(
                release["candidate"]["sha"], release["candidate"]["url"]
            ),
            "release_tag": esc(release["tag"]),
            "report_identity": esc(f"{report['id']} · v{report['version']}"),
            "source_digest": source_digest,
            "scope_summary_html": paragraphs(data["scope"]["summary"]),
            "checks_summary": esc(
                labels["checks_summary"].format(
                    passed=passed,
                    excluded=excluded,
                    missing=len(required) - passed - excluded,
                    optional=len(data["checks"]) - len(required),
                )
            ),
            "retrospective_summary_html": paragraphs(data["retrospective"]["summary"]),
            "approval_by": optional(approval["approver"]),
            "approval_at": optional(approval["decidedAt"]),
            "approval_sprint": code(approval["sprint"]),
            "approval_scope_html": code(approval["scopeDigest"]),
            "approval_ref_html": board_ref_html(approval["ref"]),
            "approval_candidate_html": code(approval["candidate"]),
            "approval_report_html": code(approval["reportDigest"])
            + (
                sub(link(labels["approved_report_link"], approval["reportUrl"]))
                if approval["reportUrl"]
                else ""
            ),
            "approval_tag_html": code(approval["tag"]),
            "approval_note_html": paragraphs(approval["note"])
            if approval["note"]
            else optional(None),
            "tag_object_html": code(publication["object"])
            + sub(optional(publication["verifiedAt"]))
            + sub(
                esc(labels["publication_tag"])
                + " "
                + code(publication["tag"])
                + " · "
                + esc(labels["publication_candidate"])
                + " "
                + code(publication["candidate"])
            )
            + evidence_html(publication["evidence"])
            if publication["object"]
            else "",
            "generated_at": esc(report["generatedAt"]),
            "prepared_by": esc(report["preparedBy"]),
        }
    )

    # Sign-off sheet, agent half: three totals, then the four review areas.
    values["total_gates_html"] = (
        gate(
            "#scope",
            esc(labels["s_scope"]),
            values["figure_scope"],
            esc(labels["figure_scope"]),
            ticks([tone(item["outcome"]) for item in data["scope"]["items"]]),
        )
        + gate(
            "#checks",
            esc(labels["s_checks"]),
            values["figure_checks"],
            esc(labels["figure_checks"])
            + "<br>"
            + esc(labels["figure_cost"])
            + " "
            + values["figure_cost"],
            ticks([tone(check["status"]) for check in required]),
            bad=any(check["status"] == "failed" for check in required),
        )
        + gate(
            "#risks",
            esc(labels["s_risks"]),
            values["figure_blockers"],
            esc(labels["figure_blockers"]),
            ticks(["bad"] * len(blockers(data))),
            bad=bool(blockers(data)),
        )
    )
    values["area_gates_html"] = "".join(
        gate(
            f"#area-{name}",
            enum(name),
            chip(area["status"]),
            esc(labels["round"]) + " " + optional(area["round"]),
        )
        for name, area in ((name, data["areas"][name]) for name in AREAS)
    )

    def signoff() -> str:
        """Fill the human part of the sign-off sheet.

        Returns:
            Approval and tag publication lines from escaped data.

        """
        decided = approval["status"] in {"approved", "rejected"}
        if decided:
            approval_detail = (
                "<p>"
                + esc(approval["decidedAt"])
                + "</p><p>"
                + esc(labels["approval_prior_note"])
                + "</p><p>"
                + link(labels["approved_report_link"], approval["reportUrl"])
                + "</p>"
            )
        else:
            approval_detail = (
                paragraphs(approval["note"]) if approval["note"] else ""
            ) + (
                "<p>"
                + esc(labels["reference"] + labels["colon"])
                + board_ref_html(approval["ref"])
                + "</p>"
                if approval["ref"]
                else ""
            )
            approval_detail += (
                '<p class="sign-label">'
                + esc(labels["decision_names"])
                + '</p><dl class="sign-targets"><div><dt>'
                + esc(labels["sprint"])
                + "</dt><dd>"
                + code(sprint["key"])
                + '</dd></div><div class="wide"><dt>'
                + esc(labels["scope_digest"])
                + "</dt><dd>"
                + code(sprint["scopeDigest"])
                + "</dd></div><div><dt>"
                + esc(labels["proposed_tag"])
                + "</dt><dd>"
                + code(release["tag"])
                + '</dd></div><div class="wide"><dt>'
                + esc(labels["candidate"])
                + "</dt><dd>"
                + code(release["candidate"]["sha"])
                + '</dd></div><div class="wide"><dt>'
                + esc(labels["report"])
                + "</dt><dd>"
                + esc(labels["report_digest_target"])
                + "</dd></div></dl>"
            )
        return (
            '<div class="sign"><p class="sign-label">'
            + esc(labels["human_approval"])
            + '</p><p class="sign-line">'
            + chip(approval["status"])
            + (
                '<span class="signer">' + esc(approval["approver"]) + "</span>"
                if decided
                else ""
            )
            + "</p>"
            + (
                '<div class="sign-detail">' + approval_detail + "</div>"
                if approval_detail
                else ""
            )
            + '</div><div class="sign"><p class="sign-label">'
            + esc(labels["tag_publication"])
            + " "
            + code(release["tag"])
            + '</p><p class="sign-line">'
            + chip(publication["status"])
            + (
                " <code>" + esc(publication["object"][:12]) + "</code>"
                if publication["object"]
                else ""
            )
            + "</p>"
            + (
                '<div class="sign-detail"><p>'
                + esc(labels["publication_verified_at"])
                + " "
                + esc(publication["verifiedAt"])
                + "</p></div>"
                if publication["verifiedAt"]
                else ""
            )
            + "</div>"
        )

    def holds() -> str:
        """List what holds the release, in the order of the readiness rules.

        Returns:
            Blocking and unfinished items, or the explicit empty state.

        """
        stops, missing = [], []
        for name in AREAS:
            area = data["areas"][name]
            if area["status"] != "passed":
                (stops if area["status"] == "failed" else missing).append(
                    hold(
                        f"#area-{name}",
                        enum(name),
                        esc(labels["round"])
                        + " "
                        + optional(area["round"])
                        + (" · " + code(area["decision"]) if area["decision"] else ""),
                        chip(area["status"]),
                    )
                )
        for number, check in enumerate(data["checks"]):
            if check["required"] and check["status"] in {"failed", "not_run"}:
                (stops if check["status"] == "failed" else missing).append(
                    hold(
                        f"#check-{number}",
                        esc(check["id"]),
                        esc(check["name"]),
                        chip(check["status"]),
                    )
                )
        for number, risk in enumerate(data["risks"]):
            if risk not in blockers(data):
                continue
            stops.append(
                hold(
                    f"#risk-{number}",
                    esc(risk["id"]),
                    esc(risk["title"]),
                    chip(risk["severity"]) + chip(risk["status"], blocking=True),
                )
            )
        return (
            "<h3>"
            + esc(labels["holds_blocking"])
            + '</h3><ul class="hold-list">'
            + "".join(stops)
            + "</ul>"
            if stops
            else ""
        ) + (
            "<h3>"
            + esc(labels["holds_missing"])
            + '</h3><ul class="hold-list">'
            + "".join(missing)
            + "</ul>"
            if missing
            else ""
        ) or "<h3>" + esc(labels["holds_none_title"]) + "</h3><p>" + esc(
            labels["holds_none"]
        ) + "</p>"

    values["signoff_html"] = signoff()
    values["holds_html"] = holds()

    values["scope_rows_html"] = table_rows(
        [
            [
                '<span class="key">' + esc(item["key"]) + "</span>",
                esc(item["title"])
                + sub(esc(labels["origin"] + labels["colon"]) + enum(item["origin"])),
                chip(item["outcome"]),
                optional(item["note"]) + sub(key_code(item["destination"])),
            ]
            for item in data["scope"]["items"]
        ],
        labels["none"],
        4,
        headers=(labels["item"], labels["title"], labels["outcome"], labels["note"]),
    )

    values["areas_html"] = "".join(
        f'<article class="area" id="area-{name}"><div class="area-head"><h3>{enum(name)}</h3>{chip(area["status"])}</div>'
        '<dl class="area-facts">'
        f"<div><dt>{esc(labels['review_decision'])}</dt><dd>"
        + (code(area["decision"]) if area["decision"] else optional(None))
        + f"</dd></div><div><dt>{esc(labels['round'])}</dt><dd>{optional(area['round'])}</dd></div>"
        + f'<div class="wide"><dt>{esc(labels["reviewers"])}</dt><dd>'
        + optional(", ".join(area["reviewers"]) or None)
        + f'</dd></div><div class="wide"><dt>{esc(labels["findings"])}</dt><dd>'
        + findings(area["findings"], labels["unknown"])
        + '</dd></div></dl><div class="prose">'
        + paragraphs(area["summary"])
        + "</div><div>"
        + evidence_html(area["evidence"])
        + "</div></article>"
        for name, area in ((name, data["areas"][name]) for name in AREAS)
    )
    check_rows = []
    for check in data["checks"]:
        cost = check["cost"]
        cost_html = optional(None)
        if cost:
            cost_html = esc(money(cost["amount"], cost["currency"], report["language"]))
            if "budget" in cost:
                cost_html += sub(
                    esc(
                        labels["budget"]
                        + " · "
                        + money(cost["budget"], cost["currency"], report["language"])
                    )
                )
                if cost["amount"] > cost["budget"]:
                    cost_html += sub(esc(labels["budget_exceeded"]))
            cost_html += sub(
                esc(
                    f"{labels['calls']} · {cost.get('calls', labels['unknown'])} · {cost.get('model', labels['unknown'])}"
                )
            )
        check_rows.append(
            [
                '<span class="key">'
                + esc(check["id"])
                + "</span> "
                + esc(check["name"])
                + sub(
                    enum(check["kind"])
                    + " · "
                    + esc(
                        labels["required"] if check["required"] else labels["optional"]
                    )
                ),
                chip(check["status"]),
                paragraphs(check["detail"])
                + (sub(code(check["command"])) if check["command"] else "")
                + evidence_html(check["evidence"]),
                cost_html,
            ]
        )
    values["check_rows_html"] = table_rows(
        check_rows,
        labels["none"],
        4,
        numeric=(3,),
        headers=(labels["check"], labels["status"], labels["detail"], labels["cost"]),
        row_attrs=[f' id="check-{number}"' for number in range(len(data["checks"]))],
    )
    values["measurement_rows_html"] = table_rows(
        [
            [
                esc(item["metric"]) + sub(esc(item["scope"])),
                optional(item["baseline"]),
                optional(item["candidate"]),
                esc(item["observation"])
                + sub(esc(labels["tool"] + labels["colon"]) + optional(item["tool"])),
            ]
            for item in data["measurements"]
        ],
        labels["none"],
        4,
        numeric=(1, 2),
        headers=(
            labels["metric"],
            labels["baseline"],
            labels["candidate"],
            labels["observation"],
        ),
    )
    values["risk_rows_html"] = table_rows(
        [
            [
                '<span class="key">' + esc(item["id"]) + "</span>",
                esc(item["title"])
                + sub(esc(labels["area"] + labels["colon"]) + enum(item["area"]))
                + evidence_html(item["evidence"]),
                chip(item["severity"]),
                chip(item["status"], blocking=item in blockers(data))
                + sub(esc(labels["risk_blocker"]) if item in blockers(data) else ""),
                optional(item["resolution"]) + sub(key_code(item["followup"])),
            ]
            for item in data["risks"]
        ],
        labels["none"],
        5,
        headers=(
            labels["id"],
            labels["risk"],
            labels["severity"],
            labels["status"],
            labels["resolution"],
        ),
        row_attrs=[
            f' id="risk-{number}"'
            + (' class="is-blocking"' if item in blockers(data) else "")
            for number, item in enumerate(data["risks"])
        ],
    )
    values["followup_items_html"] = (
        "".join(
            '<li><span class="key">'
            + esc(item["key"])
            + "</span>"
            + esc(item["title"])
            + sub(esc(item["reason"]))
            + "</li>"
            for item in data["followups"]
        )
        or "<li>" + esc(labels["none"]) + "</li>"
    )
    values["retro_rows_html"] = table_rows(
        [
            [
                '<span class="key">'
                + esc(item["id"])
                + "</span> "
                + esc(item["observation"])
                + evidence_html(item["evidence"]),
                esc(item["proposal"]) + sub(enum(item["type"])),
                chip(item["recommendation"]),
                chip(item["humanChoice"]) + sub(key_code(item["destination"])),
            ]
            for item in data["retrospective"]["candidates"]
        ],
        labels["none"],
        4,
        headers=(
            labels["observation"],
            labels["proposal"],
            labels["recommendation"],
            labels["human_choice"],
        ),
        cell_classes={3: "human-col"},
    )
    return Template(
        (ROOT / "assets/report.html").read_text(encoding="utf-8")
    ).substitute(values), source_digest


def unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    """Reject duplicate JSON keys instead of hiding an earlier source value.

    Returns:
        One JSON object with unique keys.

    """
    result: dict[str, Any] = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON key: " + key)
        result[key] = value
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        source_bytes = args.input.read_bytes()
        data = json.loads(source_bytes, object_pairs_hook=unique_object)
        output, source_digest = render(data, source_bytes=source_bytes)
        with args.output.open("xb") as stream:
            stream.write(output.encode("utf-8"))
        print(
            json.dumps(
                {
                    "sourceDigest": source_digest,
                    "htmlDigest": hashlib.sha256(output.encode()).hexdigest(),
                    "output": str(args.output),
                }
            )
        )
    except (ValueError, TypeError, KeyError, AttributeError, OSError) as exc:
        print(f"FAIL: {exc}")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
