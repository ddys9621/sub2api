package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Region resolution must always end up at the path-style streaming
// endpoint Kiro IDE itself POSTs to: q.<region>.amazonaws.com plus the
// /generateAssistantResponse path.

func TestKiroUpstreamEndpointForAccountUsesAccountRegion(t *testing.T) {
	account := &Account{Credentials: map[string]any{"region": "eu-central-1"}}

	require.Equal(t,
		"https://q.eu-central-1.amazonaws.com/generateAssistantResponse",
		kiroUpstreamEndpointForAccount(account, "arn:aws:codewhisperer:us-east-1:123456789012:profile/dev"),
	)
}

func TestKiroUpstreamEndpointForAccountFallsBackToProfileArnRegion(t *testing.T) {
	account := &Account{Credentials: map[string]any{}}

	require.Equal(t,
		"https://q.ap-southeast-1.amazonaws.com/generateAssistantResponse",
		kiroUpstreamEndpointForAccount(account, "arn:aws:codewhisperer:ap-southeast-1:123456789012:profile/dev"),
	)
}

func TestKiroUpstreamEndpointForAccountDefaultsToUSEast1(t *testing.T) {
	require.Equal(t,
		"https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		kiroUpstreamEndpointForAccount(nil, ""),
	)
}

// kiroRESTBaseEndpoint preserves the host-root URL used by the
// non-streaming RPC helpers (ListAvailableModels et al). It must NOT
// include the streaming operation in the path; only kiroUpstreamEndpoint
// does that.

func TestKiroRESTBaseEndpointKeepsHostRoot(t *testing.T) {
	require.Equal(t,
		"https://q.us-east-1.amazonaws.com/",
		kiroRESTBaseEndpoint(nil, ""),
	)
}

// The headers we send for the streaming call must impersonate the real
// Kiro IDE (Electron, aws-sdk-js) — not Amazon-Q-For-CLI. AWS abuse
// heuristics flag the latter when paired with an IDE OAuth token, which
// is what triggered the historical bans.

func TestKiroUpstreamHeadersImpersonateKiroIDE(t *testing.T) {
	account := &Account{ID: 42, Credentials: map[string]any{}}
	headers := kiroUpstreamHeaders("token", account)

	require.Equal(t, "application/json", headers.Get("Content-Type"))
	require.Equal(t, "Bearer token", headers.Get("Authorization"))

	// IDE persona — no Amazon-Q-CLI strings, must mention KiroIDE +
	// the streaming API version + lang/js (aws-sdk-js runtime, NOT
	// aws-sdk-rust which is the Amazon Q CLI).
	ua := headers.Get("User-Agent")
	require.Contains(t, ua, "aws-sdk-js/")
	require.Contains(t, ua, "lang/js")
	require.Contains(t, ua, "api/codewhispererstreaming#")
	require.Contains(t, ua, "KiroIDE-"+kiroIDEVersion+"-")
	require.NotContains(t, strings.ToLower(ua), "amazonq-for-cli")
	require.NotContains(t, strings.ToLower(ua), "aws-sdk-rust")

	amzUA := headers.Get("X-Amz-User-Agent")
	require.Contains(t, amzUA, "aws-sdk-js/")
	require.Contains(t, amzUA, "KiroIDE "+kiroIDEVersion+" ")
	require.NotContains(t, strings.ToLower(amzUA), "amazonq-for-cli")

	// Kiro IDE always tags requests with the IDE agent mode.
	require.Equal(t, "spec", headers.Get("X-Amzn-Kiro-Agent-Mode"))

	// AWS SDK signals every Kiro IDE call includes.
	require.NotEmpty(t, headers.Get("Amz-Sdk-Invocation-Id"))
	require.Equal(t, "attempt=1; max=3", headers.Get("Amz-Sdk-Request"))

	// Path-style endpoint — X-Amz-Target and Accept are NOT set on
	// the IDE's streaming call. Keeping them is the smoking gun
	// that the previous header set was modelled after the CLI SDK.
	require.Empty(t, headers.Get("X-Amz-Target"))
	require.Empty(t, headers.Get("Accept"))
	require.Empty(t, headers.Get("X-Amzn-Codewhisperer-Optout"))
}

