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

const UpdateNameRiskSystemVerdict = "data-forward-risk-system-scored"

// MakeWorkflowFunctions builds the WFFRegistry for this worker.
func MakeWorkflowFunctions(doc *defs.DocumentDescriptor) *runtime.WFFRegistry {
	registry := runtime.NewWFFRegistry()

	overrideHandler := runtime.NewStructuredDataForwardHandler(
		UpdateNameOverride,
		OverrideToDocFields(doc),
		nil, // nil → DefaultWriteIfSetting (always overwrite)
	)
	registry.Register(runtime.NewWorkflowFunctionForStructuredHandler(overrideHandler))

	verdictHandler := runtime.NewStructuredDataForwardHandler(
		UpdateNameRiskSystemVerdict,
		RiskSystemVerdictToDocFields(doc),
		nil,
	)
	registry.Register(runtime.NewWorkflowFunctionForStructuredHandler(verdictHandler))

	return registry
}
