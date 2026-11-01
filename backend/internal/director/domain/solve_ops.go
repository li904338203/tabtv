package domain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 求解器：草案操作（删除、修改、新增）落到实体上，只碰可替换对象（DR-F-105）。

// ---------------------------------------------------------------- 草案操作

// applyOps 先处理全部删除，再按草案顺序处理修改与新增：「删掉甲、再加一个甲」与先后顺序无关。
func (s *solver) applyOps(objs []DraftObject) error {
	for i := range objs {
		d := &objs[i]
		if d.Op == OpRemove {
			s.remove(d)
		}
	}
	for i := range objs {
		d := objs[i]
		switch d.Op {
		case OpRemove:
			continue
		case "":
			if e := s.existingByID(d.Ref); e != nil {
				d.Op = OpUpdate
				s.note("「%s」没有写 op，按修改处理", d.Ref)
			} else {
				d.Op = OpAdd
				s.note("%s没有写 op，按新增处理", d.label())
				if d.Kind == "" {
					d.Kind = inferKind(d)
					s.note("%s没有写 kind，按%s处理", d.label(), kindLabel(d.Kind))
				}
			}
		}
		switch d.Op {
		case OpUpdate:
			s.update(&d)
		case OpAdd:
			if err := s.add(&d); err != nil {
				return err
			}
		}
	}
	return nil
}

func inferKind(d DraftObject) string {
	switch {
	case d.Prop != nil:
		return KindProp
	case d.Crowd != nil:
		return KindCrowd
	}
	return KindCharacter
}

// existingByID 按 ID 取未删除的已有对象。
func (s *solver) existingByID(id string) *entity {
	if id == "" {
		return nil
	}
	for _, e := range s.entities {
		if e.existing && !e.removed && e.id == id {
			return e
		}
	}
	return nil
}

// existingByRef 先按 ID、再按名称取未删除的已有对象（名称重复时取第一个）。
func (s *solver) existingByRef(ref string) *entity {
	if e := s.existingByID(ref); e != nil {
		return e
	}
	for _, e := range s.entities {
		if e.existing && !e.removed && ref != "" && e.name == ref {
			return e
		}
	}
	return nil
}

// protection 说明一个对象为什么不能改：已锁定，或是用户摆的。
func protection(e *entity) string {
	if lockedOf(e.original) {
		return "已锁定"
	}
	return "是用户摆的"
}

func lockedOf(m map[string]any) bool { v, _ := m["locked"].(bool); return v }

func (s *solver) remove(d *DraftObject) {
	e := s.existingByRef(d.Ref)
	switch {
	case e == nil:
		s.note("要删除的「%s」不在场景里，已忽略", d.Ref)
	case !e.replaceable:
		s.note("「%s」（%s）%s，不能删除，已保留原样", e.name, e.id, protection(e))
	default:
		e.removed = true
	}
}

func (s *solver) update(d *DraftObject) {
	e := s.existingByRef(d.Ref)
	if e == nil {
		if s.removedRef(d.Ref) {
			s.note("「%s」已在本次方案里删除，忽略对它的修改", d.Ref)
		} else {
			s.note("要修改的「%s」不在场景里，已忽略", d.Ref)
		}
		return
	}
	if !e.replaceable {
		s.note("「%s」（%s）%s，不能修改，已保留原样", e.name, e.id, protection(e))
		return
	}
	if e.kind == KindCamera {
		s.note("「%s」是机位，机位请写在 camera 里，已忽略这项修改", e.name)
		return
	}
	if d.Kind != "" && d.Kind != e.kind {
		s.note("「%s」是%s，不能改成%s，已忽略 kind", e.name, kindLabel(e.kind), kindLabel(d.Kind))
	}
	s.applyFields(e, d)
}

func (s *solver) removedRef(ref string) bool {
	for _, e := range s.entities {
		if e.existing && e.removed && (e.id == ref || e.name == ref) {
			return true
		}
	}
	return false
}

