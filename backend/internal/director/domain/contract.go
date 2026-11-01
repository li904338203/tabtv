package domain

import (
	"fmt"
	"strings"
)

// 输出格式契约（DRD-25）：代码固定追加在系统提示词（`director.scene_build.system`）之后。
//
// ⚠️ **格式写在代码里，创作要求放进 prompt_template**：改格式等于改解析器（draft.go），不能交给运营后台；
// 让模型自由发挥格式的话，输出一长就写坏 JSON（CD-37 同类）。
// ⚠️ 码表全部取自目录常量（catalog.go），不手抄：手抄的码表与目录早晚不一致，表现是
// 「契约里让模型用的码，解析时被当成白名单外的码丢掉」，而两边各自看都没错（`TestOutputContract_码表与目录一致` 钉住）。
// ⚠️ 契约里不要出现「分镜」二字：假上游看到它就回一份分镜 JSON（dev/01 §5），E2E 会每次都拿到 42216。

// ContractHeading 是契约的固定标题：假上游认这个标题回搭景草案（dev/01 §5，DRT-30）。
const ContractHeading = "## 输出格式（导演台搭景）"

// 码的中文说明里目录没有的那部分（目录只有码与中文名）。键集合必须与目录一致，测试钉住。
var (
	propShapeNames = map[string]string{
		PropBox: "方块", PropCylinder: "圆柱", PropSphere: "球", PropPlane: "平面（薄板）",
	}
	// shotSizeHints 是每档景别的一句画面说明：避免模型把英文 close-up 理解成近景（dev/01 §4.3）。
	shotSizeHints = map[string]string{
		"extreme_long":     "人物很小，画面主要交代环境",
		"long":             "人物全身入画且较小，环境占大部分",
		"full":             "人物从头到脚完整入画，上下略有留白",
		"medium_long":      "膝盖以上",
		"medium":           "腰部以上",
		"medium_close":     "胸部以上",
		"close_up":         "肩部以上，以脸为主",
		"extreme_close_up": "脸的局部，例如眼睛",
	}
	relationHints = map[string]string{
		RelLeftOf:    "在目标左边（观看者的左手边）",
		RelRightOf:   "在目标右边（观看者的右手边）",
		RelInFrontOf: "在目标前面（离机位更近）",
		RelBehind:    "在目标后面（离机位更远）",
		RelNear:      "在目标旁边",
		RelFacing:    "与目标面对面（站在目标的左边或右边，并转身面向目标）",
		RelCenter:    "放在场景中心（不写 target）",
	}
	sideHints = map[string]string{
		"front":       "从角色群的正前方拍",
		"front_left":  "从正面观众的左手边 45° 拍",
		"front_right": "从正面观众的右手边 45° 拍",
		"left":        "从正面观众的左手边 90° 拍（侧面）",
		"right":       "从正面观众的右手边 90° 拍（侧面）",
		"back":        "从角色群的背后拍",
	}
)

