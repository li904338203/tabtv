package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"unicode/utf8"
)

// 时间轴口径（DR-PARAM-10，设计建议值）：固定 30fps，总时长 1~15 秒。
const (
	TimelineFPS       = 30
	MinDurationFrames = 30
	MaxDurationFrames = 450
)

// ValidationError 是一条校验失败：Path 指到具体字段，前端（DRT-11）报同一个 Path。
//
// Path 的格式是定死的：点号分段、数组用方括号下标，例如 `objects[3].camera.aspect`、
// `timeline.tracks[0].keyframes[2].frame`、`background.panorama.yaw`。
type ValidationError struct {
	Path   string
	Reason string
}

func (e *ValidationError) Error() string { return e.Path + "：" + e.Reason }

// ErrSceneTooLarge 表示正文超过 MaxSceneBytes；调用方映射为 41301（PAYLOAD_TOO_LARGE）。
var ErrSceneTooLarge = errors.New("场景正文超过 512 KiB 上限")

var colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Validate 校验场景正文的形状与上限（dev/02 §2.4）。
//
// ⚠️ 只拦「结构性损坏」与上限，**不顺手修复**，也不改写正文：入库的是请求里的原始字节，
// 未知字段原样保留（GO-35）。各段（stage / view / …）缺省时放行，前端补默认值；
// 出现了就逐项校验。只有 `schemaVersion` 必填。
//
// ⚠️ 姿势码**未知时放行**：前端的姿势资源可能比后端新（dev/01 §4.1）。素模码则必须认识 ——
// 素模是随代码发布的资源，认不出来的模型前端也画不出来。
func Validate(raw json.RawMessage) error {
	if len(raw) > MaxSceneBytes {
		return ErrSceneTooLarge
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return &ValidationError{Path: "$", Reason: "不是合法 JSON：" + err.Error()}
	}
	root, ok := v.(map[string]any)
	if !ok {
		return &ValidationError{Path: "$", Reason: "场景正文必须是 JSON 对象"}
	}
	c := checker{}
	c.root(root)
	if c.err != nil {
		return c.err
	}
	return nil
}

// checker 按固定顺序遍历，遇到第一处错误就停：同一份坏数据每次报同一个 Path。
type checker struct {
	err *ValidationError
}

func (c *checker) fail(path, format string, args ...any) {
	if c.err == nil {
		c.err = &ValidationError{Path: path, Reason: fmt.Sprintf(format, args...)}
	}
}

func (c *checker) ok() bool { return c.err == nil }

