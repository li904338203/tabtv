package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
)

// RenderFingerprint 算场景的渲染指纹（DRD-13）：只覆盖「会出现在画面里的内容」。
//
// 截图时把当时的指纹记在导演台媒体记录上，过期 = 记录上的指纹 ≠ 场景当前指纹（DR-F-82）。
// 这是**计算式**的（同 CD-36 的提示词 ready / stale）：标记式要每个改动点都记得标「过期」，
// 漏一处没有编译错误。
//
// ⚠️ **指纹只在 Go 算**，前端不算第二份：两边浮点格式差一位，所有截图就永远显示「过期」（DRR-14）。
//
// 计入（子树**整棵**进指纹，含其中的未知字段：将来加了影响画面的字段，不会漏掉）：
//   - stage、ground、lights、timeline、splat；
//   - background，但剔除 `panorama.sourceNodeId`（它只说明全景取自哪个上游节点，不影响画面）；
//   - objects 中的 kind、transform 与 character / crowd / prop / camera 子结构，按 id 排序。
//     **机位不论 visible 都计入**：机位的「隐藏」只是视口里不画它的模型，机位动了，它自己的截图就过期了。
//     其余对象只计 visible = true 的（隐藏的对象不会出现在截图里）。
//
// 不计入：名称、`view` 整段（标签、吸附）、`locked`、`source`、`outputCameraId`
// （换输出机位不改变任何一张已有截图的画面），以及对象层的其他字段。
//
// 规范化：键排序；数字四舍五入到 1e-6 后用最短表示（-0 记作 0）；sha256 取前 16 字节，转成 32 位 hex。
func RenderFingerprint(raw json.RawMessage) (string, error) {
	s, err := ParseScene(raw)
	if err != nil {
		return "", err
	}
	t := s.Tree()
	view := map[string]any{
		"stage":    t["stage"],
		"ground":   t["ground"],
		"lights":   t["lights"],
		"timeline": t["timeline"],
		"splat":    t["splat"],
	}
	if bg, ok := t["background"].(map[string]any); ok {
		view["background"] = backgroundForFingerprint(bg)
	} else {
		view["background"] = t["background"]
	}

	objs := s.Objects()
	sort.SliceStable(objs, func(i, j int) bool { return objs[i].ID() < objs[j].ID() })
	list := make([]any, 0, len(objs))
	for _, o := range objs {
		if o.Kind() != KindCamera && !o.Visible() {
			continue
		}
		entry := map[string]any{"kind": o.raw["kind"], "transform": o.raw["transform"]}
		for _, key := range []string{"character", "crowd", "prop", "camera"} {
			if v, present := o.raw[key]; present {
				entry[key] = v
			}
		}
		list = append(list, entry)
	}
	view["objects"] = list

	var buf bytes.Buffer
	if err := writeCanonical(&buf, view); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:16]), nil
}

func backgroundForFingerprint(bg map[string]any) map[string]any {
	out := make(map[string]any, len(bg))
	for k, v := range bg {
		out[k] = v
	}
	if p, ok := bg["panorama"].(map[string]any); ok {
		pc := make(map[string]any, len(p))
		for k, v := range p {
			if k != "sourceNodeId" {
				pc[k] = v
			}
		}
		out["panorama"] = pc
	}
	return out
}

// writeCanonical 写规范化 JSON：键排序、数字规范化。只处理 json.Unmarshal 产出的类型。
func writeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64:
		buf.WriteString(canonicalNumber(x))
	case string:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	case []any:
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		buf.WriteByte('{')
		for i, k := range sortedKeys(x) {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		// 内部构造的 map（如 backgroundForFingerprint）也只含上面这些类型；走到这里是代码写错了。
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		var back any
		if err := json.Unmarshal(b, &back); err != nil {
			return err
		}
		return writeCanonical(buf, back)
	}
	return nil
}

// canonicalNumber 把数字规范化：四舍五入到 1e-6，再用最短表示。
//
// ⚠️ 前端以 JS 浮点写出的数字，经 JSON 传到 Go 再解析，末位可能多出噪声（1.0000000001）。
// 不规范化的话，一次无关紧要的保存就会让所有截图「过期」（DRR-14）。
func canonicalNumber(f float64) string {
	r := math.Round(f*1e6) / 1e6
	if r == 0 {
		r = 0 // 把 -0 归成 0
	}
	return strconv.FormatFloat(r, 'f', -1, 64)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
