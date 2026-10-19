package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ---- payload parsing ----

// Upstreams report the same counters under different keys depending on the
// protocol the client used; all three shapes must normalize to input/output/total.
func TestExtractUsageShapes(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    tokenUsage
	}{
		{
			name:    "openai non-stream body",
			payload: `{"id":"x","usage":{"prompt_tokens":41,"completion_tokens":16,"total_tokens":57}}`,
			want:    tokenUsage{Input: 41, Output: 16, Total: 57},
		},
		{
			name:    "openai stream chunk with data prefix",
			payload: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\ndata: [DONE]\n\n",
			want:    tokenUsage{Input: 10, Output: 2, Total: 12},
		},
		{
			name:    "openai stream chunk without prefix",
			payload: `{"choices":[{"delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
			want:    tokenUsage{Input: 7, Output: 3, Total: 10},
		},
		{
			name:    "claude usage",
			payload: `{"type":"message","usage":{"input_tokens":100,"output_tokens":20}}`,
			want:    tokenUsage{Input: 100, Output: 20, Total: 120},
		},
		{
			name:    "claude streaming message_start",
			payload: `{"type":"message_start","message":{"usage":{"input_tokens":33,"output_tokens":1}}}`,
			want:    tokenUsage{Input: 33, Output: 1, Total: 34},
		},
		{
			name:    "gemini usageMetadata",
			payload: `{"candidates":[],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":6,"totalTokenCount":11}}`,
			want:    tokenUsage{Input: 5, Output: 6, Total: 11},
		},
		{
			name:    "last frame with usage wins",
			payload: "{\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n{\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}",
			want:    tokenUsage{Input: 9, Output: 4, Total: 13},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractUsage([]byte(tc.payload))
			if !ok {
				t.Fatalf("extractUsage(%q) reported no usage", tc.payload)
			}
			if got != tc.want {
				t.Fatalf("extractUsage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Payloads without usage must not invent one, and must never panic: usage is
// best-effort telemetry on the hot path.
func TestExtractUsageIgnoresNonUsagePayloads(t *testing.T) {
	for _, payload := range []string{
		"",
		"[DONE]",
		`{"choices":[{"delta":{"content":"hi"}}],"usage":null}`,
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		": heartbeat",
		"not json at all",
		`{"usage":{}}`,
	} {
		if u, ok := extractUsage([]byte(payload)); ok {
			t.Errorf("extractUsage(%q) = %+v, want no usage", payload, u)
		}
	}
}

// Streaming providers split one request's counters across frames (claude puts
// input tokens in message_start and output tokens in message_delta). Summing the
// frames would double-count; keeping only the last would lose the input count.
func TestMergeTokenUsageAcrossFrames(t *testing.T) {
	acc := tokenUsage{}
	acc = mergeTokenUsage(acc, tokenUsage{Input: 100, Output: 1, Total: 101})
	acc = mergeTokenUsage(acc, tokenUsage{Output: 25, Total: 26})
	if acc.Input != 100 || acc.Output != 25 {
		t.Fatalf("merged = %+v, want input 100 / output 25", acc)
	}
	if acc.Total < 125 {
		t.Fatalf("merged total = %d, want >= 125", acc.Total)
	}
}

// ---- request paths ----

// A group request re-enters the host via host.model.*, which marks the caller ctx
// and suppresses every usage publish point — so the plugin has to count the
// member's own report, per group and per member.
func TestExecuteRecordsGroupAndMemberTokens(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Strategy: "fallback", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
	}}, defaultConfig())

	h := p.host.(*fakeHost)
	h.setHandler(pluginabi.MethodHostModelExecute, func(json.RawMessage) (json.RawMessage, error) {
		return hostExecuteResult(200, []byte(`{"id":"x","usage":{"prompt_tokens":41,"completion_tokens":16,"total_tokens":57}}`), nil), nil
	})

	raw, err := p.execute(execReq("g1", []byte(`{"model":"g1","messages":[]}`)))
	if _, status := decodeExecutorResponse(t, mustOK(t, raw, err)); status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}

	groups := p.rt.snapshotGroupTokens()
	gt, ok := groups["g1"]
	if !ok {
		t.Fatalf("no token counters for group g1: %+v", groups)
	}
	if gt.Input != 41 || gt.Output != 16 || gt.Total != 57 || gt.Reqs != 1 {
		t.Fatalf("group counters = %+v, want input 41 / output 16 / total 57 / reqs 1", gt)
	}
	mt, ok := p.rt.snapshotTokens()["m1"]
	if !ok || mt.Total != 57 {
		t.Fatalf("member counters = %+v (present=%v), want total 57", mt, ok)
	}
}

// A member response without usage must leave the counters alone rather than
// recording a zero-token request.
func TestExecuteWithoutUsageLeavesCountersEmpty(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
	}}, defaultConfig())
	p.host.(*fakeHost).setHandler(pluginabi.MethodHostModelExecute, func(json.RawMessage) (json.RawMessage, error) {
		return hostExecuteResult(200, []byte(`{"id":"x","choices":[]}`), nil), nil
	})

	raw, err := p.execute(execReq("g1", []byte(`{"model":"g1","messages":[]}`)))
	if _, status := decodeExecutorResponse(t, mustOK(t, raw, err)); status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if n := len(p.rt.snapshotGroupTokens()); n != 0 {
		t.Fatalf("group counters = %+v, want none", p.rt.snapshotGroupTokens())
	}
	if n := len(p.rt.snapshotTokens()); n != 0 {
		t.Fatalf("member counters = %+v, want none", p.rt.snapshotTokens())
	}
}

