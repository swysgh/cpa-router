package main

import (
	"testing"
	"time"
)

func groups3RoundRobin() groupFile {
	return groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Strategy: "round-robin", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true},
			{Model: "m2", Enabled: true},
			{Model: "m3", Enabled: true},
		}},
	}}
}

func TestRoundRobinPointerAdvances(t *testing.T) {
	p := newTestPlugin(t, groups3RoundRobin(), defaultConfig())
	var got []string
	for i := 0; i < 4; i++ {
		out, err := p.expand("g", 1, map[string]bool{}, false)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out[0])
	}
	want := []string{"m1", "m2", "m3", "m1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round-robin order %v, want %v", got, want)
		}
	}
}

func TestFallbackOrder(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Strategy: "fallback", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true},
			{Model: "m2", Enabled: true},
		}},
	}}, defaultConfig())
	out, err := p.expand("g", 1, map[string]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0] != "m1" || out[1] != "m2" {
		t.Fatalf("fallback order = %v", out)
	}
}

func TestCoolingRemovesMember(t *testing.T) {
	clock := time.Unix(1000, 0)
	p := newTestPlugin(t, groups3RoundRobin(), defaultConfig())
	p.rt.setClock(func() time.Time { return clock })
	// Force m1 into cooldown for 30s.
	p.rt.recordCooldown("m1", 429, "", p.cfg)
	clock = clock.Add(5 * time.Second) // still cooling
	p.rt.nowFunc = func() time.Time { return clock }
	out, err := p.expand("g", 1, map[string]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out {
		if m == "m1" {
			t.Fatalf("cooling m1 should be removed, got %v", out)
		}
	}
}

func TestNestingExpandOrder(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "outer", Strategy: "fallback", Enabled: true, Members: []rawMember{
			{Model: "a", Enabled: true},
			{Group: "inner", Enabled: true},
			{Model: "b", Enabled: true},
		}},
		{Name: "inner", Strategy: "round-robin", Enabled: true, Members: []rawMember{
			{Model: "c", Enabled: true},
			{Model: "d", Enabled: true},
		}},
	}}, defaultConfig())
	out, err := p.expand("outer", 1, map[string]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "c", "d", "b"}
	if len(out) != len(want) {
		t.Fatalf("nested expand = %v want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("nested expand = %v want %v", out, want)
		}
	}
	// inner rr pointer advanced once.
	if p.rt.pointers["inner"] != 1 {
		t.Fatalf("inner pointer = %d, want 1", p.rt.pointers["inner"])
	}
}

func TestDepthLimit(t *testing.T) {
	// Build an 9-level nested chain.
	groups := []rawGroup{}
	for i := 0; i < 9; i++ {
		name := "l" + string(rune('0'+i))
		next := "l" + string(rune('1'+i))
		if i == 8 {
			next = "leaf"
		}
		groups = append(groups, rawGroup{Name: name, Enabled: true, Members: []rawMember{{Group: next}}})
	}
	groups = append(groups, rawGroup{Name: "leaf", Enabled: true, Members: []rawMember{{Model: "m"}}})
	p := newTestPlugin(t, groupFile{Version: 1, Groups: groups}, defaultConfig())
	_, err := p.expand("l0", 1, map[string]bool{}, false)
	if err == nil || !containsSubstr(err.Error(), "嵌套层数") {
		t.Fatalf("expected depth error, got %v", err)
	}
}

func TestNestedCycle(t *testing.T) {
	// Cycle detection runs at load time (§5.7): a cyclic config is rejected by
	// parseGroups before it can reach runtime expand. The a -> b -> a case must
	// surface as a 循环嵌套 error carrying the cycle path.
	_, _, err := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Enabled: true, Members: []rawMember{{Group: "b"}}},
		{Name: "b", Enabled: true, Members: []rawMember{{Group: "a"}}},
	}})
	if err == nil || !containsSubstr(err.Error(), "循环嵌套") || !containsSubstr(err.Error(), "a -> b -> a") {
		t.Fatalf("expected a -> b -> a cycle error, got %v", err)
	}

	// Runtime expand must also never loop: the visited guard is a defense-in-depth
	// double check. Build a valid config that reuses the same group twice in one
	// expansion path (a diamond) and confirm it terminates without infinite loop
	// and without spurious cycle error.
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "root", Enabled: true, Members: []rawMember{{Group: "a"}, {Group: "b"}}},
		{Name: "a", Enabled: true, Members: []rawMember{{Model: "m1"}}},
		{Name: "b", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())
	out, err := p.expand("root", 1, map[string]bool{}, false)
	if err != nil {
		t.Fatalf("diamond expand error: %v", err)
	}
	if len(out) != 1 || out[0] != "m1" {
		t.Fatalf("diamond dedup = %v, want [m1]", out)
	}
}

