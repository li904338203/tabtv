package domain

import (
	"math"
)

// 求解器：屏幕坐标系与机位（dev/01 §4.4 第 1、6 步）。

// ---------------------------------------------------------------- 机位方向

// chooseCamera 定屏幕坐标系（第 1 步）：之后的左右前后都按它算。
func (s *solver) chooseCamera(spec *DraftCamera) error {
	out := s.existingByID(s.scene.OutputCameraID())
	if out != nil && out.kind != KindCamera {
		out = nil
	}
	if out == nil && s.scene.OutputCameraID() != "" && s.removedRef(s.scene.OutputCameraID()) {
		// 草案删掉了输出机位：改用剩下的第一个机位。
		if first := s.firstCamera(); first != nil {
			out = first
			s.setOutput(first.id)
			s.note("删除了输出机位，改用「%s」作为输出机位", first.name)
		} else {
			s.setOutput("")
		}
	}
	s.front = s.groupFront()

	if spec == nil && out != nil {
		// 没有 camera 草案：沿用输出机位，屏幕坐标系以它为准，不动它。
		s.camRef = out
		s.right, s.toCamera = s.axesOfCamera(out)
		return nil
	}
	if spec == nil {
		spec = &DraftCamera{}
		s.note("场景里没有输出机位，按默认新建一个机位")
	}
	s.camSpec = spec
	s.camRef = out

	// 写到哪个机位上：输出机位可替换就改它；否则改第一个可替换的 AI 机位；都没有就新建。
	if out != nil && out.replaceable {
		s.camEntity = out
	} else {
		for _, e := range s.entities {
			if e.existing && !e.removed && e.kind == KindCamera && e.replaceable {
				s.camEntity = e
				break
			}
		}
	}
	if s.camEntity == nil {
		id, err := s.newID()
		if err != nil {
			return err
		}
		e := &entity{id: id, kind: KindCamera, visible: true}
		e.name = s.nextName(KindCamera, "")
		s.entities = append(s.entities, e)
		s.camEntity = e
	}
	if s.camEntity != out {
		s.setOutput(s.camEntity.id)
		if out != nil {
			verb := "改用"
			if !s.camEntity.existing {
				verb = "新建"
			}
			s.note("输出机位「%s」%s，%s「%s」作为输出机位", out.name, protection(out), verb, s.camEntity.name)
		}
	}
	// 草案没写的方位、焦距与画幅：沿用被改的那个机位；新建的沿用当前输出机位（用户选的竖屏不能被换成横屏）。
	s.camBase = out
	if s.camEntity.existing {
		s.camBase = s.camEntity
	}

	switch {
	case spec.Side != "":
		s.toCamera = sideDirection(s.front, spec.Side)
	case s.camBase != nil:
		_, s.toCamera = s.axesOfCamera(s.camBase)
		s.note("未指定方位，沿用%s的方向", s.cameraLabel(s.camBase))
	default:
		s.toCamera = sideDirection(s.front, DefaultSide)
		s.note("未指定方位，按正面")
	}
	s.right = axisY.cross(s.toCamera).normalized(axisX)
	return nil
}

func (s *solver) setOutput(id string) {
	s.outputCamID = id
	s.outputSet = true
}

// cameraLabel 是说明里怎么称呼一个机位：当前输出机位要说出来，用户才知道沿用的是谁的设置。
func (s *solver) cameraLabel(e *entity) string {
	if e == s.camRef {
		return "输出机位「" + e.name + "」"
	}
	return "「" + e.name + "」"
}

func (s *solver) firstCamera() *entity {
	for _, e := range s.entities {
		if e.existing && !e.removed && e.kind == KindCamera {
			return e
		}
	}
	return nil
}

// axesOfCamera 取已有机位的屏幕右向量与「场景指向机位」的水平方向。
func (s *solver) axesOfCamera(e *entity) (right, toCamera vec3f) {
	t, _ := e.original["transform"].(map[string]any)
	cam, _ := e.original["camera"].(map[string]any)
	roll, _ := cam["roll"].(float64)
	r, _, _ := cameraBasis(vecOf(t["rotation"]), roll)
	right = r.horizontal().normalized(axisX)
	return right, right.cross(axisY).normalized(axisZ)
}

// groupFront 是角色群的正面：留在场景里、不重新摆放的可见角色与群众的平均朝向；
// 没有、或者朝向互相抵消（两人面对面）时取 +Z（默认机位所在的一侧）。
func (s *solver) groupFront() vec3f {
	var sum vec3f
	for _, e := range s.entities {
		if e.existing && !e.removed && !e.mover && e.visible && (e.kind == KindCharacter || e.kind == KindCrowd) {
			sum = sum.add(forwardOfYaw(e.yaw))
		}
	}
	if sum.length() < 1e-6 {
		return axisZ
	}
	return sum.normalized(axisZ)
}

