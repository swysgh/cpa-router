package main

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// groupsFor parses raw groups for validation tests.
func groupsFor(t *testing.T, gs ...rawGroup) []group {
	t.Helper()
	parsed, _, err := parseGroups(groupFile{Version: 1, Groups: gs})
	if err != nil {
		t.Fatalf("parseGroups: %v", err)
	}
	return parsed
}

// Regression: a model member naming a group call name can never succeed (the
// host resolves it to this plugin's provider, finds no credential, and 503s) and
// would recurse across the plugin boundary if the host ever routed it back here.
// maxNestingDepth does not cover it, because it only counts group members.
func TestValidateModelMembersRejectsGroupCallNames(t *testing.T) {
	cases := []struct {
		name    string
		groups  []rawGroup
		prefix  string
		wantErr bool
	}{
		{"self reference", []rawGroup{
			{Name: "a", Enabled: true, Members: []rawMember{{Model: "a", Enabled: true}}},
		}, "", true},
		{"cross reference", []rawGroup{
			{Name: "a", Enabled: true, Members: []rawMember{{Model: "b", Enabled: true}}},
			{Name: "b", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
		}, "", true},
		{"via alias", []rawGroup{
			{Name: "a", Enabled: true, Aliases: []string{"al"}, Members: []rawMember{{Model: "al", Enabled: true}}},
		}, "", true},
		{"prefixed call name", []rawGroup{
			{Name: "a", Enabled: true, Members: []rawMember{{Model: "r/a", Enabled: true}}},
		}, "r/", true},
		{"bare name is not a call name when prefixed", []rawGroup{
			{Name: "a", Enabled: true, Members: []rawMember{{Model: "a", Enabled: true}}},
		}, "r/", false},
		{"ordinary model", []rawGroup{
			{Name: "a", Enabled: true, Members: []rawMember{{Model: "gpt-x", Enabled: true}}},
		}, "", false},
		{"group reference is fine", []rawGroup{
			{Name: "a", Enabled: true, Members: []rawMember{{Group: "b", Enabled: true}}},
			{Name: "b", Enabled: true, Members: []rawMember{{Model: "m1", Enabled: true}}},
		}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups := groupsFor(t, tc.groups...)
			err := validateModelMembers(groups, tc.prefix)
			if tc.wantErr && err == nil {
				t.Fatal("want an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "组配置错误") {
				t.Errorf("error should use the 组配置错误 prefix, got: %v", err)
			}
		})
	}
}

// The management save path must reject it too, not just the file loader.
func TestApplyGroupsRejectsSelfReferencingModelMember(t *testing.T) {
	h := newFakeHost()
	p := newPlugin(h)
	p.cfg = defaultConfig()
	err := p.applyGroups(groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Strategy: "fallback", Enabled: true,
			Members: []rawMember{{Model: "a", Enabled: true}}},
	}})
	if err == nil {
		t.Fatal("applyGroups accepted a model member naming its own group")
	}
	if !strings.Contains(err.Error(), "组配置错误") {
		t.Errorf("error = %v", err)
	}
}

func TestParseGroupsBasic(t *testing.T) {
	f := groupFile{Version: 1, Groups: []rawGroup{
		{
			Name:     "free-stack",
			Strategy: "round-robin",
			Enabled:  true,
			Members: []rawMember{
				{Model: "m1", Enabled: true},
				{Model: "m2", Enabled: true},
			},
		},
	}}
	groups, _, err := parseGroups(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != "free-stack" {
		t.Fatalf("bad groups: %+v", groups)
	}
	if groups[0].Strategy != "round-robin" {
		t.Errorf("strategy = %q", groups[0].Strategy)
	}
}

func TestParseGroupsMissingVersion(t *testing.T) {
	_, _, err := parseGroups(groupFile{Version: 0, Groups: []rawGroup{{Name: "a", Members: []rawMember{{Model: "m"}}}}})
	if err == nil || !containsSubstr(err.Error(), "version") {
		t.Fatalf("expected version error, got %v", err)
	}
}

func TestParseGroupsDupName(t *testing.T) {
	f := groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Members: []rawMember{{Model: "m"}}},
		{Name: "a", Members: []rawMember{{Model: "m"}}},
	}}
	_, _, err := parseGroups(f)
	if err == nil || !containsSubstr(err.Error(), "组名重复") {
		t.Fatalf("expected dup name error, got %v", err)
	}
}

