package checkduplicateplate

import (
	"context"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/fp"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
	"go.temporal.io/sdk/workflow"

	"lora-process-worker-playground/internal/process/document"
)

const ProcessAndActivityName = "check_duplicate_license_plate_pg"

// $.status is not in readSet: execFunc below never reads its value, only
// gates on it existing (via the precondition in GenerateProcessStep). Kept
// out of ReadSet on purpose - ReadSet is what Planner.Rollback's impact walk
// uses (ReadSet().RollbackTriggerPaths(), see adjustments.go's comment), and
// $.status is written by several steps besides checksubmission_pg
// (including this one). A mandatory ReadSet entry here would make every one
// of those writes a potential trigger for re-running this step - either
// directly, or transitively once some other impacted step's own WriteSet
// happens to touch $.status too, since Rollback's cumulative impact
// accumulator carries no re-exec-neutral filtering.
var readSet = []common.HString{
	document.DocProcessAssetLicensePlate,
}

var writeSet = []common.HString{
	document.DocProcessDuplicatePlateCheckPassed,
	document.DocStatus,
	document.DocStatusReason,
	document.DocProcessStatusTimestampsRejected,
	document.DocProcessStatusTimestampsTerminal,
}

// duplicatePlates is the hard-coded "already has an open loan application"
// license plate list for playground purposes - one plate must not apply for
// a loan twice.
var duplicatePlates = map[string]bool{
	"B1234XYZ": true,
}

const (
	statusRejected       = "rejected"
	reasonDuplicatePlate = "DUPLICATE_LICENSE_PLATE"
)

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

	execFunc := func(_ context.Context, data *map[common.HString]any) (*map[common.HString]any, error) {
		mOut := make(map[common.HString]any)

		plate := fp.As[document.DocTypeProcessAssetLicensePlate]((*data)[document.DocProcessAssetLicensePlate])
		notDuplicate := !duplicatePlates[plate]
		mOut[document.DocProcessDuplicatePlateCheckPassed] = notDuplicate

		if !notDuplicate {
			mOut[document.DocStatus] = statusRejected
			mOut[document.DocStatusReason] = reasonDuplicatePlate
			document.SetStatusTimestamp(mOut, statusRejected)
		}

		return &mOut, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, nil, conv, execFunc, nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	ps := runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Normal, []runtime.ProcessStepId{})
	ps.SetWriteIfEqual(runtime.None, nil)
	// $.status is a presence gate, not read-set data (see readSet's
	// comment) - required here (not optional) since documentMatchRead
	// blocks scheduling outright until a required precondition path is
	// set, matching check_submission_pg's own inverted use of the same
	// field (document/impl.go's shouldVerify).
	ps.SetPrecondition(shouldCheck, common.MakePreConditionSet(
		[]common.HString{document.DocStatus},
		nil,
	))
	return ps
}

func shouldCheck(_ workflow.Context, data map[common.HString]any) bool {
	_, alreadySubmitted := data[document.DocStatus]
	return alreadySubmitted
}
