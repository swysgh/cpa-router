package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// managementRegistrationJSON returns the management.register payload (§10.1).
func (p *plugin) managementRegistrationJSON() map[string]any {
	return map[string]any{
		"resources": []map[string]any{
			{
				"Path":        "/panel",
				"Menu":        "模型路由",
				"Description": "命名模型组：轮询/降级/嵌套，含 429 冷却与测试",
			},
		},
		"routes": []map[string]any{
			{"Method": "GET", "Path": "/plugins/cpa-router/state"},
			{"Method": "GET", "Path": "/plugins/cpa-router/models"},
			{"Method": "PUT", "Path": "/plugins/cpa-router/groups"},
			{"Method": "POST", "Path": "/plugins/cpa-router/groups"},
			{"Method": "DELETE", "Path": "/plugins/cpa-router/groups"},
			{"Method": "POST", "Path": "/plugins/cpa-router/reset"},
			{"Method": "POST", "Path": "/plugins/cpa-router/test"},
		},
	}
}

// managementRequest is the authenticated management request envelope.
type managementRequest struct {
	Method         string              `json:"method"`
	Path           string              `json:"path"`
	Headers        http.Header         `json:"headers"`
	Query          map[string][]string `json:"query"`
	Body           []byte              `json:"body"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

// handleManagement dispatches management.handle (§10). It returns a
// ManagementResponse whose Body carries the JSON/HTML payload; the abi layer
// wraps it in an ok envelope for the host.
func (p *plugin) handleManagement(raw []byte) (*pluginapi.ManagementResponse, error) {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("解析管理请求失败: %w", err)
	}
	p.reloadIfNeeded()

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path := strings.TrimSpace(req.Path)
	// Strip any query string from the path for routing, but fold it into req.Query
	// so handlers (e.g. DELETE ?name=) can read query parameters.
	if i := strings.IndexByte(path, '?'); i >= 0 {
		if q, err := url.ParseQuery(path[i+1:]); err == nil {
			if req.Query == nil {
				req.Query = q
			} else {
				for k, vv := range q {
					req.Query[k] = vv
				}
			}
		}
		path = path[:i]
	}

	// The host hands the plugin the *full* request path (verified against
	// CLIProxyAPI v8.0.13: a call to /v0/management/plugins/cpa-router/state
	// arrives here verbatim), not the path declared in management.register.
	// Strip the management prefix so the switch below can match the declared
	// routes. Both prefixes are accepted so the plugin keeps working if the
	// host is mounted under a different base.
	for _, prefix := range []string{"/v0/management", "/management"} {
		if strings.HasPrefix(path, prefix+"/") {
			path = strings.TrimPrefix(path, prefix)
			break
		}
	}

	// Resource page: GET with no side effects (§10.1/.3).
	if method == "GET" && (path == "/panel" || strings.HasSuffix(path, "/panel")) {
		return servePanel()
	}

	switch {
	case method == "GET" && path == "/plugins/cpa-router/state":
		return p.handleState(req)
	case method == "GET" && path == "/plugins/cpa-router/models":
		return p.handleModelsList(req)
	case method == "PUT" && path == "/plugins/cpa-router/groups":
		return p.handleGroupsPut(req)
	case method == "POST" && path == "/plugins/cpa-router/groups":
		return p.handleGroupsPost(req)
	case method == "DELETE" && path == "/plugins/cpa-router/groups":
		return p.handleGroupsDelete(req)
	case method == "POST" && path == "/plugins/cpa-router/reset":
		return p.handleReset(req)
	case method == "POST" && path == "/plugins/cpa-router/test":
		return p.handleTest(req)
	default:
		return mgmtResponse(http.StatusNotFound, map[string]any{"ok": false, "error": "未知路径: " + method + " " + path}), nil
	}
}

// mgmtResponse builds a JSON ManagementResponse.
func mgmtResponse(status int, body any) *pluginapi.ManagementResponse {
	raw, _ := json.Marshal(body)
	return &pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

// mgmtResponseRaw builds a ManagementResponse with an already-encoded body.
func mgmtResponseRaw(status int, body []byte) *pluginapi.ManagementResponse {
	return &pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

// ---- GET /state ----

func (p *plugin) handleState(req managementRequest) (*pluginapi.ManagementResponse, error) {
	p.mu.RLock()
	cfg := p.cfg
	absPath := p.statePath
	p.mu.RUnlock()

	var mtime int64
	exists := false
	if p.store != nil {
		mt, ex := p.store.fileModTime()
		exists = ex
		if ex {
			mtime = mt.Unix()
		}
	}

	groups := p.allGroupsSnapshot()
	gstats := p.rt.snapshotGroupStats() // key = 组内部名 g.Name
	gtokens := p.rt.snapshotGroupTokens()
	groupView := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		members := make([]map[string]any, 0, len(g.Members))
		for _, m := range g.Members {
			entry := map[string]any{"enabled": m.Enabled}
			if m.IsModel {
				entry["type"] = "model"
				entry["name"] = m.Model
			} else {
				entry["type"] = "group"
				entry["name"] = m.Group
			}
			members = append(members, entry)
		}
		gs := gstats[g.Name]
		gt := gtokens[g.Name]
		groupView = append(groupView, map[string]any{
			"name":        g.Name,
			"call_name":   p.cfg.NamePrefix + g.Name,
			"strategy":    g.Strategy,
			"enabled":     g.Enabled,
			"description": g.Description,
			"aliases":     g.Aliases,
			"members":     members,
			"group_total": gs.Total,
			"group_ok":    gs.OK,
			"group_fail":  gs.Fail,
			// Upstream-reported token usage of every request the client
			// addressed to this group (nested groups are credited to the group
			// that was called, so these sums never double-count).
			"group_input_tokens":  gt.Input,
			"group_output_tokens": gt.Output,
			"group_total_tokens":  gt.Total,
			"group_token_reqs":    gt.Reqs,
		})
	}

	models := map[string]any{}
	cooldowns := p.rt.snapshotCooldowns()
	stats := p.rt.snapshotModels()
	tokens := p.rt.snapshotTokens()
	now := p.rt.now()
	for name, st := range stats {
		cd := cooldowns[name]
		cdSecs := 0
		if cd.Until.After(now) {
			cdSecs = int(cd.Until.Sub(now).Seconds())
		}
		var avgLatency int64
		if st.LatencyN > 0 {
			avgLatency = st.Latency.Milliseconds() / st.LatencyN
		}
		models[name] = map[string]any{
			"cooldown_until": cd.Until.Unix(),
			"cooldown_secs":  cdSecs,
			"cooldown_level": cd.Level,
			"last_status":    cd.LastStatus,
			"last_error":     cd.LastError,
			"total":          st.Total,
			"ok":             st.OK,
			"fail":           st.Fail,
			"status_429":     st.Status429,
			"status_5xx":     st.Status5xx,
			"last_used_at":   st.LastUsed.Unix(),
			"avg_latency_ms": avgLatency,
		}
	}
	// Members that only ever reported usage (no cooldown/attempt bookkeeping yet)
	// still deserve an entry so the panel can show their token counters.
	for name, tk := range tokens {
		entry, ok := models[name].(map[string]any)
		if !ok {
			entry = map[string]any{}
			models[name] = entry
		}
		entry["input_tokens"] = tk.Input
		entry["output_tokens"] = tk.Output
		entry["total_tokens"] = tk.Total
		entry["token_reqs"] = tk.Reqs
		entry["tokens_last_at"] = tk.LastUsed.Unix()
	}

	body := map[string]any{
		"version": 1,
		"plugin":  map[string]any{"version": "0.8.0", "name": "cpa-router"},
		"config": map[string]any{
			"state_file":           cfg.StateFile,
			"stats_file":           cfg.StatsFile,
			"stats_flush_interval": cfg.StatsFlushInterval,
			"name_prefix":          cfg.NamePrefix,
			"reload_interval":      cfg.ReloadInterval,
			"max_attempts":         cfg.MaxAttempts,
			"attempt_timeout":      cfg.AttemptTimeout,
			"total_timeout":        cfg.TotalTimeout,
			"all_cooling_policy":   cfg.AllCoolingPolicy,
			"max_wait":             cfg.MaxWait,
			"log_level":            cfg.LogLevel,
			"filter_models":        cfg.FilterModels,
			"cooldown":             cfg.Cooldown,
		},
		"state_file": map[string]any{
			"path":   absPath,
			"exists": exists,
			"mtime":  mtime,
		},
		"groups": groupsView(groupView),
		"models": models,
		"now":    now.Unix(),
	}
	return mgmtResponse(http.StatusOK, body), nil
}

func groupsView(v []map[string]any) []map[string]any { return v }

// ---- GET /models ----

// handleModelsList returns every model name the host can actually serve, plus
// this plugin's own group call names.
//
// Why it exists: /v1/models is trimmed to the group call names (see intercept.go),
// so a management console asking the host for "all models" gets a list that is
// useless for building groups. The interceptor already lets console requests
// through, but a console cannot always reach /v1/models with a usable API key —
// this endpoint answers the same question with the management key the panel
// already holds.
//
// The catalog is assembled from three sources:
//
//   - the credential files (host.auth.list + host.auth.get): the "models" field
//     they declare, prefixed with the credential's own prefix;
//   - the registry cache (intercept.go): the ids of the last model list the host
//     produced. This is the only source that covers OAuth credentials whose model
//     list lives in provider metadata (antigravity: "anti/<model>"), which no
//     credential JSON and no config channel declares; it is empty until the host
//     answers a model list once (any client's /v1/models call fills it);
//   - this plugin's enabled group call names, from the same snapshot model.register
//     uses, so the picker never offers a group the plugin does not advertise.
func (p *plugin) handleModelsList(req managementRequest) (*pluginapi.ManagementResponse, error) {
	models, err := p.collectCredentialModels()
	if err != nil {
		p.log.warn("枚举凭证模型失败", map[string]any{"error": err.Error()})
		return mgmtResponse(http.StatusInternalServerError, map[string]any{"ok": false, "error": "枚举凭证模型失败: " + err.Error()}), nil
	}
	registry, registryAt := p.rt.registrySnapshot()
	for _, name := range registry {
		models[name] = true
	}
	p.callNameSnapshot().forEachEnabledCallName(func(id string, _ group) { models[id] = true })

	out := make([]string, 0, len(models))
	for name := range models {
		out = append(out, name)
	}
	sort.Strings(out)
	body := map[string]any{"models": out}
	// The panel shows how fresh the registry part is, so an empty catalog right
	// after a restart reads as "host has not listed models yet" instead of a bug.
	if len(registry) > 0 {
		body["registry_seen_at"] = registryAt.Unix()
		body["registry_models"] = len(registry)
	}
	return mgmtResponse(http.StatusOK, body), nil
}

// collectCredentialModels enumerates the model names declared by every host
// credential. Credentials that cannot be read (runtime-only entries generated
// from config.yaml have no backing file) are skipped: they carry no model list
// either, so skipping them loses nothing.
func (p *plugin) collectCredentialModels() (map[string]bool, error) {
	raw, err := p.host.Call(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var list struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("解析凭证列表失败: %w", err)
	}

	models := make(map[string]bool, 2*len(list.Files))
	for _, entry := range list.Files {
		index := strings.TrimSpace(entry.AuthIndex)
		if index == "" {
			continue
		}
		// A disabled credential is unregistered by the host, so none of its models
		// is callable; listing them would only hand the picker dead candidates.
		if entry.Disabled {
			continue
		}
		rawAuth, err := p.host.Call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: index})
		if err != nil {
			p.log.debug("跳过无法读取的凭证", map[string]any{"auth_index": index, "name": entry.Name, "error": err.Error()})
			continue
		}
		var got pluginapi.HostAuthGetResponse
		if err := json.Unmarshal(rawAuth, &got); err != nil {
			p.log.debug("跳过凭证 JSON 无法解析的条目", map[string]any{"auth_index": index, "error": err.Error()})
			continue
		}
		for _, name := range modelsFromCredentialJSON(got.JSON) {
			models[name] = true
		}
	}
	return models, nil
}

// modelsFromCredentialJSON pulls the model names out of one credential's JSON.
//
// The "models" field is either a plain string array or an array of
// {name, alias} objects (both shapes exist in CPA config/auth files). An alias
// is a callable name too, so it is kept alongside the upstream name.
//
// A credential may also carry a channel prefix (`"prefix": "anti"`). The host
// applies it to every model it registers for that credential, and with
// force-model-prefix on the bare id is not registered at all, so the prefix has
// to be part of the returned name -- otherwise every candidate is a 400.
func modelsFromCredentialJSON(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	rawModels, ok := root["models"]
	if !ok {
		return nil
	}
	prefix := credentialPrefix(root)
	var names []string
	if err := json.Unmarshal(rawModels, &names); err == nil {
		return applyCredentialPrefix(cleanModelNames(names), prefix)
	}
	var entries []struct {
		Name  string `json:"name"`
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(rawModels, &entries); err != nil {
		return nil
	}
	for _, e := range entries {
		names = append(names, e.Name, e.Alias)
	}
	return applyCredentialPrefix(cleanModelNames(names), prefix)
}

// credentialPrefix reads the credential's channel prefix (the same field the host
// feeds into applyModelPrefixes). Blank or non-string values mean "no prefix".
func credentialPrefix(root map[string]json.RawMessage) string {
	rawPrefix, ok := root["prefix"]
	if !ok {
		return ""
	}
	var prefix string
	if err := json.Unmarshal(rawPrefix, &prefix); err != nil {
		return ""
	}
	return strings.TrimSpace(prefix)
}

// applyCredentialPrefix turns a credential's model ids into callable names: the
// prefixed id when the credential has a prefix, the bare id otherwise.
func applyCredentialPrefix(names []string, prefix string) []string {
	if prefix == "" {
		return names
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, prefix+"/"+name)
	}
	return out
}

// cleanModelNames drops blank entries and trims the rest, so a padded name in a
// credential file never becomes a picker candidate that fails to route.
func cleanModelNames(in []string) []string {
	out := make([]string, 0, len(in))
	for _, name := range in {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// ---- JSON request decoding ----
//
// encoding/json does not run UnmarshalYAML, so decoding a request body straight
// into rawGroup/rawMember loses the presence-aware "enabled" semantics: the
// enabledSet flag stays false, memberEnabled() then reports every member as
// enabled, and an explicit `enabled: false` from the UI is silently ignored.
// These mirrors decode through *bool so presence survives, and toRaw()
// re-applies the same opt-out rule the YAML decoder uses.

type jsonMember struct {
	Model   string `json:"model"`
	Group   string `json:"group"`
	Enabled *bool  `json:"enabled"`
}

type jsonGroup struct {
	Name        string       `json:"name"`
	Strategy    string       `json:"strategy"`
	Enabled     *bool        `json:"enabled"`
	Description string       `json:"description"`
	Aliases     []string     `json:"aliases"`
	Members     []jsonMember `json:"members"`
}

func (m jsonMember) toRaw() rawMember {
	return rawMember{
		Model:      m.Model,
		Group:      m.Group,
		Enabled:    m.Enabled == nil || *m.Enabled,
		enabledSet: true,
	}
}

func (g jsonGroup) toRaw() rawGroup {
	out := rawGroup{
		Name:        g.Name,
		Strategy:    g.Strategy,
		Enabled:     g.Enabled == nil || *g.Enabled,
		Description: g.Description,
		Aliases:     g.Aliases,
		enabledSet:  true,
	}
	for _, m := range g.Members {
		out.Members = append(out.Members, m.toRaw())
	}
	return out
}

// ---- PUT /groups (full replace) ----

func (p *plugin) handleGroupsPut(req managementRequest) (*pluginapi.ManagementResponse, error) {
	var in struct {
		Groups []jsonGroup `json:"groups"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return mgmtResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON: " + err.Error()}), nil
	}
	groups := make([]rawGroup, 0, len(in.Groups))
	for _, g := range in.Groups {
		groups = append(groups, g.toRaw())
	}
	f := groupFile{Version: 1, Groups: groups}
	if err := p.applyGroups(f); err != nil {
		return mgmtResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()}), nil
	}
	return mgmtResponse(http.StatusOK, map[string]any{"ok": true, "groups": len(p.allGroupsSnapshot())}), nil
}

