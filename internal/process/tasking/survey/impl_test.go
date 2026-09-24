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
	wantTypes := []string{"underwriting", "normal", "high_risk"}
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
		{"underwriting", "underwriting"},
		{"normal", "complete_normal_survey"},
		{"high_risk", "complete_high_risk_survey"},
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
	fields := surveyOutcomesByType["underwriting"].finalPage().fields()
	if _, ok := fields[document.DocProcessUnderwritingConfirmed]; !ok {
		t.Fatal("underwriting survey outcome must write the underwriting confirmation field")
	}
	if _, ok := fields[document.DocCustomerBirthDate]; ok {
		t.Fatal("underwriting survey outcome must not write identity fields")
	}
}

// TestNormalSurveyIsMultiPage pins the shape of the survey-normal-v1 mapping:
// a single multi-page SURVEY process whose first pages are partial
// completions and whose last page alone advances the cursor.
func TestNormalSurveyIsMultiPage(t *testing.T) {
	outcome, ok := surveyOutcomesByType["normal"]
	if !ok {
		t.Fatal("missing survey outcome for the multi-page 'normal' survey type")
	}
	if len(outcome.pages) != 4 {
		t.Fatalf("normal survey must span 4 pages, got %d", len(outcome.pages))
	}
	for i, page := range outcome.pages {
		if page.final != (i == len(outcome.pages)-1) {
			t.Fatalf("normal survey page %d final=%v; only the last page may be final", i, page.final)
		}
	}

	identity := outcome.pages[0].fields()
	if _, ok := identity[document.DocCustomerBirthDate]; !ok {
		t.Fatal("normal survey page 1 must collect the customer birth date")
	}
	if _, ok := identity[document.DocCustomerName]; !ok {
		t.Fatal("normal survey page 1 must collect the customer name")
	}

	financing := outcome.pages[2].fields()
	if _, ok := financing[document.DocProcessLoanStructureLtvSubmission]; !ok {
		t.Fatal("normal survey's third page must collect ltv_submission (the financing_confirmation stage gate)")
	}

	income := outcome.pages[3].fields()
	if _, ok := income[document.DocProcessIncomeVerifiedAmount]; !ok {
		t.Fatal("normal survey's final page must collect the verified income (its stage gate)")
	}
}

// TestNormalSurveyAdvancesCursorPerPage pins the key property behind tc8:
// every page of the multi-page survey writes trigger_seq and stage_token, so
// each page's submission re-arms check_risk_system_pg (whose required reads
// are exactly trigger_seq + stage_token), not just the final page.
func TestNormalSurveyAdvancesCursorPerPage(t *testing.T) {
	wantStages := []string{"customer_verification", "asset_review", "financing_confirmation", "complete_normal_survey"}
	for pageIndex, wantStage := range wantStages {
		fields, err := SurveyPageCompletion("normal", pageIndex, pageIndex+1)
		if err != nil {
			t.Fatalf("SurveyPageCompletion(normal, %d): %v", pageIndex, err)
		}
		if fields[document.DocProcessScoringTriggerSeq] != pageIndex+1 {
			t.Fatalf("page %d trigger_seq = %v, want %d", pageIndex, fields[document.DocProcessScoringTriggerSeq], pageIndex+1)
		}
		if fields[document.DocProcessScoringStageToken] != wantStage {
			t.Fatalf("page %d stage_token = %v, want %q", pageIndex, fields[document.DocProcessScoringStageToken], wantStage)
		}
	}
}

func TestSurveyPageCompletionRejectsOutOfRangePage(t *testing.T) {
	if _, err := SurveyPageCompletion("normal", 4, 1); err == nil {
		t.Fatal("expected an error for an out-of-range page index")
	}
	if _, err := SurveyPageCompletion("normal", -1, 1); err == nil {
		t.Fatal("expected an error for a negative page index")
	}
}

