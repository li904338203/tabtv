package domain

// 目录常量（DRD-29）：素模、姿势码、道具形状、景别、角度、方位、关系、画幅、上限与默认值。
//
// ⚠️ 这些码是**前后端契约**：求解器与 AI 搭景的提示词读这里，前端读
// `api/fixtures/director/catalog.json`（前端的姿势骨骼数据在 `public/director/poses.json`）。
// 两边各持一份是必然的 —— Go 不能 embed 模块外的文件 —— 所以由 `TestCatalog_与共享fixture一致`
// 逐项比对：只改一边的话，表现是「AI 用了一个前端不认识的姿势」，而不报错。
// 改这里时同时改 fixture 与前端的 `utils/directorCatalog.ts`（DRT-11）。

// Model 是一种内置素模（DRK-2：统一素模外观，只有体型不同）。
type Model struct {
	Code    string  `json:"code"`
	Name    string  `json:"name"`
	HeightM float64 `json:"heightM"`
}

// Labeled 是「码 + 中文名」的一项（体型、姿势类别、角度、方位）。
type Labeled struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Pose 是一个姿势预设。Description 给模型看：只写身体动作，不写情绪。
type Pose struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Description string `json:"description"`
}

// ShotSize 是一档景别。Coverage = 画面高 ÷ 目标高（dev/01 §4.4）。
//
// ⚠️ 8 档是**一张表**：求解器按它反推机位距离，前端的镜头文字按相邻两档的几何平均反查（dev/03 §12）。
// 只改一边的话，「AI 按特写摆的机位，镜头文字却说近景」，而两边各自看都没错。
type ShotSize struct {
	Code     string  `json:"code"`
	Name     string  `json:"name"`
	Coverage float64 `json:"coverage"`
}

