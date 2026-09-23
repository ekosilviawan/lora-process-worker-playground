package workflow

import (
	"fmt"

	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"go.temporal.io/sdk/workflow"

	"lora-process-worker-playground/internal/process/document"
)

type RiskSystemVerdictRequest struct {
	DocumentID      string  `json:"document_id"`
	Status          string  `json:"status"`
	RequiredDataSet string  `json:"required_data_set"`
	MaxLTV          float64 `json:"max_ltv"`
	RiskType        string  `json:"risk_type"`
	RejectReason    string  `json:"reject_reason"`
	ScoredAt        string  `json:"scored_at"`
}

func RiskSystemVerdictToDocFields(doc *defs.DocumentDescriptor) func(workflow.Context, *RiskSystemVerdictRequest) (map[common.HString]any, error) {
	return func(ctx workflow.Context, req *RiskSystemVerdictRequest) (map[common.HString]any, error) {
		workflowID := workflow.GetInfo(ctx).WorkflowExecution.ID
		if req.DocumentID != workflowID {
			return nil, fmt.Errorf("risk system verdict: document_id %q does not match workflow id %q", req.DocumentID, workflowID)
		}
		if _, err := doc.Get(document.DocProcessScoringRiskSystemStatus); err != nil {
			return nil, fmt.Errorf("risk system verdict: schema is missing status field: %w", err)
		}

		return map[common.HString]any{
			document.DocProcessScoringRiskSystemStatus:          req.Status,
			document.DocProcessScoringRiskSystemRequiredDataSet: req.RequiredDataSet,
			document.DocProcessScoringRiskSystemMaxLtv:          req.MaxLTV,
			document.DocProcessScoringRiskSystemRiskType:        req.RiskType,
			document.DocProcessScoringRiskSystemRejectReason:    req.RejectReason,
			document.DocProcessScoringRiskSystemScoredAt:        req.ScoredAt,
		}, nil
	}
}
