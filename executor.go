package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// rpcExecutorRequest carries the executor request plus the stream/host callback ids.
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// hostModelExecutionRequest embeds the SDK request and forwards host_callback_id.
type hostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// hostModelStreamResponse carries the stream id from host.model.execute_stream.
type hostModelStreamResponse struct {
	pluginapi.HostModelStreamResponse
}

// attemptTrace is one attempt that was actually made while serving a single
// execute call, in the order it happened. It backs the management /test
// endpoint's "attempts" field, which previously reported the expansion plan
// (with status hardcoded to 0) instead of what was really tried.
type attemptTrace struct {
	Member    string `json:"member"`
	Status    int    `json:"status"`
	Error     string `json:"error"`
	CooldownS int    `json:"cooldown_s"`
	LatencyMS int    `json:"latency_ms"`
}

// recordAttemptTrace appends one attempt result. trace is nil on the normal
// request path, which collects no trace.
func recordAttemptTrace(trace *[]attemptTrace, t attemptTrace) {
	if trace == nil {
		return
	}
	*trace = append(*trace, t)
}

// ---- Expansion / selection algorithm (§7) ----

// expand expands a group into an ordered, de-duplicated list of real model
// names. When ignoreCooldown is false (normal path), cooling models are removed
// at expansion time. visited tracks the group path for cycle safety (double
// guard beyond load-time detection).
func (p *plugin) expand(groupName string, depth int, visited map[string]bool, ignoreCooldown bool) ([]string, error) {
	if depth > maxNestingDepth {
		return nil, fmt.Errorf("嵌套层数超过 %d", maxNestingDepth)
	}
	if visited[groupName] {
		return nil, fmt.Errorf("循环嵌套: %s", groupName)
	}
	g, ok := p.groupByName(groupName)
	if !ok || !g.Enabled {
		return nil, nil
	}

	// Build eligible members (enabled; model members filtered by cooldown).
	var eligible []member
	for _, m := range g.Members {
		if !m.Enabled {
			continue
		}
		if m.IsModel && !ignoreCooldown && p.rt.isCooling(m.Model) {
			continue
		}
		eligible = append(eligible, m)
	}

	if g.Strategy == "round-robin" && len(eligible) > 0 {
		start := p.rt.pointerAdvance(groupName) % len(eligible)
		eligible = append(append([]member{}, eligible[start:]...), eligible[:start]...)
	}

	visited[groupName] = true
	defer delete(visited, groupName)

	var out []string
	seen := make(map[string]bool)
	for _, m := range eligible {
		var models []string
		if m.IsModel {
			models = []string{m.Model}
		} else {
			inner, err := p.expand(m.Group, depth+1, visited, ignoreCooldown)
			if err != nil {
				return nil, err
			}
			models = inner
		}
		for _, mm := range models {
			if seen[mm] {
				continue
			}
			seen[mm] = true
			out = append(out, mm)
		}
	}

	// Enforce max_attempts truncation.
	if p.cfg.MaxAttempts > 0 && len(out) > p.cfg.MaxAttempts {
		out = out[:p.cfg.MaxAttempts]
	}
	return out, nil
}

// selectAttempts produces the ordered attempt list, applying the
// all_cooling_policy when the normal expansion is empty (§7.4).
func (p *plugin) selectAttempts(g *group) ([]string, error) {
	attempts, err := p.expand(g.Name, 1, map[string]bool{}, false)
	if err != nil {
		return nil, err
	}
	if len(attempts) > 0 {
		return attempts, nil
	}

	switch p.cfg.AllCoolingPolicy {
	case "error":
		return nil, p.exhaustedError(g, nil)
	case "first":
		ignored, ierr := p.expand(g.Name, 1, map[string]bool{}, true)
		if ierr != nil {
			return nil, ierr
		}
		if len(ignored) == 0 {
			return nil, p.exhaustedError(g, nil)
		}
		// Pick the model with the earliest cooldown expiry.
		pick := ignored[0]
		bestUntil := p.rt.coolingInfo(pick).Until
		for _, m := range ignored[1:] {
			u := p.rt.coolingInfo(m).Until
			if bestUntil.IsZero() || (!u.IsZero() && u.Before(bestUntil)) {
				pick = m
				bestUntil = u
			}
		}
		return []string{pick}, nil
	case "wait":
		fallthrough
	default:
		// Wait until the earliest cooling member expires (bounded by max_wait).
		all, aerr := p.expand(g.Name, 1, map[string]bool{}, true)
		if aerr != nil {
			return nil, aerr
		}
		earliest := p.rt.earliestCooling(all)
		if earliest == "" {
			return nil, p.exhaustedError(g, nil)
		}
		wait := p.rt.coolingInfo(earliest).Until.Sub(p.rt.now())
		if wait > p.cfg.MaxWaitDur {
			wait = p.cfg.MaxWaitDur
		}
		if wait > 0 {
			time.Sleep(wait)
		}
		attempts, err = p.expand(g.Name, 1, map[string]bool{}, false)
		if err != nil {
			return nil, err
		}
		if len(attempts) == 0 {
			return nil, p.exhaustedError(g, nil)
		}
		return attempts, nil
	}
}