// OutputContract 返回输出格式契约全文。内容只由目录常量决定，同一版本的代码每次返回同一段文字。
func OutputContract() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("%s\n\n", ContractHeading)
	w("只输出一个 JSON 对象：不要解释，不要反问，不要用代码块包起来。信息不够时按常理或默认值安排，并把你用了哪些默认值写进 notes，每条一句。\n\n")
	w("示例：\n%s\n\n", contractExample)

	w("### objects：要新增、修改、删除的对象（不含机位）\n\n")
	w("- op：add 新增；update 修改；remove 删除。\n")
	w("- ref：add 时写这个对象的名字（不超过 %d 个字，同一方案里不重复，别的对象用它指代）；update / remove 时写场景摘要里给出的对象 id。\n", MaxNameChars)
	w("- update / remove 只能指向场景摘要里标为「可替换」的对象；标为「用户摆的」或「已锁定」的对象只能保留，可以拿它们当位置关系的目标。\n")
	w("- 新方案里不需要的「可替换」对象用 remove 删掉；没写到的对象保持不变。\n")
	w("- kind（add 时必填）：character 单个角色、crowd 群众阵列、prop 几何道具。机位不要写进 objects，写在 camera 里。\n")
	w("- name（可省略）：update 时写了就改名。\n")
	w("- 角色：model 素模、body 体型、posePreset 姿势。群众：model、posePreset、rows 行数（1~%d）、cols 列数（1~%d，rows×cols 不超过 %d）、spacing 间距（%s~%s 米）。\n",
		MaxCrowdRows, MaxCrowdCols, MaxCrowdSize, fmtNum(minCrowdSpacing), fmtNum(maxCrowdSpacing))
	w("- 道具：shape 形状；size 尺寸 {\"x\": 宽, \"y\": 高, \"z\": 深}，单位米，每项 %s~%s。\n", fmtNum(minPropSize), fmtNum(maxPropSize))
	w("- facing：朝向。{\"target\": \"camera\"} 面向机位；{\"target\": \"名字或 id\"} 面向那个对象。省略时面向角色群的正面。\n")
	w("- relation：位置关系 {\"type\": 关系, \"target\": \"名字或 id\", \"distance\": 米}。distance 可省略（near %s 米，left_of / right_of / facing %s 米，in_front_of / behind %s 米），写的话在 %s~%s 米之间。\n",
		fmtNum(defaultNearDistance), fmtNum(defaultSideDistance), fmtNum(defaultDepthDistance), fmtNum(minRelationDistance), fmtNum(maxRelationDistance))
	w("- **不要输出坐标或旋转**（position、rotation、x / y / z 之类）：位置由系统按 relation 求解，角色自动贴地、互不重叠。\n\n")

	w("### camera：输出机位怎么拍（可省略，省略时沿用当前输出机位）\n\n")
	w("- target：要拍到的对象（名字或 id 的数组），省略时拍全部角色。\n")
	w("- shotSize 景别、angle 角度、side 方位（以角色群的正面为准），取值见下表；focalLengthMm 焦距（%s~%s，可省略）。\n", fmtNum(minFocalMm), fmtNum(maxFocalMm))
	w("- **左右、前后都按观看者算**：从输出机位看过去的左右，不是画面里人物自己的左右；in_front_of 离机位更近，behind 离机位更远。\n\n")

	w("### 可用的码（只能用下面这些，写别的会被丢弃）\n\n")
	w("#### 素模 model\n\n")
	for _, m := range models {
		w("- `%s` %s（身高 %s 米）\n", m.Code, m.Name, fmtNum(m.HeightM))
	}
	w("\n#### 体型 body\n\n")
	for _, x := range bodies {
		w("- `%s` %s\n", x.Code, x.Name)
	}
	w("\n#### 姿势 posePreset\n\n")
	for _, cat := range poseCategories {
		w("%s：\n", cat.Name)
		for _, p := range poses {
			if p.Category == cat.Code {
				w("- `%s` %s：%s\n", p.Code, p.Name, p.Description)
			}
		}
	}
	w("\n#### 道具形状 shape\n\n")
	for _, code := range propShapes {
		w("- `%s` %s\n", code, propShapeNames[code])
	}
	w("\n#### 位置关系 relation.type\n\n")
	for _, code := range relations {
		w("- `%s` %s\n", code, relationHints[code])
	}
	w("\n#### 景别 shotSize\n\n")
	for _, s := range shotSizes {
		w("- `%s` %s：%s\n", s.Code, s.Name, shotSizeHints[s.Code])
	}
	w("\n#### 角度 angle\n\n")
	for _, a := range angles {
		w("- `%s` %s\n", a.Code, a.Name)
	}
	w("\n#### 方位 side\n\n")
	for _, x := range sides {
		w("- `%s` %s：%s\n", x.Code, x.Name, sideHints[x.Code])
	}
	return b.String()
}

// contractExample 是契约里的示例（dev/01 §4.3）。它必须能被 ParseDraft 解析且一项都不丢（测试钉住）：
// 示例里写了目录外的码，等于教模型写会被丢弃的东西。
const contractExample = `{
  "objects": [
    {"op": "add", "ref": "林澈", "kind": "character", "model": "adult_male",
     "posePreset": "stand_arms_crossed", "facing": {"target": "camera"},
     "relation": {"type": "left_of", "target": "苏然", "distance": 1.2}},
    {"op": "add", "ref": "苏然", "kind": "character", "model": "adult_female", "posePreset": "point_forward"},
    {"op": "update", "ref": "obj_8f2c", "posePreset": "thinking"},
    {"op": "remove", "ref": "obj_77aa"}
  ],
  "camera": {"target": ["林澈", "苏然"], "shotSize": "medium", "angle": "low",
             "side": "front", "focalLengthMm": 35},
  "notes": ["未指定时间与天气，按白天户外"]
}`
