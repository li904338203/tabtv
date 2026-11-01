package domain

import (
	"crypto/rand"
	"fmt"
	"math"
)

// 求解器（dev/01 §4.4，DRD-27）：把草案里的「谁在谁左边、相距多远、怎么拍」换成坐标。
//
// ⚠️ 坐标永远由这里给：模型直接写坐标会重叠、悬空，而且左右容易镜像（01 §1.9、§3.7）。
// ⚠️ 「左右」「前后」都在**输出机位的屏幕坐标系**里算（DR-F-104）：屏幕右 = 机位的水平右向量，
// 不写死世界轴 —— 机位转过方位角之后，世界 −X 就不再是画面左边了。
//
// 顺序（不能换）：
//  1. 机位方向先定：有 camera 草案时按 side 定方位（相对角色群的正面）；没有时沿用输出机位的方向；
//  2. 按依赖顺序放对象：没有关系约束的第一个新对象放在场景中心，其余按关系槽位；
//  3. 避让：两两水平中心距不小于 0.5 m，沿关系方向推开，最多 10 次；
//  4. 贴地：新摆的对象底面贴到 ground.height；
//  5. 机位按景别与焦距反推距离，按角度定高度与俯仰；
//  6. 朝向最后算（「面向机位」要等机位位置定了）。
//
// 只碰可替换对象（source=ai 且未锁定，DR-F-105）；其余对象一个字节都不动。
// 文件分工：草案操作在 solve_ops.go，摆放与朝向在 solve_place.go，机位在 solve_camera.go。

// 求解常量（dev/01 §4.4）。
const (
	defaultNearDistance  = 1.0
	defaultSideDistance  = 1.2 // left_of / right_of，以及 facing（面对面站在目标左右）
	defaultDepthDistance = 1.5 // in_front_of / behind
	characterRadius      = 0.3
	minObjectGap         = 0.5
	maxPushes            = 10
	pushMargin           = 0.01
	defaultFocalMm       = 35.0
	defaultSensorMm      = 36.0
	defaultAspect        = "16:9"
	defaultNear          = 0.1
	defaultFar           = 1000.0
	lowCameraHeight      = 0.4
	minLowCameraHeight   = 0.05
	maxLowElevationDeg   = 45.0
	highElevationDeg     = 30.0
	topElevationDeg      = 85.0
	dutchRollDeg         = 15.0
	minTargetHeight      = 0.1
	// fallbackTargetHeight 是场景里没有任何可拍对象时的虚拟目标高度（一个成年人）。
	fallbackTargetHeight = 1.8
)

// 默认景别、角度、方位（DR-F-106：信息不够时按默认值摆，并说明）。
const (
	DefaultShotSize = "medium"
	DefaultAngle    = "eye_level"
	DefaultSide     = "front"
)

// lookAtRatio 是注视点高度占目标高度的比例：全景及更远看中间，中全景与中景看胸口，近景及更近看脸。
var lookAtRatio = map[string]float64{
	"extreme_long": 0.5, "long": 0.5, "full": 0.5,
	"medium_long": 0.75, "medium": 0.75,
	"medium_close": 0.9, "close_up": 0.9, "extreme_close_up": 0.9,
}

// SolveOptions 是求解的可注入项。
type SolveOptions struct {
	// NewID 生成新对象 ID；为空时用 NewObjectID。测试注入固定序列。
	NewID func() string
}

// Resolved 是求解结果：只描述变化，由 ApplyProposal 合并进场景。
type Resolved struct {
	// Added 是完整的新对象（已分配 ID，source=ai），按草案顺序，新建的机位在最后。
	Added []map[string]any
	// Updated 是修改后的完整对象：从原对象深拷贝后改，未知字段原样保留。只含真的变了的。
	Updated []map[string]any
	// Removed 是要删除的对象 ID（只会是可替换对象）。
	Removed []string
	// OutputCamera 非 nil 时改输出机位；指向空串表示置 null（一个机位都不剩）。
	OutputCamera *string
	// Defaults 是给用户看的说明：用了哪些默认值、哪些操作被忽略以及为什么。
	Defaults []string
}

// NewObjectID 生成 `obj_` + 12 位随机串（与前端 directorNewObjectId 同形）。
func NewObjectID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 在受支持的平台上不会失败；失败就是环境坏了，不能退回到可预测的 ID。
		panic(fmt.Sprintf("生成对象 ID 失败：%v", err))
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "obj_" + string(b)
}

