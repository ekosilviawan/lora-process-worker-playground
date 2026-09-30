package checkrisksystem

import (
	"slices"
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

	"lora-process-worker-playground/internal/process/document"
)

func TestInitialGateRunsOnceBothChecksPassed(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessAgeCheckPassed:            true,
		document.DocProcessDuplicatePlateCheckPassed: true,
	}
	if !initialGate(nil, data) {
		t.Fatal("must run as soon as both initial checks have passed - no survey data is needed for the first call")
	}
}

func TestInitialGateBlocksUntilBothChecksPassed(t *testing.T) {
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
		if initialGate(nil, tc.data) {
			t.Errorf("%s: must not ask Risk System", tc.name)
		}
	}
}

// TestRollbackTriggersAreEverythingCollectedSoFar pins option 3's trigger
// rule: a stage is re-triggered by the submission's fields and every data-set
// field up to and including its own, and by nothing else - the later stages'
// fields are sent but never trigger. Every field RS is sent is read exactly
// once, either way. The retired trigger_seq/stage_token cursors must not be
// read at all.
func TestRollbackTriggersAreEverythingCollectedSoFar(t *testing.T) {
	for i, ds := range All {
		rs := readSet(ds)
		want := append([]common.HString{}, submissionFields...)
		for _, earlier := range All[:i+1] {
			want = append(want, earlier.Fields...)
		}
		triggers := rs.RollbackTriggerPaths()
		for _, field := range want {
			if !slices.Contains(triggers, field) {
				t.Errorf("%s: a change to %q must re-trigger it", ds.Name, field)
			}
		}
		for _, field := range triggers {
			if !slices.Contains(want, field) {
				t.Errorf("%s: %q belongs to a later stage and must not re-trigger it", ds.Name, field)
			}
		}
		read := slices.Clone(rs.Paths)
		for _, path := range rs.OptionalPaths {
			if slices.Contains(read, path.Path) {
				t.Errorf("%s: %q is both a required and an optional read", ds.Name, path.Path)
			}
			read = append(read, path.Path)
		}
		for _, path := range contextReadSet {
			if !slices.Contains(read, path.Path) {
				t.Errorf("%s: %q must be sent to RS", ds.Name, path.Path)
			}
		}
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
// data set's field(s) - a required read, so it is also sent to RS - plus the
// initial gate every step shares.
func TestEachDataSetStepGatesOnItsOwnField(t *testing.T) {
	for _, ds := range All {
		if len(ds.Fields) == 0 {
			t.Errorf("%s: a data set must gate on at least one field", ds.Name)
		}
		ps := preconditionSet(ds)
		for _, field := range initialPreconditionPaths {
			if !slices.Contains(ps.Paths, field) {
				t.Errorf("%s: precondition must require %q", ds.Name, field)
			}
		}
		for _, field := range ds.Fields {
			if !slices.Contains(readSet(ds).Paths, field) {
				t.Errorf("%s: data-set field %q must be a required read", ds.Name, field)
			}
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
		t.Error("the initial step must keep its check_risk_system_pg name")
	}
	if seen["check_risk_system_pg_verdict"] {
		t.Error("the verdict step is not a Risk System call and must not be listed")
	}
}

// TestEarlierStagesStepAsideForTheMostAdvanced pins the step-aside rule: once
// a stage has answered, only it and later stages may ask RS - so a change
// that re-queues several stages asks RS exactly once, through the most
// advanced one.
func TestEarlierStagesStepAsideForTheMostAdvanced(t *testing.T) {
	for i, ds := range All {
		if !notSuperseded(ds, map[common.HString]any{}) {
			t.Errorf("%s: must run before any stage has answered", ds.Name)
		}
		for j, answered := range All {
			data := map[common.HString]any{document.DocProcessScoringRiskSystemMostAdvancedStage: answered.Name}
			if got, want := notSuperseded(ds, data), j <= i; got != want {
				t.Errorf("%s after %s answered: notSuperseded = %v, want %v", ds.Name, answered.Name, got, want)
			}
		}
		unknown := map[common.HString]any{document.DocProcessScoringRiskSystemMostAdvancedStage: "check_risk_system_pg_unknown"}
		if !notSuperseded(ds, unknown) {
			t.Errorf("%s: an unknown stage name must not make it step aside", ds.Name)
		}
	}
}

// TestMostAdvancedStageIsRecordedButNeverATrigger pins how the step-aside
// field is wired: every stage writes it and reads it as an optional
// precondition path (unset until the first answer), and no stage reads it as
// data, so recording a new stage re-queues nothing.
func TestMostAdvancedStageIsRecordedButNeverATrigger(t *testing.T) {
	field := common.HString(document.DocProcessScoringRiskSystemMostAdvancedStage)
	if !slices.Contains(writeSet, field) {
		t.Errorf("%q must be written with every answer", field)
	}
	for _, ds := range All {
		ps := preconditionSet(ds)
		if !slices.Contains(ps.OptionalPaths, field) || slices.Contains(ps.Paths, field) {
			t.Errorf("%s: %q must be an optional precondition path", ds.Name, field)
		}
		rs := readSet(ds)
		if slices.Contains(rs.Paths, field) || slices.Contains(rs.RollbackTriggerPaths(), field) {
			t.Errorf("%s: %q must not be read as data or trigger a rollback", ds.Name, field)
		}
		for _, path := range rs.OptionalPaths {
			if path.Path == field {
				t.Errorf("%s: %q must not be sent to RS", ds.Name, field)
			}
		}
	}
}
