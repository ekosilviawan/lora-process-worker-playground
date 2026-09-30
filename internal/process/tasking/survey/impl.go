package survey

import (
	"fmt"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
	taskdefs "github.com/bfi-finance/lora-process-sdk/framework/task/defs"
	"go.temporal.io/sdk/workflow"

	"lora-process-worker-playground/internal/process/document"
)

const ProcessAndActivityName = "survey"

// TaskName identifies this as a real, signal-gated Temporal task (via
// system.CreateTaskFunction), the same way LPW's "SURVEY" task works in
// production (lora-partnership-ndf/internal/process/tasking/survey, which
// this package now mirrors path-for-path). It only completes when an
// explicit "task-completion" update arrives (see internal/tasksim and
// cmd/testcli's "complete-survey" command) - never as a side effect of some
// other document field changing.
const TaskName = "SURVEY"

// readSet depends on survey_type so the survey never runs before Risk
// System's first (pre-survey) verdict has told LORA which survey the user needs
// to complete next (checkrisksystemverdict.check_risk_system_pg_verdict
// writes survey_type from the verdict's required_data_set, once
// check_risk_system_pg has recorded it) - transitively this also can't happen before
// both initial checks have passed, since check_risk_system_pg itself never
// runs (and so nothing sets survey_type) until checkrisksystem.initialGate
// sees both checks pass.
var readSet = []common.HString{
	document.DocProcessScoringSurveyType,
}

// optionalReadSet mirrors writeSet's own fields (plus Risk System's max_ltv)
// as non-rollback-triggering context: a real form shows the human what's
// already on file - previously-collected customer/asset/loan data, and RS's
// lending constraint - alongside whatever this stage is asking them to
// confirm or supply. Fields also present in writeSet become "pre-filled,
// editable" once a real form renders WorkerTaskData.Read/.Write together;
// TriggerRollback stays false (default) on every entry except max_funding,
// so none of this reintroduces the self-referential cascade (this step's
// own writes, e.g. customer.birth_date, are also read back here) the
// completion-gate precondition below already guards against.
//
// max_funding is the one exception: unlike the rest of this list, it isn't
// raw context the human can eyeball and correct - it's calculateriskfunding's
// computed output, and the underwriting survey's verificator reads it
// straight to the customer. OptionalWaitIfLocked (matching calculateriskfunding's
// own read of ltv_max) keeps this step from running off a value mid-recompute
// in the same tick, and TriggerRollback: true means a later RS cap change
// (which reruns calculateriskfunding and changes max_funding) re-impacts this
// step so the verificator sees the corrected figure - SetRetainDataOnRollback
// below already protects any survey answers already submitted from being wiped
// out by that rollback.
var optionalReadSet = []common.OptionalPath{
	{Path: document.DocCustomerBirthDate, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocCustomerName, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessAssetCondition, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessLoanStructureProvisionalAmount, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessLoanStructureLtvSubmission, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessIncomeVerifiedAmount, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessUnderwritingConfirmed, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessEnvironmentCheckResult, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessLoanStructureLtvMax, Strategy: common.OptionalWaitIfLocked, TriggerRollback: true},
	{Path: document.DocProcessLoanStructureMaxFunding, Strategy: common.OptionalWaitIfLocked, TriggerRollback: true},
}

// writeSet is the union of every survey type's findings, exactly what a real
// submitted form's completion payload would contain - no re-trigger cursor. A
// page whose findings are first-time data (asset, income, environment_check,
// underwriting) asks Risk System on its own: each of those fields gates a
// checkrisksystem step of its own, which becomes runnable the moment the
// field first appears, and its pending read lock on that field keeps this
// step from opening the next page until a verdict lands. Most of these
// fields are first set here, but customer.birth_date/name and
// provisional_amount/
// ltv_submission are the exception: they arrive with the initial DP
// submission (testcli's cmdInject seeds them, mirroring production's
// pre_scoring - see calculateriskfunding, whose mandatory readSet depends on
// them existing before any survey runs). The financing outcome below only
// revises them, matching production's "surveyor negotiation" update path -
// it does not originate them. Revising data RS was already sent re-triggers
// it: those fields are rollback-triggering reads of every checkrisksystem
// stage, and the most advanced stage that has answered asks again.
var writeSet = []common.HString{
	document.DocCustomerBirthDate,
	document.DocCustomerName,
	document.DocProcessAssetCondition,
	document.DocProcessLoanStructureProvisionalAmount,
	document.DocProcessLoanStructureLtvSubmission,
	document.DocProcessIncomeVerifiedAmount,
	document.DocProcessUnderwritingConfirmed,
	document.DocProcessEnvironmentCheckResult,
}

