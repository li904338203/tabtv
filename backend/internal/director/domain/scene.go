// Package domain 是 3D 导演台的场景结构与搭景规则（docs/agent/director，DRD-11）。
//
// ⚠️ 这一层**不碰数据库、不发网络**（同 workflow/domain）：场景长什么样、合不合法、
// 渲染指纹、引用了哪些媒体、复制项目时怎么改写节点上的 sceneId —— 全是纯计算。
// 画布域（存储）与 director/app（AI 搭景编排）都只调这里，不再各写一份「场景长什么样」。
//
// 坐标约定（DRD-4，前端 DRT-11 / 引擎 DRT-12 照这里）：
//   - 单位米，Y 轴向上（three.js 默认）；
//   - 角色以**脚底**为锚点，道具以**底面**为锚点：换体型、换姿势、换身高都不改变站位的含义；
//   - 旋转一律欧拉角（度，XYZ）；角色 rotation.y = 0 时**面朝 +Z**；机位 rotation 全 0 时**朝 −Z 看**；
//   - 机位存焦距 + 传感器长边（竖幅时长边是高），视角由它们推导，不直接存视角。
//
// ⚠️ 场景按 `map[string]any` 树处理，**不定义完整 struct**：用 struct 解析再序列化会丢掉
// 未知字段（GO-35，配置可以比代码新）。校验只读不改，入库的是请求里的原始字节。
// `version`（行上的乐观锁）与正文里的 `schemaVersion`（结构版本）是两件事（CLAUDE.md §5.4）。
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

// CurrentSchemaVersion 是当前场景正文的结构版本。
const CurrentSchemaVersion = 1

// 对象种类。
const (
	KindCharacter = "character"
	KindCrowd     = "crowd"
	KindProp      = "prop"
	KindCamera    = "camera"
)

// 对象来源：AI 重新搭景只替换 source=ai 且未锁定的对象（DRK-7）。
const (
	SourceUser = "user"
	SourceAI   = "ai"
)

// 默认场景里两个对象的固定 ID：`DefaultScene()` 的产出要与 fixture 逐字相等，ID 不能随机。
const (
	DefaultCameraID    = "obj_camera_1"
	DefaultCharacterID = "obj_character_1"
)

// Scene 是场景正文的只读视图（map 树）。
type Scene struct {
	tree map[string]any
}

// Object 是场景里的一个对象（只读视图）。
type Object struct {
	raw map[string]any
}

// ErrNotObject 表示正文不是 JSON 对象。
var ErrNotObject = errors.New("场景正文必须是 JSON 对象")

// ParseScene 把正文解析成只读视图。只检查「是不是 JSON 对象」；结构合法性由 Validate 判定。
func ParseScene(raw json.RawMessage) (Scene, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return Scene{}, fmt.Errorf("场景正文不是合法 JSON：%w", err)
	}
	tree, ok := v.(map[string]any)
	if !ok {
		return Scene{}, ErrNotObject
	}
	return Scene{tree: tree}, nil
}

// Tree 返回底层 map 树（给本包内的指纹、媒体提取等使用；调用方不要改它）。
func (s Scene) Tree() map[string]any { return s.tree }

// SchemaVersion 返回结构版本；缺失或不是整数时返回 0。
func (s Scene) SchemaVersion() int {
	f, ok := s.tree["schemaVersion"].(float64)
	if !ok || f != float64(int(f)) {
		return 0
	}
	return int(f)
}

// Objects 返回全部对象（保持正文里的顺序）；不是对象的项跳过。
func (s Scene) Objects() []Object {
	list, _ := s.tree["objects"].([]any)
	out := make([]Object, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, Object{raw: m})
		}
	}
	return out
}

// Cameras 返回全部机位。
func (s Scene) Cameras() []Object {
	var out []Object
	for _, o := range s.Objects() {
		if o.Kind() == KindCamera {
			out = append(out, o)
		}
	}
	return out
}

// OutputCameraID 返回输出机位 ID；没有时为空串。
func (s Scene) OutputCameraID() string {
	id, _ := s.tree["outputCameraId"].(string)
	return id
}

// ObjectByID 按 ID 取对象。
func (s Scene) ObjectByID(id string) (Object, bool) {
	for _, o := range s.Objects() {
		if o.ID() == id {
			return o, true
		}
	}
	return Object{}, false
}

// ID 返回对象 ID。
func (o Object) ID() string { v, _ := o.raw["id"].(string); return v }

// Kind 返回对象种类。
func (o Object) Kind() string { v, _ := o.raw["kind"].(string); return v }

