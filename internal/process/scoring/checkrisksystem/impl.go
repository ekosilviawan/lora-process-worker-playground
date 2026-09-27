// Package checkrisksystem asks Risk System (RS) in stages: one process step
// per data set (intake, asset, income, environment_check, underwriting). Each
// stage records RS's raw answer on its OWN fields,
// $.process.scoring.risk_system_stage.<stage>.*, and
// checkrisksystemverdict aggregates every stage's answer into the one Risk
// System verdict the rest of the flow acts on (see
// RISK_SYSTEM_RETRIGGER_OPTIONS.md, option 5).
//
// What asks RS:
//   - A stage's own data-set fields are REQUIRED reads, so a stage that has
//     never run becomes runnable the moment its data first appears.
//   - Every field of every EARLIER data set is a rollback-triggering optional
//     read, so each call sends everything collected up to that stage, and any
//     later CHANGE to that data - a revised page, or a final page that only
//     updates existing fields - re-asks the stage through planner.Rollback.
//     Freshness is a property of these read sets, not of how the survey's
//     pages happen to be laid out.
//   - A stage steps aside once a LATER stage has answered (its precondition,
//     laterStageAnswered): the aggregator uses the most advanced stage's
//     answer, and that stage already sees every earlier field, so re-asking
//     an earlier one too would only add a redundant call. So each change asks
//     RS exactly once, through the most advanced stage.
//
// Why this converges where one shared set of risk_system.* fields could not:
// a stage re-queued by Rollback's reachability walk with an unchanged input
// fast-forwards through document history, which can now only restore THAT
// stage's own answer, for that same input, into its own fields - never
// overwrite a newer answer another stage has since recorded. Identical
// replayed values are not updates, so nothing cascades further. (It does mean
// a re-queued stage with an unchanged input reuses RS's earlier answer instead
// of calling RS again.)
//
// Mutual exclusion is the SDK's locking: a pending stage read-locks the
// fields it reads, which are in survey's writeSet, so FieldLock.CanSetWrite
// keeps survey from opening its next page until the answer lands; and the
// aggregator reads every stage's outputs wait-if-locked, so it never decides
// while a call is pending. After a page, a stage (runtime.Normal) always wins
// the scheduling round over survey (runtime.Lazy).
package checkrisksystem

import (
	"context"
	"time"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
	"github.com/google/uuid"
	"go.temporal.io/sdk/workflow"

	"lora-process-worker-playground/internal/process/document"
)

// StageOutput is where one stage records RS's raw answer, verbatim.
type StageOutput struct {
	RequestId       common.HString
	Status          common.HString
	RequiredDataSet common.HString
	MaxLtv          common.HString
	RejectReason    common.HString
}

// Paths lists every output field.
func (o StageOutput) Paths() []common.HString {
	return []common.HString{o.RequestId, o.Status, o.RequiredDataSet, o.MaxLtv, o.RejectReason}
}

// DataSet is one stage of asking Risk System: the activity name of the step
// that asks, the data set's own fields (Required must all be present before
// the stage asks; Optional are sent when present), and where the stage
// records the answer.
type DataSet struct {
	Name     string
	Required []common.HString
	Optional []common.HString
	Output   StageOutput
}

// Fields returns every field this data set owns.
func (ds DataSet) Fields() []common.HString {
	return append(append([]common.HString{}, ds.Required...), ds.Optional...)
}