// Hardcoded survey findings, one set per survey type - the canned "human"
// answers cmd/testcli submits as a task completion.
const (
	surveyVerifiedBirthDate      = "1985-03-15"
	surveyVerifiedName           = "Jane Smith"
	surveyAssetCondition         = "fair"
	surveyProvisionalAmount      = 100000.0
	surveyLtvSubmission          = 0.8
	surveyVerifiedIncome         = 15000000.0
	surveyEnvironmentCheckResult = "good"
	surveyUnderwritingConfirm    = true
)

// surveyPage is one form page of a survey process. A single-page survey has
// exactly one page, marked final; a multi-page survey (the "normal" survey
// that required_data_set=survey-normal-v1 maps to) has several pages, only
// the last of which is final - each non-final page's submission is a partial
// completion (WorkerTaskCompletionData.Action == "partial") that leaves the
// task open for the next page, matching EnablePartialCompletion on the step.
type surveyPage struct {
	name   string
	fields func() map[common.HString]any
	final  bool
}

// surveyOutcome is a whole survey process: an ordered list of pages plus the
// document field whose presence means the process is done. completionField
// is always a field only the final page writes, so it's derived from real
// collected data rather than a cursor. Adding a new survey type means adding
// one entry here - nothing else in the chain changes.
type surveyOutcome struct {
	completionField common.HString
	pages           []surveyPage
}

// finalPage returns the process's last page - the one whose submission also
// closes the task.
func (o surveyOutcome) finalPage() surveyPage { return o.pages[len(o.pages)-1] }

// singlePage builds a one-page survey process. "underwriting" is the only
// remaining single-page survey type: identity/asset/financing/income used to
// each have their own single-page entry here too, but are no longer
// independently selectable - they're only reachable as pages within the
// multi-page "normal" survey process below.
func singlePage(name string, fields func() map[common.HString]any) []surveyPage {
	return []surveyPage{{name: name, fields: fields, final: true}}
}

// standardSurveyPages returns the three pages every multi-page survey
// process shares verbatim: identity, asset, and financing. They write the
// same fields under every outcome that includes them (today: "normal" and
// "high_risk") - that's what lets a document's survey_type escalate mid-flow
// (e.g. "normal" -> "high_risk" once Risk System re-assesses the applicant
// after the asset page) without re-collecting pages already submitted
// under the old survey type: shouldCreateTask only ever checks whether the
// CURRENT survey_type's completionField is present, so data a shared page
// already collected simply stays collected. Each caller appends its own
// remaining page(s) - every survey type's income page (and anything after
// it) decides its own finality, since that's exactly where outcomes diverge.
func standardSurveyPages() []surveyPage {
	return []surveyPage{
		{
			name: "identity",
			fields: func() map[common.HString]any {
				return map[common.HString]any{
					document.DocCustomerBirthDate: surveyVerifiedBirthDate,
					document.DocCustomerName:      surveyVerifiedName,
				}
			},
		},
		{
			name: "asset",
			fields: func() map[common.HString]any {
				return map[common.HString]any{document.DocProcessAssetCondition: surveyAssetCondition}
			},
		},
		{
			// Revises provisional_amount/ltv_submission, doesn't originate
			// them - see the writeSet comment above.
			name: "financing",
			fields: func() map[common.HString]any {
				return map[common.HString]any{
					document.DocProcessLoanStructureProvisionalAmount: surveyProvisionalAmount,
					document.DocProcessLoanStructureLtvSubmission:     surveyLtvSubmission,
				}
			},
		},
	}
}

