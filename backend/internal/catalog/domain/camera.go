package domain

import (
	"encoding/json"
	"strings"
)

// 视频节点的「运镜」：从运镜广场（camera_move 目录）挑一个镜头运动。
//
// ⚠️ 节点 params 里只存**选了哪一个**（`params.camera = {"code":"push_in","name":"镜头前推"}`），
// 提示词由服务端按目录里这一项的 prompt 拼进去（与风格、特效、图片预设同一条「词在服务端拼」的规则）——
// 前端拼的话词会落进 canvasData，运营改一句说法，历史节点永远停在旧词。

// CameraSelection 从节点参数里取出选中的运镜（code 与显示名）；没选时 code 为空。
//
// 兼容两种形状：对象 `{code,name}`（运镜广场写的）与裸字符串（只有 code）。
// ⚠️ 此前短暂用过「镜头运动 / 景别 / 机位」三组选项的对象形状（`{movement,shot,angle}`），
// 那种对象里没有 code，按没选处理 —— 不拦生成。
func CameraSelection(params json.RawMessage) (code, name string) {
	if len(params) == 0 {
		return "", ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(params, &m) != nil {
		return "", ""
	}
	raw := m["camera"]
	if len(raw) == 0 {
		return "", ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text), ""
	}
	var picked struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &picked) != nil {
		return "", ""
	}
	return strings.TrimSpace(picked.Code), strings.TrimSpace(picked.Name)
}