func TestSurveyPagesExposesMultiPageOrder(t *testing.T) {
	pages, err := SurveyPages("normal")
	if err != nil {
		t.Fatalf("SurveyPages(normal): %v", err)
	}
	if len(pages) != 4 {
		t.Fatalf("SurveyPages(normal) = %d pages, want 4", len(pages))
	}
	if pages[0].Final || pages[1].Final || pages[2].Final {
		t.Fatal("the first three normal survey pages must be partial completions")
	}
	if !pages[3].Final {
		t.Fatal("the last normal survey page must be the final completion")
	}
	if pages[0].Name != "identity" || pages[1].Name != "asset" || pages[2].Name != "financing" || pages[3].Name != "income" {
		t.Fatalf("unexpected normal survey page order: %q, %q, %q, %q", pages[0].Name, pages[1].Name, pages[2].Name, pages[3].Name)
	}
	wantStages := []string{"customer_verification", "asset_review", "financing_confirmation", "complete_normal_survey"}
	for i, wantStage := range wantStages {
		if pages[i].Stage != wantStage {
			t.Fatalf("normal survey page %d stage = %q, want %q", i, pages[i].Stage, wantStage)
		}
	}
}

func TestSurveyOutcomeFieldsWritesCursor(t *testing.T) {
	fields, err := SurveyOutcomeFields("normal", 3)
	if err != nil {
		t.Fatalf("SurveyOutcomeFields: %v", err)
	}
	if fields[document.DocProcessScoringTriggerSeq] != 3 {
		t.Fatalf("trigger_seq = %v, want 3", fields[document.DocProcessScoringTriggerSeq])
	}
	if fields[document.DocProcessScoringStageToken] != "complete_normal_survey" {
		t.Fatalf("stage_token = %v, want complete_normal_survey", fields[document.DocProcessScoringStageToken])
	}
}

// TestNormalAndHighRiskSharePagesBeforeDiverging pins the structural
// guarantee mid-flow escalation depends on: "normal" and "high_risk" must
// collect identity/asset/financing identically (same name, same stage_token)
// so a document whose survey_type switches from "normal" to "high_risk"
// mid-flow (e.g. after Risk System re-assesses the applicant post-asset-page)
// never has to re-collect an already-submitted page. See
// standardSurveyPages's own comment for the full reasoning.
func TestNormalAndHighRiskSharePagesBeforeDiverging(t *testing.T) {
	normalPages := surveyOutcomesByType["normal"].pages
	highRiskPages := surveyOutcomesByType["high_risk"].pages
	for i := 0; i < 3; i++ {
		if normalPages[i].name != highRiskPages[i].name {
			t.Fatalf("page %d name diverges: normal=%q high_risk=%q", i, normalPages[i].name, highRiskPages[i].name)
		}
		if normalPages[i].stage != highRiskPages[i].stage {
			t.Fatalf("page %d stage diverges: normal=%q high_risk=%q", i, normalPages[i].stage, highRiskPages[i].stage)
		}
	}
}

// TestHighRiskSurveyIsMultiPage mirrors TestNormalSurveyIsMultiPage: pins the
// shape of the survey-high-risk-v1 mapping, "normal"'s four pages plus a
// fifth, final environment_check page.
func TestHighRiskSurveyIsMultiPage(t *testing.T) {
	outcome, ok := surveyOutcomesByType["high_risk"]
	if !ok {
		t.Fatal("missing survey outcome for the multi-page 'high_risk' survey type")
	}
	if len(outcome.pages) != 5 {
		t.Fatalf("high_risk survey must span 5 pages, got %d", len(outcome.pages))
	}
	for i, page := range outcome.pages {
		if page.final != (i == len(outcome.pages)-1) {
			t.Fatalf("high_risk survey page %d final=%v; only the last page may be final", i, page.final)
		}
	}

	income := outcome.pages[3].fields()
	if _, ok := income[document.DocProcessIncomeVerifiedAmount]; !ok {
		t.Fatal("high_risk survey's fourth page must collect the verified income (the income_confirmation stage gate)")
	}

	environmentCheck := outcome.pages[4].fields()
	if _, ok := environmentCheck[document.DocProcessEnvironmentCheckResult]; !ok {
		t.Fatal("high_risk survey's final page must collect the environment check result (its stage gate)")
	}
}

