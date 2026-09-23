package seedscoringcheckpoint

import (
	"context"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
	"go.temporal.io/sdk/workflow"

	"lora-process-worker-playground/internal/process/document"
)

const ProcessAndActivityName = "seed_scoring_checkpoint_pg"

// PostSubmissionStage is the cursor value that puts the pre-survey Risk
// System checkpoint in check_risk_system_pg's runnable set (its stage gate
// is nil, so it fires as soon as the cursor exists).
const PostSubmissionStage = "post_submission"

// readSet is empty: whether to seed is decided entirely by the precondition
// below (shouldSeed), not by ordinary field-presence scheduling.
var readSet = []common.HString{}

var writeSet = []common.HString{
	document.DocProcessScoringTriggerSeq,
	document.DocProcessScoringStageToken,
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
	docFieldCheck(readSet)
	docFieldCheck(writeSet)

	convIn := mapping.NewSimpleInputConverter[map[common.HString]any]()
	convIn.SetInput(common.MakeReadSet(readSet), func(m map[common.HString]any) (*map[common.HString]any, error) { return &m, nil })
	convOut := mapping.NewSimpleOutputConverter[map[common.HString]any]()
	convOut.SetOutput(common.MakeWriteSet(writeSet), func(data *map[common.HString]any) (map[common.HString]any, error) { return *data, nil })
	conv := mapping.NewSimpleConverterFrom(convIn, convOut)

	execFunc := func(_ context.Context, _ *map[common.HString]any) (*map[common.HString]any, error) {
		return &map[common.HString]any{
			document.DocProcessScoringTriggerSeq: 0,
			document.DocProcessScoringStageToken: PostSubmissionStage,
		}, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, nil, conv, execFunc, nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	step := runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Normal, []runtime.ProcessStepId{})
	// Fires only for a customer who passed eligibility - an ineligible
	// (already-rejected) customer must never get a scoring cursor - and only
	// once: after anyone has written the trigger sequence this must never
	// fire again, since it always emits the same constant seed values, and
	// re-running after runsurvey/checkrisksystem have advanced the cursor
	// would reset scoring progress back to post_submission.
	step.SetPrecondition(shouldSeed, common.MakePreConditionSet(
		[]common.HString{document.DocProcessEligibilityPassed},
		[]common.HString{document.DocProcessScoringTriggerSeq},
	))
	return step
}

func shouldSeed(_ workflow.Context, data map[common.HString]any) bool {
	eligible, _ := data[document.DocProcessEligibilityPassed].(bool)
	if !eligible {
		return false
	}
	_, alreadySeeded := data[document.DocProcessScoringTriggerSeq]
	return !alreadySeeded
}
