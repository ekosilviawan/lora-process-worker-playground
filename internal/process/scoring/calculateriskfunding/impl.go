package calculateriskfunding

import (
	"context"
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
	document.DocProcessLoanStructureProvisionalAmount,
	document.DocProcessLoanStructureLtvSubmission,
	document.DocStatus,
}

// ltvMaxOptional is Risk System's lending cap: absent before RS has ever
// responded for this submission (this step still runs off submissionLTV
// alone - an uncapped provisional estimate, mirroring production's
// calculate_pre_scoring_ndf2w/4w running before any risk-scoring verdict
// exists), present and tightening the result from then on. TriggerRollback
// is true - a deliberate departure from every other optional field in this
// repo (all default false) - because check_risk_system_pg writes SOME
// ltv_max value on every verdict starting at post_submission (defaulting to
// 0 well before the financing stage that actually sets a real cap), so the
// transition this step must react to is a later VALUE CHANGE to an
// already-set field, not a first appearance - only an update (not a
// newly-set field) feeds planner.Rollback. Safe to enable broadly here:
// this step is ltv_max's only reader, and its own writes are already
// SetReExecutionNeutral below, so nothing cascades further.
var optionalReadSet = []common.OptionalPath{
	{Path: document.DocProcessLoanStructureLtvMax, Strategy: common.OptionalIgnoreIfLocked, TriggerRollback: true},
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
	for _, path := range optionalReadSet {
		docFieldCheck([]common.HString{path.Path})
	}

	convIn := mapping.NewSimpleInputConverter[map[common.HString]any]()
	convIn.SetInput(common.MakeReadSet(readSet).SetOptionals(optionalReadSet, true), func(m map[common.HString]any) (*map[common.HString]any, error) { return &m, nil })
	convOut := mapping.NewSimpleOutputConverter[map[common.HString]any]()
	convOut.SetOutput(common.MakeWriteSet(writeSet), func(data *map[common.HString]any) (map[common.HString]any, error) { return *data, nil })
	conv := mapping.NewSimpleConverterFrom(convIn, convOut)

	execFunc := func(_ context.Context, data *map[common.HString]any) (*map[common.HString]any, error) {
		submissionLTV := fp.As[document.DocTypeProcessLoanStructureLtvSubmission]((*data)[document.DocProcessLoanStructureLtvSubmission])
		provisionalAmount := fp.As[document.DocTypeProcessLoanStructureProvisionalAmount]((*data)[document.DocProcessLoanStructureProvisionalAmount])

		maxLTV := math.Inf(1) // no Risk System cap communicated yet - treat as uncapped
		if raw, ok := (*data)[document.DocProcessLoanStructureLtvMax]; ok {
			maxLTV = fp.As[document.DocTypeProcessLoanStructureLtvMax](raw)
		}

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
	step.SetWriteIfEqual(runtime.None, nil)
	step.SetReExecutionNeutral([]common.HString{document.DocProcessLoanStructureMaxFunding})
	return step
}

func CalculateFunding(provisionalAmount, submissionLTV, maxLTV float64) (effectiveLTV, maxFunding float64) {
	effectiveLTV = math.Min(submissionLTV, maxLTV)
	return effectiveLTV, provisionalAmount * effectiveLTV
}
