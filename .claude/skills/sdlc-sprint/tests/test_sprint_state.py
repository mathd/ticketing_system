"""Check sprint lifecycle decisions with captured, fictional facts.

Every rule in scripts/sprint_state.py has a case here that goes red when the rule is
removed. The expected digests come from the design's own code block (design_digest
below) and from two golden literals. They never come from the script under test.

Run from the repository root:
python3 -B -m unittest discover -s .claude/skills/sdlc-sprint/tests -v
"""

import copy
import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime, timezone
from hashlib import sha256
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/sprint_state.py"
spec = importlib.util.spec_from_file_location("sdlc_sprint_state", SCRIPT)
assert spec is not None
assert spec.loader is not None
state = importlib.util.module_from_spec(spec)
spec.loader.exec_module(state)

VAULT, SPRINT, DONE = "demo", "S-3", "Done"
ALL_DONE = [("DEMO-1", "Done"), ("DEMO-2", "Done"), ("DEMO-3", "Done")]

# The design's scope digest, run on the example data, and on a second vector with
# non-ASCII text, a null goal, a UTC offset, no end date and keys that sort
# differently as text and as numbers.
GOLDEN_EXAMPLE = "b86d243be72aaac94e315dca2ea1e4459b91fd049a10266c17fffb641ecf3f56"
GOLDEN_UNICODE = "a7c1e81999a7795c8650d265fe7117269f5fc6b3cf567608d9b9f9c1bc9aee27"
# Computed by the BOARD's own sprints.scope_digest (sdlc-board, feat/sprints,
# 2026-10-17) for a hand-edited note: a padded start date and a `Z` end date.
GOLDEN_BOARD_PADDED = "cadb358538f207d5c38e8dbfcf0c998bd16bff64bbb2bba885f51ea685198821"


def design_digest(vault, key, name, goal, start, end, member_keys):
    """The design's section 1 code block, copied as written."""

    def utc(value):  # None stays None; a value without an offset is refused on write
        return (
            datetime.fromisoformat(value)
            .astimezone(timezone.utc)
            .isoformat(timespec="seconds")
        )

    canonical = {
        "vault": vault,
        "key": key,
        "name": name or "",
        "goal": goal or "",
        "startDate": utc(start) if start else None,
        "endDate": utc(end) if end else None,
        "members": sorted(member_keys),
    }
    return sha256(
        json.dumps(
            canonical, sort_keys=True, separators=(",", ":"), ensure_ascii=False
        ).encode("utf-8")
    ).hexdigest()


def start_snapshot():
    return json.loads(
        (ROOT / "assets/snapshot.example.json").read_text(encoding="utf-8")
    )


def close_snapshot():
    return json.loads(
        (ROOT / "assets/snapshot.close.example.json").read_text(encoding="utf-8")
    )


def rebuild(data, approve=True, **changes):
    """Change sprint text, dates or members, then recompute the board's digest.

    The digest comes from the design's code block, so the snapshot stays consistent
    and only the change under test is wrong. With approve=True the human records
    name the new digest too.
    """
    sprint = data["sprint"]
    for field in ("name", "goal", "startDate", "endDate"):
        if field in changes:
            sprint[field] = changes[field]
    if "members" in changes:
        data["members"] = [
            {"key": key, "status": status, "blocked": False}
            for key, status in changes["members"]
        ]
    data["scopeDigest"] = design_digest(
        data["vault"],
        sprint["key"],
        sprint["name"],
        sprint["goal"],
        sprint["startDate"],
        sprint["endDate"],
        [member["key"] for member in data["members"]],
    )
    if approve:
        for record in ("brief", "startRequest", "closeApproval", "priorityApproval"):
            if data.get(record):
                data[record]["scopeDigest"] = data["scopeDigest"]
    return data


def decide(data, intent="start", vault=VAULT, sprint=SPRINT, done=DONE):
    return state.decide(data, intent, vault, sprint, done)


def progress(data, vault=VAULT, sprint=SPRINT, done=DONE):
    return state.progress(data, vault, sprint, done)