// sideDirection 是从角色群指向机位的水平方向。左右按**观看者**说：front_left 是正面观众的左手边 45°。
func sideDirection(front vec3f, side string) vec3f {
	rightOfFront := axisY.cross(front).normalized(axisX) // 从正面看过去的屏幕右
	switch side {
	case "front_left":
		return front.sub(rightOfFront).normalized(front)
	case "front_right":
		return front.add(rightOfFront).normalized(front)
	case "left":
		return rightOfFront.scale(-1)
	case "right":
		return rightOfFront
	case "back":
		return front.scale(-1)
	}
	return front
}

// ---------------------------------------------------------------- 机位

// placeCamera 按景别与焦距反推机位距离，按角度定高度与俯仰（第 6 步，dev/01 §4.4）。
func (s *solver) placeCamera() error {
	spec := s.camSpec
	if spec == nil {
		return nil
	}
	shot := spec.ShotSize
	if shot == "" {
		shot = DefaultShotSize
		s.note("未指定景别，按%s", shotSizeName(shot))
	}
	angle := spec.Angle
	if angle == "" {
		angle = DefaultAngle
		s.note("未指定角度，按%s", labelOf(angles, angle))
	}

	params := defaultCameraParams()
	if s.camBase != nil {
		params = cameraParamsOf(s.camBase.original)
	}
	focal := spec.FocalLengthMm
	switch {
	case focal > 0:
	case s.camBase != nil:
		focal = numOr(params["focalLengthMm"], defaultFocalMm)
		s.note("未指定焦距，沿用%s的 %smm", s.cameraLabel(s.camBase), fmtNum(focal))
	default:
		focal = defaultFocalMm
		s.note("未指定焦距，按 %smm", fmtNum(focal))
	}
	aspect, _ := params["aspect"].(string)
	if _, ok := AspectOf(aspect); !ok {
		aspect = defaultAspect
	}
	sensor := numOr(params["sensorWidthMm"], defaultSensorMm)
	vfov, err := verticalFOV(focal, sensor, aspect)
	if err != nil {
		return err
	}

	targets := s.cameraTargets(spec.Targets)
	box := boundsOf(targets, s.anchor)
	coverage := coverageOf(shot)
	dist := (coverage * box.height / 2) / math.Tan(vfov/2)
	look := vec3f{box.center.x, box.minY + lookAtRatio[shot]*box.height, box.center.z}

	var pos vec3f
	switch angle {
	case "high":
		pos = elevated(look, s.toCamera, dist, highElevationDeg)
	case "top":
		pos = elevated(look, s.toCamera, dist, topElevationDeg)
	case "low":
		camY := s.ground + lowCameraHeight
		if camY > look.y-0.05 {
			camY = math.Max(s.ground+minLowCameraHeight, look.y-0.2)
			s.note("拍摄目标较矮，低机位降到离地 %s 米", fmtNum(round4(camY-s.ground)))
		}
		if dy := look.y - camY; dy > dist*math.Sin(rad(maxLowElevationDeg)) {
			camY = look.y - dist*math.Sin(rad(maxLowElevationDeg))
			s.note("低机位离目标太近，按仰角 %s° 抬高到离地 %s 米", fmtNum(maxLowElevationDeg), fmtNum(round4(camY-s.ground)))
		}
		dy := look.y - camY
		h := math.Sqrt(math.Max(dist*dist-dy*dy, 0))
		pos = look.add(s.toCamera.scale(h))
		pos.y = camY
	default: // eye_level、dutch
		pos = look.add(s.toCamera.scale(dist))
	}
	roll := 0.0
	if angle == "dutch" {
		roll = dutchRollDeg
	}
	toLook := look.sub(pos)
	pitch := deg(math.Atan2(toLook.y, toLook.horizontal().length()))
	rx, ry, rz := cameraEuler(facingYaw(s.toCamera), pitch)

	s.frameWidthNote(targets, dist, vfov, aspect, shot)

	e := s.camEntity
	if e.existing {
		if e.obj == nil {
			e.obj = deepCopyMap(e.original)
		}
		e.updated = true
	} else {
		cam := params
		e.obj = map[string]any{
			"id": e.id, "name": e.name, "kind": KindCamera,
			"transform": transform(vec3(0, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
			"visible":   true, "locked": false, "source": SourceAI,
			"camera": map[string]any{
				"focalLengthMm": focal, "sensorWidthMm": sensor, "aspect": aspect,
				"near": numOr(cam["near"], defaultNear), "far": numOr(cam["far"], defaultFar), "roll": 0.0,
			},
		}
		s.added = append(s.added, e)
	}
	t := subMap(e.obj, "transform")
	t["position"] = vec3(round4(pos.x), round4(pos.y), round4(pos.z))
	t["rotation"] = vec3(round4(rx), round4(ry), round4(rz))
	if _, ok := t["scale"]; !ok {
		t["scale"] = vec3(1, 1, 1)
	}
	c := subMap(e.obj, "camera")
	c["focalLengthMm"] = focal
	c["roll"] = roll
	e.obj = jsonTree(e.obj)
	e.pos = pos
	return nil
}

func elevated(look, toCamera vec3f, dist, elevationDeg float64) vec3f {
	e := rad(elevationDeg)
	p := look.add(toCamera.scale(dist * math.Cos(e)))
	p.y = look.y + dist*math.Sin(e)
	return p
}

func cameraParamsOf(m map[string]any) map[string]any {
	out := defaultCameraParams()
	if c, ok := m["camera"].(map[string]any); ok {
		for k, v := range c {
			out[k] = v
		}
	}
	return out
}

func numOr(v any, fallback float64) float64 {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return x
		}
	case int:
		if x > 0 {
			return float64(x)
		}
	}
	return fallback
}

