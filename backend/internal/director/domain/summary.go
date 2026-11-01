package domain

import (
	"fmt"
	"strings"
)

// 给模型看的场景摘要（dev/01 §4.3）：拼进 user 消息，不进系统提示词（同 CLAUDE.md §5.7「兜底只写指令，
// 不内联上下文」）。模型要靠它知道：场景里已经有谁、哪些能改（可替换）、哪些只能保留、现在从哪个机位看。

// 摘要的条数上限：对象太多时截断并注明，免得 user 消息被一个 200 个对象的场景撑满。
const (
	summaryMaxObjects = 60
	summaryMaxCameras = 20
)

// SceneSummary 列出对象（id、名称、种类、素模 / 形状、姿势码、可替换 / 用户摆的 / 已锁定）与机位（含输出机位）。
func SceneSummary(scene Scene) string {
	var b strings.Builder
	var objs, cams []Object
	for _, o := range scene.Objects() {
		if o.Kind() == KindCamera {
			cams = append(cams, o)
		} else {
			objs = append(objs, o)
		}
	}

	b.WriteString("### 当前场景\n\n")
	fmt.Fprintf(&b, "地面高度 %s 米。\n\n", fmtNum(groundHeight(scene)))
	if len(objs) == 0 {
		b.WriteString("场景里还没有角色、群众或道具。\n")
	} else {
		fmt.Fprintf(&b, "对象（共 %d 个）：\n", len(objs))
		for i, o := range objs {
			if i == summaryMaxObjects {
				fmt.Fprintf(&b, "- 其余 %d 个略\n", len(objs)-summaryMaxObjects)
				break
			}
			fmt.Fprintf(&b, "- %s\n", objectLine(o))
		}
	}
	b.WriteString("\n")
	out := scene.OutputCameraID()
	if len(cams) == 0 {
		b.WriteString("场景里还没有机位。\n")
		return b.String()
	}
	fmt.Fprintf(&b, "机位（共 %d 个）：\n", len(cams))
	for i, c := range cams {
		if i == summaryMaxCameras {
			fmt.Fprintf(&b, "- 其余 %d 个略\n", len(cams)-summaryMaxCameras)
			break
		}
		line := cameraLine(c)
		if c.ID() == out {
			line += "｜输出机位"
		}
		fmt.Fprintf(&b, "- %s\n", line)
	}
	return b.String()
}

func objectLine(o Object) string {
	parts := []string{o.ID(), o.Name(), kindLabel(o.Kind())}
	raw := o.Raw()
	switch o.Kind() {
	case KindCharacter:
		ch, _ := raw["character"].(map[string]any)
		parts = append(parts, modelName(str(ch["model"])), poseLabel(str(ch["posePreset"])))
	case KindCrowd:
		cr, _ := raw["crowd"].(map[string]any)
		rows, cols := intOf(cr["rows"]), intOf(cr["cols"])
		parts = append(parts, fmt.Sprintf("%d×%d", rows, cols), modelName(str(cr["model"])), poseLabel(str(cr["posePreset"])))
	case KindProp:
		pr, _ := raw["prop"].(map[string]any)
		parts = append(parts, propShapeLabel(str(pr["shape"])))
	}
	parts = append(parts, statusLabel(o))
	if !o.Visible() {
		parts = append(parts, "隐藏")
	}
	return strings.Join(parts, "｜")
}

func cameraLine(o Object) string {
	cam, _ := o.Raw()["camera"].(map[string]any)
	focal := numOr(cam["focalLengthMm"], defaultFocalMm)
	aspect, _ := cam["aspect"].(string)
	parts := []string{o.ID(), o.Name(), "机位", fmtNum(focal) + "mm"}
	if aspect != "" {
		parts = append(parts, aspect)
	}
	return strings.Join(append(parts, statusLabel(o)), "｜")
}

// statusLabel 是对象能不能被 AI 改：已锁定优先于来源（锁定的 AI 对象也不能改）。
func statusLabel(o Object) string {
	switch {
	case o.Locked():
		return "已锁定"
	case Replaceable(o):
		return "可替换"
	}
	return "用户摆的"
}

func modelName(code string) string {
	if m, ok := ModelOf(code); ok {
		return m.Name
	}
	if code == "" {
		return "未知素模"
	}
	return code
}

// poseLabel 写「姿势 码（中文名）」；前端比后端新时可能有目录外的码，照样列出码。
func poseLabel(code string) string {
	if code == "" {
		return "姿势未设置"
	}
	if name := poseName(code); name != code {
		return "姿势 " + code + "（" + name + "）"
	}
	return "姿势 " + code
}

func propShapeLabel(code string) string {
	if name, ok := propShapeNames[code]; ok {
		return name
	}
	return code
}