class DigestTests(unittest.TestCase):
    BASE = (
        VAULT,
        SPRINT,
        "Invitations",
        "Invite and revoke users",
        "2026-10-19T09:00:00-04:00",
        "2026-10-30T17:00:00-04:00",
        ["DEMO-1", "DEMO-2"],
    )

    def test_golden_vectors_match_the_design_code_block(self):
        example = (*self.BASE[:6], ["DEMO-1", "DEMO-2", "DEMO-3"])
        unicode_vector = (
            VAULT,
            SPRINT,
            "Étape « 3 »",
            None,
            "2026-10-19T13:00:00+00:00",
            None,
            ["TKT-9", "TKT-10", "TKT-2"],
        )
        for vector, golden in (
            (example, GOLDEN_EXAMPLE),
            (unicode_vector, GOLDEN_UNICODE),
        ):
            with self.subTest(name=vector[2]):
                self.assertEqual(design_digest(*vector), golden)
                self.assertEqual(state.scope_digest(*vector), golden)

    def test_dates_are_normalized_like_the_board(self):
        # The board strips whitespace and reads `Z` as UTC before parsing; the
        # checker must agree byte for byte, or a hand-edited date stops a start.
        vector = ("sdlc", "S-4", "Padded", "Hand-edited dates",
                  " 2026-10-19T09:00:00-04:00 ", "2026-11-02T17:00:00Z", ["TKT-2", "TKT-1"])
        self.assertEqual(state.scope_digest(*vector), GOLDEN_BOARD_PADDED)

    def test_every_component_changes_the_digest(self):
        expected = state.scope_digest(*self.BASE)
        changes = {
            "vault": (0, "other"),
            "key": (1, "S-4"),
            "name": (2, "Other name"),
            "goal": (3, "Other goal"),
            "start": (4, "2026-10-19T10:00:00-04:00"),
            "end": (5, "2026-10-31T17:00:00-04:00"),
            "members": (6, ["DEMO-1", "DEMO-2", "DEMO-3"]),
        }
        for label, (index, value) in changes.items():
            candidate = list(self.BASE)
            candidate[index] = value
            with self.subTest(component=label):
                self.assertNotEqual(state.scope_digest(*candidate), expected)
                self.assertEqual(
                    state.scope_digest(*candidate), design_digest(*candidate)
                )

    def test_member_order_does_not_change_the_digest(self):
        reordered = list(self.BASE)
        reordered[6] = ["DEMO-2", "DEMO-1"]
        self.assertEqual(
            state.scope_digest(*reordered), state.scope_digest(*self.BASE)
        )

    def test_equivalent_instants_keep_the_digest(self):
        expected = state.scope_digest(*self.BASE)
        for start, end in (
            ("2026-10-19T13:00:00+00:00", "2026-10-30T21:00:00+00:00"),
            ("2026-10-19T13:00:00Z", "2026-10-30T21:00:00Z"),
            ("2026-10-19T09:00:00.900-04:00", "2026-10-30T17:00:00.100-04:00"),
        ):
            candidate = list(self.BASE)
            candidate[4:6] = [start, end]
            with self.subTest(start=start):
                self.assertEqual(state.scope_digest(*candidate), expected)

    def test_empty_text_and_missing_dates_follow_the_design(self):
        nothing = (VAULT, SPRINT, None, None, None, None, [])
        empty = (VAULT, SPRINT, "", "", "", "", [])
        self.assertEqual(state.scope_digest(*nothing), design_digest(*nothing))
        self.assertEqual(state.scope_digest(*empty), design_digest(*empty))
        self.assertEqual(state.scope_digest(*nothing), state.scope_digest(*empty))

    def test_a_date_without_an_offset_is_refused(self):
        with self.assertRaisesRegex(ValueError, "offset"):
            state.utc("2026-10-19T09:00:00")
        with self.assertRaisesRegex(ValueError, "offset"):
            state.scope_digest(VAULT, SPRINT, "n", "g", "2026-10-19T09:00:00", None, [])


