package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGPT61SolResponsesContract(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","reasoning":{"mode":"pro","effort":"max"},"temperature":0.7,"top_p":0.9,"include":["reasoning.encrypted_content","message.output_text.logprobs"]}`)
	out, changed, err := normalizeOpenAIResponsesReasoningMode(body, "")
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "pro", gjson.GetBytes(out, "reasoning.mode").String())
	require.Equal(t, "max", gjson.GetBytes(out, "reasoning.effort").String())
	require.False(t, gjson.GetBytes(out, "temperature").Exists())
	require.False(t, gjson.GetBytes(out, "top_p").Exists())
	require.Equal(t, "reasoning.encrypted_content", gjson.GetBytes(out, "include.0").String())

	dropped, changed, err := normalizeOpenAIResponsesReasoningMode([]byte(`{"model":"openai/gpt-6.1-sol","reasoning":{"effort":"none"},"reasoning_effort":"minimal","temperature":0.2}`), "")
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(dropped, "reasoning").Exists())
	require.False(t, gjson.GetBytes(dropped, "reasoning_effort").Exists())
	require.False(t, gjson.GetBytes(dropped, "temperature").Exists())

	require.Equal(t, "gpt-6.1-sol", normalizeKnownOpenAICodexModel("gpt6.1-sol-high"))
	require.Equal(t, "gpt-6.1-sol", normalizeCodexModel("openai/gpt-6.1-sol-2026-09-29"))
	require.Empty(t, normalizeOpenAIReasoningEffortForModel("none", "gpt-6.1-sol"))
	require.Empty(t, normalizeOpenAIReasoningEffortForModel("minimal", "gpt-6.1-sol-max"))
	require.Equal(t, "max", normalizeOpenAIReasoningEffortForModel("max", "gpt-6.1-sol"))

	descriptor := newConfiguredCodexModelDescriptor("gpt-6.1-sol")
	require.Equal(t, "GPT-6.1-Sol", descriptor.DisplayName)
	require.Equal(t, "Latest workhorse model for coding and everyday work.", descriptor.Description)
	require.NotNil(t, descriptor.DefaultReasoningLevel)
	require.Equal(t, "low", *descriptor.DefaultReasoningLevel)
	require.Equal(t, []string{"low", "medium", "high", "xhigh", "max", "ultra"}, effortsFromConfiguredCodexLevels(descriptor.SupportedReasoningLevels))
	require.EqualValues(t, 272_000, descriptor.ContextWindow)
	require.EqualValues(t, 872_000, descriptor.MaxContextWindow)
	require.Equal(t, "shell_command", descriptor.ShellType)
	require.Equal(t, "text_and_image", descriptor.WebSearchToolType)
	require.Equal(t, "code_mode_only", descriptor.ToolMode)
	require.Equal(t, true, descriptor.UseResponsesLite)
	require.Equal(t, "3000", descriptor.CompHash)
	require.NotNil(t, descriptor.MultiAgentReasoningEffort)
	require.Equal(t, "xhigh", *descriptor.MultiAgentReasoningEffort)
	require.Equal(t, "v2", descriptor.MultiAgentVersion)
	require.Equal(t, []string{"text", "image"}, descriptor.InputModalities)
	require.Equal(t, "Fast", descriptor.ServiceTiers[0].Name)
	manifest, err := BuildCodexModelsManifest([]string{"gpt-6.1-sol"})
	require.NoError(t, err)
	require.Equal(t, true, gjson.GetBytes(manifest, "models.0.supports_search_tool").Bool())
	require.Equal(t, "shell_command", gjson.GetBytes(manifest, "models.0.shell_type").String())
	require.Equal(t, "low", gjson.GetBytes(manifest, "models.0.default_reasoning_level").String())
	require.Equal(t, int64(872000), gjson.GetBytes(manifest, "models.0.max_context_window").Int())
	require.Contains(t, descriptor.ModelMessages.InstructionsTemplate, "You are Codex, an agent based on GPT-6.")
	require.True(t, isOpenAICodexImageInputModel("openai/gpt-6.1-sol"))
	require.True(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-6.1-sol-max"))
	require.False(t, shouldAutoInjectPromptCacheKeyForCompat("gpt-6.1"))
}