// applyFields 把草案里写了的字段改到对象的深拷贝上。
func (s *solver) applyFields(e *entity, d *DraftObject) {
	if e.obj == nil {
		e.obj = deepCopyMap(e.original)
	}
	e.updated = true
	e.draft = d
	if d.Name != "" {
		e.obj["name"] = truncName(d.Name)
		e.name = truncName(d.Name)
	}
	if d.Relation != nil {
		e.mover = true
	}
	switch e.kind {
	case KindCharacter:
		ch := subMap(e.obj, "character")
		if d.Model != "" {
			if m, ok := ModelOf(d.Model); ok {
				// 身高跟着素模走（dev/02 R-1 #2）：换模型不换身高的话，比例就不对了。
				ch["model"], ch["height"] = m.Code, m.HeightM
			}
		}
		if d.Body != "" {
			ch["body"] = d.Body
		}
		if d.PosePreset != "" {
			ch["posePreset"] = d.PosePreset
		}
		s.ignoreFor(e, d.Crowd != nil, "群众行列")
		s.ignoreFor(e, d.Prop != nil, "道具形状与尺寸")
	case KindCrowd:
		cr := subMap(e.obj, "crowd")
		if d.Model != "" {
			cr["model"] = d.Model
		}
		if d.PosePreset != "" {
			cr["posePreset"] = d.PosePreset
		}
		if c := d.Crowd; c != nil {
			rows, cols := intOf(cr["rows"]), intOf(cr["cols"])
			if c.Rows > 0 {
				rows = c.Rows
			}
			if c.Cols > 0 {
				cols = c.Cols
			}
			if rows*cols <= MaxCrowdSize {
				cr["rows"], cr["cols"] = float64(rows), float64(cols)
			} else {
				s.note("「%s」改行列后人数 %d 超过上限 %d，保持原行列", e.name, rows*cols, MaxCrowdSize)
			}
			if c.Spacing > 0 {
				cr["spacing"] = c.Spacing
			}
		}
		s.ignoreFor(e, d.Body != "", "体型")
		s.ignoreFor(e, d.Prop != nil, "道具形状与尺寸")
	case KindProp:
		pr := subMap(e.obj, "prop")
		if p := d.Prop; p != nil {
			if p.Shape != "" {
				pr["shape"] = p.Shape
			}
			if p.Size != nil {
				pr["size"] = map[string]any{"x": p.Size.X, "y": p.Size.Y, "z": p.Size.Z}
			}
		}
		s.ignoreFor(e, d.Model != "" || d.Body != "" || d.PosePreset != "", "素模、体型与姿势")
		s.ignoreFor(e, d.Crowd != nil, "群众行列")
	}
	e.radius, e.height = footprint(e.obj)
}

func (s *solver) ignoreFor(e *entity, present bool, what string) {
	if present {
		s.note("「%s」是%s，没有%s，已忽略", e.name, kindLabel(e.kind), what)
	}
}

func (s *solver) add(d *DraftObject) error {
	// 与已有对象重名：多半是模型在指代场景里那个对象，而不是想要第二个。
	if d.Ref != "" {
		if e := s.existingByRef(d.Ref); e != nil && e.kind != KindCamera {
			if !e.replaceable {
				s.note("场景里已有「%s」（%s），%s，不能重新摆放，已保留原样", e.name, e.id, protection(e))
				return nil
			}
			s.note("「%s」已在场景里，按修改处理", d.Ref)
			if d.Kind != "" && d.Kind != e.kind {
				s.note("「%s」是%s，不能改成%s，已忽略 kind", e.name, kindLabel(e.kind), kindLabel(d.Kind))
			}
			s.applyFields(e, d)
			return nil
		}
	}
	id, err := s.newID()
	if err != nil {
		return err
	}
	e := &entity{id: id, kind: d.Kind, ref: d.Ref, visible: true, mover: true, draft: d}
	for _, other := range s.added {
		if d.Ref != "" && other.ref == d.Ref {
			// 同一方案里重名：第二个改名，关系里的这个名字指第一个。
			e.ref = ""
			s.note("方案里有两个「%s」，第二个改名", d.Ref)
			break
		}
	}
	name := d.Name
	if name == "" {
		name = d.Ref
	}
	if name == "" || e.ref == "" && d.Ref != "" {
		name = s.nextName(d.Kind, name)
	}
	e.name = truncName(name)
	e.obj = s.newObject(e, d)
	e.radius, e.height = footprint(e.obj)
	s.entities = append(s.entities, e)
	s.added = append(s.added, e)
	return nil
}

