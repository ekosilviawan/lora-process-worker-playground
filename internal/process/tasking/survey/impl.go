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
// System's pre-survey checkpoint has told LORA which survey the user needs
// to complete next (applyrisksystemverdict.apply_risk_system_verdict_pg
// writes survey_type from the verdict's required_data_set, once
// check_risk_system_pg has recorded it) - transitively this also can't happen before
// both intake checks have passed, since nothing seeds the scoring cursor
// (and so nothing sets survey_type) until seed_scoring_checkpoint_pg's own
// precondition requires exactly that (see seedscoringcheckpoint.shouldSeed).
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
	{Path: document.DocProcessScoringRiskSystemMaxLtv, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessLoanStructureMaxFunding, Strategy: common.OptionalWaitIfLocked, TriggerRollback: true},
}

// writeSet is the union of every survey type's findings plus the cursor
// fields, exactly what a real submitted form's completion payload would
// contain. Most of these fields are first set here, but provisional_amount/
// ltv_submission are the exception: they arrive with the initial DP
// submission (testcli's cmdInject seeds them, mirroring production's
// pre_scoring - see calculateriskfunding, whose mandatory readSet depends on
// them existing before any survey runs). The financing outcome below only
// revises them, matching production's "surveyor negotiation" update path -
// it does not originate them.
var writeSet = []common.HString{
	document.DocCustomerBirthDate,
	document.DocCustomerName,
	document.DocProcessAssetCondition,
	document.DocProcessLoanStructureProvisionalAmount,
	document.DocProcessLoanStructureLtvSubmission,
	document.DocProcessIncomeVerifiedAmount,
	document.DocProcessUnderwritingConfirmed,
	document.DocProcessEnvironmentCheckResult,
	document.DocProcessScoringTriggerSeq,
	document.DocProcessScoringStageToken,
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
	stage  string // stage_token written when this page is submitted
	fields func() map[common.HString]any
	final  bool
}

// surveyOutcome is a whole survey process: an ordered list of pages plus the
// stage the cursor advances to when the final page is submitted. Adding a new
// survey type means adding one entry here plus its gate in
// checkrisksystem.stageGates - nothing else in the chain changes.
type surveyOutcome struct {
	nextStage string
	pages     []surveyPage
}

// finalPage returns the process's last page - the one whose submission also
// closes the task.
func (o surveyOutcome) finalPage() surveyPage { return o.pages[len(o.pages)-1] }

// singlePage builds a one-page survey process. "underwriting" is the only
// remaining single-page survey type: identity/asset/financing/income used to
// each have their own single-page entry here too, but are no longer
// independently selectable - they're only reachable as pages within the
// multi-page "normal" survey process below.
func singlePage(name, stage string, fields func() map[common.HString]any) []surveyPage {
	return []surveyPage{{name: name, stage: stage, fields: fields, final: true}}
}

