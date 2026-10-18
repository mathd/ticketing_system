"""Exercise the review-round contract without external services.

Ported from the upstream sdlc-ticket ReviewTests and SprintRoundTests. Run from
the repository root:
python3 -B -m unittest discover -s .claude/skills/sdlc-ticket/tests -v
"""

import copy
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SKILL = Path(__file__).resolve().parents[1]
PROJECT_CONFIG = SKILL.parents[1] / "sdlc.config.json"
sys.path.insert(0, str(SKILL / "scripts"))
import review_round  # noqa: E402

POLICY = {
    "severityMap": {"P0": "Critical", "P1": "High", "P2": "Medium", "P3": "Low", "P4": "Nit"}
}


def policy():
    return copy.deepcopy(POLICY)


def result(lens="initial", findings=None):
    return {
        "stage": "ai-review",
        "round": 1,
        "lens": lens,
        "artifact": "base:a/head:b",
        "reviewer": "gpt-6.1-sol",
        "complete": True,
        "verdict": "REVISE" if findings else "APPROVE",
        "summary": "Inspected the artifact.",
        "findings": findings or [],
    }


def finding(severity="Medium"):
    return {
        "id": "F1",
        "severity": severity,
        "evidence": "store.go:42",
        "impact": "A newer value is lost.",
        "correction": "Check the client revision.",
    }


def record(severity=None, round_number=1, stage="ai-review"):
    item = {
        "stage": stage,
        "round": round_number,
        "artifact": "base:a/head:b",
        "reviewer": "gpt-6.1-sol",
        "results": [
            result(findings=[finding(severity)] if severity else []),
            result("audit"),
        ],
        "dispositions": {"F1": "fixed"} if severity else {},
        "dispositionEvidence": {"F1": "Fixed at c; regression passes."}
        if severity
        else {},
        "verificationPassed": True,
    }
    for output in item["results"]:
        output["round"] = round_number
        output["stage"] = stage
        if stage == "sprint-review":
            output["area"] = "implementation"
    if stage == "sprint-review":
        item["area"] = "implementation"
    return item


AREAS = ("implementation", "verification", "documentation", "retrospective")


def sprint_record(area, severity=None, round_number=1):
    item = record(severity, round_number, stage="sprint-review")
    item["area"] = area
    for output in item["results"]:
        output["area"] = area
    return item


def sprint_records(round_number=1):
    return [sprint_record(area, round_number=round_number) for area in AREAS]


def ai_review_with_area(area):
    """A consistent ai-review record that also carries an area."""
    item = record(stage="ai-review")
    item["area"] = area
    for output in item["results"]:
        output["area"] = area
    return item


def with_decision(item, decision):
    """Return the record changed so that review_action gives `decision`."""
    if decision == "REVIEW_AGAIN":  # a High finding, fixed, still needs a round
        return sprint_record(item["area"], "High", item["round"])
    if decision == "FIX":  # a finding with no disposition yet
        changed = sprint_record(item["area"], "Low", item["round"])
        changed["dispositions"] = {}
        return changed
    if decision == "STOP":  # a reviewer that returned BLOCKED
        item["results"][0]["verdict"] = "BLOCKED"
    return item


class ConfigTests(unittest.TestCase):
    def test_project_config_carries_a_valid_severity_map(self):
        config = json.loads(PROJECT_CONFIG.read_text(encoding="utf-8"))
        review_round.validate_policy(config)

    def test_missing_or_invalid_severity_map_fails(self):
        for config in ({}, {"review": {}}, {"review": {"severityMap": {"P0": "Urgent"}}}):
            with self.subTest(config=config), self.assertRaises(ValueError):
                review_round.validate_policy(config)

    def test_example_assets_parse(self):
        for name in (
            "review-result.example.json",
            "review-round.example.json",
            "sprint-round.example.json",
        ):
            json.loads((SKILL / "assets" / name).read_text(encoding="utf-8"))


