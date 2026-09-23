package seedscoringcheckpoint

import (
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

	"lora-process-worker-playground/internal/process/document"
)

func TestNotYetSeededFiresOnce(t *testing.T) {
	if !notYetSeeded(nil, map[common.HString]any{}) {
		t.Fatal("seeding must run before the cursor exists")
	}
}

func TestNotYetSeededNeverReseeds(t *testing.T) {
	data := map[common.HString]any{document.DocProcessScoringTriggerSeq: 3}
	if notYetSeeded(nil, data) {
		t.Fatal("seeding must not re-fire once the cursor has already advanced past the seed value")
	}
}
