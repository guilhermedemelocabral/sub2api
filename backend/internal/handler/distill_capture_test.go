package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/distill"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const distillChatToolSSE = "data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n"

const distillChatRequestBody = `{"model":"gpt-5","messages":[{"role":"user","content":"oi"}]}`

func TestDistillCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("nil collector leaves context without key", func(t *testing.T) {
		c := newDistillCaptureContext(t)
		installDistillTap(c, nil, distill.ProtocolChatCompletions)
		require.Nil(t, c.Request.Context().Value(distill.ContextKey))
	})

	t.Run("enabled collector enqueues assistant tool call", func(t *testing.T) {
		dir := t.TempDir()
		collector := newDistillCaptureCollector(t, dir)
		c := newDistillCaptureContext(t)
		installDistillTap(c, collector, distill.ProtocolChatCompletions)

		_, err := c.Writer.Write([]byte(distillChatToolSSE))
		require.NoError(t, err)
		captureDistill(c, collector, distill.ProtocolChatCompletions, false, "req_1", "gpt-5", true, 7, []byte(distillChatRequestBody))

		require.NoError(t, collector.Close())
		lines := readDistillJSONL(t, dir)
		require.Len(t, lines, 1)

		var turn distill.Turn
		require.NoError(t, json.Unmarshal(bytes.TrimRight(lines[0], "\n"), &turn))
		require.Len(t, turn.Assistant.ToolCalls, 1)
		require.Equal(t, "bash", turn.Assistant.ToolCalls[0].Function.Name)
	})

	t.Run("client disconnect enqueues nothing", func(t *testing.T) {
		dir := t.TempDir()
		collector := newDistillCaptureCollector(t, dir)
		c := newDistillCaptureContext(t)
		installDistillTap(c, collector, distill.ProtocolChatCompletions)

		_, err := c.Writer.Write([]byte(distillChatToolSSE))
		require.NoError(t, err)
		captureDistill(c, collector, distill.ProtocolChatCompletions, true, "req_1", "gpt-5", true, 7, []byte(distillChatRequestBody))

		require.NoError(t, collector.Close())
		require.Empty(t, readDistillJSONL(t, dir))
	})

	t.Run("disabled collector leaves context without key", func(t *testing.T) {
		collector := distill.NewCollector(distill.CollectorConfig{
			Enabled:      false,
			Dir:          t.TempDir(),
			DriveEnabled: false,
		})
		c := newDistillCaptureContext(t)
		installDistillTap(c, collector, distill.ProtocolChatCompletions)
		require.Nil(t, c.Request.Context().Value(distill.ContextKey))
	})

	t.Run("missing tap enqueues nothing", func(t *testing.T) {
		dir := t.TempDir()
		collector := newDistillCaptureCollector(t, dir)
		c := newDistillCaptureContext(t)
		captureDistill(c, collector, distill.ProtocolChatCompletions, false, "req_1", "gpt-5", true, 7, []byte(distillChatRequestBody))
		require.NoError(t, collector.Close())
		require.Empty(t, readDistillJSONL(t, dir))
	})

	t.Run("assistant without text or tool call enqueues nothing", func(t *testing.T) {
		dir := t.TempDir()
		collector := newDistillCaptureCollector(t, dir)
		c := newDistillCaptureContext(t)
		installDistillTap(c, collector, distill.ProtocolChatCompletions)
		_, err := c.Writer.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"delta\":{}}]}\n"))
		require.NoError(t, err)
		captureDistill(c, collector, distill.ProtocolChatCompletions, false, "req_1", "gpt-5", true, 7, []byte(distillChatRequestBody))
		require.NoError(t, collector.Close())
		require.Empty(t, readDistillJSONL(t, dir))
	})

	t.Run("normalize error drops without enqueue", func(t *testing.T) {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		dir := t.TempDir()
		collector := newDistillCaptureCollector(t, dir)
		c := newDistillCaptureContext(t)
		installDistillTap(c, collector, distill.ProtocolChatCompletions)
		_, err := c.Writer.Write([]byte(distillChatToolSSE))
		require.NoError(t, err)
		captureDistill(c, collector, "not-a-protocol", false, "req_1", "gpt-5", true, 7, []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"SENTINEL_BODY"}]}`))
		require.NoError(t, collector.Close())
		require.Empty(t, readDistillJSONL(t, dir))
		logged := buf.String()
		require.Contains(t, logged, "distill.turn_dropped")
		require.Contains(t, logged, `"reason":"normalize"`)
		require.NotContains(t, logged, "SENTINEL_BODY")
	})
}

func newDistillCaptureContext(t *testing.T) *gin.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func newDistillCaptureCollector(t *testing.T, dir string) *distill.Collector {
	t.Helper()
	collector := distill.NewCollector(distill.CollectorConfig{
		Enabled:      true,
		Dir:          dir,
		QueueSize:    8,
		QueueBytes:   1 << 20,
		PartBytes:    1 << 20,
		DriveEnabled: false,
	})
	require.True(t, collector.Enabled())
	return collector
}

func readDistillJSONL(t *testing.T, dir string) [][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var lines [][]byte
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)
		for _, line := range bytes.Split(body, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			lines = append(lines, line)
		}
	}
	return lines
}
