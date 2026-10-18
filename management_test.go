package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func mgmtReq(method, path string, body []byte) []byte {
	raw, _ := json.Marshal(managementRequest{
		Method: method,
		Path:   path,
		Body:   body,
	})
	return raw
}

func decodeMgmtJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	// handleManagement returns a *pluginapi.ManagementResponse wrapped by the abi
	// layer in an ok envelope: {ok:true, result:{StatusCode,Headers,Body}}. The
	// Body is a []byte field, so JSON encodes it as a base64 string; json
	// unmarshals it back into a byte slice automatically.
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			StatusCode int            `json:"status_code"`
			Headers    map[string]any `json:"headers"`
			Body       []byte         `json:"body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode mgmt envelope: %v (raw=%s)", err, raw)
	}
	var m map[string]any
	if err := json.Unmarshal(env.Result.Body, &m); err != nil {
		t.Fatalf("decode mgmt body: %v (raw=%s)", err, env.Result.Body)
	}
	return m
}

// mgmtRaw calls handleManagement and returns the wrapped envelope bytes.
func mgmtRaw(t *testing.T, p *plugin, method, path string, body []byte) ([]byte, error) {
	t.Helper()
	resp, err := p.handleManagement(mgmtReq(method, path, body))
	if err != nil {
		return nil, err
	}
	return okEnvelope(resp)
}

func TestManagementRegistrationJSON(t *testing.T) {
	p := newPlugin(newFakeHost())
	reg := p.managementRegistrationJSON()
	routes, _ := reg["routes"].([]map[string]any)
	if len(routes) != 7 {
		t.Fatalf("expected 7 routes, got %d", len(routes))
	}
	foundModels := false
	for _, r := range routes {
		if r["Method"] == "GET" && r["Path"] == "/plugins/cpa-router/models" {
			foundModels = true
		}
	}
	if !foundModels {
		t.Errorf("routes must declare GET /plugins/cpa-router/models, got %v", routes)
	}
	resources, _ := reg["resources"].([]map[string]any)
	if len(resources) != 1 || resources[0]["Path"] != "/panel" {
		t.Fatalf("expected /panel resource, got %v", resources)
	}
}

func TestManagementState(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Strategy: "fallback", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())
	raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	if m["version"] != float64(1) {
		t.Fatalf("state version = %v", m["version"])
	}
	groups, _ := m["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("state groups = %v", groups)
	}
}

func TestManagementPutGroups(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{}}, defaultConfig())
	body, _ := json.Marshal(map[string]any{"groups": []map[string]any{
		{"name": "g1", "strategy": "fallback", "enabled": true, "members": []map[string]any{{"model": "m1"}}},
	}})
	raw, err := mgmtRaw(t, p, "PUT", "/plugins/cpa-router/groups", body)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	if m["ok"] != true {
		t.Fatalf("put ok = %v, error=%v", m["ok"], m["error"])
	}
	if p.allGroupsSnapshot()[0].Name != "g1" {
		t.Fatalf("group not applied")
	}
}

func TestManagementPutInvalid(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{}}, defaultConfig())
	body, _ := json.Marshal(map[string]any{"groups": []map[string]any{
		{"name": "g1", "members": []map[string]any{{"model": "m", "group": "x"}}},
	}})
	raw, err := mgmtRaw(t, p, "PUT", "/plugins/cpa-router/groups", body)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	if m["ok"] != false {
		t.Fatalf("expected ok=false, got %v", m)
	}
	if _, ok := m["error"]; !ok {
		t.Fatalf("expected error field")
	}
}

func TestManagementDelete(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Members: []rawMember{{Model: "m"}}},
		{Name: "g2", Members: []rawMember{{Model: "m"}}},
	}}, defaultConfig())
	raw, err := mgmtRaw(t, p, "DELETE", "/plugins/cpa-router/groups?name=g1", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	if m["ok"] != true {
		t.Fatalf("delete ok = %v", m["ok"])
	}
	if len(p.allGroupsSnapshot()) != 1 || p.allGroupsSnapshot()[0].Name != "g2" {
		t.Fatalf("delete failed: %+v", p.allGroupsSnapshot())
	}
}

func TestManagementDelete404(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{}}, defaultConfig())
	raw, err := mgmtRaw(t, p, "DELETE", "/plugins/cpa-router/groups?name=nope", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	if m["ok"] != false {
		t.Fatalf("expected ok=false for missing group")
	}
}

func TestManagementReset(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())
	// Put m1 into cooldown.
	p.rt.recordCooldown("m1", 429, "", p.cfg)
	if !p.rt.isCooling("m1") {
		t.Fatal("m1 should be cooling")
	}
	body, _ := json.Marshal(map[string]any{"name": "m1", "what": "cooldown"})
	raw, err := mgmtRaw(t, p, "POST", "/plugins/cpa-router/reset", body)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	if m["ok"] != true {
		t.Fatalf("reset ok = %v", m["ok"])
	}
	if p.rt.isCooling("m1") {
		t.Fatalf("m1 should be cleared")
	}
}

func TestManagementUnknownRoute(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{}}, defaultConfig())
	raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/unknown", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	if m["ok"] != false {
		t.Fatalf("expected ok=false for unknown route")
	}
}

// The host passes the full request path (including the /v0/management prefix)
// to the plugin rather than the path declared in management.register.
// Regression test: every declared route must still dispatch when prefixed.
func TestManagementPrefixedPaths(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{}}, defaultConfig())

	raw, err := mgmtRaw(t, p, "GET", "/v0/management/plugins/cpa-router/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeMgmtJSON(t, raw); m["version"] != float64(1) {
		t.Fatalf("prefixed GET /state did not dispatch: %v", m)
	}

	body, _ := json.Marshal(map[string]any{"groups": []map[string]any{
		{"name": "g1", "strategy": "fallback", "enabled": true, "members": []map[string]any{{"model": "m1"}}},
	}})
	raw, err = mgmtRaw(t, p, "PUT", "/v0/management/plugins/cpa-router/groups", body)
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeMgmtJSON(t, raw); m["ok"] != true {
		t.Fatalf("prefixed PUT /groups did not dispatch: %v", m)
	}

	// Query string must survive prefix stripping.
	raw, err = mgmtRaw(t, p, "DELETE", "/v0/management/plugins/cpa-router/groups?name=g1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeMgmtJSON(t, raw); m["ok"] != true {
		t.Fatalf("prefixed DELETE /groups did not dispatch: %v", m)
	}

	if _, err := p.handleManagement(mgmtReq("GET", "/v0/management/plugins/cpa-router/panel", nil)); err != nil {
		t.Fatalf("prefixed panel: %v", err)
	}
}

func TestPluginConfigureRegistration(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("name_prefix: \"r/\"\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.configure(raw); err != nil {
		t.Fatalf("configure error: %v", err)
	}
	// Verify registration JSON.
	reg := p.registrationJSON()
	if reg.SchemaVersion != 6 {
		t.Fatalf("schema_version = %d, want 6", reg.SchemaVersion)
	}
	if !reg.Capabilities.ModelRouter {
		t.Fatalf("model_router should be true")
	}
	if reg.Metadata.Name != "cpa-router" {
		t.Fatalf("name = %q", reg.Metadata.Name)
	}
}

// Regression: management request bodies are JSON, and encoding/json does not run
// UnmarshalYAML, so an explicit "enabled": false used to be silently coerced
// back to true (memberEnabled() saw enabledSet == false and returned true).
func TestManagementEnabledPresence(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{}}, defaultConfig())

	body := []byte(`{"groups":[{"name":"g1","strategy":"fallback","enabled":false,` +
		`"members":[{"model":"m1","enabled":false},{"model":"m2"}]}]}`)
	raw, err := mgmtRaw(t, p, "PUT", "/plugins/cpa-router/groups", body)
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeMgmtJSON(t, raw); m["ok"] != true {
		t.Fatalf("PUT failed: %v", m)
	}

	raw, err = mgmtRaw(t, p, "GET", "/plugins/cpa-router/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	groups, _ := m["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("groups = %v", groups)
	}
	g, _ := groups[0].(map[string]any)
	if g["enabled"] != false {
		t.Errorf("group enabled = %v, want false", g["enabled"])
	}
	members, _ := g["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("members = %v", members)
	}
	if m1, _ := members[0].(map[string]any); m1["enabled"] != false {
		t.Errorf("explicit enabled:false member came back %v, want false", m1["enabled"])
	}
	if m2, _ := members[1].(map[string]any); m2["enabled"] != true {
		t.Errorf("omitted enabled member came back %v, want true", m2["enabled"])
	}
}

// Regression: POST /groups merges via currentGroupFile(), which rebuilt members
// with enabledSet unset, so saving one group re-enabled disabled members of
// every other group.
func TestManagementPostKeepsOtherGroupDisabledMembers(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{}}, defaultConfig())

	first := []byte(`{"groups":[{"name":"a","strategy":"fallback",` +
		`"members":[{"model":"m1","enabled":false},{"model":"m2","enabled":true}]}]}`)
	if _, err := mgmtRaw(t, p, "PUT", "/plugins/cpa-router/groups", first); err != nil {
		t.Fatal(err)
	}

	second := []byte(`{"group":{"name":"b","strategy":"fallback","members":[{"model":"m3","enabled":true}]}}`)
	if _, err := mgmtRaw(t, p, "POST", "/plugins/cpa-router/groups", second); err != nil {
		t.Fatal(err)
	}

	raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	groups, _ := m["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups = %v", groups)
	}
	for _, gi := range groups {
		g, _ := gi.(map[string]any)
		if g["name"] != "a" {
			continue
		}
		members, _ := g["members"].([]any)
		if len(members) != 2 {
			t.Fatalf("group a members = %v", members)
		}
		if m1, _ := members[0].(map[string]any); m1["enabled"] != false {
			t.Errorf("group a member m1 re-enabled to %v by POST of another group", m1["enabled"])
		}
		return
	}
	t.Fatal("group a not found")
}

// §1: GET /state must expose the per-group request counters so the panel no
// longer shows a hardcoded 0. Keys are the internal group name; missing groups
// fall back to the zero value.
func TestManagementStateGroupStats(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Strategy: "fallback", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())

	p.rt.recordGroupAttempt("g1")
	p.rt.recordGroupAttempt("g1")
	p.rt.recordGroupSuccess("g1")
	p.rt.recordGroupFail("g1")

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
	if got := g["group_total"]; got != float64(2) {
		t.Errorf("group_total = %v, want 2", got)
	}
	if got := g["group_ok"]; got != float64(1) {
		t.Errorf("group_ok = %v, want 1", got)
	}
	if got := g["group_fail"]; got != float64(1) {
		t.Errorf("group_fail = %v, want 1", got)
	}
}

// ---- GET /models ----

// modelsListPlugin builds a plugin whose fake host serves the given credential
// scripts. Every auth.get for an index without a script fails the way the real
// host does for runtime-only credentials (no backing file).
func modelsListPlugin(t *testing.T, files []map[string]any, creds map[string]string, gs ...rawGroup) *plugin {
	t.Helper()
	cfg := defaultConfig()
	cfg.NamePrefix = "r/"
	cfg.FilterModels = true
	p := newTestPlugin(t, groupFile{Version: 1, Groups: gs}, cfg)
	host, ok := p.host.(*fakeHost)
	if !ok {
		t.Fatalf("test plugin host is %T, want *fakeHost", p.host)
	}
	host.setHandler(pluginabi.MethodHostAuthList, func(json.RawMessage) (json.RawMessage, error) {
		return okResult(map[string]any{"files": files}), nil
	})
	host.setHandler(pluginabi.MethodHostAuthGet, func(payload json.RawMessage) (json.RawMessage, error) {
		var req struct {
			AuthIndex string `json:"auth_index"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		body, ok := creds[req.AuthIndex]
		if !ok {
			return hostErrorResult("not_found", "auth file path not found for auth_index "+req.AuthIndex, 404), nil
		}
		return okResult(map[string]any{
			"auth_index": req.AuthIndex,
			"name":       req.AuthIndex + ".json",
			"json":       json.RawMessage(body),
		}), nil
	})
	return p
}

