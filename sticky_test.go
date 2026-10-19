package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func stickyCfg() config {
	cfg := defaultConfig()
	cfg.Sticky = true
	return cfg
}

func expandFirst(t *testing.T, p *plugin, key string) string {
	t.Helper()
	out, err := p.expand("g", 1, map[string]bool{}, false, key)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expand returned no members")
	}
	return out[0]
}

func TestStickyPinsSessionToSameMember(t *testing.T) {
	p := newTestPlugin(t, groups3RoundRobin(), stickyCfg())
	first := expandFirst(t, p, "h:sess-a")
	for i := 0; i < 5; i++ {
		if got := expandFirst(t, p, "h:sess-a"); got != first {
			t.Fatalf("turn %d hit %s, want pinned %s", i, got, first)
		}
	}
}

func TestStickySpreadsNewSessionsAcrossMembers(t *testing.T) {
	p := newTestPlugin(t, groups3RoundRobin(), stickyCfg())
	seen := map[string]bool{}
	for _, k := range []string{"h:a", "h:b", "h:c"} {
		seen[expandFirst(t, p, k)] = true
	}
	if len(seen) != 3 {
		t.Fatalf("three new sessions landed on %d distinct members, want 3", len(seen))
	}
}

func TestStickyOffKeepsRotating(t *testing.T) {
	p := newTestPlugin(t, groups3RoundRobin(), defaultConfig())
	want := []string{"m1", "m2", "m3"}
	for i, w := range want {
		if got := expandFirst(t, p, "h:same"); got != w {
			t.Fatalf("turn %d hit %s, want %s (sticky disabled)", i, got, w)
		}
	}
}

func TestStickyExpiresAfterTTL(t *testing.T) {
	clock := time.Unix(1000, 0)
	cfg := stickyCfg()
	p := newTestPlugin(t, groups3RoundRobin(), cfg)
	p.rt.setClock(func() time.Time { return clock })

	first := expandFirst(t, p, "h:sess")
	if first != "m1" {
		t.Fatalf("first pick = %s, want m1", first)
	}
	clock = clock.Add(cfg.StickyTTLDur - time.Second)
	if got := expandFirst(t, p, "h:sess"); got != first {
		t.Fatalf("inside TTL hit %s, want %s", got, first)
	}
	// The hit above refreshed the TTL, so expiry is measured from there.
	clock = clock.Add(cfg.StickyTTLDur + time.Second)
	if got := expandFirst(t, p, "h:sess"); got == first {
		t.Fatalf("after TTL still pinned to %s, want a new pick", got)
	}
}

func TestStickyReassignsWhenPinnedMemberCools(t *testing.T) {
	p := newTestPlugin(t, groups3RoundRobin(), stickyCfg())
	if got := expandFirst(t, p, "h:sess"); got != "m1" {
		t.Fatalf("first pick = %s, want m1", got)
	}
	p.rt.recordCooldown("m1", 429, "", p.cfg)

	got := expandFirst(t, p, "h:sess")
	if got == "m1" {
		t.Fatal("pinned member is cooling but was still selected")
	}
	if again := expandFirst(t, p, "h:sess"); again != got {
		t.Fatalf("re-pin did not stick: %s then %s", got, again)
	}
}

func TestStickyKeysAreScopedPerGroup(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Strategy: "round-robin", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true}, {Model: "m2", Enabled: true},
		}},
		{Name: "h", Strategy: "round-robin", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true}, {Model: "m2", Enabled: true},
		}},
	}}, stickyCfg())

	if got := expandFirst(t, p, "h:same"); got != "m1" {
		t.Fatalf("g first pick = %s, want m1", got)
	}
	out, err := p.expand("h", 1, map[string]bool{}, false, "h:same")
	if err != nil {
		t.Fatal(err)
	}
	if out[0] != "m1" {
		t.Fatalf("group h first pick = %s, want its own m1", out[0])
	}
}

func TestStickyKeyPrefersHeaderOverBody(t *testing.T) {
	p := newTestPlugin(t, groups3RoundRobin(), stickyCfg())
	req := &rpcExecutorRequest{}
	req.Headers = http.Header{}
	req.Headers.Set("X-Session-Id", "abc123")
	req.OriginalRequest = []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if got := p.stickyKey(req); got != "h:abc123" {
		t.Fatalf("sticky key = %q, want h:abc123", got)
	}

	req.Headers = http.Header{}
	if got := p.stickyKey(req); got == "" || got == "h:abc123" {
		t.Fatalf("body fingerprint not used: %q", got)
	}
}

