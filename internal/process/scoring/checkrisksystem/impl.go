// Package checkrisksystem is option 3 in RISK_SYSTEM_RETRIGGER_OPTIONS.md: one
// process step per data set (initial, asset, income, environment_check,
// underwriting) that asks Risk System (RS). There is no re-trigger counter -
// RS is asked because data appeared or changed, not because a cursor moved:
//
//   - A step first runs when its own data-set field first appears: that field
//     is a required read, and a newly set field makes the step runnable.
//
//   - It is re-triggered when any field already collected by its stage changes:
//     the submission's fields and every data-set field up to and including its
//     own are required reads (readSet), and a required read is always a
//     rollback trigger (common.ReadSet.RollbackTriggerPaths), so an update to
//     one re-queues the step through planner.Rollback.
//
//   - Only the most advanced stage that has answered is re-triggered. A change
//     re-queues every stage that reads it (planner.go's impactedSteps walks
//     forward from the earliest reader), so each stage records itself in
//     risk_system.most_advanced_stage with its answer, and an earlier stage's
//     precondition (notSuperseded) makes it step aside once a later one has
//     answered. The most advanced stage already sends every earlier stage's
//     data, so each change asks RS exactly once.
//
// All's order is therefore load-bearing: a stage requires every earlier
// stage's field, so it only runs on a path that has collected all of them,
// and its position ranks it for stepping aside.
//
// Why a replay can't restore a stale verdict onto the shared risk_system.*
// fields: the only stage that can re-run is the one that recorded the current
// verdict. If its input is unchanged it fast-forwards through history onto
// its own answer for that input. (So a value changed and then changed back
// reuses RS's earlier answer for it - correct only if RS answers the same
// input the same way.) A re-submission with identical values is not an
// update, so it asks nothing.
//
// Two more properties fall out of the SDK's locking rather than any code
// here:
//   - The steps are serialized: they all write-lock the same risk_system.*
//     fields, so at most one is ever pending.
//   - A pending step read-locks its page field, which is in survey's
//     writeSet, so FieldLock.CanSetWrite keeps survey from opening its next
//     page until a verdict lands. After a page's partial completion this
//     step (runtime.Normal) always wins the scheduling round over survey
//     (runtime.Lazy).
package checkrisksystem

