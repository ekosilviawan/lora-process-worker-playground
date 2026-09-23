package runsurvey

import (
	"testing"

	"lora-process-worker-playground/internal/process/document"
)

func TestSurveyOutcomesCoverEveryRequiredDataSet(t *testing.T) {
	// One outcome per survey type the required_data_set mapping in
	// applyrisksystemverdict can produce; a stage the mapping can select
	// but this table can't run would silently error every submission.
	wantTypes := []string{"identity", "asset", "financing", "income", "final_review"}
	for _, surveyType := range wantTypes {
		if _, ok := surveyOutcomesByType[surveyType]; !ok {
			t.Fatalf("no survey outcome registered for survey type %q", surveyType)
		}
	}
	if len(surveyOutcomesByType) != len(wantTypes) {
		t.Fatalf("got %d survey outcomes, want %d", len(surveyOutcomesByType), len(wantTypes))
	}
}

func TestSurveyOutcomesAdvanceToTheMatchingStage(t *testing.T) {
	tests := []struct {
		surveyType string
		wantStage  string
	}{
		{"identity", "customer_verification"},
		{"asset", "asset_review"},
		{"financing", "financing"},
		{"income", "income_review"},
		{"final_review", "final_review"},
	}
	for _, tt := range tests {
		outcome, ok := surveyOutcomesByType[tt.surveyType]
		if !ok {
			t.Fatalf("missing outcome for survey type %q", tt.surveyType)
		}
		if outcome.nextStage != tt.wantStage {
			t.Fatalf("surveyOutcomesByType[%q].nextStage = %q; want %q", tt.surveyType, outcome.nextStage, tt.wantStage)
		}
	}
}

func TestSurveyOutcomeFieldsMatchItsSurveyType(t *testing.T) {
	fields := surveyOutcomesByType["asset"].fields()
	if _, ok := fields[document.DocProcessAssetCondition]; !ok {
		t.Fatal("asset survey outcome must write the asset condition field")
	}
	if _, ok := fields[document.DocCustomerBirthDate]; ok {
		t.Fatal("asset survey outcome must not write identity fields")
	}
}
