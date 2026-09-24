package checkrisksystem

import (
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

	"lora-process-worker-playground/internal/process/document"
)

// bothChecksPassed seeds the two intake-check flags stageGate now hard-requires,
// so the tests below keep exercising stage-token gating specifically rather
// than tripping over the new dual-check gate.
func bothChecksPassed() map[common.HString]any {
	return map[common.HString]any{
		document.DocProcessAgeCheckPassed:            true,
		document.DocProcessDuplicatePlateCheckPassed: true,
	}
}

func TestStageGate(t *testing.T) {
	data := bothChecksPassed()
	data[document.DocProcessScoringStageToken] = "customer_verification"
	if stageGate(nil, data) {
		t.Fatal("customer verification must wait for birth date")
	}

	data[document.DocCustomerBirthDate] = "1985-03-15"
	if !stageGate(nil, data) {
		t.Fatal("customer verification should run once its gate field is present")
	}
}

func TestStageGateFiresImmediatelyForPostSubmission(t *testing.T) {
	data := bothChecksPassed()
	data[document.DocProcessScoringStageToken] = "post_submission"
	if !stageGate(nil, data) {
		t.Fatal("post_submission has no gate fields and must run before any survey")
	}
}

func TestStageGateSupportsLaterCheckpoints(t *testing.T) {
	data := bothChecksPassed()
	data[document.DocProcessScoringStageToken] = "asset_review"
	if stageGate(nil, data) {
		t.Fatal("asset review must wait for the asset survey's findings")
	}
	data[document.DocProcessAssetCondition] = "fair"
	if !stageGate(nil, data) {
		t.Fatal("asset review should run once its gate field is present")
	}

	data = bothChecksPassed()
	data[document.DocProcessScoringStageToken] = "financing"
	data[document.DocProcessLoanStructureLtvSubmission] = 0.8
	if !stageGate(nil, data) {
		t.Fatal("financing should run once the financing survey's findings are present")
	}

	data[document.DocProcessScoringStageToken] = "complete_normal_survey"
	data[document.DocProcessIncomeVerifiedAmount] = 15000000.0
	if !stageGate(nil, data) {
		t.Fatal("complete_normal_survey should run once the income page's findings are present")
	}

	data[document.DocProcessScoringStageToken] = "underwriting"
	data[document.DocProcessUnderwritingConfirmed] = true
	if !stageGate(nil, data) {
		t.Fatal("underwriting should run once its confirmation is present")
	}

	data = bothChecksPassed()
	data[document.DocProcessScoringStageToken] = "financing_confirmation"
	if stageGate(nil, data) {
		t.Fatal("financing_confirmation must wait for the normal survey's financing page findings")
	}
	data[document.DocProcessLoanStructureLtvSubmission] = 0.8
	if !stageGate(nil, data) {
		t.Fatal("financing_confirmation should run once the normal survey's financing page's ltv_submission is present")
	}

	data = bothChecksPassed()
	data[document.DocProcessScoringStageToken] = "income_confirmation"
	if stageGate(nil, data) {
		t.Fatal("income_confirmation must wait for the high_risk survey's income page findings")
	}
	data[document.DocProcessIncomeVerifiedAmount] = 15000000.0
	if !stageGate(nil, data) {
		t.Fatal("income_confirmation should run once the income page's verified income is present")
	}

	data = bothChecksPassed()
	data[document.DocProcessScoringStageToken] = "complete_high_risk_survey"
	if stageGate(nil, data) {
		t.Fatal("complete_high_risk_survey must wait for the environment_check page's findings")
	}
	data[document.DocProcessEnvironmentCheckResult] = "good"
	if !stageGate(nil, data) {
		t.Fatal("complete_high_risk_survey should run once the environment_check page's result is present")
	}
}

func TestStageGateRejectsUnknownStage(t *testing.T) {
	data := bothChecksPassed()
	data[document.DocProcessScoringStageToken] = "future_stage"
	if stageGate(nil, data) {
		t.Fatal("unknown stages must fail safe")
	}
}

func TestStageGateBlocksUntilBothChecksPassed(t *testing.T) {
	base := func() map[common.HString]any {
		return map[common.HString]any{document.DocProcessScoringStageToken: "post_submission"}
	}

	data := base()
	data[document.DocProcessAgeCheckPassed] = true
	if stageGate(nil, data) {
		t.Fatal("must not run with only the age check passed")
	}

	data = base()
	data[document.DocProcessDuplicatePlateCheckPassed] = true
	if stageGate(nil, data) {
		t.Fatal("must not run with only the duplicate-plate check passed")
	}

	data = base()
	if stageGate(nil, data) {
		t.Fatal("must not run with neither check passed")
	}
}

func TestReadSetUsesCursorAsRequiredTrigger(t *testing.T) {
	readSet := common.MakeReadSet(requiredReadSet).SetOptionals(optionalReadSet, true)
	triggers := readSet.RollbackTriggerPaths()
	if len(triggers) != len(requiredReadSet) {
		t.Fatalf("expected only required paths to trigger rollback, got %v", triggers)
	}
	if triggers[4] != document.DocProcessScoringTriggerSeq {
		t.Fatalf("expected trigger sequence to be a required rollback path, got %q", triggers[4])
	}
}
