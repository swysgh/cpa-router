package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxNestingDepth is the hardcoded upper bound on group nesting (§5.8).
const maxNestingDepth = 8

// nameRegex matches group names and aliases: ^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$
var nameRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// rawGroup is the on-disk representation of a group.
type rawGroup struct {
	Name        string      `yaml:"name"`
	Strategy    string      `yaml:"strategy"`
	Enabled     bool        `yaml:"enabled"`
	Description string      `yaml:"description"`
	Aliases     []string    `yaml:"aliases"`
	Members     []rawMember `yaml:"members"`
	// enabledSet tracks whether the YAML author explicitly set "enabled". When
	// the field is absent the group is treated as enabled (opt-out semantics),
	// so a compact hand-written groups.yaml without `enabled:` still routes.
	enabledSet bool `yaml:"-"`
}

// UnmarshalYAML implements presence-aware decoding for rawGroup so an omitted
// "enabled" key defaults to true (enabled), while an explicit `enabled: false`
// disables the group.
func (g *rawGroup) UnmarshalYAML(value *yaml.Node) error {
	type plain rawGroup
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*g = rawGroup(p)
	g.enabledSet = false
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "enabled" {
				g.enabledSet = true
				break
			}
		}
	}
	if !g.enabledSet {
		g.Enabled = true
	}
	return nil
}

// rawMember is a single group member: exactly one of model / group.
type rawMember struct {
	Model   string `yaml:"model"`
	Group   string `yaml:"group"`
	Enabled bool   `yaml:"enabled"`
	// enabledSet tracks whether the YAML author explicitly set "enabled". When
	// the field is absent we treat the member as enabled (opt-out semantics: a
	// member is active unless explicitly disabled). This keeps state files
	// compact while still allowing `enabled: false` to disable a member.
	enabledSet bool `yaml:"-"`
}

// UnmarshalYAML implements presence-aware decoding for rawMember so an omitted
// "enabled" key defaults to true (enabled), while an explicit `enabled: false`
// disables the member.
func (m *rawMember) UnmarshalYAML(value *yaml.Node) error {
	// Decode into a plain struct without the presence flag.
	type plain rawMember
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*m = rawMember(p)
	m.enabledSet = false
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "enabled" {
				m.enabledSet = true
				break
			}
		}
	}
	if !m.enabledSet {
		m.Enabled = true
	}
	return nil
}

// group is the validated in-memory representation.
type group struct {
	Name        string
	Strategy    string // "round-robin" | "fallback"
	Enabled     bool
	Description string
	Aliases     []string
	Members     []member
}

// member is a validated member reference.
type member struct {
	IsModel bool   // true => Model set, false => Group set
	Model   string // real model name
	Group   string // referenced group name
	Enabled bool
}

// groupFile is the top-level state file structure.
type groupFile struct {
	Version int        `yaml:"version"`
	Groups  []rawGroup `yaml:"groups"`
}

// parseGroups validates a groupFile and returns the validated slice plus a
// call-name -> group-name index. It performs name/alias uniqueness, member
// validation, reference existence, and cycle detection.
func parseGroups(f groupFile) ([]group, map[string]string, error) {
	if f.Version != 1 {
		return nil, nil, fmt.Errorf("组配置错误: version 必须为 1，实际 %d", f.Version)
	}

	byName := make(map[string]rawGroup, len(f.Groups))
	aliasToName := make(map[string]string)
	seenNames := make(map[string]bool)

	for _, rg := range f.Groups {
		name := strings.TrimSpace(rg.Name)
		if name == "" {
			return nil, nil, fmt.Errorf("组配置错误: 组名为空")
		}
		if !nameRegex.MatchString(name) {
			return nil, nil, fmt.Errorf("组配置错误: 组名 %q 不符合命名规则", name)
		}
		if seenNames[name] {
			return nil, nil, fmt.Errorf("组配置错误: 组名重复: %s", name)
		}
		seenNames[name] = true

		strategy := strings.TrimSpace(rg.Strategy)
		if strategy == "" {
			strategy = "fallback"
		}
		if strategy != "round-robin" && strategy != "fallback" {
			return nil, nil, fmt.Errorf("组配置错误: 组 %s 的 strategy 必须是 round-robin|fallback", name)
		}

		// Validate aliases.
		cleanAliases := make([]string, 0, len(rg.Aliases))
		for _, a := range rg.Aliases {
			ca := strings.TrimSpace(a)
			if ca == "" {
				continue
			}
			if !nameRegex.MatchString(ca) {
				return nil, nil, fmt.Errorf("组配置错误: 组 %s 的别名 %q 不符合命名规则", name, ca)
			}
			if seenNames[ca] {
				return nil, nil, fmt.Errorf("组配置错误: 组 %s 的别名 %q 与组名冲突", name, ca)
			}
			if _, dup := aliasToName[ca]; dup {
				return nil, nil, fmt.Errorf("组配置错误: 别名冲突: %s", ca)
			}
			aliasToName[ca] = name
			cleanAliases = append(cleanAliases, ca)
		}

		// Validate members.
		var members []member
		for i, m := range rg.Members {
			if strings.TrimSpace(m.Model) != "" && strings.TrimSpace(m.Group) != "" {
				return nil, nil, fmt.Errorf("组配置错误: 组 %s 第 %d 个成员同时指定了 model 与 group", name, i+1)
			}
			if strings.TrimSpace(m.Model) == "" && strings.TrimSpace(m.Group) == "" {
				return nil, nil, fmt.Errorf("组配置错误: 组 %s 第 %d 个成员必须指定 model 或 group 之一", name, i+1)
			}
			en := memberEnabled(m)
			if strings.TrimSpace(m.Model) != "" {
				mm := strings.TrimSpace(m.Model)
				if !nameRegex.MatchString(mm) {
					return nil, nil, fmt.Errorf("组配置错误: 组 %s 成员模型 %q 不符合命名规则", name, mm)
				}
				members = append(members, member{IsModel: true, Model: mm, Enabled: en})
			} else {
				gg := strings.TrimSpace(m.Group)
				if !nameRegex.MatchString(gg) {
					return nil, nil, fmt.Errorf("组配置错误: 组 %s 成员组 %q 不符合命名规则", name, gg)
				}
				members = append(members, member{IsModel: false, Group: gg, Enabled: en})
			}
		}

		byName[name] = rawGroup{
			Name:        name,
			Strategy:    strategy,
			Enabled:     rg.Enabled,
			Description: rg.Description,
			Aliases:     cleanAliases,
			Members:     rg.Members,
		}
		_ = members // built later after existence check
	}

	// Reference existence + cycle detection.
	for name, rg := range byName {
		for _, m := range rg.Members {
			if rg.Members == nil {
				continue
			}
			if strings.TrimSpace(m.Group) != "" {
				if _, ok := byName[m.Group]; !ok {
					return nil, nil, fmt.Errorf("组配置错误: 组 %s 引用了不存在的组 %s", name, m.Group)
				}
			}
		}
	}

	if cycle := detectCycle(byName); cycle != "" {
		return nil, nil, fmt.Errorf("组配置错误: 检测到循环嵌套: %s", cycle)
	}

	// Build final validated groups with members.
	out := make([]group, 0, len(f.Groups))
	for _, rg := range f.Groups {
		name := strings.TrimSpace(rg.Name)
		g := group{
			Name:        name,
			Strategy:    byName[name].Strategy,
			Enabled:     rg.Enabled,
			Description: rg.Description,
			Aliases:     byName[name].Aliases,
		}
		for _, m := range rg.Members {
			en := memberEnabled(m)
			if strings.TrimSpace(m.Model) != "" {
				g.Members = append(g.Members, member{IsModel: true, Model: strings.TrimSpace(m.Model), Enabled: en})
			} else {
				g.Members = append(g.Members, member{IsModel: false, Group: strings.TrimSpace(m.Group), Enabled: en})
			}
		}
		out = append(out, g)
	}

	// Build call-name index (name_prefix applied by caller via resolveNames).
	return out, aliasToName, nil
}