func join(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func index(base string, i int) string { return base + "[" + strconv.Itoa(i) + "]" }

func (c *checker) root(m map[string]any) {
	sv, present := m["schemaVersion"]
	if !present {
		c.fail("schemaVersion", "缺少结构版本")
		return
	}
	n, isNum := sv.(float64)
	if !isNum || n != math.Trunc(n) {
		c.fail("schemaVersion", "结构版本必须是整数")
		return
	}
	if int(n) != CurrentSchemaVersion {
		c.fail("schemaVersion", "不支持的结构版本 %d（当前为 %d）", int(n), CurrentSchemaVersion)
		return
	}

	if s, ok := c.optObject(m, "stage", ""); ok {
		c.number(s, "scale", "stage", 0.1, 10, false)
		c.optVec3(s, "offset", "stage", MaxCoord)
		c.optVec3(s, "rotation", "stage", maxRotation)
	}
	if v, ok := c.optObject(m, "view", ""); ok {
		c.optBool(v, "showLabels", "view")
		if g, ok := c.optObject(v, "gridSnap", "view"); ok {
			c.optBool(g, "enabled", "view.gridSnap")
			c.optOneOf(g, "step", "view.gridSnap", 0.1, 0.25, 0.5, 1)
		}
		if a, ok := c.optObject(v, "angleSnap", "view"); ok {
			c.optBool(a, "enabled", "view.angleSnap")
			c.optOneOf(a, "step", "view.angleSnap", 5, 15, 45)
		}
		c.optBool(v, "groundSnap", "view")
	}
	if b, ok := c.optObject(m, "background", ""); ok {
		c.background(b)
	}
	if s, ok := c.optObject(m, "splat", ""); ok {
		c.mediaID(s, "mediaId", "splat", true)
		c.optVec3(s, "position", "splat", MaxCoord)
		c.optVec3(s, "rotation", "splat", maxRotation)
		c.number(s, "scale", "splat", 0.01, 100, false)
		c.optBool(s, "visible", "splat")
	}
	if g, ok := c.optObject(m, "ground", ""); ok {
		c.optBool(g, "visible", "ground")
		c.number(g, "opacity", "ground", 0, 1, false)
		c.number(g, "height", "ground", -100, 100, false)
	}
	if l, ok := c.optObject(m, "lights", ""); ok {
		for _, key := range []string{"key", "fill"} {
			if light, ok := c.optObject(l, key, "lights"); ok {
				c.light(light, join("lights", key))
			}
		}
	}
	if !c.ok() {
		return
	}

	objects := c.objects(m)
	if !c.ok() {
		return
	}
	c.outputCamera(m, objects)
	if !c.ok() {
		return
	}
	if t, ok := c.optObject(m, "timeline", ""); ok {
		c.timeline(t, objects)
	}
}

func (c *checker) background(b map[string]any) {
	mode, _ := b["mode"].(string)
	if _, present := b["mode"]; present {
		c.enum(b, "mode", "background", "color", "panorama")
	}
	c.optColor(b, "color", "background")
	p, has := c.optObject(b, "panorama", "background")
	if mode == "panorama" && !has && c.ok() {
		c.fail("background.panorama", "背景为全景图时必须指定全景来源")
		return
	}
	if has {
		c.str(p, "sourceNodeId", "background.panorama", MaxIDChars, true)
		c.mediaID(p, "mediaId", "background.panorama", true)
		c.number(p, "yaw", "background.panorama", 0, 360, false)
		c.number(p, "radius", "background.panorama", 5, 200, false)
	}
}

func (c *checker) light(l map[string]any, path string) {
	c.optBool(l, "visible", path)
	c.number(l, "intensity", path, 0, 10, false)
	c.optColor(l, "color", path)
	c.number(l, "azimuth", path, 0, 360, false)
	c.number(l, "elevation", path, -90, 90, false)
}

// objects 校验对象数组，返回 ID → 种类（给机位与时间轴校验用）。
func (c *checker) objects(m map[string]any) map[string]string {
	kinds := map[string]string{}
	v, present := m["objects"]
	if !present {
		return kinds
	}
	list, ok := v.([]any)
	if !ok {
		c.fail("objects", "必须是数组")
		return kinds
	}
	if len(list) > MaxObjects {
		c.fail("objects", "对象数 %d 超过上限 %d", len(list), MaxObjects)
		return kinds
	}
	for i, item := range list {
		path := index("objects", i)
		o, ok := item.(map[string]any)
		if !ok {
			c.fail(path, "必须是对象")
			return kinds
		}
		id := c.str(o, "id", path, MaxIDChars, true)
		if !c.ok() {
			return kinds
		}
		if _, dup := kinds[id]; dup {
			c.fail(join(path, "id"), "对象 ID 重复：%s", id)
			return kinds
		}
		c.str(o, "name", path, MaxNameChars, true)
		kind := c.enum(o, "kind", path, KindCharacter, KindCrowd, KindProp, KindCamera)
		if !c.ok() {
			return kinds
		}
		kinds[id] = kind
		c.transform(o, path)
		c.optBool(o, "visible", path)
		c.optBool(o, "locked", path)
		if _, present := o["source"]; present {
			c.enum(o, "source", path, SourceUser, SourceAI)
		}
		switch kind {
		case KindCharacter:
			c.character(o, path)
		case KindCrowd:
			c.crowd(o, path)
		case KindProp:
			c.prop(o, path)
		case KindCamera:
			c.camera(o, path)
		}
		if !c.ok() {
			return kinds
		}
	}
	return kinds
}

func (c *checker) transform(o map[string]any, path string) {
	t, ok := c.reqObject(o, "transform", path)
	if !ok {
		return
	}
	tp := join(path, "transform")
	c.reqVec3(t, "position", tp, MaxCoord)
	c.reqVec3(t, "rotation", tp, maxRotation)
	c.reqScale3(t, "scale", tp)
}

func (c *checker) character(o map[string]any, path string) {
	ch, ok := c.reqObject(o, "character", path)
	if !ok {
		return
	}
	p := join(path, "character")
	c.model(ch, p)
	c.number(ch, "height", p, 0.8, 2.3, true)
	c.enumSet(ch, "body", p, bodySet, true)
	c.str(ch, "posePreset", p, maxPoseCodeChars, false)
	c.boneOverrides(ch, p)
	if f, ok := c.optObject(ch, "footLock", p); ok {
		c.optBool(f, "left", join(p, "footLock"))
		c.optBool(f, "right", join(p, "footLock"))
	}
}

func (c *checker) crowd(o map[string]any, path string) {
	cr, ok := c.reqObject(o, "crowd", path)
	if !ok {
		return
	}
	p := join(path, "crowd")
	c.model(cr, p)
	rows := c.integer(cr, "rows", p, 1, MaxCrowdRows, true)
	cols := c.integer(cr, "cols", p, 1, MaxCrowdCols, true)
	if c.ok() && rows*cols > MaxCrowdSize {
		c.fail(p, "单阵人数 %d 超过上限 %d", rows*cols, MaxCrowdSize)
		return
	}
	c.number(cr, "spacing", p, 0.5, 5, true)
	c.str(cr, "posePreset", p, maxPoseCodeChars, false)
	c.optBool(cr, "jitter", p)
	c.integer(cr, "seed", p, 0, math.MaxInt32, false)
}

func (c *checker) prop(o map[string]any, path string) {
	pr, ok := c.reqObject(o, "prop", path)
	if !ok {
		return
	}
	p := join(path, "prop")
	c.enumSet(pr, "shape", p, propShapeSet, true)
	c.reqScale3(pr, "size", p)
	c.optColor(pr, "color", p)
}

func (c *checker) camera(o map[string]any, path string) {
	ca, ok := c.reqObject(o, "camera", path)
	if !ok {
		return
	}
	p := join(path, "camera")
	c.number(ca, "focalLengthMm", p, 12, 200, true)
	c.number(ca, "sensorWidthMm", p, 10, 70, false)
	if c.ok() {
		v, present := ca["aspect"]
		s, isStr := v.(string)
		switch {
		case !present:
			c.fail(join(p, "aspect"), "缺少画幅")
		case !isStr:
			c.fail(join(p, "aspect"), "画幅必须是字符串")
		default:
			if _, known := AspectOf(s); !known {
				c.fail(join(p, "aspect"), "不支持的画幅 %q", s)
			}
		}
	}
	near := c.number(ca, "near", p, 0.01, 10, false)
	far := c.number(ca, "far", p, 10, 10000, false)
	if c.ok() && near > 0 && far > 0 && near >= far {
		c.fail(join(p, "near"), "近裁剪面必须小于远裁剪面")
	}
	c.number(ca, "roll", p, -180, 180, false)
}

func (c *checker) model(m map[string]any, path string) {
	if !c.ok() {
		return
	}
	v, present := m["model"]
	s, isStr := v.(string)
	switch {
	case !present:
		c.fail(join(path, "model"), "缺少素模")
	case !isStr || !modelSet[s]:
		c.fail(join(path, "model"), "不认识的素模 %v", v)
	}
}

func (c *checker) boneOverrides(m map[string]any, path string) {
	if !c.ok() {
		return
	}
	v, present := m["boneOverrides"]
	if !present || v == nil {
		return
	}
	bones, ok := v.(map[string]any)
	p := join(path, "boneOverrides")
	if !ok {
		c.fail(p, "必须是对象（骨骼名 → 旋转）")
		return
	}
	if len(bones) > MaxBoneOverrides {
		c.fail(p, "骨骼覆盖 %d 项超过上限 %d", len(bones), MaxBoneOverrides)
		return
	}
	for _, name := range sortedKeys(bones) {
		if utf8.RuneCountInString(name) > maxPoseCodeChars {
			c.fail(p, "骨骼名过长：%s", name)
			return
		}
		c.vec3Value(bones[name], join(p, name), maxBoneRotation)
		if !c.ok() {
			return
		}
	}
}

func (c *checker) outputCamera(m map[string]any, kinds map[string]string) {
	v, present := m["outputCameraId"]
	if !present || v == nil {
		return
	}
	id, ok := v.(string)
	if !ok {
		c.fail("outputCameraId", "必须是字符串或 null")
		return
	}
	if kinds[id] != KindCamera {
		c.fail("outputCameraId", "输出机位 %q 不存在", id)
	}
}

func (c *checker) timeline(t map[string]any, kinds map[string]string) {
	if v, present := t["fps"]; present {
		if f, ok := v.(float64); !ok || f != TimelineFPS {
			c.fail("timeline.fps", "帧率固定为 %d", TimelineFPS)
			return
		}
	}
	duration := 150
	if _, present := t["durationFrames"]; present {
		duration = c.integer(t, "durationFrames", "timeline", MinDurationFrames, MaxDurationFrames, true)
	}
	c.optBool(t, "loop", "timeline")
	if !c.ok() {
		return
	}
	v, present := t["tracks"]
	if !present {
		return
	}
	tracks, ok := v.([]any)
	if !ok {
		c.fail("timeline.tracks", "必须是数组")
		return
	}
	total := 0
	for i, item := range tracks {
		path := index("timeline.tracks", i)
		tr, ok := item.(map[string]any)
		if !ok {
			c.fail(path, "必须是对象")
			return
		}
		objectID := c.str(tr, "objectId", path, MaxIDChars, true)
		if !c.ok() {
			return
		}
		kind, exists := kinds[objectID]
		if !exists {
			c.fail(join(path, "objectId"), "对象 %q 不存在", objectID)
			return
		}
		channel := c.enum(tr, "channel", path, "transform", "pose", "camera")
		if !c.ok() {
			return
		}
		// 通道与对象种类要对得上：机位用 camera 通道（含焦距），姿势只对角色有意义。
		switch {
		case channel == "camera" && kind != KindCamera:
			c.fail(join(path, "channel"), "camera 通道只能挂在机位上")
		case channel == "pose" && kind != KindCharacter:
			c.fail(join(path, "channel"), "pose 通道只能挂在角色上")
		case channel == "transform" && kind == KindCamera:
			c.fail(join(path, "channel"), "机位的关键帧用 camera 通道")
		}
		if !c.ok() {
			return
		}
		kv, present := tr["keyframes"]
		keyframes, isList := kv.([]any)
		if !present || !isList {
			c.fail(join(path, "keyframes"), "必须是数组")
			return
		}
		if len(keyframes) > MaxKeyframesPerTrack {
			c.fail(join(path, "keyframes"), "单条轨道关键帧 %d 个超过上限 %d", len(keyframes), MaxKeyframesPerTrack)
			return
		}
		total += len(keyframes)
		if total > MaxKeyframes {
			c.fail("timeline.tracks", "全场景关键帧超过上限 %d", MaxKeyframes)
			return
		}
		seen := map[int]bool{}
		for j, kitem := range keyframes {
			kp := index(join(path, "keyframes"), j)
			kf, ok := kitem.(map[string]any)
			if !ok {
				c.fail(kp, "必须是对象")
				return
			}
			frame := c.integer(kf, "frame", kp, 0, math.MaxInt32, true)
			if !c.ok() {
				return
			}
			if frame > duration {
				c.fail(join(kp, "frame"), "帧号 %d 超出时长 %d 帧", frame, duration)
				return
			}
			if seen[frame] {
				c.fail(join(kp, "frame"), "同一轨道里帧号 %d 重复", frame)
				return
			}
			seen[frame] = true
			if _, present := kf["interpolation"]; present {
				c.enum(kf, "interpolation", kp, "linear", "smooth", "hold")
			}
			c.keyframeValue(kf, kp, channel)
			if !c.ok() {
				return
			}
		}
	}
}

func (c *checker) keyframeValue(kf map[string]any, path, channel string) {
	val, ok := c.reqObject(kf, "value", path)
	if !ok {
		return
	}
	vp := join(path, "value")
	switch channel {
	case "transform":
		c.reqVec3(val, "position", vp, MaxCoord)
		c.reqVec3(val, "rotation", vp, maxRotation)
	case "camera":
		c.reqVec3(val, "position", vp, MaxCoord)
		c.reqVec3(val, "rotation", vp, maxRotation)
		c.number(val, "focalLengthMm", vp, 12, 200, true)
	case "pose":
		c.str(val, "posePreset", vp, maxPoseCodeChars, false)
		c.boneOverrides(val, vp)
	}
}

// ---------------------------------------------------------------- 基本类型

func (c *checker) optObject(m map[string]any, key, base string) (map[string]any, bool) {
	if !c.ok() {
		return nil, false
	}
	v, present := m[key]
	if !present {
		return nil, false
	}
	o, ok := v.(map[string]any)
	if !ok {
		c.fail(join(base, key), "必须是对象")
		return nil, false
	}
	return o, true
}

func (c *checker) reqObject(m map[string]any, key, base string) (map[string]any, bool) {
	if !c.ok() {
		return nil, false
	}
	if _, present := m[key]; !present {
		c.fail(join(base, key), "缺少必填字段")
		return nil, false
	}
	return c.optObject(m, key, base)
}

// number 校验数值范围（闭区间）；required=false 时缺省放行并返回 0。
func (c *checker) number(m map[string]any, key, base string, lo, hi float64, required bool) float64 {
	if !c.ok() {
		return 0
	}
	v, present := m[key]
	if !present {
		if required {
			c.fail(join(base, key), "缺少必填字段")
		}
		return 0
	}
	f, ok := v.(float64)
	if !ok {
		c.fail(join(base, key), "必须是数字")
		return 0
	}
	if f < lo || f > hi {
		c.fail(join(base, key), "取值 %s 超出范围 %s~%s", fmtNum(f), fmtNum(lo), fmtNum(hi))
		return 0
	}
	return f
}

func (c *checker) integer(m map[string]any, key, base string, lo, hi int, required bool) int {
	if !c.ok() {
		return 0
	}
	v, present := m[key]
	if !present {
		if required {
			c.fail(join(base, key), "缺少必填字段")
		}
		return 0
	}
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) {
		c.fail(join(base, key), "必须是整数")
		return 0
	}
	if f < float64(lo) || f > float64(hi) {
		c.fail(join(base, key), "取值 %s 超出范围 %d~%d", fmtNum(f), lo, hi)
		return 0
	}
	return int(f)
}

