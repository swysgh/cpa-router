package main

import (
	"encoding/json"
	"testing"
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
	if len(routes) != 6 {
		t.Fatalf("expected 6 routes, got %d", len(routes))
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
