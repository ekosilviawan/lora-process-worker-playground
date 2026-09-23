package runsurvey

import (
	"context"
	"fmt"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/fp"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"

	"lora-process-worker-playground/internal/process/document"
)

const ProcessAndActivityName = "run_survey_pg"

// readSet depends on eligibility_passed so the survey only starts after the
// initial eligibility chain has settled, and on survey_type so it never
// runs before Risk System's pre-survey checkpoint has told LORA which
// survey the user needs to complete next (applyrisksystemverdict writes
// survey_type from the verdict's required_data_set).
var readSet = []common.HString{
	document.DocProcessEligibilityPassed,
	document.DocProcessScoringSurveyType,
	document.DocProcessScoringTriggerSeq,
}

// writeSet is the union of every survey type's findings plus the cursor
// fields. Writing the cursor here, in the same activity that writes the
// survey's own findings, mirrors LTW bundling trigger_seq with the page
// payload in a single partial completion.
var writeSet = []common.HString{
	document.DocCustomerBirthDate,
	document.DocCustomerName,
	document.DocProcessAssetCondition,
	document.DocProcessLoanStructureProvisionalAmount,
	document.DocProcessLoanStructureLtvSubmission,
	document.DocProcessIncomeVerifiedAmount,
	document.DocProcessFinalReviewConfirmed,
	document.DocProcessScoringTriggerSeq,
	document.DocProcessScoringStageToken,
}

// Hardcoded survey findings, one set per survey type.
const (
	surveyVerifiedBirthDate  = "1985-03-15"
	surveyVerifiedName       = "Jane Smith"
	surveyAssetCondition     = "fair"
	surveyProvisionalAmount  = 100000.0
	surveyLtvSubmission      = 0.8
	surveyVerifiedIncome     = 15000000.0
	surveyFinalReviewConfirm = true
)

// surveyOutcome is what completing one survey type produces: the findings
// it writes and the stage the cursor advances to. Adding a new survey type
// means adding one entry here plus its gate in checkrisksystem.stageGates -
// nothing else in the chain changes.
type surveyOutcome struct {
	nextStage string
	fields    func() map[common.HString]any
}

var surveyOutcomesByType = map[string]surveyOutcome{
	"identity": {
		nextStage: "customer_verification",
		fields: func() map[common.HString]any {
			return map[common.HString]any{
				document.DocCustomerBirthDate: surveyVerifiedBirthDate,
				document.DocCustomerName:      surveyVerifiedName,
			}
		},
	},
	"asset": {
		nextStage: "asset_review",
		fields: func() map[common.HString]any {
			return map[common.HString]any{document.DocProcessAssetCondition: surveyAssetCondition}
		},
	},
	"financing": {
		nextStage: "financing",
		fields: func() map[common.HString]any {
			return map[common.HString]any{
				document.DocProcessLoanStructureProvisionalAmount: surveyProvisionalAmount,
				document.DocProcessLoanStructureLtvSubmission:     surveyLtvSubmission,
			}
		},
	},
	"income": {
		nextStage: "income_review",
		fields: func() map[common.HString]any {
			return map[common.HString]any{document.DocProcessIncomeVerifiedAmount: surveyVerifiedIncome}
		},
	},
	"final_review": {
		nextStage: "final_review",
		fields: func() map[common.HString]any {
			return map[common.HString]any{document.DocProcessFinalReviewConfirmed: surveyFinalReviewConfirm}
		},
	},
}

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
		surveyType := fp.As[document.DocTypeProcessScoringSurveyType]((*data)[document.DocProcessScoringSurveyType])
		outcome, ok := surveyOutcomesByType[surveyType]
		if !ok {
			return nil, fmt.Errorf("run survey: unsupported survey type %q", surveyType)
		}
		// trigger_seq round-trips through JSON (ArangoDB storage, data-forward),
		// which decodes numbers as float64 - fp.As[int] would panic here.
		seq := fp.MustJsonNumAsInt((*data)[document.DocProcessScoringTriggerSeq])

		mOut := outcome.fields()
		mOut[document.DocProcessScoringTriggerSeq] = seq + 1
		mOut[document.DocProcessScoringStageToken] = outcome.nextStage
		return &mOut, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, nil, conv, execFunc, nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	return runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Normal, []runtime.ProcessStepId{})
}
