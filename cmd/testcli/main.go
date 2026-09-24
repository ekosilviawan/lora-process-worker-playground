// Command testcli drives the Risk System POC by hand (start/inject/verdict)
// or by replaying one of the scenarios documented in RISK_SYSTEM_POC.md end
// to end. It talks to the worker's Temporal server the same way the worker
// itself does (same .env), never to the worker process directly.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
	taskdefs "github.com/bfi-finance/lora-process-sdk/framework/task/defs"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"lora-process-worker-playground/internal/config"
	"lora-process-worker-playground/internal/process/scoring/checkrisksystem"
	"lora-process-worker-playground/internal/process/tasking/survey"
	"lora-process-worker-playground/internal/process/workflow"
	"lora-process-worker-playground/internal/tasksim"
)

// workflowType and taskQueue are both the document name framework.System
// derives from the schema file name (system.go:174-178, 313-315) - not the
// "task_"-prefixed value the config also derives, which backs an unrelated
// internal client.
const (
	workflowType = "lpw-playground-v0_1_1"
	taskQueue    = "lpw-playground-v0_1_1"
)

// namespace is set once in main() from dial()'s result - CompleteActivityByID
// needs it, and it's called from deep inside scenario steps that only carry
// (ctx, c, id).
var namespace string

func main() {
	_ = godotenv.Load()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	c, ns, err := dial(config.LoadFromEnv())
	must(err)
	namespace = ns
	defer c.Close()

	ctx := context.Background()
	var runErr error
	switch os.Args[1] {
	case "start":
		runErr = cmdStart(ctx, c, os.Args[2:])
	case "inject":
		runErr = cmdInject(ctx, c, os.Args[2:])
	case "override":
		runErr = cmdOverride(ctx, c, os.Args[2:])
	case "verdict":
		runErr = cmdVerdict(ctx, c, os.Args[2:])
	case "tc":
		runErr = cmdScenario(ctx, c, os.Args[2:])
	case "describe":
		runErr = cmdDescribe(ctx, c, os.Args[2:])
	case "complete-survey":
		runErr = cmdCompleteSurvey(ctx, c, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	must(runErr)
}

func usage() {
	fmt.Fprint(os.Stderr, `testcli drives the Risk System POC.

Usage:
  testcli start    [-id <workflow-id>]
  testcli inject   -id <workflow-id> [-name ..] [-nik ..] [-birth-date ..] [-license-plate ..]
                   [-provisional-amount ..] [-ltv-submission ..] [-field path=value ...] [-bare]
  testcli override -id <workflow-id> -field path=value [-field path=value ...]
  testcli verdict  -id <workflow-id> -dataset <CUSTOMER_VERIFICATION|ASSET_REVIEW|FINANCING|INCOME_REVIEW|FINAL_REVIEW>
                   [-status pending|approved|rejected] [-max-ltv 0] [-reject-reason ..]
  testcli complete-survey -id <workflow-id> -type <identity|asset|financing|income|final_review>
                   [-outcome partial|final] [-seq <n>] [-field path=value ...]
  testcli tc <tc1|tc2|tc3|tc4|tc5|tc6|tc7> [-id <workflow-id>] [-wait 2s]
  testcli describe -id <workflow-id>

"start" and "tc" begin a brand-new workflow, so -id is optional there and
defaults to a generated "poc-<uuid>" id when omitted; every other subcommand
addresses a workflow that must already exist, so -id stays required.

"inject" sets previously-unset fields via the SDK's built-in "data-set" update
- the same one LGS calls in production for a fresh submission, including the
customer's originally requested provisional_amount/ltv_submission (DP input,
same as production's pre_scoring source - see calculateriskfunding); the
financing survey can still revise these later, same as production's surveyor
negotiation path. "override" overwrites fields that are already set, via this
worker's own "data-forward-override" handler; "data-set" refuses that outright.

"verdict" delivers a Risk System verdict by completing the pending
check_risk_system_pg activity directly (client.CompleteActivityByID):
check_risk_system_pg is a genuine async Temporal activity, so this fails
outright if Risk System was never actually asked about the current stage
(no pending activity to complete).

"complete-survey" simulates a human submitting a page of the currently-open
SURVEY task: survey is a genuine signal-gated Temporal task (system.
CreateTaskFunction), so nothing else can complete it. A real survey is
multiple form pages - -outcome partial submits one page and leaves the task
open (the framework mints a fresh task id for the next page); -outcome final
(default) submits the last page and closes it. -seq is the trigger_seq value
to write on the final page only - a real task completion is a self-contained
payload with no read access back into the document, so the caller (you, or
"tc" below) is the one source of truth for "how many surveys has this
document completed so far" (0 after seeding, so the Nth completion writes
seq=N). -field path=value (repeatable) supplies that page's own data for
-outcome partial, or overrides the canned outcome for -outcome final.

"tc" starts the workflow itself and replays every step of the named scenario
from RISK_SYSTEM_POC.md in order, pausing after each one until the worker's
pending activities settle (or -wait elapses).
`)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func dial(cfg *config.Env) (client.Client, string, error) {
	ns := cfg.TemporalNamespace
	if ns == "" {
		ns = "default"
	}
	opts := client.Options{
		HostPort:  cfg.TemporalHost + ":" + strconv.Itoa(cfg.TemporalPort),
		Namespace: ns,
	}
	if cfg.TemporalAPIKey != "" {
		opts.Credentials = client.NewAPIKeyStaticCredentials(cfg.TemporalAPIKey)
		opts.ConnectionOptions = client.ConnectionOptions{TLS: &tls.Config{MinVersion: tls.VersionTLS13}}
	}
	c, err := client.Dial(opts)
	return c, ns, err
}

// defaultWorkflowID generates a workflow id for "start" and "tc" when -id is
// omitted - both begin a brand-new workflow execution, so unlike every other
// subcommand (which addresses an id that must already exist) there's nothing
// for the caller to have to invent up front.
func defaultWorkflowID() string {
	return "poc-" + uuid.New().String()
}

// ---- manual subcommands ----

func cmdStart(ctx context.Context, c client.Client, args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	id := fs.String("id", "", "workflow id (= document id); generated if omitted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		*id = defaultWorkflowID()
	}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: *id, TaskQueue: taskQueue}, workflowType)
	if err != nil {
		return err
	}
	fmt.Printf("started %s (run %s)\n", *id, run.GetRunID())
	return nil
}

func cmdInject(ctx context.Context, c client.Client, args []string) error {
	fs := flag.NewFlagSet("inject", flag.ExitOnError)
	id := fs.String("id", "", "workflow id (= document id)")
	name := fs.String("name", "John Placeholder", "customer.name")
	nik := fs.String("nik", "3201010101010001", "customer.nik")
	birthDate := fs.String("birth-date", "1990-05-20", "customer.birth_date")
	licensePlate := fs.String("license-plate", "B5678ABC", "process.asset.license_plate")
	provisionalAmount := fs.Float64("provisional-amount", 90000, "process.loan_structure.provisional_amount (DP's originally requested amount)")
	ltvSubmission := fs.Float64("ltv-submission", 0.75, "process.loan_structure.ltv_submission (DP's originally requested LTV)")
	bare := fs.Bool("bare", false, "skip the default identity fields, send only -field overrides")
	var extra fieldFlags
	fs.Var(&extra, "field", "additional raw field override, path=value (repeatable), e.g. $.process.loan_structure.ltv_submission=0.5")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("-id is required")
	}

	fields := map[string]any{}
	if !*bare {
		// $.id is not sent: the SDK sets it from the workflow ID automatically
		// at workflow start (runtime/workflow.go), before any update can run -
		// data-set would reject it as already-set.
		fields["$.customer.name"] = *name
		fields["$.customer.nik"] = *nik
		fields["$.customer.birth_date"] = *birthDate
		fields["$.process.asset.license_plate"] = *licensePlate
		fields["$.process.loan_structure.provisional_amount"] = *provisionalAmount
		fields["$.process.loan_structure.ltv_submission"] = *ltvSubmission
	}
	for path, val := range extra.parsed() {
		fields[path] = val
	}

	fmt.Printf("inject (data-set) -> %s: %v\n", *id, fields)
	return sendDataSet(ctx, c, *id, fields)
}