func TestParseGroupsAliasConflict(t *testing.T) {
	f := groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Aliases: []string{"x"}, Members: []rawMember{{Model: "m"}}},
		{Name: "b", Aliases: []string{"x"}, Members: []rawMember{{Model: "m"}}},
	}}
	_, _, err := parseGroups(f)
	if err == nil || !containsSubstr(err.Error(), "别名冲突") {
		t.Fatalf("expected alias conflict error, got %v", err)
	}
}

func TestParseGroupsMemberBoth(t *testing.T) {
	f := groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Members: []rawMember{{Model: "m", Group: "b"}}},
	}}
	_, _, err := parseGroups(f)
	if err == nil || !containsSubstr(err.Error(), "model") {
		t.Fatalf("expected member-both error, got %v", err)
	}
}

func TestParseGroupsRefMissing(t *testing.T) {
	f := groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Members: []rawMember{{Group: "ghost"}}},
	}}
	_, _, err := parseGroups(f)
	if err == nil || !containsSubstr(err.Error(), "不存在的组") {
		t.Fatalf("expected missing group error, got %v", err)
	}
}

func TestParseGroupsSelfCycle(t *testing.T) {
	f := groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Members: []rawMember{{Group: "a"}}},
	}}
	_, _, err := parseGroups(f)
	if err == nil || !containsSubstr(err.Error(), "循环嵌套") || !containsSubstr(err.Error(), "a -> a") {
		t.Fatalf("expected self cycle error, got %v", err)
	}
}

func TestParseGroupsThreeNodeCycle(t *testing.T) {
	f := groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Members: []rawMember{{Group: "b"}}},
		{Name: "b", Members: []rawMember{{Group: "c"}}},
		{Name: "c", Members: []rawMember{{Group: "a"}}},
	}}
	_, _, err := parseGroups(f)
	if err == nil || !containsSubstr(err.Error(), "循环嵌套") || !containsSubstr(err.Error(), "a -> b -> c -> a") {
		t.Fatalf("expected 3-node cycle path, got %v", err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/groups.yaml"
	store := newGroupStore(path)
	in := groupFile{Version: 1, Groups: []rawGroup{
		{Name: "a", Strategy: "fallback", Enabled: true, Aliases: []string{}, Members: []rawMember{{Model: "m1", Enabled: true}}},
	}}
	if err := store.save(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, abs, err := store.load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if abs == "" {
		t.Fatalf("abs path empty")
	}
	// The on-disk round trip is authoritative: re-serialize `in` through YAML so
	// its transient parse state (e.g. enabledSet) matches what load() produces.
	inYAML, _ := yaml.Marshal(in)
	var inNorm groupFile
	if err := yaml.Unmarshal(inYAML, &inNorm); err != nil {
		t.Fatalf("re-marshal in: %v", err)
	}
	if !reflect.DeepEqual(out, inNorm) {
		t.Fatalf("round trip mismatch: in=%+v out=%+v", inNorm, out)
	}
}

func TestNamePrefixAliasIndex(t *testing.T) {
	f := groupFile{Version: 1, Groups: []rawGroup{
		{Name: "grp", Aliases: []string{"alias"}, Members: []rawMember{{Model: "m"}}},
	}}
	groups, _, err := parseGroups(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	idx := buildGroupNameIndex(groups, "r/")
	if idx["r/grp"] != "grp" {
		t.Errorf("prefix group index missing: %+v", idx)
	}
	if idx["r/alias"] != "grp" {
		t.Errorf("prefix alias index missing: %+v", idx)
	}
	if _, ok := idx["grp"]; ok {
		t.Errorf("bare group name should not be indexed with prefix")
	}
}
