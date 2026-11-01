package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/platform/lenientjson"
)

// AI 搭景的草案：模型按输出契约（contract.go）写的 JSON，解析与白名单过滤在这里（dev/01 §4.3）。
//
// ⚠️ **容错层不是许可证**（CLAUDE.md §5.7）：救不回来就报 ErrDraftInvalid（DRT-19 映射为 42216），
// **绝不吞掉** —— 吞掉的结果是「AI 搭景成功」而场景什么都没变。
// ⚠️ **坐标永远由求解器给**（DRD-27）：模型自作主张写了坐标，一律忽略并写进丢弃说明。

// 草案操作。
const (
	OpAdd    = "add"
	OpUpdate = "update"
	OpRemove = "remove"
)

// 关系类型（与目录 relations 同一组码）。
const (
	RelLeftOf    = "left_of"
	RelRightOf   = "right_of"
	RelInFrontOf = "in_front_of"
	RelBehind    = "behind"
	RelNear      = "near"
	RelFacing    = "facing"
	RelCenter    = "center"
)

// FacingCamera 是 facing.target 的保留值：面向输出机位。
const FacingCamera = "camera"

// 草案里数值的允许范围（超出的那一项丢弃并说明，不夹值：夹出来的数不是模型的意思，也不是用户的）。
const (
	minRelationDistance = 0.1
	maxRelationDistance = 50.0
	minFocalMm          = 12.0
	maxFocalMm          = 200.0
	minPropSize         = 0.01
	maxPropSize         = 100.0
	minCrowdSpacing     = 0.5
	maxCrowdSpacing     = 5.0
	// maxDraftNotes / maxNoteChars 限制模型的 notes：它们原样给用户看，太长就是噪声。
	maxDraftNotes = 20
	maxNoteChars  = 200
	// snippetChars 是 ErrDraftInvalid 带的原文长度（dev/01 §4.3：前 300 字）。
	snippetChars = 300
)

// Draft 是过完白名单的搭景草案。
type Draft struct {
	Objects []DraftObject
	Camera  *DraftCamera
	// Notes 是模型自己写的说明（信息不够时用了哪些默认值）。
	Notes []string
}

// DraftObject 是对一个对象的操作。字段为空表示「草案没写」，由求解器取默认值或保持原样。
type DraftObject struct {
	// Op 为 add / update / remove；草案里没写时为空，由 Solve 按 Ref 推断（Ref 是已有对象 ID 就是修改）。
	Op string
	// Ref：新增时是名字（别的对象用它指代），修改 / 删除时是场景摘要里的对象 ID。
	Ref string
	// Index 是这一项在草案 objects 里的位置（从 1 起），只用于说明文字。
	Index int
	Kind  string
	// Name 是显示名称：新增时省略则用 Ref；修改时写了就改名。
	Name       string
	Model      string
	Body       string
	PosePreset string
	Facing     *DraftFacing
	Relation   *DraftRelation
	Crowd      *DraftCrowd
	Prop       *DraftProp
}

// DraftFacing 是朝向：Target 为 FacingCamera（面向输出机位）或对象的名字 / ID。
type DraftFacing struct {
	Target string
}

// DraftRelation 是位置关系。Distance 为 0 表示用默认距离。
type DraftRelation struct {
	Type     string
	Target   string
	Distance float64
}

// DraftCrowd 是群众阵列参数，为 0 的项用默认值。
type DraftCrowd struct {
	Rows    int
	Cols    int
	Spacing float64
}

// DraftProp 是道具参数。
type DraftProp struct {
	Shape string
	Size  *Size3
}

// Size3 是道具尺寸（米）。
type Size3 struct {
	X, Y, Z float64
}

// DraftCamera 是机位描述，空串 / 0 表示没写，由求解器取默认值并写进说明。
type DraftCamera struct {
	Targets       []string
	ShotSize      string
	Angle         string
	Side          string
	FocalLengthMm float64
}

// ErrDraftInvalid 表示模型输出救不回来，或过完白名单之后什么都不剩（DRT-19 映射为 42216）。
// Snippet 是原文（去掉首尾空白后）的前 300 个字，给运营判断是模型跑题了还是提示词写坏了。
type ErrDraftInvalid struct {
	Reason  string
	Snippet string
}

