package desktop

import (
	"encoding/json"

	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/seed"
)

// 设置页「恢复内置版本」的内置内容来源（TT-T-19/20）：把 seed 包的内置条目转成各域自己的类型。
// 放在装配层而不是让目录域 / Agent 域直接依赖 seed：seed 依赖几乎所有域的配置，反过来依赖就成环了。

// builtinPresets 实现 catalogapp.BuiltinPresets。
type builtinPresets struct{}

func (builtinPresets) BuiltinPreset(code string) (catalogrepo.AdminImagePreset, string, bool) {
	p, ok := seed.LookupBuiltinPreset(code)
	if !ok {
		return catalogrepo.AdminImagePreset{}, "", false
	}
	out := catalogrepo.AdminImagePreset{
		Code: p.Code, Name: p.Name, PresetGroup: p.Group, Spec: p.Spec, InputMode: p.InputMode,
		Icon: p.Icon, Prompt: p.Prompt, Params: json.RawMessage(p.Params), Operation: p.Operation,
		Sort: p.Sort, Description: p.Description, IsNew: p.IsNew,
	}
	if p.Dimensions != "" {
		out.Dimensions = json.RawMessage(p.Dimensions)
	}
	return out, p.Hash, true
}

// builtinSkills 实现 agentapp.BuiltinSkills。
type builtinSkills struct{}

func (builtinSkills) BuiltinSkill(code string) (agentrepo.SkillRestore, bool) {
	s, ok := seed.LookupBuiltinSkill(code)
	if !ok {
		return agentrepo.SkillRestore{}, false
	}
	out := agentrepo.SkillRestore{
		Name: s.Name, Category: s.Category, Description: s.Description,
		Tags: json.RawMessage(s.Tags), Steps: json.RawMessage(s.Steps), Tools: json.RawMessage(s.Tools),
		Body: s.Body, QuickEntry: s.QuickEntry, Sort: s.Sort, Hash: s.Hash,
	}
	for _, f := range s.Refs {
		out.Refs = append(out.Refs, agentrepo.SkillReference{Name: f.Name, Body: f.Body, Sort: f.Sort})
	}
	return out, true
}
