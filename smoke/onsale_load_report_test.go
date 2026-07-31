//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"ticketing/smoke/internal/loadtest"
)

// TKT-130: writeReport used to be the last statement of each profile, so a
// single aborted stage discarded every stage that had completed before it —
// TKT-125 lost its finalize/confirm p99 that way. These two tests pin the
// replacement (beginReport, onsale_load_test.go) at the mechanism level: they
// inject an abort instead of running a load profile, so they need no stack and
// no load, and they run in the ordinary gate.

// Set in the re-executed child; its presence is what selects the child body.
const abortChildEnv = "TKT130_ABORT_CHILD"

// Set in the re-executed child for TKT-138's setup-abort fixture.
const setupAbortChildEnv = "TKT138_SETUP_ABORT_CHILD"

func TestOnsaleReportSurvivesAnAbortedStage(t *testing.T) {
	if os.Getenv(abortChildEnv) != "" {
		onsaleReportAbortChild(t)
		return
	}

	path := filepath.Join(t.TempDir(), "report.json")
	// The -test.run filter is load-bearing, not a convenience: TestMain
	// (coverage_test.go) enforces the uncovered-2xx coverage gate only on
	// *unfiltered* runs, and can turn a 0 into a 1. Without the filter the
	// child's exit code would stop meaning "the injected abort happened".
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), abortChildEnv+"=1", "ONSALE_REPORT="+path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child exited 0, want the injected abort to fail it:\n%s", out)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("aborted run wrote no report: %v\nchild output:\n%s", err, out)
	}
	var got loadtest.Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("aborted run wrote unparseable report: %v\n%s", err, b)
	}

	if !got.Partial {
		t.Errorf("aborted run must be marked partial, got partial=false: %s", b)
	}
	// The stage that completed before the abort must survive with the numbers
	// that only exist in this JSON — the whole point of the ticket.
	//
	// Not asserted here: that the *aborting* stage is absent. That property is
	// real — each profile calls generatorHealthy before appending, so an
	// inconclusive stage is never published as a server measurement — but it
	// lives in the profile bodies, which this fixture does not run. An
	// assertion here would pass because the child never appends a sweep stage,
	// not because the ordering holds, and would keep passing if that ordering
	// were inverted. Covering it for real means running the guard-and-append
	// path, which is the profile, which needs the stack.
	//
	// The *other* limitation this comment used to describe — that nothing caught
	// arming being moved back below the fallible setup — is closed by
	// TestArmReportBeforeSetupReplacesAStaleReport below (TKT-138). It could not
	// be closed here because this fixture arms the report itself.
	var nfr *loadtest.StageReport
	for i := range got.Stages {
		if got.Stages[i].Name == "nfr-3000pm" {
			nfr = &got.Stages[i]
		}
	}
	if nfr == nil {
		t.Fatalf("completed stage nfr-3000pm lost to the abort: %s", b)
	}
	if nfr.FinalizeP99Ms != 12.5 || nfr.ConfirmP99Ms != 7.25 {
		t.Errorf("finalize/confirm p99 = %v/%v, want 12.5/7.25", nfr.FinalizeP99Ms, nfr.ConfirmP99Ms)
	}
}

// Child body: accumulate one completed stage, then abort the way every
// generatorHealthy check does.
func onsaleReportAbortChild(t *testing.T) {
	report := loadtest.NewReport("TKT-130", "full", "injected")
	complete := beginReport(t, report)
	// Deliberately never called: the abort below is what must leave the report
	// marked partial.
	_ = complete
	report.Stages = append(report.Stages, loadtest.StageReport{
		Name: "nfr-3000pm", FinalizeP99Ms: 12.5, ConfirmP99Ms: 7.25,
	})
	t.Fatalf("injected abort at sweep-600")
}

func TestOnsaleReportMarksACompletedRunComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	// A subtest, so the report is written (on the subtest's cleanup) before the
	// assertions below read it.
	t.Run("run", func(t *testing.T) {
		t.Setenv("ONSALE_REPORT", path)
		report := loadtest.NewReport("TKT-130", "full", "injected")
		complete := beginReport(t, report)
		report.Stages = append(report.Stages, loadtest.StageReport{Name: "nfr-3000pm"})
		complete()
	})

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("completed run wrote no report: %v", err)
	}
	var got loadtest.Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unparseable report: %v\n%s", err, b)
	}
	if got.Partial {
		t.Errorf("completed run must not be marked partial: %s", b)
	}
	if len(got.Stages) != 1 || got.Stages[0].Name != "nfr-3000pm" {
		t.Errorf("stages lost: %s", b)
	}
}

// TKT-138. TKT-130 hoisted the arming above each profile's fallible setup so an
// aborted run still overwrites ONSALE_REPORT rather than leaving a previous run's
// file to be read as this one's output — and left that ordering as two statements
// in two profile bodies, guarded by nothing. The test above cannot guard it: it
// arms the report itself, so it would pass unchanged if a profile armed last.
//
// armReportThenSetup made the ordering structural, and this drives that function
// with a setup that aborts. The mutation it exists to catch is inside the helper:
// call setup() before beginReport and the child dies with no writer armed, so the
// stale file below survives intact and every assertion here fails.
//
// A subprocess, not a subtest: t.Fatalf in a subtest permanently fails the parent,
// so the parent could never assert on the outcome. Same self-exec pattern as
// TestOnsaleReportSurvivesAnAbortedStage, including the -test.run filter, which is
// load-bearing for the same reason it is there (TestMain's coverage gate applies
// only to unfiltered runs and can turn a 0 exit into a 1).
func TestArmReportBeforeSetupReplacesAStaleReport(t *testing.T) {
	if os.Getenv(setupAbortChildEnv) != "" {
		onsaleSetupAbortChild(t)
		return
	}

	path := filepath.Join(t.TempDir(), "report.json")
	// A previous run's report, valid and parseable, sitting where this run will
	// write. The sentinel is what makes "replaced" provable rather than "present".
	stale := []byte(`{"ticket":"TKT-000","profile":"stale-profile","git_sha":"staleshaXXXX","partial":false}` + "\n")
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), setupAbortChildEnv+"=1", "ONSALE_REPORT="+path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child exited 0, want the injected setup abort to fail it:\n%s", out)
	}
	if !bytes.Contains(out, []byte("injected setup abort")) {
		t.Fatalf("child failed for some other reason than the injected setup abort:\n%s", out)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("run aborted in setup wrote no report: %v\nchild output:\n%s", err, out)
	}
	// Replaced, not merely present: the previous run's bytes must be gone.
	if bytes.Equal(b, stale) {
		t.Fatalf("the stale report survived untouched — the report was armed AFTER the fallible setup:\n%s", b)
	}
	if bytes.Contains(b, []byte("stale-profile")) || bytes.Contains(b, []byte("staleshaXXXX")) {
		t.Fatalf("stale content survived in the report: %s", b)
	}

	var got loadtest.Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("run aborted in setup wrote unparseable report: %v\n%s", err, b)
	}
	// The identity of THIS run, so a file that merely changed is not enough.
	if got.Ticket != "TKT-82" || got.Profile != "setup-abort" {
		t.Errorf("report is not this run's: ticket=%q profile=%q\n%s", got.Ticket, got.Profile, b)
	}
	if !got.Partial {
		t.Errorf("a run that died in setup must be marked partial: %s", b)
	}
}

// Child body: the profile shape reduced to its ordering — a report, and a setup
// that aborts the way publishedSlot, inventoryAdminConn and statStatementsSetup
// all can.
func onsaleSetupAbortChild(t *testing.T) {
	report := loadtest.NewReport("TKT-82", "setup-abort", "injected")
	complete := armReportThenSetup(t, report, func() {
		t.Fatalf("injected setup abort")
	})
	// Unreachable: the setup above aborts. Present so the fixture is the same
	// shape as a profile, and so complete() staying uncalled is what marks the
	// report partial.
	complete()
}
