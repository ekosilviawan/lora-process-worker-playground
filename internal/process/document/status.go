package document

import (
	"time"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"
)

var statusTimestampFieldMap = map[string]common.HString{
	"new":        DocProcessStatusTimestampsNew,
	"processing": DocProcessStatusTimestampsProcessing,
	"approved":   DocProcessStatusTimestampsApproved,
	"rejected":   DocProcessStatusTimestampsRejected,
}

var terminalStatusSet = map[string]bool{
	"approved": true,
	"rejected": true,
}

func SetStatusTimestamp(mOut map[common.HString]any, status string) {
	now := time.Now().UTC().Format(time.RFC3339)
	if tsField, ok := statusTimestampFieldMap[status]; ok {
		mOut[tsField] = now
	}
	if terminalStatusSet[status] {
		mOut[DocProcessStatusTimestampsTerminal] = now
	}
}

func IsTerminalStatus(status string) bool {
	return terminalStatusSet[status]
}
