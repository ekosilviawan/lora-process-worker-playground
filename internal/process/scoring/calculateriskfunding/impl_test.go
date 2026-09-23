package calculateriskfunding

import "testing"

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
