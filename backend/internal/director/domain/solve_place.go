package domain

import (
	"math"
)

// 求解器：按依赖顺序摆放、避让、贴地，最后算朝向（dev/01 §4.4 第 2~5 步）。

// ---------------------------------------------------------------- 摆放

// place 按依赖顺序摆放（第 2~4 步）。
func (s *solver) place() {
	s.anchor = s.sceneCenter()
	var movers []*entity
	for _, e := range s.entities {
		if e.mover && !e.removed {
			movers = append(movers, e)
		}
	}
	for {
		progressed := false
		pending := 0
		for _, e := range movers {
			if e.placed {
				continue
			}
			rel := e.draft.Relation
			if rel == nil || rel.Type == RelCenter {
				s.placeFree(e, rel != nil)
				progressed = true
				continue
			}
			target, ok := s.resolveTarget(rel.Target, e)
			switch {
			case !ok:
				s.note("「%s」的关系目标「%s」找不到，按场景中心处理", e.name, rel.Target)
				s.placeFree(e, true)
				progressed = true
			case target.mover && !target.placed:
				pending++
			default:
				s.placeRelative(e, target, rel)
				progressed = true
			}
		}
		if pending == 0 {
			break
		}
		if !progressed {
			// 互相依赖（A 在 B 左边、B 在 A 右边且都是新对象）：按草案顺序取第一个，按场景中心处理。
			for _, e := range movers {
				if !e.placed {
					s.note("「%s」的位置关系依赖还没摆好的对象（互相依赖），按场景中心处理", e.name)
					s.placeFree(e, true)
					break
				}
			}
		}
	}
}

// sceneCenter 是锚点：已有可见对象（不含机位、不含要重新摆放的）的包围盒中心，没有对象时取原点。
func (s *solver) sceneCenter() vec3f {
	first := true
	var minX, maxX, minZ, maxZ float64
	for _, e := range s.entities {
		if !e.existing || e.removed || e.mover || !e.visible || e.kind == KindCamera {
			continue
		}
		if first {
			minX, maxX, minZ, maxZ = e.pos.x, e.pos.x, e.pos.z, e.pos.z
			first = false
			continue
		}
		minX, maxX = math.Min(minX, e.pos.x), math.Max(maxX, e.pos.x)
		minZ, maxZ = math.Min(minZ, e.pos.z), math.Max(maxZ, e.pos.z)
	}
	if first {
		return vec3f{0, s.ground, 0}
	}
	return vec3f{(minX + maxX) / 2, s.ground, (minZ + maxZ) / 2}
}

// resolveTarget 找关系或朝向的目标：先按已有对象 ID，再按本次新增的名字，最后按已有对象的名字。
// 机位、已删除的对象、自己都不算。
func (s *solver) resolveTarget(ref string, self *entity) (*entity, bool) {
	pick := func(e *entity) (*entity, bool) {
		if e == nil || e == self || e.removed || e.kind == KindCamera {
			return nil, false
		}
		return e, true
	}
	if e := s.existingByID(ref); e != nil {
		return pick(e)
	}
	for _, e := range s.added {
		if e.ref != "" && e.ref == ref {
			return pick(e)
		}
	}
	for _, e := range s.entities {
		if !e.removed && e.name == ref && e != self {
			return pick(e)
		}
	}
	return nil, false
}

// placeFree 摆没有关系约束的对象：第一个放在锚点，之后的在锚点左右交替排开（右、左、右 2、左 2……）。
func (s *solver) placeFree(e *entity, explicit bool) {
	k := s.freeSlots
	s.freeSlots++
	dir := s.right
	offset := 0.0
	if k > 0 {
		offset = defaultSideDistance * float64((k+1)/2)
		if k%2 == 0 {
			dir = s.right.scale(-1)
		}
		if !explicit {
			s.note("「%s」没有给位置关系，排在场景中心旁边", e.name)
		}
	}
	p := s.anchor.add(dir.scale(offset))
	s.settle(e, p, dir)
}

// placeRelative 按关系槽位摆放（第 3 步）。
func (s *solver) placeRelative(e, target *entity, rel *DraftRelation) {
	dist := rel.Distance
	if dist == 0 {
		s.usedDefaultDistance = true
		switch rel.Type {
		case RelNear:
			dist = defaultNearDistance
		case RelInFrontOf, RelBehind:
			dist = defaultDepthDistance
		default:
			dist = defaultSideDistance
		}
	}
	base := vec3f{target.pos.x, s.ground, target.pos.z}
	var candidates []vec3f
	switch rel.Type {
	case RelLeftOf:
		candidates = []vec3f{s.right.scale(-1)}
	case RelRightOf:
		candidates = []vec3f{s.right}
	case RelInFrontOf:
		candidates = []vec3f{s.toCamera}
	case RelBehind:
		candidates = []vec3f{s.toCamera.scale(-1)}
	case RelFacing:
		// 面对面：站在目标的左右（屏幕横向），这样从机位看是两张侧脸，而不是一个人挡住另一个。
		candidates = []vec3f{s.right, s.right.scale(-1)}
	default: // near
		r, f := s.right, s.toCamera
		d1 := r.add(f.scale(-1)).normalized(r)
		d2 := r.scale(-1).add(f.scale(-1)).normalized(r)
		candidates = []vec3f{r, r.scale(-1), f.scale(-1), f, d1, d2,
			r.add(f).normalized(r), r.scale(-1).add(f).normalized(r)}
	}
	dir := candidates[0]
	for _, c := range candidates {
		if s.collider(e, base.add(c.scale(dist))) == nil {
			dir = c
			break
		}
	}
	s.settle(e, base.add(dir.scale(dist)), dir)
}