// TestHighRiskSurveyAdvancesCursorPerPage mirrors TestNormalSurveyAdvancesCursorPerPage.
func TestHighRiskSurveyAdvancesCursorPerPage(t *testing.T) {
	wantStages := []string{"customer_verification", "asset_review", "financing_confirmation", "income_confirmation", "complete_high_risk_survey"}
	for pageIndex, wantStage := range wantStages {
		fields, err := SurveyPageCompletion("high_risk", pageIndex, pageIndex+1)
		if err != nil {
			t.Fatalf("SurveyPageCompletion(high_risk, %d): %v", pageIndex, err)
		}
		if fields[document.DocProcessScoringTriggerSeq] != pageIndex+1 {
			t.Fatalf("page %d trigger_seq = %v, want %d", pageIndex, fields[document.DocProcessScoringTriggerSeq], pageIndex+1)
		}
		if fields[document.DocProcessScoringStageToken] != wantStage {
			t.Fatalf("page %d stage_token = %v, want %q", pageIndex, fields[document.DocProcessScoringStageToken], wantStage)
		}
	}
}

// TestSurveyPagesExposesHighRiskPageOrder mirrors TestSurveyPagesExposesMultiPageOrder.
func TestSurveyPagesExposesHighRiskPageOrder(t *testing.T) {
	pages, err := SurveyPages("high_risk")
	if err != nil {
		t.Fatalf("SurveyPages(high_risk): %v", err)
	}
	if len(pages) != 5 {
		t.Fatalf("SurveyPages(high_risk) = %d pages, want 5", len(pages))
	}
	for i := 0; i < 4; i++ {
		if pages[i].Final {
			t.Fatalf("high_risk survey page %d must be a partial completion", i)
		}
	}
	if !pages[4].Final {
		t.Fatal("the last high_risk survey page must be the final completion")
	}
	wantNames := []string{"identity", "asset", "financing", "income", "environment_check"}
	wantStages := []string{"customer_verification", "asset_review", "financing_confirmation", "income_confirmation", "complete_high_risk_survey"}
	for i := range wantNames {
		if pages[i].Name != wantNames[i] {
			t.Fatalf("high_risk survey page %d name = %q, want %q", i, pages[i].Name, wantNames[i])
		}
		if pages[i].Stage != wantStages[i] {
			t.Fatalf("high_risk survey page %d stage = %q, want %q", i, pages[i].Stage, wantStages[i])
		}
	}
}

// TestHighRiskSurveyOutcomeFieldsWritesCursor mirrors TestSurveyOutcomeFieldsWritesCursor.
func TestHighRiskSurveyOutcomeFieldsWritesCursor(t *testing.T) {
	fields, err := SurveyOutcomeFields("high_risk", 5)
	if err != nil {
		t.Fatalf("SurveyOutcomeFields: %v", err)
	}
	if fields[document.DocProcessScoringTriggerSeq] != 5 {
		t.Fatalf("trigger_seq = %v, want 5", fields[document.DocProcessScoringTriggerSeq])
	}
	if fields[document.DocProcessScoringStageToken] != "complete_high_risk_survey" {
		t.Fatalf("stage_token = %v, want complete_high_risk_survey", fields[document.DocProcessScoringStageToken])
	}
}

// TestShouldCreateTaskContinuesAcrossSurveyTypeEscalation is the pure-function
// proof behind the mid-flow escalation design: once Risk System rewrites
// survey_type from "normal" to "high_risk" partway through (stage_token
// already at asset_review, from the shared asset page), the task must stay
// open/continue rather than being skipped - asset_review isn't high_risk's
// own nextStage, so there's more of high_risk left to collect.
func TestShouldCreateTaskContinuesAcrossSurveyTypeEscalation(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType: "high_risk",
		document.DocProcessScoringStageToken: "asset_review",
	}
	if !shouldCreateTask(nil, data) {
		t.Fatal("escalating survey_type to high_risk mid-flow must keep the task open to collect high_risk's remaining pages")
	}
}

