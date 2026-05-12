package kiro

import "testing"

func TestFallbackModelForInvalidModelDoesNotCrossModelDowngrade(t *testing.T) {
	for _, input := range []string{
		"auto",
		"claude-opus-4-7",
		"claude-opus-4.7",
		"claude-sonnet-4-5-20250929",
		"claude-haiku-4.5",
	} {
		if got := FallbackModelForInvalidModel(input, input); got != "" {
			t.Errorf("FallbackModelForInvalidModel(%q) = %q, want empty", input, got)
		}
	}
}

func TestSortModelsByPreferencePutsVerifiedModelsFirst(t *testing.T) {
	models := []Model{
		ModelFromID("claude-sonnet-4.6", ""),
		ModelFromID("auto", ""),
		ModelFromID("deepseek-3.2", ""),
		ModelFromID("qwen3-coder-next", ""),
	}

	sorted := SortModelsByPreference(models)
	if got, want := sorted[0].ID, "deepseek-3.2"; got != want {
		t.Fatalf("first model = %q, want %q", got, want)
	}
	if got, want := sorted[1].ID, "qwen3-coder-next"; got != want {
		t.Fatalf("second model = %q, want %q", got, want)
	}
}
