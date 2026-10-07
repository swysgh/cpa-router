package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// rpcStreamEmitRequest is the host.stream.emit payload.
type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

// rpcStreamCloseRequest is the host.stream.close payload.
type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// streamForwardCtx carries everything needed to re-enter the host for a
// degraded (post-preflight) attempt. Without it a degraded stream would send an
// empty body / wrong protocol upstream.
type streamForwardCtx struct {
	Body     []byte
	Protocol string
	Headers  http.Header
	Query    url.Values
	Alt      string
}

// executeStream implements executor.execute_stream (§8.4).
func (p *plugin) executeStream(raw []byte) ([]byte, error) {
	return p.executeStreamTraced(raw, nil)
}

// executeStreamTraced is executeStream plus an optional preflight trace
// recorder. Only the synchronous preflight is traced: the forwarding that
// follows runs in a goroutine after this returns, so recording there would race
// with the caller reading the trace.
func (p *plugin) executeStreamTraced(raw []byte, trace *[]attemptTrace) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	pluginStreamID := strings.TrimSpace(req.StreamID)
	if pluginStreamID == "" {
		return errorEnvelope("plugin_error", "stream_id is required for executor.execute_stream"), nil
	}

	p.reloadIfNeeded()

	startTime := p.rt.now()

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

	body := req.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	fwd := streamForwardCtx{
		Body:     body,
		Protocol: entryProtocol(req.SourceFormat),
		Headers:  req.Headers,
		Query:    req.Query,
		Alt:      req.Alt,
	}

	// Synchronous pre-check: try each member's host stream until one returns 2xx.
	// Doing this before returning headers means an exhausted group surfaces a real
	// HTTP status to the client instead of a 200 SSE that dies immediately.
	hitStreamID := ""
	hitIndex := -1
	lastStatus := 0
	for i, member := range attempts {
		if p.cfg.TotalTimeoutDur > 0 && p.rt.now().Sub(startTime) > p.cfg.TotalTimeoutDur {
			break
		}
		streamID, ok := p.openMemberStream(g, member, fwd, req.HostCallbackID, "stream preflight", trace)
		if !ok {
			lastStatus = orStatus(p.rt.coolingInfo(member).LastStatus, lastStatus)
			continue
		}
		hitStreamID = streamID
		hitIndex = i
		break
	}

	if hitStreamID == "" || hitIndex < 0 {
		p.rt.recordGroupFail(g.Name)
		finalStatus := http.StatusBadGateway
		if lastStatus != 0 {
			finalStatus = lastStatus
		}
		return pluginabi.NewErrorEnvelope("group_exhausted", fmt.Sprintf("组 %s 全部成员流式预检失败", g.Name), finalStatus)
	}

	// Return headers immediately (no chunks); the goroutine owns the rest.
	respHeader, err := okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
	if err != nil {
		p.closeHostModelStream(hitStreamID)
		return nil, err
	}

	go p.forwardStream(g, attempts, hitIndex, hitStreamID, fwd, pluginStreamID, req.HostCallbackID)

	return respHeader, nil
}

// openMemberStream opens a host model stream for one member. On failure it
// records stats/cooldown and returns ok=false. It is used both by the
// synchronous preflight and by the degrade path.
func (p *plugin) openMemberStream(g *group, member string, fwd streamForwardCtx, hostCallbackID, phase string, trace *[]attemptTrace) (string, bool) {
	p.rt.recordAttempt(member)
	attemptStart := p.rt.now()
	rawResp, callErr := p.callWithTimeout(pluginabi.MethodHostModelExecuteStream, hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: fwd.Protocol,
			ExitProtocol:  fwd.Protocol,
			Model:         member,
			Stream:        true,
			Body:          rewriteModel(fwd.Body, member),
			Headers:       fwd.Headers,
			Query:         fwd.Query,
			Alt:           fwd.Alt,
		},
		HostCallbackID: hostCallbackID,
	}, p.cfg.AttemptTimeoutDur)

	if callErr != nil {
		status := httpStatusFromError(callErr)
		cd := p.rt.recordCooldown(member, status, callErr.Error(), p.cfg)
		p.rt.recordFailure(member, status)
		recordAttemptTrace(trace, attemptTrace{
			Member: member, Status: status, Error: callErr.Error(),
			CooldownS: int(cd.Seconds()),
			LatencyMS: int(p.rt.now().Sub(attemptStart).Milliseconds()),
		})
		p.log.warn(phase+" failed", map[string]any{"group": g.Name, "member": member, "status": status, "cooldown": int(cd.Seconds())})
		return "", false
	}
	var resp pluginapi.HostModelStreamResponse
	if err := json.Unmarshal(rawResp, &resp); err != nil {
		p.rt.recordFailure(member, 0)
		recordAttemptTrace(trace, attemptTrace{
			Member: member, Status: 0, Error: "解析 host 流响应失败: " + err.Error(),
			LatencyMS: int(p.rt.now().Sub(attemptStart).Milliseconds()),
		})
		p.log.warn(phase+" failed", map[string]any{"group": g.Name, "member": member, "error": "decode host stream response"})
		return "", false
	}
	if resp.StatusCode >= 400 {
		cd := p.rt.recordCooldown(member, resp.StatusCode, "", p.cfg)
		p.rt.recordFailure(member, resp.StatusCode)
		if strings.TrimSpace(resp.StreamID) != "" {
			p.closeHostModelStream(resp.StreamID)
		}
		recordAttemptTrace(trace, attemptTrace{
			Member: member, Status: resp.StatusCode,
			CooldownS: int(cd.Seconds()),
			LatencyMS: int(p.rt.now().Sub(attemptStart).Milliseconds()),
		})
		p.log.warn(phase+" failed", map[string]any{"group": g.Name, "member": member, "status": resp.StatusCode, "cooldown": int(cd.Seconds())})
		return "", false
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		p.rt.recordFailure(member, resp.StatusCode)
		recordAttemptTrace(trace, attemptTrace{
			Member: member, Status: resp.StatusCode, Error: "host 未返回 stream_id",
			LatencyMS: int(p.rt.now().Sub(attemptStart).Milliseconds()),
		})
		p.log.warn(phase+" failed", map[string]any{"group": g.Name, "member": member, "error": "empty stream_id"})
		return "", false
	}
	recordAttemptTrace(trace, attemptTrace{
		Member: member, Status: resp.StatusCode,
		LatencyMS: int(p.rt.now().Sub(attemptStart).Milliseconds()),
	})
	return resp.StreamID, true
}