// ---- POST /groups (add or replace by name) ----

func (p *plugin) handleGroupsPost(req managementRequest) (*pluginapi.ManagementResponse, error) {
	var in struct {
		Group jsonGroup `json:"group"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return mgmtResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON: " + err.Error()}), nil
	}
	rg := in.Group.toRaw()
	// Merge into the current group set, replacing by name.
	current := p.currentGroupFile()
	replaced := false
	for i := range current.Groups {
		if strings.TrimSpace(current.Groups[i].Name) == strings.TrimSpace(rg.Name) {
			current.Groups[i] = rg
			replaced = true
			break
		}
	}
	if !replaced {
		current.Groups = append(current.Groups, rg)
	}
	if err := p.applyGroups(current); err != nil {
		return mgmtResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()}), nil
	}
	return mgmtResponse(http.StatusOK, map[string]any{"ok": true, "groups": len(p.allGroupsSnapshot())}), nil
}

// ---- DELETE /groups?name=x ----

func (p *plugin) handleGroupsDelete(req managementRequest) (*pluginapi.ManagementResponse, error) {
	name := ""
	if req.Query != nil {
		if v, ok := req.Query["name"]; ok && len(v) > 0 {
			name = strings.TrimSpace(v[0])
		}
	}
	if name == "" {
		return mgmtResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 name 参数"}), nil
	}
	current := p.currentGroupFile()
	filtered := current.Groups[:0]
	found := false
	for _, g := range current.Groups {
		if strings.TrimSpace(g.Name) == name {
			found = true
			continue
		}
		filtered = append(filtered, g)
	}
	if !found {
		return mgmtResponse(http.StatusNotFound, map[string]any{"ok": false, "error": "组不存在: " + name}), nil
	}
	current.Groups = filtered
	if err := p.applyGroups(current); err != nil {
		return mgmtResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()}), nil
	}
	return mgmtResponse(http.StatusOK, map[string]any{"ok": true}), nil
}

// ---- POST /reset ----

func (p *plugin) handleReset(req managementRequest) (*pluginapi.ManagementResponse, error) {
	var in struct {
		Name string `json:"name"`
		What string `json:"what"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return mgmtResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON: " + err.Error()}), nil
	}
	what := strings.TrimSpace(in.What)
	if what != "cooldown" && what != "stats" && what != "all" {
		what = "all"
	}
	cleared := p.rt.reset(strings.TrimSpace(in.Name), what)
	return mgmtResponse(http.StatusOK, map[string]any{"ok": true, "cleared": cleared}), nil
}

