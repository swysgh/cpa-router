package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func newTestPlugin(t *testing.T, groups groupFile, cfg config) *plugin {
	t.Helper()
	p := newPlugin(newFakeHost())
	p.cfg = cfg
	p.rt.setClock(fixedClock(time.Unix(1000, 0)))
	parsed, _, err := parseGroups(groups)
	if err != nil {
		t.Fatalf("parse groups: %v", err)
	}
	p.groups = parsed
	p.nameIndex = buildGroupNameIndex(parsed, cfg.NamePrefix)
	return p
}

func routeReq(model string) []byte {
	raw, _ := json.Marshal(rpcModelRouteRequest{
		ModelRouteRequest: pluginapi.ModelRouteRequest{RequestedModel: model, SourceFormat: "openai"},
	})
	return raw
}

func decodeRoute(t *testing.T, raw []byte) pluginapi.ModelRouteResponse {
	t.Helper()
	var env struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.ModelRouteResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode route: %v", err)
	}
	return env.Result
}

// routeDecode calls routeModel and returns the decoded response (fails on error).
func routeDecode(t *testing.T, p *plugin, model string) pluginapi.ModelRouteResponse {
	t.Helper()
	raw, err := p.routeModel(routeReq(model))
	if err != nil {
		t.Fatalf("routeModel error: %v", err)
	}
	return decodeRoute(t, raw)
}

func TestRouteHit(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Strategy: "round-robin", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())

	resp := routeDecode(t, p, "free-stack")
	if !resp.Handled || resp.TargetKind != pluginapi.ModelRouteTargetSelf || resp.Reason != "group:free-stack:round-robin" {
		t.Fatalf("unexpected route: %+v", resp)
	}
}

func TestRouteMiss(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())
	resp := routeDecode(t, p, "nonexistent")
	if resp.Handled {
		t.Fatalf("expected not handled, got %+v", resp)
	}
}

func TestRouteDisabled(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Enabled: false, Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())
	resp := routeDecode(t, p, "free-stack")
	if resp.Handled {
		t.Fatalf("expected disabled group not handled, got %+v", resp)
	}
}

func TestRoutePrefix(t *testing.T) {
	cfg := defaultConfig()
	cfg.NamePrefix = "r/"
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, cfg)
	if resp := routeDecode(t, p, "free-stack"); resp.Handled {
		t.Fatalf("bare name with prefix should not hit: %+v", resp)
	}
	if resp := routeDecode(t, p, "r/free-stack"); !resp.Handled {
		t.Fatalf("prefixed name should hit: %+v", resp)
	}
}

func TestRouteAlias(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Aliases: []string{"fs"}, Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())
	if resp := routeDecode(t, p, "fs"); !resp.Handled {
		t.Fatalf("alias should hit: %+v", resp)
	}
}

func TestRouteHotReload(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())
	p.store = newGroupStore(t.TempDir() + "/groups.yaml")
	// Write a new file with an extra group; set mtime after lastReload.
	f := groupFile{Version: 1, Groups: []rawGroup{
		{Name: "free-stack", Enabled: true, Members: []rawMember{{Model: "m1"}}},
		{Name: "beta", Enabled: true, Members: []rawMember{{Model: "m2"}}},
	}}
	if err := p.store.save(f); err != nil {
		t.Fatal(err)
	}
	// Advance clock beyond reload_interval so reload triggers.
	p.rt.nowFunc = func() time.Time { return time.Unix(2000, 0) }
	// Set lastReload far in the past.
	p.mu.Lock()
	p.lastReload = time.Unix(0, 0)
	p.mu.Unlock()
	// Touch file mtime.
	mt := time.Unix(1500, 0)
	_ = mt
	if resp := routeDecode(t, p, "beta"); !resp.Handled {
		t.Fatalf("beta should be resolvable after hot reload: %+v", resp)
	}
}

func mustOK(t *testing.T, raw []byte, err error) []byte {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return raw
}

func mustOK2(t *testing.T, raw []byte, err error) []byte {
	return mustOK(t, raw, err)
}

var _ = pluginabi.MethodModelRoute
