//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

// genericOptInAccount builds an OpenAI-platform API-key account shaped like a
// StepFun upstream: explicit api_protocol plus per-protocol base URLs.
func genericOptInAccount(protocol string) *Account {
	return &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "step-key",
			"api_protocol": protocol,
			"base_url":     "https://api.stepfun.com/v1",
			"api_base_urls": map[string]any{
				APIProtocolChatCompletions: "https://api.stepfun.com/v1",
				APIProtocolAnthropic:       "https://api.stepfun.com",
			},
		},
	}
}

func TestIsGenericMultiProtocolAPIKey(t *testing.T) {
	t.Parallel()

	require.True(t, genericOptInAccount(APIProtocolAdaptive).IsGenericMultiProtocolAPIKey())
	require.True(t, genericOptInAccount(APIProtocolAnthropic).IsGenericMultiProtocolAPIKey())
	require.True(t, genericOptInAccount(APIProtocolResponses).IsGenericMultiProtocolAPIKey())
	require.True(t, genericOptInAccount(APIProtocolChatCompletions).IsGenericMultiProtocolAPIKey())

	byBases := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "k",
			"base_url": "https://api.stepfun.com/v1",
			"api_base_urls": map[string]any{
				APIProtocolAnthropic: "https://api.stepfun.com",
			},
		},
	}
	require.True(t, byBases.IsGenericMultiProtocolAPIKey(), "per-protocol bases opt in without api_protocol")

	plain := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "k", "base_url": "https://api.openai.com/v1"},
	}
	require.False(t, plain.IsGenericMultiProtocolAPIKey(), "plain OpenAI accounts never opt in")
	require.False(t, plain.IsMultiProtocolAPIKey())

	oauth := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"api_protocol": APIProtocolAdaptive},
	}
	require.False(t, oauth.IsGenericMultiProtocolAPIKey(), "OAuth never opts in")

	grok := &Account{
		Platform: PlatformGrok, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_protocol": APIProtocolAdaptive},
	}
	require.False(t, grok.IsGenericMultiProtocolAPIKey(), "only openai platform opts in")

	require.True(t, genericOptInAccount(APIProtocolAdaptive).IsMultiProtocolAPIKey())
	require.False(t, (&Account{}).IsGenericMultiProtocolAPIKey())
}

func TestGenericOptInAPIProtocol(t *testing.T) {
	t.Parallel()
	require.Equal(t, APIProtocolAdaptive, genericOptInAccount(APIProtocolAdaptive).GetAPIProtocol())
	require.Equal(t, APIProtocolAnthropic, genericOptInAccount(APIProtocolAnthropic).GetAPIProtocol())
	require.Equal(t, APIProtocolResponses, genericOptInAccount(APIProtocolResponses).GetAPIProtocol(), "explicit responses trusted without compiled matrix")
	require.Equal(t, APIProtocolChatCompletions, genericOptInAccount(APIProtocolChatCompletions).GetAPIProtocol())
	require.Equal(t, APIProtocolChatCompletions, genericOptInAccount("bogus").GetAPIProtocol(), "invalid falls back to chat")
}

func TestGenericOptInProtocolBaseURLs(t *testing.T) {
	t.Parallel()
	adaptive := genericOptInAccount(APIProtocolAdaptive)
	require.Equal(t, "https://api.stepfun.com/v1", adaptive.GetCNProtocolBaseURL(APIProtocolChatCompletions))
	require.Equal(t, "https://api.stepfun.com", adaptive.GetCNProtocolBaseURL(APIProtocolAnthropic))
	require.Equal(t, "https://api.stepfun.com/v1/messages", adaptive.GetAnthropicProtocolBaseURL()+"/v1/messages")
	require.True(t, adaptive.HasNativeAnthropicBase())

	chatOnly := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "k", "base_url": "https://llm.example/v1",
			"api_protocol": APIProtocolAdaptive,
		},
	}
	require.Equal(t, "https://llm.example/v1", chatOnly.GetCNProtocolBaseURL(APIProtocolChatCompletions), "chat falls back to legacy base_url")
	require.Equal(t, "", chatOnly.GetCNProtocolBaseURL(APIProtocolAnthropic), "no invented anthropic default")
	require.False(t, chatOnly.HasNativeAnthropicBase())

	anthropicOnly := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "k", "base_url": "https://api.stepfun.com",
			"api_protocol": APIProtocolAnthropic,
			"api_base_urls": map[string]any{
				APIProtocolChatCompletions: "https://api.stepfun.com/v1",
			},
		},
	}
	require.Equal(t, "https://api.stepfun.com/v1", anthropicOnly.GetOpenAIFormatBaseURL(), "models path uses per-protocol chat base")
	noChatBase := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "k", "base_url": "https://api.stepfun.com",
			"api_protocol": APIProtocolAnthropic,
		},
	}
	require.Equal(t, "", noChatBase.GetOpenAIFormatBaseURL(), "no chat base configured: empty, never the anthropic base")
}

