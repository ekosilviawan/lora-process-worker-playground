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
	"github.com/joho/godotenv"
	"go.temporal.io/sdk/client"

	"lora-process-worker-playground/internal/config"
	"lora-process-worker-playground/internal/process/workflow"
)

// workflowType and taskQueue are both the document name framework.System
// derives from the schema file name (system.go:174-178, 313-315) - not the
// "task_"-prefixed value the config also derives, which backs an unrelated
// internal client.
const (
	workflowType = "lpw-playground-v0_1_1"
	taskQueue    = "lpw-playground-v0_1_1"
)

func main() {
	_ = godotenv.Load()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	c, err := dial(config.LoadFromEnv())
	must(err)
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
	default:
		usage()
		os.Exit(2)
	}
	must(runErr)
}

func usage() {
	fmt.Fprint(os.Stderr, `testcli drives the Risk System POC.

Usage:
  testcli start    -id <workflow-id>
  testcli inject   -id <workflow-id> [-name ..] [-nik ..] [-birth-date ..] [-field path=value ...] [-bare]
  testcli override -id <workflow-id> -field path=value [-field path=value ...]
  testcli verdict  -id <workflow-id> -dataset <CUSTOMER_VERIFICATION|ASSET_REVIEW|FINANCING|INCOME_REVIEW|FINAL_REVIEW>
                   [-status pending|approved|rejected] [-max-ltv 0] [-reject-reason ..]
  testcli tc <tc1|tc2|tc3|tc4|tc5|tc6|tc7> -id <workflow-id> [-wait 2s]
  testcli describe -id <workflow-id>

"inject" sets previously-unset fields via the SDK's built-in "data-set" update
- the same one LGS calls in production for a fresh submission. "override"
overwrites fields that are already set, via this worker's own
"data-forward-override" handler; "data-set" refuses that outright.

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

func dial(cfg *config.Env) (client.Client, error) {
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
	return client.Dial(opts)
}

// ---- manual subcommands ----

func cmdStart(ctx context.Context, c client.Client, args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	id := fs.String("id", "", "workflow id (= document id)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("-id is required")
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

func sendVerdict(ctx context.Context, c client.Client, id, status, dataset string, maxLTV float64, rejectReason string) error {
	return sendUpdate(ctx, c, id, workflow.UpdateNameRiskSystemVerdict, map[string]any{
		"document_id":       id,
		"status":            status,
		"required_data_set": dataset,
		"max_ltv":           maxLTV,
		"risk_type":         "",
		"reject_reason":     rejectReason,
		"scored_at":         time.Now().UTC().Format(time.RFC3339),
	})
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

// note is a step that only prints what to check by hand (ArangoDB / Temporal
// UI) - some claims in RISK_SYSTEM_POC.md (e.g. "the document still holds
// the injected placeholder values") aren't observable through any Temporal
// update or query this worker registers.
func note(msg string) scenarioStep {
	return scenarioStep{desc: "NOTE: " + msg, run: func(context.Context, client.Client, string) error { return nil }}
}

// defaultIdentityFields excludes $.id: the SDK sets it from the workflow ID
// automatically at workflow start (runtime/workflow.go), before any update
// can run - data-set would reject it as already-set.
func defaultIdentityFields() map[string]any {
	return map[string]any{
		"$.customer.name":       "John Placeholder",
		"$.customer.nik":        "3201010101010001",
		"$.customer.birth_date": "1990-05-20",
	}
}

func injectIdentityStep() scenarioStep {
	return scenarioStep{desc: "inject identity fields (data-set)", run: func(ctx context.Context, c client.Client, id string) error {
		return sendDataSet(ctx, c, id, defaultIdentityFields())
	}}
}

var scenarios = map[string][]scenarioStep{
	"tc1": {
		injectIdentityStep(),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		verdictStep("customer_verification verdict -> ASSET_REVIEW", "pending", "ASSET_REVIEW", 0, ""),
		verdictStep("asset_review verdict -> FINANCING", "pending", "FINANCING", 0, ""),
		verdictStep("financing verdict -> INCOME_REVIEW (max_ltv=0.6 caps the loan)", "pending", "INCOME_REVIEW", 0.6, ""),
		verdictStep("income_review verdict -> FINAL_REVIEW", "pending", "FINAL_REVIEW", 0.6, ""),
		verdictStep("final_review verdict -> approved", "approved", "FINAL_REVIEW", 0.6, ""),
		note("status=approved, loan_structure.max_funding=60000, loan_structure.ltv_max=0.6"),
	},
	"tc2": {
		injectIdentityStep(),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		verdictStep("customer_verification verdict -> ASSET_REVIEW", "pending", "ASSET_REVIEW", 0, ""),
		verdictStep("asset_review verdict -> FINANCING", "pending", "FINANCING", 0, ""),
		verdictStep("financing verdict -> INCOME_REVIEW (max_ltv=0.6 caps the loan)", "pending", "INCOME_REVIEW", 0.6, ""),
		verdictStep("income_review verdict -> FINAL_REVIEW", "pending", "FINAL_REVIEW", 0.6, ""),
		verdictStep("final_review verdict -> rejected", "rejected", "FINAL_REVIEW", 0.6, "INCOME_INSUFFICIENT"),
		note("status=rejected, status_reason=INCOME_INSUFFICIENT, status_timestamps.terminal set"),
	},
	"tc3": {
		injectIdentityStep(),
		rejectedVerdictStep("post_submission verdict with a data set outside the schema enum", "pending", "COLLATERAL_REVIEW", 0, ""),
		note("rejected synchronously by the generic data-forward schema validator (enum check) - it never reaches apply_risk_system_verdict_pg's own fail-safe. That Go-level fail-safe (surveyTypeForDataSet, see TestSurveyTypeForDataSet) only matters once LSS has already widened the enum ahead of this worker's code, per the schema-first deploy order - not reproducible here without editing the schema, so it's covered by that unit test instead"),
		verdictStep("corrected post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		note("the chain proceeds normally, exactly as in tc1"),
	},
	"tc4": {
		injectIdentityStep(),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		verdictStep("customer_verification verdict -> ASSET_REVIEW", "pending", "ASSET_REVIEW", 0, ""),
		verdictStep("asset_review verdict RE-ASKS for CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		note("stage_token falls back to customer_verification even though the cursor was already past it - no hardcoded stage order"),
	},
	"tc5": {
		injectIdentityStep(),
		note("check now: stage_token=post_submission, customer.birth_date/name are still the injected placeholders - run_survey_pg has not run yet"),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		note("only now does birth_date become 1985-03-15"),
	},
	"tc6": {
		injectIdentityStep(),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		verdictStep("customer_verification verdict -> ASSET_REVIEW", "pending", "ASSET_REVIEW", 0, ""),
		overrideStep("out-of-band birth date correction", map[string]any{"$.customer.birth_date": "1991-01-01"}),
		note("trigger_seq/stage_token must be unchanged (2 / asset_review) - the seed must never re-fire once progress exists"),
	},
	"tc7": {
		injectIdentityStep(),
		verdictStep("post_submission verdict -> CUSTOMER_VERIFICATION", "pending", "CUSTOMER_VERIFICATION", 0, ""),
		verdictStep("customer_verification verdict -> ASSET_REVIEW", "pending", "ASSET_REVIEW", 0, ""),
		verdictStep("asset_review verdict -> FINANCING", "pending", "FINANCING", 0, ""),
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
	id := fs.String("id", "", "workflow id (= document id)")
	wait := fs.Duration("wait", 2*time.Second, "max time to wait for pending activities to settle after each step")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("-id is required")
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
