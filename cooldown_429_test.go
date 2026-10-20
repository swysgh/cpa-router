package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// TestRoundRobinExcludes429Member is the round-robin counterpart of
// TestExecutorFallbackOn429: after a member answers 429 it must not be picked
// again until its cooldown lapses, in every rotating strategy.
func TestRoundRobinExcludes429Member(t *testing.T) {
	for _, strategy := range []string{"round-robin", "weighted-round-robin"} {
		t.Run(strategy, func(t *testing.T) {
			h := newFakeHost()
			p := newPlugin(h)
			p.setConfig(defaultConfig())
			// Weight only applies to weighted groups: setting it on a plain
			// round-robin group is rejected, which would leave the group empty.
			w := 3
			m1 := rawMember{Model: "m1", Enabled: true}
			if strategy == "weighted-round-robin" {
				m1.Weight = &w
			}
			parsed, _, err := parseGroups(groupFile{Version: 1, Groups: []rawGroup{
				{Name: "g", Strategy: strategy, Enabled: true, Members: []rawMember{
					m1,
					{Model: "m2", Enabled: true},
					{Model: "m3", Enabled: true},
				}},
			}}, "")
			if err != nil {
				t.Fatalf("parseGroups: %v", err)
			}
			p.groups = parsed
			p.nameIndex = buildGroupNameIndex(parsed, "")

			h.setHandler(pluginabi.MethodHostModelExecute, func(payload json.RawMessage) (json.RawMessage, error) {
				var req hostModelExecutionRequest
				_ = json.Unmarshal(payload, &req)
				if req.Model == "m1" {
					return hostErrorResult("upstream", "rate limited", 429), nil
				}
				return hostExecuteResult(200, []byte(`{"ok":true}`), nil), nil
			})

			// Burn requests until m1 has been hit and cooled.
			hit429 := false
			for i := 0; i < 10 && !hit429; i++ {
				raw, err := p.execute(execReq("g", []byte(`{"model":"g","messages":[]}`)))
				if err != nil {
					t.Fatalf("execute: %v", err)
				}
				if _, status := decodeExecutorResponse(t, mustOK(t, raw, err)); status == 200 {
					hit429 = p.rt.isCooling("m1")
				}
			}
			if !hit429 {
				t.Fatal("m1 never hit 429 (scheduler did not reach it)")
			}

			// While cooling, m1 must never be picked again.
			for i := 0; i < 20; i++ {
				seq, err := p.expand("g", 1, map[string]bool{}, expandOpts{})
				if err != nil {
					t.Fatalf("expand: %v", err)
				}
				if seq[0] == "m1" {
					t.Fatalf("cooling member m1 was picked on request %d", i)
				}
			}
		})
	}
}

// TestCoolingIsScopedPerModelName pins the shared-cooldown contract: a model
// cooled through one group is skipped by every other group referencing it,
// because cooldown state is keyed by model name.
func TestCoolingIsScopedPerModelName(t *testing.T) {
	p := newTestPlugin(t, groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Strategy: "round-robin", Enabled: true, Members: []rawMember{
			{Model: "shared", Enabled: true}, {Model: "a-only", Enabled: true},
		}},
		{Name: "b", Strategy: "round-robin", Enabled: true, Members: []rawMember{
			{Model: "shared", Enabled: true}, {Model: "b-only", Enabled: true},
		}},
	}}, defaultConfig())

	p.rt.recordCooldown("shared", 429, "", *p.conf())
	for _, g := range []string{"a", "b"} {
		seq, err := p.expand(g, 1, map[string]bool{}, expandOpts{})
		if err != nil {
			t.Fatalf("expand %s: %v", g, err)
		}
		for _, m := range seq {
			if m == "shared" {
				t.Fatalf("group %s still offers the cooling model", g)
			}
		}
	}
}

// TestNonCooldownStatusesAreRetried documents the deliberate boundary: statuses
// outside cooldown.statuses / transient_statuses get stats only, so a member
// that fails with, say, 400 keeps its slot.
func TestNonCooldownStatusesAreRetried(t *testing.T) {
	p := newTestPlugin(t, groups3RoundRobin(), defaultConfig())
	for _, st := range []int{400, 404, 200} {
		if d := p.rt.recordCooldown("m1", st, "", *p.conf()); d != 0 {
			t.Fatalf("status %d produced a %s cooldown, want none", st, d)
		}
	}
	if p.rt.isCooling("m1") {
		t.Fatal("m1 cooling after non-cooldown statuses")
	}
}

// Test429CooldownBacksOff guards the retry ladder: a repeatedly failing member
// must wait longer each time so a dead upstream is not retried every 30s forever.
func Test429CooldownBacksOff(t *testing.T) {
	p := newTestPlugin(t, groups3RoundRobin(), defaultConfig())
	var delays []int
	for i := 0; i < 4; i++ {
		delays = append(delays, int(p.rt.recordCooldown("m1", 429, "", *p.conf()).Seconds()))
	}
	for i := 1; i < len(delays); i++ {
		if delays[i] <= delays[i-1] {
			t.Fatalf("cooldown did not grow: %v", delays)
		}
	}
	if delays[len(delays)-1] > int(p.conf().CooldownMax.Seconds()) {
		t.Fatalf("cooldown %d exceeds max %s", delays[len(delays)-1], p.conf().CooldownMax)
	}
}
