package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// usageEvent mirrors the fields of CPAMP's usage_events schema that the plugin
// can truthfully fill. The schema is copied from GET /v0/management/usage/export;
// missing fields are omitted and default to zero on the CPAMP side.
type usageEvent struct {
	RequestID      string `json:"request_id"`
	EventHash      string `json:"event_hash"`
	TimestampMS    int64  `json:"timestamp_ms"`
	Timestamp      string `json:"timestamp"`
	Provider       string `json:"provider"`
	ExecutorType   string `json:"executor_type"`
	Model          string `json:"model"`
	AnalyticsModel string `json:"analytics_model"`
	RequestedModel string `json:"requested_model"`
	ResolvedModel  string `json:"resolved_model"`
	Generate       bool   `json:"generate"`
	Stream         bool   `json:"stream"`
	Endpoint       string `json:"endpoint"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	AuthType       string `json:"auth_type"`
	Source         string `json:"source"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	TotalTokens    int64  `json:"total_tokens"`
	LatencyMS      int64  `json:"latency_ms"`
	TTFTMS         int64  `json:"ttft_ms"`
	Failed         bool   `json:"failed"`
	FailStatusCode int    `json:"fail_status_code"`
	CreatedAtMS    int64  `json:"created_at_ms"`
}

// usageReporter posts group-request usage to CPAMP's /v0/management/usage/import.
//
// The host drops usage for nested executions on both ends (outer
// markNestedExecution + inner InternalSource), so a group request otherwise
// leaves no record in CPAMP. This reporter is the only channel that makes
// group traffic visible on the CPAMP dashboard. Rules (from the qoder-plugin
// precedent):
//   - asynchronous: enqueue from the request path, never block or dial there
//   - bounded queue: overflow drops and counts, it never back-pressures
//   - only the outer call is reported: when the plugin itself is a member of
//     another plugin's group, Metadata["source"] == "plugin_host_model_callback"
//     and the outer plugin owns the report — reporting here too would
//     double-count.
type usageReporter struct {
	url      string
	keyFile  string
	client   *http.Client
	interval time.Duration

	mu          sync.Mutex
	queue       chan usageEvent
	stopped     chan struct{}
	stoppedOnce sync.Once
	dropped     int
	sinceMS     int64
}

const usageQueueCapacity = 256

func newUsageReporter(url, keyFile string, interval time.Duration) *usageReporter {
	return &usageReporter{
		url:      url,
		keyFile:  keyFile,
		client:   &http.Client{Timeout: 10 * time.Second},
		interval: interval,
		queue:    make(chan usageEvent, usageQueueCapacity),
		stopped:  make(chan struct{}),
	}
}

// enqueue adds one event; on overflow the event is dropped and counted.
func (r *usageReporter) enqueue(ev usageEvent) {
	select {
	case r.queue <- ev:
	default:
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
	}
}

// run consumes the queue until stopped. Send failures back off exponentially
// (capped) and the failed batch is dropped — the queue must never grow, and a
// dead CPAMP must not leak events forever.
func (r *usageReporter) run(log *logger, now func() time.Time) {
	backoff := r.interval
	for {
		select {
		case <-r.stopped:
			r.drain(log, now)
			return
		case <-time.After(backoff):
		}
		sent, err := r.sendBatch(log, now)
		if err != nil {
			// Keep the cadence from spinning on a dead endpoint.
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
			continue
		}
		backoff = r.interval
		_ = sent
	}
}

// sendBatch drains up to 64 events into one JSONL POST.
func (r *usageReporter) sendBatch(log *logger, now func() time.Time) (int, error) {
	var batch []usageEvent
drain:
	for len(batch) < 64 {
		select {
		case ev := <-r.queue:
			batch = append(batch, ev)
		default:
			break drain
		}
	}
	r.mu.Lock()
	dropped, since := r.dropped, r.sinceMS
	r.dropped, r.sinceMS = 0, now().UnixMilli()
	r.mu.Unlock()
	if dropped > 0 && log != nil {
		log.warn("usage 上报队列溢出，期间丢弃事件", map[string]any{"dropped": dropped, "since_ms": since})
	}
	if len(batch) == 0 {
		return 0, nil
	}

	key, err := os.ReadFile(r.keyFile)
	if err != nil {
		return 0, fmt.Errorf("读取管理密钥失败: %w", err)
	}
	var buf bytes.Buffer
	for _, ev := range batch {
		raw, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	req, err := http.NewRequest(http.MethodPost, r.url, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(key)))
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("usage import HTTP %d", resp.StatusCode)
	}
	if log != nil {
		log.debug("usage 已上报", map[string]any{"events": len(batch)})
	}
	return len(batch), nil
}

// drain flushes remaining events on stop (best effort, no retry loop).
func (r *usageReporter) drain(log *logger, now func() time.Time) {
	for {
		n, err := r.sendBatch(log, now)
		if err != nil || n == 0 {
			return
		}
	}
}

func (r *usageReporter) stop() {
	r.stoppedOnce.Do(func() { close(r.stopped) })
}

// newRequestID returns a random UUIDv4.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("cpa-router-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

func eventHash(requestID string) string {
	sum := sha256.Sum256([]byte("cpa-router:" + requestID))
	return hex.EncodeToString(sum[:])
}

// buildUsageEvent assembles one import event from a finished member attempt.
// now is injected for tests.
func buildUsageEvent(now time.Time, requestedModel, group, member string, stream bool, u tokenUsage, latency, ttft time.Duration, failed bool, failStatus int) usageEvent {
	ts := now.UTC()
	return usageEvent{
		RequestID:      newRequestID(),
		EventHash:      eventHash(newRequestID()),
		TimestampMS:    ts.UnixMilli(),
		Timestamp:      ts.Format(time.RFC3339Nano),
		Provider:       modelProviderName,
		ExecutorType:   "PluginExecutor",
		Model:          member,
		AnalyticsModel: member,
		RequestedModel: requestedModel,
		ResolvedModel:  member,
		Generate:       true,
		Stream:         stream,
		Endpoint:       "POST /v1/chat/completions",
		Method:         "POST",
		Path:           "/v1/chat/completions",
		AuthType:       "apikey",
		Source:         modelProviderName + ":" + group,
		InputTokens:    u.Input,
		OutputTokens:   u.Output,
		TotalTokens:    u.Total,
		LatencyMS:      latency.Milliseconds(),
		TTFTMS:         ttft.Milliseconds(),
		Failed:         failed,
		FailStatusCode: failStatus,
		CreatedAtMS:    ts.UnixMilli(),
	}
}

// isNestedExecution reports whether this executor call is a nested one (the
// plugin is itself a member of another plugin's group). The host sets
// Metadata["source"] = "plugin_host_model_callback" only on internal calls.
func isNestedExecution(meta map[string]any) bool {
	v, ok := meta["source"]
	if !ok {
		return false
	}
	s, _ := v.(string)
	return s == "plugin_host_model_callback"
}

// metadataString reads a string-valued metadata key.
func metadataString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	v, ok := meta[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// reportUsage enqueues one event when reporting is enabled. Called from the
// request path; it never blocks.
func (p *plugin) reportUsage(requestedModel, group, member string, stream bool, u tokenUsage, latency, ttft time.Duration, failed bool, failStatus int) {
	p.mu.RLock()
	reporter := p.usageReporter
	nowFn := p.rt.now
	p.mu.RUnlock()
	if reporter == nil {
		return
	}
	reporter.enqueue(buildUsageEvent(nowFn(), requestedModel, group, member, stream, u, latency, ttft, failed, failStatus))
}