// modelNamesOf decodes the endpoint's "models" array in order.
func modelNamesOf(t *testing.T, m map[string]any) []string {
	t.Helper()
	rawList, ok := m["models"].([]any)
	if !ok {
		t.Fatalf("models field = %v (%T), want array", m["models"], m["models"])
	}
	out := make([]string, 0, len(rawList))
	for _, v := range rawList {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("models entry %v is %T, want string", v, v)
		}
		out = append(out, s)
	}
	return out
}

// The endpoint exists because /v1/models is trimmed to the group call names: a
// console building groups needs the upstream names too. Credential "models"
// fields come in both shapes (plain strings and {name, alias} objects), aliases
// are callable names, runtime-only credentials must be skipped rather than
// failing the whole call, and the plugin's own call names are always included.
func TestModelsListFromCredentials(t *testing.T) {
	p := modelsListPlugin(t,
		[]map[string]any{
			{"auth_index": "a1", "name": "zen.json"},
			{"auth_index": "a2", "name": "wb.json"},
			{"auth_index": "a3", "name": "runtime-only", "runtime_only": true},
		},
		map[string]string{
			"a1": `{"type":"zen","models":["big-pickle","gpt-4o"]}`,
			"a2": `{"models":[{"name":"claude-3-5-sonnet","alias":"sonnet"},{"name":"  "},{"alias":"empty-name"}]}`,
		},
		enabledGroup("free-stack", "free"),
	)

	raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	want := []string{"big-pickle", "claude-3-5-sonnet", "empty-name", "gpt-4o", "r/free", "r/free-stack", "sonnet"}
	if got := modelNamesOf(t, m); !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	// Only names leave the plugin: no credential JSON, no secrets.
	if len(m) != 1 {
		t.Errorf("response carries extra fields: %v", m)
	}
}