func (c *checker) optOneOf(m map[string]any, key, base string, allowed ...float64) {
	if !c.ok() {
		return
	}
	v, present := m[key]
	if !present {
		return
	}
	f, ok := v.(float64)
	if !ok {
		c.fail(join(base, key), "必须是数字")
		return
	}
	for _, a := range allowed {
		if f == a {
			return
		}
	}
	c.fail(join(base, key), "取值 %s 不在可选档位内", fmtNum(f))
}

func (c *checker) optBool(m map[string]any, key, base string) {
	if !c.ok() {
		return
	}
	v, present := m[key]
	if !present {
		return
	}
	if _, ok := v.(bool); !ok {
		c.fail(join(base, key), "必须是布尔值")
	}
}

// str 校验字符串长度（按字符数）；required 时必须存在且非空。
func (c *checker) str(m map[string]any, key, base string, maxChars int, required bool) string {
	if !c.ok() {
		return ""
	}
	v, present := m[key]
	if !present {
		if required {
			c.fail(join(base, key), "缺少必填字段")
		}
		return ""
	}
	s, ok := v.(string)
	if !ok {
		c.fail(join(base, key), "必须是字符串")
		return ""
	}
	if required && s == "" {
		c.fail(join(base, key), "不能为空")
		return ""
	}
	if utf8.RuneCountInString(s) > maxChars {
		c.fail(join(base, key), "长度超过 %d 个字符", maxChars)
		return ""
	}
	return s
}

