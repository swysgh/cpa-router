package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func marshalExecTest(t *testing.T, req rpcExecutorRequest) []byte {
	t.Helper()
	return marshalExec(req)
}

func execReq(model string, body []byte) []byte {
	req := rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           model,
			SourceFormat:    "openai",
			OriginalRequest: body,
		},
	}
	raw, _ := json.Marshal(req)
	return raw
}

// decodeExecutorResponse unwraps the ok envelope result into a response + status.
func decodeExecutorResponse(t *testing.T, raw []byte) (pluginapi.ExecutorResponse, int) {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode exec resp: %v", err)
	}
	if !env.OK {
		return pluginapi.ExecutorResponse{}, env.Error.HTTPStatus
	}
	var resp pluginapi.ExecutorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return resp, 200
}

func TestExecutorFallbackOn429(t *testing.T) {
	clock := time.Unix(1000, 0)
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	p.rt.setClock(func() time.Time { return clock })

	parsed, _, _ := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Strategy: "fallback", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true}, {Model: "m2", Enabled: true},
		}},
	}})
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, "")

	// First member 429, second 200.
	h.setHandler(pluginabi.MethodHostModelExecute, func(payload json.RawMessage) (json.RawMessage, error) {
		var req hostModelExecutionRequest
		_ = json.Unmarshal(payload, &req)
		if req.Model == "m1" {
			return hostErrorResult("upstream", "rate limited", 429), nil
		}
		return hostExecuteResult(200, []byte(`{"ok":true}`), nil), nil
	})

	exRaw, exErr := p.execute(execReq("free-stack", []byte(`{"model":"free-stack","messages":[]}`)))
	resp, status := decodeExecutorResponse(t, mustOK(t, exRaw, exErr))
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("payload = %s", resp.Payload)
	}
	// m1 should be cooling.
	if !p.rt.isCooling("m1") {
		t.Fatalf("m1 should be cooling after 429")
	}
	// stats
	if p.rt.snapshotModels()["m1"].Fail != 1 {
		t.Fatalf("m1 fail stat wrong")
	}
	if p.rt.snapshotModels()["m2"].OK != 1 {
		t.Fatalf("m2 ok stat wrong")
	}
}

func TestExecutorAll429(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	parsed, _, _ := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true}, {Model: "m2", Enabled: true},
		}},
	}})
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, "")

	h.setHandler(pluginabi.MethodHostModelExecute, func(payload json.RawMessage) (json.RawMessage, error) {
		return hostErrorResult("up", "rl", 429), nil
	})
	r2, e2 := p.execute(execReq("free-stack", []byte(`{}`)))
	_, status := decodeExecutorResponse(t, mustOK(t, r2, e2))
	if status != 429 {
		t.Fatalf("status = %d, want 429", status)
	}
}

// newTracePlugin builds a plugin with a single fallback group over the given
// members, plus a fake host whose handler is supplied by the caller.
func newTracePlugin(t *testing.T, members ...string) (*plugin, *fakeHost) {
	t.Helper()
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	raw := make([]rawMember, 0, len(members))
	for _, m := range members {
		raw = append(raw, rawMember{Model: m, Enabled: true})
	}
	parsed, _, _ := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Strategy: "fallback", Enabled: true, Members: raw},
	}})
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, "")
	return p, h
}

// Regression: the management /test endpoint used to report the expansion plan as
// "attempts" with status hardcoded to 0. The trace must show what was really
// tried, in order, and stop at the first success.
func TestExecuteTracesRealAttempts(t *testing.T) {
	p, h := newTracePlugin(t, "m1", "m2", "m3")
	h.setHandler(pluginabi.MethodHostModelExecute, func(payload json.RawMessage) (json.RawMessage, error) {
		var req hostModelExecutionRequest
		_ = json.Unmarshal(payload, &req)
		if req.Model == "m1" {
			return hostErrorResult("upstream", "rate limited", 429), nil
		}
		return hostExecuteResult(200, []byte(`{"ok":true}`), nil), nil
	})

	var trace []attemptTrace
	raw, err := p.executeTraced(execReq("g", []byte(`{"model":"g"}`)), &trace)
	if _, status := decodeExecutorResponse(t, mustOK(t, raw, err)); status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}

	// m1 failed, m2 succeeded, m3 must never be tried.
	if len(trace) != 2 {
		t.Fatalf("trace has %d entries, want 2: %+v", len(trace), trace)
	}
	if trace[0].Member != "m1" || trace[0].Status != 429 {
		t.Errorf("trace[0] = %+v, want member=m1 status=429", trace[0])
	}
	if trace[0].CooldownS <= 0 {
		t.Errorf("trace[0].CooldownS = %d, want > 0 for a 429", trace[0].CooldownS)
	}
	if trace[1].Member != "m2" || trace[1].Status != 200 {
		t.Errorf("trace[1] = %+v, want member=m2 status=200", trace[1])
	}
}

