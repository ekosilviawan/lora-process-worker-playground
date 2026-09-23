// Package master creates the task-master workflow every CreateTaskFunction
// activity in this document needs. It mirrors
// lora-partnership-ndf/internal/process/tasking/master/impl.go: a thin
// Constructor around system.CreateTaskMasterFunction, run eagerly with no
// custom readset/writeset. The resulting task-master workflow execution is
// what internal/tasksim registers a worker against.
package master

import (
	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
)

const ProcessAndActivityName = "create_master_task"

type Constructor struct {
	f *runtime.Function[any, any]
}

func (c *Constructor) GenerateFunction(
	_ func(url string) (*framework.APIFunction, error),
	_ func([]common.HString),
	system *framework.System,
	_ *defs.DocumentDescriptor,
) error {
	c.f = system.CreateTaskMasterFunction(nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	return runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Eager, []runtime.ProcessStepId{})
}
