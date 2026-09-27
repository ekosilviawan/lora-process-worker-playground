package survey

import (
	"reflect"
	"slices"
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

	"lora-process-worker-playground/internal/process/document"
)

func TestSurveyOutcomesCoverEveryRequiredDataSet(t *testing.T) {
	// One outcome per survey type the required_data_set mapping in
	// checkrisksystemverdict can produce; a stage the mapping can select
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

// TestSurveyOutcomesCompleteOnTheirFinalPageField pins that each outcome's
// completionField is written by its final page and by no earlier page -
// otherwise shouldCreateTask would close the task early.
func TestSurveyOutcomesCompleteOnTheirFinalPageField(t *testing.T) {
	tests := []struct {
		surveyType string
		want       common.HString
	}{
		{"underwriting", document.DocProcessUnderwritingConfirmed},
		{"normal", document.DocProcessIncomeVerifiedAmount},
		{"high_risk", document.DocProcessEnvironmentCheckResult},
	}
	for _, tt := range tests {
		outcome, ok := surveyOutcomesByType[tt.surveyType]
		if !ok {
			t.Fatalf("missing outcome for survey type %q", tt.surveyType)
		}
		if outcome.completionField != tt.want {
			t.Fatalf("surveyOutcomesByType[%q].completionField = %q; want %q", tt.surveyType, outcome.completionField, tt.want)
		}
		if _, ok := outcome.finalPage().fields()[tt.want]; !ok {
			t.Fatalf("%s's final page must write its completion field %q", tt.surveyType, tt.want)
		}
		for i, page := range outcome.pages[:len(outcome.pages)-1] {
			if _, ok := page.fields()[tt.want]; ok {
				t.Fatalf("%s page %d (%s) must not write the completion field %q", tt.surveyType, i, page.name, tt.want)
			}
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
// completions and whose last page alone closes the task.
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
		t.Fatal("normal survey's third page must revise ltv_submission (what re-asks Risk System after it)")
	}

	income := outcome.pages[3].fields()
	if _, ok := income[document.DocProcessIncomeVerifiedAmount]; !ok {
		t.Fatal("normal survey's final page must collect the verified income (its completion field)")
	}
}

// TestSurveyPagesNeverWriteTriggerSeq pins that no page writes a re-ask
// cursor: Risk System is re-asked because a page's own data set first
// appears (see checkrisksystem), so neither the retired trigger_seq nor the
// retired stage_token is written by any page or listed in writeSet.
func TestSurveyPagesNeverWriteTriggerSeq(t *testing.T) {
	for _, retired := range []common.HString{document.DocProcessScoringTriggerSeq, document.DocProcessScoringStageToken} {
		if slices.Contains(writeSet, retired) {
			t.Errorf("writeSet must not contain the retired cursor %q", retired)
		}
	}
	for surveyType, outcome := range surveyOutcomesByType {
		for pageIndex := range outcome.pages {
			fields, err := SurveyPageCompletion(surveyType, pageIndex)
			if err != nil {
				t.Fatalf("SurveyPageCompletion(%s, %d): %v", surveyType, pageIndex, err)
			}
			for _, retired := range []common.HString{document.DocProcessScoringTriggerSeq, document.DocProcessScoringStageToken} {
				if _, ok := fields[retired]; ok {
					t.Errorf("%s page %d must not write the retired cursor %q", surveyType, pageIndex, retired)
				}
			}
		}
	}
}

// TestSurveyPageCompletionIsThePageFindings pins that a page payload is that
// page's own findings, nothing more.
func TestSurveyPageCompletionIsThePageFindings(t *testing.T) {
	fields, err := SurveyPageCompletion("normal", 1)
	if err != nil {
		t.Fatalf("SurveyPageCompletion(normal, 1): %v", err)
	}
	if len(fields) != 1 || fields[document.DocProcessAssetCondition] != surveyAssetCondition {
		t.Fatalf("asset page payload = %v, want only asset.condition=%q", fields, surveyAssetCondition)
	}
}

func TestSurveyPageCompletionRejectsOutOfRangePage(t *testing.T) {
	if _, err := SurveyPageCompletion("normal", 4); err == nil {
		t.Fatal("expected an error for an out-of-range page index")
	}
	if _, err := SurveyPageCompletion("normal", -1); err == nil {
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
}

func TestSurveyOutcomeFieldsIsTheFinalPage(t *testing.T) {
	fields, err := SurveyOutcomeFields("normal")
	if err != nil {
		t.Fatalf("SurveyOutcomeFields: %v", err)
	}
	if fields[document.DocProcessIncomeVerifiedAmount] != surveyVerifiedIncome {
		t.Fatalf("normal outcome fields = %v, want the income page's verified income", fields)
	}
}

// TestNormalAndHighRiskSharePagesBeforeDiverging pins the structural
// guarantee mid-flow escalation depends on: "normal" and "high_risk" must
// collect identity/asset/financing identically (same name, same fields) so a
// document whose survey_type switches from "normal" to "high_risk" mid-flow
// (e.g. after Risk System re-assesses the applicant post-asset-page)
// never has to re-collect an already-submitted page. See
// standardSurveyPages's own comment for the full reasoning.
func TestNormalAndHighRiskSharePagesBeforeDiverging(t *testing.T) {
	normalPages := surveyOutcomesByType["normal"].pages
	highRiskPages := surveyOutcomesByType["high_risk"].pages
	for i := 0; i < 3; i++ {
		if normalPages[i].name != highRiskPages[i].name {
			t.Fatalf("page %d name diverges: normal=%q high_risk=%q", i, normalPages[i].name, highRiskPages[i].name)
		}
		if !reflect.DeepEqual(normalPages[i].fields(), highRiskPages[i].fields()) {
			t.Fatalf("page %d fields diverge: normal=%v high_risk=%v", i, normalPages[i].fields(), highRiskPages[i].fields())
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
		t.Fatal("high_risk survey's fourth page must collect the verified income")
	}

	environmentCheck := outcome.pages[4].fields()
	if _, ok := environmentCheck[document.DocProcessEnvironmentCheckResult]; !ok {
		t.Fatal("high_risk survey's final page must collect the environment check result (its completion field)")
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
	for i := range wantNames {
		if pages[i].Name != wantNames[i] {
			t.Fatalf("high_risk survey page %d name = %q, want %q", i, pages[i].Name, wantNames[i])
		}
	}
}

// TestShouldCreateTaskContinuesAcrossSurveyTypeEscalation is the pure-function
// proof behind the mid-flow escalation design: once Risk System rewrites
// survey_type from "normal" to "high_risk" partway through (the shared
// identity/asset/financing pages already collected), the task must stay
// open rather than being skipped - high_risk's own completion field
// (environment_check.result) isn't present yet, so there's more of high_risk
// left to collect.
func TestShouldCreateTaskContinuesAcrossSurveyTypeEscalation(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType: "high_risk",
	}
	if !shouldCreateTask(nil, data) {
		t.Fatal("escalating survey_type to high_risk mid-flow must keep the task open to collect high_risk's remaining pages")
	}
	// Even after "normal"'s own completion field lands, high_risk isn't done.
	data[document.DocProcessIncomeVerifiedAmount] = surveyVerifiedIncome
	if !shouldCreateTask(nil, data) {
		t.Fatal("high_risk must stay open until its own environment_check page is collected")
	}
}

func TestSurveyOutcomeFieldsRejectsUnknownSurveyType(t *testing.T) {
	if _, err := SurveyOutcomeFields("unknown"); err == nil {
		t.Fatal("expected an error for an unsupported survey type")
	}
}

func TestShouldCreateTaskFiresForANewSurveyType(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType:        "underwriting",
		document.DocProcessEnvironmentCheckResult:   surveyEnvironmentCheckResult,
		document.DocProcessLoanStructureProductType: "NDF4W",
	}
	if !shouldCreateTask(nil, data) {
		t.Fatal("a survey_type whose outcome hasn't been collected yet must create a task")
	}
}

func TestShouldCreateTaskSkipsAnAlreadyCollectedOutcome(t *testing.T) {
	// A step's own writes elsewhere in the process graph (e.g.
	// check_customer_eligibility_pg -> age_check_passed) can make this step
	// "impacted" again after it already completed for this survey_type -
	// this must not re-create a second, uncompletable task for the same
	// outcome.
	for surveyType, outcome := range surveyOutcomesByType {
		data := map[common.HString]any{
			document.DocProcessScoringSurveyType:        surveyType,
			document.DocProcessEnvironmentCheckResult:   surveyEnvironmentCheckResult,
			document.DocProcessLoanStructureProductType: "NDF4W",
			outcome.completionField:                     true,
		}
		if shouldCreateTask(nil, data) {
			t.Errorf("%s: must not create a task once its completion field %q is present", surveyType, outcome.completionField)
		}
	}
}

func TestShouldCreateTaskRejectsUnknownSurveyType(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType: "unknown",
	}
	if shouldCreateTask(nil, data) {
		t.Fatal("unknown survey types must fail safe")
	}
}

func TestIsUnderwritingEligible(t *testing.T) {
	tests := []struct {
		name string
		data map[common.HString]any
		want bool
	}{
		{"high_risk completed + NDF4W", map[common.HString]any{
			document.DocProcessEnvironmentCheckResult:   "good",
			document.DocProcessLoanStructureProductType: "NDF4W",
		}, true},
		{"normal completed, not high_risk", map[common.HString]any{
			document.DocProcessIncomeVerifiedAmount:     surveyVerifiedIncome,
			document.DocProcessLoanStructureProductType: "NDF4W",
		}, false},
		{"high_risk completed but NDF2W", map[common.HString]any{
			document.DocProcessEnvironmentCheckResult:   "good",
			document.DocProcessLoanStructureProductType: "NDF2W",
		}, false},
		{"neither condition met", map[common.HString]any{
			document.DocProcessLoanStructureProductType: "NDF2W",
		}, false},
	}
	for _, tt := range tests {
		if got := IsUnderwritingEligible(tt.data); got != tt.want {
			t.Errorf("%s: IsUnderwritingEligible = %v, want %v", tt.name, got, tt.want)
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
		document.DocProcessIncomeVerifiedAmount:     surveyVerifiedIncome,
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
			document.DocProcessEnvironmentCheckResult:   "good",
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
// checkrisksystemverdict's own gate), this must still refuse to open the
// task rather than treating a missing field as a pass.
func TestShouldCreateTaskDefendsAgainstIneligibleUnderwriting(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessScoringSurveyType:      "underwriting",
		document.DocProcessEnvironmentCheckResult: "good",
	}
	if shouldCreateTask(nil, data) {
		t.Fatal("must fail safe when product_type is missing, not treat it as eligible")
	}
}