// ---- POST /test ----

func (p *plugin) handleTest(req managementRequest) (*pluginapi.ManagementResponse, error) {
	var in struct {
		Group  string `json:"group"`
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return mgmtResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON: " + err.Error()}), nil
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		prompt = "只回复两个字:收到"
	}
	callName := strings.TrimSpace(in.Group)
	if callName == "" {
		return mgmtResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 group 参数"}), nil
	}

	g, ok := p.resolveGroup(callName)
	if !ok {
		return mgmtResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "未找到组: " + callName}), nil
	}

	// Build a minimal executor request and run the real §8.3/§8.4 path.
	body := p.buildTestBody(g, in.Model, prompt)
	execReq := rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			Model:           callName,
			SourceFormat:    "openai",
			Stream:          in.Stream,
			OriginalRequest: body,
		},
		HostCallbackID: req.HostCallbackID,
	}

	start := p.rt.now()
	// Collect the real per-attempt trace from the execution path so the panel can
	// show which member was actually tried, with what status and cooldown.
	var trace []attemptTrace
	if in.Stream {
		_, _ = p.executeStreamTraced(marshalExec(execReq), &trace)
		// Only the synchronous preflight is traced; the forwarding that follows is
		// asynchronous and outlives this call.
		return mgmtResponse(http.StatusOK, map[string]any{
			"ok":           true,
			"status":       200,
			"latency_ms":   int(p.rt.now().Sub(start).Milliseconds()),
			"attempts":     traceAttempts(trace),
			"body_preview": "(流式测试只做同步预检，后续转发是异步的)",
		}), nil
	}

	raw, err := p.executeTraced(marshalExec(execReq), &trace)
	latency := int(p.rt.now().Sub(start).Milliseconds())
	if err != nil {
		return mgmtResponse(http.StatusOK, map[string]any{
			"ok":         false,
			"status":     0,
			"latency_ms": latency,
			"attempts":   traceAttempts(trace),
			"error":      err.Error(),
		}), nil
	}
	// A group whose members all failed comes back as an error envelope, not a Go
	// error. Report its message and status instead of parsing it as an empty
	// successful response.
	if _, msg, status, isErr := envelopeErrorInfo(raw); isErr {
		return mgmtResponse(http.StatusOK, map[string]any{
			"ok":         false,
			"status":     status,
			"latency_ms": latency,
			"attempts":   traceAttempts(trace),
			"error":      msg,
		}), nil
	}
	var resp pluginapi.ExecutorResponse
	if e := json.Unmarshal(unwrapResult(raw), &resp); e != nil {
		return mgmtResponse(http.StatusOK, map[string]any{
			"ok": false, "status": 0, "latency_ms": latency, "attempts": traceAttempts(trace), "error": "解析响应失败",
		}), nil
	}
	preview := string(resp.Payload)
	if len(preview) > 2000 {
		preview = preview[:2000]
	}
	return mgmtResponse(http.StatusOK, map[string]any{
		"ok":           true,
		"status":       200,
		"latency_ms":   latency,
		"attempts":     traceAttempts(trace),
		"body_preview": preview,
	}), nil
}

