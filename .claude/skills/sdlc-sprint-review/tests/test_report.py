"""Exercise report safety and evidence consistency without external services.

Each rule is tested at its own validator and by its own message, so a later rule
that raises the same exception type cannot hide a missing one. Markup is checked
at the position where a value must appear, not anywhere in the page.

Run from the repository root:
python3 -B -m unittest discover -s .claude/skills/sdlc-sprint-review/tests -v
"""

import copy
import hashlib
import importlib.util
import json
import re
import subprocess
import sys
import tempfile
import unittest
from html.parser import HTMLParser
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/render_report.py"
spec = importlib.util.spec_from_file_location("sdlc_sprint_report", SCRIPT)
assert spec is not None
assert spec.loader is not None
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)

REPORT_URL = (
    "https://git.example.com/demo/invitations/blob/"
    "f1e2d3c4b5a6978877665544332211ffeeddccbb/docs/sprints/S-3/report.en.html"
)
ATTACKS = (
    "javascript:alert(1)",
    "data:text/html,test",
    "//example.com",
    "file:///tmp/evidence",
    "ftp://example.com/file",
    "https://",
    "https://user:password@example.com",
    "https://user@example.com",
    "https://:password@example.com",
    "https://example.com/\nattack",
    "https://example.com/a b",
    "https://example.com/\x01",
    "",
    " ",
    None,
    5,
)


def example(language="en"):
    return json.loads((ROOT / f"assets/report.example.{language}.json").read_text())


def decided(data, status="approved"):
    """Record a human decision on the example, naming the exact targets."""
    data["decision"]["approval"].update(
        status=status,
        approver="Fictional Human",
        decidedAt="2026-10-29T16:00:00-04:00",
        ref={"key": "S-3", "digest": "d" * 64},
        sprint=data["sprint"]["key"],
        scopeDigest=data["sprint"]["scopeDigest"],
        candidate=data["release"]["candidate"]["sha"],
        reportDigest="a" * 64,
        reportUrl=REPORT_URL,
        tag=data["release"]["tag"],
    )
    return data


def published(data, verified_at="2026-10-29T16:05:00-04:00"):
    """Record a verified tag publication on a decided example."""
    data["decision"]["tagPublication"].update(
        status="published",
        object="b" * 40,
        verifiedAt=verified_at,
        tag=data["release"]["tag"],
        candidate=data["release"]["candidate"]["sha"],
        evidence=[{"label": "Remote tag", "url": "https://git.example.com/tags/v1.4.0"}],
    )
    return data


def section(output, name):
    """The HTML of one top-level section, by its id."""
    start = output.index(f'<section id="{name}"')
    return output[start : output.index("</section>", start)]


def fact(html, label):
    """Every <dd> that follows a <dt> with this label, as raw HTML."""
    return re.findall(
        r"<dt>" + re.escape(label) + r"</dt><dd>(.*?)</dd>", html, flags=re.DOTALL
    )


class Structure(HTMLParser):
    def __init__(self):
        super().__init__()
        self.ids = []
        self.targets = []
        self.scripts = []
        self.dependencies = []

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if "id" in attrs:
            self.ids.append(attrs["id"])
        if attrs.get("href", "").startswith("#"):
            self.targets.append(attrs["href"][1:])
        if tag == "script" or any(key.startswith("on") for key in attrs):
            self.scripts.append((tag, attrs))
        if tag in {"link", "iframe", "img", "object", "embed"}:
            self.dependencies.append((tag, attrs))


class RuleCase(unittest.TestCase):
    def refuses(self, validator, mutate, message, base=example):
        data = base()
        mutate(data)
        with self.assertRaisesRegex(ValueError, message):
            validator(data)

    def table(self, validator, rows, base=example):
        for label, mutate, message in rows:
            with self.subTest(case=label):
                self.refuses(validator, mutate, message, base)


class ExampleTests(unittest.TestCase):
    def test_both_examples_render_all_sections_without_dependencies(self):
        for language, label in (
            ("en", "Technical readiness"),
            ("fr", "Préparation technique"),
        ):
            data = example(language)
            output, digest = report.render(data)
            self.assertIn(f'<html lang="{language}">', output)
            self.assertIn(label, output)
            self.assertIn("example-reviewer-a", output)
            self.assertIn("default-src 'none'", output)
            self.assertIn("@media print", output)
            self.assertIn(
                '<label for="theme-dark">'
                + report.LABELS[language]["theme_dark"]
                + "</label>",
                output,
            )
            self.assertIn(digest, output)
            parser = Structure()
            parser.feed(output)
            self.assertFalse(parser.scripts or parser.dependencies)
            self.assertEqual(len(parser.ids), len(set(parser.ids)))
            self.assertTrue(set(parser.targets) <= set(parser.ids))
            self.assertLessEqual(
                {
                    "outcome",
                    "decision",
                    "scope",
                    "risks",
                    "reviews",
                    "checks",
                    "measurements",
                    "retrospective",
                    "identity",
                },
                set(parser.targets),
            )
            self.assertIsNone(re.search(r"\$(?:l_|scope_|areas_|check_rows_)", output))

    def test_both_languages_carry_the_same_copy_keys(self):
        self.assertEqual(set(report.LABELS["en"]), set(report.LABELS["fr"]))
        self.assertEqual(set(report.ENUMS["en"]), set(report.ENUMS["fr"]))

    def test_the_examples_and_labels_use_no_upstream_field_names(self):
        for language in ("en", "fr"):
            text = json.dumps(example(language)) + json.dumps(report.COPY)
            self.assertIsNone(
                re.search(r"(?i)attachment|boardId|sprintId|closureIssue|native", text)
            )

    def test_text_and_attributes_cannot_inject_markup(self):
        data = example()
        attack = '<img src=x onerror="alert(1)"> & $l_goal'
        data["sprint"]["name"] = attack
        data["outcome"]["summary"] = attack
        data["areas"]["implementation"]["evidence"][0]["label"] = attack
        data["areas"]["implementation"]["evidence"][1]["url"] = (
            'https://example.com/?q="onclick="alert(1)'
        )
        output, _ = report.render(data)
        self.assertIn("&lt;img", output)
        self.assertIn("$l_goal", output)
        self.assertNotIn("<img", output)
        parser = Structure()
        parser.feed(output)
        self.assertFalse(parser.scripts or parser.dependencies)

    def test_a_board_reference_cannot_inject_markup(self):
        data = example()
        data["areas"]["implementation"]["evidence"][0]["ref"]["key"] = "<b>S-3</b>"
        output, _ = report.render(data)
        self.assertIn("&lt;b&gt;S-3&lt;/b&gt;", output)
        self.assertNotIn("<b>S-3</b>", output)

    def test_partial_evidence_can_be_reported_without_becoming_ready(self):
        data = example("fr")
        output, _ = report.render(data)
        self.assertIn("Bloquée", output)
        self.assertIn("Non demandée", output)
        self.assertIn("Non consigné", output)
        data["checks"][3].update(status="not_run", cost=None)
        data["risks"] = []
        data["outcome"]["readiness"] = "incomplete"
        report.render(data)

    def test_optional_fields_cost_currencies_and_null_evidence(self):
        data = example()
        data["report"].pop("fictional")
        data["release"]["base"]["ref"] = None
        data["checks"][0]["cost"] = {"amount": 2.0, "currency": "CAD"}
        data["checks"][0]["evidence"].append(
            {"label": "Additional offline context", "url": None}
        )
        output, _ = report.render(data)
        self.assertIn("CAD 2.00 + USD 1.86", output.replace(" ", " "))
        self.assertNotIn('class="fictional"', output)
        data["report"]["language"] = "fr"
        self.assertIn(
            "2,00 CAD + 1,86 USD", report.render(data)[0].replace(" ", " ")
        )

    def test_the_fictional_banner_shows_only_for_a_fictional_report(self):
        output, _ = report.render(example())
        self.assertIn('<p class="fictional">', output)

    def test_subcent_costs_and_partial_review_metadata_remain_visible(self):
        data = example("fr")
        data["checks"][3]["cost"].update(amount=0.004, budget=0.003)
        data["areas"]["documentation"].update(
            round=1,
            reviewers=["example-reviewer-b"],
            findings={"raised": 1, "resolved": 0, "deferred": 0},
        )
        output, _ = report.render(data)
        self.assertGreaterEqual(output.replace(" ", " ").count("0,004 USD"), 3)
        self.assertNotIn("0.00 USD", output)
        self.assertIn(report.LABELS["fr"]["budget_exceeded"], output)
        self.assertIn("example-reviewer-b", output)
        self.assertEqual(
            report.paragraphs("First\r\n\r\nSecond"),
            report.paragraphs("First\n\nSecond"),
        )
        with self.assertRaisesRegex(ValueError, "duplicate JSON key"):
            json.loads(
                '{"readiness":"ready","readiness":"blocked"}',
                object_pairs_hook=report.unique_object,
            )

    def test_the_source_digest_of_a_direct_call_is_canonical_json(self):
        data = example()
        _, digest = report.render(data)
        canonical = json.dumps(
            data,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
            allow_nan=False,
        ).encode()
        self.assertEqual(digest, hashlib.sha256(canonical).hexdigest())
        with self.assertRaisesRegex(ValueError, "source bytes differ"):
            report.render(data, source_bytes=b'{"schemaVersion": 1}')


