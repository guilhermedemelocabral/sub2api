package antigravity

import "testing"

func TestIsHealableConversationalTurn(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"", true},
		{"STOP", true},
		{"END_TURN", true},
		{"stop", true},
		{"end_turn", true},
		{"  STOP  ", true},
		{"MALFORMED_FUNCTION_CALL", false},
		{"malformed_function_call", false},
		{"MAX_TOKENS", false},
		{"max_tokens", false},
		{"SAFETY", false},
		{"safety", false},
		{"CONTENT_FILTER", false},
		{"REFUSAL", false},
		{"BLOCKLIST", false},
		{"RECITATION", false},
		{"POLICY_VIOLATION", false},
		{"TIMEOUT", false},
		{"DEADLINE_EXCEEDED", false},
		{"SOME_FUTURE_REASON", false},
	}
	for _, c := range cases {
		if got := IsHealableConversationalTurn(c.reason); got != c.want {
			t.Errorf("healable reason mismatch: input=%q got=%v want=%v", c.reason, got, c.want)
		}
	}
}

func TestIsUpstreamSafetyRefusal(t *testing.T) {
	refusals := []string{"SAFETY", "safety", "CONTENT_FILTER", "content_filter", "REFUSAL", "refusal", "BLOCKLIST", "RECITATION", "POLICY_VIOLATION", "HARM_BLOCKLIST", "PROHIBITED_CONTENT"}
	for _, r := range refusals {
		if !IsUpstreamSafetyRefusal(r) {
			t.Errorf("expected refusal for input=%q", r)
		}
	}
	nonRefusals := []string{"", "STOP", "END_TURN", "MAX_TOKENS", "MALFORMED_FUNCTION_CALL", "TIMEOUT"}
	for _, r := range nonRefusals {
		if IsUpstreamSafetyRefusal(r) {
			t.Errorf("unexpected refusal for input=%q", r)
		}
	}
}

func TestNormalizeFinishReason(t *testing.T) {
	if got := NormalizeFinishReason("  stop "); got != "STOP" {
		t.Errorf("normalize mismatch: got=%q", got)
	}
	if got := NormalizeFinishReason(""); got != "" {
		t.Errorf("normalize empty mismatch: got=%q", got)
	}
}
