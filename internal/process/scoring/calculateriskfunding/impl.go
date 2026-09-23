package calculateriskfunding

import (
	"context"
	"fmt"
	"math"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/fp"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"

	"lora-process-worker-playground/internal/process/document"
)

const ProcessAndActivityName = "calculate_risk_funding_pg"

var readSet = []common.HString{
	document.DocProcessScoringRiskSystemMaxLtv,
	document.DocProcessScoringRequiredDataSetSatisfied,
	document.DocProcessLoanStructureProvisionalAmount,
	document.DocProcessLoanStructureLtvSubmission,
}

var writeSet = []common.HString{
	document.DocProcessScoringCalculationEffectiveLtv,
	document.DocProcessScoringCalculationMaxFunding,
	document.DocProcessLoanStructureMaxFunding,
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

	execFunc := func(_ context.Context, data *map[common.HString]any) (*map[common.HString]any, error) {
		if !fp.As[document.DocTypeProcessScoringRequiredDataSetSatisfied]((*data)[document.DocProcessScoringRequiredDataSetSatisfied]) {
			return nil, fmt.Errorf("risk funding: verdict data set is not satisfied")
		}
		maxLTV := fp.As[document.DocTypeProcessScoringRiskSystemMaxLtv]((*data)[document.DocProcessScoringRiskSystemMaxLtv])
		submissionLTV := fp.As[document.DocTypeProcessLoanStructureLtvSubmission]((*data)[document.DocProcessLoanStructureLtvSubmission])
		provisionalAmount := fp.As[document.DocTypeProcessLoanStructureProvisionalAmount]((*data)[document.DocProcessLoanStructureProvisionalAmount])
		effectiveLTV := math.Min(maxLTV, submissionLTV)
		maxFunding := provisionalAmount * effectiveLTV

		out := map[common.HString]any{
			document.DocProcessScoringCalculationEffectiveLtv: effectiveLTV,
			document.DocProcessScoringCalculationMaxFunding:   maxFunding,
			document.DocProcessLoanStructureMaxFunding:        maxFunding,
		}
		return &out, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, nil, conv, execFunc, nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	step := runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Eager, []runtime.ProcessStepId{})
	step.SetReExecutionNeutral([]common.HString{document.DocProcessLoanStructureMaxFunding})
	return step
}

func CalculateFunding(provisionalAmount, submissionLTV, maxLTV float64) (effectiveLTV, maxFunding float64) {
	effectiveLTV = math.Min(submissionLTV, maxLTV)
	return effectiveLTV, provisionalAmount * effectiveLTV
}
