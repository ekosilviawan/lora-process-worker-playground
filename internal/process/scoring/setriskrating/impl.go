package setriskrating

import (
	"context"
	"time"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/fp"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"

	"lora-process-worker-playground/internal/process/document"
)

// ProcessAndActivityName is the Temporal activity name registered in the worker.
const ProcessAndActivityName = "set_risk_rating_pg"

// readSet deliberately includes DocProcessEligibilityPassed so the data-driven
// planner schedules this activity only after check_customer_eligibility_pg writes it.
var readSet = []common.HString{
	document.DocProcessEligibilityPassed,
	document.DocCustomerBirthDate,
}

var writeSet = []common.HString{
	document.DocProcessRiskRating,
}

const (
	ratingLow    = "low"
	ratingMedium = "medium"
	ratingHigh   = "high"

	ageLowMax    = 30
	ageMediumMax = 50

	birthDateLayout = "2006-01-02"
)

// Constructor wires the activity into the process-sdk planner.
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

		eligible := fp.As[document.DocTypeProcessEligibilityPassed]((*data)[document.DocProcessEligibilityPassed])
		if !eligible {
			// customer was rejected by the eligibility check; no rating to set
			return &mOut, nil
		}

		birthDateStr := fp.As[document.DocTypeCustomerBirthDate]((*data)[document.DocCustomerBirthDate])
		age, err := calculateAge(birthDateStr)
		if err != nil {
			return &mOut, err
		}

		mOut[document.DocProcessRiskRating] = riskRating(age)
		return &mOut, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, nil, conv, execFunc, nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	return runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Normal, []runtime.ProcessStepId{})
}

func calculateAge(birthDateStr string) (int, error) {
	birthDate, err := time.Parse(birthDateLayout, birthDateStr)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	age := now.Year() - birthDate.Year()
	if now.Month() < birthDate.Month() ||
		(now.Month() == birthDate.Month() && now.Day() < birthDate.Day()) {
		age--
	}
	return age, nil
}

func riskRating(age int) string {
	switch {
	case age < ageLowMax:
		return ratingLow
	case age <= ageMediumMax:
		return ratingMedium
	default:
		return ratingHigh
	}
}