func TestGPT61SolChatCompletionsToolAndEffortContract(t *testing.T) {
	temperature := 0.4
	for _, effort := range []string{"", "medium", "max"} {
		out, err := apicompat.ChatCompletionsToResponses(&apicompat.ChatCompletionsRequest{
			Model:           "gpt-6.1-sol",
			ReasoningEffort: effort,
			Temperature:     &temperature,
			TopP:            &temperature,
			Messages:        []apicompat.ChatMessage{{Role: "user", Content: []byte(`"hello"`)}},
		})
		require.NoError(t, err)
		require.Nil(t, out.Temperature)
		require.Nil(t, out.TopP)
		if effort == "" {
			require.Nil(t, out.Reasoning)
		} else {
			require.Equal(t, effort, out.Reasoning.Effort)
		}
	}
	// Disabled reasoning is rejected outright instead of being silently
	// upgraded to the smallest supported effort.
	for _, effort := range []string{"none", "minimal"} {
		_, err := apicompat.ChatCompletionsToResponses(&apicompat.ChatCompletionsRequest{
			Model:           "gpt-6.1-sol",
			ReasoningEffort: effort,
			Temperature:     &temperature,
			TopP:            &temperature,
			Messages:        []apicompat.ChatMessage{{Role: "user", Content: []byte(`"hello"`)}},
		})
		require.ErrorContains(t, err, "does not support reasoning effort")
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"public": "gpt-6.1-sol"}}}
	_, err := (&OpenAIGatewayService{}).forwardAsRawChatCompletions(context.Background(), c, account, []byte(`{"model":"public","reasoning_effort":"medium","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`), "")
	require.ErrorContains(t, err, "Responses API")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Disabled reasoning is rejected before any upstream call on every path.
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, rejErr := (&OpenAIGatewayService{}).forwardAsRawChatCompletions(context.Background(), c, account, []byte(`{"model":"public","reasoning_effort":"none","messages":[{"role":"user","content":"hello"}]}`), "")
	require.ErrorContains(t, rejErr, "does not support reasoning effort")

	body := []byte(`{"model":"gpt-6.1-sol","reasoning_effort":"medium","temperature":0.7,"messages":[{"role":"user","content":"hello"}]}`)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"id":"chat_1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))),
	}}
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	result, err := (&OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}).forwardAsRawChatCompletions(context.Background(), c, &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "fixture-key", "base_url": "https://api.openai.com"},
	}, body, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "medium", gjson.GetBytes(upstream.lastBody, "reasoning_effort").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "temperature").Exists())
	require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(upstream.lastBody, "model").String())
}

func TestGPT61SolCodexOAuthKeepsResponsesLite(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}
	capabilities := accountCodexToolCapabilities(account, "gpt-6.1-sol")
	require.Equal(t, "true", string(capabilities["use_responses_lite"]))
	require.Equal(t, `"code_mode_only"`, string(capabilities["tool_mode"]))
	require.Equal(t, `"freeform"`, string(capabilities["apply_patch_tool_type"]))
	require.Equal(t, `"3000"`, string(capabilities["comp_hash"]))

	apiKey := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url":      "https://api.openai.com/v1",
			"model_mapping": map[string]any{"public-sol": "gpt-6.1-sol"},
		},
	}
	adjusted, err := adjustAPIKeyCodexModelsManifest([]byte(`{"models":[{"slug":"public-sol","use_responses_lite":true}]}`), apiKey)
	require.NoError(t, err)
	require.Equal(t, false, gjson.GetBytes(adjusted, "models.0.use_responses_lite").Bool())
}

func TestGPT61SolProbeUsesGPT6Instructions(t *testing.T) {
	payload := createOpenAITestPayload("gpt-6.1-sol", true)
	instructions, _ := payload["instructions"].(string)
	require.Contains(t, instructions, "You are Codex, an agent based on GPT-6.")
	require.NotContains(t, instructions, "You are Codex, a coding agent based on GPT-5.")
	other := createOpenAITestPayload("gpt-5.4", false)
	require.Equal(t, openai.DefaultInstructions, other["instructions"])
}