func TestStickyKeyEmptyWhenNoSource(t *testing.T) {
	cfg := stickyCfg()
	cfg.StickyBodyFingerprint = false
	p := newTestPlugin(t, groups3RoundRobin(), cfg)
	req := &rpcExecutorRequest{}
	req.Headers = http.Header{}
	if got := p.stickyKey(req); got != "" {
		t.Fatalf("sticky key = %q, want empty", got)
	}
}

func TestBodyFingerprintStableAcrossTurns(t *testing.T) {
	turn1 := []byte(`{"messages":[
		{"role":"system","content":"you are a bot"},
		{"role":"user","content":"first question"}]}`)
	turn2 := []byte(`{"messages":[
		{"role":"system","content":"you are a bot"},
		{"role":"user","content":"first question"},
		{"role":"assistant","content":"answer"},
		{"role":"user","content":"second question"}]}`)
	other := []byte(`{"messages":[
		{"role":"system","content":"you are a bot"},
		{"role":"user","content":"different question"}]}`)

	a, b, c := bodyFingerprint(turn1), bodyFingerprint(turn2), bodyFingerprint(other)
	if a == "" || b == "" || c == "" {
		t.Fatalf("empty fingerprint: %q %q %q", a, b, c)
	}
	if a != b {
		t.Fatalf("same conversation hashed differently: %s vs %s", a, b)
	}
	if a == c {
		t.Fatal("different conversations hashed the same")
	}
}

func TestBodyFingerprintReadsClaudeAndMultimodalShapes(t *testing.T) {
	claude := []byte(`{"system":"sys prompt","messages":[{"role":"user","content":"hi"}]}`)
	multimodal := []byte(`{"messages":[
		{"role":"system","content":[{"type":"text","text":"sys prompt"}]},
		{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if bodyFingerprint(claude) == "" || bodyFingerprint(multimodal) == "" {
		t.Fatal("fingerprint empty for claude/multimodal shapes")
	}
	// A JSON body with no conversation root must not produce a shared key.
	if got := bodyFingerprint([]byte(`{"model":"x"}`)); got != "" {
		t.Fatalf("fingerprint for rootless body = %q, want empty", got)
	}
}

func TestStickyConfigValidation(t *testing.T) {
	if _, err := decodeConfig([]byte("sticky: true\nsticky_ttl: \"0s\"\n")); err == nil {
		t.Fatal("sticky_ttl: 0s accepted")
	}
	if _, err := decodeConfig([]byte("sticky: true\nsticky_headers: []\nsticky_body_fingerprint: false\n")); err == nil {
		t.Fatal("sticky with no session source accepted")
	}
	cfg, err := decodeConfig(nil)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.Sticky {
		t.Fatal("sticky defaults to on, want off")
	}
	if cfg.StickyTTLDur != 30*time.Minute {
		t.Fatalf("default sticky_ttl = %s, want 30m", cfg.StickyTTLDur)
	}
	if len(cfg.StickyHeaders) == 0 {
		t.Fatal("default sticky_headers is empty")
	}
}

func TestStickyOnlyAppliesToRoundRobin(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Strategy: "fallback", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true}, {Model: "m2", Enabled: true},
		}},
	}}, stickyCfg())
	for i := 0; i < 3; i++ {
		out, err := p.expand("g", 1, map[string]bool{}, false, "h:sess")
		if err != nil {
			t.Fatal(err)
		}
		if out[0] != "m1" || out[1] != "m2" {
			t.Fatalf("fallback order changed by sticky: %v", out)
		}
	}
}

// TestTestEndpointDoesNotCreatePins guards the management /test path: its
// synthetic body is constant, so a pin written there would hijack real traffic.
func TestTestEndpointDoesNotCreatePins(t *testing.T) {
	p := newTestPlugin(t, groups3RoundRobin(), stickyCfg())
	req := rpcExecutorRequest{}
	req.Model = "g"
	req.OriginalRequest = []byte(`{"model":"g","messages":[{"role":"user","content":"probe"}]}`)
	raw, _ := json.Marshal(req)

	if _, err := p.executeWithSticky(raw, nil, false); err != nil {
		t.Fatalf("execute: %v", err)
	}
	p.rt.mu.Lock()
	pins := len(p.rt.sticky)
	p.rt.mu.Unlock()
	if pins != 0 {
		t.Fatalf("/test wrote %d sticky pin(s), want 0", pins)
	}
}
