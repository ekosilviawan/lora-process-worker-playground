package seedscoringcheckpoint

import (
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

	"lora-process-worker-playground/internal/process/document"
)

func TestShouldSeedFiresOnceEligible(t *testing.T) {
	data := map[common.HString]any{document.DocProcessEligibilityPassed: true}
	if !shouldSeed(nil, data) {
		t.Fatal("seeding must run once eligibility has passed and the cursor does not exist yet")
	}
}

func TestShouldSeedSkipsIneligibleCustomer(t *testing.T) {
	data := map[common.HString]any{document.DocProcessEligibilityPassed: false}
	if shouldSeed(nil, data) {
		t.Fatal("seeding must not run for a customer who failed eligibility")
	}
}

func TestShouldSeedNeverReseeds(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessEligibilityPassed: true,
		document.DocProcessScoringTriggerSeq: 3,
	}
	if shouldSeed(nil, data) {
		t.Fatal("seeding must not re-fire once the cursor has already advanced past the seed value")
	}
}
