package checkrisksystem

import (
	"maps"
	"slices"
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

	"lora-process-worker-playground/internal/process/document"
)

func TestIntakeGateRunsOnceBothChecksPassed(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessAgeCheckPassed:            true,
		document.DocProcessDuplicatePlateCheckPassed: true,
	}
	if !intakeGate(nil, data) {
		t.Fatal("must run as soon as both intake checks have passed - no survey data is needed for the first call")
	}
}

func TestIntakeGateBlocksUntilBothChecksPassed(t *testing.T) {
	cases := []struct {
		name string
		data map[common.HString]any
	}{
		{"neither check", map[common.HString]any{}},
		{"only age check", map[common.HString]any{document.DocProcessAgeCheckPassed: true}},
		{"only duplicate-plate check", map[common.HString]any{document.DocProcessDuplicatePlateCheckPassed: true}},
		{"age check failed", map[common.HString]any{
			document.DocProcessAgeCheckPassed:            false,
			document.DocProcessDuplicatePlateCheckPassed: true,
		}},
		{"duplicate-plate check failed", map[common.HString]any{
			document.DocProcessAgeCheckPassed:            true,
			document.DocProcessDuplicatePlateCheckPassed: false,
		}},
	}
	for _, tc := range cases {
		if intakeGate(nil, tc.data) {
			t.Errorf("%s: must not ask Risk System", tc.name)
		}
	}
}

// TestNoRiskSystemStepHasRollbackTriggers pins the convergence rule: no step
// that asks Risk System may have a rollback trigger path, so planner.Rollback
// can never re-queue one and replay an older verdict from history. The
// retired trigger_seq/stage_token cursors must not be read at all.
func TestNoRiskSystemStepHasRollbackTriggers(t *testing.T) {
	rs := readSet()
	if len(rs.Paths) != 0 {
		t.Errorf("required reads are always rollback triggers - expected none, got %v", rs.Paths)
	}
	if triggers := rs.RollbackTriggerPaths(); len(triggers) != 0 {
		t.Errorf("expected no rollback trigger paths, got %v", triggers)
	}
	for _, ds := range All {
		ps := preconditionSet(ds)
		paths := append(append([]common.HString{}, ps.Paths...), ps.OptionalPaths...)
		for _, path := range contextReadSet {
			paths = append(paths, path.Path)
		}
		for _, retired := range []common.HString{document.DocProcessScoringTriggerSeq, document.DocProcessScoringStageToken} {
			if slices.Contains(paths, retired) {
				t.Errorf("%s: retired cursor %q must not be read", ds.Name, retired)
			}
		}
	}
}

// TestEachDataSetStepGatesOnItsOwnField pins that each step waits for its own
// data set's field(s) - plus the intake gate every step shares - and that the
// data-set fields are also sent to RS (precondition data is not part of the
// activity input).
func TestEachDataSetStepGatesOnItsOwnField(t *testing.T) {
	sent := make([]common.HString, 0, len(contextReadSet))
	for _, path := range contextReadSet {
		sent = append(sent, path.Path)
	}
	passed := map[common.HString]any{
		document.DocProcessAgeCheckPassed:            true,
		document.DocProcessDuplicatePlateCheckPassed: true,
		document.DocStatus:                           "new",
	}
	for _, ds := range All {
		if len(ds.Fields) == 0 {
			t.Errorf("%s: a data set must gate on at least one field", ds.Name)
		}
		ps := preconditionSet(ds)
		for _, field := range intakePreconditionPaths {
			if !slices.Contains(ps.Paths, field) {
				t.Errorf("%s: precondition must require %q", ds.Name, field)
			}
		}
		collected := maps.Clone(passed)
		for _, field := range ds.Fields {
			if !slices.Contains(ps.OptionalPaths, field) {
				t.Errorf("%s: precondition must read %q", ds.Name, field)
			}
			if !slices.Contains(sent, field) {
				t.Errorf("%s: data-set field %q must also be sent to RS", ds.Name, field)
			}
			collected[field] = "collected"
		}
		if dataSetGate(ds)(nil, passed) {
			t.Errorf("%s: must not ask RS before its data set is collected", ds.Name)
		}
		if !dataSetGate(ds)(nil, collected) {
			t.Errorf("%s: must ask RS once its data set is collected", ds.Name)
		}
		collected[document.DocProcessAgeCheckPassed] = false
		if dataSetGate(ds)(nil, collected) {
			t.Errorf("%s: must never ask RS about a submission an intake check rejected", ds.Name)
		}
	}
	pageFields := map[common.HString]string{
		document.DocProcessAssetCondition:         Asset.Name,
		document.DocProcessIncomeVerifiedAmount:   Income.Name,
		document.DocProcessEnvironmentCheckResult: EnvironmentCheck.Name,
		document.DocProcessUnderwritingConfirmed:  Underwriting.Name,
	}
	for field, name := range pageFields {
		owners := 0
		for _, ds := range All {
			if slices.Contains(ds.Fields, field) {
				owners++
				if ds.Name != name {
					t.Errorf("%q must gate %s, not %s", field, name, ds.Name)
				}
			}
		}
		if owners != 1 {
			t.Errorf("%q must gate exactly one step, got %d", field, owners)
		}
	}
}

// TestRequiredPreconditionPathsAreNotAlsoReads pins an SDK constraint:
// LockMap.Lock read-locks required precondition paths and optional reads
// separately, so a field in both is read-locked twice by the same step and
// panics the workflow task (ErrFieldLockAlreadyHeldById).
func TestRequiredPreconditionPathsAreNotAlsoReads(t *testing.T) {
	for _, ds := range All {
		for _, path := range contextReadSet {
			if slices.Contains(preconditionSet(ds).Paths, path.Path) {
				t.Errorf("%s: %q is both a required precondition path and an optional read", ds.Name, path.Path)
			}
		}
	}
}

func TestActivityNamesAreUniqueAndExcludeVerdict(t *testing.T) {
	names := ActivityNames()
	if len(names) != len(All) {
		t.Fatalf("expected %d names, got %v", len(All), names)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			t.Errorf("duplicate activity name %q", name)
		}
		seen[name] = true
	}
	if !seen["check_risk_system_pg"] {
		t.Error("the intake step must keep its check_risk_system_pg name")
	}
	if seen["check_risk_system_pg_verdict"] {
		t.Error("the verdict step is not a Risk System call and must not be listed")
	}
}
