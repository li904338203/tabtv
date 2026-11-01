package domain

import "encoding/json"

// 重试时把**解析后的输入快照**还原成**声明形态**。
//
// ⚠️ 两种形态不是一回事，而它们长得很像 —— 这正是它出错的原因：
//
//	声明形态（客户端给的）：`generationId` / `referenceId` 是**字符串**
//	                        （ID 对外一律 string，防 JS 精度丢失）
//	快照形态（服务端存的）：同名字段是 **int64**，还多出 mediaId / mediaKind
//	                        / width / height 这些解析结果
//
// 直接把快照当声明发回受理入口的话，`json.Unmarshal` 在第一个数字 ID 上就
// 失败，整条重试报「inputs 不是合法的输入数组」—— 而那句话指向"客户端传错
// 了参数"，可这次请求里**根本没有客户端传的 inputs**。
//
// ⚠️ 这个缺陷对**任何带输入的任务**都成立（图生图、首尾帧、参考图…），
// 只是没有输入的纯文生任务恰好绕过了它，所以一直没暴露（P13-03 的后台
// 受控重试第一次踩到）。
//
// ⚠️ **只还原"用户当初声明了什么"**，不把解析结果带回去：
// 重试要重新解析一遍（归属可能变了、产物可能被删了），带回去的话
// 一个已经失效的 mediaId 会被当成仍然有效。

// declaredInput 是受理入口认的形态。字段与 `canvas/app.ResolveForAccept`
// 里那个匿名结构一一对应 —— 改那边要同时改这里，所以两边都留了指认注释。
type declaredInput struct {
	Kind         string `json:"kind"`
	Order        int    `json:"order"`
	SourceNodeID string `json:"sourceNodeId,omitempty"`
	Text         string `json:"text,omitempty"`
	GenerationID string `json:"generationId,omitempty"`
	ReferenceID  string `json:"referenceId,omitempty"`
	MaterialCode string `json:"materialCode,omitempty"`
	MaterialType string `json:"materialType,omitempty"`
	VoiceCode    string `json:"voiceCode,omitempty"`
	Role         string `json:"role,omitempty"`
	// RangeMs 只用素材的哪一段（docs/agent/smartcut SCD-34）。⚠️ 重试不带上它的话，重拍出来的是整段素材，
	// 而计价也会退回客户端报的时长。
	RangeMs []int `json:"rangeMs,omitempty"`
}

// snapshotInput 是 `generation.inputs_snapshot` 里的形态。
type snapshotInput struct {
	Kind         string `json:"kind"`
	Order        int    `json:"order"`
	SourceNodeID string `json:"sourceNodeId"`
	Text         string `json:"text"`
	GenerationID int64  `json:"generationId"`
	ReferenceID  int64  `json:"referenceId"`
	MaterialCode string `json:"materialCode"`
	MaterialType string `json:"materialType"`
	VoiceCode    string `json:"voiceCode"`
	Role         string `json:"role"`
	RangeMs      []int  `json:"rangeMs"`
}

// RedeclareInputs 把输入快照还原成声明形态，供重试重新走一遍受理。
//
// ⚠️ 解不开时回 `[]` 而不是报错：快照是历史数据，格式可能比代码旧。
// 让整条重试因为一段解不开的历史 JSON 而失败，比"按没有输入重试一次"
// 更糟 —— 后者至少给出一个能看懂的上游错误，前者只给一句
// 「inputs 不是合法的输入数组」，而用户压根没传 inputs。
func RedeclareInputs(snapshot json.RawMessage) json.RawMessage {
	if len(snapshot) == 0 {
		return json.RawMessage("[]")
	}
	var raw []snapshotInput
	if err := json.Unmarshal(snapshot, &raw); err != nil {
		return json.RawMessage("[]")
	}
	out := make([]declaredInput, 0, len(raw))
	for _, s := range raw {
		out = append(out, declaredInput{
			Kind: s.Kind, Order: s.Order, SourceNodeID: s.SourceNodeID,
			Text: s.Text,
			// ⚠️ 数字 → 字符串，且 **0 要变成空串**：写成 "0" 的话受理侧会
			// 拿着一个不存在的 ID 去查，报"引用的产出不存在"——
			// 而真相是这条输入本来就没有产出引用。
			GenerationID: idString(s.GenerationID),
			ReferenceID:  idString(s.ReferenceID),
			MaterialCode: s.MaterialCode, MaterialType: s.MaterialType,
			VoiceCode: s.VoiceCode, Role: s.Role, RangeMs: s.RangeMs,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage("[]")
	}
	return b
}

func idString(v int64) string {
	if v == 0 {
		return ""
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
