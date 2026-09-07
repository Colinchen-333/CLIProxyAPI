package helps

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// StreamTiming separates local preparation from upstream wait without recording
// bodies, credentials, cache keys, or identity fields. It currently targets Muse.
type StreamTiming struct {
	ctx         context.Context
	started     time.Time
	prepared    time.Time
	model       string
	inputBytes  int
	outputBytes int
	cacheKey    bool
}

func NewStreamTiming(ctx context.Context, model string, inputBytes int) *StreamTiming {
	if !strings.HasPrefix(strings.ToLower(model), "muse-") {
		return nil
	}
	return &StreamTiming{ctx: ctx, started: time.Now(), model: model, inputBytes: inputBytes}
}

func (t *StreamTiming) Prepared(outputBytes int, cacheKey bool) {
	if t == nil {
		return
	}
	t.outputBytes, t.cacheKey, t.prepared = outputBytes, cacheKey, time.Now()
}

func (t *StreamTiming) Response(status int, failed bool) {
	if t == nil {
		return
	}
	now := time.Now()
	data, _ := json.Marshal(map[string]any{
		"model": t.model, "input_bytes": t.inputBytes, "upstream_bytes": t.outputBytes,
		"prepare_ms":        float64(t.prepared.Sub(t.started).Microseconds()) / 1000,
		"wait_headers_ms":   float64(now.Sub(t.prepared).Microseconds()) / 1000,
		"cache_key_present": t.cacheKey, "status": status, "transport_error": failed,
	})
	LogWithRequestID(t.ctx).Info("muse upstream timing " + string(data))
}