// settle 把对象放在 p，贴地，并沿 dir 推开到不与已摆好的对象重叠（第 4、5 步）。
func (s *solver) settle(e *entity, p, dir vec3f) {
	p.y = s.ground
	for i := 0; i < maxPushes; i++ {
		other := s.collider(e, p)
		if other == nil {
			break
		}
		p = p.add(dir.scale(pushDistance(p, other.pos, dir, gapBetween(e, other))))
		p.y = s.ground
	}
	if other := s.collider(e, p); other != nil {
		s.note("「%s」周围太挤，推了 %d 次仍与「%s」靠得太近", e.name, maxPushes, other.name)
	}
	e.pos = p
	e.placed = true
}

// collider 返回与「e 放在 p」重叠的第一个已摆好的对象（不含机位）。
func (s *solver) collider(e *entity, p vec3f) *entity {
	for _, o := range s.entities {
		if o == e || o.removed || o.kind == KindCamera || (o.mover && !o.placed) {
			continue
		}
		if p.horizontalDist(o.pos) < gapBetween(e, o) {
			return o
		}
	}
	return nil
}

// gapBetween 是两个对象中心的最小水平距离：两个角色是 0.5 m（半径各 0.3，允许轻微贴近），
// 大的道具与群众按各自的半径放宽。
func gapBetween(a, b *entity) float64 {
	return math.Max(minObjectGap, a.radius+b.radius-2*characterRadius+minObjectGap)
}

// pushDistance 是沿 dir 至少要走多远，p 与 q 的水平距离才不小于 gap。
func pushDistance(p, q, dir vec3f, gap float64) float64 {
	w := p.sub(q).horizontal()
	dh := dir.horizontal().normalized(axisX)
	b := w.dot(dh)
	disc := b*b - (w.dot(w) - gap*gap)
	if disc < 0 {
		disc = 0
	}
	return -b + math.Sqrt(disc) + pushMargin
}

// ---------------------------------------------------------------- 朝向

// orient 算朝向（最后一步）：面向机位、面向目标、面对面关系，或者默认面向角色群的正面。
func (s *solver) orient() {
	camPos, hasCam := s.cameraPosition()
	for _, e := range s.entities {
		if e.removed || e.kind == KindCamera || e.obj == nil || e.draft == nil {
			continue
		}
		d := e.draft
		var dir vec3f
		set := false
		switch {
		case d.Facing != nil && d.Facing.Target == FacingCamera:
			if hasCam {
				dir, set = camPos.sub(e.pos), true
			} else {
				s.note("「%s」要面向机位，但场景里没有机位，按默认朝向", e.name)
			}
		case d.Facing != nil:
			if t, ok := s.facingTarget(d.Facing.Target, e); ok {
				dir, set = t.sub(e.pos), true
			} else {
				s.note("「%s」的朝向目标「%s」找不到，按默认朝向", e.name, d.Facing.Target)
			}
		case d.Relation != nil && d.Relation.Type == RelFacing:
			if t, ok := s.resolveTarget(d.Relation.Target, e); ok {
				dir, set = t.pos.sub(e.pos), true
			}
		}
		if !set {
			if e.existing && !e.mover {
				// 修改但没动位置、也没说朝向：保持原朝向。
				s.writeTransform(e, false)
				continue
			}
			dir = s.front
		}
		e.yaw = facingYaw(dir)
		s.writeTransform(e, true)
	}
}

// facingTarget 找朝向目标的位置：除了对象，也可以是某个机位。
func (s *solver) facingTarget(ref string, self *entity) (vec3f, bool) {
	if t, ok := s.resolveTarget(ref, self); ok {
		return t.pos, true
	}
	for _, e := range s.entities {
		if !e.removed && e.kind == KindCamera && (e.id == ref || e.name == ref) {
			if e == s.camEntity {
				return s.cameraPosition()
			}
			return e.pos, true
		}
	}
	return vec3f{}, false
}

func (s *solver) cameraPosition() (vec3f, bool) {
	switch {
	case s.camEntity != nil:
		return s.camEntity.pos, true
	case s.camRef != nil:
		return s.camRef.pos, true
	}
	return vec3f{}, false
}

// writeTransform 把求解出的位置（与朝向）写回对象 map。
func (s *solver) writeTransform(e *entity, withYaw bool) {
	t := subMap(e.obj, "transform")
	if e.mover {
		t["position"] = vec3(round4(e.pos.x), round4(s.ground), round4(e.pos.z))
	} else if p, ok := t["position"].(map[string]any); ok {
		// 修改而没有重新摆放的对象也贴地（第 5 步）：它本来就是 AI 摆的。
		p["y"] = round4(s.ground)
	}
	if withYaw {
		r, _ := t["rotation"].(map[string]any)
		x, _ := r["x"].(float64)
		z, _ := r["z"].(float64)
		t["rotation"] = vec3(x, round4(e.yaw), z)
	}
	if _, ok := t["scale"]; !ok {
		t["scale"] = vec3(1, 1, 1)
	}
	e.obj = jsonTree(e.obj)
}
