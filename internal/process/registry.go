package process

import (
	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"

	"lora-process-worker-playground/internal/process/scoring/applyrisksystemverdict"
	"lora-process-worker-playground/internal/process/scoring/calculateriskfunding"
	"lora-process-worker-playground/internal/process/scoring/checkduplicateplate"
	"lora-process-worker-playground/internal/process/scoring/checkeligibility"
	"lora-process-worker-playground/internal/process/scoring/checkrisksystem"
	"lora-process-worker-playground/internal/process/scoring/checksubmission"
	"lora-process-worker-playground/internal/process/scoring/seedscoringcheckpoint"
	"lora-process-worker-playground/internal/process/tasking/master"
	"lora-process-worker-playground/internal/process/tasking/survey"
)

// Registriable is implemented by every activity Constructor. system and doc
// are only used by constructors that create task-master/task activities
// (system.CreateTaskMasterFunction/CreateTaskFunction) - most ignore them.
type Registriable interface {
	GenerateFunction(
		apiLoader func(url string) (*framework.APIFunction, error),
		docFieldCheck func([]common.HString),
		system *framework.System,
		doc *defs.DocumentDescriptor,
	) error
	GenerateProcessStep() *runtime.ProcessStep
}

// registry is the ordered list of all activity constructors.
// Add new activities here.
var registry = []Registriable{
	&master.Constructor{},
	&checksubmission.Constructor{},
	&checkeligibility.Constructor{},
	&checkduplicateplate.Constructor{},
	&seedscoringcheckpoint.Constructor{},
	&survey.Constructor{},
	&checkrisksystem.Constructor{},
	&applyrisksystemverdict.Constructor{},
	&calculateriskfunding.Constructor{},
}

func MakeAllProcessSteps(
	apiLoader func(url string) (*framework.APIFunction, error),
	docFieldCheck func([]common.HString),
	system *framework.System,
	doc *defs.DocumentDescriptor,
) ([]*runtime.ProcessStep, error) {
	steps := make([]*runtime.ProcessStep, len(registry))
	for i, reg := range registry {
		if err := reg.GenerateFunction(apiLoader, docFieldCheck, system, doc); err != nil {
			return nil, err
		}
		steps[i] = reg.GenerateProcessStep()
	}
	return steps, nil
}