// Name 返回对象名称。
func (o Object) Name() string { v, _ := o.raw["name"].(string); return v }

// Visible 返回是否显示；缺省为 true。
func (o Object) Visible() bool {
	v, ok := o.raw["visible"].(bool)
	return !ok || v
}

// Locked 返回是否锁定；缺省为 false。
func (o Object) Locked() bool { v, _ := o.raw["locked"].(bool); return v }

// Source 返回来源；缺省为 user。
func (o Object) Source() string {
	if v, ok := o.raw["source"].(string); ok && v != "" {
		return v
	}
	return SourceUser
}

// Raw 返回对象的 map（调用方不要改它）。
func (o Object) Raw() map[string]any { return o.raw }

// DefaultScene 是新建场景的正文（DR-F-12，对标「默认 1 机位 1 人物」）。
//
// 机位：16:9、35mm、传感器长边 36mm，中景对准角色。竖直视角 ≈ 32.3°，中景 coverage 0.6
// × 身高 1.8m = 画面高 1.08m → 距离 ≈ 1.87m，注视点 0.75H = 1.35m（dev/01 §4.4）。
// 角色在原点、面朝 +Z（朝向机位），机位在 +Z 处、rotation 全 0 即朝 −Z 看向角色。
//
// ⚠️ 两个默认对象记为 `source=ai`（可替换）：它们不是用户摆的。记成 user 的话，新节点上第一次 AI 搭景
// 动不了它们 —— 「三个人对峙」会搭出 4 个角色、2 个机位（DR-AC-27 要的是 3 个、1 个）。
// 用户一旦动过，前端就按 DR-F-105 改写为 user（directorTouch），从此原样保留。
func DefaultScene() json.RawMessage {
	scene := map[string]any{
		"schemaVersion": CurrentSchemaVersion,
		"stage":         defaultStage(),
		"view":          defaultView(),
		"background":    defaultBackground(),
		"ground":        defaultGround(),
		"lights":        defaultLights(),
		"objects": []any{
			map[string]any{
				"id": DefaultCameraID, "name": "机位 1", "kind": KindCamera,
				"transform": transform(vec3(0, 1.35, 1.87), vec3(0, 0, 0), vec3(1, 1, 1)),
				"visible":   true, "locked": false, "source": SourceAI,
				"camera": defaultCameraParams(),
			},
			map[string]any{
				"id": DefaultCharacterID, "name": "角色 1", "kind": KindCharacter,
				"transform": transform(vec3(0, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
				"visible":   true, "locked": false, "source": SourceAI,
				"character": map[string]any{
					"model": ModelAdultMale, "height": 1.8, "body": "standard", "posePreset": "stand_neutral",
				},
			},
		},
		"outputCameraId": DefaultCameraID,
		"timeline":       defaultTimeline(),
	}
	out, err := json.Marshal(scene)
	if err != nil {
		// 只有内置字面量，不可能失败；失败就是代码写错了。
		panic(fmt.Sprintf("默认场景序列化失败：%v", err))
	}
	return out
}

func vec3(x, y, z float64) map[string]any { return map[string]any{"x": x, "y": y, "z": z} }

func transform(pos, rot, scale map[string]any) map[string]any {
	return map[string]any{"position": pos, "rotation": rot, "scale": scale}
}

func defaultCameraParams() map[string]any {
	return map[string]any{
		"focalLengthMm": 35, "sensorWidthMm": 36, "aspect": "16:9", "near": 0.1, "far": 1000, "roll": 0,
	}
}

func defaultStage() map[string]any {
	return map[string]any{"scale": 1, "offset": vec3(0, 0, 0), "rotation": vec3(0, 0, 0)}
}

func defaultView() map[string]any {
	return map[string]any{
		"showLabels": true,
		"gridSnap":   map[string]any{"enabled": false, "step": 0.5},
		"angleSnap":  map[string]any{"enabled": false, "step": 15},
		"groundSnap": true,
	}
}

func defaultBackground() map[string]any {
	return map[string]any{"mode": "color", "color": "#060608"}
}

func defaultGround() map[string]any {
	return map[string]any{"visible": true, "opacity": 1, "height": 0}
}

func defaultLights() map[string]any {
	return map[string]any{
		"key":  map[string]any{"visible": true, "intensity": 1.2, "color": "#ffffff", "azimuth": 45, "elevation": 50},
		"fill": map[string]any{"visible": true, "intensity": 0.5, "color": "#ffffff", "azimuth": 225, "elevation": 20},
	}
}

func defaultTimeline() map[string]any {
	return map[string]any{"fps": TimelineFPS, "durationFrames": 150, "loop": false, "tracks": []any{}}
}