class CommandTests(unittest.TestCase):
    def run_cli(self, source, output):
        return subprocess.run(
            [
                sys.executable,
                "-B",
                str(SCRIPT),
                "--input",
                str(source),
                "--output",
                str(output),
            ],
            capture_output=True,
            text=True,
            check=False,
        )

    def test_cli_hashes_exact_source_and_output_and_refuses_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "report.html"
            source = ROOT / "assets/report.example.en.json"
            result = self.run_cli(source, output)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            receipt = json.loads(result.stdout)
            self.assertEqual(
                receipt["sourceDigest"], hashlib.sha256(source.read_bytes()).hexdigest()
            )
            self.assertEqual(
                receipt["htmlDigest"], hashlib.sha256(output.read_bytes()).hexdigest()
            )
            self.assertNotEqual(receipt["htmlDigest"], receipt["sourceDigest"])
            self.assertEqual(receipt["output"], str(output))
            original = output.read_bytes()
            self.assertEqual(self.run_cli(source, output).returncode, 1)
            self.assertEqual(output.read_bytes(), original)

    def test_cli_refuses_an_invalid_source_without_writing_a_file(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "report.json"
            output = Path(directory) / "report.html"
            data = example()
            data["outcome"]["readiness"] = "blocked"
            source.write_text(json.dumps(data), encoding="utf-8")
            result = self.run_cli(source, output)
            self.assertEqual(result.returncode, 1)
            self.assertTrue(result.stdout.startswith("FAIL: readiness contradicts"))
            self.assertFalse(output.exists())

    def test_duplicate_json_keys_are_refused_by_the_command(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "report.json"
            text = (ROOT / "assets/report.example.en.json").read_text()
            source.write_text(
                text.replace('"version": 2,', '"version": 2, "version": 3,', 1),
                encoding="utf-8",
            )
            result = self.run_cli(source, Path(directory) / "report.html")
            self.assertEqual(result.returncode, 1)
            self.assertIn("duplicate JSON key: version", result.stdout)

    def test_a_missing_source_file_is_a_failure_not_a_traceback(self):
        with tempfile.TemporaryDirectory() as directory:
            result = self.run_cli(Path(directory) / "none.json", Path(directory) / "x.html")
            self.assertEqual(result.returncode, 1)
            self.assertTrue(result.stdout.startswith("FAIL:"))


class FieldTests(RuleCase):
    """Every documented key is required unless it is marked optional."""

    OBJECTS = (
        ("top level", lambda d: d, "validate_identity", set()),
        ("report", lambda d: d["report"], "validate_identity", {"fictional", "packetDigest"}),
        ("project", lambda d: d["project"], "validate_identity", set()),
        ("sprint", lambda d: d["sprint"], "validate_identity", set()),
        ("release", lambda d: d["release"], "validate_identity", set()),
        ("base", lambda d: d["release"]["base"], "validate_identity", set()),
        ("candidate", lambda d: d["release"]["candidate"], "validate_identity", set()),
        ("outcome", lambda d: d["outcome"], "validate_identity", set()),
        ("areas", lambda d: d["areas"], "validate_reviews", set()),
        ("area", lambda d: d["areas"]["implementation"], "validate_reviews", set()),
        (
            "finding counts",
            lambda d: d["areas"]["implementation"]["findings"],
            "validate_reviews",
            set(),
        ),
        (
            "evidence item",
            lambda d: d["areas"]["implementation"]["evidence"][0],
            "validate_reviews",
            {"ref"},
        ),
        (
            "board reference",
            lambda d: d["areas"]["implementation"]["evidence"][0]["ref"],
            "validate_reviews",
            set(),
        ),
        ("scope", lambda d: d["scope"], "validate_scope", set()),
        ("scope item", lambda d: d["scope"]["items"][0], "validate_scope", set()),
        ("check", lambda d: d["checks"][0], "validate_checks", set()),
        (
            "cost",
            lambda d: d["checks"][3]["cost"],
            "validate_checks",
            {"budget", "calls", "model"},
        ),
        ("measurement", lambda d: d["measurements"][0], "validate_checks", set()),
        ("follow-up", lambda d: d["followups"][0], "validate_learning", set()),
        ("risk", lambda d: d["risks"][0], "validate_learning", set()),
        ("retrospective", lambda d: d["retrospective"], "validate_learning", set()),
        (
            "improvement candidate",
            lambda d: d["retrospective"]["candidates"][0],
            "validate_learning",
            set(),
        ),
        ("decision", lambda d: d["decision"], "validate_decision", set()),
        ("approval", lambda d: d["decision"]["approval"], "validate_decision", set()),
        (
            "tag publication",
            lambda d: d["decision"]["tagPublication"],
            "validate_decision",
            set(),
        ),
    )

    def test_a_required_key_cannot_be_left_out(self):
        for name, pick, validator, optional in self.OBJECTS:
            for key in list(pick(example())):
                if key in optional:
                    continue
                data = example()
                del pick(data)[key]
                with (
                    self.subTest(object=name, key=key),
                    self.assertRaisesRegex(
                        ValueError, "expected fields|array with \\w+ required"
                    ),
                ):
                    getattr(report, validator)(data)

    def test_an_unknown_key_is_refused(self):
        for name, pick, validator, _ in self.OBJECTS:
            data = example()
            pick(data)["unexpected"] = "x"
            with (
                self.subTest(object=name),
                self.assertRaisesRegex(ValueError, "expected fields"),
            ):
                getattr(report, validator)(data)

    def test_an_optional_key_may_be_absent(self):
        for name, pick, validator, optional in self.OBJECTS:
            for key in sorted(optional - {"model"}):
                data = example()
                del pick(data)[key]
                with self.subTest(object=name, key=key):
                    getattr(report, validator)(data)

    def test_the_upstream_sprint_fields_are_gone(self):
        for field, value in (
            ("id", 17),
            ("boardId", 42),
            ("url", "https://example.com/board"),
            ("closureIssue", {"key": "DEMO-2", "url": "https://example.com/DEMO-2"}),
        ):
            data = example()
            data["sprint"][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(
                ValueError, "expected fields"
            ):
                report.validate_identity(data)


class UrlTests(unittest.TestCase):
    def test_only_plain_absolute_http_urls_are_accepted(self):
        for value in (
            "https://example.com/a?b=c#d",
            "http://example.com/x",
            "https://git.example.com/demo/invitations/compare/9c8b7a6...f1e2d3c",
        ):
            report.url(value)
        for value in ATTACKS:
            with self.subTest(value=value), self.assertRaises(ValueError):
                report.url(value)

    def test_the_link_builder_checks_the_url_again(self):
        for value in ATTACKS:
            with self.subTest(value=value), self.assertRaises(ValueError):
                report.link("label", value)
            with self.subTest(code_link=value), self.assertRaises(ValueError):
                report.code_link("label", value)
        self.assertEqual(
            report.link('a"b<c', "https://example.com/x?y=1&z=2"),
            '<a href="https://example.com/x?y=1&amp;z=2" rel="noreferrer">a&quot;b&lt;c</a>',
        )

    def test_a_bad_url_is_refused_wherever_the_data_holds_one(self):
        places = (
            ("repository", lambda d, v: d["release"].update(repository=v)),
            ("candidate url", lambda d, v: d["release"]["candidate"].update(url=v)),
            (
                "evidence url",
                lambda d, v: d["areas"]["implementation"]["evidence"][1].update(url=v),
            ),
        )
        for name, place in places:
            for value in ATTACKS:
                if name == "evidence url" and value is None:
                    continue  # a null url means the item has no link
                data = example()
                place(data, value)
                with (
                    self.subTest(place=name, value=value),
                    self.assertRaises(ValueError),
                ):
                    report.validate(data)
                with (
                    self.subTest(render=name, value=value),
                    self.assertRaises(ValueError),
                ):
                    report.render(data)

    def test_the_decision_report_url_is_checked(self):
        for value in ATTACKS[:-4]:
            data = decided(example())
            data["decision"]["approval"]["reportUrl"] = value
            with self.subTest(value=value), self.assertRaises(ValueError):
                report.validate_decision(data)
            with self.subTest(render=value), self.assertRaises(ValueError):
                report.render(data)


class EvidenceRuleTests(RuleCase):
    REF = {"key": "S-3", "digest": "c" * 64}

    def evidence(self, data, items):
        data["areas"]["implementation"]["evidence"] = items

    def test_evidence_items_are_validated(self):
        self.table(
            report.validate_reviews,
            [
                ("not a list", lambda d: self.evidence(d, "x"), "evidence array required"),
                ("not an object", lambda d: self.evidence(d, ["x"]), "expected fields"),
                (
                    "blank label",
                    lambda d: self.evidence(d, [{"label": "", "url": None}]),
                    "text required: label",
                ),
                (
                    "no label key",
                    lambda d: self.evidence(d, [{"url": None}]),
                    "expected fields",
                ),
                (
                    "no url key",
                    lambda d: self.evidence(d, [{"label": "x"}]),
                    "expected fields",
                ),
                (
                    "other key",
                    lambda d: self.evidence(
                        d, [{"label": "x", "url": None, "comment": "10101"}]
                    ),
                    "expected fields",
                ),
                (
                    "bad url",
                    lambda d: self.evidence(d, [{"label": "x", "url": "javascript:1"}]),
                    "only absolute HTTP",
                ),
                (
                    "url and reference",
                    lambda d: self.evidence(
                        d, [{"label": "x", "url": "https://example.com/x", "ref": self.REF}]
                    ),
                    "not both",
                ),
            ],
        )

    def test_a_board_reference_names_a_key_and_a_digest_and_nothing_else(self):
        rows = []
        for ref in (
            {"key": "S-3"},
            {"digest": "c" * 64},
            {"key": "S-3", "digest": "c" * 64, "commentId": "10101"},
            "S-3",
        ):
            rows.append(
                (
                    f"shape {ref}",
                    lambda d, ref=ref: self.evidence(
                        d, [{"label": "x", "url": None, "ref": ref}]
                    ),
                    "expected fields",
                )
            )
        for digest in ("c" * 63, "c" * 65, "C" * 64, "short", ""):
            rows.append(
                (
                    f"digest {digest!r}",
                    lambda d, digest=digest: self.evidence(
                        d,
                        [{"label": "x", "url": None, "ref": {"key": "S-3", "digest": digest}}],
                    ),
                    "invalid digest",
                )
            )
        rows.append(
            (
                "blank key",
                lambda d: self.evidence(
                    d, [{"label": "x", "url": None, "ref": {"key": " ", "digest": "c" * 64}}]
                ),
                "text required: key",
            )
        )
        self.table(report.validate_reviews, rows)

    def test_a_board_reference_is_linked_evidence_and_a_bare_label_is_not(self):
        data = example()
        self.evidence(data, [{"label": "Round record", "url": None, "ref": self.REF}])
        report.validate_reviews(data)
        for items in (
            [{"label": "Round record", "url": None}],
            [{"label": "Round record", "url": None, "ref": None}],
            [],
        ):
            data = example()
            self.evidence(data, items)
            with self.assertRaisesRegex(ValueError, "complete review evidence"):
                report.validate_reviews(data)
        data = example()
        self.evidence(data, [{"label": "Receipt", "url": "https://example.com/r"}])
        report.validate_reviews(data)

    def test_a_board_reference_renders_as_its_key_and_digest(self):
        data = example()
        self.evidence(data, [{"label": "Round record", "url": None, "ref": self.REF}])
        output, _ = report.render(data)
        self.assertIn(
            '<li>Round record <code>S-3 sha256:' + "c" * 64 + "</code></li>",
            section(output, "reviews"),
        )

    def test_a_url_renders_as_a_link_and_a_label_as_text(self):
        html = report.evidence_html(
            [
                {"label": "Receipt", "url": "https://example.com/r"},
                {"label": "Offline note", "url": None},
            ]
        )
        self.assertEqual(
            html,
            '<ul class="evidence"><li><a href="https://example.com/r" rel="noreferrer">'
            "Receipt</a></li><li>Offline note</li></ul>",
        )
        self.assertEqual(report.evidence_html([]), "")


class IdentityRuleTests(RuleCase):
    def test_sprint_identity_is_vault_plus_sprint_key(self):
        for key in ("S3", "s-3", "S-", "3", "S-3 ", "S-x", "S-3-4", "", None, 3):
            data = example()
            data["sprint"]["key"] = key
            with (
                self.subTest(key=key),
                self.assertRaisesRegex(ValueError, "sprint key must look like"),
            ):
                report.validate_identity(data)
        for key in ("S-1", "S-12", "S-007"):
            data = example()
            data["sprint"]["key"] = key
            report.validate_identity(data)

    def test_identity_rules(self):
        self.table(
            report.validate_identity,
            [
                ("schema 2", lambda d: d.update(schemaVersion=2), "unsupported schemaVersion"),
                ("schema true", lambda d: d.update(schemaVersion=True), "unsupported schemaVersion"),
                ("schema text", lambda d: d.update(schemaVersion="1"), "unsupported schemaVersion"),
                ("language", lambda d: d["report"].update(language="es"), "invalid enum"),
                ("version 0", lambda d: d["report"].update(version=0), "invalid report identity"),
                ("version true", lambda d: d["report"].update(version=True), "invalid report identity"),
                ("version float", lambda d: d["report"].update(version=1.0), "invalid report identity"),
                ("fictional", lambda d: d["report"].update(fictional="yes"), "invalid report identity"),
                ("generated without zone", lambda d: d["report"].update(generatedAt="2026-10-29T15:40:00"), "requires timezone"),
                ("generated none", lambda d: d["report"].update(generatedAt=None), "timestamp required"),
                ("generated blank", lambda d: d["report"].update(generatedAt=""), "timestamp required"),
                ("report id", lambda d: d["report"].update(id=" "), "text required: id"),
                ("author", lambda d: d["report"].update(preparedBy=""), "text required: preparedBy"),
                ("project key", lambda d: d["project"].update(key=""), "text required: key"),
                ("project name", lambda d: d["project"].update(name=""), "text required: name"),
                ("vault", lambda d: d["sprint"].update(vault=" "), "text required: vault"),
                ("sprint name", lambda d: d["sprint"].update(name=""), "text required: name"),
                ("sprint goal", lambda d: d["sprint"].update(goal=""), "text required: goal"),
                ("closure task", lambda d: d["sprint"].update(closureTask=" "), "text required: closureTask"),
                ("closure task type", lambda d: d["sprint"].update(closureTask={"key": "DEMO-2"}), "text required: closureTask"),
                ("state future", lambda d: d["sprint"].update(state="future"), "invalid enum"),
                ("state blank", lambda d: d["sprint"].update(state=""), "invalid enum"),
                ("scope digest short", lambda d: d["sprint"].update(scopeDigest="short"), "invalid digest"),
                ("scope digest upper case", lambda d: d["sprint"].update(scopeDigest="E" * 64), "invalid digest"),
                ("scope digest long", lambda d: d["sprint"].update(scopeDigest="e" * 65), "invalid digest"),
                ("scope digest none", lambda d: d["sprint"].update(scopeDigest=None), "invalid digest"),
                (
                    "end before start",
                    lambda d: d["sprint"].update(
                        startDate="2026-10-30T17:00:00-04:00", endDate="2026-10-19T09:00:00-04:00"
                    ),
                    "invalid sprint dates",
                ),
                (
                    "end equals start",
                    lambda d: d["sprint"].update(endDate=d["sprint"]["startDate"]),
                    "invalid sprint dates",
                ),
                ("start without zone", lambda d: d["sprint"].update(startDate="2026-10-19T09:00:00"), "requires timezone"),
                ("start none", lambda d: d["sprint"].update(startDate=None), "timestamp required"),
                ("commit text", lambda d: d["release"]["candidate"].update(sha="HEAD"), "invalid digest"),
                ("candidate short", lambda d: d["release"]["candidate"].update(sha="a" * 39), "invalid digest"),
                ("base short", lambda d: d["release"]["base"].update(sha="a" * 39), "invalid digest"),
                ("tag", lambda d: d["release"].update(tag=""), "text required: tag"),
                ("base ref blank", lambda d: d["release"]["base"].update(ref=""), "text required: ref"),
                ("base ref type", lambda d: d["release"]["base"].update(ref=5), "text required: ref"),
                ("headline", lambda d: d["outcome"].update(headline=""), "text required: headline"),
                ("summary", lambda d: d["outcome"].update(summary=" "), "text required: summary"),
                ("readiness", lambda d: d["outcome"].update(readiness="done"), "invalid enum"),
            ],
        )

    def test_identity_values_that_must_be_accepted(self):
        for mutate in (
            lambda d: d["release"]["candidate"].update(sha="c" * 64),
            lambda d: d["release"]["base"].update(ref=None),
            lambda d: d["sprint"].update(state="planned"),
            lambda d: d["sprint"].update(state="closed"),
            lambda d: d["sprint"].update(startDate="2026-10-19T13:00:00Z"),
        ):
            data = example()
            mutate(data)
            report.validate_identity(data)

    def test_a_null_is_refused_where_a_key_must_hold_text(self):
        for pick, key in (
            (lambda d: d["sprint"], "goal"),
            (lambda d: d["release"], "tag"),
            (lambda d: d["outcome"], "headline"),
            (lambda d: d["scope"]["items"][0], "title"),
            (lambda d: d["checks"][0], "detail"),
            (lambda d: d["risks"][0], "title"),
        ):
            data = example()
            pick(data)[key] = None
            with (
                self.subTest(key=key),
                self.assertRaisesRegex(ValueError, "text required"),
            ):
                for validator in (
                    report.validate_identity,
                    report.validate_scope,
                    report.validate_checks,
                    report.validate_learning,
                ):
                    validator(data)

    def test_sprint_state_uses_the_board_names(self):
        for state in ("planned", "active", "closed"):
            data = example()
            data["sprint"]["state"] = state
            self.assertEqual(report.validate_identity(data), None)
        output, _ = report.render(example())
        self.assertIn(report.ENUMS["en"]["active"], output)


class ReviewRuleTests(RuleCase):
    def test_review_rules(self):
        area = lambda d: d["areas"]["retrospective"]  # noqa: E731
        self.table(
            report.validate_reviews,
            [
                ("status", lambda d: area(d).update(status="done"), "invalid enum"),
                ("summary", lambda d: area(d).update(summary=""), "text required: summary"),
                ("decision", lambda d: area(d).update(decision="APPROVE"), "invalid enum"),
                ("round 0", lambda d: area(d).update(round=0), "positive review round"),
                ("round -1", lambda d: area(d).update(round=-1), "positive review round"),
                ("round true", lambda d: area(d).update(round=True), "positive review round"),
                ("round float", lambda d: area(d).update(round=1.5), "positive review round"),
                ("round text", lambda d: area(d).update(round="2"), "positive review round"),
                ("reviewers type", lambda d: area(d).update(reviewers="example-reviewer-a"), "independent review identities"),
                ("reviewer blank", lambda d: area(d).update(reviewers=[" "]), "independent review identities"),
                (
                    "reviewer is the author",
                    lambda d: area(d).update(reviewers=[d["report"]["preparedBy"]]),
                    "independent review identities",
                ),
                (
                    "one reviewer is the author",
                    lambda d: area(d).update(reviewers=["example-reviewer-a", d["report"]["preparedBy"]]),
                    "independent review identities",
                ),
                ("counts raised", lambda d: area(d).update(findings={"raised": 0, "resolved": 1, "deferred": 0}), "invalid finding counts"),
                ("counts sum", lambda d: area(d).update(findings={"raised": 1, "resolved": 1, "deferred": 1}), "invalid finding counts"),
                ("counts negative", lambda d: area(d).update(findings={"raised": -1, "resolved": 0, "deferred": 0}), "invalid finding counts"),
                (
                    "negative resolved inside the sum",
                    lambda d: area(d).update(findings={"raised": 1, "resolved": -1, "deferred": 0}),
                    "invalid finding counts",
                ),
                (
                    "negative deferred inside the sum",
                    lambda d: area(d).update(findings={"raised": 2, "resolved": 3, "deferred": -1}),
                    "invalid finding counts",
                ),
                ("counts float", lambda d: area(d).update(findings={"raised": 1.0, "resolved": 0, "deferred": 0}), "invalid finding counts"),
                ("counts true", lambda d: area(d).update(findings={"raised": True, "resolved": 0, "deferred": 0}), "invalid finding counts"),
                ("no round", lambda d: area(d).update(round=None), "captured review metadata"),
                ("no reviewers", lambda d: area(d).update(reviewers=[]), "captured review metadata"),
                ("no counts", lambda d: area(d).update(findings=None), "captured review metadata"),
                (
                    "passed with an unlinked item",
                    lambda d: area(d).update(evidence=[{"label": "Trust me", "url": None}]),
                    "complete review evidence",
                ),
                ("passed with no evidence", lambda d: area(d).update(evidence=[]), "complete review evidence"),
                (
                    "passed with an open finding",
                    lambda d: area(d).update(findings={"raised": 2, "resolved": 1, "deferred": 0}),
                    "complete review evidence",
                ),
                (
                    "areas on two rounds",
                    lambda d: d["areas"]["verification"].update(round=3),
                    "share one round",
                ),
            ],
        )

    def test_area_status_follows_the_review_decision(self):
        for decision, status in (
            ("HUMAN_REVIEW", "passed"),
            ("STOP", "failed"),
            ("FIX", "incomplete"),
            ("REVIEW_AGAIN", "incomplete"),
        ):
            data = example()
            data["areas"]["retrospective"].update(decision=decision, status=status)
            with self.subTest(decision=decision, status=status):
                report.validate_reviews(data)
        for decision, status in (
            ("HUMAN_REVIEW", "incomplete"),
            ("HUMAN_REVIEW", "failed"),
            ("STOP", "passed"),
            ("STOP", "incomplete"),
            ("FIX", "passed"),
            ("FIX", "failed"),
            ("REVIEW_AGAIN", "passed"),
            ("REVIEW_AGAIN", "failed"),
        ):
            data = example()
            data["areas"]["retrospective"].update(decision=decision, status=status)
            with (
                self.subTest(decision=decision, status=status),
                self.assertRaisesRegex(ValueError, "contradicts review decision"),
            ):
                report.validate_reviews(data)

    def test_an_area_without_a_decision_is_incomplete_and_never_passes(self):
        data = example()
        data["areas"]["retrospective"].update(
            decision=None, status="incomplete", round=None, reviewers=[], findings=None
        )
        report.validate_reviews(data)
        for status in ("passed", "failed"):
            data["areas"]["retrospective"]["status"] = status
            with self.assertRaisesRegex(ValueError, "contradicts review decision"):
                report.validate_reviews(data)

    def test_the_french_example_keeps_partial_review_facts(self):
        data = example("fr")
        report.validate_reviews(data)
        documentation = data["areas"]["documentation"]
        self.assertEqual(
            (documentation["decision"], documentation["round"], documentation["reviewers"]),
            (None, None, []),
        )


class ScopeRuleTests(RuleCase):
    def test_scope_rules(self):
        item = lambda d, n=0: d["scope"]["items"][n]  # noqa: E731
        self.table(
            report.validate_scope,
            [
                ("summary", lambda d: d["scope"].update(summary=""), "text required: summary"),
                ("items type", lambda d: d["scope"].update(items="x"), "array with key required"),
                (
                    "duplicate key",
                    lambda d: d["scope"]["items"].append(copy.deepcopy(item(d))),
                    "duplicate key",
                ),
                ("key blank", lambda d: item(d).update(key=" "), "array with key required"),
                ("title blank", lambda d: item(d).update(title=""), "text required: title"),
                ("origin", lambda d: item(d).update(origin="amended"), "invalid enum"),
                ("outcome", lambda d: item(d).update(outcome="done"), "invalid enum"),
                ("note type", lambda d: item(d).update(note=5), "text required: note"),
                ("destination type", lambda d: item(d, 2).update(destination={"key": "DEMO-8"}), "text required: destination"),
                ("destination blank", lambda d: item(d, 2).update(destination=""), "text required: destination"),
                (
                    "closure task in scope",
                    lambda d: item(d).update(key=d["sprint"]["closureTask"]),
                    "functional stories only",
                ),
                ("deferred without note", lambda d: item(d, 2).update(note=None), "code disposition"),
                ("scope note blank", lambda d: item(d, 2).update(note=" "), "text required: note"),
                (
                    "removed without note",
                    lambda d: item(d, 2).update(outcome="removed", destination=None, note=None),
                    "code disposition",
                ),
                ("deferred without destination", lambda d: item(d, 2).update(destination=None), "requires destination"),
                ("url key", lambda d: item(d).update(url="https://example.com/DEMO-1"), "expected fields"),
            ],
        )

    def test_removed_work_needs_a_note_but_no_destination(self):
        data = example()
        data["scope"]["items"][2].update(outcome="removed", destination=None, note="Reverted in abc.")
        report.validate_scope(data)

    def test_tickets_are_plain_keys_in_the_page(self):
        output, _ = report.render(example())
        scope = section(output, "scope")
        self.assertIn('<span class="key">DEMO-1</span>', scope)
        self.assertIn("<code>DEMO-8</code>", scope)
        self.assertNotIn("<a ", scope)


class CheckRuleTests(RuleCase):
    def test_check_rules(self):
        check = lambda d, n=0: d["checks"][n]  # noqa: E731
        self.table(
            report.validate_checks,
            [
                ("duplicate id", lambda d: d["checks"].append(copy.deepcopy(check(d))), "duplicate id"),
                ("id blank", lambda d: check(d).update(id=" "), "array with id required"),
                ("none required", lambda d: d.update(checks=[]), "at least one required"),
                (
                    "only optional",
                    lambda d: [c.update(required=False) for c in d["checks"]],
                    "at least one required",
                ),
                ("name", lambda d: check(d).update(name=""), "text required: name"),
                ("detail", lambda d: check(d).update(detail=""), "text required: detail"),
                ("command blank", lambda d: check(d).update(command=""), "text required: command"),
                ("command type", lambda d: check(d).update(command=5), "text required: command"),
                ("kind", lambda d: check(d).update(kind="load"), "invalid enum"),
                ("status", lambda d: check(d).update(status="skipped"), "invalid enum"),
                ("required type", lambda d: check(d).update(required="yes"), "required must be boolean"),
                ("evidence type", lambda d: check(d).update(evidence="x"), "evidence array required"),
                (
                    "passed without evidence",
                    lambda d: check(d).update(evidence=[]),
                    "linked run evidence",
                ),
                (
                    "passed with an unlinked item",
                    lambda d: check(d).update(evidence=[{"label": "Trust me", "url": None}]),
                    "linked run evidence",
                ),
                (
                    "failed without evidence",
                    lambda d: check(d).update(status="failed", evidence=[]),
                    "linked run evidence",
                ),
                (
                    "failed with an unlinked item",
                    lambda d: check(d).update(status="failed", evidence=[{"label": "x", "url": None}]),
                    "linked run evidence",
                ),
                ("llm without cost", lambda d: check(d, 3).update(cost=None), "cost and model"),
                (
                    "llm cost without model",
                    lambda d: check(d, 3).update(cost={"amount": 1.0, "currency": "USD"}),
                    "cost and model",
                ),
                (
                    "failed llm without cost",
                    lambda d: check(d, 3).update(status="failed", cost=None),
                    "cost and model",
                ),
                ("measurements type", lambda d: d.update(measurements={}), "measurements array required"),
                ("measurement name", lambda d: d["measurements"][0].update(metric=" "), "text required: metric"),
                ("measurement scope", lambda d: d["measurements"][0].update(scope=""), "text required: scope"),
                ("measurement note", lambda d: d["measurements"][0].update(observation=""), "text required: observation"),
                ("measurement value", lambda d: d["measurements"][0].update(baseline=""), "text required: baseline"),
                ("measurement tool", lambda d: d["measurements"][0].update(tool=5), "text required: tool"),
            ],
        )

    def test_check_states_that_must_be_accepted(self):
        data = example()
        data["checks"][5].update(  # the optional check can fail with a receipt
            status="failed", evidence=[{"label": "Run", "url": "https://example.com/run"}]
        )
        report.validate_checks(data)
        data = example()
        data["checks"][4].update(status="not_applicable", evidence=[])
        report.validate_checks(data)
        data = example()
        data["checks"][3].update(status="not_run", cost=None, evidence=[])
        report.validate_checks(data)

    def test_cost_rules(self):
        cost = lambda d: d["checks"][0].update  # noqa: E731
        rows = []
        for value, message in (
            ({"amount": -1, "currency": "USD"}, "invalid cost"),
            ({"amount": 1, "currency": "USD", "budget": -1}, "invalid cost"),
            ({"amount": float("inf"), "currency": "USD"}, "invalid cost"),
            ({"amount": float("nan"), "currency": "USD"}, "invalid cost"),
            ({"amount": "1", "currency": "USD"}, "invalid cost"),
            ({"amount": True, "currency": "USD"}, "invalid cost"),
            ({"amount": 1, "currency": "usd"}, "three-letter currency"),
            ({"amount": 1, "currency": "US"}, "three-letter currency"),
            ({"amount": 1, "currency": 5}, "three-letter currency"),
            ({"amount": 1, "currency": "USD", "calls": -1}, "call count"),
            ({"amount": 1, "currency": "USD", "calls": 1.5}, "call count"),
            ({"amount": 1, "currency": "USD", "calls": True}, "call count"),
            ({"amount": 1, "currency": "USD", "model": " "}, "text required: model"),
            ({"amount": 1, "currency": "USD", "unit": "calls"}, "expected fields"),
            ({"currency": "USD"}, "expected fields"),
        ):
            rows.append((str(value), lambda d, value=value: cost(d)(cost=value), message))
        self.table(report.validate_checks, rows)
        data = example()
        data["checks"][0]["cost"] = {
            "amount": 0,
            "currency": "EUR",
            "budget": 0,
            "calls": 0,
            "model": "example-llm",
        }
        report.validate_checks(data)


class LearningRuleTests(RuleCase):
    def test_learning_rules(self):
        risk = lambda d, n=0: d["risks"][n]  # noqa: E731
        candidate = lambda d: d["retrospective"]["candidates"][0]  # noqa: E731
        self.table(
            report.validate_learning,
            [
                (
                    "duplicate follow-up",
                    lambda d: d["followups"].append(copy.deepcopy(d["followups"][0])),
                    "duplicate key",
                ),
                ("follow-up reason", lambda d: d["followups"][0].update(reason=""), "text required: reason"),
                ("follow-up title", lambda d: d["followups"][0].update(title=" "), "text required: title"),
                ("follow-up url", lambda d: d["followups"][0].update(url="https://example.com/DEMO-8"), "expected fields"),
                ("duplicate risk", lambda d: d["risks"].append(copy.deepcopy(risk(d))), "duplicate id"),
                ("risk title", lambda d: risk(d).update(title=""), "text required: title"),
                ("risk id", lambda d: risk(d).update(id=" "), "array with id required"),
                ("risk resolution type", lambda d: risk(d).update(resolution=5), "text required: resolution"),
                ("risk follow-up type", lambda d: risk(d, 1).update(followup=5), "text required: followup"),
                ("risk severity", lambda d: risk(d).update(severity="Urgent"), "invalid enum"),
                ("risk area", lambda d: risk(d).update(area="security"), "invalid enum"),
                ("risk status", lambda d: risk(d).update(status="closed"), "invalid enum"),
                ("risk blocking", lambda d: risk(d).update(blocking="yes"), "blocking must be boolean"),
                ("risk evidence type", lambda d: risk(d).update(evidence="x"), "evidence array required"),
                ("resolved without resolution", lambda d: risk(d).update(resolution=None), "resolved finding requires evidence"),
                ("risk resolution blank", lambda d: risk(d).update(resolution=" "), "text required: resolution"),
                ("resolved without evidence", lambda d: risk(d).update(evidence=[]), "resolved finding requires evidence"),
                (
                    "resolved with an unlinked item",
                    lambda d: risk(d).update(evidence=[{"label": "Trust me", "url": None}]),
                    "resolved finding requires evidence",
                ),
                ("deferred that blocks", lambda d: risk(d, 1).update(blocking=True), "nonblocking listed follow-up"),
                ("deferred without follow-up", lambda d: risk(d, 1).update(followup=None), "nonblocking listed follow-up"),
                ("deferred to an unlisted key", lambda d: risk(d, 1).update(followup="DEMO-404"), "nonblocking listed follow-up"),
                ("retrospective summary", lambda d: d["retrospective"].update(summary=""), "text required: summary"),
                (
                    "duplicate candidate",
                    lambda d: d["retrospective"]["candidates"].append(copy.deepcopy(candidate(d))),
                    "duplicate id",
                ),
                ("candidate id", lambda d: candidate(d).update(id=" "), "array with id required"),
                ("candidate observation", lambda d: candidate(d).update(observation=""), "text required: observation"),
                ("candidate proposal", lambda d: candidate(d).update(proposal=""), "text required: proposal"),
                ("candidate destination", lambda d: candidate(d).update(destination=5), "text required: destination"),
                ("candidate destination object", lambda d: candidate(d).update(destination={"key": "DEMO-12"}), "text required: destination"),
                ("candidate type", lambda d: candidate(d).update(type="policy"), "invalid enum"),
                ("candidate recommendation", lambda d: candidate(d).update(recommendation="defer"), "invalid enum"),
                ("candidate choice", lambda d: candidate(d).update(humanChoice="maybe"), "invalid enum"),
                ("candidate evidence type", lambda d: candidate(d).update(evidence="x"), "evidence array required"),
            ],
        )

    def test_a_critical_or_high_risk_may_be_deferred_but_still_blocks(self):
        data = example()
        report.validate_learning(data)  # a Medium deferral is fine here
        data["risks"][1].update(severity="Critical")
        report.validate_learning(data)
        self.assertEqual([risk["id"] for risk in report.blockers(data)], ["R2"])

    def test_adopting_or_trying_an_improvement_needs_evidence_and_a_destination(self):
        for choice in ("adopt", "trial"):
            data = example()
            data["retrospective"]["candidates"][0].update(
                humanChoice=choice, destination="DEMO-12"
            )
            report.validate_learning(data)
            for mutate in (
                lambda c: c.update(destination=None),
                lambda c: c.update(evidence=[]),
                lambda c: c.update(evidence=[{"label": "Offline note", "url": None}]),
            ):
                candidate = copy.deepcopy(data)
                mutate(candidate["retrospective"]["candidates"][0])
                with (
                    self.subTest(choice=choice),
                    self.assertRaisesRegex(ValueError, "human decision evidence"),
                ):
                    report.validate_learning(candidate)
        for choice in ("pending", "reject", "no_action"):
            data = example()
            data["retrospective"]["candidates"][0].update(
                humanChoice=choice, destination=None, evidence=[]
            )
            report.validate_learning(data)

    def test_blockers(self):
        data = example()
        self.assertEqual(report.blockers(data), [])
        for severity, status, blocking, expected in (
            ("Critical", "open", False, True),
            ("High", "open", False, True),
            ("Critical", "deferred", False, True),
            ("High", "deferred", False, True),
            ("Medium", "open", True, True),
            ("Medium", "deferred", True, True),
            ("Medium", "open", False, False),
            ("Low", "open", False, False),
            ("High", "resolved", True, False),
            ("Critical", "resolved", True, False),
        ):
            data = example()
            data["risks"][1].update(severity=severity, status=status, blocking=blocking)
            with self.subTest(severity=severity, status=status, blocking=blocking):
                self.assertEqual(
                    "R2" in [risk["id"] for risk in report.blockers(data)], expected
                )


class DecisionRuleTests(RuleCase):
    def approval(self, data):
        return data["decision"]["approval"]

    def test_decision_value_rules(self):
        approval = self.approval
        self.table(
            report.validate_decision,
            [
                ("status", lambda d: approval(d).update(status="signed"), "invalid enum"),
                ("note type", lambda d: approval(d).update(note=5), "text required: note"),
                ("reference shape", lambda d: approval(d).update(ref="S-3"), "expected fields"),
                (
                    "reference digest",
                    lambda d: approval(d).update(ref={"key": "S-3", "digest": "short"}),
                    "invalid digest",
                ),
                ("publication status", lambda d: d["decision"]["tagPublication"].update(status="done"), "invalid enum"),
                (
                    "publication evidence type",
                    lambda d: d["decision"]["tagPublication"].update(evidence="x"),
                    "evidence array required",
                ),
            ],
        )

    def test_an_undecided_approval_cannot_name_a_target(self):
        for status in ("not_requested", "pending"):
            for key, value in (
                ("approver", "Human"),
                ("decidedAt", "2026-10-29T16:00:00-04:00"),
                ("sprint", "S-3"),
                ("scopeDigest", "a" * 64),
                ("candidate", "a" * 40),
                ("reportDigest", "a" * 64),
                ("reportUrl", "https://example.com/report"),
                ("tag", "v1.4.0"),
            ):
                data = example()
                data["decision"]["approval"].update(status=status)
                data["decision"]["approval"][key] = value
                with (
                    self.subTest(status=status, field=key),
                    self.assertRaisesRegex(ValueError, "undecided approval"),
                ):
                    report.validate_decision(data)
        data = example()
        data["decision"]["approval"].update(status="not_requested", ref=None, note=None)
        report.validate_decision(data)

    def test_a_decision_names_every_target_exactly(self):
        for status in ("approved", "rejected"):
            for field, value in (
                ("sprint", "S-4"),
                ("scopeDigest", "e" * 64),
                ("candidate", "c" * 40),
                ("tag", "v9.9.9"),
            ):
                data = decided(example("en" if status == "approved" else "fr"), status)
                data["decision"]["approval"][field] = value
                with (
                    self.subTest(status=status, field=field),
                    self.assertRaisesRegex(ValueError, "targets must match"),
                ):
                    report.validate_decision(data)

    def test_a_decision_needs_its_evidence(self):
        for field, value, message in (
            ("ref", None, "board reference and exact report evidence"),
            ("reportUrl", None, "board reference and exact report evidence"),
            ("approver", None, "board reference and exact report evidence"),
            ("approver", " ", "text required: approver"),
            ("decidedAt", None, "board reference and exact report evidence"),
            ("decidedAt", "2026-10-29T16:00:00", "requires timezone"),
            ("reportDigest", "not-a-digest", "invalid digest"),
            ("reportDigest", None, "invalid digest"),
            ("scopeDigest", "not-a-digest", "invalid digest"),
            ("scopeDigest", None, "invalid digest"),
        ):
            data = decided(example())
            data["decision"]["approval"][field] = value
            with (
                self.subTest(field=field, value=value),
                self.assertRaisesRegex(ValueError, message),
            ):
                report.validate_decision(data)

    def test_a_rejection_may_name_a_candidate_that_is_not_ready(self):
        data = decided(example("fr"), "rejected")
        report.validate_decision(data)
        output, _ = report.render(data)
        self.assertIn(report.ENUMS["fr"]["rejected"], output)
        data = decided(example("fr"), "approved")
        with self.assertRaisesRegex(ValueError, "technically ready"):
            report.validate_decision(data)

    def test_an_approval_of_a_ready_candidate_is_accepted(self):
        report.validate_decision(decided(example()))

    def test_publication_rules(self):
        base = lambda: published(decided(example()))  # noqa: E731
        publication = lambda d: d["decision"]["tagPublication"]  # noqa: E731
        self.table(
            report.validate_decision,
            [
                (
                    "object text",
                    lambda d: publication(d).update(object="not-an-object"),
                    "invalid digest",
                ),
                (
                    "object length",
                    lambda d: publication(d).update(object="b" * 41),
                    "invalid digest",
                ),
                (
                    "before the decision",
                    lambda d: publication(d).update(verifiedAt="2026-10-29T15:59:59-04:00"),
                    "follow human approval",
                ),
                (
                    "tag",
                    lambda d: publication(d).update(tag="v9.9.9"),
                    "linked publication evidence",
                ),
                (
                    "candidate",
                    lambda d: publication(d).update(candidate="c" * 40),
                    "linked publication evidence",
                ),
                (
                    "no evidence",
                    lambda d: publication(d).update(evidence=[]),
                    "linked publication evidence",
                ),
                (
                    "unlinked evidence",
                    lambda d: publication(d).update(evidence=[{"label": "x", "url": None}]),
                    "linked publication evidence",
                ),
            ],
            base=base,
        )
        data = base()
        data["decision"]["approval"].update(status="pending")
        for key in ("approver", "decidedAt", "sprint", "scopeDigest", "candidate", "reportDigest", "reportUrl", "tag"):
            data["decision"]["approval"][key] = None
        with self.assertRaises(ValueError):
            report.validate_decision(data)

    def test_publication_may_happen_at_the_decision_instant(self):
        data = published(decided(example()), verified_at="2026-10-29T16:00:00-04:00")
        report.validate_decision(data)
        data = published(decided(example()), verified_at="2026-10-29T20:00:00Z")
        report.validate_decision(data)

    def test_publication_needs_an_approval_not_a_rejection(self):
        data = published(decided(example("fr"), "rejected"))
        with self.assertRaises(ValueError):
            report.validate_decision(data)

    def test_an_unpublished_tag_claims_nothing(self):
        for key, value in (
            ("object", "b" * 40),
            ("verifiedAt", "2026-10-29T16:05:00-04:00"),
            ("tag", "v1.4.0"),
            ("candidate", "c" * 40),
            ("evidence", [{"label": "x", "url": "https://example.com/x"}]),
        ):
            data = example()
            data["decision"]["tagPublication"][key] = value
            with (
                self.subTest(field=key),
                self.assertRaisesRegex(ValueError, "unpublished tag cannot claim"),
            ):
                report.validate_decision(data)

    def test_a_closed_sprint_needs_a_verified_publication(self):
        data = decided(example())
        data["sprint"]["state"] = "closed"
        with self.assertRaisesRegex(ValueError, "closed sprint requires"):
            report.validate_decision(data)
        published(data)
        report.validate_decision(data)
        for state in ("planned", "active"):
            data["sprint"]["state"] = state
            report.validate_decision(data)


class ReadinessTests(RuleCase):
    def test_readiness_is_derived_from_reviews_checks_and_unresolved_risks(self):
        for label, mutate in (
            (
                "an area is incomplete",
                lambda d: d["areas"]["verification"].update(status="incomplete", decision="FIX"),
            ),
            ("a required check did not run", lambda d: d["checks"][1].update(status="not_run")),
            ("a required check failed", lambda d: d["checks"][1].update(status="failed")),
            ("an open High risk", lambda d: d["risks"][0].update(status="open", blocking=False)),
            ("an open blocking risk", lambda d: d["risks"][1].update(status="open", blocking=True)),
            ("a deferred High risk", lambda d: d["risks"][1].update(severity="High")),
            ("a deferred Critical risk", lambda d: d["risks"][1].update(severity="Critical")),
            (
                "an area failed",
                lambda d: d["areas"]["verification"].update(status="failed", decision="STOP"),
            ),
        ):
            with self.subTest(case=label):
                self.refuses(report.validate, mutate, "readiness contradicts")

    def test_a_blocked_report_cannot_say_ready_and_a_ready_one_cannot_say_blocked(self):
        data = example()
        data["outcome"]["readiness"] = "blocked"
        with self.assertRaisesRegex(ValueError, "readiness contradicts"):
            report.validate(data)
        data = example("fr")
        data["outcome"]["readiness"] = "ready"
        with self.assertRaisesRegex(ValueError, "readiness contradicts"):
            report.validate(data)
        data["outcome"]["readiness"] = "incomplete"
        with self.assertRaisesRegex(ValueError, "readiness contradicts"):
            report.validate(data)

    def test_blocked_outranks_incomplete(self):
        data = example()
        data["areas"]["verification"].update(status="incomplete", decision="FIX")
        data["checks"][1].update(status="failed")
        data["outcome"]["readiness"] = "incomplete"
        with self.assertRaisesRegex(ValueError, "readiness contradicts"):
            report.validate(data)
        data["outcome"]["readiness"] = "blocked"
        report.validate(data)

    def test_the_expected_value_for_each_state(self):
        data = example()
        report.validate(data)
        data["areas"]["verification"].update(status="failed", decision="STOP")
        data["outcome"]["readiness"] = "blocked"
        report.validate(data)
        data = example()
        data["checks"][1].update(status="not_run", evidence=[])
        data["outcome"]["readiness"] = "incomplete"
        report.validate(data)

    def test_an_optional_check_never_changes_readiness(self):
        for status in ("not_run", "failed"):
            data = example()
            data["checks"][5].update(
                status=status,
                evidence=[{"label": "Run", "url": "https://example.com/run"}]
                if status == "failed"
                else [],
            )
            with self.subTest(status=status):
                report.validate(data)

    def test_a_failed_required_check_blocks_and_an_unrun_one_only_holds(self):
        data = example()
        data["checks"][1].update(status="failed")
        data["outcome"]["readiness"] = "blocked"
        report.validate(data)
        data = example()
        data["checks"][1].update(status="not_run", evidence=[])
        data["outcome"]["readiness"] = "blocked"
        with self.assertRaisesRegex(ValueError, "readiness contradicts"):
            report.validate(data)

    def test_a_not_applicable_required_check_does_not_hold(self):
        data = example()
        self.assertEqual(data["checks"][4]["status"], "not_applicable")
        self.assertTrue(data["checks"][4]["required"])
        report.validate(data)


class MarkupTests(unittest.TestCase):
    def test_the_masthead_and_the_identity_section_name_the_sprint_by_vault_and_key(self):
        data = example()
        output, _ = report.render(data)
        labels = report.LABELS["en"]
        masthead = output[: output.index('<main class="wrap">')]
        self.assertEqual(
            fact(masthead, labels["sprint"]),
            ["<code>S-3</code> · " + report.ENUMS["en"]["active"]],
        )
        self.assertEqual(fact(masthead, labels["closure_task"]), ["<code>DEMO-2</code>"])
        identity = section(output, "identity")
        self.assertEqual(fact(identity, labels["vault"]), ["demo"])
        self.assertEqual(fact(identity, labels["sprint"]), ["<code>S-3</code>"])
        self.assertEqual(fact(identity, labels["closure_task"]), ["<code>DEMO-2</code>"])
        self.assertEqual(
            fact(identity, labels["scope_digest"]),
            ['<code class="digest">' + data["sprint"]["scopeDigest"] + "</code>"],
        )
        self.assertTrue(
            fact(identity, labels["source_digest"])[0].startswith('<code class="digest">')
        )

    def test_the_decision_section_shows_the_exact_targets(self):
        data = decided(example())
        output, _ = report.render(data)
        labels = report.LABELS["en"]
        decision = section(output, "decision")
        self.assertEqual(fact(decision, labels["decision_sprint"]), ["<code>S-3</code>"])
        self.assertEqual(
            fact(decision, labels["decision_scope"]),
            ["<code>" + data["sprint"]["scopeDigest"] + "</code>"],
        )
        self.assertEqual(
            fact(decision, labels["decision_candidate"]),
            ["<code>" + data["release"]["candidate"]["sha"] + "</code>"],
        )
        self.assertEqual(fact(decision, labels["decision_tag"]), ["<code>v1.4.0</code>"])
        self.assertEqual(
            fact(decision, labels["reference"]), ["<code>S-3 sha256:" + "d" * 64 + "</code>"]
        )
        self.assertIn(
            "<code>" + "a" * 64 + "</code>", fact(decision, labels["decision_report"])[0]
        )
        self.assertIn(REPORT_URL, fact(decision, labels["decision_report"])[0])
        self.assertIn(labels["approved_report_link"], fact(decision, labels["decision_report"])[0])

    def test_an_undecided_decision_shows_no_targets_and_its_request(self):
        data = example()
        output, _ = report.render(data)
        labels = report.LABELS["en"]
        decision = section(output, "decision")
        for label in ("decision_sprint", "decision_scope", "decision_candidate", "decision_tag"):
            self.assertEqual(fact(decision, labels[label]), [labels["unknown"]])
        request = data["decision"]["approval"]["ref"]
        self.assertEqual(
            fact(decision, labels["reference"]),
            ["<code>" + request["key"] + " sha256:" + request["digest"] + "</code>"],
        )

    def test_the_sign_off_names_what_a_decision_must_name(self):
        data = example()
        output, _ = report.render(data)
        labels = report.LABELS["en"]
        outcome = section(output, "outcome")
        targets = outcome[outcome.index('<dl class="sign-targets">') :]
        targets = targets[: targets.index("</dl>")]
        self.assertEqual(fact(targets, labels["sprint"]), ["<code>S-3</code>"])
        self.assertEqual(
            fact(targets, labels["scope_digest"]),
            ["<code>" + data["sprint"]["scopeDigest"] + "</code>"],
        )
        self.assertEqual(fact(targets, labels["proposed_tag"]), ["<code>v1.4.0</code>"])
        self.assertEqual(
            fact(targets, labels["candidate"]),
            ["<code>" + data["release"]["candidate"]["sha"] + "</code>"],
        )
        self.assertIn(
            "<p>" + labels["reference"] + labels["colon"] + "<code>S-3 sha256:",
            outcome,
        )

    def test_a_decided_sign_off_names_the_earlier_report_twice(self):
        output, _ = report.render(decided(example()))
        note = report.LABELS["en"]["approval_prior_note"]
        self.assertEqual(output.count(note), 2)
        self.assertEqual(section(output, "outcome").count(note), 1)
        self.assertEqual(section(output, "decision").count(note), 1)
        self.assertNotIn("sign-targets", output.replace(".sign-targets", ""))

    def test_scope_follow_ups_and_destinations_show_as_keys(self):
        data = example()
        data["retrospective"]["candidates"][0].update(
            humanChoice="adopt", destination="DEMO-12"
        )
        output, _ = report.render(data)
        risks = section(output, "risks")
        self.assertIn("<code>DEMO-8</code>", risks)
        self.assertIn("<code>DEMO-11</code>", risks)
        self.assertIn('<li><span class="key">DEMO-8</span>', risks)
        self.assertIn('<li><span class="key">DEMO-9</span>', risks)
        self.assertIn("<code>DEMO-12</code>", section(output, "retrospective"))
        self.assertNotIn("<code>DEMO-12</code>", section(output, "risks"))

    def test_the_overview_shows_the_derived_figures(self):
        output, _ = report.render(example())
        overview = section(output, "outcome")
        self.assertIn(">3 / 4<", overview)  # delivered scope items
        self.assertIn(">4 / 4<", overview)  # required checks, one is not applicable
        self.assertIn(">0<", overview)  # blocking risks
        self.assertIn("USD 1.86", overview.replace(" ", " "))

    def test_money_keeps_a_nonzero_amount_visible(self):
        for language, expected in (("en", "USD 0.0042"), ("fr", "0,0042 USD")):
            self.assertEqual(
                report.money(0.0042, "USD", language).replace(" ", " "), expected
            )
        self.assertEqual(report.money(2, "CAD", "en").replace("\u00a0", " "), "CAD 2.00")
        self.assertEqual(report.money(1.5, "EUR", "fr").replace(" ", " "), "1,50 EUR")

    def test_a_budget_overrun_is_flagged_only_when_the_amount_exceeds_the_budget(self):
        flag = report.LABELS["en"]["budget_exceeded"]
        for amount, budget, flagged in ((6.0, 5.0, True), (5.0, 5.0, False), (4.0, 5.0, False)):
            data = example()
            data["checks"][3]["cost"].update(amount=amount, budget=budget)
            output, _ = report.render(data)
            with self.subTest(amount=amount, budget=budget):
                self.assertEqual(flag in section(output, "checks"), flagged)

    def test_costs_are_summed_per_currency_and_never_converted(self):
        data = example()
        data["checks"][0]["cost"] = {"amount": 1.5, "currency": "CAD"}
        data["checks"][1]["cost"] = {"amount": 0.25, "currency": "CAD"}
        output, _ = report.render(data)
        self.assertIn("CAD 1.75 + USD 1.86", output.replace(" ", " "))

    def test_tones_map_each_status_to_one_class(self):
        for value, tone in (
            ("failed", "bad"),
            ("rejected", "bad"),
            ("blocked", "bad"),
            ("Critical", "bad"),
            ("High", "bad"),
            ("passed", "good"),
            ("approved", "good"),
            ("published", "good"),
            ("resolved", "good"),
            ("delivered", "good"),
            ("closed", "good"),
            ("adopt", "good"),
            ("incomplete", "warn"),
            ("pending", "warn"),
            ("not_run", "warn"),
            ("open", "warn"),
            ("Medium", "warn"),
            ("trial", "info"),
            ("active", "info"),
            ("planned", "muted"),
            ("deferred", "muted"),
            ("Low", "muted"),
        ):
            with self.subTest(value=value):
                self.assertEqual(report.tone(value), tone)
        self.assertEqual(report.tone("resolved", blocking=True), "bad")
        self.assertEqual(report.tone("open", blocking=False), "warn")

    def test_values_are_escaped_for_text_and_quoted_attributes(self):
        self.assertEqual(report.esc('<a href="x">&\'</a>'), "&lt;a href=&quot;x&quot;&gt;&amp;&#x27;&lt;/a&gt;")
        self.assertEqual(report.esc(None), "")
        self.assertEqual(report.esc(5), "5")
        self.assertEqual(
            report.paragraphs("a<b>\nc\n\nd & e"), "<p>a&lt;b&gt;<br>c</p><p>d &amp; e</p>"
        )

    def test_the_page_has_one_section_per_reader_question_in_order(self):
        output, _ = report.render(example())
        order = [
            output.index(f'<section id="{name}"')
            for name in (
                "outcome",
                "decision",
                "scope",
                "risks",
                "reviews",
                "checks",
                "measurements",
                "retrospective",
                "identity",
            )
        ]
        self.assertEqual(order, sorted(order))


if __name__ == "__main__":
    unittest.main()
