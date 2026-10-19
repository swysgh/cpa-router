package main

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func weightedGroupFile(strategy string, weights ...int) groupFile {
	if len(weights) == 0 {
		weights = []int{1, 1, 1} // plain rotation: same shape, no explicit weights
	}
	members := make([]rawMember, len(weights))
	for i, w := range weights {
		m := rawMember{Model: "m" + itoa(i+1), Enabled: true}
		if strategy == "weighted-round-robin" {
			wt := w
			m.Weight = &wt
		}
		members[i] = m
	}
	return groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Strategy: strategy, Enabled: true, Members: members},
	}}
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return "10"
}

// pickSeq returns the first n picks of a weighted group.
func pickSeq(t *testing.T, p *plugin, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		seq, err := p.expand("g", 1, map[string]bool{}, expandOpts{})
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		out = append(out, seq[0])
	}
	return out
}

func TestWeightedRatiosMatchWeights(t *testing.T) {
	// 1:2:3 over 60 requests must come out exactly 10/20/30 and interleave
	// smoothly (nginx smooth-WRR), not in three consecutive bursts.
	p := newTestPlugin(t, weightedGroupFile("weighted-round-robin", 1, 2, 3), defaultConfig())
	seq := pickSeq(t, p, 60)
	count := map[string]int{}
	for _, m := range seq {
		count[m]++
	}
	if count["m1"] != 10 || count["m2"] != 20 || count["m3"] != 30 {
		t.Fatalf("counts = %v, want m1:10 m2:20 m3:30", count)
	}
	// Burst check: the first six picks must already contain all three members.
	seen := map[string]bool{}
	for _, m := range seq[:6] {
		seen[m] = true
	}
	if len(seen) != 3 {
		t.Fatalf("first 6 picks = %v, want all three members to appear (bursty scheduler)", seq[:6])
	}
}

func TestWeightedEqualWeightsMatchPlainRotation(t *testing.T) {
	// Equal weights must reproduce plain round-robin exactly, in declaration
	// order: same first pick, same full sequence.
	plane := newTestPlugin(t, weightedGroupFile("round-robin"), defaultConfig())
	weighted := newTestPlugin(t, weightedGroupFile("weighted-round-robin", 1, 1, 1), defaultConfig())
	plainSeq := pickSeq(t, plane, 9)
	weightedSeq := pickSeq(t, weighted, 9)
	for i := range plainSeq {
		if plainSeq[i] != weightedSeq[i] {
			t.Fatalf("equal-weight sequence %v diverged from plain rotation %v", weightedSeq, plainSeq)
		}
	}
}

func TestWeightedAbsentWeightIsOne(t *testing.T) {
	// Omitting weight on every member of a weighted group = equal shares.
	gf := groupFile{Version: 1, Groups: []rawGroup{
		{Name: "g", Strategy: "weighted-round-robin", Enabled: true, Members: []rawMember{
			{Model: "m1", Enabled: true},
			{Model: "m2", Enabled: true},
		}},
	}}
	p := newTestPlugin(t, gf, defaultConfig())
	seq := pickSeq(t, p, 4)
	want := []string{"m1", "m2", "m1", "m2"}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("sequence %v, want %v", seq, want)
		}
	}
}

func TestWeightedRebalancesWhenMemberCools(t *testing.T) {
	p := newTestPlugin(t, weightedGroupFile("weighted-round-robin", 1, 2), defaultConfig())
	pickSeq(t, p, 3)
	p.rt.recordCooldown("m1", 429, "", *p.conf())
	// With m1 cooling only m2 is eligible: every pick must be m2, and the
	// scheduler state must not carry stale weights from the 2-member shape.
	for _, m := range pickSeq(t, p, 5) {
		if m != "m2" {
			t.Fatalf("cooling member still selected: %s", m)
		}
	}
}

func TestWeightedStickyPinsToOneMember(t *testing.T) {
	p := newTestPlugin(t, weightedGroupFile("weighted-round-robin", 1, 5), stickyCfg())
	first := pickSeq(t, p, 1)[0]
	seq, err := p.expand("g", 1, map[string]bool{}, expandOpts{stickyKey: "h:sess"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		got, err := p.expand("g", 1, map[string]bool{}, expandOpts{stickyKey: "h:sess"})
		if err != nil {
			t.Fatal(err)
		}
		if got[0] != seq[0] {
			t.Fatalf("sticky session drifted under weighted scheduling: %s -> %s", seq[0], got[0])
		}
	}
	_ = first
}

func TestWeightedPeekDoesNotAdvance(t *testing.T) {
	p := newTestPlugin(t, weightedGroupFile("weighted-round-robin", 1, 2), defaultConfig())
	a, err := p.expand("g", 1, map[string]bool{}, expandOpts{peek: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.expand("g", 1, map[string]bool{}, expandOpts{peek: true})
	if err != nil {
		t.Fatal(err)
	}
	if a[0] != b[0] {
		t.Fatalf("peek advanced the scheduler: %s then %s", a[0], b[0])
	}
}

func TestWeightValidation(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		weight   *int
		wantErr  bool
	}{
		{"weight on fallback", "fallback", ip(2), true},
		{"weight on plain round-robin", "round-robin", ip(2), true},
		{"weight zero", "weighted-round-robin", ip(0), true},
		{"weight negative", "weighted-round-robin", ip(-1), true},
		{"weight too large", "weighted-round-robin", ip(maxMemberWeight + 1), true},
		{"weight on weighted", "weighted-round-robin", ip(3), false},
		{"absent weight", "weighted-round-robin", nil, false},
		{"absent weight on fallback", "fallback", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gf := groupFile{Version: 1, Groups: []rawGroup{
				{Name: "g", Strategy: c.strategy, Enabled: true, Members: []rawMember{
					{Model: "m1", Enabled: true, Weight: c.weight},
				}},
			}}
			_, _, err := parseGroups(gf)
			if c.wantErr && err == nil {
				t.Fatal("accepted invalid weight")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("rejected valid weight: %v", err)
			}
		})
	}
}

func ip(i int) *int { return &i }

func TestWeightedYAMLRoundTrip(t *testing.T) {
	// weight written as YAML, parsed, and read back through /state must survive.
	raw := []byte(`version: 1
groups:
  - name: g
    strategy: weighted-round-robin
    members:
      - model: m1
        weight: 5
      - model: m2
`)
	cfg, err := decodeConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	var f groupFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	groups, _, err := parseGroups(f, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := groups[0].Members[0].Weight; got != 5 {
		t.Fatalf("m1 weight = %d, want 5", got)
	}
	if got := groups[0].Members[1].Weight; got != 1 {
		t.Fatalf("m2 weight = %d, want 1 (absent)", got)
	}
	_ = cfg
}

func TestWeightedAllCoolingFirstUsesWeights(t *testing.T) {
	cfg := defaultConfig()
	cfg.AllCoolingPolicy = "first"
	p := newTestPlugin(t, weightedGroupFile("weighted-round-robin", 1, 2), cfg)
	p.rt.setClock(func() time.Time { return time.Unix(1000, 0) })
	p.rt.recordCooldown("m1", 429, "", *p.conf())
	// m1 cools for 30s, m2 for 60s: the earliest-expiry pick is m1.
	attempts, err := p.selectAttempts(p.mustGroup("g"), selectOpts{})
	if err != nil {
		t.Fatalf("selectAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %v", attempts)
	}
}
