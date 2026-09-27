// Package checkrisksystemverdict aggregates every Risk System stage's answer
// (checkrisksystem records one per data set, on its own
// $.process.scoring.risk_system_stage.<stage>.* fields) into the one Risk
// System verdict - $.process.scoring.risk_system.* - and interprets it:
// required_data_set -> survey_type, status -> $.status, terminal timestamps,
// max_ltv -> ltv_max, and the underwriting eligibility gate. It exists as
// separate, ordinary (synchronous) activities - not folded into the stages'
// async handler - because that handler is a runtime.AsyncPayloadHandler
// (func(map[string]any) (map[common.HString]any, error)): it only ever
// receives the raw external payload, never current document state. The
// aggregation needs every stage's answer, and the underwriting gate needs
// environment_check.result and product_type, which only an ordinary activity
// - with a normal readSet like any other Constructor in this repo - can see.
//
// The aggregation rule: the most advanced stage that has answered wins
// (checkrisksystem.All's order). That stage's call sent everything collected
// up to it, and it is re-asked on any change to that data, so its answer is
// always the one made on the freshest data.
//
// There is one aggregator step per stage, all computing the same pure
// function of every stage's answer. A stage's FIRST answer is newly-set,
// which re-arms nothing through planner.Rollback, so a single aggregator that
// had already run would never see it; the stage's own aggregator, which
// requires that answer, becomes runnable the moment it appears. Later answers
// are updates (checkrisksystem writes every output field on every answer),
// which re-run the aggregators through their rollback-triggering reads.
// Several aggregators writing the same fields is safe for the same reason as
// calculateriskfunding's two steps: each writes the correct result for the
// current answers, and a history fast-forward replays the correct result for
// identical input.
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
	"lora-process-worker-playground/internal/process/scoring/checkrisksystem"
	"lora-process-worker-playground/internal/process/tasking/survey"
)

// ProcessAndActivityName is the intake stage's aggregator; every other
// stage's aggregator appends the stage's suffix (see stepName).
const ProcessAndActivityName = "check_risk_system_pg_verdict"

// stepName names the aggregator for a stage: check_risk_system_pg_verdict for
// intake, check_risk_system_pg_verdict_<stage> for the rest, mirroring the
// stage's own check_risk_system_pg_<stage> name.
func stepName(stage checkrisksystem.DataSet) string {
	return ProcessAndActivityName + stage.Name[len(checkrisksystem.Intake.Name):]
}

// eligibilityReadSet is read (not rollback-triggering) because the
// underwriting eligibility gate must run BEFORE survey_type is ever written -
// the stages' async handler can't check it (see the package comment), so this
// is the only place that can refuse to write survey_type=underwriting for an
// ineligible applicant.
var eligibilityReadSet = []common.OptionalPath{
	{Path: document.DocProcessEnvironmentCheckResult, Strategy: common.OptionalWaitIfLocked},
	{Path: document.DocProcessLoanStructureProductType, Strategy: common.OptionalWaitIfLocked},
}

// readSet requires the stage's own answer and reads every other stage's
// answer as a rollback-triggering optional, wait-if-locked - so an aggregator
// re-runs on any stage's new answer, and never decides while a stage call is
// pending (a pending stage write-locks its outputs).
func readSet(stage checkrisksystem.DataSet) *common.ReadSet {
	optional := make([]common.OptionalPath, 0)
	for _, other := range checkrisksystem.All {
		if other.Name == stage.Name {
			continue
		}
		for _, path := range other.Output.Paths() {
			optional = append(optional, common.OptionalPath{Path: path, Strategy: common.OptionalWaitIfLocked, TriggerRollback: true})
		}
	}
	optional = append(optional, eligibilityReadSet...)
	return common.MakeReadSet(stage.Output.Paths()).SetOptionals(optional, true)
}