func (e *ErrDraftInvalid) Error() string {
	return e.Reason + "（原文开头：" + e.Snippet + "）"
}

// ParseDraft 解析模型输出：先严格解析，失败再走容错层；然后逐项过白名单。
// 返回的第二个值是丢弃说明（每条一句，给用户看）。
func ParseDraft(raw string) (Draft, []string, error) {
	snippet := headRunes(strings.TrimSpace(raw), snippetChars)
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		if err := lenientjson.Decode(raw, &v); err != nil {
			reason := "模型输出无法解析为 JSON"
			if !strings.ContainsAny(raw, "{[") {
				reason = "模型没有按要求输出 JSON，回的是一段文字"
			}
			return Draft{}, nil, &ErrDraftInvalid{Reason: reason, Snippet: snippet}
		}
	}
	root, ok := v.(map[string]any)
	if !ok {
		return Draft{}, nil, &ErrDraftInvalid{Reason: "模型输出不是 JSON 对象", Snippet: snippet}
	}

	p := draftParser{}
	d := Draft{Notes: p.notes(root["notes"])}
	d.Objects = p.objects(root["objects"])
	if c, present := root["camera"]; present && c != nil {
		d.Camera = p.camera(c)
	}
	if len(d.Objects) == 0 && d.Camera == nil {
		reason := "模型没有给出可用的搭景方案"
		if len(p.dropped) > 0 {
			reason += "：" + p.dropped[0]
		}
		return Draft{}, p.dropped, &ErrDraftInvalid{Reason: reason, Snippet: snippet}
	}
	return d, p.dropped, nil
}

type draftParser struct {
	dropped []string
}

func (p *draftParser) drop(format string, args ...any) {
	p.dropped = append(p.dropped, fmt.Sprintf(format, args...))
}

func (p *draftParser) notes(v any) []string {
	list, _ := v.([]any)
	if s, ok := v.(string); ok {
		// 宽容一种常见写法：notes 写成一句话而不是数组。
		list = []any{s}
	}
	var out []string
	for _, item := range list {
		s, ok := item.(string)
		s = strings.TrimSpace(s)
		if !ok || s == "" {
			continue
		}
		if len(out) == maxDraftNotes {
			break
		}
		out = append(out, headRunes(s, maxNoteChars))
	}
	return out
}

// coordinateKeys 是「模型自作主张给坐标」时常见的键：一律忽略并说明（DRD-27）。
var coordinateKeys = []string{"position", "transform", "rotation", "location", "coordinates", "coords", "pos", "x", "y", "z"}

func (p *draftParser) objects(v any) []DraftObject {
	if v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		p.drop("objects 不是数组，已忽略")
		return nil
	}
	var out []DraftObject
	for i, item := range list {
		n := i + 1
		if len(out) == MaxObjects {
			p.drop("草案对象超过 %d 个，第 %d 项起已忽略", MaxObjects, n)
			break
		}
		m, ok := item.(map[string]any)
		if !ok {
			p.drop("第 %d 项不是对象，已丢弃", n)
			continue
		}
		if o, ok := p.object(m, n); ok {
			out = append(out, o)
		}
	}
	return out
}

