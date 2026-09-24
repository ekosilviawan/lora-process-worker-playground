package survey

import (
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

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

func TestSurveyOutcomeFieldsWritesCursor(t *testing.T) {
	fields, err := SurveyOutcomeFields("identity", 3)
	if err != nil {
		t.Fatalf("SurveyOutcomeFields: %v", err)
	}
	if fields[document.DocProcessScoringTriggerSeq] != 3 {
		t.Fatalf("trigger_seq = %v, want 3", fields[document.DocProcessScoringTriggerSeq])
	}
	if fields[document.DocProcessScoringStageToken] != "customer_verification" {
		t.Fatalf("stage_token = %v, want customer_verification", fields[document.DocProcessScoringStageToken])
	}
}

func TestSurveyOutcomeFieldsRejectsUnknownSurveyType(t *testing.T) {
	if _, err := SurveyOutcomeFields("unknown", 0); err == nil {
		t.Fatal("expected an error for an unsupported survey type")
	}
}

func TestShouldCreateTaskFiresForANewSurveyType(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType: "identity",
		document.DocProcessScoringStageToken: "post_submission",
	}
	if !shouldCreateTask(nil, data) {
		t.Fatal("a survey_type whose stage hasn't been produced yet must create a task")
	}
}

func TestShouldCreateTaskSkipsAnAlreadyProducedStage(t *testing.T) {
	// The survey's own writes (customer.birth_date ->
	// check_customer_eligibility_pg -> age_check_passed) can make this step
	// "impacted" again after it already completed for this survey_type -
	// this must not re-create a second, uncompletable task for the same
	// stage.
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType: "identity",
		document.DocProcessScoringStageToken: "customer_verification",
	}
	if shouldCreateTask(nil, data) {
		t.Fatal("must not create a task once stage_token already reflects this survey_type's outcome")
	}
}

func TestShouldCreateTaskRejectsUnknownSurveyType(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType: "unknown",
		document.DocProcessScoringStageToken: "post_submission",
	}
	if shouldCreateTask(nil, data) {
		t.Fatal("unknown survey types must fail safe")
	}
}