func TestSurveyOutcomeFieldsRejectsUnknownSurveyType(t *testing.T) {
	if _, err := SurveyOutcomeFields("unknown", 0); err == nil {
		t.Fatal("expected an error for an unsupported survey type")
	}
}

func TestShouldCreateTaskFiresForANewSurveyType(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType:        "underwriting",
		document.DocProcessScoringStageToken:        "complete_high_risk_survey",
		document.DocProcessLoanStructureProductType: "NDF4W",
	}
	if !shouldCreateTask(nil, data) {
		t.Fatal("a survey_type whose stage hasn't been produced yet must create a task")
	}
}

func TestShouldCreateTaskSkipsAnAlreadyProducedStage(t *testing.T) {
	// A step's own writes elsewhere in the process graph (e.g.
	// check_customer_eligibility_pg -> age_check_passed) can make this step
	// "impacted" again after it already completed for this survey_type -
	// this must not re-create a second, uncompletable task for the same
	// stage.
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType: "underwriting",
		document.DocProcessScoringStageToken: "underwriting",
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

func TestIsUnderwritingEligible(t *testing.T) {
	tests := []struct {
		name        string
		stageToken  string
		productType string
		want        bool
	}{
		{"high_risk completed + NDF4W", "complete_high_risk_survey", "NDF4W", true},
		{"normal completed, not high_risk", "complete_normal_survey", "NDF4W", false},
		{"high_risk completed but NDF2W", "complete_high_risk_survey", "NDF2W", false},
		{"neither condition met", "post_submission", "NDF2W", false},
	}
	for _, tt := range tests {
		if got := IsUnderwritingEligible(tt.stageToken, tt.productType); got != tt.want {
			t.Errorf("%s: IsUnderwritingEligible(%q, %q) = %v, want %v", tt.name, tt.stageToken, tt.productType, got, tt.want)
		}
	}
}

// TestUnderwritingRequiresHighRiskPath pins that shouldCreateTask's own
// defense-in-depth copy of the eligibility gate refuses to open the
// underwriting SURVEY task for a document that completed the "normal" path,
// even with an otherwise-eligible product_type.
func TestUnderwritingRequiresHighRiskPath(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType:        "underwriting",
		document.DocProcessScoringStageToken:        "complete_normal_survey",
		document.DocProcessLoanStructureProductType: "NDF4W",
	}
	if shouldCreateTask(nil, data) {
		t.Fatal("underwriting must never be reachable from the normal survey path")
	}
}

// TestUnderwritingRequiresNdf4wProduct pins the product half of the gate.
func TestUnderwritingRequiresNdf4wProduct(t *testing.T) {
	tests := []struct {
		productType string
		want        bool
	}{
		{"NDF2W", false},
		{"NDF4W", true},
	}
	for _, tt := range tests {
		data := map[common.HString]any{
			document.DocProcessScoringSurveyType:        "underwriting",
			document.DocProcessScoringStageToken:        "complete_high_risk_survey",
			document.DocProcessLoanStructureProductType: tt.productType,
		}
		if got := shouldCreateTask(nil, data); got != tt.want {
			t.Errorf("product_type=%q: shouldCreateTask = %v, want %v", tt.productType, got, tt.want)
		}
	}
}

// TestShouldCreateTaskDefendsAgainstIneligibleUnderwriting pins the
// fail-safe-on-missing-data idiom: if survey_type somehow reaches
// "underwriting" without product_type present at all (it should never - see
// applyrisksystemverdict's own gate), this must still refuse to open the
// task rather than treating a missing field as a pass.
func TestShouldCreateTaskDefendsAgainstIneligibleUnderwriting(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType: "underwriting",
		document.DocProcessScoringStageToken: "complete_high_risk_survey",
	}
	if shouldCreateTask(nil, data) {
		t.Fatal("must fail safe when product_type is missing, not treat it as eligible")
	}
}