// cameraTargets 解析机位要拍的对象；没写或一个都找不到时拍全部角色（再退到全部可见对象）。
func (s *solver) cameraTargets(refs []string) []*entity {
	var out []*entity
	for _, ref := range refs {
		if e, ok := s.resolveTarget(ref, nil); ok {
			out = append(out, e)
		} else {
			s.note("机位要拍的「%s」找不到，已忽略", ref)
		}
	}
	if len(out) > 0 {
		return out
	}
	if len(refs) == 0 {
		s.note("未指定机位拍谁，按全部角色")
	}
	for _, e := range s.entities {
		if !e.removed && e.visible && (e.kind == KindCharacter || e.kind == KindCrowd) {
			out = append(out, e)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, e := range s.entities {
		if !e.removed && e.visible && e.kind != KindCamera {
			out = append(out, e)
		}
	}
	return out
}

type bounds struct {
	center     vec3f
	minY       float64
	height     float64
	minX, maxX float64
	minZ, maxZ float64
}

// boundsOf 是目标的包围盒；没有目标时是锚点处一个成年人大小的虚拟目标。
func boundsOf(targets []*entity, anchor vec3f) bounds {
	if len(targets) == 0 {
		return bounds{center: anchor, minY: anchor.y, height: fallbackTargetHeight,
			minX: anchor.x, maxX: anchor.x, minZ: anchor.z, maxZ: anchor.z}
	}
	b := bounds{minX: math.Inf(1), maxX: math.Inf(-1), minZ: math.Inf(1), maxZ: math.Inf(-1)}
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, e := range targets {
		b.minX, b.maxX = math.Min(b.minX, e.pos.x-e.radius), math.Max(b.maxX, e.pos.x+e.radius)
		b.minZ, b.maxZ = math.Min(b.minZ, e.pos.z-e.radius), math.Max(b.maxZ, e.pos.z+e.radius)
		minY, maxY = math.Min(minY, e.pos.y), math.Max(maxY, e.pos.y+e.height)
	}
	b.minY = minY
	b.height = math.Max(maxY-minY, minTargetHeight)
	b.center = vec3f{(b.minX + b.maxX) / 2, minY, (b.minZ + b.maxZ) / 2}
	return b
}

// frameWidthNote：距离只按目标高度反推（景别的定义），多人横向排开时画面可能装不下，说一声而不是悄悄拉远。
func (s *solver) frameWidthNote(targets []*entity, dist, vfov float64, aspect, shot string) {
	if len(targets) < 2 {
		return
	}
	ratio, err := aspectRatio(aspect)
	if err != nil {
		return
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, e := range targets {
		x := e.pos.dot(s.right)
		lo, hi = math.Min(lo, x-e.radius), math.Max(hi, x+e.radius)
	}
	frameWidth := 2 * dist * math.Tan(vfov/2) * ratio
	if hi-lo > frameWidth {
		s.note("按%s拍，画面装不下全部 %d 个目标，左右会裁到", shotSizeName(shot), len(targets))
	}
}
