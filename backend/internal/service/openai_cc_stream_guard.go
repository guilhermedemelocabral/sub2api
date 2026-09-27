package service

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
)

// ccStreamHeaderTimeout bounds the wait for response headers on streamed Chat
// Completions. Stream upstreams answer headers before generating, so a long
// silence means the provider accepted the socket and hung; non-stream bodies
// only arrive after the whole generation and are left unbounded.
func (s *OpenAIGatewayService) ccStreamHeaderTimeout(account *Account, stream bool, body []byte) time.Duration {
	if !stream || account == nil || account.Platform == PlatformGrok {
		return 0
	}
	return s.openAIFirstOutputTimeout(gjson.GetBytes(body, "reasoning_effort").String())
}

func (s *OpenAIGatewayService) ccStreamIdleTimeout(account *Account, stream bool) time.Duration {
	if !stream || account == nil || account.Platform == PlatformGrok {
		return 0
	}
	if s == nil || s.cfg == nil || s.cfg.Gateway.StreamDataIntervalTimeout <= 0 {
		return 0
	}
	return time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
}

// ccStreamIdleReadCloser closes a stalled upstream body so the SSE scanner
// returns errStreamDataIntervalTimeout instead of blocking until the provider
// drops the socket. Reasoning deltas stream continuously, so only a full
// silence for the whole interval trips it.
type ccStreamIdleReadCloser struct {
	io.ReadCloser
	accountID int64
	interval  time.Duration
	lastRead  atomic.Int64
	timedOut  atomic.Bool
	done      chan struct{}
	stopOnce  sync.Once
}

func newCCStreamIdleReadCloser(body io.ReadCloser, accountID int64, interval time.Duration) *ccStreamIdleReadCloser {
	r := &ccStreamIdleReadCloser{ReadCloser: body, accountID: accountID, interval: interval, done: make(chan struct{})}
	r.lastRead.Store(time.Now().UnixNano())
	tick := interval / 4
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	if tick > time.Minute {
		tick = time.Minute
	}
	go r.watch(tick)
	return r
}

func (r *ccStreamIdleReadCloser) watch(tick time.Duration) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, r.lastRead.Load())) < r.interval {
				continue
			}
			r.timedOut.Store(true)
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI chat completions] upstream stream data interval timeout: account=%d interval=%s", r.accountID, r.interval)
			_ = r.ReadCloser.Close()
			return
		}
	}
}

func (r *ccStreamIdleReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.lastRead.Store(time.Now().UnixNano())
	}
	if err != nil && err != io.EOF && r.timedOut.Load() {
		return n, errStreamDataIntervalTimeout
	}
	return n, err
}

func (r *ccStreamIdleReadCloser) Close() error {
	r.stopOnce.Do(func() { close(r.done) })
	return r.ReadCloser.Close()
}

func (r *ccStreamIdleReadCloser) TimedOut() bool {
	return r != nil && r.timedOut.Load()
}
