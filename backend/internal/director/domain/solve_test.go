package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 测试工具

// seqIDs 是可预测的对象 ID 序列（SolveOptions.NewID 注入）。
func seqIDs() func() string {
	n := 0
	return func() string { n++; return fmt.Sprintf("obj_new_%d", n) }
}

func parseScene(t *testing.T, raw []byte) Scene {
	t.Helper()
	if err := Validate(raw); err != nil {
		t.Fatalf("输入场景不合法：%v", err)
	}
	s, err := ParseScene(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sceneOf(t *testing.T, v any) Scene {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return parseScene(t, raw)
}

// build 跑完整条「解析 → 求解 → 应用」，返回提案与解析后的建议场景。
func build(t *testing.T, scene Scene, draft string) (Proposal, Scene) {
	t.Helper()
	d, dropped, err := ParseDraft(draft)
	if err != nil {
		t.Fatalf("草案解析失败：%v", err)
	}
	p, err := Propose(scene, d, dropped, SolveOptions{NewID: seqIDs()})
	if err != nil {
		t.Fatalf("求解失败：%v", err)
	}
	out, err := ParseScene(p.Scene)
	if err != nil {
		t.Fatal(err)
	}
	return p, out
}

func emptyScene(t *testing.T) Scene {
	return parseScene(t, []byte(`{"schemaVersion":1,"objects":[],"outputCameraId":null}`))
}

func byName(t *testing.T, s Scene, name string) Object {
	t.Helper()
	for _, o := range s.Objects() {
		if o.Name() == name {
			return o
		}
	}
	t.Fatalf("场景里没有「%s」", name)
	return Object{}
}

func posOf(o Object) vec3f { p, _ := transformOf(o.Raw()); return p }

func heightOf(o Object) float64 { _, h := footprint(o.Raw()); return h }

// view 是从机位看出去的相机坐标系（独立于求解器重建：XYZ 欧拉 + 绕视线轴的横滚）。
type view struct {
	pos, right, up, forward vec3f
	vfov                    float64
	cam                     map[string]any
}

func viewOf(t *testing.T, o Object) view {
	t.Helper()
	tr, _ := o.Raw()["transform"].(map[string]any)
	cam, _ := o.Raw()["camera"].(map[string]any)
	roll, _ := cam["roll"].(float64)
	rot := vecOf(tr["rotation"])
	m := eulerXYZMatrix(rot.x, rot.y, rot.z).mul(rotZ(rad(roll)))
	// 传感器缺省 36mm（dev/02 §2.4 的默认值，校验允许不写）。
	focal := numOr(cam["focalLengthMm"], defaultFocalMm)
	sensor := numOr(cam["sensorWidthMm"], defaultSensorMm)
	aspect, _ := cam["aspect"].(string)
	vfov, err := verticalFOV(focal, sensor, aspect)
	if err != nil {
		t.Fatal(err)
	}
	return view{pos: vecOf(tr["position"]), right: m.mulVec(axisX), up: m.mulVec(axisY),
		forward: m.mulVec(vec3f{0, 0, -1}), vfov: vfov, cam: cam}
}

func outputView(t *testing.T, s Scene) view {
	t.Helper()
	o, ok := s.ObjectByID(s.OutputCameraID())
	if !ok || o.Kind() != KindCamera {
		t.Fatalf("输出机位 %q 不存在", s.OutputCameraID())
	}
	return viewOf(t, o)
}

// screenX 是点在机位画面里的横坐标（归一化：-1 左边缘、+1 右边缘之外也照算）。
func (v view) screenX(t *testing.T, p vec3f) float64 {
	t.Helper()
	rel := p.sub(v.pos)
	depth := rel.dot(v.forward)
	if depth <= 0 {
		t.Fatalf("点 %+v 在机位背后", p)
	}
	return rel.dot(v.right) / depth
}

func pitchDeg(v view) float64 { return deg(math.Asin(math.Max(-1, math.Min(1, v.forward.y)))) }

func chestOf(o Object) vec3f {
	p := posOf(o)
	p.y += heightOf(o) / 2
	return p
}

func angleBetween(a, b vec3f) float64 {
	la, lb := a.length(), b.length()
	if la < 1e-9 || lb < 1e-9 {
		return 0
	}
	return deg(math.Acos(math.Max(-1, math.Min(1, a.dot(b)/(la*lb)))))
}

// docLookAt 是 dev/01 §4.4 的注视点比例（测试里照文档另写一份，不读实现的表）。
var docLookAt = map[string]float64{
	"extreme_long": 0.5, "long": 0.5, "full": 0.5, "medium_long": 0.75, "medium": 0.75,
	"medium_close": 0.9, "close_up": 0.9, "extreme_close_up": 0.9,
}

// assertFraming：机位朝着目标包围盒的注视点看，且「画面高 ÷ 目标高」等于该景别的 coverage。
// 只用于半径一致的目标（全是角色）：包围盒中心按位置算。
func assertFraming(t *testing.T, s Scene, targets []string, shot string) {
	t.Helper()
	v := outputView(t, s)
	minX, maxX, minZ, maxZ := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, name := range targets {
		o := byName(t, s, name)
		p := posOf(o)
		minX, maxX = math.Min(minX, p.x), math.Max(maxX, p.x)
		minZ, maxZ = math.Min(minZ, p.z), math.Max(maxZ, p.z)
		minY, maxY = math.Min(minY, p.y), math.Max(maxY, p.y+heightOf(o))
	}
	h := maxY - minY
	look := vec3f{(minX + maxX) / 2, minY + docLookAt[shot]*h, (minZ + maxZ) / 2}
	toLook := look.sub(v.pos)
	if a := angleBetween(toLook, v.forward); a > 0.5 {
		t.Errorf("机位没有对准注视点：偏了 %.2f°", a)
	}
	coverage := 2 * toLook.length() * math.Tan(v.vfov/2) / h
	want := coverageOf(shot)
	if math.Abs(coverage-want)/want > 0.01 {
		t.Errorf("景别 %s：coverage 应为 %.3f，实际 %.3f", shot, want, coverage)
	}
}

func assertMinGap(t *testing.T, s Scene, gap float64) {
	t.Helper()
	var chars []Object
	for _, o := range s.Objects() {
		if o.Kind() == KindCharacter {
			chars = append(chars, o)
		}
	}
	for i := range chars {
		for j := i + 1; j < len(chars); j++ {
			a, b := chars[i], chars[j]
			if a.Source() != SourceAI && b.Source() != SourceAI {
				continue // 两个都是用户摆的：不归求解器管
			}
			if d := posOf(a).horizontalDist(posOf(b)); d < gap {
				t.Errorf("「%s」与「%s」水平距离 %.3f 米，小于 %.1f", a.Name(), b.Name(), d, gap)
			}
		}
	}
}

func assertGrounded(t *testing.T, s Scene) {
	t.Helper()
	ground := groundHeight(s)
	for _, o := range s.Objects() {
		if o.Kind() == KindCamera || o.Source() != SourceAI {
			continue
		}
		if y := posOf(o).y; math.Abs(y-ground) > 1e-9 {
			t.Errorf("「%s」没有贴地：y=%.4f，地面 %.4f", o.Name(), y, ground)
		}
	}
}

func assertLeftOf(t *testing.T, s Scene, left, right string) {
	t.Helper()
	v := outputView(t, s)
	xl := v.screenX(t, chestOf(byName(t, s, left)))
	xr := v.screenX(t, chestOf(byName(t, s, right)))
	if xl >= xr {
		t.Errorf("从输出机位看「%s」应在「%s」左边：x=%.3f / %.3f", left, right, xl, xr)
	}
}

func assertFaces(t *testing.T, o Object, target vec3f) {
	t.Helper()
	_, yaw := transformOf(o.Raw())
	if a := angleBetween(forwardOfYaw(yaw), target.sub(posOf(o)).horizontal()); a > 1 {
		t.Errorf("「%s」没有面向目标：偏了 %.1f°", o.Name(), a)
	}
}

func hasNote(p Proposal, sub string) bool {
	for _, d := range p.Defaults {
		if strings.Contains(d, sub) {
			return true
		}
	}
	return false
}

func countKind(s Scene, kind string) int {
	n := 0
	for _, o := range s.Objects() {
		if o.Kind() == kind {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- fixture

type buildExpect struct {
	Added           *int              `json:"added"`
	Updated         *int              `json:"updated"`
	Removed         *int              `json:"removed"`
	Characters      *int              `json:"characters"`
	Cameras         *int              `json:"cameras"`
	Props           *int              `json:"props"`
	Crowds          *int              `json:"crowds"`
	MinGapM         float64           `json:"minGapM"`
	Grounded        bool              `json:"grounded"`
	LeftOf          [][2]string       `json:"leftOf"`
	Framing         *framingExpect    `json:"framing"`
	Pitch           string            `json:"pitch"`
	PitchDeg        *float64          `json:"pitchDeg"`
	Roll            *float64          `json:"roll"`
	Aspect          string            `json:"aspect"`
	OutputCamera    string            `json:"outputCamera"`
	Unchanged       []string          `json:"unchanged"`
	Absent          []string          `json:"absent"`
	UpdatedContains []string          `json:"updatedContains"`
	KeepsField      map[string]string `json:"keepsField"`
	Faces           [][2]string       `json:"faces"`
	FacesCamera     []string          `json:"facesCamera"`
	DefaultsContain []string          `json:"defaultsContain"`
}

type framingExpect struct {
	Targets  []string `json:"targets"`
	ShotSize string   `json:"shotSize"`
}

type buildCase struct {
	Why    string          `json:"why"`
	Scene  json.RawMessage `json:"scene"`
	Draft  json.RawMessage `json:"draft"`
	Expect buildExpect     `json:"expect"`
}

func checkCase(t *testing.T, c buildCase) {
	t.Helper()
	in := parseScene(t, c.Scene)
	p, out := build(t, in, string(c.Draft))
	e := c.Expect
	ints := []struct {
		name string
		want *int
		got  int
	}{
		{"added", e.Added, len(p.Changes.Added)}, {"updated", e.Updated, len(p.Changes.Updated)},
		{"removed", e.Removed, len(p.Changes.Removed)}, {"characters", e.Characters, countKind(out, KindCharacter)},
		{"cameras", e.Cameras, countKind(out, KindCamera)}, {"props", e.Props, countKind(out, KindProp)},
		{"crowds", e.Crowds, countKind(out, KindCrowd)},
	}
	for _, x := range ints {
		if x.want != nil && *x.want != x.got {
			t.Errorf("%s 应为 %d，实际 %d", x.name, *x.want, x.got)
		}
	}
	if e.MinGapM > 0 {
		assertMinGap(t, out, e.MinGapM)
	}
	if e.Grounded {
		assertGrounded(t, out)
	}
	for _, pair := range e.LeftOf {
		assertLeftOf(t, out, pair[0], pair[1])
	}
	if e.Framing != nil {
		assertFraming(t, out, e.Framing.Targets, e.Framing.ShotSize)
	}
	if e.Pitch != "" || e.PitchDeg != nil || e.Roll != nil || e.Aspect != "" || len(e.FacesCamera) > 0 {
		v := outputView(t, out)
		switch e.Pitch {
		case "up":
			if pitchDeg(v) <= 1 {
				t.Errorf("应为仰视，俯仰 %.2f°", pitchDeg(v))
			}
		case "down":
			if pitchDeg(v) >= -1 {
				t.Errorf("应为俯视，俯仰 %.2f°", pitchDeg(v))
			}
		}
		if e.PitchDeg != nil && math.Abs(pitchDeg(v)-*e.PitchDeg) > 0.5 {
			t.Errorf("俯仰应为 %.1f°，实际 %.2f°", *e.PitchDeg, pitchDeg(v))
		}
		if e.Roll != nil {
			if roll, _ := v.cam["roll"].(float64); roll != *e.Roll {
				t.Errorf("横滚应为 %v，实际 %v", *e.Roll, roll)
			}
		}
		if e.Aspect != "" && v.cam["aspect"] != e.Aspect {
			t.Errorf("画幅应为 %s，实际 %v", e.Aspect, v.cam["aspect"])
		}
		for _, name := range e.FacesCamera {
			assertFaces(t, byName(t, out, name), v.pos)
		}
	}
	switch e.OutputCamera {
	case "":
	case "new":
		if !contains(p.Changes.Added, out.OutputCameraID()) {
			t.Errorf("输出机位应是新建的，实际 %q", out.OutputCameraID())
		}
	default:
		if out.OutputCameraID() != e.OutputCamera {
			t.Errorf("输出机位应为 %s，实际 %q", e.OutputCamera, out.OutputCameraID())
		}
	}
	for _, id := range e.Unchanged {
		before, ok1 := in.ObjectByID(id)
		after, ok2 := out.ObjectByID(id)
		if !ok1 || !ok2 || !reflect.DeepEqual(before.Raw(), after.Raw()) {
			t.Errorf("「%s」应逐字不变", id)
		}
	}
	for _, id := range e.Absent {
		if _, ok := out.ObjectByID(id); ok {
			t.Errorf("「%s」应被删除", id)
		}
	}
	for _, id := range e.UpdatedContains {
		if !contains(p.Changes.Updated, id) {
			t.Errorf("变更清单的 updated 里应有 %s：%v", id, p.Changes.Updated)
		}
	}
	for id, field := range e.KeepsField {
		if o, ok := out.ObjectByID(id); !ok || o.Raw()[field] == nil {
			t.Errorf("「%s」的未知字段 %s 应保留", id, field)
		}
	}
	for _, pair := range e.Faces {
		assertFaces(t, byName(t, out, pair[0]), posOf(byName(t, out, pair[1])))
	}
	for _, sub := range e.DefaultsContain {
		if !hasNote(p, sub) {
			t.Errorf("说明里应有「%s」：\n%s", sub, strings.Join(p.Defaults, "\n"))
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestSolve_fixture用例全部通过：遍历 build/case-*.json（DRT-30 的假上游从这里取草案）。
func TestSolve_fixture用例全部通过(t *testing.T) {
	files, err := filepath.Glob(fixturePath("build", "case-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 10 {
		t.Fatalf("求解用例只有 %d 个，至少要 10 个", len(files))
	}
	shots := map[string]bool{}
	angleSeen := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c buildCase
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s：%v", f, err)
		}
		var draft struct {
			Camera struct {
				ShotSize string `json:"shotSize"`
				Angle    string `json:"angle"`
			} `json:"camera"`
		}
		_ = json.Unmarshal(c.Draft, &draft)
		shots[draft.Camera.ShotSize] = true
		angleSeen[draft.Camera.Angle] = true
		t.Run(strings.TrimSuffix(filepath.Base(f), ".json"), func(t *testing.T) { checkCase(t, c) })
	}
	for _, s := range shotSizes {
		if !shots[s.Code] {
			t.Errorf("fixture 没有覆盖景别 %s", s.Code)
		}
	}
	for _, a := range angles {
		if !angleSeen[a.Code] {
			t.Errorf("fixture 没有覆盖角度 %s", a.Code)
		}
	}
}

// ---------------------------------------------------------------- 单条规则

// TestSolve_观看者左右：六个方位都在机位的屏幕上判左右（DR-AC-29）；没有 camera 草案时按输出机位。
func TestSolve_观看者左右(t *testing.T) {
	for _, side := range []string{"front", "front_left", "front_right", "left", "right", "back"} {
		for _, angle := range []string{"eye_level", "high", "low", "top", "dutch"} {
			_, out := build(t, emptyScene(t), `{"objects":[
				{"op":"add","ref":"A","kind":"character","relation":{"type":"left_of","target":"B"}},
				{"op":"add","ref":"B","kind":"character"},
				{"op":"add","ref":"C","kind":"character","relation":{"type":"right_of","target":"B"}},
				{"op":"add","ref":"D","kind":"character","relation":{"type":"in_front_of","target":"B"}}],
				"camera":{"shotSize":"long","angle":"`+angle+`","side":"`+side+`"}}`)
			assertLeftOf(t, out, "A", "B")
			assertLeftOf(t, out, "B", "C")
			// 前后同样按观看者：in_front_of 离机位更近。
			v := outputView(t, out)
			if posOf(byName(t, out, "D")).sub(v.pos).length() >= posOf(byName(t, out, "B")).sub(v.pos).length() {
				t.Errorf("%s/%s：D 在 B 前面，应离机位更近", side, angle)
			}
		}
	}
	// 没有 camera 草案：用户的输出机位在 +X 处朝 −X 看，屏幕右是世界 −Z。
	scene := sceneOf(t, map[string]any{"schemaVersion": 1, "outputCameraId": "obj_cam", "objects": []any{
		map[string]any{"id": "obj_cam", "name": "侧机位", "kind": "camera",
			"transform": transform(vec3(5, 1.5, 0), vec3(0, 90, 0), vec3(1, 1, 1)),
			"camera":    map[string]any{"focalLengthMm": 35, "aspect": "16:9"}},
	}})
	p, out := build(t, scene, `{"objects":[{"op":"add","ref":"A","kind":"character","relation":{"type":"left_of","target":"B"}},{"op":"add","ref":"B","kind":"character"}]}`)
	assertLeftOf(t, out, "A", "B")
	if a := posOf(byName(t, out, "A")); a.z <= 0 {
		t.Errorf("从 +X 看过去的左边是世界 +Z，A 应在 z>0 处：%+v", a)
	}
	if len(p.Changes.Added) != 2 || out.OutputCameraID() != "obj_cam" {
		t.Errorf("没有 camera 草案时不动机位：%+v", p.Changes)
	}
}

func TestSolve_两两距离不小于半米(t *testing.T) {
	// 六个人全挤在场景中心、彼此「靠近」：避让后两两不小于 0.5 米。
	var objs []string
	for i := 0; i < 6; i++ {
		rel := `"relation":{"type":"center"}`
		if i%2 == 1 {
			rel = fmt.Sprintf(`"relation":{"type":"near","target":"P%d","distance":0.2}`, i-1)
		}
		objs = append(objs, fmt.Sprintf(`{"op":"add","ref":"P%d","kind":"character",%s}`, i, rel))
	}
	_, out := build(t, emptyScene(t), `{"objects":[`+strings.Join(objs, ",")+`],"camera":{"shotSize":"full"}}`)
	assertMinGap(t, out, minObjectGap)
	if n := countKind(out, KindCharacter); n != 6 {
		t.Fatalf("6 个人一个都不能少，实际 %d", n)
	}
}

func TestSolve_推不开时说明(t *testing.T) {
	// 一长排 25 个用户摆的角色，间距 0.5 米：新角色落在中间，沿屏幕右推 10 次也出不去。
	var objs []any
	for i := 0; i < 25; i++ {
		objs = append(objs, map[string]any{"id": fmt.Sprintf("obj_u%d", i), "name": fmt.Sprintf("路人 %d", i),
			"kind": KindCharacter, "transform": transform(vec3(float64(i-12)*0.5, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
			"character": map[string]any{"model": ModelAdultMale, "height": 1.8, "body": "standard"}})
	}
	scene := sceneOf(t, map[string]any{"schemaVersion": 1, "objects": objs})
	p, _ := build(t, scene, `{"objects":[{"op":"add","ref":"闯入者","kind":"character"}],"camera":{"shotSize":"long"}}`)
	if !hasNote(p, "「闯入者」周围太挤") {
		t.Errorf("推不开时要说明：%v", p.Defaults)
	}
}

func TestSolve_角色贴地(t *testing.T) {
	scene := sceneOf(t, map[string]any{"schemaVersion": 1, "ground": map[string]any{"height": 0.5}, "objects": []any{
		map[string]any{"id": "obj_ai", "name": "旧角色", "kind": KindCharacter, "source": SourceAI,
			"transform": transform(vec3(3, 2, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
			"character": map[string]any{"model": ModelAdultMale, "height": 1.8, "body": "standard"}},
	}})
	_, out := build(t, scene, `{"objects":[
		{"op":"add","ref":"人","kind":"character"},
		{"op":"add","ref":"箱","kind":"prop","shape":"box","relation":{"type":"near","target":"人"}},
		{"op":"add","ref":"群","kind":"crowd","rows":2,"cols":2,"relation":{"type":"behind","target":"人"}},
		{"op":"update","ref":"obj_ai","posePreset":"wave"}],
		"camera":{"shotSize":"full","angle":"low"}}`)
	assertGrounded(t, out)
	if y := posOf(byName(t, out, "旧角色")).y; y != 0.5 {
		t.Errorf("修改过的 AI 对象也贴地：y=%v", y)
	}
	// 低机位离地 0.4 米，按地面高度算。
	if v := outputView(t, out); math.Abs(v.pos.y-(0.5+lowCameraHeight)) > 1e-6 {
		t.Errorf("低机位应离地 %.1f 米：y=%.4f", lowCameraHeight, v.pos.y)
	}
}

func TestSolve_景别反推距离(t *testing.T) {
	for _, aspect := range []string{"16:9", "9:16", "21:9"} {
		for _, s := range shotSizes {
			scene := sceneOf(t, map[string]any{"schemaVersion": 1, "outputCameraId": "obj_cam", "objects": []any{
				map[string]any{"id": "obj_cam", "name": "机位 1", "kind": KindCamera, "source": SourceAI,
					"transform": transform(vec3(0, 1.5, 4), vec3(0, 0, 0), vec3(1, 1, 1)),
					"camera":    map[string]any{"focalLengthMm": 50, "sensorWidthMm": 36, "aspect": aspect}},
			}})
			_, out := build(t, scene, `{"objects":[{"op":"add","ref":"主角","kind":"character","model":"adult_female"}],
				"camera":{"target":["主角"],"shotSize":"`+s.Code+`","angle":"eye_level","side":"front"}}`)
			assertFraming(t, out, []string{"主角"}, s.Code)
			// 显式核一遍公式：d = (coverage × H / 2) / tan(vfov / 2)，H = 成年女身高 1.625。
			v := outputView(t, out)
			vfov, _ := verticalFOV(50, 36, aspect)
			want := (s.Coverage * 1.625 / 2) / math.Tan(vfov/2)
			look := vec3f{0, docLookAt[s.Code] * 1.625, 0}
			if got := v.pos.sub(look).length(); math.Abs(got-want) > 1e-3 {
				t.Errorf("%s / %s：距离应为 %.4f，实际 %.4f", aspect, s.Code, want, got)
			}
		}
	}
}

func TestSolve_角度对应机位高度与俯仰(t *testing.T) {
	cases := []struct {
		angle string
		check func(t *testing.T, v view, look vec3f)
	}{
		{"eye_level", func(t *testing.T, v view, look vec3f) {
			if math.Abs(v.pos.y-look.y) > 1e-3 || math.Abs(pitchDeg(v)) > 0.01 {
				t.Errorf("平视：机位与注视点等高、俯仰 0，实际 y=%.3f 俯仰 %.2f°", v.pos.y, pitchDeg(v))
			}
		}},
		{"low", func(t *testing.T, v view, look vec3f) {
			if math.Abs(v.pos.y-lowCameraHeight) > 1e-3 || pitchDeg(v) <= 0 {
				t.Errorf("低机位：离地 0.4 米、仰视，实际 y=%.3f 俯仰 %.2f°", v.pos.y, pitchDeg(v))
			}
		}},
		{"high", func(t *testing.T, v view, look vec3f) {
			h := v.pos.sub(look).horizontal().length()
			if math.Abs((v.pos.y-look.y)-h*math.Tan(rad(30))) > 1e-3 || math.Abs(pitchDeg(v)+30) > 0.01 {
				t.Errorf("俯拍：高出注视点 d·tan30°、俯仰 −30°，实际高出 %.3f（应为 %.3f）俯仰 %.2f°", v.pos.y-look.y, h*math.Tan(rad(30)), pitchDeg(v))
			}
		}},
		{"top", func(t *testing.T, v view, look vec3f) {
			if math.Abs(pitchDeg(v)+85) > 0.01 {
				t.Errorf("顶视：俯仰 −85°，实际 %.2f°", pitchDeg(v))
			}
		}},
		{"dutch", func(t *testing.T, v view, look vec3f) {
			if roll, _ := v.cam["roll"].(float64); roll != dutchRollDeg || math.Abs(pitchDeg(v)) > 0.01 {
				t.Errorf("斜角：横滚 15°、俯仰 0，实际横滚 %v 俯仰 %.2f°", v.cam["roll"], pitchDeg(v))
			}
			// 横滚写在 camera.roll，不混进 transform.rotation：视线方向与平视相同。
			if math.Abs(v.up.y-math.Cos(rad(dutchRollDeg))) > 1e-6 {
				t.Errorf("斜角的上方向应绕视线转 15°：up=%+v", v.up)
			}
		}},
	}
	for _, c := range cases {
		_, out := build(t, emptyScene(t), `{"objects":[{"op":"add","ref":"主角","kind":"character"}],
			"camera":{"target":["主角"],"shotSize":"medium","angle":"`+c.angle+`","side":"front_left"}}`)
		look := vec3f{0, 0.75 * 1.8, 0}
		c.check(t, outputView(t, out), look)
		assertFraming(t, out, []string{"主角"}, "medium")
	}
}

func TestSolve_无关系对象放场景中心(t *testing.T) {
	// 已有可见对象的包围盒中心是 (1, 0, -1)；隐藏的对象与机位不算。
	scene := sceneOf(t, map[string]any{"schemaVersion": 1, "objects": []any{
		map[string]any{"id": "obj_a", "name": "柱子甲", "kind": KindProp, "transform": transform(vec3(-3, 0, -5), vec3(0, 0, 0), vec3(1, 1, 1)),
			"prop": map[string]any{"shape": PropCylinder, "size": vec3(0.3, 3, 0.3)}},
		map[string]any{"id": "obj_b", "name": "柱子乙", "kind": KindProp, "transform": transform(vec3(5, 0, 3), vec3(0, 0, 0), vec3(1, 1, 1)),
			"prop": map[string]any{"shape": PropCylinder, "size": vec3(0.3, 3, 0.3)}},
		map[string]any{"id": "obj_h", "name": "隐藏的", "kind": KindProp, "visible": false, "transform": transform(vec3(50, 0, 50), vec3(0, 0, 0), vec3(1, 1, 1)),
			"prop": map[string]any{"shape": PropBox, "size": vec3(1, 1, 1)}},
		map[string]any{"id": "obj_c", "name": "远处机位", "kind": KindCamera, "transform": transform(vec3(-40, 2, 40), vec3(0, 0, 0), vec3(1, 1, 1)),
			"camera": map[string]any{"focalLengthMm": 35, "aspect": "16:9"}},
	}})
	p, out := build(t, scene, `{"objects":[{"op":"add","ref":"甲","kind":"character"},{"op":"add","ref":"乙","kind":"character"}],"camera":{"side":"front"}}`)
	if a := posOf(byName(t, out, "甲")); math.Abs(a.x-1) > 1e-9 || math.Abs(a.z+1) > 1e-9 {
		t.Errorf("第一个没有关系的新对象应放在包围盒中心 (1, -1)：%+v", a)
	}
	b := posOf(byName(t, out, "乙"))
	if math.Abs(b.horizontalDist(vec3f{1, 0, -1})-defaultSideDistance) > 1e-9 {
		t.Errorf("第二个排在中心旁边 %.1f 米：%+v", defaultSideDistance, b)
	}
	if !hasNote(p, "「乙」没有给位置关系，排在场景中心旁边") {
		t.Errorf("要说明第二个是怎么摆的：%v", p.Defaults)
	}
	// 空场景取原点。
	_, out = build(t, emptyScene(t), `{"objects":[{"op":"add","ref":"独","kind":"character"}]}`)
	if a := posOf(byName(t, out, "独")); a.x != 0 || a.z != 0 {
		t.Errorf("空场景放在原点：%+v", a)
	}
}

func TestSolve_找不到目标按中心并说明(t *testing.T) {
	p, out := build(t, emptyScene(t), `{"objects":[
		{"op":"add","ref":"甲","kind":"character","relation":{"type":"behind","target":"查无此人"}},
		{"op":"add","ref":"乙","kind":"character","relation":{"type":"left_of","target":"camera"}},
		{"op":"add","ref":"丙","kind":"character","relation":{"type":"right_of","target":"丙"}},
		{"op":"add","ref":"丁","kind":"character","facing":{"target":"不存在"}}],
		"camera":{"target":["鬼影"],"shotSize":"full"}}`)
	for _, want := range []string{
		"「甲」的关系目标「查无此人」找不到，按场景中心处理",
		"「乙」的关系目标「camera」找不到，按场景中心处理",
		"「丙」的关系目标「丙」找不到，按场景中心处理",
		"「丁」的朝向目标「不存在」找不到，按默认朝向",
		"机位要拍的「鬼影」找不到，已忽略",
	} {
		if !hasNote(p, want) {
			t.Errorf("说明里应有「%s」：\n%s", want, strings.Join(p.Defaults, "\n"))
		}
	}
	if a := posOf(byName(t, out, "甲")); a.x != 0 || a.z != 0 {
		t.Errorf("找不到目标的按场景中心（第一个）：%+v", a)
	}
	assertMinGap(t, out, minObjectGap)
}

func TestSolve_互相依赖按中心处理(t *testing.T) {
	p, out := build(t, emptyScene(t), `{"objects":[
		{"op":"add","ref":"甲","kind":"character","relation":{"type":"left_of","target":"乙"}},
		{"op":"add","ref":"乙","kind":"character","relation":{"type":"right_of","target":"甲"}}],
		"camera":{"shotSize":"full"}}`)
	if !hasNote(p, "互相依赖") {
		t.Errorf("要说明打破了循环：%v", p.Defaults)
	}
	assertLeftOf(t, out, "甲", "乙")
}

func TestSolve_重名与已有对象(t *testing.T) {
	scene := sceneOf(t, map[string]any{"schemaVersion": 1, "objects": []any{
		map[string]any{"id": "obj_u", "name": "老王", "kind": KindCharacter, "transform": transform(vec3(0, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
			"character": map[string]any{"model": ModelAdultMale, "height": 1.8, "body": "standard"}},
		map[string]any{"id": "obj_ai", "name": "小李", "kind": KindCharacter, "source": SourceAI, "transform": transform(vec3(2, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
			"character": map[string]any{"model": ModelAdultMale, "height": 1.8, "body": "standard", "posePreset": "walk"}},
	}})
	p, out := build(t, scene, `{"objects":[
		{"op":"add","ref":"老王","kind":"character","posePreset":"bow"},
		{"op":"add","ref":"小李","kind":"character","posePreset":"wave"},
		{"op":"add","ref":"阿花","kind":"character"},
		{"op":"add","ref":"阿花","kind":"character"},
		{"ref":"obj_ai","posePreset":"cheer"}]}`)
	if !hasNote(p, "场景里已有「老王」（obj_u），是用户摆的，不能重新摆放") {
		t.Errorf("与用户对象重名的新增不能顶替它：%v", p.Defaults)
	}
	if n := countKind(out, KindCharacter); n != 4 {
		t.Errorf("老王、小李各一个，阿花两个：实际 %d 个角色", n)
	}
	ch, _ := byName(t, out, "小李").Raw()["character"].(map[string]any)
	if ch["posePreset"] != "cheer" || !contains(p.Changes.Updated, "obj_ai") {
		t.Errorf("与可替换对象重名的新增按修改处理（后一条没写 op 的修改也作用于它）：%v", ch["posePreset"])
	}
	byName(t, out, "阿花 2")
	if !hasNote(p, "方案里有两个「阿花」") || !hasNote(p, "「obj_ai」没有写 op，按修改处理") {
		t.Errorf("重名与推断都要说明：%v", p.Defaults)
	}
}

func TestSolve_删除输出机位改用剩下的机位(t *testing.T) {
	scene := sceneOf(t, map[string]any{"schemaVersion": 1, "outputCameraId": "obj_ai_cam", "objects": []any{
		map[string]any{"id": "obj_ai_cam", "name": "AI 机位", "kind": KindCamera, "source": SourceAI,
			"transform": transform(vec3(0, 1.5, 3), vec3(0, 0, 0), vec3(1, 1, 1)),
			"camera":    map[string]any{"focalLengthMm": 35, "aspect": "16:9"}},
		map[string]any{"id": "obj_user_cam", "name": "用户机位", "kind": KindCamera,
			"transform": transform(vec3(0, 1.5, 5), vec3(0, 0, 0), vec3(1, 1, 1)),
			"camera":    map[string]any{"focalLengthMm": 50, "aspect": "16:9"}},
	}})
	p, out := build(t, scene, `{"objects":[{"op":"remove","ref":"obj_ai_cam"},{"op":"add","ref":"甲","kind":"character"}]}`)
	if out.OutputCameraID() != "obj_user_cam" || !contains(p.Changes.Removed, "obj_ai_cam") {
		t.Fatalf("删掉输出机位后改用剩下的第一个：%q %+v", out.OutputCameraID(), p.Changes)
	}
	if !hasNote(p, "删除了输出机位，改用「用户机位」作为输出机位") {
		t.Errorf("要说明：%v", p.Defaults)
	}
}

func TestSolve_不改入参场景(t *testing.T) {
	raw := readFixture(t, "build", "case-rebuild-keep.json")
	var c buildCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	scene := parseScene(t, c.Scene)
	before, _ := json.Marshal(scene.Tree())
	build(t, scene, string(c.Draft))
	after, _ := json.Marshal(scene.Tree())
	if string(before) != string(after) {
		t.Fatal("求解与应用不能改入参的场景（调用方可能拿它重试）")
	}
}

func TestSolve_ID重复时报错(t *testing.T) {
	scene := parseScene(t, DefaultScene())
	d, dropped, err := ParseDraft(`{"objects":[{"op":"add","ref":"甲","kind":"character"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Propose(scene, d, dropped, SolveOptions{NewID: func() string { return DefaultCameraID }})
	if err == nil || !strings.Contains(err.Error(), "生成新对象 ID 失败") {
		t.Fatalf("新 ID 一直与已有对象重复时要报错，不能覆盖已有对象：%v", err)
	}
}

func TestNewObjectID_形状(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewObjectID()
		if !strings.HasPrefix(id, "obj_") || len(id) != 16 || seen[id] {
			t.Fatalf("ID 应为 obj_ + 12 位随机串且不重复：%q", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------- 视角与欧拉角

// TestVerticalFOV_横幅与竖幅：传感器尺寸是长边（DRT-11 的共享 fixture camera/fov.json 落地后一并核对）。
func TestVerticalFOV_横幅与竖幅(t *testing.T) {
	cases := []struct {
		focal, sensor float64
		aspect        string
		vfovDeg       float64
	}{
		{35, 36, "16:9", 32.26880217111643},
		{35, 36, "9:16", 54.43222311461495},
		{50, 36, "1:1", 39.597752709049864},
		{24, 36, "21:9", 35.637777829045554},
		{85, 36, "3:4", 23.913168486298265},
		{200, 70, "4:3", 14.9546606133216},
	}
	for _, c := range cases {
		got, err := verticalFOV(c.focal, c.sensor, c.aspect)
		if err != nil || math.Abs(deg(got)-c.vfovDeg) > 1e-6 {
			t.Errorf("%vmm / %vmm / %s：应为 %.6f°，实际 %.6f°（%v）", c.focal, c.sensor, c.aspect, c.vfovDeg, deg(got), err)
		}
	}
	if _, err := verticalFOV(35, 36, "5:4"); err == nil {
		t.Error("不认识的画幅要报错，不猜")
	}

	raw, err := os.ReadFile(fixturePath("camera", "fov.json"))
	if errors.Is(err, os.ErrNotExist) {
		t.Log("camera/fov.json 尚未落地（DRT-11），只核对内置用例")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	type fovCase struct {
		FocalMm  float64 `json:"focalMm"`
		SensorMm float64 `json:"sensorMm"`
		Aspect   string  `json:"aspect"`
		VfovDeg  float64 `json:"vfovDeg"`
	}
	var shared []fovCase
	if err := json.Unmarshal(raw, &shared); err != nil {
		var wrapped struct {
			Cases []fovCase `json:"cases"`
		}
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			t.Fatalf("camera/fov.json 的形状与约定不符（[{focalMm, sensorMm, aspect, vfovDeg}]）：%v", err)
		}
		shared = wrapped.Cases
	}
	if len(shared) == 0 {
		t.Fatal("camera/fov.json 没有用例")
	}
	for _, c := range shared {
		if c.FocalMm <= 0 || c.SensorMm <= 0 || c.VfovDeg <= 0 {
			t.Fatalf("camera/fov.json 的字段名与约定不符：%+v", c)
		}
		got, err := verticalFOV(c.FocalMm, c.SensorMm, c.Aspect)
		if err != nil || math.Abs(deg(got)-c.VfovDeg) > 1e-6 {
			t.Errorf("共享用例 %+v：Go 算出 %.6f°（%v），与前端不一致", c, deg(got), err)
		}
	}
}

// TestEulerXYZ_与three一致：矩阵按 three.js `makeRotationFromEuler`（顺序 XYZ）逐项展开另算一遍。
func TestEulerXYZ_与three一致(t *testing.T) {
	x, y, z := rad(20), rad(-35), rad(50)
	a, b, c, d, e, f := math.Cos(x), math.Sin(x), math.Cos(y), math.Sin(y), math.Cos(z), math.Sin(z)
	ae, af, be, bf := a*e, a*f, b*e, b*f
	want := mat3{
		{c * e, -c * f, d},
		{af + be*d, ae - bf*d, -b * c},
		{bf - ae*d, be + af*d, a * c},
	}
	got := eulerXYZMatrix(20, -35, 50)
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if math.Abs(got[i][j]-want[i][j]) > 1e-12 {
				t.Fatalf("m[%d][%d] = %v，three.js 为 %v", i, j, got[i][j], want[i][j])
			}
		}
	}
	rx, ry, rz := eulerXYZFromMatrix(got)
	if math.Abs(rx-20) > 1e-9 || math.Abs(ry+35) > 1e-9 || math.Abs(rz-50) > 1e-9 {
		t.Errorf("分解回来应为 (20, -35, 50)，实际 (%v, %v, %v)", rx, ry, rz)
	}
	// 偏航 + 俯仰换成 XYZ 欧拉角之后，视线方向不变。
	for _, yaw := range []float64{0, 45, 90, 135, 180, -60} {
		for _, pitch := range []float64{-85, -30, 0, 20, 45} {
			ex, ey, ez := cameraEuler(yaw, pitch)
			_, _, fwd := cameraBasis(vec3f{ex, ey, ez}, 0)
			want := rotY(rad(yaw)).mul(rotX(rad(pitch))).mulVec(vec3f{0, 0, -1})
			if angleBetween(fwd, want) > 1e-6 {
				t.Errorf("偏航 %v 俯仰 %v：视线方向不一致", yaw, pitch)
			}
			if r, _, _ := cameraBasis(vec3f{ex, ey, ez}, 0); math.Abs(r.y) > 1e-9 {
				t.Errorf("偏航 %v 俯仰 %v：不横滚时右向量应水平：%+v", yaw, pitch, r)
			}
		}
	}
}