class ReviewTests(unittest.TestCase):
    def test_cli_distinguishes_valid_actions_from_invalid_records(self):
        cases = [
            (record(), "HUMAN_REVIEW", 0),
            (record("Medium"), "HUMAN_REVIEW", 0),
            (record("Medium", stage="plan-review"), "HUMAN_REVIEW", 0),
            (record("P2", 4, stage="plan-review"), "HUMAN_REVIEW", 0),
            (record("High"), "REVIEW_AGAIN", 1),
            (record("High", 4), "STOP", 1),
        ]
        incomplete = record("Low")
        incomplete["dispositions"] = {}
        cases.append((incomplete, "FIX", 1))
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "round.json"
            config = Path(directory) / "config.json"
            config.write_text(json.dumps({"review": POLICY}), encoding="utf-8")
            command = [
                sys.executable,
                "-B",
                str(SKILL / "scripts/review_round.py"),
                "review",
                str(path),
                "--config",
                str(config),
                "--artifact",
                "base:a/head:b",
                "--reviewer",
                "gpt-6.1-sol",
                "--author",
                "opus-5.5",
            ]
            for candidate, expected, code in cases:
                path.write_text(json.dumps(candidate), encoding="utf-8")
                completed = subprocess.run(
                    command, capture_output=True, text=True, check=False
                )
                with self.subTest(expected=expected):
                    self.assertEqual(completed.returncode, code)
                    self.assertEqual(completed.stdout.strip(), expected)
            path.write_text('{"results":', encoding="utf-8")
            completed = subprocess.run(
                command, capture_output=True, text=True, check=False
            )
            self.assertEqual(completed.returncode, 1)
            self.assertTrue(completed.stdout.startswith("FAIL:"))
            path.write_text(json.dumps(record(round_number=5)), encoding="utf-8")
            completed = subprocess.run(
                [*command, "--authorized-through", "5"],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 1)
            self.assertIn("authorization", completed.stdout)

    def test_expected_target_and_all_authors_are_checked(self):
        candidate = record()
        review_round.validate_review_target(
            candidate, "base:a/head:b", "gpt-6.1-sol", ["opus-5.5"]
        )
        for artifact, reviewer, authors in [
            ("older", "gpt-6.1-sol", ["author"]),
            ("base:a/head:b", "wrong-model", ["author"]),
            ("base:a/head:b", "gpt-6.1-sol", ["author", "gpt-6.1-sol"]),
        ]:
            with (
                self.subTest(artifact=artifact, reviewer=reviewer),
                self.assertRaises(ValueError),
            ):
                review_round.validate_review_target(
                    candidate, artifact, reviewer, authors
                )

    def test_extra_round_requires_explicit_extended_budget(self):
        candidate = record(round_number=5)
        with self.assertRaisesRegex(ValueError, "budget"):
            review_round.review_action(candidate, policy())
        self.assertEqual(
            review_round.review_action(candidate, policy(), authorized_through=5),
            "HUMAN_REVIEW",
        )

    def test_complete_zero_findings_can_handoff(self):
        self.assertEqual(review_round.review_action(record(), policy()), "HUMAN_REVIEW")

    def test_every_high_severity_requires_review_even_when_fixed(self):
        for stage in ("plan-review", "ai-review", "sprint-review"):
            for severity in ("Critical", "High", "P0", "P1"):
                for number, expected in ((1, "REVIEW_AGAIN"), (4, "STOP")):
                    with self.subTest(stage=stage, severity=severity, round=number):
                        self.assertEqual(
                            review_round.review_action(
                                record(severity, number, stage=stage), policy()
                            ),
                            expected,
                        )

    def test_verified_noncritical_fixes_do_not_repeat_deliverable_review(self):
        for stage in ("plan-review", "ai-review", "sprint-review"):
            for severity in ("Medium", "Low", "Nit", "P2", "P3", "P4"):
                for number in (1, 4):
                    with self.subTest(stage=stage, severity=severity, round=number):
                        candidate = record(severity, number, stage=stage)
                        candidate["materialChange"] = False
                        self.assertEqual(
                            review_round.review_action(candidate, policy()),
                            "HUMAN_REVIEW",
                        )

    def test_medium_plan_finding_still_needs_disposition_and_verification(self):
        for severity in ("Medium", "P2"):
            with self.subTest(severity=severity):
                candidate = record(severity, stage="plan-review")
                candidate["dispositions"] = {}
                self.assertEqual(review_round.review_action(candidate, policy()), "FIX")
                candidate = record(severity, stage="plan-review")
                candidate["verificationPassed"] = False
                self.assertEqual(review_round.review_action(candidate, policy()), "FIX")

    def test_material_change_requires_review_without_severity_trigger(self):
        for stage in ("plan-review", "ai-review", "sprint-review"):
            candidate = record("Low", stage=stage)
            candidate["materialChange"] = True
            self.assertEqual(
                review_round.review_action(candidate, policy()), "REVIEW_AGAIN"
            )
            candidate = record("Medium", 4, stage=stage)
            candidate["materialChange"] = True
            self.assertEqual(review_round.review_action(candidate, policy()), "STOP")

    def test_refutation_needs_independent_revision_bound_confirmation(self):
        candidate = record("High", 4)
        candidate["dispositions"]["F1"] = "rejected"
        confirmation = {
            "artifact": candidate["artifact"],
            "reviewer": candidate["reviewer"],
            "complete": True,
            "refuted": True,
            "evidence": "The caller rejects this input before any write.",
        }
        candidate["refutationChecks"] = {"F1": confirmation}
        self.assertEqual(
            review_round.review_action(candidate, policy()), "HUMAN_REVIEW"
        )
        for field, value in (
            ("artifact", "another-head"),
            ("reviewer", "author-model"),
            ("complete", False),
            ("refuted", "true"),
            ("evidence", ""),
        ):
            invalid = copy.deepcopy(candidate)
            invalid["refutationChecks"]["F1"][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                review_round.review_action(invalid, policy())

    def test_negative_refutation_is_retained_after_the_defect_is_fixed(self):
        candidate = record("High")
        candidate["dispositions"]["F1"] = "rejected"
        candidate["refutationChecks"] = {
            "F1": {
                "artifact": candidate["artifact"],
                "reviewer": candidate["reviewer"],
                "complete": True,
                "refuted": False,
                "evidence": "The claimed guard does not cover this caller.",
            }
        }
        self.assertEqual(review_round.review_action(candidate, policy()), "STOP")
        candidate["dispositions"]["F1"] = "fixed"
        candidate["dispositionEvidence"]["F1"] = "Fixed at c; regression passes."
        self.assertEqual(
            review_round.review_action(candidate, policy()), "REVIEW_AGAIN"
        )

    def test_refutation_cannot_hide_a_fix_or_another_finding(self):
        candidate = record("High")
        candidate["refutationChecks"] = {
            "F1": {
                "artifact": candidate["artifact"],
                "reviewer": candidate["reviewer"],
                "complete": True,
                "refuted": True,
                "evidence": "Verified refutation.",
            }
        }
        with self.assertRaisesRegex(ValueError, "rejected finding"):
            review_round.review_action(candidate, policy())
        candidate["dispositions"]["F1"] = "rejected"
        candidate["refutationChecks"]["unknown"] = candidate["refutationChecks"].pop(
            "F1"
        )
        with self.assertRaisesRegex(ValueError, "rejected finding"):
            review_round.review_action(candidate, policy())
        candidate["refutationChecks"]["F1"] = candidate["refutationChecks"].pop(
            "unknown"
        )
        other = finding("High")
        other["id"] = "F2"
        candidate["results"][1] = result("audit", [other])
        candidate["dispositions"]["F2"] = "fixed"
        candidate["dispositionEvidence"]["F2"] = "Fixed at c; targeted check passes."
        self.assertEqual(
            review_round.review_action(candidate, policy()), "REVIEW_AGAIN"
        )

    def test_cap_stops_and_does_not_reset_for_rejected_findings(self):
        candidate = record("High", 4)
        candidate["dispositions"]["F1"] = "rejected"
        self.assertEqual(review_round.review_action(candidate, policy()), "STOP")

    def test_audit_finding_counts(self):
        candidate = record()
        candidate["results"][1] = result("audit", [finding("High")])
        candidate["dispositions"] = {"F1": "fixed"}
        candidate["dispositionEvidence"] = {"F1": "Verified fix c."}
        self.assertEqual(
            review_round.review_action(candidate, policy()), "REVIEW_AGAIN"
        )

    def test_invalid_results_fail_closed(self):
        mutations = [
            lambda r: r["results"].pop(),
            lambda r: r["results"][0].update(complete=False),
            lambda r: r["results"][0].update(artifact="older-revision"),
            lambda r: r["results"][0].update(reviewer="author"),
            lambda r: r["results"][0].update(summary=""),
            lambda r: r["results"][0].update(findings="truncated"),
            lambda r: r["results"][0].update(lens="audit"),
            lambda r: r.update(stage="sprint-review"),
            lambda r: r.update(materialChange="false"),
            lambda r: r.update(refutationChecks=[]),
        ]
        for change in mutations:
            candidate = record()
            change(candidate)
            with self.subTest(change=change), self.assertRaises(ValueError):
                review_round.review_action(candidate, policy())

    def test_open_research_requests_are_not_a_complete_result(self):
        candidate = record()
        candidate["results"][0]["researchRequests"] = [
            {"id": "Q1", "question": "Who calls store.Save?", "scope": "services/"}
        ]
        with self.assertRaisesRegex(ValueError, "research requests"):
            review_round.review_action(candidate, policy())
        candidate["results"][0]["researchRequests"] = "none"
        with self.assertRaisesRegex(ValueError, "research requests"):
            review_round.review_action(candidate, policy())
        candidate["results"][0]["researchRequests"] = []
        self.assertEqual(review_round.review_action(candidate, policy()), "HUMAN_REVIEW")

    def test_finding_ids_are_unique_across_lenses(self):
        candidate = record("Low")
        candidate["results"][1] = result("audit", [finding("Low")])
        with self.assertRaisesRegex(ValueError, "unique"):
            review_round.review_action(candidate, policy())

    def test_revise_without_findings_is_refused(self):
        candidate = record()
        candidate["results"][1]["verdict"] = "REVISE"
        with self.assertRaisesRegex(ValueError, "REVISE requires findings"):
            review_round.review_action(candidate, policy())

    def test_result_round_must_match_the_record(self):
        candidate = record(round_number=2)
        candidate["results"][1]["round"] = 1
        with self.assertRaisesRegex(ValueError, "mismatch"):
            review_round.review_action(candidate, policy())

    def test_unresolved_or_unverified_never_handoffs(self):
        candidate = record("Medium")
        candidate["dispositions"] = {}
        self.assertEqual(review_round.review_action(candidate, policy()), "FIX")
        candidate = record()
        candidate["verificationPassed"] = False
        self.assertEqual(review_round.review_action(candidate, policy()), "FIX")
        candidate = record()
        candidate["results"][0]["verdict"] = "BLOCKED"
        self.assertEqual(review_round.review_action(candidate, policy()), "STOP")

    def test_custom_priority_map_is_used(self):
        custom = policy()
        custom["severityMap"]["P3"] = "High"
        self.assertEqual(review_round.review_action(record("P3"), custom), "REVIEW_AGAIN")
        with self.assertRaisesRegex(ValueError, "severity"):
            review_round.review_action(record("P9"), custom)

    def test_unknown_stage_is_refused(self):
        # "sprint-review" is a valid stage now, so the old invalid-stage mutation in
        # test_invalid_results_fail_closed no longer reaches this rule.
        for stage in ("deploy-review", "", None):
            candidate = record()
            candidate["stage"] = stage
            for output in candidate["results"]:
                output["stage"] = stage
            with (
                self.subTest(stage=stage),
                self.assertRaisesRegex(ValueError, "invalid review stage"),
            ):
                review_round.review_action(candidate, policy())

    def test_round_must_be_inside_the_budget(self):
        for number in (0, -1, 5, True, 2.0):
            candidate = record(round_number=number)
            with (
                self.subTest(round=number),
                self.assertRaisesRegex(ValueError, "budget"),
            ):
                review_round.review_action(candidate, policy())
        for budget in (3, 0, 4.0):
            with (
                self.subTest(budget=budget),
                self.assertRaisesRegex(ValueError, "budget"),
            ):
                review_round.review_action(record(), policy(), authorized_through=budget)

    def test_result_stage_must_match_the_record(self):
        candidate = record()
        candidate["results"][0]["stage"] = "plan-review"
        with self.assertRaisesRegex(ValueError, "mismatch"):
            review_round.review_action(candidate, policy())

    def test_artifact_and_reviewer_are_required_even_when_the_results_agree(self):
        for field in ("artifact", "reviewer"):
            candidate = record()
            candidate[field] = ""
            for output in candidate["results"]:
                output[field] = ""
            with (
                self.subTest(field=field),
                self.assertRaisesRegex(ValueError, "artifact and reviewer required"),
            ):
                review_round.review_action(candidate, policy())

    def test_unknown_verdict_is_refused(self):
        for verdict in ("MAYBE", "", None):
            candidate = record()
            candidate["results"][0]["verdict"] = verdict
            with (
                self.subTest(verdict=verdict),
                self.assertRaisesRegex(ValueError, "verdict"),
            ):
                review_round.review_action(candidate, policy())

    def test_unresolved_findings_at_the_cap_stop_instead_of_asking_for_a_fix(self):
        candidate = record("Medium", 4)
        candidate["dispositions"] = {}
        self.assertEqual(review_round.review_action(candidate, policy()), "STOP")
        candidate = record("Medium", 3)
        candidate["dispositions"] = {}
        self.assertEqual(review_round.review_action(candidate, policy()), "FIX")

    def test_approval_cannot_hide_material_finding(self):
        for level in ("High", "Medium"):
            candidate = record(level)
            candidate["results"][0]["verdict"] = "APPROVE"
            with self.subTest(level=level), self.assertRaises(ValueError):
                review_round.review_action(candidate, policy())


class SprintReviewTests(unittest.TestCase):
    def test_sprint_review_requires_both_lenses_and_uses_deliverable_policy(self):
        candidate = record("Medium", stage="sprint-review")
        self.assertEqual(review_round.review_action(candidate, policy()), "HUMAN_REVIEW")
        candidate["results"].pop()
        with self.assertRaisesRegex(ValueError, "initial review and audit"):
            review_round.review_action(candidate, policy())

    def test_sprint_review_repeats_for_critical_and_high_only(self):
        for severity, expected in (
            ("Critical", "REVIEW_AGAIN"),
            ("High", "REVIEW_AGAIN"),
            ("P1", "REVIEW_AGAIN"),
            ("Medium", "HUMAN_REVIEW"),
            ("Low", "HUMAN_REVIEW"),
        ):
            candidate = record(severity, stage="sprint-review")
            with self.subTest(severity=severity):
                self.assertEqual(
                    review_round.review_action(candidate, policy()), expected
                )

    def test_sprint_review_stops_at_the_cap_and_needs_an_extended_budget(self):
        self.assertEqual(
            review_round.review_action(record("High", 4, "sprint-review"), policy()),
            "STOP",
        )
        candidate = record(round_number=5, stage="sprint-review")
        with self.assertRaisesRegex(ValueError, "budget"):
            review_round.review_action(candidate, policy())
        self.assertEqual(
            review_round.review_action(candidate, policy(), authorized_through=5),
            "HUMAN_REVIEW",
        )

    def test_a_sprint_record_must_name_a_known_area(self):
        for change in (
            lambda r: r.pop("area"),
            lambda r: r.update(area=None),
            lambda r: r.update(area="security"),
        ):
            candidate = sprint_record("implementation")
            change(candidate)
            with self.assertRaisesRegex(ValueError, "area required"):
                review_round.review_action(candidate, policy())

    def test_sprint_area_must_match_both_lenses_and_expected_target(self):
        candidate = sprint_record("implementation")
        review_round.validate_review_target(
            candidate, "base:a/head:b", "gpt-6.1-sol", ["opus-5.5"], "implementation"
        )
        for area in ("verification", None, "security"):
            with (
                self.subTest(area=area),
                self.assertRaisesRegex(ValueError, "unexpected sprint review area"),
            ):
                review_round.validate_review_target(
                    candidate, "base:a/head:b", "gpt-6.1-sol", ["opus-5.5"], area
                )
        unknown = sprint_record("implementation")
        unknown["area"] = "security"
        with self.assertRaisesRegex(ValueError, "unexpected sprint review area"):
            review_round.validate_review_target(
                unknown, "base:a/head:b", "gpt-6.1-sol", ["opus-5.5"], "security"
            )
        for lens in (0, 1):
            wrong = copy.deepcopy(candidate)
            wrong["results"][lens]["area"] = "verification"
            absent = copy.deepcopy(candidate)
            del absent["results"][lens]["area"]
            for changed in (wrong, absent):
                with (
                    self.subTest(lens=lens),
                    self.assertRaisesRegex(ValueError, "area mismatch"),
                ):
                    review_round.review_action(changed, policy())

    def test_other_stages_do_not_need_an_area(self):
        for stage in ("plan-review", "ai-review"):
            self.assertEqual(
                review_round.review_action(record(stage=stage), policy()),
                "HUMAN_REVIEW",
            )

    def test_cli_checks_the_area_of_a_sprint_record(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "round.json"
            config = Path(directory) / "config.json"
            config.write_text(json.dumps({"review": POLICY}), encoding="utf-8")
            path.write_text(json.dumps(sprint_record("verification")), encoding="utf-8")
            command = [
                sys.executable,
                "-B",
                str(SKILL / "scripts/review_round.py"),
                "review",
                str(path),
                "--config",
                str(config),
                "--artifact",
                "base:a/head:b",
                "--reviewer",
                "gpt-6.1-sol",
                "--author",
                "opus-5.5",
            ]
            for extra, code, expected in (
                (["--area", "verification"], 0, "HUMAN_REVIEW"),
                ([], 1, "FAIL: unexpected sprint review area"),
                (["--area", "implementation"], 1, "FAIL: unexpected sprint review area"),
            ):
                completed = subprocess.run(
                    [*command, *extra], capture_output=True, text=True, check=False
                )
                with self.subTest(extra=extra):
                    self.assertEqual(completed.returncode, code)
                    self.assertEqual(completed.stdout.strip(), expected)
            completed = subprocess.run(
                [*command, "--area", "security"],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 2)


class SprintRoundTests(unittest.TestCase):
    REVIEWERS = dict.fromkeys(AREAS, "gpt-6.1-sol")

    def action(self, records, reviewers=None, authors=("opus-5.5",), authorized=4):
        return review_round.sprint_round_action(
            records,
            policy(),
            "base:a/head:b",
            reviewers or dict(self.REVIEWERS),
            list(authors),
            authorized,
        )

    def test_full_shared_round_is_required(self):
        records = sprint_records()
        self.assertEqual(self.action(records), "HUMAN_REVIEW")
        for label, change in (
            ("one area missing", lambda values: values.pop()),
            ("one area twice", lambda values: values.__setitem__(1, copy.deepcopy(values[0]))),
            ("five records", lambda values: values.append(copy.deepcopy(values[0]))),
            ("record round only", lambda values: values[0].update(round=2)),
            (
                "one area from another round",
                lambda values: values.__setitem__(
                    0, sprint_record("implementation", round_number=2)
                ),
            ),
            ("another artifact", lambda values: values[0].update(artifact="other")),
            ("not an object", lambda values: values.__setitem__(0, "implementation")),
            ("record stage only", lambda values: values[0].update(stage="ai-review")),
            (
                "one area from another stage",
                lambda values: values.__setitem__(0, ai_review_with_area("implementation")),
            ),
        ):
            candidate = copy.deepcopy(records)
            change(candidate)
            with self.subTest(case=label), self.assertRaises(ValueError):
                self.action(candidate)
        with self.assertRaisesRegex(ValueError, "four sprint review records"):
            self.action({"implementation": records[0]})

    def test_the_round_names_exactly_four_expected_reviewers(self):
        for reviewers in (
            {area: "gpt-6.1-sol" for area in AREAS[:3]},
            {**self.REVIEWERS, "security": "gpt-6.1-sol"},
            {"implementation": "a", "verification": "a", "documentation": "a", "security": "a"},
        ):
            with (
                self.subTest(reviewers=sorted(reviewers)),
                self.assertRaisesRegex(ValueError, "four expected sprint reviewers"),
            ):
                self.action(sprint_records(), reviewers)

    def test_each_area_uses_its_own_expected_reviewer(self):
        models = dict(zip(AREAS, ("gpt-6.1-sol", "gpt-6-astra", "gpt-6-luna", "gemini-3.8-flash-high"), strict=True))
        records = sprint_records()
        for item in records:
            item["reviewer"] = models[item["area"]]
            for output in item["results"]:
                output["reviewer"] = models[item["area"]]
        self.assertEqual(self.action(records, models), "HUMAN_REVIEW")
        swapped = {**models, "implementation": models["verification"]}
        with self.assertRaisesRegex(ValueError, "reviewer identity"):
            self.action(records, swapped)

    def test_reviewers_must_be_independent_of_every_author(self):
        for authors in (("gpt-6.1-sol",), ("opus-5.5", "gpt-6.1-sol")):
            with (
                self.subTest(authors=authors),
                self.assertRaisesRegex(ValueError, "independent"),
            ):
                self.action(sprint_records(), authors=authors)
        with self.assertRaisesRegex(ValueError, "author identities"):
            self.action(sprint_records(), authors=())

    def test_the_most_restrictive_area_decides(self):
        order = ("STOP", "FIX", "REVIEW_AGAIN", "HUMAN_REVIEW")
        for first in order:
            for second in order:
                records = sprint_records()
                records[0] = with_decision(records[0], first)
                records[2] = with_decision(records[2], second)
                expected = min(first, second, key=order.index)
                with self.subTest(first=first, second=second):
                    self.assertEqual(self.action(records), expected)
                    records.reverse()
                    self.assertEqual(self.action(records), expected)

    def test_the_cap_and_the_extended_budget_cover_the_whole_round(self):
        records = sprint_records(round_number=4)
        records[3] = with_decision(records[3], "REVIEW_AGAIN")
        self.assertEqual(self.action(records), "STOP")
        records = sprint_records(round_number=5)
        with self.assertRaisesRegex(ValueError, "budget"):
            self.action(records)
        self.assertEqual(self.action(records, authorized=5), "HUMAN_REVIEW")

    def test_an_invalid_area_pair_fails_the_whole_round(self):
        records = sprint_records()
        records[1]["results"][0]["complete"] = False
        with self.assertRaisesRegex(ValueError, "incomplete"):
            self.action(records)

    def test_the_shipped_example_validates(self):
        example = json.loads(
            (SKILL / "assets/sprint-round.example.json").read_text(encoding="utf-8")
        )
        reviewers = {record_["area"]: record_["reviewer"] for record_ in example}
        self.assertEqual(
            review_round.sprint_round_action(
                example, policy(), example[0]["artifact"], reviewers, ["example-author"]
            ),
            "HUMAN_REVIEW",
        )

    def test_aggregate_cli_checks_targets_and_combines_decisions(self):
        records = sprint_records()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "round.json"
            config = Path(directory) / "config.json"
            config.write_text(json.dumps({"review": POLICY}), encoding="utf-8")
            command = [
                sys.executable,
                "-B",
                str(SKILL / "scripts/review_round.py"),
                "sprint-round",
                str(path),
                "--config",
                str(config),
                "--artifact",
                "base:a/head:b",
                "--author",
                "opus-5.5",
            ]
            for area in AREAS:
                command += ["--reviewer", area + "=gpt-6.1-sol"]

            def run(args=command):
                return subprocess.run(args, capture_output=True, text=True, check=False)

            path.write_text(json.dumps(records), encoding="utf-8")
            result = run()
            self.assertEqual(
                (result.returncode, result.stdout.strip()), (0, "HUMAN_REVIEW")
            )
            result = run([*command, "--author", "gpt-6.1-sol"])
            self.assertEqual(result.returncode, 1)
            self.assertIn("independent", result.stdout)
            for values in (records[:-1], [*records[:-1], copy.deepcopy(records[0])]):
                path.write_text(json.dumps(values), encoding="utf-8")
                result = run()
                with self.subTest(records=len(values)):
                    self.assertEqual(result.returncode, 1)
                    self.assertTrue(result.stdout.startswith("FAIL:"))
            path.write_text(json.dumps(records), encoding="utf-8")
            fewer = command[:]
            index = fewer.index("implementation=gpt-6.1-sol")
            del fewer[index - 1 : index + 1]
            result = run(fewer)
            self.assertEqual(result.returncode, 1)
            self.assertIn("four AREA=reviewer bindings", result.stdout)
            unbound = [part.replace("verification=", "verification:") for part in command]
            result = run(unbound)
            self.assertEqual(result.returncode, 1)
            self.assertIn("four AREA=reviewer bindings", result.stdout)
            records[0]["materialChange"] = True
            records[1]["verificationPassed"] = False
            path.write_text(json.dumps(records), encoding="utf-8")
            result = run()
            self.assertEqual((result.returncode, result.stdout.strip()), (1, "FIX"))
            path.write_text(json.dumps(sprint_records(round_number=5)), encoding="utf-8")
            result = run([*command, "--authorized-through", "5"])
            self.assertEqual(result.returncode, 1)
            self.assertIn("authorization", result.stdout)
            result = run(
                [*command, "--authorized-through", "5", "--authorization", "S-3 note"]
            )
            self.assertEqual(
                (result.returncode, result.stdout.strip()), (0, "HUMAN_REVIEW")
            )
            path.write_text("{}", encoding="utf-8")
            result = run()
            self.assertEqual(result.returncode, 1)
            self.assertTrue(result.stdout.startswith("FAIL:"))


if __name__ == "__main__":
    unittest.main()
