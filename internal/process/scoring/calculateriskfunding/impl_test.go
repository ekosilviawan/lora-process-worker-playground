package calculateriskfunding

import (
	"math"
	"testing"

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
// optionalReadSet for why (check_risk_system_pg writes SOME ltv_max value
// from post_submission onward, so the meaningful transition is a later value
// change, which only feeds planner.Rollback when TriggerRollback is set).
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
