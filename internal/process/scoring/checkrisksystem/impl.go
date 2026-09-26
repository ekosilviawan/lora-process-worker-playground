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

const ProcessAndActivityName = "check_risk_system_pg"

// requiredReadSet is what LPW's (simulated) call to RS includes, plus the two
// intake checks' own verdicts and the re-ask counter. Every entry here
// doubles as a rollback trigger - including the intake flags, whose VALUES
// (not just presence) intakeGate below also checks, to enforce "Risk System
// is never asked about a submission either check has rejected."
//
// trigger_seq is the one field whose only job is to re-ask RS. Every survey
// page bumps it (survey.SurveyPageCompletion), which is a genuine UPDATE -
// the only kind of write the SDK re-arms an already-run step for
// (planner.Rollback ignores newly-set fields). Without it, a page that only
// sets data for the first time (asset, income, environment_check,
// underwriting) would never re-ask RS; see RISK_SYSTEM_RETRIGGER_OPTIONS.md
// for why a counter was chosen over the alternatives.
// seed_scoring_checkpoint_pg seeds it to 0 once intake passes, so page 1's
// write is already an update, and this step's first call waits for that
// seed. It also carries the mutual exclusion: it's in survey's writeSet too,
// and while this step is pending (waiting on RS) it holds a read lock on it,
// so FieldLock.CanSetWrite keeps survey from locking - and so from opening
// the next page - until a verdict lands.
//
// product_type is sent alongside the customer identity fields - RS's own
// required-data-set decision is made with full knowledge of the loan's
// product, so a correctly-behaving RS should never request underwriting-v1
// for an NDF2W applicant in the first place. survey.IsUnderwritingEligible
// (enforced in checkrisksystemverdict) is the backstop for when it does
// anyway.
var requiredReadSet = []common.HString{
	document.DocId,
	document.DocCustomerNik,
	document.DocCustomerName,
	document.DocProcessLoanStructureProductType,
	document.DocProcessScoringTriggerSeq,
	document.DocProcessAgeCheckPassed,
	document.DocProcessDuplicatePlateCheckPassed,
}

// optionalReadSet is the rest of the data RS reviews. Every entry is also a
// rollback trigger (rearmOnChange), so a later CHANGE to data RS already saw
// (e.g. a surveyor revising a submitted value) re-asks RS on its own, not
// only through trigger_seq. A value set for the FIRST time never does that
// by itself - the SDK only re-queues an already-run step through
// planner.Rollback, which only considers updated fields, never newly-set ones
// (runtime/workflow.go's completion handling, versioned_value.go's Set) -
// which is exactly why the page's trigger_seq bump is needed.
// data-set/data-forward updates never re-arm this step at all - only
// activity/task completions do.
var optionalReadSet = []common.OptionalPath{
	rearmOnChange(document.DocCustomerBirthDate),
	rearmOnChange(document.DocProcessAssetCondition),
	rearmOnChange(document.DocProcessLoanStructureProvisionalAmount),
	rearmOnChange(document.DocProcessLoanStructureLtvSubmission),
	rearmOnChange(document.DocProcessIncomeVerifiedAmount),
	rearmOnChange(document.DocProcessUnderwritingConfirmed),
	rearmOnChange(document.DocProcessEnvironmentCheckResult),
}

// writeSet is just "calling RS" 's own output: RS's raw answer, recorded
// verbatim onto its own $.process.scoring.risk_system.* fields. This
// activity does NOT interpret the verdict (map required_data_set to
// survey_type, translate status, or decide underwriting eligibility) - that
// interpretation happens in checkrisksystemverdict.check_risk_system_pg_verdict,
// an ordinary synchronous activity that reads these fields back. It has to
// live there: this activity's asyncHandler below only ever receives the raw
// external payload (runtime.AsyncPayloadHandler), never current document
// state, so it structurally cannot validate the verdict against the
// document (e.g. checking environment_check/product_type for the
// underwriting gate) - see checkrisksystemverdict's package comment for the full reasoning.
var writeSet = []common.HString{
	document.DocProcessScoringRiskSystemRequestId,
	document.DocProcessScoringRiskSystemStatus,
	document.DocProcessScoringRiskSystemRequiredDataSet,
	document.DocProcessScoringRiskSystemMaxLtv,
	document.DocProcessScoringRiskSystemRejectReason,
}