var (
	// Intake is the first call, before any survey runs: the customer
	// identity, product and requested financing the initial submission
	// carries. The identity and financing survey pages revise these fields.
	//
	// product_type is sent alongside the customer identity fields - RS's own
	// required-data-set decision is made with full knowledge of the loan's
	// product, so a correctly-behaving RS should never request underwriting-v1
	// for an NDF2W applicant in the first place. survey.IsUnderwritingEligible
	// (enforced in checkrisksystemverdict) is the backstop for when it does
	// anyway.
	Intake = DataSet{
		Name: "check_risk_system_pg",
		Required: []common.HString{
			document.DocId,
			document.DocCustomerNik,
			document.DocCustomerName,
			document.DocProcessLoanStructureProductType,
		},
		Optional: []common.HString{
			document.DocCustomerBirthDate,
			document.DocProcessLoanStructureProvisionalAmount,
			document.DocProcessLoanStructureLtvSubmission,
		},
		Output: StageOutput{
			RequestId:       document.DocProcessScoringRiskSystemStageIntakeRequestId,
			Status:          document.DocProcessScoringRiskSystemStageIntakeStatus,
			RequiredDataSet: document.DocProcessScoringRiskSystemStageIntakeRequiredDataSet,
			MaxLtv:          document.DocProcessScoringRiskSystemStageIntakeMaxLtv,
			RejectReason:    document.DocProcessScoringRiskSystemStageIntakeRejectReason,
		},
	}
	Asset = DataSet{
		Name:     "check_risk_system_pg_asset",
		Required: []common.HString{document.DocProcessAssetCondition},
		Output: StageOutput{
			RequestId:       document.DocProcessScoringRiskSystemStageAssetRequestId,
			Status:          document.DocProcessScoringRiskSystemStageAssetStatus,
			RequiredDataSet: document.DocProcessScoringRiskSystemStageAssetRequiredDataSet,
			MaxLtv:          document.DocProcessScoringRiskSystemStageAssetMaxLtv,
			RejectReason:    document.DocProcessScoringRiskSystemStageAssetRejectReason,
		},
	}
	// Income serves both surveys: "normal" (where it is the final page) and
	// "high_risk" (where it is not) - income.verified_amount first appears
	// once either way.
	Income = DataSet{
		Name:     "check_risk_system_pg_income",
		Required: []common.HString{document.DocProcessIncomeVerifiedAmount},
		Output: StageOutput{
			RequestId:       document.DocProcessScoringRiskSystemStageIncomeRequestId,
			Status:          document.DocProcessScoringRiskSystemStageIncomeStatus,
			RequiredDataSet: document.DocProcessScoringRiskSystemStageIncomeRequiredDataSet,
			MaxLtv:          document.DocProcessScoringRiskSystemStageIncomeMaxLtv,
			RejectReason:    document.DocProcessScoringRiskSystemStageIncomeRejectReason,
		},
	}
	EnvironmentCheck = DataSet{
		Name:     "check_risk_system_pg_environment_check",
		Required: []common.HString{document.DocProcessEnvironmentCheckResult},
		Output: StageOutput{
			RequestId:       document.DocProcessScoringRiskSystemStageEnvironmentCheckRequestId,
			Status:          document.DocProcessScoringRiskSystemStageEnvironmentCheckStatus,
			RequiredDataSet: document.DocProcessScoringRiskSystemStageEnvironmentCheckRequiredDataSet,
			MaxLtv:          document.DocProcessScoringRiskSystemStageEnvironmentCheckMaxLtv,
			RejectReason:    document.DocProcessScoringRiskSystemStageEnvironmentCheckRejectReason,
		},
	}
	Underwriting = DataSet{
		Name:     "check_risk_system_pg_underwriting",
		Required: []common.HString{document.DocProcessUnderwritingConfirmed},
		Output: StageOutput{
			RequestId:       document.DocProcessScoringRiskSystemStageUnderwritingRequestId,
			Status:          document.DocProcessScoringRiskSystemStageUnderwritingStatus,
			RequiredDataSet: document.DocProcessScoringRiskSystemStageUnderwritingRequiredDataSet,
			MaxLtv:          document.DocProcessScoringRiskSystemStageUnderwritingMaxLtv,
			RejectReason:    document.DocProcessScoringRiskSystemStageUnderwritingRejectReason,
		},
	}
)

// All lists every stage from least to most advanced. The order matters: a
// stage sends every earlier stage's data, steps aside once a later stage has
// answered, and the aggregator takes the most advanced answer.
var All = []DataSet{Intake, Asset, Income, EnvironmentCheck, Underwriting}