class SnapshotValidationTests(unittest.TestCase):
    def test_progress_refuses_an_invalid_or_foreign_snapshot(self):
        cases = [
            ("wrong vault", "wrong vault", lambda d: d.update(vault="other")),
            ("wrong sprint", "wrong sprint", lambda d: d["sprint"].update(key="S-4")),
            (
                "unknown state",
                "unknown sprint state",
                lambda d: d["sprint"].update(state="future"),
            ),
            ("blank name", "name required", lambda d: d["sprint"].update(name=" ")),
            (
                "goal type",
                "goal must be text or null",
                lambda d: d["sprint"].update(goal=5),
            ),
            (
                "start scope type",
                "startScope must be a list",
                lambda d: d["sprint"].update(startScope="DEMO-1"),
            ),
            (
                "end scope duplicates",
                "endScope must be a list",
                lambda d: d["sprint"].update(endScope=["DEMO-1", "DEMO-1"]),
            ),
            (
                "membership incomplete",
                "membership is incomplete",
                lambda d: d.update(membershipComplete=False),
            ),
            (
                "membership flag missing",
                "membership is incomplete",
                lambda d: d.pop("membershipComplete"),
            ),
            (
                "unreadable member note",
                "cannot be read: DEMO-2",
                lambda d: d.update(unreadable=["DEMO-2"]),
            ),
            (
                "unreadable missing",
                "unreadable must be a list",
                lambda d: d.pop("unreadable"),
            ),
            (
                "history gaps missing",
                "historyGaps must be a list",
                lambda d: d.pop("historyGaps"),
            ),
            ("observation missing", "timestamp required", lambda d: d.pop("observedAt")),
            (
                "observation without offset",
                "offset",
                lambda d: d.update(observedAt="2026-10-19T08:59:00"),
            ),
            ("members type", "members must be a list", lambda d: d.update(members="DEMO-1")),
            (
                "member type",
                "member must be an object",
                lambda d: d["members"].append("DEMO-9"),
            ),
            (
                "member without key",
                "duplicate or missing member key",
                lambda d: d["members"][0].pop("key"),
            ),
            (
                "duplicate member",
                "duplicate or missing member key",
                lambda d: d["members"].append(copy.deepcopy(d["members"][0])),
            ),
            (
                "member status",
                "member status required",
                lambda d: d["members"][0].update(status=""),
            ),
            (
                "blocker state",
                "blocker state must be explicit",
                lambda d: d["members"][0].update(blocked="false"),
            ),
            ("digest missing", "SHA-256", lambda d: d.pop("scopeDigest")),
            ("digest malformed", "SHA-256", lambda d: d.update(scopeDigest="ABC")),
            (
                "digest too long",
                "SHA-256",
                lambda d: d.update(scopeDigest="e" * 65),
            ),
            (
                "digest in upper case",
                "SHA-256",
                lambda d: d.update(scopeDigest=d["scopeDigest"].upper()),
            ),
            (
                "digest differs",
                "differs from the board's",
                lambda d: d.update(scopeDigest="e" * 64),
            ),
            (
                "date without offset",
                "offset",
                lambda d: d["sprint"].update(endDate="2026-10-30T17:00:00"),
            ),
        ]
        for label, message, mutate in cases:
            data = start_snapshot()
            mutate(data)
            with self.subTest(case=label), self.assertRaisesRegex(ValueError, message):
                progress(data)

    def test_progress_refuses_a_wrong_expected_identity(self):
        data = start_snapshot()
        for vault, sprint, done, message in (
            ("other", SPRINT, DONE, "wrong vault"),
            (VAULT, "S-4", DONE, "wrong sprint"),
            ("", SPRINT, DONE, "vault name required"),
            (VAULT, "S3", DONE, "sprint key must look like"),
            (VAULT, "s-3", DONE, "sprint key must look like"),
            (VAULT, "S-3 ", DONE, "sprint key must look like"),
            (VAULT, "S-", DONE, "sprint key must look like"),
            (VAULT, SPRINT, "", "done status required"),
        ):
            with (
                self.subTest(vault=vault, sprint=sprint, done=done),
                self.assertRaisesRegex(ValueError, message),
            ):
                state.progress(data, vault, sprint, done)

    def test_progress_refuses_a_malformed_brief(self):
        cases = [
            ("digest", "brief digest", lambda b: b.update(digest="xyz")),
            ("sprint", "must name its sprint", lambda b: b.update(sprint="")),
            (
                "release flag",
                "release must be true or false",
                lambda b: b.update(release="true"),
            ),
            (
                "invalid review policy",
                "review must be full or none",
                lambda b: b.update(review="skip"),
            ),
            (
                "closure task type",
                "closureTask must be a key or null",
                lambda b: b.update(closureTask=3),
            ),
            (
                "members",
                "unique keys",
                lambda b: b.update(members=["DEMO-1", "DEMO-1"]),
            ),
            (
                "member key",
                "unique keys",
                lambda b: b.update(members=["DEMO-1", ""]),
            ),
            ("scope digest", "scope digest", lambda b: b.pop("scopeDigest")),
            ("approver", "approver required", lambda b: b.pop("approvedBy")),
        ]
        for label, message, mutate in cases:
            data = start_snapshot()
            mutate(data["brief"])
            with self.subTest(case=label), self.assertRaisesRegex(ValueError, message):
                progress(data)

    def test_progress_refuses_a_brief_that_is_not_an_object(self):
        data = start_snapshot()
        data["brief"] = "approved"
        with self.assertRaisesRegex(ValueError, "brief must be an object"):
            progress(data)

    def test_decisions_fail_when_the_snapshot_is_invalid(self):
        for intent, data in (("start", start_snapshot()), ("close", close_snapshot())):
            data["membershipComplete"] = False
            with self.subTest(intent=intent), self.assertRaises(ValueError):
                decide(data, intent)
        with self.assertRaisesRegex(ValueError, "unknown intent"):
            state.decide(start_snapshot(), "open", VAULT, SPRINT, DONE)


