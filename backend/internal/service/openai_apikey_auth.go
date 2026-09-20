package service

import (
	"net/http"
	"strings"
)

const (
	openaiAPIKeyAuthSchemeExtraKey = "openai_apikey_auth_scheme"

	OpenAIAPIKeyAuthSchemeAuthorizationBearer = "authorization_bearer"
	OpenAIAPIKeyAuthSchemeXAPIKey             = "x_api_key"
)

// GetOpenAIAPIKeyAuthScheme returns the upstream authentication scheme for
// OpenAI-protocol API-key accounts. Missing or invalid values keep the
// historical Authorization: Bearer behavior. Generic OpenAI-compat upstreams
// that only accept x-api-key (e.g. Flatt, which 401s any Bearer) opt in via
// extra["openai_apikey_auth_scheme"] = "x_api_key".
func (a *Account) GetOpenAIAPIKeyAuthScheme() string {
	if a == nil || a.Type != AccountTypeAPIKey {
		return OpenAIAPIKeyAuthSchemeAuthorizationBearer
	}
	if a.Platform != PlatformOpenAI && !a.IsMultiProtocolAPIKey() {
		return OpenAIAPIKeyAuthSchemeAuthorizationBearer
	}

	switch strings.TrimSpace(a.GetExtraString(openaiAPIKeyAuthSchemeExtraKey)) {
	case OpenAIAPIKeyAuthSchemeXAPIKey:
		return OpenAIAPIKeyAuthSchemeXAPIKey
	default:
		return OpenAIAPIKeyAuthSchemeAuthorizationBearer
	}
}

// setOpenAIAPIKeyAuthHeader writes the upstream auth header for
// OpenAI-protocol API-key calls (forward, models sync, probes, embeddings).
// Non-API-key accounts and accounts without the opt-in keep Bearer.
func setOpenAIAPIKeyAuthHeader(header http.Header, account *Account, token string) {
	if account != nil && account.GetOpenAIAPIKeyAuthScheme() == OpenAIAPIKeyAuthSchemeXAPIKey {
		header.Set("x-api-key", token)
		return
	}
	header.Set("Authorization", "Bearer "+token)
}