var surveyOutcomesByType = map[string]surveyOutcome{
	// "underwriting" is required_data_set=underwriting-v1's outcome. Unlike
	// every other entry in this table, reachability isn't governed solely by
	// completionField: checkrisksystemverdict additionally requires the
	// applicant completed the "high_risk" survey (environment_check.result is
	// present) and the loan's product is NDF4W before it will even write
	// survey_type=underwriting - see IsUnderwritingEligible and
	// shouldCreateTask's own defense-in-depth copy of the same check below.
	// checkrisksystem's asyncHandler has no read access to current document
	// state (it only ever sees the raw verdict payload), which is exactly why
	// that validation lives in checkrisksystemverdict instead.
	"underwriting": {
		completionField: document.DocProcessUnderwritingConfirmed,
		pages: singlePage("underwriting", func() map[common.HString]any {
			return map[common.HString]any{document.DocProcessUnderwritingConfirmed: surveyUnderwritingConfirm}
		}),
	},
	// "normal" is the multi-page survey process required_data_set=
	// survey-normal-v1 selects: four form pages (identity, asset, financing,
	// income) collected in one SURVEY task. The first three pages are partial
	// completions; only the final (income) page closes the task, and its
	// income.verified_amount is what marks "normal" as done.
	//
	// Every page asks Risk System: the asset and income pages because their
	// fields first appearing make check_risk_system_pg_asset/_income runnable,
	// the identity and financing pages because they revise data RS was
	// already sent, which re-triggers the most advanced stage so far. While
	// that call is pending it read-locks fields in this step's writeSet, so
	// the next page's task cannot be re-created until a verdict completes
	// it. The first three pages come
	// from standardSurveyPages() - identity, asset, and financing are
	// collected identically whether the applicant ends up on "normal" or
	// "high_risk" (see that function's comment).
	"normal": {
		completionField: document.DocProcessIncomeVerifiedAmount,
		pages: append(standardSurveyPages(), surveyPage{
			name:  "income",
			final: true,
			fields: func() map[common.HString]any {
				return map[common.HString]any{document.DocProcessIncomeVerifiedAmount: surveyVerifiedIncome}
			},
		}),
	},
	// "high_risk" is required_data_set=survey-high-risk-v1's outcome: Risk
	// System selects it for applicants it flags as high risk (e.g. income too
	// low, asset condition poor, provisional amount too high). It's
	// "normal"'s four pages (via standardSurveyPages(), plus its own non-final
	// income page) plus a fifth, final environment_check page - a check of
	// the customer's behavior and track record by asking people near their
	// home. environment_check.result is written by no other page, so its
	// presence both marks "high_risk" as done and proves the applicant came
	// through this path (IsUnderwritingEligible).
	//
	// A document doesn't have to start on this outcome: Risk System can
	// escalate mid-flow (survey_type "normal" -> "high_risk") on any verdict
	// - see standardSurveyPages()'s comment for why that's safe.
	"high_risk": {
		completionField: document.DocProcessEnvironmentCheckResult,
		pages: append(standardSurveyPages(),
			surveyPage{
				name: "income",
				fields: func() map[common.HString]any {
					return map[common.HString]any{document.DocProcessIncomeVerifiedAmount: surveyVerifiedIncome}
				},
			},
			surveyPage{
				name:  "environment_check",
				final: true,
				fields: func() map[common.HString]any {
					return map[common.HString]any{document.DocProcessEnvironmentCheckResult: surveyEnvironmentCheckResult}
				},
			},
		),
	},
}

// SurveyOutcomeFields returns the document fields the final page of a
// completed survey process produces, for a caller simulating a human
// submission (cmd/testcli's "complete-survey") to send back as the final
// task-completion payload. For a multi-page survey this is equivalent to
// submitting the last page (see SurveyPageCompletion).
func SurveyOutcomeFields(surveyType string) (map[common.HString]any, error) {
	outcome, ok := surveyOutcomesByType[surveyType]
	if !ok {
		return nil, fmt.Errorf("run survey: unsupported survey type %q", surveyType)
	}
	return surveyPageCompletion(outcome, len(outcome.pages)-1)
}

// SurveyPageCompletion returns the document payload for submitting page
// pageIndex (0-based) of a survey process: that page's own findings, nothing
// more. It is the generalisation of SurveyOutcomeFields to any page, so a
// caller driving a multi-page survey (cmd/testcli) can submit each page in
// turn.
func SurveyPageCompletion(surveyType string, pageIndex int) (map[common.HString]any, error) {
	outcome, ok := surveyOutcomesByType[surveyType]
	if !ok {
		return nil, fmt.Errorf("run survey: unsupported survey type %q", surveyType)
	}
	return surveyPageCompletion(outcome, pageIndex)
}

func surveyPageCompletion(outcome surveyOutcome, pageIndex int) (map[common.HString]any, error) {
	if pageIndex < 0 || pageIndex >= len(outcome.pages) {
		return nil, fmt.Errorf("run survey: page index %d out of range (survey has %d pages)", pageIndex, len(outcome.pages))
	}
	return outcome.pages[pageIndex].fields(), nil
}

// SurveyPage is one resolved form page of a survey process, exported so an
// external driver (cmd/testcli) can replay a multi-page survey without
// duplicating the field definitions.
type SurveyPage struct {
	Name   string
	Fields map[common.HString]any
	Final  bool
}

// SurveyPages returns the ordered, resolved form pages of a survey process.
// Every page except the last is a partial completion; the last page is the
// final completion that closes the task.
func SurveyPages(surveyType string) ([]SurveyPage, error) {
	outcome, ok := surveyOutcomesByType[surveyType]
	if !ok {
		return nil, fmt.Errorf("run survey: unsupported survey type %q", surveyType)
	}
	pages := make([]SurveyPage, 0, len(outcome.pages))
	for _, p := range outcome.pages {
		pages = append(pages, SurveyPage{Name: p.name, Fields: p.fields(), Final: p.final})
	}
	return pages, nil
}