// standardSurveyPages returns the three pages every multi-page survey
// process shares verbatim: identity (customer_verification), asset
// (asset_review), and financing (financing_confirmation). These stage names
// are deliberately identical across every outcome that includes them (today:
// "normal" and "high_risk") rather than being namespaced per outcome - that's
// what lets a document's survey_type escalate mid-flow (e.g. "normal" ->
// "high_risk" once Risk System re-assesses the applicant after the asset
// page) without re-collecting pages already submitted under the old survey
// type: shouldCreateTask only ever compares the CURRENT survey_type's
// nextStage against the CURRENT stage_token, so an already-satisfied shared
// checkpoint is simply never asked again. Giving a future outcome its own
// distinct names for these pages would silently break that. Each caller
// appends its own remaining page(s) - every survey type's income page (and
// anything after it) decides its own stage/finality, since that's exactly
// where outcomes diverge.
func standardSurveyPages() []surveyPage {
	return []surveyPage{
		{
			name:  "identity",
			stage: "customer_verification",
			fields: func() map[common.HString]any {
				return map[common.HString]any{
					document.DocCustomerBirthDate: surveyVerifiedBirthDate,
					document.DocCustomerName:      surveyVerifiedName,
				}
			},
		},
		{
			name:  "asset",
			stage: "asset_review",
			fields: func() map[common.HString]any {
				return map[common.HString]any{document.DocProcessAssetCondition: surveyAssetCondition}
			},
		},
		{
			name: "financing",
			// Revises provisional_amount/ltv_submission, doesn't originate
			// them - see the writeSet comment above.
			stage: "financing_confirmation",
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
	// stage_token != nextStage: applyrisksystemverdict additionally requires
	// the applicant came through the "high_risk" path (stage_token ==
	// "complete_high_risk_survey") and the loan's product is NDF4W before it
	// will even write survey_type=underwriting - see IsUnderwritingEligible
	// and shouldCreateTask's own defense-in-depth copy of the same check
	// below. checkrisksystem's asyncHandler has no read access to current
	// document state (it only ever sees the raw verdict payload), which is
	// exactly why that validation lives in applyrisksystemverdict instead.
	"underwriting": {
		nextStage: "underwriting",
		pages: singlePage("underwriting", "underwriting", func() map[common.HString]any {
			return map[common.HString]any{document.DocProcessUnderwritingConfirmed: surveyUnderwritingConfirm}
		}),
	},
	// "normal" is the multi-page survey process required_data_set=
	// survey-normal-v1 selects: four form pages (identity, asset, financing,
	// income) collected in one SURVEY task. The first three pages are partial
	// completions; only the final (income) page closes the task.
	//
	// Every page advances the scoring cursor: it writes trigger_seq (the
	// monotonic re-trigger, incremented by the driver on each page) and
	// stage_token (the checkpoint that page reaches). Both are REQUIRED reads
	// of check_risk_system_pg, so each page's submission deterministically
	// re-arms that async trigger - RS re-evaluates after every page, not just
	// the final one. The trigger then goes pending holding a write lock on
	// $.process.scoring.survey_type, so the next page's task cannot be
	// re-created until a verdict completes it (tc8 sends a verdictStep after
	// every page). The first three pages come from standardSurveyPages() -
	// identity, asset, and financing are collected identically whether the
	// applicant ends up on "normal" or "high_risk" (see that function's
	// comment). The final (income) page uses its own dedicated checkpoint
	// (complete_normal_survey) rather than the retired granular income_review
	// checkpoint, since this stage specifically marks "normal" itself as
	// done. Every stage's gate lives in checkrisksystem.stageGates. identity,
	// asset, financing, and income are no longer independently selectable
	// survey types - "normal" is one of two paths to their checkpoints.
	"normal": {
		nextStage: "complete_normal_survey",
		pages: append(standardSurveyPages(), surveyPage{
			name:  "income",
			stage: "complete_normal_survey",
			final: true,
			fields: func() map[common.HString]any {
				return map[common.HString]any{document.DocProcessIncomeVerifiedAmount: surveyVerifiedIncome}
			},
		}),
	},
	// "high_risk" is required_data_set=survey-high-risk-v1's outcome: Risk
	// System selects it for applicants it flags as high risk (e.g. income too
	// low, asset condition poor, provisional amount too high). It's
	// "normal"'s four pages (via standardSurveyPages(), plus its own income
	// page) plus a fifth, final environment_check page - a check of the
	// customer's behavior and track record by asking people near their home.
	// Because a page follows it here, income uses its own dedicated
	// non-final checkpoint (income_confirmation) instead of reusing
	// complete_normal_survey, which specifically marks "normal" as done.
	//
	// A document doesn't have to start on this outcome: Risk System can
	// escalate mid-flow (survey_type "normal" -> "high_risk") after any
	// shared page - see standardSurveyPages()'s comment for why that's safe.
	"high_risk": {
		nextStage: "complete_high_risk_survey",
		pages: append(standardSurveyPages(),
			surveyPage{
				name:  "income",
				stage: "income_confirmation",
				fields: func() map[common.HString]any {
					return map[common.HString]any{document.DocProcessIncomeVerifiedAmount: surveyVerifiedIncome}
				},
			},
			surveyPage{
				name:  "environment_check",
				stage: "complete_high_risk_survey",
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
// task-completion payload. seq is the trigger_seq value to write: a genuine
// task completion is a self-contained payload with no read access back into
// the document, so whatever drives the submission (testcli, tracking its own
// scenario sequence) is the source of truth for "what's next", not this
// worker. For a multi-page survey this is equivalent to submitting the last
// page (see SurveyPageCompletion).
func SurveyOutcomeFields(surveyType string, seq int) (map[common.HString]any, error) {
	outcome, ok := surveyOutcomesByType[surveyType]
	if !ok {
		return nil, fmt.Errorf("run survey: unsupported survey type %q", surveyType)
	}
	return surveyPageCompletion(outcome, len(outcome.pages)-1, seq)
}

// SurveyPageCompletion returns the full document payload for submitting page
// pageIndex (0-based) of a survey process: the page's own findings plus the
// cursor fields (trigger_seq=seq, stage_token=the page's checkpoint). It is
// the generalisation of SurveyOutcomeFields to any page, so a caller driving
// a multi-page survey (cmd/testcli) can advance the cursor on EVERY page, not
// just the final one - each page bumps trigger_seq and sets stage_token,
// which is exactly what re-arms check_risk_system_pg after each partial
// completion. seq is the trigger_seq value to write: a genuine task
// completion is a self-contained payload with no read access back into the
// document, so the driver (testcli, tracking its own scenario sequence) is
// the source of truth for it.
func SurveyPageCompletion(surveyType string, pageIndex, seq int) (map[common.HString]any, error) {
	outcome, ok := surveyOutcomesByType[surveyType]
	if !ok {
		return nil, fmt.Errorf("run survey: unsupported survey type %q", surveyType)
	}
	return surveyPageCompletion(outcome, pageIndex, seq)
}

func surveyPageCompletion(outcome surveyOutcome, pageIndex, seq int) (map[common.HString]any, error) {
	if pageIndex < 0 || pageIndex >= len(outcome.pages) {
		return nil, fmt.Errorf("run survey: page index %d out of range (survey has %d pages)", pageIndex, len(outcome.pages))
	}
	page := outcome.pages[pageIndex]
	fields := page.fields()
	fields[document.DocProcessScoringTriggerSeq] = seq
	fields[document.DocProcessScoringStageToken] = page.stage
	return fields, nil
}

// SurveyPage is one resolved form page of a survey process, exported so an
// external driver (cmd/testcli) can replay a multi-page survey without
// duplicating the field definitions.
type SurveyPage struct {
	Name   string
	Fields map[common.HString]any
	Final  bool
	Stage  string // stage_token written when this page is submitted
}

// SurveyPages returns the ordered, resolved form pages of a survey process.
// Every page except the last is a partial completion; the last page is the
// final completion that closes the task. Every page advances the cursor via
// trigger_seq/stage_token (see SurveyPageCompletion).
func SurveyPages(surveyType string) ([]SurveyPage, error) {
	outcome, ok := surveyOutcomesByType[surveyType]
	if !ok {
		return nil, fmt.Errorf("run survey: unsupported survey type %q", surveyType)
	}
	pages := make([]SurveyPage, 0, len(outcome.pages))
	for _, p := range outcome.pages {
		pages = append(pages, SurveyPage{Name: p.name, Fields: p.fields(), Final: p.final, Stage: p.stage})
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
	// Once this step is impacted by a rollback (e.g. check_risk_system_pg
	// re-arming apply_risk_system_verdict_pg, which rewrites survey_type -
	// something this step mandatorily reads), planner.Rollback would
	// otherwise revert every field this step wrote for the *completed*
	// stage (e.g. wiping the asset survey's own asset.condition back to
	// unset) purely as a side effect of an unrelated later stage's verdict
	// cycle - silently destroying a human's already-submitted answer.
	// Matches production's real survey activity (tasking/survey/impl.go).
	step.SetRetainDataOnRollback()
	// Guards against opening a second, uncompletable task for a survey_type
	// whose outcome is already reflected in stage_token - e.g. the survey's
	// own writes (customer.birth_date -> check_customer_eligibility_pg ->
	// age_check_passed) can make this step "impacted" again even though
	// nothing about the actual survey changed. CreateTaskFunction alone stops
	// the task from auto-*completing*; this precondition stops it from being
	// re-*created*. $.status is also required here (not consumed by
	// shouldCreateTask, just its presence) so this step waits for
	// check_submission_pg like every other non-exempt activity.
	step.SetPrecondition(shouldCreateTask, common.MakePreConditionSet(
		[]common.HString{document.DocProcessScoringSurveyType, document.DocStatus},
		[]common.HString{document.DocProcessScoringStageToken, document.DocProcessLoanStructureProductType},
	))
	return step
}

// underwritingHighRiskStageToken is the checkpoint the high_risk survey's
// final page advances stage_token to (surveyOutcomesByType["high_risk"].
// nextStage). It doubles as the signal that an applicant came through the
// high_risk path: applyrisksystemverdict writes survey_type but never
// touches stage_token, so at the moment survey_type first becomes
// "underwriting", stage_token still holds whichever checkpoint the PRIOR
// outcome left behind - "complete_high_risk_survey" only if that prior
// outcome was "high_risk". Once underwriting's own page is submitted,
// stage_token becomes "underwriting" and the ordinary
// stageToken != outcome.nextStage check below already returns false, so this
// only matters in the window before that.
const underwritingHighRiskStageToken = "complete_high_risk_survey"

// underwritingProduct is the only product_type underwriting applies to.
const underwritingProduct = "NDF4W"

// IsUnderwritingEligible is the single definition of "may this document
// proceed into the underwriting survey": it must have completed the
// high_risk path (not normal) and be the NDF4W product (not NDF2W).
// applyrisksystemverdict calls this before ever writing
// survey_type=underwriting, since checkrisksystem's async verdict handler has
// no read access to document state to check this itself; shouldCreateTask's
// own copy below is cheap defense-in-depth, not the primary enforcement.
func IsUnderwritingEligible(stageToken, productType string) bool {
	return stageToken == underwritingHighRiskStageToken && productType == underwritingProduct
}

func shouldCreateTask(_ workflow.Context, data map[common.HString]any) bool {
	surveyType, _ := data[document.DocProcessScoringSurveyType].(string)
	outcome, ok := surveyOutcomesByType[surveyType]
	if !ok {
		return false // unknown survey types fail safe, same as checkrisksystem.stageGate
	}
	stageToken, _ := data[document.DocProcessScoringStageToken].(string)
	if stageToken == outcome.nextStage {
		return false // this survey type's own outcome is already reflected in stage_token
	}
	if surveyType == "underwriting" {
		productType, _ := data[document.DocProcessLoanStructureProductType].(string)
		if !IsUnderwritingEligible(stageToken, productType) {
			// Defense-in-depth: applyrisksystemverdict should already have
			// refused to write survey_type=underwriting for an ineligible
			// applicant. If it somehow landed here anyway, fail safe rather
			// than opening the task.
			return false
		}
	}
	return true
}