// Different accounts must produce different device fingerprints — that
// is the property that prevents AWS from clustering all of sub2api's
// traffic into a single banned device.

func TestKiroUpstreamHeadersEmbedPerAccountMachineID(t *testing.T) {
	a := &Account{ID: 1, Credentials: map[string]any{}}
	b := &Account{ID: 2, Credentials: map[string]any{}}

	uaA := kiroUpstreamHeaders("tok", a).Get("User-Agent")
	uaB := kiroUpstreamHeaders("tok", b).Get("User-Agent")
	require.NotEqual(t, uaA, uaB, "every account must present a unique machineId in User-Agent")

	// The fallback hash must be stable across calls so AWS sees the
	// same "device" repeatedly (real users do not rotate machineId).
	require.Equal(t, uaA, kiroUpstreamHeaders("tok", a).Get("User-Agent"))
}

// Admin-set machine_id always wins over the deterministic fallback so
// operators can pin a specific GUID to an account (e.g. one harvested
// from a real Kiro IDE install).

func TestKiroUpstreamHeadersHonourExplicitMachineID(t *testing.T) {
	account := &Account{
		ID: 7,
		Credentials: map[string]any{
			"machine_id": "deadbeef-cafe-1234-5678-aaaabbbbcccc",
		},
	}
	headers := kiroUpstreamHeaders("tok", account)
	require.Contains(t, headers.Get("User-Agent"), "KiroIDE-"+kiroIDEVersion+"-deadbeef-cafe-1234-5678-aaaabbbbcccc")
	require.Contains(t, headers.Get("X-Amz-User-Agent"), "KiroIDE "+kiroIDEVersion+" deadbeef-cafe-1234-5678-aaaabbbbcccc")
}

// IDC (Identity Center) accounts log into Kiro through the Amazon Q
// CLI binding rather than the IDE GUI; their real upstream sets the
// agent-mode header to "vibe". Mirror that so heuristics that compare
// auth-method ↔ agent-mode stay consistent.

func TestKiroUpstreamHeadersUseVibeAgentModeForIDCAccounts(t *testing.T) {
	account := &Account{
		ID: 99,
		Credentials: map[string]any{
			"auth_method": "idc",
		},
	}
	require.Equal(t, "vibe", kiroUpstreamHeaders("tok", account).Get("X-Amzn-Kiro-Agent-Mode"))
}

// REST helpers still need X-Amz-Target because they POST to the host
// root using the AWS RPC pattern. UA & agent-mode must stay aligned
// with the streaming path so AWS does not see two different "clients"
// behind one bearer token.

func TestKiroRESTHeadersStillSendXAmzTargetButKeepIDEUserAgent(t *testing.T) {
	account := &Account{ID: 11, Credentials: map[string]any{}}
	headers := kiroRESTHeaders("tok", kiroListAvailableModelsAmzTarget, account)

	require.Equal(t, "application/x-amz-json-1.0", headers.Get("Content-Type"))
	require.Equal(t, "*/*", headers.Get("Accept"))
	require.Equal(t, kiroListAvailableModelsAmzTarget, headers.Get("X-Amz-Target"))
	require.Equal(t, "true", headers.Get("X-Amzn-Codewhisperer-Optout"))
	require.Contains(t, headers.Get("User-Agent"), "KiroIDE-"+kiroIDEVersion+"-")
	require.Equal(t, "spec", headers.Get("X-Amzn-Kiro-Agent-Mode"))
}

func TestKiroModelMappingForAccountUsesCredentialsMapping(t *testing.T) {
	account := &Account{
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-opus-4-5-20251101": "claude-sonnet-4.5",
			},
		},
		Extra: map[string]any{
			"model_mapping": map[string]any{
				"claude-opus-4-5-20251101": "claude-opus-4.5",
			},
		},
	}

	require.Equal(t,
		"claude-sonnet-4.5",
		kiroModelMappingForAccount(account)["claude-opus-4-5-20251101"],
	)
}