// forwardStream relays the already-open host stream (attempts[hitIndex]) to the
// plugin stream. While no payload has been delivered it may degrade to the
// remaining members; after the first payload the result is terminal (§8.4).
func (p *plugin) forwardStream(g *group, attempts []string, hitIndex int, firstStreamID string, fwd streamForwardCtx, pluginStreamID, hostCallbackID string) {
	defer func() {
		if rec := recover(); rec != nil {
			p.closePluginStream(pluginStreamID, fmt.Sprintf("stream orchestration panic: %v", rec))
		}
	}()

	streamID := strings.TrimSpace(firstStreamID)
	for i := hitIndex; i < len(attempts); i++ {
		member := attempts[i]
		if streamID == "" {
			id, ok := p.openMemberStream(g, member, fwd, hostCallbackID, "stream degrade", nil)
			if !ok {
				continue
			}
			streamID = id
		}

		emitted, failedEarly := p.relayMemberStream(g, member, streamID, pluginStreamID)
		p.closeHostModelStream(streamID)
		streamID = ""

		if !failedEarly {
			// Stream finished (normally, or with an error after the first payload).
			return
		}
		if emitted {
			return
		}
		// No payload was delivered and the plugin stream is still open: cool the
		// member down and try the next one.
		cd := p.rt.recordCooldown(member, 0, "stream read failed before first payload", p.cfg)
		p.rt.recordFailure(member, 0)
		p.log.warn("stream member failed before first payload", map[string]any{
			"group": g.Name, "member": member, "cooldown": int(cd.Seconds()),
		})
	}
	p.closePluginStream(pluginStreamID, "整组流式尝试失败")
}

// relayMemberStream streams one host model stream into the plugin stream.
//
// It returns failedEarly=true only when the stream failed before any payload was
// emitted AND the plugin stream was left open (so the caller may degrade to the
// next member). In every other case the plugin stream is closed and the result
// is terminal.
func (p *plugin) relayMemberStream(g *group, member, hostStreamID, pluginStreamID string) (emitted bool, failedEarly bool) {
	for {
		rawChunk, readErr := p.host.Call(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: hostStreamID})
		if readErr != nil {
			return p.finishMemberStream(pluginStreamID, emitted, readErr.Error())
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if err := json.Unmarshal(rawChunk, &chunk); err != nil {
			return p.finishMemberStream(pluginStreamID, emitted, err.Error())
		}
		if chunk.Error != "" {
			return p.finishMemberStream(pluginStreamID, emitted, chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			if !emitted {
				emitted = true
				p.rt.recordSuccess(member, 0)
				p.rt.recordGroupSuccess(g.Name)
			}
			if err := p.emitPluginStreamChunk(pluginStreamID, bytes.Clone(chunk.Payload)); err != nil {
				p.closePluginStream(pluginStreamID, err.Error())
				return true, false
			}
		}
		if chunk.Done {
			p.closePluginStream(pluginStreamID, "")
			return emitted, false
		}
	}
}

// finishMemberStream terminates a member stream that ended with errMsg.
func (p *plugin) finishMemberStream(pluginStreamID string, emitted bool, errMsg string) (bool, bool) {
	if !emitted {
		// Leave the plugin stream open: the caller may degrade to another member.
		return false, true
	}
	p.closePluginStream(pluginStreamID, errMsg)
	return true, false
}

// ---- host stream / plugin stream helpers ----

func (p *plugin) emitPluginStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	_, err := p.host.Call(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return err
}

func (p *plugin) closePluginStream(streamID, errMsg string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = p.host.Call(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errMsg),
	})
}

func (p *plugin) closeHostModelStream(streamID string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = p.host.Call(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
}