// ActivityNames returns the Temporal activity type of every stage - what a
// caller delivering a verdict (cmd/testcli's "verdict") must look for among a
// workflow's pending activities. It deliberately excludes the
// check_risk_system_pg_verdict* aggregators, which share the name prefix and
// can themselves sit pending while they fail and retry (TC4).
func ActivityNames() []string {
	names := make([]string, 0, len(All))
	for _, ds := range All {
		names = append(names, ds.Name)
	}
	return names
}

// intakePreconditionPaths gate every stage, not only Intake: Risk System is
// never asked about a submission either intake check has rejected, even if a
// page field were injected by hand before the checks ran. $.status makes
// every stage wait for check_submission_pg, like every other non-exempt
// activity. None of these may also be a read: LockMap.Lock read-locks
// required precondition paths and reads separately, and a field in both is
// read-locked twice by the same step, which panics
// (ErrFieldLockAlreadyHeldById).
var intakePreconditionPaths = []common.HString{
	document.DocProcessAgeCheckPassed,
	document.DocProcessDuplicatePlateCheckPassed,
	document.DocStatus,
}

func stageIndex(ds DataSet) int {
	for i, s := range All {
		if s.Name == ds.Name {
			return i
		}
	}
	panic("checkrisksystem: unknown data set " + ds.Name)
}

// readSet requires the stage's own required fields and reads everything else
// the stage sends - its own optional fields plus every earlier data set's
// fields - as rollback-triggering optionals, so a change to any of them
// re-asks the stage.
func readSet(ds DataSet) *common.ReadSet {
	optional := make([]common.OptionalPath, 0)
	for _, earlier := range All[:stageIndex(ds)] {
		for _, field := range earlier.Fields() {
			optional = append(optional, rearmOnChange(field))
		}
	}
	for _, field := range ds.Optional {
		optional = append(optional, rearmOnChange(field))
	}
	return common.MakeReadSet(append([]common.HString{}, ds.Required...)).SetOptionals(optional, true)
}

func rearmOnChange(path common.HString) common.OptionalPath {
	return common.OptionalPath{Path: path, Strategy: common.OptionalWaitIfLocked, TriggerRollback: true}
}

// laterRequestIds are the request_id fields of every stage after ds - the
// presence of any one means a later stage has answered.
func laterRequestIds(ds DataSet) []common.HString {
	ids := make([]common.HString, 0)
	for _, later := range All[stageIndex(ds)+1:] {
		ids = append(ids, later.Output.RequestId)
	}
	return ids
}

func preconditionSet(ds DataSet) *common.PreConditionSet {
	return common.MakePreConditionSet(
		append([]common.HString{}, intakePreconditionPaths...),
		laterRequestIds(ds),
	)
}

// Constructor builds the step that asks Risk System for one stage.
type Constructor struct {
	DataSet DataSet
	f       *runtime.Function[any, any]
}

func (c *Constructor) GenerateFunction(
	_ func(url string) (*framework.APIFunction, error),
	docFieldCheck func([]common.HString),
	_ *framework.System,
	_ *defs.DocumentDescriptor,
) error {
	rs := readSet(c.DataSet)
	docFieldCheck(rs.Paths)
	for _, path := range rs.OptionalPaths {
		docFieldCheck([]common.HString{path.Path})
	}
	docFieldCheck(c.DataSet.Output.Paths())
	docFieldCheck(preconditionSet(c.DataSet).Paths)
	docFieldCheck(preconditionSet(c.DataSet).OptionalPaths)

	convIn := mapping.NewSimpleInputConverter[map[common.HString]any]()
	convIn.SetInput(rs, func(m map[common.HString]any) (*map[common.HString]any, error) { return &m, nil })
	convOut := mapping.NewSimpleOutputConverter[map[common.HString]any]()
	convOut.SetOutput(common.MakeWriteSet(c.DataSet.Output.Paths()), func(data *map[common.HString]any) (map[common.HString]any, error) { return *data, nil })
	conv := mapping.NewSimpleConverterFrom(convIn, convOut)

	// work runs synchronously the instant the activity is scheduled, but per
	// the async contract (runtime/function.go's Function.Execute) its return
	// value is discarded the moment the activity goes pending - only
	// asyncHandler's output, supplied later via a real Temporal completion,
	// ever reaches the document.
	work := func(_ context.Context, _ *map[common.HString]any) (*map[common.HString]any, error) {
		return &map[common.HString]any{}, nil
	}

	asyncHandler := recordAnswer(c.DataSet.Output)
	c.f = runtime.NewAnyFunction(c.DataSet.Name, &asyncHandler, conv, work, nil)
	return nil
}

