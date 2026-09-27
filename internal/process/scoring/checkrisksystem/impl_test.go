package checkrisksystem

import (
	"maps"
	"slices"
	"strings"
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

func readPaths(rs *common.ReadSet) []common.HString {
	paths := append([]common.HString{}, rs.Paths...)
	for _, opt := range rs.OptionalPaths {
		paths = append(paths, opt.Path)
	}
	return paths
}

// TestEveryReadIsARollbackTrigger pins the freshness contract: every field a
// stage sends RS is a rollback trigger, so any change to it - a revised page,
// or a final page that only updates existing fields - re-asks the stage.
func TestEveryReadIsARollbackTrigger(t *testing.T) {
	for _, ds := range All {
		rs := readSet(ds)
		triggers := rs.RollbackTriggerPaths()
		for _, path := range readPaths(rs) {
			if !slices.Contains(triggers, path) {
				t.Errorf("%s: read %q must trigger rollback", ds.Name, path)
			}
		}
		for _, required := range ds.Required {
			if !slices.Contains(rs.Paths, required) {
				t.Errorf("%s: own field %q must be a required read, so the stage runs when it first appears", ds.Name, required)
			}
		}
	}
}

// TestEachStageSendsEverythingCollectedUpToIt pins what makes "the most
// advanced stage wins" fresh: each stage reads every field of its own and
// every earlier data set, and nothing of a later one.
func TestEachStageSendsEverythingCollectedUpToIt(t *testing.T) {
	for i, ds := range All {
		paths := readPaths(readSet(ds))
		for _, earlier := range All[:i+1] {
			for _, field := range earlier.Fields() {
				if !slices.Contains(paths, field) {
					t.Errorf("%s must send %s's field %q", ds.Name, earlier.Name, field)
				}
			}
		}
		for _, later := range All[i+1:] {
			for _, field := range later.Fields() {
				if slices.Contains(paths, field) {
					t.Errorf("%s must not read later stage %s's field %q - it would block until that data exists", ds.Name, later.Name, field)
				}
			}
		}
	}
}

// TestEveryRiskSystemFieldIsOwnedByOneStage pins coverage: every field RS is
// sent belongs to exactly one data set, so the most advanced stage - which
// reads all of them - re-asks on a change to any of them.
func TestEveryRiskSystemFieldIsOwnedByOneStage(t *testing.T) {
	fields := []common.HString{
		document.DocId, document.DocCustomerNik, document.DocCustomerName, document.DocCustomerBirthDate,
		document.DocProcessLoanStructureProductType, document.DocProcessLoanStructureProvisionalAmount,
		document.DocProcessLoanStructureLtvSubmission, document.DocProcessAssetCondition,
		document.DocProcessIncomeVerifiedAmount, document.DocProcessEnvironmentCheckResult,
		document.DocProcessUnderwritingConfirmed,
	}
	for _, field := range fields {
		owners := 0
		for _, ds := range All {
			if slices.Contains(ds.Fields(), field) {
				owners++
			}
		}
		if owners != 1 {
			t.Errorf("%q must belong to exactly one stage, got %d", field, owners)
		}
	}
	last := readPaths(readSet(All[len(All)-1]))
	for _, field := range fields {
		if !slices.Contains(last, field) {
			t.Errorf("the most advanced stage must read %q", field)
		}
	}
}

// TestStageOutputsAreDisjoint pins that each stage writes only its own
// fields, so a stage replaying its history can never overwrite another
// stage's newer answer. The retired shared risk_system.* fields and
// trigger_seq/stage_token cursors are never written by a stage.
func TestStageOutputsAreDisjoint(t *testing.T) {
	seen := map[common.HString]string{}
	for _, ds := range All {
		for _, path := range ds.Output.Paths() {
			if owner, ok := seen[path]; ok {
				t.Errorf("%q is written by both %s and %s", path, owner, ds.Name)
			}
			seen[path] = ds.Name
		}
	}
	for _, shared := range []common.HString{
		document.DocProcessScoringRiskSystemStatus, document.DocProcessScoringRiskSystemRequestId,
		document.DocProcessScoringTriggerSeq, document.DocProcessScoringStageToken,
	} {
		if owner, ok := seen[shared]; ok {
			t.Errorf("%s must not write %q", owner, shared)
		}
	}
}

// TestRecordAnswerWritesEveryOutput pins that every answer writes all five
// fields, so a stage's second answer is an update of each (never a newly-set
// max_ltv/reject_reason, which would re-arm nothing downstream).
func TestRecordAnswerWritesEveryOutput(t *testing.T) {
	out, err := recordAnswer(Asset.Output)(map[string]any{"status": "pending", "required_data_set": "survey-normal-v1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range Asset.Output.Paths() {
		if _, ok := out[path]; !ok {
			t.Errorf("answer must write %q even when RS sends no value for it", path)
		}
	}
	if len(out) != len(Asset.Output.Paths()) {
		t.Errorf("answer must write only the stage's own fields, got %v", out)
	}
}

// TestEarlierStageStepsAsideOnceALaterStageAnswered pins that each change
// asks RS exactly once: once a later stage has answered, an earlier stage's
// gate stays shut, because the aggregator uses the later answer and that
// stage already re-asks on changes to the earlier data.
func TestEarlierStageStepsAsideOnceALaterStageAnswered(t *testing.T) {
	passed := map[common.HString]any{
		document.DocProcessAgeCheckPassed:            true,
		document.DocProcessDuplicatePlateCheckPassed: true,
		document.DocStatus:                           "processing",
	}
	for i, ds := range All {
		if !stageGate(ds)(nil, passed) {
			t.Errorf("%s: must be allowed to ask while no later stage has answered", ds.Name)
		}
		for _, later := range All[i+1:] {
			answered := maps.Clone(passed)
			answered[later.Output.RequestId] = "playground-rs-x"
			if stageGate(ds)(nil, answered) {
				t.Errorf("%s: must step aside once %s has answered", ds.Name, later.Name)
			}
		}
		for _, earlier := range All[:i] {
			answered := maps.Clone(passed)
			answered[earlier.Output.RequestId] = "playground-rs-x"
			if !stageGate(ds)(nil, answered) {
				t.Errorf("%s: an earlier stage's answer (%s) must not block it", ds.Name, earlier.Name)
			}
		}
		rejected := maps.Clone(passed)
		rejected[document.DocProcessAgeCheckPassed] = false
		if stageGate(ds)(nil, rejected) {
			t.Errorf("%s: must never ask RS about a submission an intake check rejected", ds.Name)
		}
	}
}

// TestRequiredPreconditionPathsAreNotAlsoReads pins an SDK constraint:
// LockMap.Lock read-locks required precondition paths and reads separately,
// so a field in both is read-locked twice by the same step and panics the
// workflow task (ErrFieldLockAlreadyHeldById).
func TestRequiredPreconditionPathsAreNotAlsoReads(t *testing.T) {
	for _, ds := range All {
		paths := readPaths(readSet(ds))
		for _, pre := range preconditionSet(ds).Paths {
			if slices.Contains(paths, pre) {
				t.Errorf("%s: %q is both a required precondition path and a read", ds.Name, pre)
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
		if strings.HasPrefix(name, "check_risk_system_pg_verdict") {
			t.Errorf("aggregator %q is not a Risk System call and must not be listed", name)
		}
	}
	if !seen["check_risk_system_pg"] {
		t.Error("the intake stage must keep its check_risk_system_pg name")
	}
}
