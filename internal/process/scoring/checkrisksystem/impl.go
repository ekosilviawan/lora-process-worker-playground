package checkrisksystem

import (
	"context"
	"fmt"
	"time"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
	"github.com/google/uuid"
	"go.temporal.io/sdk/workflow"

	"lora-process-worker-playground/internal/process/document"
)

const ProcessAndActivityName = "check_risk_system_pg"

// requiredReadSet's last two entries - the intake checks' own verdicts - are
// dual-placed here and in the precondition below (same treatment as
// stage_token): stageGate reads their VALUES, not just their presence, to
// enforce "Risk System is never asked about a submission either check has
// rejected."
var requiredReadSet = []common.HString{
	document.DocId,
	document.DocCustomerNik,
	document.DocCustomerName,
	document.DocProcessScoringTriggerSeq,
	document.DocProcessScoringStageToken,
	document.DocProcessAgeCheckPassed,
	document.DocProcessDuplicatePlateCheckPassed,
}

var optionalReadSet = []common.OptionalPath{
	documentOptional(document.DocCustomerBirthDate),
	documentOptional(document.DocProcessAssetCondition),
	documentOptional(document.DocProcessLoanStructureProvisionalAmount),
	documentOptional(document.DocProcessLoanStructureLtvSubmission),
	documentOptional(document.DocProcessIncomeVerifiedAmount),
	documentOptional(document.DocProcessFinalReviewConfirmed),
}

// writeSet is the union of what "calling RS" produces (request_id) and what
// "applying RS's verdict" produces (status/survey_type/etc). Both happen
// atomically at completion time, since this is a single genuine async
// Temporal activity (system.async, via a non-nil asyncHandler): the
// activity goes pending the moment it's scheduled and only resolves once
// cmd/testcli's "verdict" command completes it (client.CompleteActivityByID)
// with the verdict data - mirroring how a real external call and its
// caller-visible effects land together, with no observable in-between state
// where only request_id exists.
var writeSet = []common.HString{
	document.DocProcessScoringRiskSystemRequestId,
	document.DocStatus,
	document.DocStatusReason,
	document.DocProcessStatusTimestampsApproved,
	document.DocProcessStatusTimestampsRejected,
	document.DocProcessStatusTimestampsTerminal,
	document.DocProcessLoanStructureLtvMax,
	document.DocProcessScoringSurveyType,
}

func documentOptional(path common.HString) common.OptionalPath {
	return common.OptionalPath{Path: path, Strategy: common.OptionalIgnoreIfLocked}
}

type Constructor struct {
	f *runtime.Function[any, any]
}

func (c *Constructor) GenerateFunction(
	_ func(url string) (*framework.APIFunction, error),
	docFieldCheck func([]common.HString),
	_ *framework.System,
	_ *defs.DocumentDescriptor,
) error {
	docFieldCheck(requiredReadSet)
	docFieldCheck(writeSet)
	for _, path := range optionalReadSet {
		docFieldCheck([]common.HString{path.Path})
	}

	readSet := common.MakeReadSet(requiredReadSet).SetOptionals(optionalReadSet, true)
	convIn := mapping.NewSimpleInputConverter[map[common.HString]any]()
	convIn.SetInput(readSet, func(m map[common.HString]any) (*map[common.HString]any, error) { return &m, nil })
	convOut := mapping.NewSimpleOutputConverter[map[common.HString]any]()
	convOut.SetOutput(common.MakeWriteSet(writeSet), func(data *map[common.HString]any) (map[common.HString]any, error) { return *data, nil })
	conv := mapping.NewSimpleConverterFrom(convIn, convOut)

	// work runs synchronously the instant the activity is scheduled, but per
	// the async contract (runtime/function.go's Function.Execute) its return
	// value is discarded the moment the activity goes pending - only
	// asyncHandler's output, supplied later via a real Temporal completion,
	// ever reaches the document.
	work := func(_ context.Context, _ *map[common.HString]any) (*map[common.HString]any, error) {
		return &map[common.HString]any{}, nil
	}

	var asyncHandler runtime.AsyncPayloadHandler = func(raw map[string]any) (map[common.HString]any, error) {
		status, _ := raw["status"].(string)
		requiredDataSet, _ := raw["required_data_set"].(string)
		maxLTV, _ := raw["max_ltv"].(float64)
		rejectReason, _ := raw["reject_reason"].(string)

		surveyType, ok := surveyTypeForDataSet(requiredDataSet)
		if !ok {
			return nil, fmt.Errorf("risk system: unsupported required data set %q", requiredDataSet)
		}
		mappedStatus, ok := translateStatus(status)
		if !ok {
			return nil, fmt.Errorf("risk system: unsupported verdict status %q", status)
		}

		out := map[common.HString]any{
			document.DocProcessScoringRiskSystemRequestId: "playground-rs-" + uuid.New().String(),
			document.DocStatus:                            mappedStatus,
			document.DocProcessLoanStructureLtvMax:        maxLTV,
			document.DocProcessScoringSurveyType:          surveyType,
		}
		if rejectReason != "" {
			out[document.DocStatusReason] = rejectReason
		}
		if mappedStatus == "approved" || mappedStatus == "rejected" {
			document.SetStatusTimestamp(out, mappedStatus)
		}
		return out, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, &asyncHandler, conv, work, nil)
	return nil
}

func translateStatus(status string) (string, bool) {
	mappedStatus, ok := map[string]string{"approved": "approved", "rejected": "rejected", "pending": "processing"}[status]
	return mappedStatus, ok
}