// exhaustedError builds the group_exhausted envelope (http 429) with the
// earliest-recovery hint.
func (p *plugin) exhaustedError(g *group, last map[string]struct{}) error {
	all, _ := p.expand(g.Name, 1, map[string]bool{}, true)
	earliest := p.rt.earliestCooling(all)
	msg := fmt.Sprintf("组 %s 的成员全部处于冷却中", g.Name)
	if earliest != "" {
		secs := int(p.rt.coolingInfo(earliest).Until.Sub(p.rt.now()).Seconds())
		if secs < 0 {
			secs = 0
		}
		msg += fmt.Sprintf("，最早 %ds 后恢复", secs)
	}
	return &pluginError{code: "group_exhausted", message: msg, httpStatus: http.StatusTooManyRequests}
}

// pluginError is a typed error carrying an http status for envelope emission.
type pluginError struct {
	code       string
	message    string
	httpStatus int
}

func (e *pluginError) Error() string { return e.message }

// ---- body model rewrite (§8.2) ----

// rewriteModel rewrites the top-level "model" field of a JSON body to the given
// member model. Non-JSON bodies or bodies without a string model field are
// returned unchanged.
func rewriteModel(body []byte, model string) []byte {
	if len(body) == 0 {
		return body
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	raw, ok := top["model"]
	if !ok {
		return body
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return body
	}
	top["model"] = json.RawMessage(`"` + strings.ReplaceAll(model, `"`, `\"`) + `"`)
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// ---- host model execution with timeouts (§8.2) ----

func (p *plugin) callWithTimeout(method string, payload any, timeout time.Duration) (json.RawMessage, error) {
	type res struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan res, 1)
	go func() {
		raw, err := p.host.Call(method, payload)
		ch <- res{raw: raw, err: err}
	}()
	select {
	case r := <-ch:
		return r.raw, r.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("host callback %s 超时", method)
	}
}

// execute implements executor.execute (§8.3).
func (p *plugin) execute(raw []byte) ([]byte, error) {
	return p.executeTraced(raw, nil)
}

// executeTraced is execute plus an optional per-attempt trace recorder. The
// management /test endpoint uses it to report what was actually tried; the
// normal request path passes nil and collects nothing.
func (p *plugin) executeTraced(raw []byte, trace *[]attemptTrace) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	p.reloadIfNeeded()

	g, ok := p.resolveGroup(req.Model)
	if !ok {
		return pluginabi.NewErrorEnvelope("group_not_found", fmt.Sprintf("未找到组（或别名）: %s", req.Model), http.StatusBadRequest)
	}

	p.rt.recordGroupAttempt(g.Name)

	attempts, err := p.selectAttempts(g)
	if err != nil {
		if pe, ok := err.(*pluginError); ok {
			return pluginabi.NewErrorEnvelope(pe.code, pe.message, pe.httpStatus)
		}
		return errorEnvelope("group_exhausted", err.Error()), nil
	}

	start := p.rt.now()
	var lastStatus int
	var summaries []string
	for _, member := range attempts {
		if p.cfg.TotalTimeoutDur > 0 && p.rt.now().Sub(start) > p.cfg.TotalTimeoutDur {
			return pluginabi.NewErrorEnvelope("group_exhausted", "单请求总时间预算已耗尽", http.StatusGatewayTimeout)
		}

		body := req.OriginalRequest
		if len(body) == 0 {
			body = req.Payload
		}
		body = rewriteModel(body, member)

		p.rt.recordAttempt(member)
		attemptStart := p.rt.now()
		rawResp, callErr := p.callWithTimeout(pluginabi.MethodHostModelExecute, hostModelExecutionRequest{
			HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
				EntryProtocol: entryProtocol(req.SourceFormat),
				ExitProtocol:  entryProtocol(req.SourceFormat),
				Model:         member,
				Stream:        false,
				Body:          body,
				Headers:       req.Headers,
				Query:         req.Query,
				Alt:           req.Alt,
			},
			HostCallbackID: req.HostCallbackID,
		}, p.cfg.AttemptTimeoutDur)

		if callErr != nil {
			status := httpStatusFromError(callErr)
			cd := p.rt.recordCooldown(member, status, callErr.Error(), p.cfg)
			p.rt.recordFailure(member, status)
			lastStatus = orStatus(status, lastStatus)
			recordAttemptTrace(trace, attemptTrace{
				Member: member, Status: status, Error: callErr.Error(),
				CooldownS: int(cd.Seconds()),
				LatencyMS: int(p.rt.now().Sub(attemptStart).Milliseconds()),
			})
			summaries = append(summaries, fmt.Sprintf("%s: %s", member, attemptSummary(status, callErr.Error(), cd)))
			p.log.warn("attempt failed", map[string]any{"group": g.Name, "member": member, "status": status, "cooldown": int(cd.Seconds())})
			continue
		}

		var resp pluginapi.HostModelExecutionResponse
		if err := json.Unmarshal(rawResp, &resp); err != nil {
			return errorEnvelope("plugin_error", "解析 host.model.execute 响应失败: "+err.Error()), nil
		}
		if resp.StatusCode >= 400 {
			cd := p.rt.recordCooldown(member, resp.StatusCode, "", p.cfg)
			p.rt.recordFailure(member, resp.StatusCode)
			lastStatus = orStatus(resp.StatusCode, lastStatus)
			recordAttemptTrace(trace, attemptTrace{
				Member: member, Status: resp.StatusCode,
				CooldownS: int(cd.Seconds()),
				LatencyMS: int(p.rt.now().Sub(attemptStart).Milliseconds()),
			})
			summaries = append(summaries, fmt.Sprintf("%s: %d", member, resp.StatusCode))
			p.log.warn("attempt failed", map[string]any{"group": g.Name, "member": member, "status": resp.StatusCode, "cooldown": int(cd.Seconds())})
			continue
		}

		// Success.
		elapsed := p.rt.now().Sub(attemptStart)
		p.rt.recordSuccess(member, elapsed)
		p.rt.recordGroupSuccess(g.Name)
		recordAttemptTrace(trace, attemptTrace{
			Member: member, Status: http.StatusOK,
			LatencyMS: int(elapsed.Milliseconds()),
		})
		p.log.info("ok", map[string]any{"group": g.Name, "member": member, "latency_ms": p.rt.now().Sub(attemptStart).Milliseconds()})
		return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers})
	}

	// All members failed.
	p.rt.recordGroupFail(g.Name)
	msg := fmt.Sprintf("组 %s 全部成员失败: %s", g.Name, strings.Join(summaries, "; "))
	finalStatus := lastStatus
	if finalStatus == 0 {
		finalStatus = http.StatusBadGateway
	}
	return pluginabi.NewErrorEnvelope("group_exhausted", msg, finalStatus)
}

// entryProtocol normalizes a source format to a valid protocol, defaulting to
// openai (§8.2).
func entryProtocol(s string) string {
	if strings.TrimSpace(s) == "" {
		return "openai"
	}
	return s
}

func orStatus(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

func attemptSummary(status int, errMsg string, cd time.Duration) string {
	if status != 0 {
		return fmt.Sprintf("%d(冷却 %ds)", status, int(cd.Seconds()))
	}
	return fmt.Sprintf("错误(冷却 %ds)", int(cd.Seconds()))
}