func TestBackoffSequence(t *testing.T) {
	clock := time.Unix(1000, 0)
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())
	p.cfg.Cooldown.Jitter = 0 // deterministic

	// Sequence of 429s: 30s, 60s, 120s.
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	for i, w := range want {
		p.rt.nowFunc = func() time.Time { return clock }
		d := p.rt.recordCooldown("m1", 429, "", p.cfg)
		if d != w {
			t.Fatalf("backoff[%d] = %v, want %v", i, d, w)
		}
		clock = clock.Add(w)
	}
	// Exceeds max (30m) -> capped.
	for i := 0; i < 20; i++ {
		p.rt.nowFunc = func() time.Time { return clock }
		d := p.rt.recordCooldown("m1", 429, "", p.cfg)
		clock = clock.Add(d)
		if d > 30*time.Minute {
			t.Fatalf("backoff exceeded max: %v", d)
		}
	}
	// Success resets level.
	p.rt.recordSuccess("m1", 0)
	if p.rt.coolingInfo("m1").Level != 0 {
		t.Fatalf("level not reset after success: %d", p.rt.coolingInfo("m1").Level)
	}
}

func TestOther4xxNoCooldown(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, defaultConfig())
	p.rt.recordCooldown("m1", 400, "", p.cfg)
	if p.rt.isCooling("m1") {
		t.Fatalf("4xx (400) should not cool down")
	}
}

func TestAllCoolingWait(t *testing.T) {
	// Clock advances with real elapsed time so the wait policy can observe
	// cooldown expiry.
	base := time.Unix(1000, 0)
	cfg := defaultConfig()
	cfg.AllCoolingPolicy = "wait"
	cfg.MaxWait = "15s"
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, cfg)
	start := time.Now()
	p.rt.setClock(func() time.Time { return base.Add(time.Since(start)) })
	// m1 cools for 10s (within max_wait 15s), so the wait should sleep ~10s and
	// then recover the member.
	p.rt.cooldowns["m1"] = &cooldownState{Until: base.Add(10 * time.Second), Level: 1, LastStatus: 429}
	attempts, err := p.selectAttempts(p.mustGroup("g"))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("selectAttempts wait error: %v", err)
	}
	if len(attempts) != 1 || attempts[0] != "m1" {
		t.Fatalf("expected m1 recovered after wait, got %v", attempts)
	}
	if elapsed < 9*time.Second || elapsed > 11*time.Second {
		t.Fatalf("wait elapsed = %v, want ~10s (min(earliest 10s, max_wait 15s))", elapsed)
	}
}

func TestAllCoolingFirst(t *testing.T) {
	clock := time.Unix(1000, 0)
	cfg := defaultConfig()
	cfg.AllCoolingPolicy = "first"
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, cfg)
	p.rt.setClock(func() time.Time { return clock })
	p.rt.recordCooldown("m1", 429, "", p.cfg)
	attempts, err := p.selectAttempts(p.mustGroup("g"))
	if err != nil {
		t.Fatalf("selectAttempts first error: %v", err)
	}
	if len(attempts) != 1 || attempts[0] != "m1" {
		t.Fatalf("first policy should ignore cooldown, got %v", attempts)
	}
}

func TestAllCoolingError(t *testing.T) {
	cfg := defaultConfig()
	cfg.AllCoolingPolicy = "error"
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Enabled: true, Members: []rawMember{{Model: "m1"}}},
	}}, cfg)
	p.rt.recordCooldown("m1", 429, "", p.cfg)
	_, err := p.selectAttempts(p.mustGroup("g"))
	if err == nil {
		t.Fatalf("error policy should return error when all cooling")
	}
}

func TestDedupAndMaxAttempts(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Enabled: true, Members: []rawMember{{Model: "m1"}}},
		{Name: "b", Enabled: true, Members: []rawMember{{Model: "m1"}}},
		{Name: "g", Enabled: true, Members: []rawMember{{Group: "a"}, {Group: "b"}, {Model: "m1"}}},
	}}, defaultConfig())
	out, err := p.expand("g", 1, map[string]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0] != "m1" {
		t.Fatalf("dedup failed: %v", out)
	}

	// max_attempts truncation
	cfg := defaultConfig()
	cfg.MaxAttempts = 2
	p2 := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Enabled: true, Members: []rawMember{
			{Model: "m1"}, {Model: "m2"}, {Model: "m3"}, {Model: "m4"},
		}},
	}}, cfg)
	out2, err := p2.expand("g", 1, map[string]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out2) != 2 {
		t.Fatalf("max_attempts truncation = %v, want 2", out2)
	}
}

func (p *plugin) mustGroup(name string) *group {
	g, ok := p.groupByName(name)
	if !ok {
		panic("group not found: " + name)
	}
	return g
}