// Replaceable 判断对象能不能被 AI 重新搭景替换：source=ai 且未锁定（DR-F-105）。
// 用户改过的 AI 对象由前端在改动时把 source 改写为 user（DRT-14），这里只看字段。
func Replaceable(o Object) bool { return o.Source() == SourceAI && !o.Locked() }

// entity 是求解过程中的一个对象（已有的或新建的）。
type entity struct {
	id, name, kind string
	// ref 是草案里指代它用的名字（新建对象）；已有对象靠 id 与 name 匹配。
	ref         string
	pos         vec3f
	yaw         float64
	radius      float64
	height      float64
	visible     bool
	existing    bool
	replaceable bool
	removed     bool
	// mover 为真时位置由求解器给（新建对象，或带了位置关系的修改）。
	mover  bool
	placed bool
	// obj 是新对象的 map，或已有对象的深拷贝（只有被修改时才拷贝）。
	obj      map[string]any
	original map[string]any
	draft    *DraftObject
	updated  bool
}

type solver struct {
	scene    Scene
	opt      SolveOptions
	ground   float64
	entities []*entity
	added    []*entity
	defaults []string
	seenNote map[string]bool
	ids      map[string]bool

	// 屏幕坐标系：right 是输出机位的水平右向量，toCamera 是从场景指向机位的水平方向。
	right, toCamera vec3f
	front           vec3f
	anchor          vec3f
	freeSlots       int

	usedDefaultDistance bool
	defaultModelUsed    bool
	defaultPoseUsed     bool

	camSpec     *DraftCamera
	camRef      *entity // 当前的输出机位（没有 camera 草案时，屏幕坐标系以它为准）
	camEntity   *entity // 本次写入的机位（就地修改的 AI 机位，或新建的机位）
	camBase     *entity // 草案没写的机位参数沿用谁（被改的机位本身，或新建时的当前输出机位）
	outputCamID string
	outputSet   bool
}

// Solve 把草案求解成具体的变化（坐标、朝向、机位）。
func Solve(scene Scene, d Draft, opt SolveOptions) (Resolved, error) {
	if opt.NewID == nil {
		opt.NewID = NewObjectID
	}
	s := &solver{scene: scene, opt: opt, seenNote: map[string]bool{}, ids: map[string]bool{}}
	s.ground = groundHeight(scene)
	s.load()
	if err := s.applyOps(d.Objects); err != nil {
		return Resolved{}, err
	}
	if err := s.chooseCamera(d.Camera); err != nil {
		return Resolved{}, err
	}
	s.place()
	if err := s.placeCamera(); err != nil {
		return Resolved{}, err
	}
	s.orient()
	s.aggregateNotes()
	return s.result(), nil
}

func (s *solver) note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !s.seenNote[msg] {
		s.seenNote[msg] = true
		s.defaults = append(s.defaults, msg)
	}
}

func groundHeight(scene Scene) float64 {
	g, _ := scene.Tree()["ground"].(map[string]any)
	h, _ := g["height"].(float64)
	return h
}

// ---------------------------------------------------------------- 已有对象

func (s *solver) load() {
	for _, o := range s.scene.Objects() {
		e := &entity{
			id: o.ID(), name: o.Name(), kind: o.Kind(), visible: o.Visible(),
			existing: true, replaceable: Replaceable(o), original: o.Raw(),
		}
		e.pos, e.yaw = transformOf(o.Raw())
		e.radius, e.height = footprint(o.Raw())
		s.entities = append(s.entities, e)
		s.ids[e.id] = true
	}
}

// transformOf 取位置与绕 Y 的朝向（度）。
func transformOf(m map[string]any) (vec3f, float64) {
	t, _ := m["transform"].(map[string]any)
	p := vecOf(t["position"])
	r := vecOf(t["rotation"])
	return p, r.y
}

func vecOf(v any) vec3f {
	m, _ := v.(map[string]any)
	x, _ := m["x"].(float64)
	y, _ := m["y"].(float64)
	z, _ := m["z"].(float64)
	return vec3f{x, y, z}
}

