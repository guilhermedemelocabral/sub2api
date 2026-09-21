//go:build unit

package service

import "testing"

func TestClassifyErrorTriage(t *testing.T) {
	cases := []struct {
		name           string
		phase          string
		errType        string
		status         int
		upstreamStatus int
		message        string
		upstreamMsg    string
		want           string
	}{
		{"upstream 401", "upstream", "upstream_error", 401, 401, "Upstream error: 401", "", TriageUpstreamAuth},
		{"upstream 403", "upstream", "upstream_error", 403, 403, "forbidden", "", TriageUpstreamAuth},
		{"upstream 404", "upstream", "upstream_error", 404, 404, "Upstream error: 404", "", TriageUpstreamNotFound},
		{"upstream 400 validation", "upstream", "upstream_error", 400, 400, "Upstream error: 400", "invalid content block", TriageUpstreamValidation},
		{"upstream 503 server", "upstream", "upstream_error", 503, 503, "service unavailable", "", TriageUpstreamServer},
		{"upstream status fallback when no attempt", "upstream", "upstream_error", 500, 0, "connection reset", "", TriageUpstreamServer},
		{"model not found routing", "routing", "model_not_found", 404, 0, "model not found", "", TriageRoutingNotFound},
		{"group availability message", "routing", "routing_error", 404, 0, `Model "glm-5.3-flash" is not available for this group`, "", TriageRoutingNotFound},
		{"routing breaker 503", "routing", "routing_error", 503, 0, "no healthy accounts", "", TriageRoutingBreaker},
		{"timeout message", "upstream", "upstream_error", 504, 0, "upstream timeout after 30s", "", TriageUpstreamTimeout},
		{"deadline message", "upstream", "upstream_error", 500, 500, "context deadline exceeded", "", TriageUpstreamTimeout},
		{"truncated stream", "response", "stream_error", 200, 0, "response truncated at max tokens", "", TriageStreamTruncated},
		{"unknown stays unclassified", "auth", "auth_error", 401, 0, "invalid api key", "", TriageUnclassified},
		{"empty input", "", "", 0, 0, "", "", TriageUnclassified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyErrorTriage(tc.phase, tc.errType, tc.status, tc.upstreamStatus, tc.message, tc.upstreamMsg); got != tc.want {
				t.Fatalf("ClassifyErrorTriage(%q,%q,%d,%d,%q,%q) = %q, want %q",
					tc.phase, tc.errType, tc.status, tc.upstreamStatus, tc.message, tc.upstreamMsg, got, tc.want)
			}
		})
	}
}