func cmdOverride(ctx context.Context, c client.Client, args []string) error {
	fs := flag.NewFlagSet("override", flag.ExitOnError)
	id := fs.String("id", "", "workflow id (= document id)")
	var extra fieldFlags
	fs.Var(&extra, "field", "raw field override, path=value (repeatable), e.g. $.customer.birth_date=1991-01-01")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("-id is required")
	}
	fields := extra.parsed()
	if len(fields) == 0 {
		return fmt.Errorf("at least one -field path=value is required")
	}

	fmt.Printf("override -> %s: %v\n", *id, fields)
	return sendOverride(ctx, c, *id, fields)
}

func cmdVerdict(ctx context.Context, c client.Client, args []string) error {
	fs := flag.NewFlagSet("verdict", flag.ExitOnError)
	id := fs.String("id", "", "workflow id (= document id)")
	status := fs.String("status", "pending", "approved|rejected|pending")
	dataset := fs.String("dataset", "", "required_data_set")
	maxLTV := fs.Float64("max-ltv", 0, "risk_system.max_ltv")
	rejectReason := fs.String("reject-reason", "", "risk_system.reject_reason")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *dataset == "" {
		return fmt.Errorf("-id and -dataset are required")
	}
	fmt.Printf("verdict -> %s: status=%s dataset=%s max_ltv=%v\n", *id, *status, *dataset, *maxLTV)
	return sendVerdict(ctx, c, *id, *status, *dataset, *maxLTV, *rejectReason)
}