func scaleOf(m map[string]any) vec3f {
	t, _ := m["transform"].(map[string]any)
	sc, ok := t["scale"].(map[string]any)
	if !ok {
		return vec3f{1, 1, 1}
	}
	out := vecOf(sc)
	if out.x == 0 {
		out.x = 1
	}
	if out.y == 0 {
		out.y = 1
	}
	if out.z == 0 {
		out.z = 1
	}
	return out
}

// footprint 给出对象的水平半径与高度（米）：避让用半径，机位反推用高度。
func footprint(m map[string]any) (radius, height float64) {
	sc := scaleOf(m)
	horiz := math.Max(math.Abs(sc.x), math.Abs(sc.z))
	switch str(m["kind"]) {
	case KindCharacter:
		ch, _ := m["character"].(map[string]any)
		h, _ := ch["height"].(float64)
		if h <= 0 {
			if model, ok := ModelOf(str(ch["model"])); ok {
				h = model.HeightM
			} else {
				h = fallbackTargetHeight
			}
		}
		return characterRadius * horiz, h * math.Abs(sc.y)
	case KindCrowd:
		cr, _ := m["crowd"].(map[string]any)
		rows, _ := cr["rows"].(float64)
		cols, _ := cr["cols"].(float64)
		spacing, _ := cr["spacing"].(float64)
		h := fallbackTargetHeight
		if model, ok := ModelOf(str(cr["model"])); ok {
			h = model.HeightM
		}
		return crowdRadius(int(rows), int(cols), spacing) * horiz, h * math.Abs(sc.y)
	case KindProp:
		pr, _ := m["prop"].(map[string]any)
		size := vecOf(pr["size"])
		return 0.5 * math.Hypot(size.x*sc.x, size.z*sc.z), math.Abs(size.y * sc.y)
	}
	return 0, 0
}

// crowdRadius 是群众阵列（以中心为锚点的 rows × cols 网格）的外接半径，含边上一个人的半径。
func crowdRadius(rows, cols int, spacing float64) float64 {
	if rows < 1 {
		rows = 1
	}
	if cols < 1 {
		cols = 1
	}
	return 0.5*math.Hypot(float64(cols-1)*spacing, float64(rows-1)*spacing) + characterRadius
}

// ---------------------------------------------------------------- 结果

func (s *solver) aggregateNotes() {
	if s.usedDefaultDistance {
		s.note("没写距离的位置关系按默认距离（靠近 %s 米，左右与面对面 %s 米，前后 %s 米）",
			fmtNum(defaultNearDistance), fmtNum(defaultSideDistance), fmtNum(defaultDepthDistance))
	}
	if s.defaultModelUsed {
		model, _ := ModelOf(ModelAdultMale)
		s.note("没写素模的角色与群众按%s", model.Name)
	}
	if s.defaultPoseUsed {
		s.note("没写姿势的角色与群众按%s", poseName("stand_neutral"))
	}
}

func (s *solver) result() Resolved {
	r := Resolved{Defaults: s.defaults}
	for _, e := range s.added {
		r.Added = append(r.Added, e.obj)
	}
	for _, e := range s.entities {
		switch {
		case !e.existing:
		case e.removed:
			r.Removed = append(r.Removed, e.id)
		case e.updated && e.obj != nil && !jsonEqual(e.obj, e.original):
			r.Updated = append(r.Updated, e.obj)
		}
	}
	if s.outputSet {
		id := s.outputCamID
		r.OutputCamera = &id
	}
	return r
}

// ---------------------------------------------------------------- 工具

func subMap(m map[string]any, key string) map[string]any {
	sub, ok := m[key].(map[string]any)
	if !ok {
		sub = map[string]any{}
		m[key] = sub
	}
	return sub
}

func intOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

func coverageOf(code string) float64 {
	for _, s := range shotSizes {
		if s.Code == code {
			return s.Coverage
		}
	}
	return coverageOf(DefaultShotSize)
}

func shotSizeName(code string) string {
	for _, s := range shotSizes {
		if s.Code == code {
			return s.Name
		}
	}
	return code
}

func labelOf(list []Labeled, code string) string {
	for _, l := range list {
		if l.Code == code {
			return l.Name
		}
	}
	return code
}

func poseName(code string) string {
	for _, p := range poses {
		if p.Code == code {
			return p.Name
		}
	}
	return code
}
