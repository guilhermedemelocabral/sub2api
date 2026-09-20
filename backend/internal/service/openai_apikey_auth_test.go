package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// NOTE (go1.27 rodata hazard, see docs/GO_TOOLCHAIN_NOTES.md): never spell a
// full "Bearer <key>" expectation literal next to the key literal in these
// tests — build it at runtime from the account credentials map instead.

func TestGetOpenAIAPIKeyAuthScheme(t *testing.T) {
	t.Parallel()

	require.Equal(t, OpenAIAPIKeyAuthSchemeAuthorizationBearer, (*Account)(nil).GetOpenAIAPIKeyAuthScheme())

	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.Equal(t, OpenAIAPIKeyAuthSchemeAuthorizationBearer, oauth.GetOpenAIAPIKeyAuthScheme())

	plain := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.Equal(t, OpenAIAPIKeyAuthSchemeAuthorizationBearer, plain.GetOpenAIAPIKeyAuthScheme(),
		"missing extra keeps historical Bearer")

	optIn := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{openaiAPIKeyAuthSchemeExtraKey: OpenAIAPIKeyAuthSchemeXAPIKey}}
	require.Equal(t, OpenAIAPIKeyAuthSchemeXAPIKey, optIn.GetOpenAIAPIKeyAuthScheme())

	invalid := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Extra: map[string]any{openaiAPIKeyAuthSchemeExtraKey: "bogus"}}
	require.Equal(t, OpenAIAPIKeyAuthSchemeAuthorizationBearer, invalid.GetOpenAIAPIKeyAuthScheme())

	otherPlatform := &Account{Platform: PlatformGrok, Type: AccountTypeAPIKey,
		Extra: map[string]any{openaiAPIKeyAuthSchemeExtraKey: OpenAIAPIKeyAuthSchemeXAPIKey}}
	require.Equal(t, OpenAIAPIKeyAuthSchemeAuthorizationBearer, otherPlatform.GetOpenAIAPIKeyAuthScheme(),
		"non-OpenAI, non-multiprotocol platforms keep Bearer")
}

func openAIAPIKeyAuthTestAccount(extra map[string]any) *Account {
	return &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "test-key",
			"base_url": "https://infer.flatt.com.br/v1",
		},
		Extra: extra,
	}
}

func TestSetOpenAIAPIKeyAuthHeader(t *testing.T) {
	t.Parallel()

	optIn := openAIAPIKeyAuthTestAccount(map[string]any{openaiAPIKeyAuthSchemeExtraKey: OpenAIAPIKeyAuthSchemeXAPIKey})
	header := http.Header{}
	setOpenAIAPIKeyAuthHeader(header, optIn, optIn.GetCredential("api_key"))
	require.Equal(t, optIn.GetCredential("api_key"), header.Get("x-api-key"))
	require.Empty(t, header.Get("Authorization"))

	plain := openAIAPIKeyAuthTestAccount(nil)
	header = http.Header{}
	setOpenAIAPIKeyAuthHeader(header, plain, plain.GetCredential("api_key"))
	require.Equal(t, "Bearer "+plain.GetCredential("api_key"), header.Get("Authorization"))
	require.Empty(t, header.Get("x-api-key"))

	header = http.Header{}
	setOpenAIAPIKeyAuthHeader(header, nil, plain.GetCredential("api_key"))
	require.Equal(t, "Bearer "+plain.GetCredential("api_key"), header.Get("Authorization"))
}

func TestBuildOpenAIAPIKeyModelsRequestHonorsAuthScheme(t *testing.T) {
	t.Parallel()

	validate := func(raw string) (string, error) { return raw, nil }

	plain := openAIAPIKeyAuthTestAccount(nil)
	plainReq, err := buildOpenAIAPIKeyModelsRequest(context.Background(), plain, validate)
	require.NoError(t, err)
	require.Equal(t, "Bearer "+plain.GetCredential("api_key"), plainReq.Header.Get("Authorization"))
	require.Empty(t, plainReq.Header.Get("x-api-key"))

	opted := openAIAPIKeyAuthTestAccount(map[string]any{openaiAPIKeyAuthSchemeExtraKey: OpenAIAPIKeyAuthSchemeXAPIKey})
	xkeyReq, err := buildOpenAIAPIKeyModelsRequest(context.Background(), opted, validate)
	require.NoError(t, err)
	require.Equal(t, opted.GetCredential("api_key"), xkeyReq.Header.Get("x-api-key"))
	require.Empty(t, xkeyReq.Header.Get("Authorization"),
		"Flatt 401s any request carrying Authorization: Bearer, so it must be absent")
}