import (
	"context"
	"slices"
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

// DataSet is one reason to ask Risk System: the activity name of the step
// that asks, and the document fields whose presence means that data set has
// been collected.
type DataSet struct {
	Name   string
	Fields []common.HString
}

var (
	// Initial is the first call, before any survey runs: it waits only for the
	// customer identity and product the initial submission carries (plus the
	// initial gate every data set shares, see initialPreconditionPaths).
	Initial = DataSet{
		Name: "check_risk_system_pg",
		Fields: []common.HString{
			document.DocId,
			document.DocCustomerNik,
			document.DocCustomerName,
			document.DocProcessLoanStructureProductType,
		},
	}
	Asset = DataSet{
		Name:   "check_risk_system_pg_asset",
		Fields: []common.HString{document.DocProcessAssetCondition},
	}
	// Income serves both surveys: "normal" (where it is the final page) and
	// "high_risk" (where it is not) - income.verified_amount first appears
	// once either way.
	Income = DataSet{
		Name:   "check_risk_system_pg_income",
		Fields: []common.HString{document.DocProcessIncomeVerifiedAmount},
	}
	EnvironmentCheck = DataSet{
		Name:   "check_risk_system_pg_environment_check",
		Fields: []common.HString{document.DocProcessEnvironmentCheckResult},
	}
	Underwriting = DataSet{
		Name:   "check_risk_system_pg_underwriting",
		Fields: []common.HString{document.DocProcessUnderwritingConfirmed},
	}
)

// All lists every data set that asks Risk System, in the order every survey
// path collects them. The order matters: each stage requires, and is
// re-triggered by, the fields of every stage before it (requiredFields).
var All = []DataSet{Initial, Asset, Income, EnvironmentCheck, Underwriting}

// ActivityNames returns the Temporal activity type of every step that asks
// Risk System - what a caller delivering a verdict (cmd/testcli's "verdict")
// must look for among a workflow's pending activities. It deliberately
// excludes check_risk_system_pg_verdict, which shares the name prefix and can
// itself sit pending while it fails and retries (TC4).
func ActivityNames() []string {
	names := make([]string, 0, len(All))
	for _, ds := range All {
		names = append(names, ds.Name)
	}
	return names
}

// initialPreconditionPaths gate every data set, not only Initial: Risk System
// is never asked about a submission either initial check has rejected, even if
// a page field were injected by hand before the checks ran. $.status makes
// every step wait for check_submission_pg, like every other non-exempt
// activity.
var initialPreconditionPaths = []common.HString{
	document.DocProcessAgeCheckPassed,
	document.DocProcessDuplicatePlateCheckPassed,
	document.DocStatus,
}

// contextReadSet is everything LPW's (simulated) call to RS includes. readSet
// splits it per stage into required, rollback-triggering reads and
// non-triggering optional reads, so each call sends whatever the document
// holds at that moment. None of them may be a REQUIRED precondition path:
// LockMap.Lock read-locks required precondition paths and optional reads
// separately, and a field in both is read-locked twice by the same step, which
// panics (ErrFieldLockAlreadyHeldById).
//
// product_type is sent alongside the customer identity fields - RS's own
// required-data-set decision is made with full knowledge of the loan's
// product, so a correctly-behaving RS should never request underwriting-v1
// for an NDF2W applicant in the first place. survey.IsUnderwritingEligible
// (enforced in checkrisksystemverdict) is the backstop for when it does
// anyway.
var contextReadSet = []common.OptionalPath{
	sendToRiskSystem(document.DocId),
	sendToRiskSystem(document.DocCustomerNik),
	sendToRiskSystem(document.DocCustomerName),
	sendToRiskSystem(document.DocCustomerBirthDate),
	sendToRiskSystem(document.DocProcessLoanStructureProductType),
	sendToRiskSystem(document.DocProcessAssetCondition),
	sendToRiskSystem(document.DocProcessLoanStructureProvisionalAmount),
	sendToRiskSystem(document.DocProcessLoanStructureLtvSubmission),
	sendToRiskSystem(document.DocProcessIncomeVerifiedAmount),
	sendToRiskSystem(document.DocProcessUnderwritingConfirmed),
	sendToRiskSystem(document.DocProcessEnvironmentCheckResult),
}

// writeSet is just "calling RS" 's own output: RS's raw answer, recorded
// verbatim onto its own $.process.scoring.risk_system.* fields. Every data
// set writes the same fields, and the steps are serialized, so the latest
// write is the latest call. most_advanced_stage is written alongside the
// answer: the stage's own name, which earlier stages step aside for (see
// notSuperseded). This activity does NOT
// interpret the verdict (map required_data_set to survey_type, translate
// status, or decide underwriting eligibility) - that happens in
// checkrisksystemverdict.check_risk_system_pg_verdict, an ordinary
// synchronous activity that reads these fields back. It has to live there:
// this activity's asyncHandler below only ever receives the raw external
// payload (runtime.AsyncPayloadHandler), never current document state, so it
// structurally cannot validate the verdict against the document (e.g.
// checking environment_check/product_type for the underwriting gate) - see
// checkrisksystemverdict's package comment for the full reasoning.
var writeSet = []common.HString{
	document.DocProcessScoringRiskSystemRequestId,
	document.DocProcessScoringRiskSystemStatus,
	document.DocProcessScoringRiskSystemRequiredDataSet,
	document.DocProcessScoringRiskSystemMaxLtv,
	document.DocProcessScoringRiskSystemRejectReason,
	document.DocProcessScoringRiskSystemMostAdvancedStage,
}

func sendToRiskSystem(path common.HString) common.OptionalPath {
	return common.OptionalPath{Path: path, Strategy: common.OptionalWaitIfLocked}
}

// readSet is option 3's trigger rule: every field that already exists when a
// stage runs is a REQUIRED read - so it gates the stage and is a rollback
// trigger - and only the fields of later stages, which can't exist yet, stay
// non-triggering optional reads. A field is never both.
//
// "Already exists" means: the submission's fields (submissionFields), plus the
// data-set fields of this stage and of every stage before it in All.
func readSet(ds DataSet) *common.ReadSet {
	required := requiredFields(ds)
	optional := make([]common.OptionalPath, 0, len(contextReadSet))
	for _, path := range contextReadSet {
		if !slices.Contains(required, path.Path) {
			optional = append(optional, path)
		}
	}
	return common.MakeReadSet(required).SetOptionals(optional, true)
}

// submissionFields are sent to RS and always present from the initial
// submission, beyond Initial's own fields: birth_date (the age check that the
// initial gate requires reads it) and the requested loan structure (which
// calculate_risk_funding_pg already requires).
var submissionFields = []common.HString{
	document.DocCustomerBirthDate,
	document.DocProcessLoanStructureProvisionalAmount,
	document.DocProcessLoanStructureLtvSubmission,
}

// requiredFields returns submissionFields plus the data-set fields of ds and
// of every data set before it in All.
func requiredFields(ds DataSet) []common.HString {
	required := append([]common.HString{}, submissionFields...)
	for _, earlier := range All {
		for _, f := range earlier.Fields {
			if !slices.Contains(required, f) {
				required = append(required, f)
			}
		}
		if earlier.Name == ds.Name {
			break
		}
	}
	return required
}

// preconditionSet requires the initial gate's fields (the data-set fields are
// required reads, which already guarantees they are present) and reads
// most_advanced_stage for notSuperseded. That is an optional path, since it
// is unset until the first answer, and never a read: precondition paths are
// never rollback triggers, so a new stage recording itself re-queues nothing.
func preconditionSet(_ DataSet) *common.PreConditionSet {
	return common.MakePreConditionSet(
		append([]common.HString{}, initialPreconditionPaths...),
		[]common.HString{document.DocProcessScoringRiskSystemMostAdvancedStage},
	)
}

// Constructor builds the step that asks Risk System for one data set.
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
	docFieldCheck(writeSet)
	docFieldCheck(preconditionSet(c.DataSet).Paths)
	docFieldCheck(preconditionSet(c.DataSet).OptionalPaths)
	for _, path := range contextReadSet {
		docFieldCheck([]common.HString{path.Path})
	}

	convIn := mapping.NewSimpleInputConverter[map[common.HString]any]()
	convIn.SetInput(readSet(c.DataSet), func(m map[common.HString]any) (*map[common.HString]any, error) { return &m, nil })
	convOut := mapping.NewSimpleOutputConverter[map[common.HString]any]()
	convOut.SetOutput(common.MakeWriteSet(writeSet), func(data *map[common.HString]any) (map[common.HString]any, error) { return *data, nil })
	conv := mapping.NewSimpleConverterFrom(convIn, convOut)

	// work runs synchronously the instant the activity is scheduled, but per
	// the async contract (runtime/function.go's Function.Execute) its return
	// value is discarded the moment the activity goes pending - only
	// asyncHandler's output, supplied later via a real Temporal completion,
	// ever reaches the document.
	work := func(_ context.Context, _ *map[common.HString]any) (*map[common.HString]any, error) {
		return &map[common.HString]any{}, nil
	}

	// asyncHandler just records what RS said, verbatim - it does not
	// interpret it (see writeSet's comment above for why that's not this
	// activity's job).
	var asyncHandler runtime.AsyncPayloadHandler = func(raw map[string]any) (map[common.HString]any, error) {
		status, _ := raw["status"].(string)
		requiredDataSet, _ := raw["required_data_set"].(string)
		maxLTV, _ := raw["max_ltv"].(float64)
		rejectReason, _ := raw["reject_reason"].(string)

		out := map[common.HString]any{
			document.DocProcessScoringRiskSystemRequestId:         "playground-rs-" + uuid.New().String(),
			document.DocProcessScoringRiskSystemStatus:            status,
			document.DocProcessScoringRiskSystemRequiredDataSet:   requiredDataSet,
			document.DocProcessScoringRiskSystemMostAdvancedStage: c.DataSet.Name,
		}
		if maxLTV > 0 {
			out[document.DocProcessScoringRiskSystemMaxLtv] = maxLTV
		}
		if rejectReason != "" {
			out[document.DocProcessScoringRiskSystemRejectReason] = rejectReason
		}
		return out, nil
	}

	c.f = runtime.NewAnyFunction(c.DataSet.Name, &asyncHandler, conv, work, nil)
	return nil
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
	// A re-triggered step keeps the verdict it recorded until the new call
	// answers: a rollback doesn't revert risk_system.* to an older value or
	// reset it to unset.
	step.SetRetainDataOnRollback()
	step.SetPrecondition(precondition(c.DataSet), preconditionSet(c.DataSet))
	return step
}

