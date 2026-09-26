package checkrisksystemverdict

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
		document.DocProcessEnvironmentCheckResult:           "good",
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

// TestApplyVerdictRequiresHighRiskPath covers both the normal path (which
// never writes environment_check.result) and an underwriting request that
// arrives before the high_risk survey has finished.
func TestApplyVerdictRequiresHighRiskPath(t *testing.T) {
	if _, err := applyVerdict(map[common.HString]any{
		document.DocProcessScoringRiskSystemRequiredDataSet: "underwriting-v1",
		document.DocProcessScoringRiskSystemStatus:          "pending",
		document.DocProcessIncomeVerifiedAmount:             15000000.0,
		document.DocProcessLoanStructureProductType:         "NDF4W",
	}); err == nil {
		t.Fatal("underwriting requested without a completed high_risk survey must be rejected")
	}
}

func TestApplyVerdictRequiresNdf4wProduct(t *testing.T) {
	if _, err := applyVerdict(map[common.HString]any{
		document.DocProcessScoringRiskSystemRequiredDataSet: "underwriting-v1",
		document.DocProcessScoringRiskSystemStatus:          "pending",
		document.DocProcessEnvironmentCheckResult:           "good",
		document.DocProcessLoanStructureProductType:         "NDF2W",
	}); err == nil {
		t.Fatal("underwriting requested for an NDF2W applicant must be rejected")
	}
}

// TestApplyVerdictGateStillHoldsAfterUnderwriting pins that the gate is
// derived from collected data, not a cursor that moves on: a verdict arriving
// after the underwriting page itself (e.g. the final approval) still sees
// environment_check.result and product_type unchanged, so it passes.
func TestApplyVerdictGateStillHoldsAfterUnderwriting(t *testing.T) {
	out, err := applyVerdict(map[common.HString]any{
		document.DocProcessScoringRiskSystemRequiredDataSet: "underwriting-v1",
		document.DocProcessScoringRiskSystemStatus:          "approved",
		document.DocProcessScoringRiskSystemMaxLtv:          0.6,
		document.DocProcessEnvironmentCheckResult:           "good",
		document.DocProcessLoanStructureProductType:         "NDF4W",
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
		// Deliberately ineligible (no environment_check, NDF2W): the gate only
		// applies to survey_type=="underwriting", so this must still succeed.
		document.DocProcessLoanStructureProductType: "NDF2W",
	})
	if err != nil {
		t.Fatalf("applyVerdict: %v", err)
	}
	if out[document.DocProcessScoringSurveyType] != "normal" {
		t.Fatalf("survey_type = %v, want normal", out[document.DocProcessScoringSurveyType])
	}
}
