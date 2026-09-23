package document

import (
	"github.com/bfi-finance/lora-process-sdk/framework/defs"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
	"github.com/bfi-finance/lora-process-sdk/framework/defs/mapping"
	"github.com/bfi-finance/lora-process-sdk/framework/fp"
)

func AdjustAndMakeDocumentDescriptor(fields map[common.HString]*defs.FieldDescriptor[any]) *defs.DocumentDescriptor {
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
