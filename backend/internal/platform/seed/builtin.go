package seed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// builtinHash 是一条内置内容的指纹（TT-T-24）：内置内容改了它就变，seed 据此判断要不要更新没被用户改过的行。
// 用 JSON 序列化各字段再哈希：字段顺序固定，任一字段变化都会反映出来。
func builtinHash(fields ...any) string {
	b, _ := json.Marshal(fields)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BuiltinPreset 是一条内置图片预设的内容（设置页「恢复内置版本」用，TT-T-20）。
type BuiltinPreset struct {
	Code, Name, Group, Spec, InputMode, Operation string
	Prompt, Params, Dimensions                    string
	Sort                                          int
	Icon, Description                             string
	IsNew                                         bool
	Hash                                          string
}

// LookupBuiltinPreset 按 code 取内置预设；不是内置的返回 false。
// 哈希与 seed 写入时的算法一致，恢复后 seed 认得它是「最新内置版本」。
func LookupBuiltinPreset(code string) (BuiltinPreset, bool) {
	all := append(append([]presetSeed{}, pickerPresets...), presets...)
	for _, p := range all {
		if p.code != code {
			continue
		}
		params := p.params
		if params == "" {
			params = "{}"
		}
		return BuiltinPreset{
			Code: p.code, Name: p.name, Group: p.group, Spec: p.spec, InputMode: p.inputMode, Operation: p.operation,
			Prompt: p.prompt, Params: params, Dimensions: p.dimensions, Sort: p.sort,
			Icon: p.icon, Description: p.description, IsNew: p.isNew,
			Hash: builtinHash(p.code, p.name, p.group, p.spec, p.inputMode, p.prompt, params, p.dimensions,
				p.operation, p.sort, p.icon, p.description, p.isNew),
		}, true
	}
	return BuiltinPreset{}, false
}

// BuiltinSkillRef 是内置技能的一份附属文档。
type BuiltinSkillRef struct {
	Name string
	Body string
	Sort int
}

// BuiltinSkill 是一条内置 Agent 技能（设置页「恢复内置版本」用，TT-T-19）。
// Tags / Steps / Tools 已序列化成 JSON，与 seed 写库时一致。
type BuiltinSkill struct {
	Code, Name, Category, Description string
	Tags, Steps, Tools                string
	Body                              string
	QuickEntry                        bool
	Sort                              int
	Refs                              []BuiltinSkillRef
	Hash                              string
}

// LookupBuiltinSkill 按 code 取内置通用技能；不是内置的（含营销技能、用户新建的）返回 false。
func LookupBuiltinSkill(code string) (BuiltinSkill, bool) {
	for _, s := range agentSkillSeeds() {
		if s.code != code {
			continue
		}
		tags, _ := json.Marshal(s.tags)
		steps, _ := json.Marshal(nonNilSteps(s.steps))
		tools, _ := json.Marshal(nonNilStrings(s.tools))
		out := BuiltinSkill{Code: s.code, Name: s.name, Category: s.category, Description: s.description,
			Tags: string(tags), Steps: string(steps), Tools: string(tools), Body: s.body,
			QuickEntry: s.quickEntry, Sort: s.sort,
			Hash: builtinHash(s.code, s.name, s.category, s.description, string(tags), s.body,
				string(steps), string(tools), s.quickEntry, s.sort, refsKey(s.refs))}
		for _, r := range s.refs {
			out.Refs = append(out.Refs, BuiltinSkillRef{Name: r.name, Body: r.body, Sort: r.sort})
		}
		return out, true
	}
	return BuiltinSkill{}, false
}
