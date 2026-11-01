package app

import "encoding/json"

// 技能保存时要问营销域的两件事（MKT-T-05，dev/01 §2.2）。
//
// ⚠️ 端口定义在 agent，实现在 wiring（适配到 marketing/domain）：agent 不 import marketing。
// 口播禁用词与槽位格式全仓各只有一份实现（MKD-12）—— 在这里再抄一份词表或槽名，
// 迟早一边少一个词、另一边多一个槽，而两边各自都"没错"。

// CopyIssue 是一条文案 / 槽位问题。Field 是请求体里的字段路径（如 `prefillText`、
// `steps[1].name`、`inputSlots[0].maxCount`），为空表示不针对某个字段（如「缺口播提示句」）。
type CopyIssue struct {
	Field   string
	Code    string
	Message string
}

// CopyChecker 检查技能面向用户的文案（名称、说明、预填描述、步骤名）。
// errs 拦截保存（禁用词），warns 只提示（缺口播提示句、合规风险用语）。
type CopyChecker interface {
	CheckCopy(fields map[string]string, tags []string) (errs, warns []CopyIssue)
}

// SlotDeclValidator 校验技能的输入槽声明（`agent_skill.input_slots` 原文，JSON 数组）。
// 返回规范化后的声明（落库用这一份）与全部问题（一次报完）；raw 解不开也作为一条问题返回。
//
// ⚠️ 落库存规范化结果而不是原文：Go 的 JSON 解码对键名**大小写不敏感**，`maxcount` 会被
// 当成 maxCount 校验通过；原文照存的话，前端读 `maxCount` 拿到的是 undefined。
type SlotDeclValidator interface {
	ValidateSlotDecl(raw json.RawMessage) (normalized json.RawMessage, issues []CopyIssue)
}

// SlotDeclNormalizedCode 是 ValidateSlotDecl 返回的「旧形状已换成新形状」提醒的 code（MKD-26）：
// 它**不拦截保存**，作为 warnings 回给管理端（`warnings[{field:"inputSlots", code:"normalized"}]`）。
const SlotDeclNormalizedCode = "normalized"

// SlotDeclNormalizer 把库里的输入槽声明换成新形状再下发（MKD-26，读路径：`/agent/skills`、
// 管理端技能列表 / 详情）。解不开的原样返回 —— 读路径不该因为一条坏数据让整页打不开。
// 实现与 SlotDeclValidator 是同一个（wiring.AgentSkillSlots），规范化只有营销域一处。
type SlotDeclNormalizer interface {
	NormalizeSlotDecl(raw json.RawMessage) json.RawMessage
}
