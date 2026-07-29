//go:build smoke

package smoke_test

import (
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
