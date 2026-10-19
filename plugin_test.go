package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// decodeModelRegistration runs model.register and decodes the registration.
func decodeModelRegistration(t *testing.T, p *plugin) pluginapi.ModelRegistrationResponse {
	t.Helper()
	raw, err := p.HandleMethod(pluginabi.MethodModelRegister, []byte("{}"))
	if err != nil {
		t.Fatalf("model.register returned error: %v", err)
	}
	var env struct {
		OK     bool                                `json:"ok"`
		Result pluginapi.ModelRegistrationResponse `json:"result"`
		Error  *envelopeError                      `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode model.register envelope: %v (raw=%s)", err, raw)
	}
	if !env.OK {
		t.Fatalf("model.register not ok: %s", raw)
	}
	return env.Result
}

// Regression: group names were callable but never showed up in /v1/models,
// because the plugin advertised model_registrar but never answered model.register.
func TestModelRegistrationAdvertisesGroups(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	parsed, _, _ := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Strategy: "fallback", Enabled: true, Description: "免费优先",
			Aliases: []string{"free"}, Members: []rawMember{{Model: "m1", Enabled: true}}},
		{Name: "off", Strategy: "fallback", Enabled: false,
			Members: []rawMember{{Model: "m2", Enabled: true}}},
	}})
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, "")

	reg := decodeModelRegistration(t, p)

	// The host drops the whole registration when the provider key is empty.
	if reg.Provider == "" {
		t.Fatal("Provider must be non-empty or the host ignores the registration")
	}
	got := map[string]string{}
	for _, m := range reg.Models {
		got[m.ID] = m.Description
	}
	if _, ok := got["free-stack"]; !ok {
		t.Errorf("group name not advertised: %v", got)
	}
	if _, ok := got["free"]; !ok {
		t.Errorf("group alias not advertised: %v", got)
	}
	if _, ok := got["off"]; ok {
		t.Errorf("disabled group must not be advertised: %v", got)
	}
	if len(reg.Models) != 2 {
		t.Errorf("advertised %d models, want 2: %v", len(reg.Models), got)
	}
	if d := got["free-stack"]; d != "免费优先" {
		t.Errorf("description = %q, want the group description", d)
	}
	for _, m := range reg.Models {
		if m.ID == "" || m.Object != "model" || !m.UserDefined {
			t.Errorf("model %+v is missing required fields", m)
		}
	}
}

// The advertised names must carry the configured name_prefix, matching how the
// router resolves call names.
func TestModelRegistrationHonoursNamePrefix(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	p.cfg.NamePrefix = "r/"
	parsed, _, _ := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g1", Strategy: "fallback", Enabled: true, Aliases: []string{"one"},
			Members: []rawMember{{Model: "m1", Enabled: true}}},
	}})
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, "r/")

	reg := decodeModelRegistration(t, p)
	got := map[string]bool{}
	for _, m := range reg.Models {
		got[m.ID] = true
	}
	if !got["r/g1"] || !got["r/one"] {
		t.Fatalf("advertised %v, want r/g1 and r/one", got)
	}
	if got["g1"] {
		t.Errorf("unprefixed name must not be advertised when name_prefix is set: %v", got)
	}
}

// No groups must still produce a valid registration with an empty model list.
func TestModelRegistrationEmptyIsValid(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	reg := decodeModelRegistration(t, p)
	if reg.Provider == "" {
		t.Error("Provider must stay non-empty even with no groups")
	}
	if len(reg.Models) != 0 {
		t.Errorf("models = %v, want empty", reg.Models)
	}
}
