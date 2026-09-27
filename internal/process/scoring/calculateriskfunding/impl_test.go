package calculateriskfunding

import (
	"math"
	"slices"
	"testing"

	"github.com/bfi-finance/lora-process-sdk/framework/defs/common"

	"lora-process-worker-playground/internal/process/document"
)

func TestCalculateFundingCapsSubmissionLTVAtRiskSystemMax(t *testing.T) {
	effectiveLTV, maxFunding := CalculateFunding(100000, 0.8, 0.6)
	if effectiveLTV != 0.6 {
		t.Fatalf("effective LTV = %v, want 0.6", effectiveLTV)
	}
	if maxFunding != 60000 {
		t.Fatalf("max funding = %v, want 60000", maxFunding)
	}
}

func TestCalculateFundingKeepsLowerSubmissionLTV(t *testing.T) {
	effectiveLTV, maxFunding := CalculateFunding(100000, 0.5, 0.6)
	if effectiveLTV != 0.5 || maxFunding != 50000 {
		t.Fatalf("got effective LTV %v and max funding %v, want 0.5 and 50000", effectiveLTV, maxFunding)
	}
}

// TestCalculateFundingUncappedWithoutRiskSystemVerdict mirrors what execFunc
// does when $.process.loan_structure.ltv_max is absent (Risk System hasn't
// responded yet): treat the cap as +Inf, so submissionLTV alone determines
// the provisional estimate.
func TestCalculateFundingUncappedWithoutRiskSystemVerdict(t *testing.T) {
	effectiveLTV, maxFunding := CalculateFunding(100000, 0.8, math.Inf(1))
	if effectiveLTV != 0.8 {
		t.Fatalf("effective LTV = %v, want 0.8 (uncapped)", effectiveLTV)
	}
	if maxFunding != 80000 {
		t.Fatalf("max funding = %v, want 80000", maxFunding)
	}
}

// TestOptionalLtvMaxTriggersRollback locks in the one optional field in this
// repo that deliberately sets TriggerRollback: true — see the comment on
// optionalReadSet for why (a later change to the cap only feeds
// planner.Rollback when TriggerRollback is set).
func TestOptionalLtvMaxTriggersRollback(t *testing.T) {
	if len(optionalReadSet) != 1 {
		t.Fatalf("expected exactly one optional field, got %d", len(optionalReadSet))
	}
	opt := optionalReadSet[0]
	if opt.Path != document.DocProcessLoanStructureLtvMax {
		t.Fatalf("expected the optional field to be ltv_max, got %q", opt.Path)
	}
	if !opt.TriggerRollback {
		t.Fatal("ltv_max must trigger rollback on update, or a later Risk System verdict would never re-tighten this step's result")
	}
}

// TestCappedStepRequiresLtvMax pins the cap's own data-set step: ltv_max must
// be a required read, so the step becomes runnable the moment the cap first
// appears - a newly-set field re-runs nothing through planner.Rollback, which
// is the gap this step closes - and re-runs on any later change to it.
func TestCappedStepRequiresLtvMax(t *testing.T) {
	rs := common.MakeReadSet(cappedReadSet)
	if !slices.Contains(rs.Paths, document.DocProcessLoanStructureLtvMax) {
		t.Fatalf("ltv_max must be a required read of %s, got %v", CappedProcessAndActivityName, rs.Paths)
	}
	for _, path := range readSet {
		if !slices.Contains(rs.Paths, path) {
			t.Errorf("%s must read everything %s does, missing %q", CappedProcessAndActivityName, ProcessAndActivityName, path)
		}
	}
	if slices.Contains(readSet, document.DocProcessLoanStructureLtvMax) {
		t.Error("the uncapped step must keep ltv_max optional, or it could not run before Risk System sends a cap")
	}
}
