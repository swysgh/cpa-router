package main

// review_fixes_test.go —— 评审阶段独立补的回归测试（期望值独立于实现推导）。
//
// 覆盖两处评审发现的问题：
//  1. 流式降级（首个 payload 之前失败）必须带着「客户端原始请求体 + 客户端协议」
//     重新进入宿主；早期实现会发空 body 且把协议硬编码成 openai。
//  2. groups.yaml 里省略 `enabled` 字段时，组与成员都应视为启用（opt-out 语义）；
//     早期实现按 Go 零值处理，省略即等于禁用，手写配置会静默不生效。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// TestReviewGroupEnabledDefaultsTrue 验证省略 enabled 时的 opt-out 语义。
func TestReviewGroupEnabledDefaultsTrue(t *testing.T) {
	raw := []byte(`version: 1
groups:
  - name: compact
    members:
      - model: m1
  - name: off
    enabled: false
    members:
      - model: m2
  - name: member-off
    members:
      - model: m3
      - model: m4
        enabled: false
`)
	var f groupFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	groups, _, err := parseGroups(f)
	if err != nil {
		t.Fatalf("parseGroups: %v", err)
	}
	byName := map[string]group{}
	for _, g := range groups {
		byName[g.Name] = g
	}
	if !byName["compact"].Enabled {
		t.Fatalf("组 compact 省略 enabled 时应为启用")
	}
	if !byName["compact"].Members[0].Enabled {
		t.Fatalf("成员省略 enabled 时应为启用")
	}
	if byName["off"].Enabled {
		t.Fatalf("组显式 enabled:false 应被禁用")
	}
	if !byName["member-off"].Members[0].Enabled {
		t.Fatalf("member-off 第一个成员应启用")
	}
	if byName["member-off"].Members[1].Enabled {
		t.Fatalf("member-off 第二个成员显式 enabled:false 应被禁用")
	}

	// 该组必须能被路由命中（而不是因为零值被静默跳过）。
	p := newPlugin(newFakeHost())
	p.cfg = defaultConfig()
	p.groups = groups
	p.nameIndex = buildGroupNameIndex(groups, "")
	routeRaw, _ := json.Marshal(pluginapi.ModelRouteRequest{RequestedModel: "compact", SourceFormat: "openai"})
	out, err := p.routeModel(routeRaw)
	if err != nil {
		t.Fatalf("routeModel: %v", err)
	}
	var env struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.ModelRouteResponse `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("decode route envelope: %v", err)
	}
	if !env.OK || !env.Result.Handled {
		t.Fatalf("省略 enabled 的组应被路由命中, got %+v", env)
	}
	if env.Result.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("TargetKind = %q, want self", env.Result.TargetKind)
	}
}

// TestReviewDegradeForwardsOriginalRequest 验证流式降级会带上原始请求体与客户端协议。
func TestReviewDegradeForwardsOriginalRequest(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()

	groups, _, err := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Strategy: "fallback", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true},
			{Model: "m2", Enabled: true},
		}},
	}})
	if err != nil {
		t.Fatalf("parseGroups: %v", err)
	}
	p.groups = groups
	p.nameIndex = buildGroupNameIndex(groups, "")

	h.setHandler(pluginabi.MethodHostModelExecuteStream, func(payload json.RawMessage) (json.RawMessage, error) {
		var req hostModelExecutionRequest
		_ = json.Unmarshal(payload, &req)
		if req.Model == "m1" {
			return hostStreamResult(200, "hs-m1"), nil
		}
		return hostStreamResult(200, "hs-m2"), nil
	})
	m2Reads := 0
	h.setHandler(pluginabi.MethodHostModelStreamRead, func(payload json.RawMessage) (json.RawMessage, error) {
		var req pluginapi.HostModelStreamReadRequest
		_ = json.Unmarshal(payload, &req)
		if req.StreamID == "hs-m1" {
			// m1 在首个 payload 之前失败 → 允许降级。
			return hostStreamReadResult("", "boom before payload", false), nil
		}
		m2Reads++
		if m2Reads == 1 {
			return hostStreamReadResult("M2-PAYLOAD", "", false), nil
		}
		return hostStreamReadResult("", "", true), nil
	})

	clientBody := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	reqRaw, _ := json.Marshal(rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           "g",
			SourceFormat:    "claude",
			Stream:          true,
			OriginalRequest: clientBody,
		},
		StreamID: "ps1",
	})

	out, err := p.executeStream(reqRaw)
	if err != nil {
		t.Fatalf("executeStream: %v", err)
	}
	if _, status := decodeStreamResponse(t, out); status != 200 {
		t.Fatalf("executeStream status = %d, want 200", status)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.callsFor(pluginabi.MethodHostStreamEmit)) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	emits := h.callsFor(pluginabi.MethodHostStreamEmit)
	if len(emits) == 0 {
		t.Fatalf("降级后 m2 的 payload 没有被转发")
	}
	var emitReq rpcStreamEmitRequest
	if err := json.Unmarshal(emits[0].Payload, &emitReq); err != nil {
		t.Fatalf("decode emit: %v", err)
	}
	if string(emitReq.Payload) != "M2-PAYLOAD" {
		t.Fatalf("转发的 payload = %q, want M2-PAYLOAD", emitReq.Payload)
	}

	// 降级时的宿主调用必须带原始请求体与客户端协议。
	var degraded *hostModelExecutionRequest
	for _, c := range h.callsFor(pluginabi.MethodHostModelExecuteStream) {
		var r hostModelExecutionRequest
		if err := json.Unmarshal(c.Payload, &r); err != nil {
			t.Fatalf("decode host.model.execute_stream payload: %v", err)
		}
		if r.Model == "m2" {
			rr := r
			degraded = &rr
		}
	}
	if degraded == nil {
		t.Fatalf("降级时没有对 m2 发起 host.model.execute_stream")
	}
	if degraded.EntryProtocol != "claude" || degraded.ExitProtocol != "claude" {
		t.Fatalf("降级协议 = %q/%q, want claude/claude", degraded.EntryProtocol, degraded.ExitProtocol)
	}
	if !strings.Contains(string(degraded.Body), `"content":"hi"`) {
		t.Fatalf("降级时丢失了客户端请求体: %s", degraded.Body)
	}
	if !strings.Contains(string(degraded.Body), `"model":"m2"`) {
		t.Fatalf("降级时未改写 model: %s", degraded.Body)
	}
}
