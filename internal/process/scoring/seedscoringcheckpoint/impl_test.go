package seedscoringcheckpoint

import (
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

	"lora-process-worker-playground/internal/process/document"
)

func TestShouldSeedFiresOnceEligible(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessAgeCheckPassed:            true,
		document.DocProcessDuplicatePlateCheckPassed: true,
	}
	if !shouldSeed(nil, data) {
		t.Fatal("seeding must run once both intake checks have passed and the cursor does not exist yet")
	}
}

func TestShouldSeedSkipsIneligibleCustomer(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessAgeCheckPassed:            false,
		document.DocProcessDuplicatePlateCheckPassed: false,
	}
	if shouldSeed(nil, data) {
		t.Fatal("seeding must not run for a customer who failed both intake checks")
	}
}

func TestShouldSeedRequiresBothChecks(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessAgeCheckPassed:            true,
		document.DocProcessDuplicatePlateCheckPassed: false,
	}
	if shouldSeed(nil, data) {
		t.Fatal("seeding must not run when only the age check passed")
	}

	data = map[common.HString]any{
		document.DocProcessAgeCheckPassed:            false,
		document.DocProcessDuplicatePlateCheckPassed: true,
	}
	if shouldSeed(nil, data) {
		t.Fatal("seeding must not run when only the duplicate-plate check passed")
	}
}

func TestShouldSeedNeverReseeds(t *testing.T) {
	data := map[common.HString]any{
		document.DocProcessAgeCheckPassed:            true,
		document.DocProcessDuplicatePlateCheckPassed: true,
		document.DocProcessScoringTriggerSeq:         3,
	}
	if shouldSeed(nil, data) {
		t.Fatal("seeding must not re-fire once the cursor has already advanced past the seed value")
	}
}