class ProgressTests(unittest.TestCase):
    def test_counts_derive_acceptance_from_the_done_status(self):
        data = close_snapshot()
        data["members"][0].update(status="Building", blocked=True)
        data["members"][1].update(blocked=True)  # Done and flagged: not blocked
        counts = progress(data)
        self.assertEqual(
            (counts["accepted"], counts["total"], counts["remaining"]), (2, 3, 1)
        )
        self.assertEqual(counts["blocked"], ["DEMO-1"])
        self.assertEqual(counts["functional"], {"accepted": 1, "total": 2})
        self.assertEqual(counts["closure"], "Done")
        self.assertEqual((counts["added"], counts["removed"]), ([], []))

    def test_the_done_status_comes_from_the_caller(self):
        counts = progress(close_snapshot(), done="PO Review")
        self.assertEqual((counts["accepted"], counts["remaining"]), (0, 3))
        self.assertEqual(counts["functional"], {"accepted": 0, "total": 2})

    def test_progress_reports_the_digest_it_recomputed(self):
        counts = progress(start_snapshot())
        self.assertEqual(counts["scopeDigest"], GOLDEN_EXAMPLE)
        self.assertEqual((counts["vault"], counts["sprint"]), (VAULT, SPRINT))
        self.assertEqual(counts["state"], "planned")

    def test_scope_changes_are_reported_against_the_brief_and_the_start_scope(self):
        data = rebuild(
            close_snapshot(),
            approve=False,
            members=[("DEMO-1", "Done"), ("DEMO-2", "Done"), ("DEMO-4", "Done")],
        )
        counts = progress(data)
        self.assertEqual((counts["added"], counts["removed"]), (["DEMO-4"], ["DEMO-3"]))
        self.assertEqual(
            counts["sinceStart"], {"added": ["DEMO-4"], "removed": ["DEMO-3"]}
        )
        self.assertIsNone(counts["closure"])

    def test_a_planned_sprint_has_no_start_baseline(self):
        data = start_snapshot()
        data["sprint"]["startScope"] = []
        self.assertIsNone(progress(data)["sinceStart"])

    def test_a_sprint_without_a_brief_has_no_approved_baseline(self):
        data = start_snapshot()
        del data["brief"], data["startRequest"]
        counts = progress(data)
        self.assertIsNone(counts["added"])
        self.assertIsNone(counts["removed"])
        self.assertIsNone(counts["closure"])
        self.assertEqual(counts["functional"], {"accepted": 0, "total": 3})

    def test_a_closed_sprint_reports_a_hand_edit_against_end_scope(self):
        data = close_snapshot()
        data["sprint"].update(
            state="closed", endScope=["DEMO-1", "DEMO-2", "DEMO-3"]
        )
        self.assertIs(progress(data)["endScopeMismatch"], False)
        rebuild(data, approve=False, members=[("DEMO-1", "Done"), ("DEMO-2", "Done")])
        self.assertIs(progress(data)["endScopeMismatch"], True)
        data["sprint"]["state"] = "active"  # only a closed sprint is compared
        self.assertIsNone(progress(data)["endScopeMismatch"])
        data["sprint"].update(state="closed", endScope=None)
        self.assertIsNone(progress(data)["endScopeMismatch"])

    def test_history_gaps_are_counted_and_never_decide(self):
        data = start_snapshot()
        data["historyGaps"] = [{"key": "DEMO-1"}, "no membership line for DEMO-2"]
        self.assertEqual(progress(data)["historyGaps"], 2)
        self.assertEqual(decide(data), ("START", []))
        data = close_snapshot()
        data["historyGaps"] = [{"key": "DEMO-1"}]
        self.assertEqual(decide(data, "close"), ("CLOSE", []))