// Streaming: usage arrives in a content frame; the counters must be recorded
// once, and the relayed frames must stay byte-identical to what the host read.
func TestStreamRecordsTokensOnce(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
	}}, defaultConfig())

	h := p.host.(*fakeHost)
	h.setHandler(pluginabi.MethodHostModelExecuteStream, func(json.RawMessage) (json.RawMessage, error) {
		return hostStreamResult(200, "hs1"), nil
	})
	// The host answers stream emit/close with an ok envelope; without these the
	// relay aborts after the first frame (the fake host errors on unknown methods).
	okHandler := func(json.RawMessage) (json.RawMessage, error) {
		return okResult(map[string]any{"ok": true}), nil
	}
	h.setHandler(pluginabi.MethodHostStreamEmit, okHandler)
	h.setHandler(pluginabi.MethodHostStreamClose, okHandler)
	h.setHandler(pluginabi.MethodHostModelStreamClose, okHandler)
	frames := []string{
		`{"choices":[{"delta":{"content":"hi"}}],"usage":null}`,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
	}
	var reads int
	h.setHandler(pluginabi.MethodHostModelStreamRead, func(json.RawMessage) (json.RawMessage, error) {
		if reads < len(frames) {
			f := frames[reads]
			reads++
			return hostStreamReadResult(f, "", false), nil
		}
		return hostStreamReadResult("", "", true), nil
	})

	raw, err := p.executeStream(marshalExecTest(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "g1", SourceFormat: "openai", Stream: true},
		StreamID:        "ps1",
	}))
	if _, status := decodeStreamResponse(t, mustOK(t, raw, err)); status != 200 {
		t.Fatalf("stream status = %d, want 200", status)
	}
	waitFor(t, func() bool {
		gt := p.rt.snapshotGroupTokens()["g1"]
		return gt.Reqs == 1
	})

	gt := p.rt.snapshotGroupTokens()["g1"]
	if gt.Input != 10 || gt.Output != 2 || gt.Total != 12 || gt.Reqs != 1 {
		t.Fatalf("group counters = %+v, want input 10 / output 2 / total 12 / reqs 1 (repeated usage frames must not double-count)", gt)
	}
	if mt := p.rt.snapshotTokens()["m1"]; mt.Total != 12 || mt.Reqs != 1 {
		t.Fatalf("member counters = %+v, want total 12 / reqs 1", mt)
	}

	emits := h.callsFor(pluginabi.MethodHostStreamEmit)
	if len(emits) != len(frames) {
		t.Fatalf("emitted %d frames, want %d", len(emits), len(frames))
	}
	for i, e := range emits {
		var req rpcStreamEmitRequest
		if err := json.Unmarshal(e.Payload, &req); err != nil {
			t.Fatalf("decode emit %d: %v", i, err)
		}
		if string(req.Payload) != frames[i] {
			t.Errorf("emitted payload %d = %s, want %s (token counting must not rewrite the stream)", i, req.Payload, frames[i])
		}
	}
}

