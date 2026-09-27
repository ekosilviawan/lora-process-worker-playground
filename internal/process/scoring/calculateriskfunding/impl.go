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

const (
	ProcessAndActivityName       = "calculate_risk_funding_pg"
	CappedProcessAndActivityName = "calculate_risk_funding_pg_capped"
)

var readSet = []common.HString{
	document.DocProcessLoanStructureProvisionalAmount,
	document.DocProcessLoanStructureLtvSubmission,
	document.DocStatus,
}

// ltvMaxOptional is Risk System's lending cap: absent before RS has sent one
// for this submission (this step still runs off submissionLTV alone - an
// uncapped provisional estimate, mirroring production's
// calculate_pre_scoring_ndf2w/4w running before any risk-scoring verdict
// exists), present and tightening the result from then on. TriggerRollback
// is true - a deliberate departure from almost every other optional field in
// this repo - so a later CHANGE to the cap re-runs this step: only an update
// (not a newly-set field) feeds planner.Rollback. The cap's FIRST appearance
// is newly-set and so re-runs nothing here; calculate_risk_funding_pg_capped
// (CappedConstructor) covers exactly that case. Safe to enable: this step's
// own writes are SetReExecutionNeutral below, so nothing cascades further.
var optionalReadSet = []common.OptionalPath{
	{Path: document.DocProcessLoanStructureLtvMax, Strategy: common.OptionalWaitIfLocked, TriggerRollback: true},
}

// cappedReadSet is readSet plus the cap as a REQUIRED read, so
// calculate_risk_funding_pg_capped becomes runnable the moment
// check_risk_system_pg_verdict first writes ltv_max - the data-set trigger the
// SDK gives a step that has never run - and, like any required read, re-runs
// on every later change to it or to the financing fields.
var cappedReadSet = append(append([]common.HString{}, readSet...), document.DocProcessLoanStructureLtvMax)

var writeSet = []common.HString{
	document.DocProcessScoringCalculationEffectiveLtv,
	document.DocProcessScoringCalculationMaxFunding,
	document.DocProcessLoanStructureMaxFunding,
}

// Constructor is the calculation that runs as soon as the submission's
// financing data exists, capped by ltv_max whenever one is present.
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
	for _, path := range optionalReadSet {
		docFieldCheck([]common.HString{path.Path})
	}
	c.f = newFunction(ProcessAndActivityName, common.MakeReadSet(readSet).SetOptionals(optionalReadSet, true), docFieldCheck)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	return newProcessStep(ProcessAndActivityName, c.f)
}

// CappedConstructor is the same calculation split out for the cap's data set:
// see cappedReadSet. The two steps write the same fields, but both are pure
// functions of the document's current values (both read ltv_max), so
// whichever runs last writes the correct result, and a history fast-forward
// of either replays the correct result for identical input.
type CappedConstructor struct {
	f *runtime.Function[any, any]
}

func (c *CappedConstructor) GenerateFunction(
	_ func(url string) (*framework.APIFunction, error),
	docFieldCheck func([]common.HString),
	_ *framework.System,
	_ *defs.DocumentDescriptor,
) error {
	docFieldCheck(cappedReadSet)
	c.f = newFunction(CappedProcessAndActivityName, common.MakeReadSet(cappedReadSet), docFieldCheck)
	return nil
}

func (c *CappedConstructor) GenerateProcessStep() *runtime.ProcessStep {
	return newProcessStep(CappedProcessAndActivityName, c.f)
}

func newFunction(name string, rs *common.ReadSet, docFieldCheck func([]common.HString)) *runtime.Function[any, any] {
	docFieldCheck(writeSet)

	convIn := mapping.NewSimpleInputConverter[map[common.HString]any]()
	convIn.SetInput(rs, func(m map[common.HString]any) (*map[common.HString]any, error) { return &m, nil })
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

	return runtime.NewAnyFunction(name, nil, conv, execFunc, nil)
}

func newProcessStep(name runtime.ProcessStepId, f *runtime.Function[any, any]) *runtime.ProcessStep {
	step := runtime.NewProcessStep(name, f, runtime.Normal, []runtime.ProcessStepId{})
	step.SetWriteIfEqual(runtime.None, nil)
	step.SetReExecutionNeutral([]common.HString{document.DocProcessLoanStructureMaxFunding})
	return step
}

func CalculateFunding(provisionalAmount, submissionLTV, maxLTV float64) (effectiveLTV, maxFunding float64) {
	effectiveLTV = math.Min(submissionLTV, maxLTV)
	return effectiveLTV, provisionalAmount * effectiveLTV
}
