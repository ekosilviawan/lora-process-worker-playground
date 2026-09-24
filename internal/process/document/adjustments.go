package document

import (
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/fp"
)

func AdjustAndMakeDocumentDescriptor(fields map[common.HString]*defs.FieldDescriptor[any]) *defs.DocumentDescriptor {
	// fields neutral to re-execution: status/status_reason are written by
	// almost every checkpoint (check_submission_pg, check_risk_system_pg,
	// checkeligibility, checkduplicateplate, ...) purely to reflect current
	// progress, not as data those steps themselves depend on.
	// Planner.Rollback (runtime/planner.go) propagates impact purely via each
	// step's mandatory ReadSet (ReadSet().RollbackTriggerPaths()) - it never
	// consults PreConditionSet, which only gates scheduling
	// (documentMatchRead), not rollback. check_risk_system_pg does not even
	// mandatorily read $.status, so a status write could never retroactively
	// impact it directly either way.
	//
	// What this neutrality marking actually buys: Rollback strips
	// re-exec-neutral fields from the *seed* set exactly once, at the very
	// top (planner.go's Rollback, before PriorWrites/findEarliestReaderAfter
	// run) - so a $.status write can never by itself *start* an impact walk.
	//
	// It does NOT make a step immune to $.status just by being neutral at
	// the source. Once some *other*, non-neutral field's write starts a walk
	// for an unrelated reason, impactedSteps' cumulative "written"
	// accumulator (planner.go's impactedSteps:
	// written.AddMany(...WriteSet().Paths)) folds in each impacted step's
	// *entire* WriteSet with no neutrality filtering at all - that filtering
	// only ever ran once, on the seed set. Any step that still carries
	// $.status in its own mandatory ReadSet remains reachable this way, as a
	// passenger, with no data of its own having actually changed.
	//
	// checkeligibility and checkduplicateplate no longer carry this risk at
	// all: neither ever reads $.status's value (see each impl.go's execFunc),
	// so both moved it out of ReadSet entirely into a PreConditionSet
	// presence gate (mirroring check_submission_pg's own shouldVerify) -
	// this was observed directly: the identity survey completing writes
	// $.customer.birth_date (correctly impactful - checkeligibility must
	// re-run the age check against the real birth date), and
	// checkeligibility's own $.status write used to re-enter "written"
	// unfiltered and drag checkduplicateplate along for a redundant re-run,
	// purely on $.status's coattails, with its own license_plate unchanged.
	// calculateriskfunding still has $.status in its mandatory ReadSet and
	// remains exposed to this same transitive path - a candidate for the
	// same treatment if its own $.status read turns out to be gate-only too.
	fields[DocStatus].SetReExecNeutral(true)
	fields[DocStatusReason].SetReExecNeutral(true)

	doc := defs.NewDocumentDescriptorFromFields(fields)
	doc.SetTermination(termination())
	return doc
}

// termination signals the SDK to stop workflow execution when status reaches a terminal value.
func termination() defs.DocumentPredicateFunction {
	conv := mapping.NewSimpleInputConverter[bool]()
	readSet := []common.HString{DocStatus}
	conv.SetInput(common.MakeReadSet(readSet), func(m map[common.HString]any) (*bool, error) {
		terminal := IsTerminalStatus(fp.As[string](m[DocStatus]))
		return &terminal, nil
	})
	return conv
}
