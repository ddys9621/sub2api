package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/websearch"
	"github.com/stretchr/testify/require"
)

// TestGetWebSearchEmulationMode_KiroOAuthEnabled verifies that a Kiro OAuth
// account (which was previously rejected by the Anthropic-only platform gate)
// is now eligible for the three-state web search emulation flag.
func TestGetWebSearchEmulationMode_KiroOAuthEnabled(t *testing.T) {
	a := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{featureKeyWebSearchEmulation: "enabled"},
	}
	require.Equal(t, WebSearchModeEnabled, a.GetWebSearchEmulationMode())
}

func TestGetWebSearchEmulationMode_KiroOAuthDisabled(t *testing.T) {
	a := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{featureKeyWebSearchEmulation: "disabled"},
	}
	require.Equal(t, WebSearchModeDisabled, a.GetWebSearchEmulationMode())
}

func TestGetWebSearchEmulationMode_KiroOAuthDefault(t *testing.T) {
	a := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{featureKeyWebSearchEmulation: "default"},
	}
	require.Equal(t, WebSearchModeDefault, a.GetWebSearchEmulationMode())
}

// TestGetWebSearchEmulationMode_NonKiroNonAnthropicStaysDefault ensures we did
// not accidentally open the gate to other platforms such as OpenAI / Gemini.
func TestGetWebSearchEmulationMode_NonKiroNonAnthropicStaysDefault(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformGemini, PlatformAntigravity} {
		a := &Account{
			Platform: platform,
			Type:     AccountTypeAPIKey,
			Extra:    map[string]any{featureKeyWebSearchEmulation: "enabled"},
		}
		require.Equalf(t, WebSearchModeDefault, a.GetWebSearchEmulationMode(),
			"platform %q should still fall back to default mode", platform)
	}
}

// TestEvaluateWebSearchEmulation_KiroAccountForcedEnabled exercises the
// shared decision function along the Kiro-OAuth + forced-enabled path to
// catch future regressions.
func TestEvaluateWebSearchEmulation_KiroAccountForcedEnabled(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	account := &Account{
		ID:       123,
		Name:     "kiro-test",
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{featureKeyWebSearchEmulation: "enabled"},
	}

	deps := stubWebSearchDeps{globallyEnabled: true}
	body := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	require.True(t, evaluateWebSearchEmulation(context.Background(), deps, account, nil, body))
}

// TestEvaluateWebSearchEmulation_KiroWithMultipleToolsNotIntercepted ensures
// that a Kiro request carrying web_search alongside user tools is NOT
// intercepted — the emulation path is deliberately narrow to avoid hijacking
// mixed-tool conversations. (The sibling fix in request_transformer drops
// the server-side tool so the rest can still be forwarded.)
func TestEvaluateWebSearchEmulation_KiroWithMultipleToolsNotIntercepted(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	account := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{featureKeyWebSearchEmulation: "enabled"},
	}

	deps := stubWebSearchDeps{globallyEnabled: true}
	body := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"Read","input_schema":{}}]}`)
	require.False(t, evaluateWebSearchEmulation(context.Background(), deps, account, nil, body))
}

// TestEvaluateWebSearchEmulation_GlobalDisabledBlocksKiro asserts the global
// feature switch still takes precedence for Kiro accounts.
func TestEvaluateWebSearchEmulation_GlobalDisabledBlocksKiro(t *testing.T) {
	mgr := websearch.NewManager([]websearch.ProviderConfig{{Type: "brave", APIKey: "k"}}, nil)
	SetWebSearchManager(mgr)
	defer SetWebSearchManager(nil)

	account := &Account{
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{featureKeyWebSearchEmulation: "enabled"},
	}
	deps := stubWebSearchDeps{globallyEnabled: false}
	body := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	require.False(t, evaluateWebSearchEmulation(context.Background(), deps, account, nil, body))
}

// stubWebSearchDeps is a minimal implementation of webSearchEmulationDeps for
// the Kiro-side tests. channelService is not needed because these tests cover
// the force-enabled and global-disabled branches only.
type stubWebSearchDeps struct {
	globallyEnabled bool
	channel         *Channel
	channelErr      error
}

func (s stubWebSearchDeps) IsWebSearchEmulationEnabledGlobally(context.Context) bool {
	return s.globallyEnabled
}

func (s stubWebSearchDeps) ChannelForGroup(context.Context, int64) (*Channel, error) {
	return s.channel, s.channelErr
}