func (c *checker) enum(m map[string]any, key, base string, allowed ...string) string {
	return c.enumSet(m, key, base, stringSet(allowed), true)
}

func (c *checker) enumSet(m map[string]any, key, base string, allowed map[string]bool, required bool) string {
	if !c.ok() {
		return ""
	}
	v, present := m[key]
	if !present {
		if required {
			c.fail(join(base, key), "缺少必填字段")
		}
		return ""
	}
	s, ok := v.(string)
	if !ok || !allowed[s] {
		c.fail(join(base, key), "不支持的取值 %v", v)
		return ""
	}
	return s
}

func (c *checker) optColor(m map[string]any, key, base string) {
	if !c.ok() {
		return
	}
	v, present := m[key]
	if !present {
		return
	}
	s, ok := v.(string)
	if !ok || !colorPattern.MatchString(s) {
		c.fail(join(base, key), "颜色必须是 #RRGGBB")
	}
}

// mediaID 校验媒体 ID：对外一律字符串（防 JS 精度丢失），内容是正整数。
func (c *checker) mediaID(m map[string]any, key, base string, required bool) {
	if !c.ok() {
		return
	}
	v, present := m[key]
	if !present {
		if required {
			c.fail(join(base, key), "缺少必填字段")
		}
		return
	}
	s, ok := v.(string)
	if !ok {
		c.fail(join(base, key), "媒体 ID 必须是字符串")
		return
	}
	if id, err := strconv.ParseInt(s, 10, 64); err != nil || id <= 0 {
		c.fail(join(base, key), "媒体 ID 不合法：%q", s)
	}
}