func rearmOnChange(path common.HString) common.OptionalPath {
	return common.OptionalPath{Path: path, Strategy: common.OptionalWaitIfLocked, TriggerRollback: true}
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

	readSet := common.MakeReadSet(requiredReadSet).SetOptionals(optionalReadSet, true)
	convIn := mapping.NewSimpleInputConverter[map[common.HString]any]()
	convIn.SetInput(readSet, func(m map[common.HString]any) (*map[common.HString]any, error) { return &m, nil })
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

	c.f = runtime.NewAnyFunction(ProcessAndActivityName, &asyncHandler, conv, work, nil)
	return nil
}

func (c *Constructor) GenerateProcessStep() *runtime.ProcessStep {
	step := runtime.NewProcessStep(ProcessAndActivityName, c.f, runtime.Normal, []runtime.ProcessStepId{})
	step.SetWriteIfEqual(runtime.None, nil)
	// Async activities stay pending until explicitly completed; the SDK
	// default startToCloseTimeout (2 minutes, runtime/process.go) would
	// otherwise time this out and retry it - re-minting a fresh async token
	// - long before a human/testcli ever gets to call "verdict". Matches
	// survey's own long timeout for the same reason.
	step.SetTimeout(30 * 24 * time.Hour)
	// This step is reused for every re-ask, so a later data change (e.g. the
	// financing page revising ltv_submission) makes planner.Rollback treat it
	// as "impacted" again - and without this, Rollback would revert every
	// field this step already wrote for the PREVIOUS verdict (the raw
	// risk_system.request_id/status/required_data_set/max_ltv fields) back to
	// unset, purely as a side effect of being re-armed. That would in turn
	// make checkrisksystemverdict see those fields as newly-set rather than
	// updated on the NEXT verdict, and silently breaks calculateriskfunding's
	// optional ltv_max read the same way (see checkrisksystemverdict's own
	// SetRetainDataOnRollback for the continuation of this chain). Matches
	// survey's own SetRetainDataOnRollback (tasking/survey/impl.go) for the
	// identical reason.
	step.SetRetainDataOnRollback()
	step.SetPrecondition(intakeGate, common.MakePreConditionSet(
		[]common.HString{
			document.DocProcessAgeCheckPassed,
			document.DocProcessDuplicatePlateCheckPassed,
			document.DocStatus,
		},
		nil,
	))
	// Deliberately NOT SetNonDeterministic(): planner.Rollback's walk is
	// reachability-based, not value-diff-based (planner.go's impactedSteps),
	// so this step can be re-marked "impacted" even when nothing it reads has
	// actually changed - e.g. a verdict updating ltv_max re-impacts survey,
	// whose writeSet overlaps this step's reads. With an unchanged readset,
	// that re-arm fast-forwards via document.history.MatchInput (workflow.go's
	// doActivityExec) straight back to the identical verdict already given,
	// instead of opening a second real pending activity. A genuine re-ask of
	// Risk System always carries a changed input - a new trigger_seq at
	// minimum - which naturally defeats history-matching on its own.
	return step
}

// intakeGate keeps Risk System from ever being asked about a submission
// either intake check has rejected: both flags must be present AND true.
// There is no per-stage gate - whatever data RS sees on a given call is
// simply whatever the document holds when a page (or a data change) re-asks
// it.
func intakeGate(_ workflow.Context, data map[common.HString]any) bool {
	ageOK, _ := data[document.DocProcessAgeCheckPassed].(bool)
	plateOK, _ := data[document.DocProcessDuplicatePlateCheckPassed].(bool)
	return ageOK && plateOK
}