type Constructor struct {
	f *runtime.Function[any, any]
}

func (c *Constructor) GenerateFunction(
	_ func(url string) (*framework.APIFunction, error),
	docFieldCheck func([]common.HString),
	system *framework.System,
	doc *defs.DocumentDescriptor,
) error {
	docFieldCheck(readSet)
	docFieldCheck(writeSet)
	for _, path := range optionalReadSet {
		docFieldCheck([]common.HString{path.Path})
	}

	rs := common.MakeReadSet(readSet).SetOptionals(optionalReadSet, true)
	ws := common.MakeWriteSet(writeSet)
	c.f = system.CreateTaskFunction(taskdefs.TaskType(TaskName), *rs, *ws, doc.Fields, nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	step := runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Lazy, []runtime.ProcessStepId{})
	step.SetWriteIfEqual(runtime.None, nil)
	// A survey is realistically multiple form pages: each page submit is a
	// partial completion (WorkerTaskCompletionData.Action == "partial"),
	// only the last page closes the task. EnablePartialCompletion is valid
	// here because the function is task-tagged (process.go's guard).
	if err := step.EnablePartialCompletion(); err != nil {
		panic(err)
	}
	// Once this step is impacted by a rollback (e.g. a later Risk System
	// verdict re-running check_risk_system_pg_verdict, which rewrites survey_type -
	// something this step mandatorily reads), planner.Rollback would
	// otherwise revert every field this step wrote for the *completed*
	// stage (e.g. wiping the asset survey's own asset.condition back to
	// unset) purely as a side effect of an unrelated later stage's verdict
	// cycle - silently destroying a human's already-submitted answer.
	// Matches production's real survey activity (tasking/survey/impl.go).
	step.SetRetainDataOnRollback()
	// Guards against opening a second, uncompletable task for a survey_type
	// whose outcome is already collected (its completionField is present) -
	// e.g. the survey's own writes (customer.birth_date ->
	// check_customer_eligibility_pg -> age_check_passed) can make this step
	// "impacted" again even though nothing about the actual survey changed.
	// CreateTaskFunction alone stops the task from auto-*completing*; this
	// precondition stops it from being re-*created*. $.status is also
	// required here (not consumed by shouldCreateTask, just its presence) so
	// this step waits for check_submission_pg like every other non-exempt
	// activity.
	step.SetPrecondition(shouldCreateTask, common.MakePreConditionSet(
		[]common.HString{document.DocProcessScoringSurveyType, document.DocStatus},
		[]common.HString{
			document.DocProcessIncomeVerifiedAmount,
			document.DocProcessEnvironmentCheckResult,
			document.DocProcessUnderwritingConfirmed,
			document.DocProcessLoanStructureProductType,
		},
	))
	return step
}

// underwritingProduct is the only product_type underwriting applies to.
const underwritingProduct = "NDF4W"

// IsUnderwritingEligible is the single definition of "may this document
// proceed into the underwriting survey": it must have completed the
// high_risk path (environment_check.result is present - only high_risk's
// final page writes it, "normal" never does) and be the NDF4W product (not
// NDF2W). Both are read straight off collected data, so once they hold they
// keep holding - including for the verdict Risk System sends after the
// underwriting page itself. checkrisksystemverdict calls this before ever
// writing survey_type=underwriting, since checkrisksystem's async verdict
// handler has no read access to document state to check this itself;
// shouldCreateTask's own copy below is cheap defense-in-depth, not the
// primary enforcement.
func IsUnderwritingEligible(data map[common.HString]any) bool {
	_, highRiskCompleted := data[document.DocProcessEnvironmentCheckResult]
	productType, _ := data[document.DocProcessLoanStructureProductType].(string)
	return highRiskCompleted && productType == underwritingProduct
}

func shouldCreateTask(_ workflow.Context, data map[common.HString]any) bool {
	surveyType, _ := data[document.DocProcessScoringSurveyType].(string)
	outcome, ok := surveyOutcomesByType[surveyType]
	if !ok {
		return false // unknown survey types fail safe
	}
	if _, done := data[outcome.completionField]; done {
		return false // this survey type's outcome has already been collected
	}
	if surveyType == "underwriting" && !IsUnderwritingEligible(data) {
		// Defense-in-depth: checkrisksystemverdict should already have
		// refused to write survey_type=underwriting for an ineligible
		// applicant. If it somehow landed here anyway, fail safe rather than
		// opening the task.
		return false
	}
	return true
}
