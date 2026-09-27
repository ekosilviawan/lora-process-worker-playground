// Package checkrisksystemverdict interprets what check_risk_system_pg
// recorded from Risk System's raw verdict. It exists as a separate,
// ordinary (synchronous) activity - not folded into checkrisksystem's own
// async handler - because checkrisksystem's asyncHandler is a
// runtime.AsyncPayloadHandler (func(map[string]any) (map[common.HString]any,
// error)): it only ever receives the raw external payload, never current
// document state. The underwriting eligibility gate below needs
// environment_check.result and product_type from the document, which only an ordinary activity - with
// a normal readSet like any other Constructor in this repo - can see.
package checkrisksystemverdict

import (
	"context"
	"fmt"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"

	"lora-process-worker-playground/internal/process/document"
	"lora-process-worker-playground/internal/process/tasking/survey"
)

const ProcessAndActivityName = "check_risk_system_pg_verdict"

// requiredReadSet is the Risk System calls' own raw recording of RS's
// verdict - this step doesn't run until the first call has completed.
//
// request_id is what makes this step interpret EVERY later call, not only
// those that change status/required_data_set. There is one RS step per data
// set (see checkrisksystem), all writing these same fields, and each call
// mints a new request_id - an UPDATE after the first call - so every call
// re-runs this step through planner.Rollback against the fields' current
// values: the latest call's. Without it, a call that repeats the previous
// status/required_data_set but sends a cap for the first time (max_ltv newly
// set, which re-arms nothing) would never have that cap applied until some
// later verdict changed status - too late if that one is terminal.
var requiredReadSet = []common.HString{
	document.DocProcessScoringRiskSystemRequestId,
	document.DocProcessScoringRiskSystemRequiredDataSet,
	document.DocProcessScoringRiskSystemStatus,
}

// environment_check.result/product_type are read here (not just in survey)
// because the underwriting eligibility gate below must run BEFORE
// survey_type is ever written - checkrisksystem's asyncHandler can't check
// them itself (see the package comment), so this is the only place that can
// refuse to write survey_type=underwriting for an ineligible applicant.
var optionalReadSet = []common.OptionalPath{
	{Path: document.DocProcessScoringRiskSystemMaxLtv, Strategy: common.OptionalWaitIfLocked, TriggerRollback: true},
	{Path: document.DocProcessScoringRiskSystemRejectReason, Strategy: common.OptionalWaitIfLocked, TriggerRollback: true},
	{Path: document.DocProcessEnvironmentCheckResult, Strategy: common.OptionalWaitIfLocked},
	{Path: document.DocProcessLoanStructureProductType, Strategy: common.OptionalWaitIfLocked},
}

var writeSet = []common.HString{
	document.DocStatus,
	document.DocStatusReason,
	document.DocProcessStatusTimestampsApproved,
	document.DocProcessStatusTimestampsRejected,
	document.DocProcessStatusTimestampsTerminal,
	document.DocProcessLoanStructureLtvMax,
	document.DocProcessScoringSurveyType,
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
//
// underwriting-v1 is the one entry whose selectability isn't purely a
// mapping concern: survey.IsUnderwritingEligible must also agree (high_risk
// path completed, NDF4W product) before execFunc below will actually write
// survey_type=underwriting - see that function.
//
// The granular checkpoints (CUSTOMER_VERIFICATION/ASSET_REVIEW/FINANCING/
// INCOME_REVIEW) are deliberately absent: they're no longer independently
// selectable survey types (see tasking/survey.surveyOutcomesByType) - they're
// only reachable as pages within the "normal" multi-page survey below - so
// RS asking for one of them directly must fail safe here too, same as any
// other unrecognised value.
var surveyTypeByDataSet = map[string]string{
	"underwriting-v1": "underwriting",
	// survey-normal-v1 is RS's "just run the standard survey" identifier: it
	// maps onto a single multi-page SURVEY process (survey type "normal")
	// rather than one granular dataset at a time. See tasking/survey, where
	// "normal" is a multi-page outcome - each page's submission is a partial
	// completion, only the final page closes the task.
	"survey-normal-v1": "normal",
	// survey-high-risk-v1 selects the "high_risk" survey process: RS's
	// identifier for applicants it flags as high risk (e.g. income too low,
	// asset condition poor, provisional amount too high). See tasking/survey,
	// "high_risk" - it can also be the result of RS re-assessing a document
	// mid-way through the "normal" survey, escalating it onto this outcome.
	"survey-high-risk-v1": "high_risk",
}

func surveyTypeForDataSet(dataSet string) (string, bool) {
	surveyType, ok := surveyTypeByDataSet[dataSet]
	return surveyType, ok
}

// ValidStatus and ValidRequiredDataSet let a caller validate a verdict
// before completing check_risk_system_pg (system.CompleteActivityByID).
// Since that's a genuine async Temporal activity, nothing validates the
// payload before completion the way a data-forward update's JSON Schema gate
// used to - an invalid value discovered only inside this step's execFunc
// doesn't just fail this activity, it retries indefinitely (Temporal's
// default retry policy), surfacing as a stuck activity rather than failing
// fast. cmd/testcli's "verdict" command calls these first, mirroring where
// that schema gate (LGS's real proxy validation) used to sit.
func ValidStatus(status string) bool {
	_, ok := translateStatus(status)
	return ok
}

func ValidRequiredDataSet(dataSet string) bool {
	_, ok := surveyTypeByDataSet[dataSet]
	return ok
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
	docFieldCheck(requiredReadSet)
	docFieldCheck(writeSet)
	for _, path := range optionalReadSet {
		docFieldCheck([]common.HString{path.Path})
	}

	convIn := mapping.NewSimpleInputConverter[map[common.HString]any]()
	convIn.SetInput(common.MakeReadSet(requiredReadSet).SetOptionals(optionalReadSet, true), func(m map[common.HString]any) (*map[common.HString]any, error) {
		return &m, nil
	})
	convOut := mapping.NewSimpleOutputConverter[map[common.HString]any]()
	convOut.SetOutput(common.MakeWriteSet(writeSet), func(data *map[common.HString]any) (map[common.HString]any, error) {
		return *data, nil
	})
	conv := mapping.NewSimpleConverterFrom(convIn, convOut)

	execFunc := func(_ context.Context, data *map[common.HString]any) (*map[common.HString]any, error) {
		out, err := applyVerdict(*data)
		if err != nil {
			return nil, err
		}
		return &out, nil
	}

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, nil, conv, execFunc, nil)
	return nil
}

