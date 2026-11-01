package domain

import (
	"fmt"
	"strings"
)

// 营销角色（平台预置）的内容校验（MKT-T-36；02 MKT-F-120~124、MKT-D-52；dev/05 MKD-30、§3.2）。
//
// ⚠️ 本文件只管「字段本身合不合法」。媒体的**存在 / 来源 / 类型**要查媒体域，在 app 层判
// （来源必须是管理端上传的平台素材：workspace_id=0 ∧ purpose=material_cover）。
// ⚠️ 名称规则与工作区角色**同一处**（CharacterNameIssue）：用户「添加」营销角色时会把名称原样
// 复制进工作区角色，两边规则不一致的话会出现「管理端存得进、用户添加时报名称非法」。

// MaxPresetSummary 是营销角色一句话说明（marketing_preset_character.summary varchar(60)）。
const MaxPresetSummary = 60

// PresetInput 是营销角色新建 / 整份更新的内容（ID 已解析成 int64）。
type PresetInput struct {
	Name       string
	Summary    string
	Appearance string
	VoiceText  string
	// ImageMediaIDs 有序，[0] 为主图（用户添加时同序复制）。
	ImageMediaIDs []int64
	// VoiceMediaID 参考音频，可空（只存不发，MKD-33）。
	VoiceMediaID *int64
}

// Normalized 去掉名称与说明两端空白；0 当作没给音频。形象 / 音色描述原样保留（换行可能是有意的）。
func (p PresetInput) Normalized() PresetInput {
	p.Name = strings.TrimSpace(p.Name)
	p.Summary = strings.TrimSpace(p.Summary)
	if p.ImageMediaIDs == nil {
		p.ImageMediaIDs = []int64{}
	}
	if p.VoiceMediaID != nil && *p.VoiceMediaID == 0 {
		p.VoiceMediaID = nil
	}
	return p
}

// ValidatePreset 校验营销角色内容，按「什么时候拦」三级（与营销选项同一套分法）：
//   - 任何时候拦（error）：名称规则、长度、图片张数超限 / ID 非法 / 重复、音频 ID 非法；
//   - 草稿告警、启用时拦：没有图片（enabling=false 时 warn，true 时 error）；
//   - 只告警（warn）：没有一句话说明、没有形象描述。
//
// ⚠️ 一次报完，Field 是请求体字段路径（`imageMediaIds[2]`…），管理端据此标红。调用前先 Normalized()。
func ValidatePreset(p PresetInput, enabling bool) []Issue {
	var out []Issue
	out = append(out, CharacterNameIssue("name", p.Name)...)
	if n := RuneLen(p.Summary); n > MaxPresetSummary {
		out = append(out, tooLong("summary", "一句话说明", n, MaxPresetSummary))
	}
	if n := RuneLen(p.Appearance); n > MaxAppearance {
		out = append(out, tooLong("appearance", "形象描述", n, MaxAppearance))
	}
	if n := RuneLen(p.VoiceText); n > MaxVoiceText {
		out = append(out, tooLong("voiceText", "音色描述", n, MaxVoiceText))
	}
	switch n := len(p.ImageMediaIDs); {
	case n == 0:
		// 草稿可以先存文字、图后补；但没有图的营销角色上线后，用户添加得到一个没有主图的角色，
		// 建会话时角色槽取不到图（执行路径「没图即失败」）。
		msg := "请至少上传 1 张角色图（启用前必须有图）"
		if enabling {
			out = append(out, errIssue("imageMediaIds", IssueRequired, msg))
		} else {
			out = append(out, warnIssue("imageMediaIds", IssueRequired, msg))
		}
	case n > MaxCharacterImages:
		out = append(out, errIssue("imageMediaIds", IssueTooMany,
			fmt.Sprintf("角色图最多 %d 张，当前 %d 张", MaxCharacterImages, n)))
	}
	seen := make(map[int64]int, len(p.ImageMediaIDs))
	for i, id := range p.ImageMediaIDs {
		field := fmt.Sprintf("imageMediaIds[%d]", i)
		if id <= 0 {
			out = append(out, errIssue(field, IssueBadValue, fmt.Sprintf("第 %d 张图片的 ID 非法", i+1)))
			continue
		}
		if j, dup := seen[id]; dup {
			out = append(out, errIssue(field, IssueDuplicate, fmt.Sprintf("第 %d 张与第 %d 张是同一张图片", i+1, j+1)))
			continue
		}
		seen[id] = i
	}
	if p.VoiceMediaID != nil && *p.VoiceMediaID < 0 {
		out = append(out, errIssue("voiceMediaId", IssueBadValue, "参考音频 ID 非法"))
	}
	if p.Summary == "" {
		out = append(out, warnIssue("summary", IssueRequired, "没有一句话说明：用户端卡片上只显示名称"))
	}
	if strings.TrimSpace(p.Appearance) == "" {
		out = append(out, warnIssue("appearance", IssueRequired,
			"没有形象描述：用户添加后 Agent 只知道名字，不知道这个角色长什么样"))
	}
	return out
}
