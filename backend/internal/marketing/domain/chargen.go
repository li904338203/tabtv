package domain

import (
	"fmt"
	"strings"
)

// 生成角色（MKT-T-35；02 MK-26、MKT-F-106~108；dev/05 MKD-28、§4.3）。
//
// ⚠️ 「用什么操作、几张参考图」只在这里算一处：dryRun 报价与真生成走同一份，前端不复制规则。

// 生成角色用到的两个操作码（目录里已有的，**不自造** —— 自造的话 operation 表里没有那一行，
// 绑操作 / 配价 / 能力矩阵全落不上）。
const (
	OpCharacterText2Img = "text2img"
	OpCharacterImg2Img  = "img2img"
)

// CharacterGenOperation 按参考图张数选操作：0 张 → text2img，≥ 1 张 → img2img。
//
// ⚠️ 不按「有没有画风」选：画风是素材输入（只贡献提示词），text2img 本来就收 style。
func CharacterGenOperation(refCount int) string {
	if refCount > 0 {
		return OpCharacterImg2Img
	}
	return OpCharacterText2Img
}

// CharacterGenRequest 是一次生成角色请求的字段（transport 解析后、调用前 TrimSpace）。
type CharacterGenRequest struct {
	ModelCode         string
	Description       string
	StyleCode         string
	ReferenceMediaIDs []int64
	Count             int
}

// MaxCharacterGenRefs 是一次生成角色的参考图张数硬上限。
//
// ⚠️ 这是**防滥用的外沿**，不是模型能收几张：后者由能力矩阵在报价时判（超了照报价原因回，
// dev/05 §3.4 的 42203）。取与角色图片上限相同的 9，用户最多也只会从自己的角色图里挑。
const MaxCharacterGenRefs = MaxCharacterImages

// ValidateGenRequest 校验字段本身（一次报完）：描述 1~MaxAppearance 码点、张数 1~MaxCharacterGenCount、
// 参考图 ID 合法且不重复、模型必填。
//
// 不在这里做的：参考图归属（受理的输入解析按工作区判）、画风存在性（StyleLookup，要查目录）。
func ValidateGenRequest(in CharacterGenRequest) []Issue {
	var out []Issue
	if strings.TrimSpace(in.ModelCode) == "" {
		out = append(out, errIssue("modelCode", IssueRequired, "请选择模型"))
	}
	switch n := RuneLen(strings.TrimSpace(in.Description)); {
	case n == 0:
		out = append(out, errIssue("description", IssueRequired, "请填写角色形象描述"))
	case n > MaxAppearance:
		out = append(out, tooLong("description", "形象描述", n, MaxAppearance))
	}
	if in.Count < 1 || in.Count > MaxCharacterGenCount {
		out = append(out, errIssue("count", IssueBadValue,
			fmt.Sprintf("生成张数只能是 1~%d，当前 %d", MaxCharacterGenCount, in.Count)))
	}
	if n := len(in.ReferenceMediaIDs); n > MaxCharacterGenRefs {
		out = append(out, errIssue("referenceMediaIds", IssueTooMany,
			fmt.Sprintf("参考图最多 %d 张，当前 %d 张", MaxCharacterGenRefs, n)))
	}
	seen := make(map[int64]int, len(in.ReferenceMediaIDs))
	for i, id := range in.ReferenceMediaIDs {
		field := fmt.Sprintf("referenceMediaIds[%d]", i)
		if id <= 0 {
			out = append(out, errIssue(field, IssueBadValue, fmt.Sprintf("第 %d 张参考图的 ID 非法", i+1)))
			continue
		}
		if j, dup := seen[id]; dup {
			// 同一张图传两次会让能力矩阵按 2 张判、上游也收到两份，而用户只看到一张。
			out = append(out, errIssue(field, IssueDuplicate,
				fmt.Sprintf("第 %d 张与第 %d 张是同一张参考图", i+1, j+1)))
			continue
		}
		seen[id] = i
	}
	if n := RuneLen(in.StyleCode); n > 64 {
		out = append(out, tooLong("styleCode", "画风", n, 64))
	}
	return out
}
