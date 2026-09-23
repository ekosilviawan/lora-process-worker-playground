package checkrisksystem

import (
	"context"
	"fmt"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/fp"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
	"go.temporal.io/sdk/workflow"

	"lora-process-worker-playground/internal/process/document"
)

const ProcessAndActivityName = "check_risk_system_pg"

var requiredReadSet = []common.HString{
	document.DocId,
	document.DocCustomerNik,
	document.DocCustomerName,
	document.DocProcessScoringTriggerSeq,
	document.DocProcessScoringStageToken,
}

var optionalReadSet = []common.OptionalPath{
	documentOptional(document.DocCustomerBirthDate),
	documentOptional(document.DocProcessRiskRating),
	documentOptional(document.DocProcessAssetCondition),
	documentOptional(document.DocProcessLoanStructureProvisionalAmount),
	documentOptional(document.DocProcessLoanStructureLtvSubmission),
	documentOptional(document.DocProcessIncomeVerifiedAmount),
	documentOptional(document.DocProcessFinalReviewConfirmed),
}

var writeSet = []common.HString{
	document.DocProcessScoringRiskSystemRequestId,
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

	execFunc := func(_ context.Context, data *map[common.HString]any) (*map[common.HString]any, error) {
		// trigger_seq round-trips through JSON (ArangoDB storage, data-forward),
		// which decodes numbers as float64 - fp.As[int] would panic here.
		seq := fp.MustJsonNumAsInt((*data)[document.DocProcessScoringTriggerSeq])
		requestID := fmt.Sprintf("playground-rs-%d", seq)
		out := map[common.HString]any{document.DocProcessScoringRiskSystemRequestId: requestID}
		return &out, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, nil, conv, execFunc, nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	step := runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Normal, []runtime.ProcessStepId{})
	step.SetPrecondition(stageGate, common.MakePreConditionSet(
		[]common.HString{document.DocProcessScoringStageToken},
		[]common.HString{
			document.DocCustomerBirthDate,
			document.DocProcessAssetCondition,
			document.DocProcessLoanStructureLtvSubmission,
			document.DocProcessIncomeVerifiedAmount,
			document.DocProcessFinalReviewConfirmed,
		},
	))
	step.SetNonDeterministic()
	return step
}

// stageGates maps each cursor stage to the survey field that must exist
// before Risk System is asked to review that stage. Every stage after
// post_submission is gated on the field the matching survey_type just
// collected (see runsurvey), so the chain never asks RS about data the
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
