package workflow

import (
	"fmt"

	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"go.temporal.io/sdk/workflow"
)

// OverrideRequest is the payload sent via Temporal workflow update to
// overwrite arbitrary, already-set field values on a running document
// workflow. Initial submission of unset fields should go through the SDK's
// built-in "data-set" update instead (see runtime.WorkflowUpdateSet), which
// this playground does not wrap - LGS calls it directly the same way in
// production (application/data_set.go).
type OverrideRequest struct {
	DocumentID string         `json:"document_id"`
	Fields     map[string]any `json:"fields"`
}

// OverrideToDocFields returns a StructuredDataForwardParser that validates
// each incoming field path against the document schema and writes the
// values, replacing whatever was there before.
func OverrideToDocFields(doc *defs.DocumentDescriptor) func(workflow.Context, *OverrideRequest) (map[common.HString]any, error) {
	return func(ctx workflow.Context, req *OverrideRequest) (map[common.HString]any, error) {
		log := workflow.GetLogger(ctx)

		id := workflow.GetInfo(ctx).WorkflowExecution.ID
		if req.DocumentID != id {
			err := fmt.Errorf("override: document_id %q does not match workflow id %q", req.DocumentID, id)
			log.Error(err.Error())
			return nil, err
		}

		out := make(map[common.HString]any, len(req.Fields))
		for rawPath, val := range req.Fields {
			path := common.HString(rawPath)
			if _, err := doc.Get(path); err != nil {
				return nil, fmt.Errorf("override: unknown document field %q: %w", rawPath, err)
			}
			out[path] = val
		}

		log.Debug("data-forward-override", "field_count", len(out))
		return out, nil
	}
}
