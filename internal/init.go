package internal

import (
	"net/http"

	"github.com/bfi-finance/lora-process-sdk/framework"
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/external"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime/notification"

	"lora-process-worker-playground/internal/process"
	"lora-process-worker-playground/internal/process/document"
	"lora-process-worker-playground/internal/process/workflow"
)

// DocSchema is the document schema path served by the schema service.
// This constant is intentionally NOT injected via config: the worker version
// is coupled to a specific schema version at compile time.
const (
	DocSchema = "/json-schema/business/document/lpw-playground-v0_1_1.schema.json"
	Tenant    = "playground"
)

func RunLoraWorker(cfg external.LoraConfig, hc *http.Client) error {
	m := notification.NewNotificationMapping(Tenant, document.DocStatus, nil)
	system, err := framework.NewSystem(cfg, m, hc)
	if err != nil {
		return err
	}

	fds, err := system.LoadDocumentSchema(DocSchema)
	if err != nil {
		cfg.Logger.Error().Err(err).Msg("failed to load document schema")
		return err
	}

	doc := document.AdjustAndMakeDocumentDescriptor(fds)
	fieldChecker := defs.NewMiniDoc(fds).MustCheckExists

	processSteps, err := buildProcessSteps(system, fieldChecker)
	if err != nil {
		return err
	}

	system.SetWorkflowFunctions(workflow.MakeWorkflowFunctions(doc))

	if runErr := system.Run(
		doc,
		processSteps,
	// if runErr := system.ReplayFromJSONFile(
	// 	"2cd34b1b-a36d-4573-9505-e0d617130ad8",
	// 	" 019fec8b-b534-712a-9d3f-bf69a5c8b4db",
	// 	"/Users/ekofahrudi/Downloads/019fec8b-b534-712a-9d3f-bf69a5c8b4db_events.json",
	// 	doc,
	// 	processSteps,
	); runErr != nil {
		cfg.Logger.Error().Err(runErr).Msg("worker exited with error")
		return runErr
	}
	return nil
}

func buildProcessSteps(system *framework.System, fieldChecker func([]common.HString)) ([]*runtime.ProcessStep, error) {
	return process.MakeAllProcessSteps(system.LoadAPISchemaAsFunction, fieldChecker)
}