// precondition is a stage's gate beyond its required reads: the initial gate
// holds and no later stage has answered yet.
func precondition(ds DataSet) func(workflow.Context, map[common.HString]any) bool {
	return func(ctx workflow.Context, data map[common.HString]any) bool {
		return initialGate(ctx, data) && notSuperseded(ds, data)
	}
}

// notSuperseded makes a stage step aside once a later stage in All has
// answered: most_advanced_stage is unset (nobody has answered) or names this
// stage or an earlier one. It only ever moves forward - a stage runs only
// when it is at least as advanced as the recorded one, and records itself.
// A name All doesn't know (e.g. written by a newer worker) supersedes
// nothing: an extra RS call sends current data, whereas stepping aside could
// leave nobody to ask.
func notSuperseded(ds DataSet, data map[common.HString]any) bool {
	latest, _ := data[document.DocProcessScoringRiskSystemMostAdvancedStage].(string)
	latestRank := slices.IndexFunc(All, func(d DataSet) bool { return d.Name == latest })
	return latestRank <= stageRank(ds)
}

// stageRank is ds's position in All.
func stageRank(ds DataSet) int {
	return slices.IndexFunc(All, func(d DataSet) bool { return d.Name == ds.Name })
}

// initialGate keeps Risk System from ever being asked about a submission
// either initial check has rejected: both flags must be present AND true.
func initialGate(_ workflow.Context, data map[common.HString]any) bool {
	ageOK, _ := data[document.DocProcessAgeCheckPassed].(bool)
	plateOK, _ := data[document.DocProcessDuplicatePlateCheckPassed].(bool)
	return ageOK && plateOK
}