class StartTests(unittest.TestCase):
    def test_the_example_is_eligible(self):
        self.assertEqual(decide(start_snapshot()), ("START", []))
        self.assertEqual(
            state.action(start_snapshot(), "start", VAULT, SPRINT, DONE), "START"
        )

    def test_each_rule_stops_the_start_with_its_own_reason(self):
        def empty_sprint(data):
            data["brief"].update(release=False, review="none", reviewReason="Planning exercise approved by owner.", closureTask=None, members=[])
            rebuild(data, members=[])

        added = [
            ("DEMO-1", "Ready"),
            ("DEMO-2", "Ready"),
            ("DEMO-3", "Backlog"),
            ("DEMO-9", "Ready"),
        ]
        cases = [
            (
                "active",
                ["sprint is not planned"],
                lambda d: d["sprint"].update(state="active"),
            ),
            (
                "closed",
                ["sprint is not planned"],
                lambda d: d["sprint"].update(state="closed"),
            ),
            ("no brief", ["no approved brief"], lambda d: d.pop("brief")),
            (
                "brief sprint",
                ["brief names another sprint"],
                lambda d: d["brief"].update(sprint="S-4"),
            ),
            (
                "stale brief approval",
                ["brief approval names another scope digest"],
                lambda d: d["brief"].update(scopeDigest="e" * 64),
            ),
            (
                "member added after approval",
                ["members differ from the approved brief (added: DEMO-9; removed: none)"],
                lambda d: rebuild(d, members=added),
            ),
            (
                "member removed after approval",
                ["members differ from the approved brief (added: none; removed: DEMO-2)"],
                lambda d: rebuild(d, members=[("DEMO-1", "Ready"), ("DEMO-3", "Backlog")]),
            ),
            (
                "closure task not a member",
                ["closure task is not a member"],
                lambda d: d["brief"].update(closureTask="DEMO-99"),
            ),
            (
                "release without a closure task",
                ["closure task is not a member"],
                lambda d: d["brief"].update(closureTask=None),
            ),
            ("no member", ["no member"], empty_sprint),
            ("no goal", ["goal required"], lambda d: rebuild(d, goal=None)),
            ("blank goal", ["goal required"], lambda d: rebuild(d, goal="  ")),
            ("no start request", ["no start request"], lambda d: d.pop("startRequest")),
            (
                "request sprint",
                ["start request names another sprint"],
                lambda d: d["startRequest"].update(sprint="S-4"),
            ),
            (
                "request brief",
                ["start request names another brief"],
                lambda d: d["startRequest"].update(briefDigest="e" * 64),
            ),
            (
                "request scope digest",
                ["start request names another scope digest"],
                lambda d: d["startRequest"].update(scopeDigest="e" * 64),
            ),
        ]
        for label, reasons, mutate in cases:
            data = start_snapshot()
            mutate(data)
            with self.subTest(case=label):
                self.assertEqual(decide(data), ("STOP", reasons))

    def test_every_rule_is_evaluated_so_all_reasons_are_listed(self):
        data = start_snapshot()
        data["sprint"]["state"] = "active"
        del data["startRequest"]
        data["brief"]["scopeDigest"] = "e" * 64
        self.assertEqual(
            decide(data),
            (
                "STOP",
                [
                    "sprint is not planned",
                    "brief approval names another scope digest",
                    "no start request",
                ],
            ),
        )

    def test_invalid_start_inputs_fail_instead_of_stopping(self):
        cases = [
            (
                "no start date",
                "startDate and endDate are required",
                lambda d: rebuild(d, startDate=None),
            ),
            (
                "no end date",
                "startDate and endDate are required",
                lambda d: rebuild(d, endDate=None),
            ),
            (
                "end before start",
                "must follow startDate",
                lambda d: rebuild(d, endDate="2026-10-01T12:00:00Z"),
            ),
            (
                "end equals start",
                "must follow startDate",
                lambda d: rebuild(d, endDate=d["sprint"]["startDate"]),
            ),
            (
                "request digest",
                "start request digest",
                lambda d: d["startRequest"].update(digest="short"),
            ),
            (
                "request requester",
                "requester required",
                lambda d: d["startRequest"].pop("requestedBy"),
            ),
            (
                "request type",
                "start request must be an object",
                lambda d: d.update(startRequest="yes"),
            ),
        ]
        for label, message, mutate in cases:
            data = start_snapshot()
            mutate(data)
            with self.subTest(case=label), self.assertRaisesRegex(ValueError, message):
                decide(data)

    def test_equivalent_date_spellings_keep_the_approved_digest(self):
        data = start_snapshot()
        data["sprint"].update(
            startDate="2026-10-19T13:00:00.000Z", endDate="2026-10-30T21:00:00+00:00"
        )
        self.assertEqual(data["scopeDigest"], GOLDEN_EXAMPLE)  # unchanged on purpose
        self.assertEqual(decide(data), ("START", []))


