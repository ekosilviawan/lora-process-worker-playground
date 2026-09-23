package workflow

import (
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/runtime"
)

// UpdateNameOverride is the Temporal workflow update name for overwriting
// already-set fields out of band (see override.go). Initial submission of
// unset fields uses the SDK's built-in runtime.WorkflowUpdateSet ("data-set")
// instead, the same update LGS calls in production
// (lora-gateway-service/internal/httpserver/handler/application/data_set.go)
// - no worker-side registration needed for that one.
const UpdateNameOverride = "data-forward-override"

// MakeWorkflowFunctions builds the WFFRegistry for this worker.
//
// There is no data-forward handler for the Risk System verdict: check_risk_
// system_pg is a genuine async Temporal activity (system.AsyncPayloadHandler,
// see internal/process/scoring/checkrisksystem), so the verdict is delivered
// via a real Temporal activity completion (client.CompleteActivityByID, see
// cmd/testcli's "verdict" command) rather than a workflow update.
func MakeWorkflowFunctions(doc *defs.DocumentDescriptor) *runtime.WFFRegistry {
	registry := runtime.NewWFFRegistry()

	overrideHandler := runtime.NewStructuredDataForwardHandler(
		UpdateNameOverride,
		OverrideToDocFields(doc),
		nil, // nil → DefaultWriteIfSetting (always overwrite)
	)
	registry.Register(runtime.NewWorkflowFunctionForStructuredHandler(overrideHandler))

	return registry
}
