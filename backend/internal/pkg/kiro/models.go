package kiro

// Model is the JSON shape returned by the admin account-model picker.
type Model struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// DefaultTestModel is a Kiro-native model verified against GenerateAssistantResponse.
const DefaultTestModel = "deepseek-3.2"

// DefaultModels are real Kiro model IDs observed from ListAvailableModels.
// Keep these IDs in Kiro's dotted format; aliases are handled by
// DefaultModelMapping in request_transformer.go.
var DefaultModels = []Model{
	{ID: "deepseek-3.2", Type: "model", DisplayName: "DeepSeek 3.2", CreatedAt: ""},
	{ID: "qwen3-coder-next", Type: "model", DisplayName: "Qwen3 Coder Next", CreatedAt: ""},
	{ID: "minimax-m2.5", Type: "model", DisplayName: "MiniMax M2.5", CreatedAt: ""},
	{ID: "minimax-m2.1", Type: "model", DisplayName: "MiniMax M2.1", CreatedAt: ""},
	{ID: "glm-5", Type: "model", DisplayName: "GLM 5", CreatedAt: ""},
	{ID: "claude-opus-4.7", Type: "model", DisplayName: "Claude Opus 4.7", CreatedAt: ""},
	{ID: "claude-opus-4.6", Type: "model", DisplayName: "Claude Opus 4.6", CreatedAt: ""},
	{ID: "claude-sonnet-4.6", Type: "model", DisplayName: "Claude Sonnet 4.6", CreatedAt: ""},
	{ID: "claude-opus-4.5", Type: "model", DisplayName: "Claude Opus 4.5", CreatedAt: ""},
	{ID: "claude-sonnet-4.5", Type: "model", DisplayName: "Claude Sonnet 4.5", CreatedAt: ""},
	{ID: "claude-sonnet-4", Type: "model", DisplayName: "Claude Sonnet 4", CreatedAt: ""},
	{ID: "claude-haiku-4.5", Type: "model", DisplayName: "Claude Haiku 4.5", CreatedAt: ""},
	{ID: "auto", Type: "model", DisplayName: "Auto", CreatedAt: ""},
}

// ModelFromID returns a display-friendly model record for a Kiro model ID.
func ModelFromID(id, displayName string) Model {
	for _, model := range DefaultModels {
		if model.ID == id {
			if displayName != "" {
				model.DisplayName = displayName
			}
			return model
		}
	}
	if displayName == "" {
		displayName = id
	}
	return Model{
		ID:          id,
		Type:        "model",
		DisplayName: displayName,
		CreatedAt:   "",
	}
}

var modelOrder = func() map[string]int {
	out := make(map[string]int, len(DefaultModels))
	for i, model := range DefaultModels {
		out[model.ID] = i
	}
	return out
}()

// SortModelsByPreference returns models ordered so verified Kiro-native models
// are tried before provider-backed Claude/auto entries that may be region-gated.
func SortModelsByPreference(models []Model) []Model {
	out := append([]Model(nil), models...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && modelPreference(out[j]) < modelPreference(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func modelPreference(model Model) int {
	if idx, ok := modelOrder[model.ID]; ok {
		return idx
	}
	return len(modelOrder) + 100
}

// FallbackModelForInvalidModel returns an explicit retry target for model IDs
// that are known aliases of the same user intent. Do not silently downgrade a
// requested Claude model to an unrelated Kiro-native model: usage logs and
// billing must reflect the model the user actually asked for.
func FallbackModelForInvalidModel(requestedModel, resolvedModel string) string {
	for _, key := range []string{requestedModel, resolvedModel} {
		if target := invalidModelFallbacks[key]; target != "" {
			return target
		}
	}
	return ""
}

var invalidModelFallbacks = map[string]string{}

// IsInvalidModelError reports whether an upstream error body is Kiro's
// INVALID_MODEL_ID validation failure.
func IsInvalidModelError(body []byte) bool {
	return containsBytes(body, []byte("INVALID_MODEL_ID"))
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
