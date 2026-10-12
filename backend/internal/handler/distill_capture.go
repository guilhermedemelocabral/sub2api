package handler

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/distill"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func installDistillTap(c *gin.Context, collector *distill.Collector, protocol string) {
	if c == nil || c.Request == nil || collector == nil || !collector.Enabled() {
		return
	}
	tap := distill.NewTap(protocol)
	c.Writer = tap.Wrap(c.Writer)
	c.Set(distill.GinKey, tap)
}

func captureDistill(c *gin.Context, collector *distill.Collector, protocol string, clientDisconnect bool, requestID, model string, stream bool, apiKeyID int64, body []byte) {
	if collector == nil || !collector.Enabled() || clientDisconnect {
		return
	}
	tap := distillTapFrom(c)
	if tap == nil {
		return
	}
	assistant, responseID, ok := tap.Finish()
	if !ok {
		return
	}
	line, err := distill.Normalize(distill.Input{
		At:          time.Now().UTC(),
		RequestID:   requestID,
		SessionID:   service.ExtractClientSessionID(c),
		ResponseID:  responseID,
		APIKeyID:    apiKeyID,
		Protocol:    protocol,
		Model:       model,
		Stream:      stream,
		RequestBody: body,
		Assistant:   assistant,
	})
	if err != nil {
		collector.NoteNormalizeDrop()
		return
	}
	collector.Enqueue(append([]byte(nil), line...))
}

func distillTapFrom(c *gin.Context) *distill.Tap {
	if c == nil {
		return nil
	}
	tap, _ := c.Get(distill.GinKey)
	typed, _ := tap.(*distill.Tap)
	return typed
}