func (s *solver) newID() (string, error) {
	for i := 0; i < 10; i++ {
		id := s.opt.NewID()
		if id != "" && utf8.RuneCountInString(id) <= MaxIDChars && !s.ids[id] {
			s.ids[id] = true
			return id, nil
		}
	}
	return "", fmt.Errorf("生成新对象 ID 失败：连续 10 次重复或不合法")
}

var numberedName = regexp.MustCompile(`^(.+) (\d+)$`)

// nextName 生成「角色 N / 群众 N / 道具 N / 机位 N」：取同类当前最大号 + 1（与前端 directorNextName 同规则）。
// base 非空时（同名改名）生成「base 2」「base 3」……
func (s *solver) nextName(kind, base string) string {
	prefix := base
	if prefix == "" {
		prefix = kindLabel(kind)
	}
	maxN := 0
	if base != "" {
		maxN = 1
	}
	for _, e := range s.entities {
		if e.removed || (base == "" && e.kind != kind) {
			continue
		}
		if m := numberedName.FindStringSubmatch(e.name); m != nil && m[1] == prefix {
			if n, err := strconv.Atoi(m[2]); err == nil && n > maxN {
				maxN = n
			}
		}
	}
	suffix := " " + strconv.Itoa(maxN+1)
	// 名称上限 20 字：截掉前缀而不是截掉编号，否则「角色 12」和「角色 1」截出来一样。
	limit := MaxNameChars - utf8.RuneCountInString(suffix)
	return headRunes(prefix, limit) + suffix
}

func truncName(name string) string { return headRunes(strings.TrimSpace(name), MaxNameChars) }

// newObject 建新对象的 map：默认值取目录的 defaults 段（与前端新建对象同一份）。
func (s *solver) newObject(e *entity, d *DraftObject) map[string]any {
	defaults := catalogDefaults()
	obj := map[string]any{
		"id": e.id, "name": e.name, "kind": e.kind,
		"transform": transform(vec3(0, s.ground, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
		"visible":   true, "locked": false, "source": SourceAI,
	}
	switch e.kind {
	case KindCharacter:
		ch := copyAny(defaults["character"]).(map[string]any)
		if d.Model != "" {
			ch["model"] = d.Model
		} else {
			s.defaultModelUsed = true
		}
		if d.Body != "" {
			ch["body"] = d.Body
		}
		if d.PosePreset != "" {
			ch["posePreset"] = d.PosePreset
		} else {
			s.defaultPoseUsed = true
		}
		model, _ := ModelOf(str(ch["model"]))
		ch["height"] = model.HeightM
		obj["character"] = ch
		if d.Crowd != nil || d.Prop != nil {
			s.note("「%s」是角色，已忽略群众与道具参数", e.name)
		}
	case KindCrowd:
		cr := copyAny(defaults["crowd"]).(map[string]any)
		if d.Model != "" {
			cr["model"] = d.Model
		} else {
			s.defaultModelUsed = true
		}
		if d.PosePreset != "" {
			cr["posePreset"] = d.PosePreset
		} else {
			s.defaultPoseUsed = true
		}
		if c := d.Crowd; c != nil {
			rows, cols := intOf(cr["rows"]), intOf(cr["cols"])
			if c.Rows > 0 {
				rows = c.Rows
			}
			if c.Cols > 0 {
				cols = c.Cols
			}
			if rows*cols <= MaxCrowdSize {
				cr["rows"], cr["cols"] = rows, cols
			} else {
				s.note("「%s」的人数 %d 超过上限 %d，按默认行列", e.name, rows*cols, MaxCrowdSize)
			}
			if c.Spacing > 0 {
				cr["spacing"] = c.Spacing
			}
		}
		obj["crowd"] = cr
	case KindProp:
		pr := copyAny(defaults["prop"]).(map[string]any)
		if p := d.Prop; p != nil {
			if p.Shape != "" {
				pr["shape"] = p.Shape
			}
			if p.Size != nil {
				pr["size"] = vec3(p.Size.X, p.Size.Y, p.Size.Z)
			}
		}
		obj["prop"] = pr
		if d.Model != "" || d.PosePreset != "" {
			s.note("「%s」是道具，已忽略素模与姿势", e.name)
		}
	}
	return jsonTree(obj)
}