// Aspect 是一种画幅及其截图像素（DR-PARAM-08，设计建议值）。
type Aspect struct {
	Code   string `json:"code"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// Limits 是场景上限（DR-PARAM-09，设计建议值）。
type Limits struct {
	MaxObjects           int `json:"maxObjects"`
	MaxCrowdSize         int `json:"maxCrowdSize"`
	MaxCrowdRows         int `json:"maxCrowdRows"`
	MaxCrowdCols         int `json:"maxCrowdCols"`
	MaxKeyframesPerTrack int `json:"maxKeyframesPerTrack"`
	MaxKeyframes         int `json:"maxKeyframes"`
	MaxBoneOverrides     int `json:"maxBoneOverrides"`
	MaxSceneBytes        int `json:"maxSceneBytes"`
	MaxNameChars         int `json:"maxNameChars"`
	MaxIDChars           int `json:"maxIdChars"`
	MaxCoord             int `json:"maxCoord"`
}

// Catalog 是 `api/fixtures/director/catalog.json` 的 Go 形状。
type Catalog struct {
	SchemaVersion  int            `json:"schemaVersion"`
	Models         []Model        `json:"models"`
	Bodies         []Labeled      `json:"bodies"`
	PoseCategories []Labeled      `json:"poseCategories"`
	Poses          []Pose         `json:"poses"`
	PropShapes     []string       `json:"propShapes"`
	PropColors     []string       `json:"propColors"`
	ShotSizes      []ShotSize     `json:"shotSizes"`
	Angles         []Labeled      `json:"angles"`
	Sides          []Labeled      `json:"sides"`
	Relations      []string       `json:"relations"`
	Aspects        []Aspect       `json:"aspects"`
	Limits         Limits         `json:"limits"`
	Defaults       map[string]any `json:"defaults"`
}

// 上限（DR-PARAM-09）。前端同值，由 fixture 绊线保证一致。
const (
	MaxObjects           = 200
	MaxCrowdSize         = 100
	MaxCrowdRows         = 10
	MaxCrowdCols         = 10
	MaxKeyframesPerTrack = 120
	MaxKeyframes         = 2000
	MaxBoneOverrides     = 70
	// MaxSceneBytes 是场景正文序列化后的上限；超出由调用方映射为 41301。
	MaxSceneBytes = 512 << 10
	MaxNameChars  = 20
	MaxIDChars    = 64
	// MaxCoord 是坐标分量的绝对值上限（米）。超出不是「场景很大」，而是数据坏了。
	MaxCoord = 10000
	// maxRotation 是旋转分量的绝对值上限（度）：允许累积转了很多圈的值，但挡住坏数据。
	maxRotation = 36000
	// maxBoneRotation 是骨骼覆盖每个分量的上限（度）。
	maxBoneRotation = 360
	// maxPoseCodeChars 是姿势码的长度上限。
	maxPoseCodeChars = 64
)

// 素模码（DRK-2）。身高是模型的原始身高（shot-generator-models README）。
const (
	ModelAdultMale   = "adult_male"
	ModelAdultFemale = "adult_female"
	ModelYouthMale   = "youth_male"
	ModelYouthFemale = "youth_female"
)

var models = []Model{
	{ModelAdultMale, "成年男", 1.8},
	{ModelAdultFemale, "成年女", 1.625},
	{ModelYouthMale, "少年男", 1.6},
	{ModelYouthFemale, "少年女", 1.6},
}

var bodies = []Labeled{
	{"slim", "偏瘦"}, {"standard", "标准"}, {"stocky", "偏壮"},
}

var poseCategories = []Labeled{
	{"stand", "站立"}, {"move", "行走奔跑"}, {"rest", "坐卧"},
	{"gesture", "手势交流"}, {"action", "动作打斗"}, {"interact", "互动"},
}

// poses 是首批姿势（DR-PARAM-15：不少于 30 个，覆盖六类）。骨骼数据在前端 `poses.json`（DRT-10）。
//
// ⚠️ 说明只写身体动作，不写情绪：AI 搭景按说明挑姿势，「愤怒地」这种词只会让它挑不准。
var poses = []Pose{
	{"stand_neutral", "自然站立", "stand", "双脚与肩同宽站直，双臂自然下垂"},
	{"stand_arms_crossed", "双臂交叉", "stand", "站立，双臂在胸前交叉抱住"},
	{"stand_hands_on_hips", "叉腰", "stand", "站立，双手叉在腰间，手肘向外"},
	{"stand_hands_in_pockets", "插兜", "stand", "站立，双手插在裤兜里，肩膀放松"},
	{"stand_hands_behind", "背手", "stand", "站直，双手背在身后相握"},
	{"stand_look_back", "回头", "stand", "站立，上身与头向一侧后方扭转回望"},
	{"walk", "行走", "move", "正常步速行走，一腿迈出，双臂前后自然摆动"},
	{"walk_fast", "快走", "move", "步子更大的快步走，上身略前倾，摆臂明显"},
	{"run", "奔跑", "move", "跑步，一腿蹬地一腿屈膝前提，双臂屈肘摆动"},
	{"sprint", "冲刺", "move", "全力冲刺，上身大幅前倾，大步蹬地，用力摆臂"},
	{"jump", "起跳", "move", "双脚离地向上跳起，双臂上扬，膝盖微屈"},
	{"sneak", "蹑手蹑脚", "move", "弯腰屈膝，小步前移，双手在身前微抬"},
	{"sit_chair", "坐椅子", "rest", "坐在椅子上，大腿水平，小腿垂直，双手放在膝上"},
	{"sit_floor_cross", "盘腿坐地", "rest", "盘腿坐在地上，上身直立，双手搭在膝上"},
	{"squat", "蹲下", "rest", "双脚着地深蹲，膝盖弯曲，双臂搭在膝上"},
	{"kneel_one", "单膝跪地", "rest", "一膝着地一脚踏地，上身直立"},
	{"lie_back", "仰躺", "rest", "身体平躺在地，面朝上，双臂放在身体两侧"},
	{"lie_side", "侧躺", "rest", "侧身躺在地上，一臂枕在头下，双腿微屈"},
	{"wave", "挥手", "gesture", "站立，一只手臂举过肩向外挥动"},
	{"point_forward", "指向前方", "gesture", "站立，一只手臂向前伸直，食指指向前方"},
	{"phone_call", "打电话", "gesture", "站立，一只手把手机贴在耳边，另一只手自然下垂"},
	{"thinking", "托腮思考", "gesture", "站立，一手抱胸，另一手托住下巴，头微低"},
	{"cheer", "双手举起欢呼", "gesture", "站立，双臂高举过头张开"},
	{"bow", "鞠躬", "gesture", "双脚并拢，上身向前弯约四十五度，双手贴在身侧"},
	{"fight_stance", "格斗架势", "action", "两脚前后分开微屈膝，双拳举在下巴前"},
	{"punch", "出拳", "action", "前腿弓步，一臂向前直拳打出，另一拳收在下巴前"},
	{"kick_side", "侧踢", "action", "单腿支撑，另一腿向侧面伸直踢出，上身侧倾"},
	{"flying_kick", "飞身踢", "action", "腾空，一腿向前伸直踢出，另一腿屈膝收起"},
	{"block", "格挡", "action", "前臂竖起挡在头部与身体前方，膝盖微屈"},
	{"fall_back", "后仰倒地", "action", "身体向后仰倒，双臂向两侧张开，单脚离地"},
	{"handshake", "握手", "interact", "站立，右手向前伸出到腰高，准备与对方握手"},
	{"hug", "拥抱", "interact", "站立，双臂向前张开环抱，上身微前倾"},
	{"hold_hands", "牵手", "interact", "站立，一只手向侧下方伸出，牵住身旁的人"},
	{"push", "推", "interact", "弓步向前，双臂向前伸直推出"},
	{"hand_over", "递东西", "interact", "站立，双手在胸前向前递出一件物品"},
	{"carry_box", "搬箱子", "interact", "站立，双手在身前托住一个箱子，手肘弯曲"},
}

// 道具形状（DR-F-51）。
const (
	PropBox      = "box"
	PropCylinder = "cylinder"
	PropSphere   = "sphere"
	PropPlane    = "plane"
)

var propShapes = []string{PropBox, PropCylinder, PropSphere, PropPlane}

// propColors 是道具的灰阶色板（8 档，DR-F-51 / 52：道具一律白模外观）。校验不限定在色板内。
var propColors = []string{
	"#f1f3f4", "#dadce0", "#bdc1c6", "#9aa0a6", "#80868b", "#5f6368", "#3c4043", "#202124",
}

var shotSizes = []ShotSize{
	{"extreme_long", "大远景", 6.0},
	{"long", "远景", 3.0},
	{"full", "全景", 1.25},
	{"medium_long", "中全景", 0.85},
	{"medium", "中景", 0.6},
	{"medium_close", "近景", 0.4},
	{"close_up", "特写", 0.25},
	{"extreme_close_up", "大特写", 0.12},
}

var angles = []Labeled{
	{"eye_level", "平视"}, {"low", "低机位仰拍"}, {"high", "俯拍"}, {"top", "顶视"}, {"dutch", "斜角"},
}

var sides = []Labeled{
	{"front", "正面"}, {"front_left", "左前"}, {"front_right", "右前"},
	{"left", "左侧"}, {"right", "右侧"}, {"back", "背面"},
}

// relations 是 AI 搭景草案里的关系类型（dev/01 §4.3）。左右按**观看者**算（DRD-27）。
var relations = []string{"left_of", "right_of", "in_front_of", "behind", "near", "facing", "center"}

var aspects = []Aspect{
	{"16:9", 1280, 720},
	{"9:16", 720, 1280},
	{"1:1", 1024, 1024},
	{"4:3", 1152, 864},
	{"3:4", 864, 1152},
	{"21:9", 1680, 720},
}

// BuildCatalog 返回完整目录。返回的是副本，调用方改它不影响包内常量。
func BuildCatalog() Catalog {
	return Catalog{
		SchemaVersion:  CurrentSchemaVersion,
		Models:         append([]Model(nil), models...),
		Bodies:         append([]Labeled(nil), bodies...),
		PoseCategories: append([]Labeled(nil), poseCategories...),
		Poses:          append([]Pose(nil), poses...),
		PropShapes:     append([]string(nil), propShapes...),
		PropColors:     append([]string(nil), propColors...),
		ShotSizes:      append([]ShotSize(nil), shotSizes...),
		Angles:         append([]Labeled(nil), angles...),
		Sides:          append([]Labeled(nil), sides...),
		Relations:      append([]string(nil), relations...),
		Aspects:        append([]Aspect(nil), aspects...),
		Limits: Limits{
			MaxObjects: MaxObjects, MaxCrowdSize: MaxCrowdSize, MaxCrowdRows: MaxCrowdRows,
			MaxCrowdCols: MaxCrowdCols, MaxKeyframesPerTrack: MaxKeyframesPerTrack,
			MaxKeyframes: MaxKeyframes, MaxBoneOverrides: MaxBoneOverrides, MaxSceneBytes: MaxSceneBytes,
			MaxNameChars: MaxNameChars, MaxIDChars: MaxIDChars, MaxCoord: MaxCoord,
		},
		Defaults: catalogDefaults(),
	}
}

// catalogDefaults 是新对象与默认场景各段的默认值。
//
// 角色的默认身高**不写在这里**：取所选模型的 `heightM`（换模型时身高跟着换）。
func catalogDefaults() map[string]any {
	return map[string]any{
		"camera": defaultCameraParams(),
		"character": map[string]any{
			"model": ModelAdultMale, "body": "standard", "posePreset": "stand_neutral",
		},
		"crowd": map[string]any{
			"model": ModelAdultMale, "rows": 3, "cols": 3, "spacing": 1.0,
			"posePreset": "stand_neutral", "jitter": false, "seed": 1,
		},
		"prop": map[string]any{
			"shape": PropBox, "size": vec3(1, 1, 1), "color": "#9aa0a6",
		},
		"scene": map[string]any{
			"stage":      defaultStage(),
			"view":       defaultView(),
			"background": defaultBackground(),
			"ground":     defaultGround(),
			"lights":     defaultLights(),
			"timeline":   defaultTimeline(),
		},
	}
}

// 校验与求解用的查找表。
var (
	modelSet = func() map[string]bool {
		m := map[string]bool{}
		for _, x := range models {
			m[x.Code] = true
		}
		return m
	}()
	bodySet      = labeledSet(bodies)
	propShapeSet = stringSet(propShapes)
	aspectSet    = func() map[string]Aspect {
		m := map[string]Aspect{}
		for _, a := range aspects {
			m[a.Code] = a
		}
		return m
	}()
	poseSet = func() map[string]bool {
		m := map[string]bool{}
		for _, p := range poses {
			m[p.Code] = true
		}
		return m
	}()
)

// ModelOf 取素模；不认识的码返回 false。
func ModelOf(code string) (Model, bool) {
	for _, m := range models {
		if m.Code == code {
			return m, true
		}
	}
	return Model{}, false
}

// AspectOf 取画幅与截图像素；不认识的码返回 false。
func AspectOf(code string) (Aspect, bool) {
	a, ok := aspectSet[code]
	return a, ok
}

// IsKnownPose 判断姿势码是否在目录里。
//
// ⚠️ 校验时**未知姿势码放行**（dev/01 §4.1）：前端的姿势资源可能比后端新；
// 求解器与 AI 搭景的白名单才用这个函数拦。
func IsKnownPose(code string) bool { return poseSet[code] }

func labeledSet(list []Labeled) map[string]bool {
	m := map[string]bool{}
	for _, x := range list {
		m[x.Code] = true
	}
	return m
}

func stringSet(list []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range list {
		m[x] = true
	}
	return m
}