// writeSet is the aggregated Risk System verdict record plus its
// interpretation.
var writeSet = []common.HString{
	document.DocProcessScoringRiskSystemRequestId,
	document.DocProcessScoringRiskSystemStatus,
	document.DocProcessScoringRiskSystemRequiredDataSet,
	document.DocProcessScoringRiskSystemMaxLtv,
	document.DocProcessScoringRiskSystemRejectReason,
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

// Constructor builds the aggregator for one stage.
type Constructor struct {
	Stage checkrisksystem.DataSet
	f     *runtime.Function[any, any]
}

func (c *Constructor) GenerateFunction(
	_ func(url string) (*framework.APIFunction, error),
	docFieldCheck func([]common.HString),
	_ *framework.System,
	_ *defs.DocumentDescriptor,
) error {
	rs := readSet(c.Stage)
	docFieldCheck(rs.Paths)
	for _, path := range rs.OptionalPaths {
		docFieldCheck([]common.HString{path.Path})
	}
	docFieldCheck(writeSet)

	convIn := mapping.NewSimpleInputConverter[map[common.HString]any]()
	convIn.SetInput(rs, func(m map[common.HString]any) (*map[common.HString]any, error) {
		return &m, nil
	})
	convOut := mapping.NewSimpleOutputConverter[map[common.HString]any]()
	convOut.SetOutput(common.MakeWriteSet(writeSet), func(data *map[common.HString]any) (map[common.HString]any, error) {
		return *data, nil
	})
	conv := mapping.NewSimpleConverterFrom(convIn, convOut)

	execFunc := func(_ context.Context, data *map[common.HString]any) (*map[common.HString]any, error) {
		out, err := aggregate(*data)
		if err != nil {
			return nil, err
		}
		return &out, nil
	}

	c.f = runtime.NewAnyFunction(stepName(c.Stage), nil, conv, execFunc, nil)
	return nil
}

// aggregate picks the most advanced stage that has answered, records its
// answer as the Risk System verdict (risk_system.*) and interprets it.
func aggregate(data map[common.HString]any) (map[common.HString]any, error) {
	var winner *checkrisksystem.DataSet
	for i := len(checkrisksystem.All) - 1; i >= 0; i-- {
		if _, answered := data[checkrisksystem.All[i].Output.Status]; answered {
			winner = &checkrisksystem.All[i]
			break
		}
	}
	if winner == nil {
		return nil, fmt.Errorf("check risk system verdict: no stage has answered")
	}

	verdict := map[common.HString]any{
		document.DocProcessScoringRiskSystemRequestId:       data[winner.Output.RequestId],
		document.DocProcessScoringRiskSystemStatus:          data[winner.Output.Status],
		document.DocProcessScoringRiskSystemRequiredDataSet: data[winner.Output.RequiredDataSet],
		document.DocProcessScoringRiskSystemMaxLtv:          data[winner.Output.MaxLtv],
		document.DocProcessScoringRiskSystemRejectReason:    data[winner.Output.RejectReason],
	}
	interpreted := map[common.HString]any{}
	for k, v := range verdict {
		interpreted[k] = v
	}
	for _, path := range eligibilityReadSet {
		if v, ok := data[path.Path]; ok {
			interpreted[path.Path] = v
		}
	}
	out, err := applyVerdict(interpreted)
	if err != nil {
		return nil, fmt.Errorf("%w (deciding stage: %s)", err, winner.Name)
	}
	for k, v := range verdict {
		out[k] = v
	}
	return out, nil
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
	step := runtime.NewProcessStep(runtime.ProcessStepId(stepName(c.Stage)), c.f, runtime.Normal, []runtime.ProcessStepId{})
	step.SetWriteIfEqual(runtime.None, nil)
	// Aggregators sit downstream of every stage and are re-impacted whenever
	// a stage answers again. Without this, that rollback would revert
	// survey_type/$.status/ltv_max back to unset purely as a side effect of
	// Risk System being asked again, destroying an already-applied verdict.
	step.SetRetainDataOnRollback()
	// No custom precondition: the stage's own answer (the required reads) is
	// the gate - the aggregator simply doesn't run until that stage has
	// answered.
	return step
}
