package applyrisksystemverdict

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

const ProcessAndActivityName = "apply_risk_system_verdict_pg"

var readSet = []common.HString{
	document.DocProcessScoringRiskSystemStatus,
	document.DocProcessScoringRiskSystemRejectReason,
	document.DocProcessScoringRiskSystemRequiredDataSet,
	document.DocProcessScoringRiskSystemMaxLtv,
}

var writeSet = []common.HString{
	document.DocStatus,
	document.DocStatusReason,
	document.DocProcessStatusTimestampsApproved,
	document.DocProcessStatusTimestampsRejected,
	document.DocProcessStatusTimestampsTerminal,
	document.DocProcessLoanStructureLtvMax,
	document.DocProcessScoringRequiredDataSetSatisfied,
	document.DocProcessScoringSurveyType,
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
	convIn.SetInput(common.MakeReadSet(readSet), func(m map[common.HString]any) (*map[common.HString]any, error) { return &m, nil })
	convOut := mapping.NewSimpleOutputConverter[map[common.HString]any]()
	convOut.SetOutput(common.MakeWriteSet(writeSet), func(data *map[common.HString]any) (map[common.HString]any, error) { return *data, nil })
	conv := mapping.NewSimpleConverterFrom(convIn, convOut)

	execFunc := func(_ context.Context, data *map[common.HString]any) (*map[common.HString]any, error) {
		status := fp.As[document.DocTypeProcessScoringRiskSystemStatus]((*data)[document.DocProcessScoringRiskSystemStatus])
		requiredDataSet := fp.As[document.DocTypeProcessScoringRiskSystemRequiredDataSet]((*data)[document.DocProcessScoringRiskSystemRequiredDataSet])
		surveyType, ok := surveyTypeForDataSet(requiredDataSet)
		if !ok {
			return nil, fmt.Errorf("risk system: unsupported required data set %q", requiredDataSet)
		}
		mappedStatus, ok := translateStatus(status)
		if !ok {
			return nil, fmt.Errorf("risk system: unsupported verdict status %q", status)
		}

		out := map[common.HString]any{
			document.DocStatus:                                 mappedStatus,
			document.DocProcessLoanStructureLtvMax:             (*data)[document.DocProcessScoringRiskSystemMaxLtv],
			document.DocProcessScoringRequiredDataSetSatisfied: true,
			document.DocProcessScoringSurveyType:               surveyType,
		}
		if reason, exists := (*data)[document.DocProcessScoringRiskSystemRejectReason]; exists {
			out[document.DocStatusReason] = fp.As[document.DocTypeStatusReason](reason)
		}
		if mappedStatus == "approved" || mappedStatus == "rejected" {
			document.SetStatusTimestamp(out, mappedStatus)
		}
		return &out, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, nil, conv, execFunc, nil)
	return nil
}

func translateStatus(status string) (string, bool) {
	mappedStatus, ok := map[string]string{"approved": "approved", "rejected": "rejected", "pending": "processing"}[status]
	return mappedStatus, ok
}

// surveyTypeByDataSet maps what Risk System says it still needs onto which
// survey type the user must complete next. RS decides this internally from
// its own risk-level calculation (a blackbox to LORA, per the design tenet);
// LORA only needs the resulting required_data_set identifier. An unknown
// value fails safe (§2.4: an in-flight worker can be older than RS's release
// cadence and must not silently stall or guess).
var surveyTypeByDataSet = map[string]string{
	"CUSTOMER_VERIFICATION": "identity",
	"ASSET_REVIEW":          "asset",
	"FINANCING":             "financing",
	"INCOME_REVIEW":         "income",
	"FINAL_REVIEW":          "final_review",
}

func surveyTypeForDataSet(dataSet string) (string, bool) {
	surveyType, ok := surveyTypeByDataSet[dataSet]
	return surveyType, ok
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	return runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Normal, []runtime.ProcessStepId{})
}