func marshalExec(req rpcExecutorRequest) []byte {
	raw, _ := json.Marshal(req)
	return raw
}

func unwrapResult(raw []byte) json.RawMessage {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return raw
	}
	if env.Result != nil {
		return env.Result
	}
	return raw
}

// traceAttempts renders the per-attempt trace collected during a test run.
func traceAttempts(trace []attemptTrace) []map[string]any {
	out := make([]map[string]any, 0, len(trace))
	for _, t := range trace {
		out = append(out, map[string]any{
			"member":     t.Member,
			"status":     t.Status,
			"error":      t.Error,
			"cooldown_s": t.CooldownS,
			"latency_ms": t.LatencyMS,
		})
	}
	return out
}

// envelopeErrorInfo reports whether raw is an error envelope, and if so returns
// its code, message and http status.
func envelopeErrorInfo(raw []byte) (code, message string, status int, isErr bool) {
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.OK || env.Error == nil {
		return "", "", 0, false
	}
	return env.Error.Code, env.Error.Message, env.Error.HTTPStatus, true
}

func (p *plugin) buildTestBody(g *group, override, prompt string) []byte {
	model := override
	if model == "" {
		// Use the first expanded member (or group call name fallback).
		if attempts, err := p.expand(g.Name, 1, map[string]bool{}, false); err == nil && len(attempts) > 0 {
			model = attempts[0]
		} else {
			model = g.Name
		}
	}
	payload := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	}
	raw, _ := json.Marshal(payload)
	return raw
}