// applyVerdict is execFunc's actual logic, pulled out as a plain function so
// it's unit-testable directly (runtime.Function.Execute requires a real
// Temporal activity context, which is what execFunc's wiring above provides
// in production but tests can't cheaply fake).
func applyVerdict(data map[common.HString]any) (map[common.HString]any, error) {
	requiredDataSet, _ := data[document.DocProcessScoringRiskSystemRequiredDataSet].(string)
	status, _ := data[document.DocProcessScoringRiskSystemStatus].(string)
	maxLTV, _ := data[document.DocProcessScoringRiskSystemMaxLtv].(float64)
	rejectReason, _ := data[document.DocProcessScoringRiskSystemRejectReason].(string)

	surveyType, ok := surveyTypeForDataSet(requiredDataSet)
	if !ok {
		return nil, fmt.Errorf("check risk system verdict: unsupported required data set %q", requiredDataSet)
	}
	mappedStatus, ok := translateStatus(status)
	if !ok {
		return nil, fmt.Errorf("check risk system verdict: unsupported verdict status %q", status)
	}

	// This is the one gate checkrisksystem's asyncHandler cannot enforce
	// itself (see the package comment): a real Risk System should never
	// request underwriting for an applicant who didn't complete the
	// high_risk survey or isn't NDF4W, since product_type is part of what
	// LPW sends it - but if it does anyway (bug, or a deliberately
	// inconsistent test verdict), refuse to write survey_type=underwriting at
	// all. Returning an error here causes Temporal to retry this activity
	// indefinitely, surfacing as a visibly stuck/failing
	// check_risk_system_pg_verdict activity in Temporal UI - a diagnosable
	// failure, not a silent stall and not an automatic loan rejection (a
	// data/integration mismatch between RS and LORA is a bug, not a reason to
	// reject a customer's loan).
	if surveyType == "underwriting" && !survey.IsUnderwritingEligible(data) {
		_, highRiskCompleted := data[document.DocProcessEnvironmentCheckResult]
		productType, _ := data[document.DocProcessLoanStructureProductType].(string)
		return nil, fmt.Errorf(
			"check risk system verdict: risk system requested underwriting for an ineligible applicant (high_risk survey completed=%t, product_type=%q); expected environment_check.result present and product_type=NDF4W - Risk System and LORA disagree, needs investigation",
			highRiskCompleted, productType,
		)
	}

	out := map[common.HString]any{
		document.DocStatus:                   mappedStatus,
		document.DocProcessScoringSurveyType: surveyType,
	}
	if maxLTV > 0 {
		out[document.DocProcessLoanStructureLtvMax] = maxLTV
	}
	if rejectReason != "" {
		out[document.DocStatusReason] = rejectReason
	}
	if mappedStatus == "approved" || mappedStatus == "rejected" {
		document.SetStatusTimestamp(out, mappedStatus)
	}
	return out, nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	step := runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Normal, []runtime.ProcessStepId{})
	step.SetWriteIfEqual(runtime.None, nil)
	// This step sits downstream of every Risk System call (one per data set,
	// see checkrisksystem): each later call's verdict updates the
	// risk_system.* fields this step reads, which makes planner.Rollback
	// re-impact it. Without this, that rollback would revert
	// survey_type/$.status/ltv_max back to unset purely as a side effect of
	// Risk System being asked again, destroying an already-applied verdict.
	step.SetRetainDataOnRollback()
	// No custom precondition: this step's requiredReadSet
	// (risk_system.required_data_set/status) is itself the gate, matching
	// calculateriskfunding's own lack of an explicit SetPrecondition - it
	// simply doesn't run until checkrisksystem has recorded a verdict.
	return step
}