// surveyTypeByDataSet maps what Risk System says it still needs onto which
// survey type the user must complete next. RS decides this internally from
// its own risk-level calculation (a blackbox to LORA, per the design tenet);
// LORA only needs the resulting required_data_set identifier. An unknown
// value fails safe (§2.4: an in-flight worker can be older than RS's release
// cadence and must not silently stall or guess).
var surveyTypeByDataSet = map[string]string{
	"CUSTOMER_VERIFICATION": "identity",
	"ASSET_REVIEW":          "asset",
	"FINANCING":             "financing",
	"INCOME_REVIEW":         "income",
	"FINAL_REVIEW":          "final_review",
}

func surveyTypeForDataSet(dataSet string) (string, bool) {
	surveyType, ok := surveyTypeByDataSet[dataSet]
	return surveyType, ok
}

// ValidStatus and ValidRequiredDataSet let a caller validate a verdict
// before completing this activity (system.CompleteActivityByID). Since this
// is a genuine async Temporal activity, nothing validates the payload
// before completion the way a data-forward update's JSON Schema gate used
// to - an invalid value discovered only inside asyncHandler doesn't just
// fail this activity, it fails the whole workflow execution. cmd/testcli's
// "verdict" command calls these first, mirroring where that schema gate
// (LGS's real proxy validation) used to sit.
func ValidStatus(status string) bool {
	_, ok := translateStatus(status)
	return ok
}

func ValidRequiredDataSet(dataSet string) bool {
	_, ok := surveyTypeByDataSet[dataSet]
	return ok
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	step := runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Normal, []runtime.ProcessStepId{})
	step.SetWriteIfEqual(runtime.None, nil)
	// Async activities stay pending until explicitly completed; the SDK
	// default startToCloseTimeout (2 minutes, runtime/process.go) would
	// otherwise time this out and retry it - re-minting a fresh async token
	// - long before a human/testcli ever gets to call "verdict". Matches
	// survey's own long timeout for the same reason.
	step.SetTimeout(30 * 24 * time.Hour)
	// This step is reused for every checkpoint, so a later checkpoint's
	// trigger_seq/stage_token change (survey advancing the cursor) makes
	// planner.Rollback treat this step as "impacted" again - and without
	// this, Rollback would revert every field this step already wrote for
	// the *completed* checkpoint (ltv_max, risk_system.request_id,
	// survey_type) back to unset, purely as a side effect of the next
	// checkpoint being armed. That silently breaks calculateriskfunding's
	// optional ltv_max read (TriggerRollback: true): reverted-then-set-again
	// lands as newlySet, never updated, so calculateriskfunding is never
	// told to react to Risk System's real cap. Matches survey's own
	// SetRetainDataOnRollback (tasking/survey/impl.go) for the identical
	// reason.
	step.SetRetainDataOnRollback()
	step.SetPrecondition(stageGate, common.MakePreConditionSet(
		[]common.HString{
			document.DocProcessScoringStageToken,
			document.DocProcessAgeCheckPassed,
			document.DocProcessDuplicatePlateCheckPassed,
			document.DocStatus,
		},
		[]common.HString{
			document.DocCustomerBirthDate,
			document.DocProcessAssetCondition,
			document.DocProcessLoanStructureLtvSubmission,
			document.DocProcessIncomeVerifiedAmount,
			document.DocProcessFinalReviewConfirmed,
		},
	))
	// Deliberately NOT SetNonDeterministic(): the survey_type/stage_token
	// self-loop (this step writes survey_type -> survey mandatorily reads it
	// and writes stage_token -> this step mandatorily reads stage_token) makes
	// planner.Rollback mark this step "impacted" again right after every
	// verdict, before stage_token has actually changed value (Rollback's walk
	// is reachability-based, not value-diff-based - see planner.go's
	// impactedSteps). With an unchanged readset, that re-arm is meant to
	// fast-forward via document.history.MatchInput (workflow.go's
	// doActivityExec) straight back to the identical verdict just given,
	// instead of opening a second real pending activity. A genuine re-ask of
	// Risk System (a real resubmission, e.g. testcli's tc4) always carries a
	// new trigger_seq, which naturally defeats history-matching on its own -
	// so nothing here depends on treating this step as non-deterministic.
	return step
}

// stageGates maps each cursor stage to the survey field that must exist
// before Risk System is asked to review that stage. Every stage after
// post_submission is gated on the field the matching survey_type just
// collected (see tasking/survey), so the chain never asks RS about data the
// user has not submitted yet.
var stageGates = map[string][]common.HString{
	"post_submission":       nil,
	"customer_verification": {document.DocCustomerBirthDate},
	"asset_review":          {document.DocProcessAssetCondition},
	"financing":             {document.DocProcessLoanStructureLtvSubmission},
	"income_review":         {document.DocProcessIncomeVerifiedAmount},
	"final_review":          {document.DocProcessFinalReviewConfirmed},
}

func stageGate(_ workflow.Context, data map[common.HString]any) bool {
	ageOK, _ := data[document.DocProcessAgeCheckPassed].(bool)
	plateOK, _ := data[document.DocProcessDuplicatePlateCheckPassed].(bool)
	if !ageOK || !plateOK {
		return false
	}
	stage, ok := data[document.DocProcessScoringStageToken].(string)
	if !ok {
		return false
	}
	for _, path := range stageGates[stage] {
		if _, exists := data[path]; !exists {
			return false
		}
	}
	_, known := stageGates[stage]
	return known
}
