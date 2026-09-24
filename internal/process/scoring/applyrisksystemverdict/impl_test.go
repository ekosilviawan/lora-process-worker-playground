package applyrisksystemverdict

import (
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

	"lora-process-worker-playground/internal/process/document"
)

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
		"underwriting-v1":     "underwriting",
		"survey-normal-v1":    "normal",
		"survey-high-risk-v1": "high_risk",
	}
	for input, want := range tests {
		got, ok := surveyTypeForDataSet(input)
		if !ok || got != want {
			t.Fatalf("surveyTypeForDataSet(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	// The granular checkpoints, and the retired FINAL_REVIEW value it
	// replaced, are no longer independently selectable - RS asking for one of
	// them directly must fail safe, same as any other unknown set.
	for _, unsupported := range []string{"CUSTOMER_VERIFICATION", "ASSET_REVIEW", "FINANCING", "INCOME_REVIEW", "FINAL_REVIEW", "UNKNOWN_SET"} {
		if _, ok := surveyTypeForDataSet(unsupported); ok {
			t.Fatalf("surveyTypeForDataSet(%q) must fail safe: it is no longer an independently selectable survey type", unsupported)
		}
	}
}

func TestValidStatusAndValidRequiredDataSet(t *testing.T) {
	if !ValidStatus("approved") || ValidStatus("unknown") {
		t.Fatal("ValidStatus must accept known statuses and reject unknown ones")
	}
	if !ValidRequiredDataSet("underwriting-v1") || ValidRequiredDataSet("FINAL_REVIEW") {
		t.Fatal("ValidRequiredDataSet must accept underwriting-v1 and reject the retired FINAL_REVIEW value")
	}
}

func TestApplyVerdictPassesForHighRiskNdf4w(t *testing.T) {
	out, err := applyVerdict(map[common.HString]any{
		document.DocProcessScoringRiskSystemRequiredDataSet: "underwriting-v1",
		document.DocProcessScoringRiskSystemStatus:          "pending",
		document.DocProcessScoringRiskSystemMaxLtv:          0.6,
		document.DocProcessScoringStageToken:                "complete_high_risk_survey",
		document.DocProcessLoanStructureProductType:         "NDF4W",
	})
	if err != nil {
		t.Fatalf("applyVerdict: %v", err)
	}
	if out[document.DocProcessScoringSurveyType] != "underwriting" {
		t.Fatalf("survey_type = %v, want underwriting", out[document.DocProcessScoringSurveyType])
	}
	if out[document.DocStatus] != "processing" {
		t.Fatalf("$.status = %v, want processing", out[document.DocStatus])
	}
}

func TestApplyVerdictRequiresHighRiskPath(t *testing.T) {
	if _, err := applyVerdict(map[common.HString]any{
		document.DocProcessScoringRiskSystemRequiredDataSet: "underwriting-v1",
		document.DocProcessScoringRiskSystemStatus:          "pending",
		document.DocProcessScoringStageToken:                "complete_normal_survey",
		document.DocProcessLoanStructureProductType:         "NDF4W",
	}); err == nil {
		t.Fatal("underwriting requested after the normal path must be rejected")
	}
}

func TestApplyVerdictRequiresNdf4wProduct(t *testing.T) {
	if _, err := applyVerdict(map[common.HString]any{
		document.DocProcessScoringRiskSystemRequiredDataSet: "underwriting-v1",
		document.DocProcessScoringRiskSystemStatus:          "pending",
		document.DocProcessScoringStageToken:                "complete_high_risk_survey",
		document.DocProcessLoanStructureProductType:         "NDF2W",
	}); err == nil {
		t.Fatal("underwriting requested for an NDF2W applicant must be rejected")
	}
}

// TestApplyVerdictSkipsGateOnReconfirmation pins the fix for a real bug found
// via manual smoke testing (tc9): once survey_type has already transitioned
// to "underwriting", completing the underwriting survey's own page advances
// stage_token to "underwriting" itself. Risk System's next verdict (e.g. the
// final approve/reject) re-arms this step with the SAME required_data_set,
// but stage_token no longer equals "complete_high_risk_survey" - the gate
// must not re-fire and reject an already-accepted transition just because
// the survey it gated has since completed.
func TestApplyVerdictSkipsGateOnReconfirmation(t *testing.T) {
	out, err := applyVerdict(map[common.HString]any{
		document.DocProcessScoringRiskSystemRequiredDataSet: "underwriting-v1",
		document.DocProcessScoringRiskSystemStatus:          "approved",
		document.DocProcessScoringRiskSystemMaxLtv:          0.6,
		document.DocProcessScoringStageToken:                "underwriting",
		document.DocProcessLoanStructureProductType:         "NDF4W",
		document.DocProcessScoringSurveyType:                "underwriting",
	})
	if err != nil {
		t.Fatalf("applyVerdict: %v", err)
	}
	if out[document.DocStatus] != "approved" {
		t.Fatalf("$.status = %v, want approved", out[document.DocStatus])
	}
}

func TestApplyVerdictOtherSurveyTypesUnaffected(t *testing.T) {
	out, err := applyVerdict(map[common.HString]any{
		document.DocProcessScoringRiskSystemRequiredDataSet: "survey-normal-v1",
		document.DocProcessScoringRiskSystemStatus:          "pending",
		// Deliberately ineligible stage_token/product_type: the gate only
		// applies to survey_type=="underwriting", so this must still succeed.
		document.DocProcessScoringStageToken:        "post_submission",
		document.DocProcessLoanStructureProductType: "NDF2W",
	})
	if err != nil {
		t.Fatalf("applyVerdict: %v", err)
	}
	if out[document.DocProcessScoringSurveyType] != "normal" {
		t.Fatalf("survey_type = %v, want normal", out[document.DocProcessScoringSurveyType])
	}
}
