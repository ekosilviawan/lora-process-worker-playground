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
	// ...) purely to reflect current progress, not as data those steps
	// themselves depend on. Without this, an unrelated step's status write
	// after check_risk_system_pg was scheduled retroactively marks
	// check_risk_system_pg "impacted" (status is one of its own
	// PreConditionSet fields, see checkrisksystem.impl.go's stageGate
	// precondition) and Planner.Rollback cancels and re-schedules it,
	// re-acquiring its write lock on survey_type and starving the SURVEY
	// task-creation step that's waiting to read it.
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
