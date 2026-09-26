package checkrisksystem

import (
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

// TestEveryReadIsARollbackTrigger pins the re-ask contract: every read path -
// required and optional - must be a rollback trigger, trigger_seq (bumped by
// every survey page) must be one of them, and the retired stage_token must
// not be read at all.
func TestEveryReadIsARollbackTrigger(t *testing.T) {
	readSet := common.MakeReadSet(requiredReadSet).SetOptionals(optionalReadSet, true)
	triggers := readSet.RollbackTriggerPaths()
	if len(triggers) != len(requiredReadSet)+len(optionalReadSet) {
		t.Fatalf("expected every required and optional path to trigger rollback, got %v", triggers)
	}
	for _, path := range optionalReadSet {
		if !slices.Contains(triggers, path.Path) {
			t.Errorf("optional path %q must trigger rollback", path.Path)
		}
	}
	if !slices.Contains(triggers, document.DocProcessScoringTriggerSeq) {
		t.Error("trigger_seq must be a rollback trigger - it's what re-asks RS after a page that only sets first-time data")
	}
	if slices.Contains(triggers, document.DocProcessScoringStageToken) {
		t.Error("retired stage_token must not be read")
	}
}
