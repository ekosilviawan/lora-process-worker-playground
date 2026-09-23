package tasksim

import (
	"crypto/tls"
	"strconv"

	"github.com/bfi-finance/lora-process-sdk/framework/external"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
	"github.com/bfi-finance/lora-process-sdk/framework/task"
	"go.temporal.io/sdk/client"
)

// StartWorker runs the task-master side of the document's task activities:
// it registers task.TaskWorkflow (the SDK's generic task-master workflow) on
// taskQueue, backed by this package's minimal handler instead of the full
// production task_execution.WorkflowExecutor. taskQueue must be exactly what
// framework.System derives from the document schema filename
// ("task_" + docName, system.go LoadDocumentSchema) - the same Temporal
// server/namespace as the document workflow, just a different queue, the
// same way LPW and LTW are two independent workers today.
//
// This call blocks (RunWorker); the caller runs it in a goroutine.
func StartWorker(cfg external.LoraConfig, taskQueue string) error {
	ns := cfg.TemporalNamespace
	if ns == "" {
		ns = "default"
	}
	logger := runtime.MakeStructuredLogger(cfg.Logger) // nil-safe: falls back to a default zerolog logger
	options := client.Options{
		HostPort:  cfg.TemporalHost + ":" + strconv.Itoa(cfg.TemporalPort),
		Namespace: ns,
		Logger:    logger,
	}
	if cfg.TemporalAPIKey != "" {
		options.Credentials = client.NewAPIKeyStaticCredentials(cfg.TemporalAPIKey)
		options.ConnectionOptions = client.ConnectionOptions{TLS: &tls.Config{MinVersion: tls.VersionTLS13}}
	}

	t, err := runtime.NewTemporal(&options)
	if err != nil {
		return err
	}
	t.RegisterAsWorker(taskQueue, cfg.TemporalWorkerConcurrentWorkflows)

	wf := task.NewTaskWorkflowFromExecutor(NewHandler, t, logger, nil)
	t.RegisterParametrizedWorkflow(taskQueue, wf.Workflow)

	return t.RunWorker()
}