class CloseTests(unittest.TestCase):
    @staticmethod
    def planning_only():
        data = close_snapshot()
        data["brief"].update(release=False, review="none", reviewReason="Owner approved a planning exercise.", closureTask=None)
        del data["release"]
        for field in ("candidate", "reportDigest", "tag"):
            del data["closeApproval"][field]
        return data

    def test_the_example_is_eligible(self):
        self.assertEqual(decide(close_snapshot(), "close"), ("CLOSE", []))
        self.assertEqual(
            state.action(close_snapshot(), "close", VAULT, SPRINT, DONE), "CLOSE"
        )

    def test_each_rule_stops_the_close_with_its_own_reason(self):
        cases = [
            (
                "planned",
                ["sprint is not active"],
                lambda d: d["sprint"].update(state="planned"),
            ),
            (
                "closed",
                ["sprint is not active"],
                lambda d: d["sprint"].update(state="closed"),
            ),
            (
                "a story is not Done",
                ["members not Done: DEMO-2"],
                lambda d: d["members"][1].update(status="PO Review"),
            ),
            (
                "the closure task is not Done",
                ["members not Done: DEMO-3"],
                lambda d: d["members"][2].update(status="PO Review"),
            ),
            ("no brief", ["no approved brief"], lambda d: d.pop("brief")),
            (
                "brief sprint",
                ["brief names another sprint"],
                lambda d: d["brief"].update(sprint="S-4"),
            ),
            (
                "stale brief approval",
                ["brief approval names another scope digest"],
                lambda d: d["brief"].update(scopeDigest="e" * 64),
            ),
            (
                "closure task not a member",
                ["closure task is not a member"],
                lambda d: d["brief"].update(closureTask="DEMO-99"),
            ),
            ("no approval", ["no close approval"], lambda d: d.pop("closeApproval")),
            (
                "approval sprint",
                ["close approval names another sprint"],
                lambda d: d["closeApproval"].update(sprint="S-4"),
            ),
            (
                "approval brief",
                ["close approval names another brief"],
                lambda d: d["closeApproval"].update(briefDigest="e" * 64),
            ),
            (
                "approval scope digest",
                ["close approval names another scope digest"],
                lambda d: d["closeApproval"].update(scopeDigest="e" * 64),
            ),
            (
                "approval forbids the close",
                ["close approval does not permit closing the sprint"],
                lambda d: d["closeApproval"].update(closeSprint=False),
            ),
            (
                "approval flag missing",
                ["close approval does not permit closing the sprint"],
                lambda d: d["closeApproval"].pop("closeSprint"),
            ),
            (
                "approval flag is text",
                ["close approval does not permit closing the sprint"],
                lambda d: d["closeApproval"].update(closeSprint="true"),
            ),
            (
                "observation precedes the approval",
                ["observation precedes the close approval"],
                lambda d: d.update(observedAt="2026-10-30T14:59:59-04:00"),
            ),
            ("no release evidence", ["release evidence missing"], lambda d: d.pop("release")),
            (
                "release not ready",
                ["release is not ready"],
                lambda d: d["release"].update(ready=False),
            ),
            (
                "release ready flag is text",
                ["release is not ready"],
                lambda d: d["release"].update(ready="true"),
            ),
            (
                "approved candidate",
                ["close approval names another candidate"],
                lambda d: d["closeApproval"].update(candidate="a" * 40),
            ),
            (
                "approved report digest",
                ["close approval names another reportDigest"],
                lambda d: d["closeApproval"].update(reportDigest="e" * 64),
            ),
            (
                "approved tag",
                ["close approval names another tag"],
                lambda d: d["closeApproval"].update(tag="v9.9.9"),
            ),
            (
                "no publication",
                ["tag publication is not verified"],
                lambda d: d["release"].pop("publication"),
            ),
            (
                "publication not verified",
                ["tag publication is not verified"],
                lambda d: d["release"]["publication"].update(verified=False),
            ),
            (
                "publication verified flag is text",
                ["tag publication is not verified"],
                lambda d: d["release"]["publication"].update(verified="true"),
            ),
            (
                "published candidate",
                ["published tag differs from the release: candidate"],
                lambda d: d["release"]["publication"].update(candidate="a" * 40),
            ),
            (
                "published tag",
                ["published tag differs from the release: tag"],
                lambda d: d["release"]["publication"].update(tag="v9.9.9"),
            ),
            (
                "published tag object",
                ["published tag differs from the release: tagObject"],
                lambda d: d["release"]["publication"].update(tagObject="e" * 40),
            ),
        ]
        for label, reasons, mutate in cases:
            data = close_snapshot()
            mutate(data)
            with self.subTest(case=label):
                self.assertEqual(decide(data, "close"), ("STOP", reasons))

    def test_the_done_status_comes_from_the_caller(self):
        self.assertEqual(
            decide(close_snapshot(), "close", done="Shipped"),
            ("STOP", ["members not Done: DEMO-1, DEMO-2, DEMO-3"]),
        )

    def test_the_observation_may_equal_the_approval_instant(self):
        data = close_snapshot()
        data["observedAt"] = data["closeApproval"]["approvedAt"]
        self.assertEqual(decide(data, "close"), ("CLOSE", []))
        data["observedAt"] = "2026-10-30T19:00:00Z"  # the same instant, another offset
        self.assertEqual(decide(data, "close"), ("CLOSE", []))

    def test_members_that_differ_from_the_approved_brief_stop_the_close(self):
        data = rebuild(
            close_snapshot(), members=[*ALL_DONE, ("DEMO-4", "Done")]
        )  # approvals name the new digest, the brief still lists three members
        self.assertEqual(
            decide(data, "close"),
            (
                "STOP",
                [
                    "members differ from the approved brief "
                    "(added: DEMO-4; removed: none)"
                ],
            ),
        )

    def test_a_reapproved_scope_does_not_reuse_an_old_close_approval(self):
        data = rebuild(
            close_snapshot(), approve=False, members=[*ALL_DONE, ("DEMO-4", "Done")]
        )
        data["brief"].update(
            digest="e" * 64,
            members=["DEMO-1", "DEMO-2", "DEMO-3", "DEMO-4"],
            scopeDigest=data["scopeDigest"],
        )
        self.assertEqual(
            decide(data, "close"),
            (
                "STOP",
                [
                    "close approval names another brief",
                    "close approval names another scope digest",
                ],
            ),
        )

    def test_invalid_close_inputs_fail_instead_of_stopping(self):
        cases = [
            (
                "approval type",
                "close approval must be an object",
                lambda d: d.update(closeApproval="yes"),
            ),
            (
                "approval digest",
                "close approval digest",
                lambda d: d["closeApproval"].update(digest="short"),
            ),
            (
                "approver",
                "approver required",
                lambda d: d["closeApproval"].pop("approvedBy"),
            ),
            (
                "approval time missing",
                "timestamp required",
                lambda d: d["closeApproval"].pop("approvedAt"),
            ),
            (
                "approval time without offset",
                "offset",
                lambda d: d["closeApproval"].update(approvedAt="2026-10-30T15:00:00"),
            ),
            (
                "candidate text",
                "invalid candidate",
                lambda d: d["release"].update(candidate="xyz"),
            ),
            (
                "candidate length",
                "invalid candidate",
                lambda d: d["release"].update(candidate="a" * 41),
            ),
            (
                "candidate case",
                "invalid candidate",
                lambda d: d["release"].update(candidate="F" * 40),
            ),
            (
                "report digest length",
                "invalid reportDigest",
                lambda d: d["release"].update(reportDigest="e" * 40),
            ),
            (
                "tag object text",
                "invalid tagObject",
                lambda d: d["release"].update(tagObject="not-an-object"),
            ),
            (
                "tag name",
                "release tag required",
                lambda d: d["release"].update(tag=" "),
            ),
            (
                "unreadable member note",
                "cannot be read",
                lambda d: d.update(unreadable=["DEMO-2"]),
            ),
        ]
        for label, message, mutate in cases:
            data = close_snapshot()
            mutate(data)
            with self.subTest(case=label), self.assertRaisesRegex(ValueError, message):
                decide(data, "close")

    def test_a_sha256_candidate_is_accepted(self):
        data = close_snapshot()
        long_id = "c" * 64
        data["release"]["candidate"] = long_id
        data["release"]["report"]["candidate"] = long_id
        data["release"]["review"]["candidate"] = long_id
        data["release"]["publication"]["candidate"] = long_id
        data["closeApproval"]["candidate"] = long_id
        self.assertEqual(decide(data, "close"), ("CLOSE", []))

    def test_a_planning_only_sprint_closes_without_release_evidence(self):
        self.assertEqual(decide(self.planning_only(), "close"), ("CLOSE", []))

    def test_a_planning_only_sprint_does_not_read_release_evidence(self):
        data = self.planning_only()
        data["release"] = {"ready": False, "candidate": "not-a-commit"}
        self.assertEqual(decide(data, "close"), ("CLOSE", []))

    def test_a_planning_only_sprint_still_needs_done_members_and_the_approval(self):
        cases = [
            ("no approval", ["no close approval"], lambda d: d.pop("closeApproval")),
            (
                "scope digest",
                ["close approval names another scope digest"],
                lambda d: d["closeApproval"].update(scopeDigest="e" * 64),
            ),
            (
                "member",
                ["members not Done: DEMO-2"],
                lambda d: d["members"][1].update(status="Building"),
            ),
            (
                "permission",
                ["close approval does not permit closing the sprint"],
                lambda d: d["closeApproval"].update(closeSprint=False),
            ),
        ]
        for label, reasons, mutate in cases:
            data = self.planning_only()
            mutate(data)
            with self.subTest(case=label):
                self.assertEqual(decide(data, "close"), ("STOP", reasons))

    def test_a_close_stops_while_the_start_checks_do_not_apply(self):
        data = start_snapshot()
        decision, reasons = decide(data, "close")
        self.assertEqual(decision, "STOP")
        self.assertIn("sprint is not active", reasons)