// recordAnswer records what RS said, verbatim, onto the stage's own fields -
// it does not interpret it; checkrisksystemverdict does, because this handler
// (runtime.AsyncPayloadHandler) only ever receives the raw external payload,
// never current document state. Every field is written on every answer
// (max_ltv 0 and reject_reason "" when RS sends none), so after a stage's
// first answer each later one is an UPDATE of all of them - never a
// newly-set field, which would re-arm nothing downstream.
func recordAnswer(out StageOutput) runtime.AsyncPayloadHandler {
	return func(raw map[string]any) (map[common.HString]any, error) {
		status, _ := raw["status"].(string)
		requiredDataSet, _ := raw["required_data_set"].(string)
		maxLTV, _ := raw["max_ltv"].(float64)
		rejectReason, _ := raw["reject_reason"].(string)
		return map[common.HString]any{
			out.RequestId:       "playground-rs-" + uuid.New().String(),
			out.Status:          status,
			out.RequiredDataSet: requiredDataSet,
			out.MaxLtv:          maxLTV,
			out.RejectReason:    rejectReason,
		}, nil
	}
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	step := runtime.NewProcessStep(runtime.ProcessStepId(c.DataSet.Name), c.f, runtime.Normal, []runtime.ProcessStepId{})
	step.SetWriteIfEqual(runtime.None, nil)
	// Async activities stay pending until explicitly completed; the SDK
	// default startToCloseTimeout (2 minutes, runtime/process.go) would
	// otherwise time this out and retry it - re-minting a fresh async token
	// - long before a human/testcli ever gets to call "verdict". Matches
	// survey's own long timeout for the same reason.
	step.SetTimeout(30 * 24 * time.Hour)
	// A stage is re-queued by planner.Rollback whenever something it reads
	// changes. Without this, that rollback would revert the stage's
	// previously recorded answer back to unset, making its next answer a
	// newly-set write that re-arms nothing, and would cancel a pending call.
	step.SetRetainDataOnRollback()
	step.SetPrecondition(stageGate(c.DataSet), preconditionSet(c.DataSet))
	// Deliberately NOT SetNonDeterministic(): Rollback's walk is
	// reachability-based, so a stage can be re-queued even when nothing it
	// reads has changed. With an unchanged input it fast-forwards via
	// document.history.MatchInput to its own identical earlier answer instead
	// of opening a redundant RS call. A genuine change always carries a
	// changed input, which defeats history matching on its own.
	return step
}

// stageGate is a stage's precondition: the intake gate holds, and no later
// stage has answered yet - once one has, the aggregator uses that later
// answer, and that stage already re-asks on every change to this stage's
// data.
func stageGate(ds DataSet) func(workflow.Context, map[common.HString]any) bool {
	later := laterRequestIds(ds)
	return func(ctx workflow.Context, data map[common.HString]any) bool {
		if !intakeGate(ctx, data) {
			return false
		}
		for _, id := range later {
			if _, answered := data[id]; answered {
				return false
			}
		}
		return true
	}
}

// intakeGate keeps Risk System from ever being asked about a submission
// either intake check has rejected: both flags must be present AND true.
func intakeGate(_ workflow.Context, data map[common.HString]any) bool {
	ageOK, _ := data[document.DocProcessAgeCheckPassed].(bool)
	plateOK, _ := data[document.DocProcessDuplicatePlateCheckPassed].(bool)
	return ageOK && plateOK
}
