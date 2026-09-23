package applyrisksystemverdict

import "testing"

func TestTranslateStatus(t *testing.T) {
	tests := map[string]string{
		"approved": "approved",
		"rejected": "rejected",
		"pending":  "processing",
	}
	for input, want := range tests {
		got, ok := translateStatus(input)
		if !ok || got != want {
			t.Fatalf("translateStatus(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	if _, ok := translateStatus("unknown"); ok {
		t.Fatal("unknown verdict status must be rejected")
	}
}

func TestSurveyTypeForDataSet(t *testing.T) {
	tests := map[string]string{
		"CUSTOMER_VERIFICATION": "identity",
		"ASSET_REVIEW":          "asset",
		"FINANCING":             "financing",
		"INCOME_REVIEW":         "income",
		"FINAL_REVIEW":          "final_review",
	}
	for input, want := range tests {
		got, ok := surveyTypeForDataSet(input)
		if !ok || got != want {
			t.Fatalf("surveyTypeForDataSet(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	if _, ok := surveyTypeForDataSet("UNKNOWN_SET"); ok {
		t.Fatal("an unrecognised required data set must fail safe, not guess a survey type")
	}
}