func cmdDescribe(ctx context.Context, c client.Client, args []string) error {
	fs := flag.NewFlagSet("describe", flag.ExitOnError)
	id := fs.String("id", "", "workflow id (= document id)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("-id is required")
	}
	resp, err := c.DescribeWorkflowExecution(ctx, *id, "")
	if err != nil {
		return err
	}
	info := resp.GetWorkflowExecutionInfo()
	fmt.Printf("status=%s pendingActivities=%d\n", info.GetStatus(), len(resp.PendingActivities))
	for _, pa := range resp.PendingActivities {
		fmt.Printf("  - %s (id=%s) attempt=%d state=%s lastFailure=%v\n",
			pa.GetActivityType().GetName(), pa.GetActivityId(), pa.GetAttempt(), pa.GetState(), pa.GetLastFailure())
	}
	return nil
}

func cmdCompleteSurvey(ctx context.Context, c client.Client, args []string) error {
	fs := flag.NewFlagSet("complete-survey", flag.ExitOnError)
	id := fs.String("id", "", "workflow id (= document id)")
	surveyType := fs.String("type", "", "survey type (identity|asset|financing|income|final_review)")
	seq := fs.Int("seq", 0, "trigger_seq value to write (final page only)")
	outcome := fs.String("outcome", "final", "partial|final - a real survey is multiple form pages; only the final page closes the task")
	var extra fieldFlags
	fs.Var(&extra, "field", "raw field override, path=value (repeatable) - the only source of data for -outcome partial (one form page's subset of fields); layered on top of the canned outcome for -outcome final")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *surveyType == "" {
		return fmt.Errorf("-id and -type are required")
	}
	if *outcome != "partial" && *outcome != "final" {
		return fmt.Errorf("-outcome must be partial or final, got %q", *outcome)
	}
	fmt.Printf("complete-survey -> %s: type=%s outcome=%s seq=%d fields=%v\n", *id, *surveyType, *outcome, *seq, extra.parsed())
	return completeSurvey(ctx, c, *id, *surveyType, *outcome, *seq, extra.parsed())
}

// completeSurvey simulates a human submitting a page of the currently-open
// SURVEY task for document id: it looks up the task-master workflow
// system.CreateTaskMasterFunction started for this document, finds the
// pending SURVEY task on it, and sends the same "task-completion" Temporal
// update a real Task Service would send once a human submits a form page.
// outcome "partial" sends only fieldOverrides (that page's subset of
// writable fields, no cursor fields); outcome "final" sends
// survey.SurveyOutcomeFields' full canned payload (including the cursor
// fields) with fieldOverrides layered on top, and closes the task.
func completeSurvey(ctx context.Context, c client.Client, id, surveyType, outcome string, seq int, fieldOverrides map[string]any) error {
	var data map[string]any
	action := taskdefs.OutcomeCompleted
	if outcome == "partial" {
		action = taskdefs.OutcomePartial
		data = fieldOverrides
	} else {
		fields, err := survey.SurveyOutcomeFields(surveyType, seq)
		if err != nil {
			return err
		}
		data = make(map[string]any, len(fields)+len(fieldOverrides))
		for path, val := range fields {
			data[string(path)] = val
		}
		for path, val := range fieldOverrides {
			data[path] = val
		}
	}

	taskMasterID, err := findTaskMasterID(ctx, c, id)
	if err != nil {
		return fmt.Errorf("find task master: %w", err)
	}
	taskID, err := findPendingTaskID(ctx, c, taskMasterID, survey.TaskName)
	if err != nil {
		return fmt.Errorf("find pending %s task: %w", survey.TaskName, err)
	}

	tcd := taskdefs.ServiceTaskCompletionData{
		Id:       taskdefs.TaskId(taskID),
		MasterId: taskdefs.TaskId(taskMasterID),
		Type:     taskdefs.TaskType(survey.TaskName),
		Action:   action,
		Data:     data,
	}
	handle, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   taskMasterID,
		UpdateName:   "task-completion",
		Args:         []interface{}{tcd},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return fmt.Errorf("task-completion: %w", err)
	}
	var resp taskdefs.ServiceTaskCompletionSubmissionResponse
	return handle.Get(ctx, &resp)
}

