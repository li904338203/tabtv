package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MarketingParams 是节点 params 里的 `marketing` 段：只有钉住的 (kind, code, version)，
// **没有片段原文**（MK-8：片段不进 canvasData）。
type MarketingParams struct {
	Picks []Pick `json:"picks"`
}

// ParsePicks 从生成参数里取 `params.marketing.picks`。
//
//   - present 表示 `marketing` 段**存在**：存在就要拼场景约束段（哪怕一样都没选），
//     不存在就是通用生成、一个字都不拼。所以「不存在」与「存在但为空」必须分开。
//   - 每项校验 kind / code 格式 / version ≥ 1，坏一项整体报错并点名第几项
//     （⚠️ 不悄悄丢掉坏项：丢掉的话这次生成少了一段配方而任务是成功的）。
//   - 同一 kind+code 只留第一次出现的版本（同 MergePins）。
func ParsePicks(params json.RawMessage) (picks []Pick, present bool, err error) {
	if emptyJSONObject(params) {
		return nil, false, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(params, &envelope); err != nil {
		return nil, false, fmt.Errorf("params 不是合法的对象：%w", err)
	}
	raw, ok := envelope["marketing"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false, nil
	}
	var mp MarketingParams
	if err := json.Unmarshal(raw, &mp); err != nil {
		return nil, true, fmt.Errorf("params.marketing 不是合法的对象：%w", err)
	}
	for i, p := range mp.Picks {
		switch {
		case !p.Kind.Valid():
			return nil, true, fmt.Errorf("params.marketing.picks[%d].kind 非法：%q", i, p.Kind)
		case !ValidCode(p.Code):
			return nil, true, fmt.Errorf("params.marketing.picks[%d].code 格式非法：%q", i, p.Code)
		case p.Version < 1:
			return nil, true, fmt.Errorf("params.marketing.picks[%d].version 必须 ≥ 1", i)
		}
	}
	return MergePins(nil, mp.Picks), true, nil
}

// PinKey 是钉住的身份：kind + code（**不含** version）。
func PinKey(kind Kind, code string) string { return string(kind) + ":" + code }

// MergePins 把新选的合并进已钉住的：**同一 kind+code 只钉第一次的版本**。
//
// ⚠️ 会话里某个选项一旦钉住，运营之后再改它、Agent 重新规划时再次引用它，
// 都还是会话开始时那一版 —— 否则同一会话里先后两次生成会用到两段不同的片段，
// 而确认卡上写的是同一个名字。要换新版只能新开会话（dev/01 §12 #3）。
// 结果保持「已有在前、新增按出现顺序在后」，不改入参。
func MergePins(existing, incoming []Pick) []Pick {
	out := make([]Pick, 0, len(existing)+len(incoming))
	seen := make(map[string]bool, len(existing)+len(incoming))
	for _, list := range [][]Pick{existing, incoming} {
		for _, p := range list {
			k := PinKey(p.Kind, p.Code)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}
