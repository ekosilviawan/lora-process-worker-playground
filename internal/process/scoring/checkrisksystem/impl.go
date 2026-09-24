package checkrisksystem

import (
	"context"
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
//
// product_type is part of what LPW's (simulated) call to RS includes,
// alongside the customer identity fields below - RS's own required-data-set
// decision is made with full knowledge of the loan's product, so a
// correctly-behaving RS should never request underwriting-v1 for an NDF2W
// applicant in the first place. applyrisksystemverdict.IsUnderwritingEligible
// is the backstop for when it does anyway.
var requiredReadSet = []common.HString{
	document.DocId,
	document.DocCustomerNik,
	document.DocCustomerName,
	document.DocProcessLoanStructureProductType,
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
	documentOptional(document.DocProcessUnderwritingConfirmed),
	documentOptional(document.DocProcessEnvironmentCheckResult),
}

// writeSet is just "calling RS" 's own output: RS's raw answer, recorded
// verbatim onto its own $.process.scoring.risk_system.* fields. This
// activity does NOT interpret the verdict (map required_data_set to
// survey_type, translate status, or decide underwriting eligibility) - that
// interpretation happens in applyrisksystemverdict.apply_risk_system_verdict_pg,
// an ordinary synchronous activity that reads these fields back. It has to
// live there: this activity's asyncHandler below only ever receives the raw
// external payload (runtime.AsyncPayloadHandler), never current document
// state, so it structurally cannot validate the verdict against the
// document (e.g. checking stage_token/product_type for the underwriting
// gate) - see applyrisksystemverdict's package comment for the full reasoning.
var writeSet = []common.HString{
	document.DocProcessScoringRiskSystemRequestId,
	document.DocProcessScoringRiskSystemStatus,
	document.DocProcessScoringRiskSystemRequiredDataSet,
	document.DocProcessScoringRiskSystemMaxLtv,
	document.DocProcessScoringRiskSystemRejectReason,
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

	// asyncHandler just records what RS said, verbatim - it does not
	// interpret it (see writeSet's comment above for why that's not this
	// activity's job).
	var asyncHandler runtime.AsyncPayloadHandler = func(raw map[string]any) (map[common.HString]any, error) {
		status, _ := raw["status"].(string)
		requiredDataSet, _ := raw["required_data_set"].(string)
		maxLTV, _ := raw["max_ltv"].(float64)
		rejectReason, _ := raw["reject_reason"].(string)

		out := map[common.HString]any{
			document.DocProcessScoringRiskSystemRequestId:       "playground-rs-" + uuid.New().String(),
			document.DocProcessScoringRiskSystemStatus:          status,
			document.DocProcessScoringRiskSystemRequiredDataSet: requiredDataSet,
			document.DocProcessScoringRiskSystemMaxLtv:          maxLTV,
		}
		if rejectReason != "" {
			out[document.DocProcessScoringRiskSystemRejectReason] = rejectReason
		}
		return out, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, &asyncHandler, conv, work, nil)
	return nil
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
	// the *completed* checkpoint (the raw risk_system.request_id/status/
	// required_data_set/max_ltv fields) back to unset, purely as a side
	// effect of the next checkpoint being armed. That would in turn make
	// applyrisksystemverdict see those fields as newly-set rather than
	// updated on the NEXT verdict, and silently breaks calculateriskfunding's
	// optional ltv_max read the same way (see applyrisksystemverdict's own
	// SetRetainDataOnRollback for the continuation of this chain). Matches
	// survey's own SetRetainDataOnRollback (tasking/survey/impl.go) for the
	// identical reason.
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
			document.DocProcessUnderwritingConfirmed,
			document.DocProcessEnvironmentCheckResult,
		},
	))
	// Deliberately NOT SetNonDeterministic(): the risk_system.*/survey_type/
	// stage_token self-loop (this step writes the raw risk_system.* fields ->
	// applyrisksystemverdict mandatorily reads them and writes survey_type ->
	// survey mandatorily reads survey_type and writes stage_token -> this
	// step mandatorily reads stage_token) makes planner.Rollback mark this
	// step "impacted" again right after every verdict, before stage_token has
	// actually changed value (Rollback's walk is reachability-based, not
	// value-diff-based - see planner.go's impactedSteps). With an unchanged
	// readset, that re-arm is meant to fast-forward via
	// document.history.MatchInput (workflow.go's doActivityExec) straight
	// back to the identical verdict just given, instead of opening a second
	// real pending activity. A genuine re-ask of Risk System (a real
	// resubmission, e.g. testcli's tc4) always carries a new trigger_seq,
	// which naturally defeats history-matching on its own - so nothing here
	// depends on treating this step as non-deterministic.
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
	"underwriting":          {document.DocProcessUnderwritingConfirmed},
	// financing_confirmation is the checkpoint reached after the multi-page
	// "normal" survey's third (financing) page; its gate is the field that
	// page writes, so RS is never asked before all three preceding pages are
	// in. The survey's fourth and final page (income) then advances the
	// cursor on to complete_normal_survey.
	"financing_confirmation": {document.DocProcessLoanStructureLtvSubmission},
	// complete_normal_survey is the checkpoint reached after the multi-page
	// "normal" survey's fourth and final (income) page, marking the whole
	// process as done; its gate is the field that page writes. This is a
	// dedicated checkpoint, not the retired standalone income_review one.
	"complete_normal_survey": {document.DocProcessIncomeVerifiedAmount},
	// income_confirmation is the checkpoint reached after the "high_risk"
	// survey's fourth (income) page. It shares its gate field with
	// complete_normal_survey (both pages write the same income field), but is
	// a distinct checkpoint name since "high_risk" continues to a fifth page
	// rather than closing the task here.
	"income_confirmation": {document.DocProcessIncomeVerifiedAmount},
	// complete_high_risk_survey is the checkpoint reached after the
	// "high_risk" survey's fifth and final (environment_check) page, marking
	// the whole high_risk process as done; its gate is the field that page
	// writes.
	"complete_high_risk_survey": {document.DocProcessEnvironmentCheckResult},
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
