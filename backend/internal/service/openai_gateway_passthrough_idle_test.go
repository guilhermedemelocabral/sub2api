package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// idleTestBody emits one optional chunk, then blocks until closed (or a long
// fallback so a broken watchdog fails the test instead of hanging it).
type idleTestBody struct {
	mu     sync.Mutex
	chunk  string
	sent   bool
	closed chan struct{}
	once   sync.Once
}

func newIdleTestBody(chunk string) *idleTestBody {
	return &idleTestBody{chunk: chunk, sent: chunk == "", closed: make(chan struct{})}
}

func (b *idleTestBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if !b.sent {
		b.sent = true
		n := copy(p, b.chunk)
		b.mu.Unlock()
		return n, nil
	}
	ch := b.closed
	b.mu.Unlock()
	select {
	case <-ch:
		return 0, errors.New("body closed")
	case <-time.After(10 * time.Second):
		return 0, io.EOF
	}
}

func (b *idleTestBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func runPassthroughIdleTest(t *testing.T, body io.ReadCloser, intervalSec int) (*httptest.ResponseRecorder, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	svc := &OpenAIGatewayService{cfg: &config.Config{
		Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize, StreamDataIntervalTimeout: intervalSec},
	}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	_, err := svc.handleStreamingResponsePassthrough(
		context.Background(), resp, c,
		&Account{ID: 1, Platform: PlatformOpenAI, Name: "idle-test"},
		time.Now(), "idle-model", "idle-model",
	)
	return recorder, err
}

func TestPassthroughIdleTimeout_PreOutputFailsOver(t *testing.T) {
	recorder, err := runPassthroughIdleTest(t, newIdleTestBody(""), 1)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr, "pre-output stall must be failover-eligible, got: %v", err)
	require.True(t, failoverErr.ShouldRetryNextAccount(), "pre-output stall must retry next account")
	_ = recorder
}

func TestPassthroughIdleTimeout_PostOutputHardError(t *testing.T) {
	chunk := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	recorder, err := runPassthroughIdleTest(t, newIdleTestBody(chunk), 1)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr, "post-output stall cannot failover, got: %v", err)
	require.ErrorIs(t, err, errStreamDataIntervalTimeout)
	require.Contains(t, recorder.Body.String(), "hi")
}

func TestPassthroughIdleTimeout_FastStreamUnaffected(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	recorder, err := runPassthroughIdleTest(t, io.NopCloser(strings.NewReader(body)), 1)
	require.NoError(t, err)
	require.Contains(t, recorder.Body.String(), "hi")
}
