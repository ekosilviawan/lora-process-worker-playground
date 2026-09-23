package seedscoringcheckpoint

import (
	"context"

	"github.com/bfi-finance/lora-process-sdk/framework"
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

// readSet gates seeding on risk rating rather than eligibility directly:
// setriskrating only writes a rating once eligibility has passed, so an
// ineligible (already-rejected) customer never gets a scoring cursor.
var readSet = []common.HString{
	document.DocProcessRiskRating,
}

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
	// Once anyone has written the trigger sequence, this must never fire
	// again: it always emits the same constant seed values, and re-running
	// after runsurvey/checkrisksystem have advanced the cursor would reset
	// scoring progress back to post_submission.
	step.SetPrecondition(notYetSeeded, common.MakePreConditionSet(nil, []common.HString{document.DocProcessScoringTriggerSeq}))
	return step
}

func notYetSeeded(_ workflow.Context, data map[common.HString]any) bool {
	_, exists := data[document.DocProcessScoringTriggerSeq]
	return !exists
}