func (p *draftParser) object(m map[string]any, n int) (DraftObject, bool) {
	o := DraftObject{Index: n, Ref: strings.TrimSpace(str(m["ref"])), Name: strings.TrimSpace(str(m["name"]))}
	label := o.label()

	switch op := str(m["op"]); op {
	case OpAdd, OpUpdate, OpRemove:
		o.Op = op
	case "":
		// 没写 op：交给 Solve 按 Ref 推断（Ref 是已有对象 ID 就是修改，否则是新增）。
	default:
		p.drop("%s的 op「%s」不认识，已丢弃", label, op)
		return DraftObject{}, false
	}
	if o.Op != OpAdd && o.Ref == "" {
		if o.Op == "" {
			o.Op = OpAdd
		} else {
			p.drop("第 %d 项（%s）没有写 ref，不知道要改哪个对象，已丢弃", n, o.Op)
			return DraftObject{}, false
		}
	}
	if o.Op == OpRemove {
		return o, true
	}

	if k, present := m["kind"]; present {
		switch kind := str(k); kind {
		case KindCharacter, KindCrowd, KindProp:
			o.Kind = kind
		case KindCamera:
			p.drop("%s把机位写在了 objects 里，已丢弃：机位请写在 camera 里", label)
			return DraftObject{}, false
		default:
			p.drop("%s的 kind「%v」不认识，已丢弃", label, k)
			return DraftObject{}, false
		}
	}
	if keys := presentKeys(m, coordinateKeys); len(keys) > 0 {
		p.drop("%s带了坐标（%s），已忽略：位置由系统按位置关系求解", label, strings.Join(keys, "、"))
	}

	o.Model = p.code(m, "model", label, "素模", modelSet)
	o.Body = p.code(m, "body", label, "体型", bodySet)
	o.PosePreset = p.code(m, "posePreset", label, "姿势码", poseSet)
	o.Facing = p.facing(m["facing"], label)
	o.Relation = p.relation(m["relation"], label)
	o.Crowd = p.crowd(m, label)
	o.Prop = p.prop(m, label)
	if o.Op == OpAdd && o.Kind == "" {
		// 没写 kind：按字段推断，猜不出来就当角色。
		switch {
		case o.Prop != nil:
			o.Kind = KindProp
		case o.Crowd != nil:
			o.Kind = KindCrowd
		default:
			o.Kind = KindCharacter
		}
		p.drop("%s没有写 kind，按%s处理", label, kindLabel(o.Kind))
	}
	return o, true
}

func (o DraftObject) label() string {
	if o.Ref != "" {
		return "「" + o.Ref + "」"
	}
	return "第 " + strconv.Itoa(o.Index) + " 项"
}

// code 取一个白名单码：缺省返回空；白名单外的丢弃并说明。
func (p *draftParser) code(m map[string]any, key, label, what string, allowed map[string]bool) string {
	v, present := m[key]
	if !present || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok || !allowed[s] {
		p.drop("%s的%s「%v」不在目录里，已忽略", label, what, v)
		return ""
	}
	return s
}

func (p *draftParser) facing(v any, label string) *DraftFacing {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		// 宽容一种常见写法：facing 直接写目标。
		if t := strings.TrimSpace(x); t != "" {
			return &DraftFacing{Target: t}
		}
	case map[string]any:
		if t := strings.TrimSpace(str(x["target"])); t != "" {
			return &DraftFacing{Target: t}
		}
	}
	p.drop("%s的 facing 写法不对，已忽略", label)
	return nil
}

func (p *draftParser) relation(v any, label string) *DraftRelation {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		p.drop("%s的 relation 不是对象，已忽略", label)
		return nil
	}
	r := &DraftRelation{Type: str(m["type"]), Target: strings.TrimSpace(str(m["target"]))}
	if !relationSet[r.Type] {
		p.drop("%s的关系类型「%v」不认识，已忽略", label, m["type"])
		return nil
	}
	if r.Type != RelCenter && r.Target == "" {
		p.drop("%s的关系 %s 没有写 target，已忽略", label, r.Type)
		return nil
	}
	if d, present := m["distance"]; present && d != nil {
		f, ok := d.(float64)
		if ok && f >= minRelationDistance && f <= maxRelationDistance {
			r.Distance = f
		} else {
			p.drop("%s的关系距离「%v」超出范围（%s~%s 米），按默认距离", label, d, fmtNum(minRelationDistance), fmtNum(maxRelationDistance))
		}
	}
	return r
}

// crowd 取群众参数：平铺在对象上（rows / cols / spacing），也认嵌在 crowd 里的写法。
func (p *draftParser) crowd(m map[string]any, label string) *DraftCrowd {
	src := m
	if nested, ok := m["crowd"].(map[string]any); ok {
		src = nested
	}
	_, hasRows := src["rows"]
	_, hasCols := src["cols"]
	_, hasSpacing := src["spacing"]
	if !hasRows && !hasCols && !hasSpacing {
		return nil
	}
	c := &DraftCrowd{}
	rows, rowsOK := intIn(src["rows"], 1, MaxCrowdRows)
	cols, colsOK := intIn(src["cols"], 1, MaxCrowdCols)
	switch {
	case (hasRows && !rowsOK) || (hasCols && !colsOK):
		p.drop("%s的群众行列数「%v×%v」超出范围（每边 1~%d），按默认行列", label, src["rows"], src["cols"], MaxCrowdRows)
	case rowsOK && colsOK && rows*cols > MaxCrowdSize:
		p.drop("%s的群众人数 %d 超过上限 %d，按默认行列", label, rows*cols, MaxCrowdSize)
	default:
		c.Rows, c.Cols = rows, cols
	}
	if hasSpacing {
		if f, ok := src["spacing"].(float64); ok && f >= minCrowdSpacing && f <= maxCrowdSpacing {
			c.Spacing = f
		} else {
			p.drop("%s的群众间距「%v」超出范围（%s~%s 米），按默认间距", label, src["spacing"], fmtNum(minCrowdSpacing), fmtNum(maxCrowdSpacing))
		}
	}
	return c
}

