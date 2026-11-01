package app

import (
	"encoding/json"
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 营销会话的规划条目（MKT-T-20）在 app 层要用到的 scene_input 读法。
// 规划工具（submit_plan）、批准换图（SubmitCard）与执行（generate，MKT-T-22）共用：
// 「这个会话有哪些商品图、角色、源视频、钉住了哪些版本」只有这一种读法。

// ParseSceneInput 解析 agent_session.scene_input。空 / `{}` 返回零值（通用会话）。
func ParseSceneInput(raw json.RawMessage) (SceneInput, error) {
	var si SceneInput
	if len(raw) == 0 {
		return si, nil
	}
	if err := json.Unmarshal(raw, &si); err != nil {
		return SceneInput{}, fmt.Errorf("会话营销输入损坏：%w", err)
	}
	return si, nil
}

// PlanInputs 是报价与执行用的输入事实（商品图有序 ID、角色数、有无源视频）。
func (si SceneInput) PlanInputs() domain.MarketingInputs {
	return domain.MarketingInputs{Products: append([]string(nil), si.Products...),
		Characters: len(si.Characters), SourceVideo: si.SourceVideo != nil}
}

// PinnedList 是会话已钉住的全部选项：建会话时的三个维度在前，规划时新钉的在后。
func (si SceneInput) PinnedList() []ScenePin {
	var out []ScenePin
	for _, x := range []struct {
		kind string
		p    *ScenePick
	}{{"creative", si.Picks.Creative}, {"hook", si.Picks.Hook}, {"style", si.Picks.Style}} {
		if x.p != nil && x.p.Code != "" {
			out = append(out, ScenePin{Kind: x.kind, Code: x.p.Code, Version: x.p.Version, Name: x.p.Name})
		}
	}
	for _, p := range si.Pins {
		out = append(out, ScenePin{Kind: p.Kind, Code: p.Code, Version: p.Version})
	}
	return out
}

// SessionPickCode 是建会话时某个维度选的 code（未选为 ""）。
func (si SceneInput) SessionPickCode(kind string) string {
	var p *ScenePick
	switch kind {
	case "creative":
		p = si.Picks.Creative
	case "hook":
		p = si.Picks.Hook
	case "style":
		p = si.Picks.Style
	}
	if p == nil {
		return ""
	}
	return p.Code
}

// PinSourcePlan 是规划时新钉的选项在 scene_input.pins 里的来源标记。
const PinSourcePlan = "plan"
