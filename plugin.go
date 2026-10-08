package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// envelope is the host<->plugin RPC envelope (pure data, lives in a non-C file).
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// HTTPStatus surfaces the client-visible HTTP code (§8.3).
	HTTPStatus int `json:"http_status,omitempty"`
}

// plugin is the central state object. All host interaction goes through the
// hostCaller stored in `host`. Business logic is pure Go and testable.
type plugin struct {
	host hostCaller

	mu        sync.RWMutex
	cfg       config
	groups    []group
	nameIndex map[string]string
	store     *groupStore
	statePath string
	rt        *runtimeState
	log       *logger

	lastReload time.Time
}

// newPlugin constructs a plugin with the given host caller and default config.
func newPlugin(host hostCaller) *plugin {
	p := &plugin{
		host:      host,
		cfg:       defaultConfig(),
		rt:        newRuntimeState(),
		log:       &logger{host: host, level: "info"},
		nameIndex: map[string]string{},
	}
	return p
}

// logger filters by level and forwards to host.log.
type logger struct {
	host  hostCaller
	level string
}

var levelRank = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}

func (l *logger) enabled(level string) bool {
	lv, ok := levelRank[strings.ToLower(l.level)]
	if !ok {
		lv = 1
	}
	rv, ok := levelRank[strings.ToLower(level)]
	if !ok {
		rv = 1
	}
	return rv >= lv
}

func (l *logger) log(level, msg string, fields map[string]any) {
	if l == nil || l.host == nil {
		return
	}
	if !l.enabled(level) {
		return
	}
	payload := map[string]any{"level": strings.ToLower(level), "message": msg}
	if fields != nil {
		payload["fields"] = fields
	}
	_, _ = l.host.Call(pluginabi.MethodHostLog, payload)
}

func (l *logger) debug(msg string, fields map[string]any) { l.log("debug", msg, fields) }
func (l *logger) info(msg string, fields map[string]any)  { l.log("info", msg, fields) }
func (l *logger) warn(msg string, fields map[string]any)  { l.log("warn", msg, fields) }
func (l *logger) error(msg string, fields map[string]any) { l.log("error", msg, fields) }

// lifecycleRequest is the {config_yaml} envelope (raw YAML bytes).
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

// configure parses the lifecycle request, stores config, loads state, logs.
func (p *plugin) configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return fmt.Errorf("配置错误: config_yaml: 无法解析请求体: %w", err)
		}
	}
	cfg, err := decodeConfig(req.ConfigYAML)
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.cfg = cfg
	p.log.level = cfg.LogLevel
	if cfg.StateFile != "" {
		p.store = newGroupStore(cfg.StateFile)
	} else {
		p.store = nil
	}
	// Preserve existing runtime (cooldown/stats) across reconfigure.
	p.mu.Unlock()

	// Load state file (best-effort; keep empty groups on failure).
	p.reloadGroups()

	// Log effective config full text.
	p.mu.RLock()
	absPath := p.statePath
	cfgCopy := p.cfg
	p.mu.RUnlock()
	cfgYAML, _ := yaml.Marshal(cfgCopy)
	p.log.info("cpa-router 已加载", map[string]any{
		"groups":     len(p.groups),
		"state_file": absPath,
		"config":     string(cfgYAML),
	})
	return nil
}

// reloadGroups loads the state file, validates, and swaps the in-memory groups.
// On failure it logs a warning and keeps the previous groups.
func (p *plugin) reloadGroups() {
	p.mu.RLock()
	store := p.store
	p.mu.RUnlock()
	if store == nil {
		p.mu.Lock()
		p.groups = nil
		p.nameIndex = map[string]string{}
		p.statePath = ""
		p.lastReload = p.rt.now()
		p.mu.Unlock()
		return
	}
	f, abs, err := store.load()
	if err != nil {
		p.log.warn("组配置加载失败，保留上一份配置", map[string]any{"error": err.Error()})
		p.mu.Lock()
		p.lastReload = p.rt.now()
		p.mu.Unlock()
		return
	}
	f, notes := normalizeGroupRefs(f, p.cfg.NamePrefix)
	if len(notes) > 0 {
		p.log.info("组成员引用已归一化", map[string]any{"notes": notes})
	}
	groups, _, err := parseGroups(f, p.cfg.NamePrefix)
	if err == nil {
		err = validateModelMembers(groups, p.cfg.NamePrefix)
	}
	if err != nil {
		p.log.warn("组配置校验失败，保留上一份配置", map[string]any{"error": err.Error()})
		p.mu.Lock()
		p.lastReload = p.rt.now()
		p.mu.Unlock()
		return
	}
	nameIdx := buildGroupNameIndex(groups, p.cfg.NamePrefix)
	p.mu.Lock()
	p.groups = groups
	p.nameIndex = nameIdx
	p.statePath = abs
	p.lastReload = p.rt.now()
	p.mu.Unlock()
	p.log.info("组配置已热加载", map[string]any{"groups": len(groups)})
}

// reloadIfNeeded performs throttled mtime-based hot reload.
func (p *plugin) reloadIfNeeded() {
	p.mu.RLock()
	interval := p.cfg.ReloadIntervalDur
	store := p.store
	last := p.lastReload
	p.mu.RUnlock()
	if interval <= 0 || store == nil {
		return
	}
	now := p.rt.now()
	if now.Sub(last) < interval {
		return
	}
	mt, exists := store.fileModTime()
	if !exists {
		return
	}
	if mt.After(last) {
		p.reloadGroups()
	}
}