// prop 取道具参数：shape / size 平铺在对象上，也认嵌在 prop 里的写法。
func (p *draftParser) prop(m map[string]any, label string) *DraftProp {
	src := m
	if nested, ok := m["prop"].(map[string]any); ok {
		src = nested
	}
	_, hasShape := src["shape"]
	_, hasSize := src["size"]
	if !hasShape && !hasSize {
		return nil
	}
	pr := &DraftProp{Shape: p.code(src, "shape", label, "道具形状", propShapeSet)}
	if hasSize {
		if s, ok := size3(src["size"]); ok {
			pr.Size = &s
		} else {
			p.drop("%s的道具尺寸「%v」不合法（x / y / z 都要在 %s~%s 米），按默认尺寸", label, src["size"], fmtNum(minPropSize), fmtNum(maxPropSize))
		}
	}
	return pr
}

func (p *draftParser) camera(v any) *DraftCamera {
	m, ok := v.(map[string]any)
	if !ok {
		p.drop("camera 不是对象，已忽略")
		return nil
	}
	c := &DraftCamera{}
	switch t := m["target"].(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			c.Targets = []string{s}
		}
	case []any:
		for _, item := range t {
			if s := strings.TrimSpace(str(item)); s != "" {
				c.Targets = append(c.Targets, s)
			}
		}
	}
	c.ShotSize = p.code(m, "shotSize", "camera ", "景别", shotSizeSet)
	c.Angle = p.code(m, "angle", "camera ", "角度", angleSet)
	c.Side = p.code(m, "side", "camera ", "方位", sideSet)
	if f, present := m["focalLengthMm"]; present && f != nil {
		if x, ok := f.(float64); ok && x >= minFocalMm && x <= maxFocalMm {
			c.FocalLengthMm = x
		} else {
			p.drop("camera 的焦距「%v」超出范围（%s~%smm），已忽略", f, fmtNum(minFocalMm), fmtNum(maxFocalMm))
		}
	}
	if keys := presentKeys(m, append([]string{"distance", "height"}, coordinateKeys...)); len(keys) > 0 {
		p.drop("camera 带了坐标（%s），已忽略：机位位置由系统按景别与角度求解", strings.Join(keys, "、"))
	}
	return c
}

// 白名单查找表（码取自目录常量，不手抄）。
var (
	shotSizeSet = func() map[string]bool {
		m := map[string]bool{}
		for _, s := range shotSizes {
			m[s.Code] = true
		}
		return m
	}()
	angleSet    = labeledSet(angles)
	sideSet     = labeledSet(sides)
	relationSet = stringSet(relations)
)

func str(v any) string {
	s, _ := v.(string)
	return s
}

func presentKeys(m map[string]any, keys []string) []string {
	var out []string
	for _, k := range keys {
		if _, ok := m[k]; ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func intIn(v any, lo, hi int) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f < float64(lo) || f > float64(hi) {
		return 0, false
	}
	return int(f), true
}

func size3(v any) (Size3, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return Size3{}, false
	}
	var out [3]float64
	for i, axis := range []string{"x", "y", "z"} {
		f, ok := m[axis].(float64)
		if !ok || f < minPropSize || f > maxPropSize {
			return Size3{}, false
		}
		out[i] = f
	}
	return Size3{out[0], out[1], out[2]}, true
}

// headRunes 取前 n 个字（按字符，不切坏多字节字符）。
func headRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func kindLabel(kind string) string {
	switch kind {
	case KindCharacter:
		return "角色"
	case KindCrowd:
		return "群众"
	case KindProp:
		return "道具"
	case KindCamera:
		return "机位"
	}
	return kind
}