// A fully failed group must still report every attempt it made.
func TestExecuteTracesAllFailures(t *testing.T) {
	p, h := newTracePlugin(t, "m1", "m2")
	h.setHandler(pluginabi.MethodHostModelExecute, func(payload json.RawMessage) (json.RawMessage, error) {
		return hostErrorResult("upstream", "rate limited", 429), nil
	})

	var trace []attemptTrace
	raw, err := p.executeTraced(execReq("g", []byte(`{}`)), &trace)
	if _, status := decodeExecutorResponse(t, mustOK(t, raw, err)); status != 429 {
		t.Fatalf("status = %d, want 429", status)
	}
	if len(trace) != 2 {
		t.Fatalf("trace has %d entries, want 2: %+v", len(trace), trace)
	}
	for i, want := range []string{"m1", "m2"} {
		if trace[i].Member != want || trace[i].Status != 429 {
			t.Errorf("trace[%d] = %+v, want member=%s status=429", i, trace[i], want)
		}
	}
}

// The normal request path passes no trace; that must stay safe.
func TestExecuteWithoutTraceIsSafe(t *testing.T) {
	p, h := newTracePlugin(t, "m1")
	h.setHandler(pluginabi.MethodHostModelExecute, func(payload json.RawMessage) (json.RawMessage, error) {
		return hostExecuteResult(200, []byte(`{"ok":true}`), nil), nil
	})
	raw, err := p.execute(execReq("g", []byte(`{}`)))
	if _, status := decodeExecutorResponse(t, mustOK(t, raw, err)); status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
}

// A group whose members are all cooling must record no attempts at all — the
// old snapshot reported every member here, which is exactly the misleading
// behaviour this replaced.
func TestExecuteTracesNothingWhenGroupExhausted(t *testing.T) {
	p, h := newTracePlugin(t, "m1", "m2")
	_ = h
	// Cool both members down first.
	p.rt.recordCooldown("m1", 429, "rl", p.cfg)
	p.rt.recordCooldown("m2", 429, "rl", p.cfg)
	p.cfg.AllCoolingPolicy = "error"

	var trace []attemptTrace
	raw, err := p.executeTraced(execReq("g", []byte(`{}`)), &trace)
	if _, status := decodeExecutorResponse(t, mustOK(t, raw, err)); status != 429 {
		t.Fatalf("status = %d, want 429", status)
	}
	if len(trace) != 0 {
		t.Fatalf("trace = %+v, want empty (nothing was attempted)", trace)
	}
}

func TestExecutorModelRewrite(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	parsed, _, _ := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Enabled: true, Members: []rawMember{{Model: "real-model", Enabled: true}}},
	}})
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, "")

	var seenModel string
	h.setHandler(pluginabi.MethodHostModelExecute, func(payload json.RawMessage) (json.RawMessage, error) {
		var req hostModelExecutionRequest
		_ = json.Unmarshal(payload, &req)
		seenModel = req.Model
		return hostExecuteResult(200, []byte(`{}`), nil), nil
	})
	body := []byte(`{"model":"free-stack","messages":[]}`)
	r3, e3 := p.execute(execReq("free-stack", body))
	_ = mustOK(t, r3, e3)
	if seenModel != "real-model" {
		t.Fatalf("model not rewritten: %q", seenModel)
	}

	// Non-JSON body passes through unchanged.
	r4, e4 := p.execute(execReq("free-stack", []byte("not json")))
	_ = mustOK(t, r4, e4)
}

// decodeStreamResponse decodes the executor.execute_stream ok envelope.
func decodeStreamResponse(t *testing.T, raw []byte) (map[string]any, int) {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode stream resp: %v", err)
	}
	if !env.OK {
		return nil, env.Error.HTTPStatus
	}
	var m map[string]any
	_ = json.Unmarshal(env.Result, &m)
	return m, 200
}

