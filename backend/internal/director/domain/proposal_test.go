package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// mixedScene：用户的角色、锁定的 AI 道具、可替换的 AI 角色与 AI 机位，外加每一层的未知字段。
func mixedScene(t *testing.T) Scene {
	t.Helper()
	return sceneOf(t, map[string]any{
		"schemaVersion": 1, "futureTopLevel": map[string]any{"keep": true}, "outputCameraId": "obj_cam",
		"objects": []any{
			map[string]any{"id": "obj_cam", "name": "机位 1", "kind": KindCamera, "source": SourceAI,
				"transform": transform(vec3(0, 1.35, 3), vec3(0, 0, 0), vec3(1, 1, 1)),
				"camera":    map[string]any{"focalLengthMm": 35, "aspect": "16:9", "futureLens": "anamorphic"}},
			map[string]any{"id": "obj_user", "name": "用户角色", "kind": KindCharacter, "source": SourceUser,
				"transform": transform(vec3(-1, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
				"character": map[string]any{"model": ModelAdultMale, "height": 1.8, "body": "standard"}},
			map[string]any{"id": "obj_locked", "name": "锁定道具", "kind": KindProp, "source": SourceAI, "locked": true,
				"transform": transform(vec3(2, 0, -1), vec3(0, 0, 0), vec3(1, 1, 1)),
				"prop":      map[string]any{"shape": PropBox, "size": vec3(1, 1, 1)}},
			map[string]any{"id": "obj_ai", "name": "AI 角色", "kind": KindCharacter, "source": SourceAI,
				"futureObject": []any{1, 2, 3},
				"transform":    transform(vec3(1, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
				"character":    map[string]any{"model": ModelAdultMale, "height": 1.8, "body": "standard", "posePreset": "walk", "futureBone": "x"}},
			map[string]any{"id": "obj_ai_2", "name": "AI 角色 2", "kind": KindCharacter, "source": SourceAI,
				"transform": transform(vec3(0, 0, -2), vec3(0, 0, 0), vec3(1, 1, 1)),
				"character": map[string]any{"model": ModelAdultFemale, "height": 1.625, "body": "standard"}},
		},
	})
}

func TestApplyProposal_只替换可替换对象(t *testing.T) {
	scene := mixedScene(t)
	edited := func(id string) map[string]any {
		o, _ := scene.ObjectByID(id)
		m := deepCopyMap(o.Raw())
		m["name"] = "改过的"
		return m
	}
	// 手造一份越权的 Resolved：改用户对象、删锁定对象 —— ApplyProposal 是第二道闸，照样跳过。
	r := Resolved{
		Updated: []map[string]any{edited("obj_user"), edited("obj_ai")},
		Removed: []string{"obj_locked", "obj_ai_2"},
	}
	raw, changes, err := ApplyProposal(scene, r)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ParseScene(raw)
	for _, id := range []string{"obj_user", "obj_locked"} {
		before, _ := scene.ObjectByID(id)
		after, ok := out.ObjectByID(id)
		if !ok || !reflect.DeepEqual(before.Raw(), after.Raw()) {
			t.Errorf("%s 不可替换，应逐字不变", id)
		}
	}
	if o, _ := out.ObjectByID("obj_ai"); o.Name() != "改过的" {
		t.Errorf("可替换的 AI 对象应被修改：%q", o.Name())
	}
	if _, ok := out.ObjectByID("obj_ai_2"); ok {
		t.Error("可替换的 AI 对象应被删除")
	}
	if !reflect.DeepEqual(changes, Changes{Added: []string{}, Updated: []string{"obj_ai"}, Removed: []string{"obj_ai_2"}}) {
		t.Errorf("变更清单只列真正生效的：%+v", changes)
	}

	// 走完整求解：对用户对象与锁定对象的修改、删除被忽略并说明。
	p, out2 := build(t, scene, `{"objects":[
		{"op":"update","ref":"obj_user","posePreset":"bow"},
		{"op":"remove","ref":"obj_locked"},
		{"op":"update","ref":"obj_locked","shape":"sphere"},
		{"op":"remove","ref":"obj_ai_2"}]}`)
	for _, want := range []string{"「用户角色」（obj_user）是用户摆的，不能修改", "「锁定道具」（obj_locked）已锁定，不能删除", "「锁定道具」（obj_locked）已锁定，不能修改"} {
		if !hasNote(p, want) {
			t.Errorf("说明里应有「%s」：%v", want, p.Defaults)
		}
	}
	for _, id := range []string{"obj_user", "obj_locked"} {
		before, _ := scene.ObjectByID(id)
		after, _ := out2.ObjectByID(id)
		if !reflect.DeepEqual(before.Raw(), after.Raw()) {
			t.Errorf("%s 应逐字不变", id)
		}
	}
}

func TestApplyProposal_变更清单(t *testing.T) {
	scene := mixedScene(t)
	p, out := build(t, scene, `{"objects":[
		{"op":"remove","ref":"obj_ai_2"},
		{"op":"update","ref":"obj_ai","posePreset":"cheer"},
		{"op":"update","ref":"obj_ai","posePreset":"cheer"},
		{"op":"add","ref":"新人甲","kind":"character"},
		{"op":"add","ref":"新道具","kind":"prop","shape":"sphere","relation":{"type":"near","target":"新人甲"}}],
		"camera":{"shotSize":"full","side":"front"}}`)
	want := Changes{
		Added:   []string{"obj_new_1", "obj_new_2"},
		Updated: []string{"obj_cam", "obj_ai"},
		Removed: []string{"obj_ai_2"},
	}
	if !reflect.DeepEqual(p.Changes, want) {
		t.Errorf("变更清单：期望 %+v，实际 %+v", want, p.Changes)
	}
	for _, id := range want.Added {
		o, ok := out.ObjectByID(id)
		if !ok || o.Source() != SourceAI || o.Locked() || !o.Visible() {
			t.Errorf("新对象 %s 应为 source=ai、未锁定、可见", id)
		}
	}
	// 没有真正变化的修改不进清单：只把姿势改成它原本的值。
	p2, _ := build(t, scene, `{"objects":[{"op":"update","ref":"obj_ai","posePreset":"walk"}]}`)
	if len(p2.Changes.Updated) != 0 {
		t.Errorf("没变化就不算修改：%+v", p2.Changes)
	}
	// JSON 形状是契约（dev/02 §4.2）：空清单是 []，不是 null。
	b, _ := json.Marshal(p2)
	if !strings.Contains(string(b), `"changes":{"added":[],"updated":[],"removed":[]}`) || !strings.Contains(string(b), `"defaults":[`) {
		t.Errorf("提案的 JSON 形状不对：%s", b)
	}
}

func TestApplyProposal_未知字段保留(t *testing.T) {
	scene := mixedScene(t)
	p, out := build(t, scene, `{"objects":[{"op":"update","ref":"obj_ai","posePreset":"cheer","relation":{"type":"right_of","target":"obj_user","distance":2}}],
		"camera":{"shotSize":"medium","angle":"high"}}`)
	if out.Tree()["futureTopLevel"] == nil {
		t.Error("顶层未知字段丢了")
	}
	ai, _ := out.ObjectByID("obj_ai")
	if ai.Raw()["futureObject"] == nil {
		t.Error("被修改对象的未知字段丢了")
	}
	ch, _ := ai.Raw()["character"].(map[string]any)
	if ch["futureBone"] != "x" || ch["posePreset"] != "cheer" {
		t.Errorf("子结构里的未知字段要保留、改的字段要改：%+v", ch)
	}
	cam, _ := out.ObjectByID("obj_cam")
	if c, _ := cam.Raw()["camera"].(map[string]any); c["futureLens"] != "anamorphic" {
		t.Errorf("就地修改的机位也要保留未知字段：%+v", c)
	}
	if len(p.Changes.Updated) != 2 {
		t.Errorf("机位与角色都改了：%+v", p.Changes)
	}
}

func TestApplyProposal_超上限报错(t *testing.T) {
	objs := make([]any, 0, MaxObjects)
	for i := 0; i < MaxObjects-1; i++ {
		objs = append(objs, map[string]any{"id": fmt.Sprintf("obj_%d", i), "name": "道具", "kind": KindProp,
			"transform": transform(vec3(float64(i%20)*3, 0, float64(i/20)*3), vec3(0, 0, 0), vec3(1, 1, 1)),
			"prop":      map[string]any{"shape": PropBox, "size": vec3(1, 1, 1)}})
	}
	scene := sceneOf(t, map[string]any{"schemaVersion": 1, "objects": objs})
	d, dropped, err := ParseDraft(`{"objects":[{"op":"add","ref":"甲","kind":"character"},{"op":"add","ref":"乙","kind":"character"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Propose(scene, d, dropped, SolveOptions{NewID: seqIDs()})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Path != "objects" {
		t.Fatalf("加上新对象（还有新建的机位）超过 %d 个，应报 objects 超上限：%v", MaxObjects, err)
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("错误要说清上限：%v", err)
	}
}

func TestApplyProposal_不改入参(t *testing.T) {
	scene := mixedScene(t)
	before, _ := json.Marshal(scene.Tree())
	o, _ := scene.ObjectByID("obj_ai_2")
	edited := deepCopyMap(o.Raw())
	edited["name"] = "x"
	if _, _, err := ApplyProposal(scene, Resolved{Removed: []string{"obj_ai"}, Updated: []map[string]any{edited}}); err != nil {
		t.Fatal(err)
	}
	// 修改里给的是残缺对象：合进去不合法，报错而不是存一个坏场景。
	if _, _, err := ApplyProposal(scene, Resolved{Updated: []map[string]any{{"id": "obj_ai_2", "name": "x"}}}); err == nil {
		t.Error("残缺的修改对象应被 Validate 拦下")
	}
	after, _ := json.Marshal(scene.Tree())
	if string(before) != string(after) {
		t.Fatal("ApplyProposal 不能改入参的场景")
	}
}
