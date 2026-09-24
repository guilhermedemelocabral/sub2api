//go:build unit

package service

import (
	"strings"
	"testing"
)

func TestTruncateUpstreamBadRequestBody(t *testing.T) {
	small := []byte(`{"model":"m"}`)
	if got := truncateUpstreamBadRequestBody(small); got != string(small) {
		t.Fatalf("small body must pass through untouched, got %q", got)
	}
	big := []byte(strings.Repeat("x", maxUpstreamBadRequestBodyLogBytes+100))
	got := truncateUpstreamBadRequestBody(big)
	if len(got) <= maxUpstreamBadRequestBodyLogBytes {
		t.Fatalf("expected truncation marker past the bound, len=%d", len(got))
	}
	if !strings.HasSuffix(got, "\n...[truncated upstream request body]\n") {
		t.Fatalf("missing explicit truncation marker: %q", got[len(got)-40:])
	}
}

func TestSummarizeUpstreamBadRequestBody(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"role":"system","content":[{"type":"input_text","text":"s"}]},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"user","content":""},` +
		`{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"out"}` +
		`],"tools":[{"type":"function","name":"read"}]}`)
	got := summarizeUpstreamBadRequestBody(body)
	for _, want := range []string{
		"input_items=5",
		"/systemx1",
		"/userx2",
		"function_call/x1",
		"function_call_output/x1",
		"stringx1",
		"empty_stringx1",
		"arrayx1",
		"input_textx1",
		"Nullx2",
		"tools=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q missing %q", got, want)
		}
	}
	nonArray := summarizeUpstreamBadRequestBody([]byte(`{"model":"m","input":"hi"}`))
	if !strings.Contains(nonArray, "input=String") {
		t.Fatalf("string input must be reported as-is: %q", nonArray)
	}
}
