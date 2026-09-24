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
	// impact it directly either way. What this neutrality marking actually
	// protects is checkeligibility, checkduplicateplate, and
	// calculateriskfunding - all three mandatorily read $.status - from being
	// marked "impacted" and rescheduled by Planner.Rollback every time any
	// other step (re)writes status to the same or a different value.
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