// ---- group application helpers ----

func (p *plugin) currentGroupFile() groupFile {
	groups := p.allGroupsSnapshot()
	f := groupFile{Version: 1}
	for _, g := range groups {
		rg := rawGroup{
			Name:        g.Name,
			Strategy:    g.Strategy,
			Enabled:     g.Enabled,
			Description: g.Description,
			Aliases:     g.Aliases,
			// Enabled above is already the resolved effective value, so mark it
			// as explicitly set; otherwise memberEnabled() would force every
			// member back to enabled and a later POST /groups would silently
			// re-enable members the user had disabled.
			enabledSet: true,
		}
		for _, m := range g.Members {
			rm := rawMember{Enabled: m.Enabled, enabledSet: true}
			if m.IsModel {
				rm.Model = m.Model
			} else {
				rm.Group = m.Group
			}
			rg.Members = append(rg.Members, rm)
		}
		f.Groups = append(f.Groups, rg)
	}
	return f
}

// applyGroups validates, persists, and swaps the in-memory groups atomically.
func (p *plugin) applyGroups(f groupFile) error {
	f, notes := normalizeGroupRefs(f, p.cfg.NamePrefix)
	if len(notes) > 0 {
		p.log.info("组成员引用已归一化", map[string]any{"notes": notes})
	}
	groups, _, err := parseGroups(f, p.cfg.NamePrefix)
	if err != nil {
		return err
	}
	if err := validateModelMembers(groups, p.cfg.NamePrefix); err != nil {
		return err
	}
	if p.store != nil {
		if err := p.store.save(f); err != nil {
			return err
		}
	}
	nameIdx := buildGroupNameIndex(groups, p.cfg.NamePrefix)
	// Sort before publishing: readers copy p.groups under RLock only.
	sortGroupsByName(groups)
	p.mu.Lock()
	p.groups = groups
	p.nameIndex = nameIdx
	p.lastReload = p.rt.now()
	p.mu.Unlock()
	p.log.info("组配置已更新", map[string]any{"groups": len(groups)})
	return nil
}

// ensure pluginabi import is used (registered method names referenced in handlers).
var _ = pluginabi.MethodManagementHandle

func servePanel() (*pluginapi.ManagementResponse, error) {
	body := panelHTML()
	if body == nil {
		body = []byte("")
	}
	return &pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       body,
	}, nil
}