func TestGenericOptInResponsesRouting(t *testing.T) {
	t.Parallel()
	probed := genericOptInAccount(APIProtocolAdaptive)
	probed.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: true}
	require.False(t, shouldForwardOpenAIResponsesViaRawChatCompletionsForModel(probed, "step-3.7-flash"), "adaptive follows probe like regular openai")

	unprobed := genericOptInAccount(APIProtocolAdaptive)
	unprobed.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}
	require.True(t, shouldForwardOpenAIResponsesViaRawChatCompletionsForModel(unprobed, "step-3.7-flash"))

	fixedChat := genericOptInAccount(APIProtocolChatCompletions)
	require.True(t, shouldForwardOpenAIResponsesViaRawChatCompletionsForModel(fixedChat, "step-3.7-flash"))

	fixedResponses := genericOptInAccount(APIProtocolResponses)
	require.False(t, shouldForwardOpenAIResponsesViaRawChatCompletionsForModel(fixedResponses, "step-3.7-flash"), "explicit responses trusted")
}

func TestGenericOptInIgnoresGroupDispatchDefaults(t *testing.T) {
	t.Parallel()
	optedIn := genericOptInAccount(APIProtocolAdaptive)
	require.Equal(t, "claude-sonnet-4-5",
		resolveOpenAIForwardModel(optedIn, "claude-sonnet-4-5", "gpt-5.4"),
		"openai-specific dispatch defaults never leak to third-party upstreams")

	optedIn.Credentials["model_mapping"] = map[string]any{"claude-sonnet-4-5": "step-3.7-flash"}
	require.Equal(t, "step-3.7-flash",
		resolveOpenAIForwardModel(optedIn, "claude-sonnet-4-5", "gpt-5.4"),
		"account mapping still wins")

	plain := &Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "k"},
	}
	require.Equal(t, "gpt-5.4",
		resolveOpenAIForwardModel(plain, "claude-sonnet-4-5", "gpt-5.4"),
		"regular openai accounts keep dispatch defaults")
}

func TestManagedCatalogSlug(t *testing.T) {
	t.Parallel()
	withSlug := &Account{Extra: map[string]any{"managed_slug": "StepFun-Plan"}}
	require.Equal(t, "stepfunplan", withSlug.ManagedCatalogSlug())

	require.Equal(t, "", (&Account{}).ManagedCatalogSlug())
	require.Equal(t, "", (*Account)(nil).ManagedCatalogSlug())
}

func TestRemoteContractPlatformExplicitFamily(t *testing.T) {
	t.Parallel()
	explicit := &Account{
		Name:        "my remote box",
		Platform:    PlatformOpenAI,
		Credentials: map[string]any{"vendor_family": "StepFun"},
	}
	require.Equal(t, "stepfun", remoteContractPlatform(explicit), "explicit family wins over name")

	legacy := &Account{Name: "step-plan legacy", Platform: PlatformOpenAI}
	require.Equal(t, "stepfun", remoteContractPlatform(legacy), "legacy name fallback kept")

	plain := &Account{Name: "openai paid", Platform: PlatformOpenAI}
	require.Equal(t, PlatformOpenAI, remoteContractPlatform(plain))
}
