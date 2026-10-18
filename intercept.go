package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// modelListSourceFormat is the only source format whose model list we rewrite.
// The Claude and Gemini handlers build their own shapes, and guessing at them
// would mean rewriting a payload we do not own.
const modelListSourceFormat = "openai"

// internalRefererMarkers identify requests made by the host's own management UI:
// the plugin panel is rendered inside /v0/resource/plugins/..., and CPAMP lives
// under /management. Those pages call /v1/models to populate model pickers, so
// trimming their answer would hide exactly the candidates an operator needs.
var internalRefererMarkers = []string{"/management", "/v0/resource/plugins/", "/v0/management/"}

// internalUserAgents are optional markers a panel build may send. The host adds
// none today, so this is a reserved hook, not the primary signal.
var internalUserAgents = []string{"cpa-router-panel", "cpamp-panel"}

// isInternalRequest reports whether the request came from a management console
// rather than from an API client.
//
// The interceptor payload carries no request URL (CPA v8.0.13), so the Referer
// of the calling page is the only reliable discriminator available.
func isInternalRequest(headers http.Header) bool {
	if headers == nil {
		return false
	}
	if referer := headers.Get("Referer"); referer != "" {
		for _, marker := range internalRefererMarkers {
			if strings.Contains(referer, marker) {
				return true
			}
		}
	}
	if ua := headers.Get("User-Agent"); ua != "" {
		for _, marker := range internalUserAgents {
			if strings.Contains(ua, marker) {
				return true
			}
		}
	}
	return false
}

// rpcResponseInterceptRequest mirrors the host's flattened RPC payload for
// response.intercept_after: the SDK request fields promoted inline plus
// host_callback_id (see pluginhost/rpc_schema.go). The SDK types carry no JSON
// tags, so the wire keys are the Go field names and Body is base64 ([]byte).
type rpcResponseInterceptRequest struct {
	pluginapi.ResponseInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// interceptResponse implements response.intercept_after: it trims the host's
// /v1/models payload down to the group call names this plugin advertises, so a
// client listing models sees the groups rather than every upstream model behind
// them.
//
// Fail-open is the contract. The host replaces the response body only when we
// return a non-empty one, and it skips this plugin entirely when we return an
// error, so every path that cannot prove the body is an OpenAI model list
// returns an empty modification and leaves the host's own bytes alone.
func (p *plugin) interceptResponse(raw []byte) ([]byte, error) {
	var req rpcResponseInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		p.log.warn("response.intercept_after 请求无法解析，响应原样放行", map[string]any{"error": err.Error()})
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}

	// Pick up state-file edits before deciding the allow-list, exactly like
	// model.route: a group saved a moment ago must be listable immediately.
	p.reloadIfNeeded()

	filtered, changed := p.filterModelList(req.SourceFormat, req.RequestHeaders, req.Body)
	if !changed {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	p.log.debug("已过滤 /v1/models", map[string]any{
		"before_bytes": len(req.Body),
		"after_bytes":  len(filtered),
	})
	return okEnvelope(pluginapi.ResponseInterceptResponse{Body: filtered})
}

// filterModelList returns the rewritten body and whether anything changed.
//
// It only touches bodies it can prove are OpenAI model lists: a JSON object
// whose top-level "object" is "list" and whose "data" is an array. Another
// source format, a chat/completions response, truncated or invalid JSON, a
// missing/non-array "data", or an entry without a readable id all report
// "unchanged", which leaves the host's bytes exactly as they were.
//
// Requests coming from a management console are never trimmed: their pickers
// need the full upstream catalog, and hiding it only pushes the operator back
// to guessing model names by hand.
func (p *plugin) filterModelList(sourceFormat string, headers http.Header, body []byte) ([]byte, bool) {
	if sourceFormat != modelListSourceFormat {
		return nil, false
	}
	if isInternalRequest(headers) {
		return nil, false
	}

	p.mu.RLock()
	enabled := p.cfg.FilterModels
	p.mu.RUnlock()
	if !enabled {
		return nil, false
	}

	// Decode into raw fields so every key we do not touch (the root's siblings,
	// and each model's created / owned_by / anything the host adds later)
	// survives verbatim.
	//
	// This runs for every successful non-streaming response (the host routes
	// them all through response.intercept_after), so it is deliberately a plain
	// parse with no heuristic pre-gate: json.RawMessage aliases the input bytes,
	// so the cost is one pass over the body and a handful of map entries, and a
	// substring guess could only ever skip work by mistake (a model list whose
	// "list" is escaped would silently go unfiltered).
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false
	}
	var object string
	if rawObject, ok := root["object"]; !ok || json.Unmarshal(rawObject, &object) != nil || object != "list" {
		return nil, false
	}
	rawData, ok := root["data"]
	if !ok {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawData, &items); err != nil {
		return nil, false
	}

	allow := p.allowedModelIDs()
	kept := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var entry struct {
			ID string `json:"id"`
		}
		// An entry we cannot read an id from is never proven to be ours, so it
		// stays: hiding another provider's model is not this plugin's job.
		if err := json.Unmarshal(item, &entry); err != nil || entry.ID == "" || allow[entry.ID] {
			kept = append(kept, item)
		}
	}
	if len(kept) == len(items) {
		// Nothing to drop — hand back the original bytes untouched.
		return nil, false
	}

	data, err := json.Marshal(kept)
	if err != nil {
		return nil, false
	}
	root["data"] = data
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false
	}
	return out, true
}

// allowedModelIDs is the set of call names this plugin advertises as models: the
// name_prefix-prefixed name and aliases of every enabled group. It comes from the
// same iterator model.register uses, so a name that is not advertised is never
// kept either.
func (p *plugin) allowedModelIDs() map[string]bool {
	snap := p.callNameSnapshot()
	allow := make(map[string]bool, 2*len(snap.groups))
	snap.forEachEnabledCallName(func(id string, _ group) { allow[id] = true })
	return allow
}
