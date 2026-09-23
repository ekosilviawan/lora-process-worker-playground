package runsurvey

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

const ProcessAndActivityName = "run_survey_pg"

// TaskName identifies this as a real, signal-gated Temporal task (via
// system.CreateTaskFunction), the same way LTW's "SURVEY" task works in
// production (lora-partnership-ndf/internal/process/tasking/survey). It only
// completes when an explicit "task-completion" update arrives (see
// internal/tasksim and cmd/testcli's "complete-survey" command) - never as a
// side effect of some other document field changing.
const TaskName = "SURVEY"

// readSet depends on eligibility_passed so the survey only starts after the
// initial eligibility chain has settled, and on survey_type so it never
// runs before Risk System's pre-survey checkpoint has told LORA which
// survey the user needs to complete next (applyrisksystemverdict writes
// survey_type from the verdict's required_data_set).
var readSet = []common.HString{
	document.DocProcessEligibilityPassed,
	document.DocProcessScoringSurveyType,
}

// optionalReadSet mirrors writeSet's own fields (plus Risk System's max_ltv)
// as non-rollback-triggering context: a real form shows the human what's
// already on file - previously-collected customer/asset/loan data, and RS's
// lending constraint - alongside whatever this stage is asking them to
// confirm or supply. Fields also present in writeSet become "pre-filled,
// editable" once a real form renders WorkerTaskData.Read/.Write together;
// TriggerRollback stays false (default) on every entry, so none of this
// reintroduces the customer_name/eligibility_passed cascade the completion-
// gate precondition below already guards against.
var optionalReadSet = []common.OptionalPath{
	{Path: document.DocCustomerBirthDate, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocCustomerName, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessAssetCondition, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessLoanStructureProvisionalAmount, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessLoanStructureLtvSubmission, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessIncomeVerifiedAmount, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessFinalReviewConfirmed, Strategy: common.OptionalIgnoreIfLocked},
	{Path: document.DocProcessScoringRiskSystemMaxLtv, Strategy: common.OptionalIgnoreIfLocked},
}

// writeSet is the union of every survey type's findings plus the cursor
// fields, exactly what a real submitted form's completion payload would
// contain.
var writeSet = []common.HString{
	document.DocCustomerBirthDate,
	document.DocCustomerName,
	document.DocProcessAssetCondition,
	document.DocProcessLoanStructureProvisionalAmount,
	document.DocProcessLoanStructureLtvSubmission,
	document.DocProcessIncomeVerifiedAmount,
	document.DocProcessFinalReviewConfirmed,
	document.DocProcessScoringTriggerSeq,
	document.DocProcessScoringStageToken,
}

// Hardcoded survey findings, one set per survey type - the canned "human"
// answers cmd/testcli submits as a task completion.
const (
	surveyVerifiedBirthDate  = "1985-03-15"
	surveyVerifiedName       = "Jane Smith"
	surveyAssetCondition     = "fair"
	surveyProvisionalAmount  = 100000.0
	surveyLtvSubmission      = 0.8
	surveyVerifiedIncome     = 15000000.0
	surveyFinalReviewConfirm = true
)

// surveyOutcome is what completing one survey type produces: the findings
// it writes and the stage the cursor advances to. Adding a new survey type
// means adding one entry here plus its gate in checkrisksystem.stageGates -
// nothing else in the chain changes.
type surveyOutcome struct {
	nextStage string
	fields    func() map[common.HString]any
}

var surveyOutcomesByType = map[string]surveyOutcome{
	"identity": {
		nextStage: "customer_verification",
		fields: func() map[common.HString]any {
			return map[common.HString]any{
				document.DocCustomerBirthDate: surveyVerifiedBirthDate,
				document.DocCustomerName:      surveyVerifiedName,
			}
		},
	},
	"asset": {
		nextStage: "asset_review",
		fields: func() map[common.HString]any {
			return map[common.HString]any{document.DocProcessAssetCondition: surveyAssetCondition}
		},
	},
	"financing": {
		nextStage: "financing",
		fields: func() map[common.HString]any {
			return map[common.HString]any{
				document.DocProcessLoanStructureProvisionalAmount: surveyProvisionalAmount,
				document.DocProcessLoanStructureLtvSubmission:     surveyLtvSubmission,
			}
		},
	},
	"income": {
		nextStage: "income_review",
		fields: func() map[common.HString]any {
			return map[common.HString]any{document.DocProcessIncomeVerifiedAmount: surveyVerifiedIncome}
		},
	},
	"final_review": {
		nextStage: "final_review",
		fields: func() map[common.HString]any {
			return map[common.HString]any{document.DocProcessFinalReviewConfirmed: surveyFinalReviewConfirm}
		},
	},
}

// SurveyOutcomeFields returns the document fields a completed survey of the
// given type produces, for a caller simulating a human submission (cmd/
// testcli's "complete-survey") to send back as a task-completion payload.
// seq is the trigger_seq value to write: a genuine task completion is a
// self-contained payload with no read access back into the document, so
// whatever drives the submission (testcli, tracking its own scenario
// sequence) is the source of truth for "what's next", not this worker.
func SurveyOutcomeFields(surveyType string, seq int) (map[common.HString]any, error) {
	outcome, ok := surveyOutcomesByType[surveyType]
	if !ok {
		return nil, fmt.Errorf("run survey: unsupported survey type %q", surveyType)
	}
	fields := outcome.fields()
	fields[document.DocProcessScoringTriggerSeq] = seq
	fields[document.DocProcessScoringStageToken] = outcome.nextStage
	return fields, nil
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
	// own writes (customer.name -> check_name_denylist_pg -> eligibility_passed)
	// can make this step "impacted" again even though nothing about the
	// actual survey changed. CreateTaskFunction alone stops the task from
	// auto-*completing*; this precondition stops it from being re-*created*.
	step.SetPrecondition(shouldCreateTask, common.MakePreConditionSet(
		[]common.HString{document.DocProcessScoringSurveyType},
		[]common.HString{document.DocProcessScoringStageToken},
	))
	return step
}

func shouldCreateTask(_ workflow.Context, data map[common.HString]any) bool {
	surveyType, _ := data[document.DocProcessScoringSurveyType].(string)
	outcome, ok := surveyOutcomesByType[surveyType]
	if !ok {
		return false // unknown survey types fail safe, same as checkrisksystem.stageGate
	}
	stageToken, _ := data[document.DocProcessScoringStageToken].(string)
	return stageToken != outcome.nextStage
}