// The endpoint's third source is the registry cache: the ids of the last model
// list the host produced. It is the only way the picker can offer OAuth models
// (antigravity's "anti/<model>"), which no credential JSON and no config channel
// declares. The response reports how many registry ids it used so a cold cache is
// diagnosable instead of looking like a missing model.
func TestModelsListIncludesCachedRegistry(t *testing.T) {
	p := modelsListPlugin(t,
		[]map[string]any{{"auth_index": "a1", "name": "zen.json"}},
		map[string]string{"a1": `{"models":["big-pickle"]}`},
		enabledGroup("free"),
	)
	p.rt.rememberRegistry([]string{"anti/gemini-3-flash", "anti/auto", "r/free"})

	raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	want := []string{"anti/auto", "anti/gemini-3-flash", "big-pickle", "r/free"}
	if got := modelNamesOf(t, m); !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	if got := m["registry_models"]; got != float64(3) {
		t.Errorf("registry_models = %v, want 3", got)
	}
	if _, ok := m["registry_seen_at"]; !ok {
		t.Error("registry_seen_at must be reported when the cache is warm")
	}
}

// With a cold cache the endpoint still answers: credential models + group names,
// and no registry fields (the panel uses their absence to say the host has not
// listed models yet).
func TestModelsListColdRegistryOmitsRegistryFields(t *testing.T) {
	p := modelsListPlugin(t, nil, nil, enabledGroup("free"))
	raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	if _, ok := m["registry_seen_at"]; ok {
		t.Error("cold cache must not report registry_seen_at")
	}
	if _, ok := m["registry_models"]; ok {
		t.Error("cold cache must not report registry_models")
	}
}

