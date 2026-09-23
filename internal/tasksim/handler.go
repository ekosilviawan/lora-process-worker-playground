// Package tasksim is a minimal, playground-only task-master handler.
//
// Production task types (survey, review, mKYC, e-sign) are driven by
// lora-partnership-task-ndf's full task_execution.WorkflowExecutor, which
// syncs every task to a real Task Service (LTS) so a human can see and fill
// a form. This playground has no LTS, no form UI, and no per-task-type
// business logic to simulate that round trip - the only thing it needs from
// the task-master workflow is the one property CreateTaskFunction exists
// for: a task stays genuinely pending until an explicit external
// "task-completion" update arrives, never completing itself as a side
// effect of unrelated document field churn.
//
// handler implements taskdefs.TaskWorkflowHandler directly (bypassing
// task_execution.WorkflowExecutor and everything LTS-shaped it would try to
// call) so that no Task Service, schema-verification, NATS, or ArangoDB
// wiring is required to exercise the real Temporal signal path end to end.
package tasksim

import (
	"fmt"

	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	taskdefs "github.com/bfi-finance/lora-process-sdk/framework/task/defs"
	"go.temporal.io/sdk/workflow"
)

// PendingTaskQuery is a Temporal query registered on the task-master
// workflow, returning every task this handler is currently waiting on. It
// exists so an external caller simulating a human (cmd/testcli's
// complete-survey command) can discover a task's id without parsing raw
// Temporal history - it only needs the id, which round-trips back to this
// handler's own InboundTaskCompletionHandler.
const PendingTaskQuery = "pending-task"

// PendingTask is what PendingTaskQuery returns for one still-open task.
type PendingTask struct {
	Id   taskdefs.TaskId
	Type taskdefs.TaskType
}

type handler struct {
	ops taskdefs.TaskWorkflowOutboundOperations
	// pending remembers each open task's TemporalToken by task id, so
	// InboundTaskCompletionHandler can relay completion back to the exact
	// activity that's waiting on it. Workflow-local state is safe here: one
	// handler instance is constructed per task-master workflow execution,
	// and every access happens on that workflow's single logical thread.
	pending map[taskdefs.TaskId]*taskdefs.WorkerTaskDefinition
}

// NewHandler is a taskdefs.ConstructWithOperations - the constructor
// task.TaskWorkflow calls once per task-master workflow execution.
func NewHandler(_ workflow.CancelFunc, operations taskdefs.TaskWorkflowOutboundOperations, _ ...any) taskdefs.TaskWorkflowHandler {
	return &handler{
		ops:     operations,
		pending: map[taskdefs.TaskId]*taskdefs.WorkerTaskDefinition{},
	}
}

func (h *handler) TaskMasterCreationRequest(ctx workflow.Context, def *taskdefs.WorkerTaskMasterDefinition) error {
	err := workflow.SetQueryHandler(ctx, PendingTaskQuery, func() ([]PendingTask, error) {
		out := make([]PendingTask, 0, len(h.pending))
		for id, wtd := range h.pending {
			out = append(out, PendingTask{Id: id, Type: wtd.Type})
		}
		return out, nil
	})
	if err != nil {
		return err
	}
	workflow.GetLogger(ctx).Info("tasksim: task master ready", "document_id", def.DocumentId)
	return nil
}

func (h *handler) TaskCreationRequest(ctx workflow.Context, wtd *taskdefs.WorkerTaskDefinition) error {
	h.pending[wtd.Id] = wtd
	workflow.GetLogger(ctx).Info("tasksim: task created, waiting for an explicit task-completion update",
		"id", wtd.Id, "type", wtd.Type, "master_id", wtd.MasterId, "document_id", wtd.DocumentId)
	return nil
}

// InboundTaskDataHandler would relay an unsolicited data-forward to whatever
// task is currently open - this playground has no such flow, so it's a
// no-op rather than reimplementing form-partial-update semantics no
// activity here enables (EnableExternalDataForwarding is never called).
func (h *handler) InboundTaskDataHandler(_ workflow.Context, _ *defs.TaskDataForward) error {
	return nil
}

// InboundTaskCompletionHandler is the one method that matters: it's called
// when the "task-completion" update arrives (see cmd/testcli's
// complete-survey command), and relays it straight back to the pending
// activity on the document workflow via the captured Temporal token.
func (h *handler) InboundTaskCompletionHandler(
	ctx workflow.Context, tcd *taskdefs.ServiceTaskCompletionData,
) (*taskdefs.ServiceTaskCompletionSubmissionResponse, error) {
	wtd, ok := h.pending[tcd.Id]
	if !ok {
		return nil, fmt.Errorf("tasksim: task-completion for unknown or already-completed task id %q", tcd.Id)
	}
	delete(h.pending, tcd.Id)

	if err := h.ops.RelayTaskCompletionToDocumentActivity(ctx, &taskdefs.WorkerTaskCompletionData{
		Id:            tcd.Id,
		MasterId:      tcd.MasterId,
		Type:          tcd.Type,
		Action:        tcd.Action,
		Data:          tcd.Data,
		TemporalToken: wtd.TemporalToken,
	}); err != nil {
		return nil, err
	}
	workflow.GetLogger(ctx).Info("tasksim: task completed", "id", tcd.Id, "type", tcd.Type)
	return &taskdefs.ServiceTaskCompletionSubmissionResponse{}, nil
}

// TerminationNotification, TaskMasterClosureRequest: this playground never
// cancels/terminates an open task early, so these are legitimate no-ops -
// the same as production's own Termination stub for tasks that don't need
// early-termination handling.
func (h *handler) TerminationNotification(_ workflow.Context, _ *defs.TaskTerminationNotification) (*taskdefs.WorkerTaskCompletionData, error) {
	return nil, nil
}

func (h *handler) TaskMasterClosureRequest(_ workflow.Context) error {
	return nil
}
