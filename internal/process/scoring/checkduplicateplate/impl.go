package checkduplicateplate

import (
	"context"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/fp"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"

	"lora-process-worker-playground/internal/process/document"
)

const ProcessAndActivityName = "check_duplicate_license_plate_pg"

var readSet = []common.HString{
	document.DocProcessAssetLicensePlate,
	document.DocStatus,
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
	return ps
}
