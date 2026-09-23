package checkrisksystem

import (
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

	"lora-process-worker-playground/internal/process/document"
)

func TestStageGate(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringStageToken: "customer_verification",
	}
	if stageGate(nil, data) {
		t.Fatal("customer verification must wait for birth date")
	}

	data[document.DocCustomerBirthDate] = "1985-03-15"
	if !stageGate(nil, data) {
		t.Fatal("customer verification should run once its gate field is present")
	}
}

func TestStageGateFiresImmediatelyForPostSubmission(t *testing.T) {
	if !stageGate(nil, map[common.HString]any{document.DocProcessScoringStageToken: "post_submission"}) {
		t.Fatal("post_submission has no gate fields and must run before any survey")
	}
}

func TestStageGateSupportsLaterCheckpoints(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringStageToken: "asset_review",
	}
	if stageGate(nil, data) {
		t.Fatal("asset review must wait for the asset survey's findings")
	}
	data[document.DocProcessAssetCondition] = "fair"
	if !stageGate(nil, data) {
		t.Fatal("asset review should run once its gate field is present")
	}

	data = map[common.HString]any{
		document.DocProcessScoringStageToken:          "financing",
		document.DocProcessLoanStructureLtvSubmission: 0.8,
	}
	if !stageGate(nil, data) {
		t.Fatal("financing should run once the financing survey's findings are present")
	}

	data[document.DocProcessScoringStageToken] = "income_review"
	data[document.DocProcessIncomeVerifiedAmount] = 15000000.0
	if !stageGate(nil, data) {
		t.Fatal("income review should run once the income survey's findings are present")
	}

	data[document.DocProcessScoringStageToken] = "final_review"
	data[document.DocProcessFinalReviewConfirmed] = true
	if !stageGate(nil, data) {
		t.Fatal("final review should run once its confirmation is present")
	}
}

func TestStageGateRejectsUnknownStage(t *testing.T) {
	if stageGate(nil, map[common.HString]any{
		document.DocProcessScoringStageToken: "future_stage",
	}) {
		t.Fatal("unknown stages must fail safe")
	}
}

func TestReadSetUsesCursorAsRequiredTrigger(t *testing.T) {
	readSet := common.MakeReadSet(requiredReadSet).SetOptionals(optionalReadSet, true)
	triggers := readSet.RollbackTriggerPaths()
	if len(triggers) != len(requiredReadSet) {
		t.Fatalf("expected only required paths to trigger rollback, got %v", triggers)
	}
	if triggers[3] != document.DocProcessScoringTriggerSeq {
		t.Fatalf("expected trigger sequence to be a required rollback path, got %q", triggers[3])
	}
}

func TestTranslateStatus(t *testing.T) {
	tests := map[string]string{
		"approved": "approved",
		"rejected": "rejected",
		"pending":  "processing",
	}
	for input, want := range tests {
		got, ok := translateStatus(input)
		if !ok || got != want {
			t.Fatalf("translateStatus(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	if _, ok := translateStatus("unknown"); ok {
		t.Fatal("unknown verdict status must be rejected")
	}
}

func TestSurveyTypeForDataSet(t *testing.T) {
	tests := map[string]string{
		"CUSTOMER_VERIFICATION": "identity",
		"ASSET_REVIEW":          "asset",
		"FINANCING":             "financing",
		"INCOME_REVIEW":         "income",
		"FINAL_REVIEW":          "final_review",
	}
	for input, want := range tests {
		got, ok := surveyTypeForDataSet(input)
		if !ok || got != want {
			t.Fatalf("surveyTypeForDataSet(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	if _, ok := surveyTypeForDataSet("UNKNOWN_SET"); ok {
		t.Fatal("an unrecognised required data set must fail safe, not guess a survey type")
	}
}
