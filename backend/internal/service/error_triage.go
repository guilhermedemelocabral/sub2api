package service

import "strings"

// In-gateway error triage ("JEV inside"): every persisted gateway failure
// gets one stable class so operators can separate WHAT failed from HOW it
// failed without inventing ad-hoc buckets per incident.
//
// Classes mirror docs/JEV_LOG_TRIAGE.md. Anything unmatched lands on
// TriageUnclassified WITH full context preserved, so new failure modes stay
// visible instead of silently merging into a generic bucket.
const (
	TriageUpstreamAuth       = "upstream_auth"
	TriageUpstreamNotFound   = "upstream_not_found"
	TriageUpstreamValidation = "upstream_validation"
	TriageUpstreamServer     = "upstream_server"
	TriageUpstreamTimeout    = "upstream_timeout"
	TriageRoutingNotFound    = "routing_not_found"
	TriageRoutingBreaker     = "routing_breaker"
	TriageStreamTruncated    = "stream_truncated"
	TriageUnclassified       = "unclassified"
)

// ClassifyErrorTriage maps persisted error fields to one triage class.
// phase/type are the stored error_phase/error_type values, status the
// client-facing status, upstreamStatus the provider status (0 when no
// upstream attempt was made), and the messages the stored error texts.
func ClassifyErrorTriage(phase, errType string, status, upstreamStatus int, message, upstreamMsg string) string {
	phase = strings.ToLower(strings.TrimSpace(phase))
	errType = strings.ToLower(strings.TrimSpace(errType))
	combined := strings.ToLower(message + "\n" + upstreamMsg)

	if strings.Contains(combined, "truncat") {
		return TriageStreamTruncated
	}
	if strings.Contains(combined, "timeout") || strings.Contains(combined, "deadline") {
		return TriageUpstreamTimeout
	}
	if errType == "model_not_found" || strings.Contains(combined, "not available for this group") ||
		strings.Contains(combined, "not supported by any") {
		return TriageRoutingNotFound
	}
	if phase == "routing" && status == 503 {
		return TriageRoutingBreaker
	}
	switch upstreamStatus {
	case 401, 403:
		return TriageUpstreamAuth
	case 400:
		return TriageUpstreamValidation
	case 404:
		return TriageUpstreamNotFound
	}
	if upstreamStatus >= 500 {
		return TriageUpstreamServer
	}
	if phase == "upstream" {
		switch status {
		case 401, 403:
			return TriageUpstreamAuth
		case 400:
			return TriageUpstreamValidation
		case 404:
			return TriageUpstreamNotFound
		}
		if status >= 500 {
			return TriageUpstreamServer
		}
	}
	return TriageUnclassified
}