func TestStreamPreflightDegrade(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	parsed, _, _ := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true}, {Model: "m2", Enabled: true},
		}},
	}})
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, "")

	// m1 preflight 429, m2 preflight 200 with a stream id; then stream emits one chunk.
	var streamCalls int
	h.setHandler(pluginabi.MethodHostModelExecuteStream, func(payload json.RawMessage) (json.RawMessage, error) {
		var req hostModelExecutionRequest
		_ = json.Unmarshal(payload, &req)
		if req.Model == "m1" {
			return hostErrorResult("up", "rl", 429), nil
		}
		return hostStreamResult(200, "host-stream-1"), nil
	})
	h.setHandler(pluginabi.MethodHostModelStreamRead, func(payload json.RawMessage) (json.RawMessage, error) {
		streamCalls++
		var req pluginapi.HostModelStreamReadRequest
		_ = json.Unmarshal(payload, &req)
		if streamCalls == 1 {
			return hostStreamReadResult("hello", "", false), nil
		}
		return hostStreamReadResult("", "", true), nil
	})

	sr1, se1 := p.executeStream(marshalExecTest(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "free-stack", SourceFormat: "openai", Stream: true},
		StreamID:        "plugin-stream-1",
	}))
	m, status := decodeStreamResponse(t, mustOK(t, sr1, se1))
	if status != 200 {
		t.Fatalf("stream status = %d", status)
	}
	// Let the async goroutine run.
	time.Sleep(200 * time.Millisecond)
	emits := h.callsFor(pluginabi.MethodHostStreamEmit)
	if len(emits) == 0 {
		t.Fatalf("expected plugin stream emits")
	}
	// close plugin stream at end.
	closes := h.callsFor(pluginabi.MethodHostStreamClose)
	if len(closes) == 0 {
		t.Fatalf("expected plugin stream close")
	}
	_ = m
}

func TestStreamPreflightAllFail(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	parsed, _, _ := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
	}})
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, "")

	h.setHandler(pluginabi.MethodHostModelExecuteStream, func(payload json.RawMessage) (json.RawMessage, error) {
		return hostErrorResult("up", "rl", 429), nil
	})
	sr2, se2 := p.executeStream(marshalExecTest(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "free-stack", Stream: true},
		StreamID:        "ps1",
	}))
	_, status := decodeStreamResponse(t, mustOK(t, sr2, se2))
	if status != 429 {
		t.Fatalf("stream all-fail status = %d, want 429", status)
	}
	time.Sleep(50 * time.Millisecond)
	// Must NOT have emitted any plugin-stream chunk.
	if n := len(h.callsFor(pluginabi.MethodHostStreamEmit)); n != 0 {
		t.Fatalf("expected no emit on preflight failure, got %d", n)
	}
}

func TestStreamDegradeAfterError(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	parsed, _, _ := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true}, {Model: "m2", Enabled: true},
		}},
	}})
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, "")

	// m1 preflight ok with stream, but first read returns error before any payload.
	var readCount int
	h.setHandler(pluginabi.MethodHostModelExecuteStream, func(payload json.RawMessage) (json.RawMessage, error) {
		var req hostModelExecutionRequest
		_ = json.Unmarshal(payload, &req)
		if req.Model == "m1" {
			return hostStreamResult(200, "hs-m1"), nil
		}
		return hostStreamResult(200, "hs-m2"), nil
	})
	h.setHandler(pluginabi.MethodHostModelStreamRead, func(payload json.RawMessage) (json.RawMessage, error) {
		var req pluginapi.HostModelStreamReadRequest
		_ = json.Unmarshal(payload, &req)
		if req.StreamID == "hs-m1" {
			readCount++
			return hostStreamReadResult("", "boom", false), nil
		}
		// m2 stream delivers payload.
		readCount++
		if readCount == 2 {
			return hostStreamReadResult("ok", "", false), nil
		}
		return hostStreamReadResult("", "", true), nil
	})

	sr3, se3 := p.executeStream(marshalExecTest(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "free-stack", Stream: true},
		StreamID:        "ps1",
	}))
	_ = mustOK(t, sr3, se3)
	time.Sleep(200 * time.Millisecond)
	emits := h.callsFor(pluginabi.MethodHostStreamEmit)
	if len(emits) == 0 {
		t.Fatalf("expected m2 payload emit after m1 read error")
	}
}

func TestStreamNoDegradeAfterPayload(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	parsed, _, _ := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
	}})
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, "")

	var readCount int
	h.setHandler(pluginabi.MethodHostModelExecuteStream, func(payload json.RawMessage) (json.RawMessage, error) {
		return hostStreamResult(200, "hs-m1"), nil
	})
	h.setHandler(pluginabi.MethodHostModelStreamRead, func(payload json.RawMessage) (json.RawMessage, error) {
		readCount++
		if readCount == 1 {
			return hostStreamReadResult("first", "", false), nil
		}
		// After payload delivered, error → should emit error + close, not degrade.
		return hostStreamReadResult("", "late", false), nil
	})

	sr3, se3 := p.executeStream(marshalExecTest(t, rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "free-stack", Stream: true},
		StreamID:        "ps1",
	}))
	_ = mustOK(t, sr3, se3)
	time.Sleep(200 * time.Millisecond)
	emits := h.callsFor(pluginabi.MethodHostStreamEmit)
	if len(emits) < 1 {
		t.Fatalf("expected at least the first payload emit")
	}
	closes := h.callsFor(pluginabi.MethodHostStreamClose)
	if len(closes) == 0 {
		t.Fatalf("expected plugin stream close after late error")
	}
}

var _ = pluginabi.MethodExecutorExecuteStream