class CommandTests(unittest.TestCase):
    def run_cli(self, intent, data=None, *, raw=None, vault=VAULT, sprint=SPRINT):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "snapshot.json"
            if raw is not None or data is not None:
                path.write_text(
                    raw if raw is not None else json.dumps(data), encoding="utf-8"
                )
            return subprocess.run(
                [
                    sys.executable,
                    "-B",
                    str(SCRIPT),
                    intent,
                    str(path),
                    "--vault",
                    vault,
                    "--sprint",
                    sprint,
                    "--done-status",
                    DONE,
                ],
                capture_output=True,
                text=True,
                check=False,
            )

    def test_progress_prints_one_json_object(self):
        result = self.run_cli("progress", start_snapshot())
        self.assertEqual((result.returncode, result.stderr), (0, ""))
        counts = json.loads(result.stdout)
        self.assertEqual(counts["scopeDigest"], GOLDEN_EXAMPLE)
        self.assertEqual(counts["total"], 3)

    def test_eligible_requests_print_start_or_close_and_exit_zero(self):
        for intent, data in (("start", start_snapshot()), ("close", close_snapshot())):
            result = self.run_cli(intent, data)
            with self.subTest(intent=intent):
                self.assertEqual(
                    (result.returncode, result.stdout, result.stderr),
                    (0, intent.upper() + "\n", ""),
                )

    def test_stop_prints_only_stop_on_stdout_and_exits_one(self):
        result = self.run_cli("close", start_snapshot())
        self.assertEqual((result.returncode, result.stdout), (1, "STOP\n"))
        self.assertIn("STOP: sprint is not active", result.stderr)
        result = self.run_cli("start", close_snapshot())
        self.assertEqual((result.returncode, result.stdout), (1, "STOP\n"))
        self.assertIn("STOP: sprint is not planned", result.stderr)
        self.assertIn("STOP: no start request", result.stderr)

    def test_invalid_input_prints_fail_and_exits_one(self):
        broken = start_snapshot()
        del broken["members"]
        cases = [
            ("wrong vault", self.run_cli("start", start_snapshot(), vault="other")),
            ("missing field", self.run_cli("start", broken)),
            ("invalid json", self.run_cli("start", raw="{")),
            ("no file", self.run_cli("start")),
        ]
        for label, result in cases:
            with self.subTest(case=label):
                self.assertEqual(result.returncode, 1)
                self.assertTrue(result.stdout.startswith("FAIL: "), result.stdout)
        self.assertEqual(cases[0][1].stdout.strip(), "FAIL: wrong vault")
        self.assertEqual(cases[1][1].stdout.strip(), "FAIL: missing field 'members'")

    def test_a_missing_argument_is_a_usage_error(self):
        result = subprocess.run(
            [sys.executable, "-B", str(SCRIPT), "start", "snapshot.json"],
            capture_output=True,
            text=True,
            check=False,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("usage", result.stderr)


class ExampleTests(unittest.TestCase):
    def test_examples_carry_the_designs_digest_and_pass_their_own_checks(self):
        for data, intent, decision in (
            (start_snapshot(), "start", "START"),
            (close_snapshot(), "close", "CLOSE"),
        ):
            sprint = data["sprint"]
            with self.subTest(intent=intent):
                self.assertEqual(
                    design_digest(
                        data["vault"],
                        sprint["key"],
                        sprint["name"],
                        sprint["goal"],
                        sprint["startDate"],
                        sprint["endDate"],
                        [member["key"] for member in data["members"]],
                    ),
                    GOLDEN_EXAMPLE,
                )
                self.assertEqual(data["scopeDigest"], GOLDEN_EXAMPLE)
                self.assertEqual(decide(data, intent)[0], decision)


if __name__ == "__main__":
    unittest.main()
