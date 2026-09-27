// Package checkrisksystem asks Risk System (RS) once per data set: one
// process step per data set (intake, asset, income, environment_check,
// underwriting), each gated on that data set's own field first appearing in
// the document. There is no re-ask counter - RS is asked because data
// appeared, not because a cursor moved (option 3 in
// RISK_SYSTEM_RETRIGGER_OPTIONS.md).
//
// The one rule that makes this converge: no step here has a single rollback
// trigger path. A step's data-set field sits in its PRECONDITION (as an
// optional precondition path, which dataSetGate then requires to be present),
// which the planner read-locks (lock_map.go's Lock) but never treats as a
// rollback trigger (common.ReadSet.RollbackTriggerPaths only covers required
// reads and TriggerRollback optionals), and everything RS is sent is read as
// a non-triggering optional. So planner.Rollback's reachability walk
// (planner.go's impactedSteps) can never pull one of these steps back in:
// each runs exactly once, when its field first appears, and so never
// fast-forwards through document history onto a verdict older than the one
// another step has since recorded. That is the fragility the options doc
// warned about for several steps writing the same risk_system.* fields.
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
//
// The costs, accepted deliberately: pages that only revise data RS already
// saw at intake (identity: customer.name/birth_date; financing:
// provisional_amount/ltv_submission) have no first-time field and so do not
// re-ask RS, and neither does re-submitting a page with a changed value.
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

// DataSet is one reason to ask Risk System: the activity name of the step
// that asks, and the document fields whose presence means that data set has
// been collected.
type DataSet struct {
	Name   string
	Fields []common.HString
}

var (
	// Intake is the first call, before any survey runs: it waits only for the
	// customer identity and product the initial submission carries (plus the
	// intake gate every data set shares, see intakePreconditionPaths).
	Intake = DataSet{
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

// All lists every data set that asks Risk System, in the order a document
// normally meets them.
var All = []DataSet{Intake, Asset, Income, EnvironmentCheck, Underwriting}

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

// intakePreconditionPaths gate every data set, not only Intake: Risk System
// is never asked about a submission either intake check has rejected, even if
// a page field were injected by hand before the checks ran. $.status makes
// every step wait for check_submission_pg, like every other non-exempt
// activity.
var intakePreconditionPaths = []common.HString{
	document.DocProcessAgeCheckPassed,
	document.DocProcessDuplicatePlateCheckPassed,
	document.DocStatus,
}

// contextReadSet is everything LPW's (simulated) call to RS includes, read as
// non-triggering optionals so each call still sends whatever the document
// holds at that moment. None of them may be a rollback trigger (see the
// package comment) - including the data-set fields themselves, which is why
// they gate through the precondition instead. None of them may be a REQUIRED
// precondition path either: LockMap.Lock read-locks required precondition
// paths and optional reads separately, and a field in both is read-locked
// twice by the same step, which panics (ErrFieldLockAlreadyHeldById). Optional
// precondition paths are de-duplicated against optional reads, which is why
// the data-set fields are optional precondition paths.
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
// set writes the same fields, so the latest write is always the latest call
// (the steps are serialized and never replay). This activity does NOT
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
}

func sendToRiskSystem(path common.HString) common.OptionalPath {
	return common.OptionalPath{Path: path, Strategy: common.OptionalWaitIfLocked}
}

// readSet has no required paths at all: a required read is always a rollback
// trigger.
func readSet() *common.ReadSet {
	return common.MakeReadSet([]common.HString{}).SetOptionals(contextReadSet, true)
}

// preconditionSet requires the intake gate's fields and lists the data-set
// fields as optional paths - dataSetGate is what makes them mandatory (see
// contextReadSet for why they can't be required paths).
func preconditionSet(ds DataSet) *common.PreConditionSet {
	return common.MakePreConditionSet(
		append([]common.HString{}, intakePreconditionPaths...),
		append([]common.HString{}, ds.Fields...),
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
	convIn.SetInput(readSet(), func(m map[common.HString]any) (*map[common.HString]any, error) { return &m, nil })
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
			document.DocProcessScoringRiskSystemRequestId:       "playground-rs-" + uuid.New().String(),
			document.DocProcessScoringRiskSystemStatus:          status,
			document.DocProcessScoringRiskSystemRequiredDataSet: requiredDataSet,
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
	// With no rollback trigger paths this step is never "impacted", so
	// neither this nor history matching should ever come into play. It stays
	// as a guard: if a trigger path were ever added, a rollback would neither
	// revert an already-recorded verdict nor cancel a pending RS call.
	step.SetRetainDataOnRollback()
	step.SetPrecondition(dataSetGate(c.DataSet), preconditionSet(c.DataSet))
	return step
}

// dataSetGate is a data set's precondition: the intake gate holds and every
// one of the data set's fields is present - i.e. that data has been
// collected.
func dataSetGate(ds DataSet) func(workflow.Context, map[common.HString]any) bool {
	return func(ctx workflow.Context, data map[common.HString]any) bool {
		if !intakeGate(ctx, data) {
			return false
		}
		for _, field := range ds.Fields {
			if _, ok := data[field]; !ok {
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
