package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeKeyFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "key")
	if err := os.WriteFile(p, []byte("test-admin-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestUsageReporterPostsJSONL verifies the wire format: one NDJSON line per
// event, Bearer auth from the key file, and the CPAMP event schema fields.
func TestUsageReporterPostsJSONL(t *testing.T) {
	dir := t.TempDir()
	keyFile := writeKeyFile(t, dir)

	var (
		mu    sync.Mutex
		auth  string
		ct    string
		lines []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		mu.Lock()
		auth = r.Header.Get("Authorization")
		ct = r.Header.Get("Content-Type")
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			var ev map[string]any
			json.Unmarshal([]byte(line), &ev)
			lines = append(lines, ev)
		}
		mu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte(`{"added":1,"skipped":0}`))
	}))
	defer srv.Close()

	r := newUsageReporter(srv.URL, keyFile, 10*time.Millisecond)
	go r.run(nil, func() time.Time { return time.Now() })
	now := time.Now()
	r.enqueue(buildUsageEvent(now, "GLM", "GLM", "sn/glm-5.2", false, tokenUsage{Input: 10, Output: 5, Total: 15}, time.Second, time.Second, false, 0))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := len(lines)
		mu.Unlock()
		if got == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.stop()

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 {
		t.Fatalf("imported lines = %d, want 1", len(lines))
	}
	if auth != "Bearer test-admin-key" {
		t.Fatalf("Authorization = %q", auth)
	}
	if !strings.Contains(ct, "ndjson") {
		t.Fatalf("Content-Type = %q", ct)
	}
	ev := lines[0]
	for _, k := range []string{"request_id", "event_hash", "timestamp_ms", "provider", "model", "requested_model", "source", "input_tokens", "output_tokens", "total_tokens", "latency_ms", "failed"} {
		if _, ok := ev[k]; !ok {
			t.Fatalf("event missing %q: %v", k, ev)
		}
	}
	if ev["provider"] != "cpa-router" || ev["model"] != "sn/glm-5.2" || ev["requested_model"] != "GLM" {
		t.Fatalf("event identity wrong: %v", ev)
	}
	if ev["source"] != "cpa-router:GLM" {
		t.Fatalf("source = %v, want cpa-router:GLM", ev["source"])
	}
}

// TestUsageReporterBoundedQueue verifies overflow drops instead of blocking.
func TestUsageReporterBoundedQueue(t *testing.T) {
	dir := t.TempDir()
	r := newUsageReporter("http://127.0.0.1:1/nope", writeKeyFile(t, dir), time.Hour)
	ev := buildUsageEvent(time.Now(), "g", "g", "m", false, tokenUsage{}, 0, 0, false, 0)
	for i := 0; i < usageQueueCapacity+50; i++ {
		r.enqueue(ev) // must never block
	}
	r.mu.Lock()
	dropped := r.dropped
	r.mu.Unlock()
	if dropped != 50 {
		t.Fatalf("dropped = %d, want 50", dropped)
	}
}

// TestIsNestedExecution verifies the double-count guard: only calls with
// Metadata["source"] == "plugin_host_model_callback" are nested.
func TestIsNestedExecution(t *testing.T) {
	cases := []struct {
		meta map[string]any
		want bool
	}{
		{nil, false},
		{map[string]any{}, false},
		{map[string]any{"source": "other"}, false},
		{map[string]any{"requested_model": "GLM"}, false},
		{map[string]any{"source": "plugin_host_model_callback", "requested_model": "GLM"}, true},
	}
	for _, c := range cases {
		if got := isNestedExecution(c.meta); got != c.want {
			t.Fatalf("isNestedExecution(%v) = %v, want %v", c.meta, got, c.want)
		}
	}
}

// TestConfigureUsageReporterLifecycle verifies the reporter starts when
// configured, stops and restarts on reconfigure, and stays nil when not
// configured.
func TestConfigureUsageReporterLifecycle(t *testing.T) {
	dir := t.TempDir()
	keyFile := writeKeyFile(t, dir)
	h := newFakeHost()
	p := newPlugin(h)

	// Not configured: reporter stays nil, configure succeeds.
	if err := p.configure(mustLifecycleJSON(t, "")); err != nil {
		t.Fatal(err)
	}
	if p.usageReporter != nil {
		t.Fatal("reporter should be nil when unconfigured")
	}

	// Configured with unreachable URL: reporter runs but must not block.
	cfg := "usage_report_url: http://127.0.0.1:1/import\nusage_report_key_file: " + keyFile + "\n"
	if err := p.configure(mustLifecycleJSON(t, cfg)); err != nil {
		t.Fatal(err)
	}
	p.mu.RLock()
	first := p.usageReporter
	p.mu.RUnlock()
	if first == nil {
		t.Fatal("reporter should be set when configured")
	}

	// Reconfigure: old reporter stops, new one takes its place.
	if err := p.configure(mustLifecycleJSON(t, cfg)); err != nil {
		t.Fatal(err)
	}
	p.mu.RLock()
	second := p.usageReporter
	p.mu.RUnlock()
	if second == nil || second == first {
		t.Fatal("reconfigure must replace the reporter")
	}
	p.shutdown()
}

// TestValidateUsageReportConfigPairs verifies the url/key-file pairing rule.
func TestValidateUsageReportConfigPairs(t *testing.T) {
	_, err := decodeConfig([]byte("usage_report_url: http://x/y\n"))
	if err == nil || !strings.Contains(err.Error(), "usage_report_key_file") {
		t.Fatalf("url without key file should fail, got %v", err)
	}
	_, err = decodeConfig([]byte("usage_report_key_file: /tmp/k\n"))
	if err == nil || !strings.Contains(err.Error(), "usage_report_url") {
		t.Fatalf("key file without url should fail, got %v", err)
	}
	if _, err := decodeConfig([]byte("")); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
}

func mustLifecycleJSON(t *testing.T, cfgYAML string) []byte {
	t.Helper()
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte(cfgYAML)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
