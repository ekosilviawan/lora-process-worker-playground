// Package checksubmission verifies a submission is ready to enter scoring
// and records the document's very first status transition, $.status ->
// "new" - the same status-stamping convention every later transition
// follows (see document.SetStatusTimestamp).
package checksubmission

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

const ProcessAndActivityName = "check_submission_pg"

// readSet is empty: this is the document's first status transition, so it
// must not wait on any other field - only the precondition below (whether
// $.status has been set yet) gates it.
var readSet = []common.HString{
	document.DocId,
}

var writeSet = []common.HString{
	document.DocStatus,
	document.DocProcessStatusTimestampsNew,
}

const statusNew = "new"

// Constructor wires the activity into the process-sdk planner.
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
	convIn.SetInput(common.MakeReadSet(readSet), func(m map[common.HString]any) (*map[common.HString]any, error) {
		return &m, nil
	})

	convOut := mapping.NewSimpleOutputConverter[map[common.HString]any]()
	convOut.SetOutput(common.MakeWriteSet(writeSet), func(data *map[common.HString]any) (map[common.HString]any, error) {
		return *data, nil
	})

	conv := mapping.NewSimpleConverterFrom(convIn, convOut)

	execFunc := func(_ context.Context, _ *map[common.HString]any) (*map[common.HString]any, error) {
		mOut := map[common.HString]any{
			document.DocStatus: statusNew,
		}
		document.SetStatusTimestamp(mOut, statusNew)
		return &mOut, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, nil, conv, execFunc, nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	step := runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Eager, []runtime.ProcessStepId{})
	step.SetWriteIfEqual(runtime.None, nil)
	// Runs exactly once, before anything else touches $.status: gating on
	// $.status itself being absent (rather than a real read-set field) is
	// what makes this the document's first status transition rather than a
	// second, spurious "new" stamped over whatever later replaced it -
	// mirrors seedscoringcheckpoint's own re-fire guard.
	step.SetPrecondition(shouldVerify, common.MakePreConditionSet(
		[]common.HString{},
		[]common.HString{document.DocStatus},
	))
	return step
}

func shouldVerify(_ workflow.Context, data map[common.HString]any) bool {
	_, alreadySet := data[document.DocStatus]
	return !alreadySet
}