// memberEnabled resolves the effective enabled state of a rawMember. A member is
// enabled unless its "enabled" field was explicitly set to false (opt-out).
// This keeps state files compact: omitting the field means enabled.
func memberEnabled(m rawMember) bool {
	if !m.enabledSet {
		return true
	}
	return m.Enabled
}

// detectCycle performs DFS over group references and returns the cycle path as
// "a -> b -> c -> a" or "" when no cycle exists.
func detectCycle(byName map[string]rawGroup) string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int)
	var stack []string

	var dfs func(string) string
	dfs = func(n string) string {
		color[n] = gray
		stack = append(stack, n)
		for _, m := range byName[n].Members {
			if strings.TrimSpace(m.Group) != "" {
				switch color[m.Group] {
				case gray:
					// Build cycle path from the stack.
					idx := 0
					for i, s := range stack {
						if s == m.Group {
							idx = i
							break
						}
					}
					path := append(append([]string{}, stack[idx:]...), m.Group)
					return strings.Join(path, " -> ")
				case white:
					if p := dfs(m.Group); p != "" {
						return p
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return ""
	}

	// Iterate in a deterministic (sorted) order so cycle reporting is stable and
	// the reported path starts from the alphabetically-first group in the cycle.
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if color[n] == white {
			if p := dfs(n); p != "" {
				return p
			}
		}
	}
	return ""
}

// buildGroupNameIndex builds the call-name -> group-name index given a prefix.
func buildGroupNameIndex(groups []group, prefix string) map[string]string {
	idx := make(map[string]string)
	for _, g := range groups {
		idx[prefix+g.Name] = g.Name
		for _, a := range g.Aliases {
			idx[prefix+a] = g.Name
		}
	}
	return idx
}

// validateModelMembers rejects a model member whose name is a group call name.
//
// Such a member can never succeed: the name resolves back to this plugin, so the
// host looks for a credential for the plugin's provider, finds none, and fails
// the attempt (observed: 503 auth_not_found), while also cooling the name down.
// It is only "safe" because the host happens to require an auth for the provider
// instead of re-entering this plugin's executor — if that ever changed, the
// request would recurse across the plugin boundary, which maxNestingDepth does
// not bound (it only counts group members). Rejecting it at save time turns a
// silently dead member into a clear error.
func validateModelMembers(groups []group, prefix string) error {
	callNames := buildGroupNameIndex(groups, prefix)
	for _, g := range groups {
		for i, m := range g.Members {
			if !m.IsModel {
				continue
			}
			owner, hit := callNames[m.Model]
			if !hit {
				continue
			}
			where := ""
			if owner != g.Name {
				where = fmt.Sprintf("（它指向组 %s）", owner)
			}
			return fmt.Errorf("组配置错误: 组 %s 第 %d 个成员把 %q 当成模型，但 %q 是组调用名%s；"+
				"要引用组请把该成员的类型改成「组」，否则它永远不会成功",
				g.Name, i+1, m.Model, m.Model, where)
		}
	}
	return nil
}