// ---- persistence ----

// "这个组一共用了多少 token" has to survive a restart, so the counters are
// flushed to their own file and restored on load.
func TestTokenStatsPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.StatsFile = filepath.Join(dir, "stats.json")

	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
	}}, cfg)
	p.statsStore = newStatsStore(cfg.StatsFile)
	p.rt.recordTokens("g1", "m1", tokenUsage{Input: 5, Output: 7, Total: 12})
	if err := p.flushTokenStats(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	p2 := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
	}}, cfg)
	p2.statsStore = newStatsStore(cfg.StatsFile)
	p2.loadTokenStats()

	gt := p2.rt.snapshotGroupTokens()["g1"]
	if gt.Input != 5 || gt.Output != 7 || gt.Total != 12 || gt.Reqs != 1 {
		t.Fatalf("restored group counters = %+v, want input 5 / output 7 / total 12 / reqs 1", gt)
	}
	if mt := p2.rt.snapshotTokens()["m1"]; mt.Total != 12 {
		t.Fatalf("restored member counters = %+v, want total 12", mt)
	}
}

// A missing or corrupt stats file must never break plugin startup.
func TestLoadTokenStatsToleratesBadFile(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.StatsFile = filepath.Join(dir, "missing.json")

	p := newTestPlugin(t, groupFile{Version: 1, Groups: nil}, cfg)
	p.statsStore = newStatsStore(cfg.StatsFile)
	p.loadTokenStats()
	if n := len(p.rt.snapshotGroupTokens()); n != 0 {
		t.Fatalf("counters = %d, want 0 for a missing file", n)
	}

	if err := os.WriteFile(cfg.StatsFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.loadTokenStats() // must not panic and must keep the (empty) counters
	if n := len(p.rt.snapshotGroupTokens()); n != 0 {
		t.Fatalf("counters = %d, want 0 for a corrupt file", n)
	}
}

// The periodic writer must write only when the interval elapsed and something
// changed — otherwise every tick would rewrite the file.
func TestFlushTokenStatsIfDue(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.StatsFile = filepath.Join(dir, "stats.json")
	cfg.StatsFlushInterval = "30s"
	cfg.StatsFlushIntervalDur = 30 * time.Second

	now := time.Unix(1000, 0)
	p := newTestPlugin(t, groupFile{Version: 1, Groups: nil}, cfg)
	p.rt.setClock(func() time.Time { return now })
	p.statsStore = newStatsStore(cfg.StatsFile)

	p.rt.recordTokens("g1", "m1", tokenUsage{Input: 1, Output: 1, Total: 2})
	p.flushTokenStatsIfDue() // first call: lastFlush is zero => due
	if _, err := os.Stat(cfg.StatsFile); err != nil {
		t.Fatalf("expected a stats file after the first flush: %v", err)
	}

	// No new usage: not due even after the interval passes.
	now = now.Add(time.Hour)
	p.flushTokenStatsIfDue()
	if !p.statsLastFlush.Equal(time.Unix(1000, 0)) {
		t.Fatalf("flush ran without new usage (lastFlush=%v)", p.statsLastFlush)
	}

	// New usage inside the interval: still not due.
	now = time.Unix(1000, 0).Add(10 * time.Second)
	p.statsLastFlush = time.Unix(1000, 0)
	p.rt.recordTokens("g1", "m1", tokenUsage{Input: 1, Output: 1, Total: 2})
	p.flushTokenStatsIfDue()
	if !p.statsLastFlush.Equal(time.Unix(1000, 0)) {
		t.Fatalf("flush ran before the interval elapsed (lastFlush=%v)", p.statsLastFlush)
	}

	// New usage after the interval: due.
	now = time.Unix(1000, 0).Add(31 * time.Second)
	p.flushTokenStatsIfDue()
	if !p.statsLastFlush.Equal(now) {
		t.Fatalf("flush did not run when due (lastFlush=%v, want %v)", p.statsLastFlush, now)
	}
	f, err := newStatsStore(cfg.StatsFile).load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := f.Groups["g1"].Total; got != 4 {
		t.Fatalf("persisted total = %d, want 4", got)
	}
}

// stats_file: "" means "count in memory only" (no file is written).
func TestEmptyStatsFileDisablesPersistence(t *testing.T) {
	cfg := defaultConfig()
	cfg.StatsFile = ""
	p := newTestPlugin(t, groupFile{Version: 1, Groups: nil}, cfg)
	p.statsStore = nil
	p.rt.recordTokens("g1", "m1", tokenUsage{Input: 1, Output: 1, Total: 2})
	if err := p.flushTokenStats(); err != nil {
		t.Fatalf("flush with no store must be a no-op, got %v", err)
	}
	if p.rt.snapshotGroupTokens()["g1"].Total != 2 {
		t.Fatal("in-memory counters must keep working without a stats file")
	}
}

// ---- reset + state ----

func TestResetStatsClearsTokens(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
		{Name: "g2", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
	}}, defaultConfig())
	p.rt.recordTokens("g1", "m1", tokenUsage{Input: 1, Output: 1, Total: 2})
	p.rt.recordTokens("g2", "m1", tokenUsage{Input: 3, Output: 3, Total: 6})

	body, _ := json.Marshal(map[string]string{"name": "g1", "what": "stats"})
	if _, err := mgmtRaw(t, p, "POST", "/plugins/cpa-router/reset", body); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, ok := p.rt.snapshotGroupTokens()["g1"]; ok {
		t.Fatalf("g1 counters survived a reset: %+v", p.rt.snapshotGroupTokens())
	}
	if p.rt.snapshotGroupTokens()["g2"].Total != 6 {
		t.Fatalf("reset of g1 must not touch g2: %+v", p.rt.snapshotGroupTokens())
	}
}

