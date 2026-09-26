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

// readSet is empty: whether to seed is decided entirely by the precondition
// below (shouldSeed), not by ordinary field-presence scheduling. Being empty
// also means this step has no rollback triggers, so it can never be
// re-impacted and rewrite the counter.
var readSet = []common.HString{}

// writeSet is just the re-ask counter. trigger_seq has to exist BEFORE the
// first survey page: the SDK only re-arms an already-run step through
// planner.Rollback, which only considers UPDATED fields - so page 1's
// trigger_seq=1 re-asks Risk System only because it changes this seeded 0.
// Without the seed, that first write would be newly-set and ignored. It is
// also what makes check_risk_system_pg's first call wait until both intake
// checks pass (trigger_seq is one of its required reads).
var writeSet = []common.HString{
	document.DocProcessScoringTriggerSeq,
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
		}, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, nil, conv, execFunc, nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	step := runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Normal, []runtime.ProcessStepId{})
	step.SetWriteIfEqual(runtime.None, nil)
	// Fires only once both intake checks have passed - a rejected
	// (age or duplicate-plate) customer must never get a re-ask counter, so
	// Risk System is never asked about them - and only once: after anyone has
	// written trigger_seq this must never fire again, since it always emits
	// the same constant 0, and re-running after survey pages have advanced
	// the counter would reset it.
	step.SetPrecondition(shouldSeed, common.MakePreConditionSet(
		[]common.HString{
			document.DocProcessAgeCheckPassed,
			document.DocProcessDuplicatePlateCheckPassed,
			document.DocStatus,
		},
		[]common.HString{document.DocProcessScoringTriggerSeq},
	))
	return step
}

func shouldSeed(_ workflow.Context, data map[common.HString]any) bool {
	ageOK, _ := data[document.DocProcessAgeCheckPassed].(bool)
	plateOK, _ := data[document.DocProcessDuplicatePlateCheckPassed].(bool)
	if !ageOK || !plateOK {
		return false
	}
	_, alreadySeeded := data[document.DocProcessScoringTriggerSeq]
	return !alreadySeeded
}