// resolveGroup maps a call name (with prefix) to a group.
func (p *plugin) resolveGroup(callName string) (*group, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	name, ok := p.nameIndex[callName]
	if !ok {
		return nil, false
	}
	for i := range p.groups {
		if p.groups[i].Name == name {
			return &p.groups[i], true
		}
	}
	return nil, false
}

// groupByName returns a group by its internal name.
func (p *plugin) groupByName(name string) (*group, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.groups {
		if p.groups[i].Name == name {
			return &p.groups[i], true
		}
	}
	return nil, false
}

// allGroupsSnapshot returns a deep-ish copy of groups for safe reads.
func (p *plugin) allGroupsSnapshot() []group {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]group, len(p.groups))
	copy(out, p.groups)
	return out
}

// ---- Registration / capability JSON ----

type registrationCapability struct {
	ModelRouter           bool     `json:"model_router"`
	ModelRegistrar        bool     `json:"model_registrar"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	ManagementAPI         bool     `json:"management_api"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

func (p *plugin) registrationJSON() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "cpa-router",
			Version:          "0.2.0",
			Author:           "swysgh",
			GitHubRepository: "https://github.com/swysgh/cpa-router",
			Logo:             "",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "state_file", Type: pluginapi.ConfigFieldTypeString, Description: "组配置状态文件路径（相对进程工作目录）"},
				{Name: "name_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "组调用名前缀，默认空"},
				{Name: "reload_interval", Type: pluginapi.ConfigFieldTypeString, Description: "状态文件热加载检查间隔，如 2s"},
				{Name: "max_attempts", Type: pluginapi.ConfigFieldTypeNumber, Description: "单请求最多尝试的成员数，0 表示不限制"},
				{Name: "attempt_timeout", Type: pluginapi.ConfigFieldTypeString, Description: "单次上游尝试的超时"},
				{Name: "total_timeout", Type: pluginapi.ConfigFieldTypeString, Description: "单请求总时间预算"},
				{Name: "all_cooling_policy", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"wait", "first", "error"}, Description: "整组冷却时的策略"},
				{Name: "max_wait", Type: pluginapi.ConfigFieldTypeString, Description: "all_cooling_policy=wait 时的最长等待"},
				{Name: "log_level", Type: pluginapi.ConfigFieldTypeString, Description: "debug|info|warn|error"},
			},
		},
		Capabilities: registrationCapability{
			ModelRouter:           true,
			ModelRegistrar:        true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  []string{"openai", "openai-response", "claude", "gemini", "codex", "antigravity", "interactions"},
			ExecutorOutputFormats: []string{"openai", "openai-response", "claude", "gemini", "codex", "antigravity", "interactions"},
			ManagementAPI:         true,
		},
	}
}

// modelProviderName is the provider key the plugin registers its models under.
const modelProviderName = "cpa-router"

// modelCreatedAt is stamped on registered models; it is fixed for the lifetime of
// the process so repeated registrations do not churn the model list.
var modelCreatedAt = time.Now().Unix()

// modelRegistrationJSON advertises the group call names (and aliases) as models.
//
// Without this the groups are callable but invisible: clients that list models
// never see them, because the host only learns about plugin-provided models via
// model.register. Disabled groups are not advertised.
func (p *plugin) modelRegistrationJSON() pluginapi.ModelRegistrationResponse {
	prefix := p.cfg.NamePrefix
	seen := make(map[string]bool)
	models := make([]pluginapi.ModelInfo, 0, 8)
	for _, g := range p.allGroupsSnapshot() {
		if !g.Enabled {
			continue
		}
		for _, n := range append([]string{g.Name}, g.Aliases...) {
			id := prefix + strings.TrimSpace(n)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			desc := g.Description
			if desc == "" {
				desc = fmt.Sprintf("cpa-router 组（%s，%d 个成员）", g.Strategy, len(g.Members))
			}
			models = append(models, pluginapi.ModelInfo{
				ID:          id,
				Object:      "model",
				Created:     modelCreatedAt,
				OwnedBy:     modelProviderName,
				DisplayName: id,
				Name:        id,
				Description: desc,
				UserDefined: true,
			})
		}
	}
	return pluginapi.ModelRegistrationResponse{Provider: modelProviderName, Models: models}
}

// ---- method dispatch ----
// HandleMethod dispatches a plugin RPC method. It is the single entry point used
// by abi.go. Business logic must never import "C".
func (p *plugin) HandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := p.configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(p.registrationJSON())
	case pluginabi.MethodModelRoute:
		return p.routeModel(request)
	case pluginabi.MethodModelRegister:
		// Advertise the group call names as models so they appear in /v1/models.
		return okEnvelope(p.modelRegistrationJSON())
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": "cpa-router"})
	case pluginabi.MethodExecutorExecute:
		return p.execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return p.executeStream(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(p.managementRegistrationJSON())
	case pluginabi.MethodManagementHandle:
		resp, err := p.handleManagement(request)
		if err != nil {
			return errorEnvelope("plugin_error", err.Error()), nil
		}
		return okEnvelope(resp)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ---- shared envelope helpers (pure data, no C) ----

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// sortStrings is a small stable sort helper (exported for tests if needed).
func sortStrings(s []string) {
	sort.Strings(s)
}