func (c *checker) optVec3(m map[string]any, key, base string, bound float64) {
	if !c.ok() {
		return
	}
	if v, present := m[key]; present {
		c.vec3Value(v, join(base, key), bound)
	}
}

func (c *checker) reqVec3(m map[string]any, key, base string, bound float64) {
	if !c.ok() {
		return
	}
	v, present := m[key]
	if !present {
		c.fail(join(base, key), "缺少必填字段")
		return
	}
	c.vec3Value(v, join(base, key), bound)
}

func (c *checker) vec3Value(v any, path string, bound float64) {
	vec, ok := v.(map[string]any)
	if !ok {
		c.fail(path, "必须是 {x, y, z}")
		return
	}
	for _, axis := range []string{"x", "y", "z"} {
		c.number(vec, axis, path, -bound, bound, true)
		if !c.ok() {
			return
		}
	}
}

// reqScale3 校验缩放 / 尺寸：三个分量都在 0.01~100。
func (c *checker) reqScale3(m map[string]any, key, base string) {
	if !c.ok() {
		return
	}
	v, present := m[key]
	if !present {
		c.fail(join(base, key), "缺少必填字段")
		return
	}
	vec, ok := v.(map[string]any)
	p := join(base, key)
	if !ok {
		c.fail(p, "必须是 {x, y, z}")
		return
	}
	for _, axis := range []string{"x", "y", "z"} {
		c.number(vec, axis, p, 0.01, 100, true)
		if !c.ok() {
			return
		}
	}
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