// A disabled credential is unregistered by the host, so its models cannot be
// called at all; listing them would hand the picker dead candidates that answer
// 400 model_not_found on the first attempt.
func TestModelsListSkipsDisabledCredentials(t *testing.T) {
	p := modelsListPlugin(t,
		[]map[string]any{
			{"auth_index": "a1", "name": "on.json"},
			{"auth_index": "a2", "name": "off.json", "disabled": true},
		},
		map[string]string{
			"a1": `{"models":["live"]}`,
			"a2": `{"models":["dead"]}`,
		},
	)
	raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelNamesOf(t, decodeMgmtJSON(t, raw)), []string{"live"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
}

// The host hands the plugin the full management path, so the declared route must
// also answer under the /v0/management prefix.
func TestModelsListUnderManagementPrefix(t *testing.T) {
	p := modelsListPlugin(t, nil, nil, enabledGroup("g1"))
	raw, err := mgmtRaw(t, p, "GET", "/v0/management/plugins/cpa-router/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := modelNamesOf(t, decodeMgmtJSON(t, raw)), []string{"r/g1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
}

// No credentials and no groups is an empty catalog, not null and not an error.
func TestModelsListEmpty(t *testing.T) {
	p := modelsListPlugin(t, nil, nil)
	raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMgmtJSON(t, raw)
	if got := modelNamesOf(t, m); len(got) != 0 {
		t.Fatalf("models = %v, want empty", got)
	}
}

// A failing host.auth.list is a real host problem, so it surfaces as 500 instead
// of a silently empty catalog the panel would trust.
func TestModelsListAuthListFailure(t *testing.T) {
	p := modelsListPlugin(t, nil, nil, enabledGroup("g1"))
	host := p.host.(*fakeHost)
	host.setHandler(pluginabi.MethodHostAuthList, func(json.RawMessage) (json.RawMessage, error) {
		return hostErrorResult("internal_error", "auth manager unavailable", 500), nil
	})

	raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			StatusCode int    `json:"StatusCode"`
			Body       []byte `json:"Body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Result.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", env.Result.StatusCode)
	}
	var body map[string]any
	if err := json.Unmarshal(env.Result.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["ok"] != false {
		t.Errorf("ok = %v, want false", body["ok"])
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "枚举凭证模型失败") {
		t.Errorf("error = %q, want a Chinese reason", msg)
	}
}

// modelsFromCredentialJSON tolerates every shape a credential file can carry.
func TestModelsFromCredentialJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"string array", `{"models":["a","b"]}`, []string{"a", "b"}},
		{"object array", `{"models":[{"name":"a","alias":"x"},{"name":"b"}]}`, []string{"a", "x", "b"}},
		// A credential prefix is part of the callable id: with force-model-prefix
		// on the bare id is never registered, so a bare name would be a 400.
		{"prefixed strings", `{"prefix":"anti","models":["a","b"]}`, []string{"anti/a", "anti/b"}},
		{"prefixed objects", `{"prefix":"anti","models":[{"name":"a","alias":"x"}]}`, []string{"anti/a", "anti/x"}},
		{"blank prefix", `{"prefix":"   ","models":["a"]}`, []string{"a"}},
		{"non-string prefix", `{"prefix":7,"models":["a"]}`, []string{"a"}},
		{"no models field", `{"type":"antigravity","email":"a@b.c"}`, nil},
		{"models not an array", `{"models":"a,b"}`, nil},
		{"invalid json", `{`, nil},
		{"empty", ``, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelsFromCredentialJSON(json.RawMessage(tc.raw)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("models = %v, want %v", got, tc.want)
			}
		})
	}
}