// findTaskMasterID polls findTaskMasterIDOnce for a few seconds: create_task_master
// runs eagerly but asynchronously relative to the verdict update that
// unblocked it, so calling this immediately after waitUntilIdle returns can
// race ahead of it actually landing in history.
func findTaskMasterID(ctx context.Context, c client.Client, id string) (string, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		taskMasterID, err := findTaskMasterIDOnce(ctx, c, id)
		if err == nil {
			return taskMasterID, nil
		}
		if time.Now().After(deadline) {
			return "", err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// findTaskMasterIDOnce reads the document workflow's history for the result
// of create_master_task (system.CreateTaskMasterFunction) - the workflow id
// of the task-master workflow it started, also written to
// $.internal.taskmaster.id. There's no query for reading document fields
// directly (see internal/process/workflow), so this is the only way to
// discover it from outside the worker process.
func findTaskMasterIDOnce(ctx context.Context, c client.Client, id string) (string, error) {
	scheduledByEventID := map[int64]bool{}
	iter := c.GetWorkflowHistory(ctx, id, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			return "", err
		}
		if sched := event.GetActivityTaskScheduledEventAttributes(); sched != nil {
			if sched.GetActivityType().GetName() == "create_task_master" {
				scheduledByEventID[event.GetEventId()] = true
			}
			continue
		}
		if completed := event.GetActivityTaskCompletedEventAttributes(); completed != nil {
			if !scheduledByEventID[completed.GetScheduledEventId()] {
				continue
			}
			// CreateTaskMasterFunction's write-side converter wraps the raw
			// taskMasterId string into {"$.internal.taskmaster.id": id}
			// before it becomes the activity result (system.go
			// CreateTaskMasterFunction's fOut) - decode that shape, not a
			// bare string.
			var out map[string]string
			if err := converter.GetDefaultDataConverter().FromPayloads(completed.GetResult(), &out); err != nil {
				return "", fmt.Errorf("decode create_task_master result: %w", err)
			}
			taskMasterID, ok := out["$.internal.taskmaster.id"]
			if !ok {
				return "", fmt.Errorf("create_task_master result missing $.internal.taskmaster.id: %v", out)
			}
			return taskMasterID, nil
		}
	}
	return "", fmt.Errorf("create_task_master has not completed yet for workflow %q", id)
}

// findPendingTaskID queries the task-master workflow (internal/tasksim) for
// the still-open task of the given type, polling for a few seconds since
// survey's task-create update can still be in flight right after
// findTaskMasterID resolves.
func findPendingTaskID(ctx context.Context, c client.Client, taskMasterID string, taskType string) (string, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		taskID, err := findPendingTaskIDOnce(ctx, c, taskMasterID, taskType)
		if err == nil {
			return taskID, nil
		}
		if time.Now().After(deadline) {
			return "", err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func findPendingTaskIDOnce(ctx context.Context, c client.Client, taskMasterID string, taskType string) (string, error) {
	val, err := c.QueryWorkflow(ctx, taskMasterID, "", tasksim.PendingTaskQuery)
	if err != nil {
		return "", err
	}
	var pending []tasksim.PendingTask
	if err := val.Get(&pending); err != nil {
		return "", err
	}
	for _, p := range pending {
		if string(p.Type) == taskType {
			return string(p.Id), nil
		}
	}
	return "", fmt.Errorf("no pending %s task on task master %q", taskType, taskMasterID)
}

// fieldFlags collects repeated -field path=value flags.
type fieldFlags []string

func (f *fieldFlags) String() string     { return strings.Join(*f, ",") }
func (f *fieldFlags) Set(s string) error { *f = append(*f, s); return nil }

func (f fieldFlags) parsed() map[string]any {
	out := make(map[string]any, len(f))
	for _, kv := range f {
		path, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[path] = parseFieldValue(val)
	}
	return out
}

func parseFieldValue(s string) any {
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n
	}
	if b, err := strconv.ParseBool(s); err == nil {
		return b
	}
	return s
}

// ---- update senders shared by the manual and scenario commands ----

// sendDataSet sets previously-unset fields via the SDK's built-in "data-set"
// update (runtime.WorkflowUpdateSet) - registered for every worker, no
// worker-side code needed. This is the same update LGS's DataSet handler
// calls in production (application/data_set.go): a flat path->value map, no
// document_id wrapper.
func sendDataSet(ctx context.Context, c client.Client, id string, fields map[string]any) error {
	return sendUpdate(ctx, c, id, runtime.WorkflowUpdateSet, fields)
}

// sendOverride overwrites already-set fields via this worker's own
// "data-forward-override" handler (override.go) - data-set refuses to touch
// a field that's already set, so out-of-band corrections need this instead.
func sendOverride(ctx context.Context, c client.Client, id string, fields map[string]any) error {
	return sendUpdate(ctx, c, id, workflow.UpdateNameOverride, map[string]any{
		"document_id": id,
		"fields":      fields,
	})
}

// sendVerdict delivers a Risk System verdict by completing the pending
// check_risk_system_pg activity directly. check_risk_system_pg is a genuine
// async Temporal activity (system.AsyncPayloadHandler) - it goes pending
// the moment it's scheduled and stays that way until this completes it, so
// there is structurally nothing to complete unless Risk System was actually
// asked about the current stage first.
//
// It validates status/dataset before completing: nothing else validates
// this payload the way a data-forward update's JSON Schema gate used to -
// an invalid value discovered only inside check_risk_system_pg's async
// handler doesn't just fail the activity, it fails the whole workflow
// execution. This mirrors where that gate (LGS's real proxy schema
// validation) would sit in production, ahead of ever reaching the worker.
func sendVerdict(ctx context.Context, c client.Client, id, status, dataset string, maxLTV float64, rejectReason string) error {
	if !checkrisksystem.ValidStatus(status) {
		return fmt.Errorf("verdict rejected before sending: status %q is not a recognized value", status)
	}
	if !checkrisksystem.ValidRequiredDataSet(dataset) {
		return fmt.Errorf("verdict rejected before sending: required_data_set %q is not a recognized value", dataset)
	}
	activityID, err := findPendingActivityID(ctx, c, id, "check_risk_system_pg")
	if err != nil {
		return fmt.Errorf("find pending check_risk_system_pg: %w", err)
	}
	payload := map[string]any{
		"status":            status,
		"required_data_set": dataset,
		"max_ltv":           maxLTV,
		"reject_reason":     rejectReason,
	}
	return c.CompleteActivityByID(ctx, namespace, id, "", activityID, payload, nil)
}

// findPendingActivityID polls findPendingActivityIDOnce for a few seconds:
// check_risk_system_pg becomes pending asynchronously relative to whatever
// unblocked it (survey completion, seeding), so calling this immediately
// after waitUntilIdle returns can race ahead of it actually showing up.
func findPendingActivityID(ctx context.Context, c client.Client, id, activityType string) (string, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		activityID, err := findPendingActivityIDOnce(ctx, c, id, activityType)
		if err == nil {
			return activityID, nil
		}
		if time.Now().After(deadline) {
			return "", err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func findPendingActivityIDOnce(ctx context.Context, c client.Client, id, activityType string) (string, error) {
	resp, err := c.DescribeWorkflowExecution(ctx, id, "")
	if err != nil {
		return "", err
	}
	for _, pa := range resp.PendingActivities {
		if pa.GetActivityType().GetName() == activityType {
			return pa.GetActivityId(), nil
		}
	}
	return "", fmt.Errorf("no pending %s activity for workflow %q - Risk System was never asked about the current stage, or a verdict was already delivered", activityType, id)
}

func sendUpdate(ctx context.Context, c client.Client, id, updateName string, payload map[string]any) error {
	handle, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   id,
		UpdateName:   updateName,
		Args:         []interface{}{payload},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", updateName, err)
	}
	var result any
	if err := handle.Get(ctx, &result); err != nil {
		return fmt.Errorf("%s: %w", updateName, err)
	}
	return nil
}

// waitUntilIdle gives the workflow a moment to run whatever the last update
// triggered, then polls until no activity is pending or timeout elapses.
// It never fails the run outright on timeout - a still-pending activity is
// exactly what TC3 (unrecognised required_data_set) expects to observe.
func waitUntilIdle(ctx context.Context, c client.Client, id string, timeout time.Duration) error {
	time.Sleep(300 * time.Millisecond)
	deadline := time.Now().Add(timeout)
	for {
		resp, err := c.DescribeWorkflowExecution(ctx, id, "")
		if err != nil {
			return err
		}
		if len(resp.PendingActivities) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out with %d activity(ies) still pending", len(resp.PendingActivities))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// ---- scenarios, one per RISK_SYSTEM_POC.md test case ----

type scenarioStep struct {
	desc string
	// expectErr marks a step whose update call is supposed to be rejected -
	// the runner treats a non-nil error as success and a nil error as the
	// scenario failing to prove its point.
	expectErr bool
	run       func(ctx context.Context, c client.Client, id string) error
}

func overrideStep(desc string, fields map[string]any) scenarioStep {
	return scenarioStep{desc: desc, run: func(ctx context.Context, c client.Client, id string) error {
		return sendOverride(ctx, c, id, fields)
	}}
}

func verdictStep(desc, status, dataset string, maxLTV float64, rejectReason string) scenarioStep {
	return scenarioStep{desc: desc, run: func(ctx context.Context, c client.Client, id string) error {
		return sendVerdict(ctx, c, id, status, dataset, maxLTV, rejectReason)
	}}
}

func rejectedVerdictStep(desc, status, dataset string, maxLTV float64, rejectReason string) scenarioStep {
	s := verdictStep(desc, status, dataset, maxLTV, rejectReason)
	s.expectErr = true
	return s
}

// surveyCompleteStep simulates a human submitting (and closing) the SURVEY
// task that the preceding verdict step opened - survey no longer
// completes itself, so every scenario that expects a survey's findings to
// land must send this after each verdict that advances required_data_set
// to a new survey_type.
func surveyCompleteStep(desc, surveyType string, seq int) scenarioStep {
	return scenarioStep{desc: desc, run: func(ctx context.Context, c client.Client, id string) error {
		return completeSurvey(ctx, c, id, surveyType, "final", seq, nil)
	}}
}

// surveyPartialStep simulates a human submitting one non-final page of an
// open SURVEY task - the task stays open (EnablePartialCompletion), and the
// framework mints a fresh task id/token for the next page.
func surveyPartialStep(desc, surveyType string, fields map[string]any) scenarioStep {
	return scenarioStep{desc: desc, run: func(ctx context.Context, c client.Client, id string) error {
		return completeSurvey(ctx, c, id, surveyType, "partial", 0, fields)
	}}
}

// note is a step that only prints what to check by hand (ArangoDB / Temporal
// UI) - some claims in RISK_SYSTEM_POC.md (e.g. "the document still holds
// the injected placeholder values") aren't observable through any Temporal
// update or query this worker registers.
func note(msg string) scenarioStep {
	return scenarioStep{desc: "NOTE: " + msg, run: func(context.Context, client.Client, string) error { return nil }}
}

// defaultIdentityFields excludes $.id: the SDK sets it from the workflow ID
// automatically at workflow start (runtime/workflow.go), before any update
// can run - data-set would reject it as already-set. It also carries the
// DP's originally requested provisional_amount/ltv_submission, matching
// cmdInject's own defaults - calculateriskfunding's readSet requires them
// before any survey runs (see survey/impl.go's writeSet comment), and the
// financing survey only revises them later.
func defaultIdentityFields() map[string]any {
	return map[string]any{
		"$.customer.name":                             "John Placeholder",
		"$.customer.nik":                              "3201010101010001",
		"$.customer.birth_date":                       "1990-05-20",
		"$.process.asset.license_plate":               "B5678ABC",
		"$.process.loan_structure.provisional_amount": 90000.0,
		"$.process.loan_structure.ltv_submission":     0.75,
	}
}

func injectIdentityStep() scenarioStep {
	return scenarioStep{desc: "inject identity fields (data-set)", run: func(ctx context.Context, c client.Client, id string) error {
		return sendDataSet(ctx, c, id, defaultIdentityFields())
	}}
}

// Every scenario below matches RISK_SYSTEM_POC.md's original multi-stage
// flow in full: check_risk_system_pg is a genuine async Temporal activity
// (see internal/process/scoring/checkrisksystem), so it now genuinely stays
// open per stage until "verdict" completes it - every verdict that opens a
// new survey stage is followed by the matching surveyCompleteStep, exactly
// as a real applicant would progress through the journey.
var scenarios = map[string][]scenarioStep{
	"tc1": {
		injectIdentityStep(),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		surveyCompleteStep("submit identity survey", "identity", 1),
		verdictStep("customer_verification verdict -> ASSET_REVIEW", "pending", "ASSET_REVIEW", 0, ""),
		surveyCompleteStep("submit asset survey", "asset", 2),
		verdictStep("asset_review verdict -> FINANCING", "pending", "FINANCING", 0, ""),
		surveyCompleteStep("submit financing survey", "financing", 3),
		verdictStep("financing verdict -> INCOME_REVIEW (max_ltv=0.6 caps the loan)", "pending", "INCOME_REVIEW", 0.6, ""),
		surveyCompleteStep("submit income survey", "income", 4),
		verdictStep("income_review verdict -> FINAL_REVIEW", "pending", "FINAL_REVIEW", 0.6, ""),
		surveyCompleteStep("submit final_review survey", "final_review", 5),
		verdictStep("final_review verdict -> approved", "approved", "FINAL_REVIEW", 0.6, ""),
		note("status=approved, loan_structure.max_funding=60000, loan_structure.ltv_max=0.6"),
	},
	"tc2": {
		injectIdentityStep(),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		surveyCompleteStep("submit identity survey", "identity", 1),
		verdictStep("customer_verification verdict -> ASSET_REVIEW", "pending", "ASSET_REVIEW", 0, ""),
		surveyCompleteStep("submit asset survey", "asset", 2),
		verdictStep("asset_review verdict -> FINANCING", "pending", "FINANCING", 0, ""),
		surveyCompleteStep("submit financing survey", "financing", 3),
		verdictStep("financing verdict -> INCOME_REVIEW (max_ltv=0.6 caps the loan)", "pending", "INCOME_REVIEW", 0.6, ""),
		surveyCompleteStep("submit income survey", "income", 4),
		verdictStep("income_review verdict -> FINAL_REVIEW", "pending", "FINAL_REVIEW", 0.6, ""),
		surveyCompleteStep("submit final_review survey", "final_review", 5),
		verdictStep("final_review verdict -> rejected", "rejected", "FINAL_REVIEW", 0.6, "INCOME_INSUFFICIENT"),
		note("status=rejected, status_reason=INCOME_INSUFFICIENT, status_timestamps.terminal set"),
	},
	"tc3": {
		injectIdentityStep(),
		rejectedVerdictStep("post_submission verdict with a data set outside the schema enum", "pending", "COLLATERAL_REVIEW", 0, ""),
		note("rejected client-side by sendVerdict's checkrisksystem.ValidRequiredDataSet check, before it ever calls CompleteActivityByID - mirrors where LGS's proxy schema gate used to sit in production. check_risk_system_pg is now a genuine async activity with no data-forward Update in front of it, so an unrecognized value can't be allowed to reach asyncHandler: that would fail the activity after CompleteActivityByID already looked like it succeeded, crashing the whole workflow run instead of just this step. The same enum lives in checkrisksystem's surveyTypeByDataSet (see TestSurveyTypeForDataSet) - testcli just checks it earlier"),
		verdictStep("corrected post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		surveyCompleteStep("submit identity survey", "identity", 1),
		note("the chain proceeds normally, exactly as in tc1"),
	},
	"tc4": {
		injectIdentityStep(),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		surveyCompleteStep("submit identity survey", "identity", 1),
		verdictStep("customer_verification verdict -> ASSET_REVIEW", "pending", "ASSET_REVIEW", 0, ""),
		surveyCompleteStep("submit asset survey", "asset", 2),
		verdictStep("asset_review verdict RE-ASKS for CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		surveyCompleteStep("re-submit identity survey", "identity", 3),
		note("stage_token falls back to customer_verification even though the cursor was already past it - no hardcoded stage order; check_risk_system_pg fires again for customer_verification since its gate (birth_date present) is already satisfied"),
	},
	"tc5": {
		injectIdentityStep(),
		note("check now: stage_token=post_submission, customer.birth_date/name are still the injected placeholders - survey has not run yet"),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		note("the identity SURVEY task is now open and pending - it is a genuine signal-gated Temporal task (system.CreateTaskFunction), so it does NOT auto-complete on its own: birth_date is still the placeholder"),
		surveyPartialStep("submit page 1 of the identity survey (birth_date only, partial)", "identity", map[string]any{"$.customer.birth_date": "1985-03-15"}),
		note("a partial completion doesn't close the task: the framework mints a fresh task id for page 2 (WorkflowActivity.IsPartial re-exec) - birth_date is set, but name/trigger_seq/stage_token are still pending page 2"),
		surveyCompleteStep("submit page 2 of the identity survey (name + cursor, final)", "identity", 1),
		note("only now, after the final page's task-completion, does the task close and the cursor advance"),
	},
	"tc6": {
		injectIdentityStep(),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		surveyCompleteStep("submit identity survey", "identity", 1),
		verdictStep("customer_verification verdict -> ASSET_REVIEW", "pending", "ASSET_REVIEW", 0, ""),
		surveyCompleteStep("submit asset survey", "asset", 2),
		overrideStep("out-of-band birth date correction", map[string]any{"$.customer.birth_date": "1991-01-01"}),
		note("trigger_seq/stage_token must be unchanged (2 / asset_review) - the seed must never re-fire once progress exists, and this unrelated override must not re-open a SURVEY task for a stage that's already done (survey's completion-gate precondition) nor create a new pending check_risk_system_pg activity (its own readset/precondition are untouched by this override)"),
	},
	"tc7": {
		injectIdentityStep(),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		surveyCompleteStep("submit identity survey", "identity", 1),
		verdictStep("customer_verification verdict -> ASSET_REVIEW", "pending", "ASSET_REVIEW", 0, ""),
		surveyCompleteStep("submit asset survey", "asset", 2),
		verdictStep("asset_review verdict -> FINANCING", "pending", "FINANCING", 0, ""),
		surveyCompleteStep("submit financing survey", "financing", 3),
		overrideStep("override the submitted LTV lower than Risk System's cap", map[string]any{"$.process.loan_structure.ltv_submission": 0.5}),
		verdictStep("financing verdict with max_ltv=0.6 (higher than submitted)", "pending", "INCOME_REVIEW", 0.6, ""),
		note("effective_ltv=0.5 and max_funding=50000 - the lower submitted value wins, RS's cap never inflates it"),
	},
}

func cmdScenario(ctx context.Context, c client.Client, args []string) error {
	// The scenario name is a leading positional argument (`tc tc1 -id ..`),
	// but Go's flag package stops parsing flags at the first non-flag token -
	// it would otherwise swallow -id as a positional arg instead of parsing
	// it. Peel the name off before handing the rest to the flag set.
	if len(args) < 1 {
		return fmt.Errorf("usage: testcli tc <tc1|tc2|tc3|tc4|tc5|tc6|tc7> -id <workflow-id>")
	}
	name, rest := args[0], args[1:]
	steps, ok := scenarios[name]
	if !ok {
		return fmt.Errorf("unknown scenario %q (want one of tc1..tc7)", name)
	}

	fs := flag.NewFlagSet("tc", flag.ExitOnError)
	id := fs.String("id", "", "workflow id (= document id); generated if omitted")
	wait := fs.Duration("wait", 2*time.Second, "max time to wait for pending activities to settle after each step")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *id == "" {
		*id = defaultWorkflowID()
	}

	fmt.Printf("=== %s: starting workflow %s ===\n", name, *id)
	if _, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: *id, TaskQueue: taskQueue}, workflowType); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	for i, s := range steps {
		fmt.Printf("[%d/%d] %s\n", i+1, len(steps), s.desc)
		err := s.run(ctx, c, *id)
		switch {
		case s.expectErr && err == nil:
			return fmt.Errorf("step %d (%s): expected this update to be rejected, but it succeeded", i+1, s.desc)
		case s.expectErr:
			fmt.Printf("  rejected as expected: %v\n", err)
		case err != nil:
			return fmt.Errorf("step %d (%s): %w", i+1, s.desc, err)
		}
		if err := waitUntilIdle(ctx, c, *id, *wait); err != nil {
			fmt.Printf("  warning: %v\n", err)
		}
	}
	fmt.Printf("=== %s: done ===\n", name)
	return nil
}