func TestStateExposesTokenCounters(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
	}}, defaultConfig())
	p.rt.recordTokens("g1", "m1", tokenUsage{Input: 41, Output: 16, Total: 57})

	raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	groups, _ := m["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("groups = %v", groups)
	}
	g, _ := groups[0].(map[string]any)
	for key, want := range map[string]float64{
		"group_input_tokens":  41,
		"group_output_tokens": 16,
		"group_total_tokens":  57,
		"group_token_reqs":    1,
	} {
		if got := g[key]; got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	models, _ := m["models"].(map[string]any)
	entry, _ := models["m1"].(map[string]any)
	if entry == nil {
		t.Fatalf("models = %v", models)
	}
	if got := entry["total_tokens"]; got != float64(57) {
		t.Errorf("models[m1].total_tokens = %v, want 57", got)
	}
	if got := entry["input_tokens"]; got != float64(41) {
		t.Errorf("models[m1].input_tokens = %v, want 41", got)
	}
	cfgView, _ := m["config"].(map[string]any)
	if got := cfgView["stats_file"]; got != p.conf().StatsFile {
		t.Errorf("config.stats_file = %v, want %v", got, p.conf().StatsFile)
	}
}

// waitFor polls cond for up to ~2s; streaming forwards run in a goroutine.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

// ---- config ----

func TestStatsConfigDefaultsAndValidation(t *testing.T) {
	cfg := defaultConfig()
	if cfg.StatsFile != "plugins/cpa-router/stats.json" {
		t.Errorf("default stats_file = %q", cfg.StatsFile)
	}
	if cfg.StatsFlushIntervalDur != 30*time.Second {
		t.Errorf("default stats_flush_interval = %v", cfg.StatsFlushIntervalDur)
	}

	if _, err := decodeConfig([]byte("stats_flush_interval: \"nope\"\n")); err == nil ||
		!strings.Contains(err.Error(), "stats_flush_interval") {
		t.Errorf("invalid stats_flush_interval must fail with the field name, got %v", err)
	}
	if _, err := decodeConfig([]byte("stats_flush_interval: \"0s\"\n")); err == nil ||
		!strings.Contains(err.Error(), "stats_flush_interval") {
		t.Errorf("zero stats_flush_interval must fail with the field name, got %v", err)
	}
	cfg2, err := decodeConfig([]byte("stats_file: \"\"\nstats_flush_interval: \"1s\"\n"))
	if err != nil {
		t.Fatalf("valid stats config rejected: %v", err)
	}
	if cfg2.StatsFile != "" || cfg2.StatsFlushIntervalDur != time.Second {
		t.Errorf("decoded stats config = %+v", cfg2)
	}
}
