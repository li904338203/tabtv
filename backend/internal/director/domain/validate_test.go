package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_合法场景与fixture一致(t *testing.T) {
	for _, name := range []string{"valid-default.json", "valid-unknown-fields.json"} {
		if err := Validate(readFixture(t, "scenes", name)); err != nil {
			t.Errorf("%s 应当合法：%v", name, err)
		}
	}
}

// TestValidate_非法场景报出字段路径：遍历 scenes/invalid-*.json，Path 必须与 fixture 一致（前端报同一个）。
func TestValidate_非法场景报出字段路径(t *testing.T) {
	files, err := filepath.Glob(fixturePath("scenes", "invalid-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 8 {
		t.Fatalf("非法用例只有 %d 个，至少要 8 个", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Scene json.RawMessage `json:"scene"`
			Path  string          `json:"path"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s：%v", f, err)
		}
		err = Validate(c.Scene)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s：期望字段错误 %q，实际 %v", filepath.Base(f), c.Path, err)
			continue
		}
		if ve.Path != c.Path {
			t.Errorf("%s：期望 path %q，实际 %q（%s）", filepath.Base(f), c.Path, ve.Path, ve.Reason)
		}
	}
}

// TestValidate_未知字段原样保留：校验只读不改；解析后再序列化，未知字段仍在（GO-35）。
func TestValidate_未知字段原样保留(t *testing.T) {
	raw := readFixture(t, "scenes", "valid-unknown-fields.json")
	if err := Validate(raw); err != nil {
		t.Fatalf("带未知字段的场景应当合法：%v", err)
	}
	s, err := ParseScene(raw)
	if err != nil {
		t.Fatal(err)
	}
	back, err := json.Marshal(s.Tree())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"futureTopLevel", "futureStage", "futureSnap", "futureLight", "futureObject",
		"futureTransform", "futureSub", "futureTimeline", `"w":1`} {
		if !strings.Contains(string(back), key) {
			t.Errorf("未知字段 %s 丢了", key)
		}
	}
}

func objectsScene(n int) []byte {
	objs := make([]any, 0, n)
	for i := 0; i < n; i++ {
		objs = append(objs, map[string]any{
			"id": fmt.Sprintf("obj_%d", i), "name": "道具", "kind": KindProp,
			"transform": transform(vec3(0, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
			"prop":      map[string]any{"shape": PropBox, "size": vec3(1, 1, 1)},
		})
	}
	b, _ := json.Marshal(map[string]any{"schemaVersion": 1, "objects": objs})
	return b
}

func keyframes(n int) []any {
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{
			"frame": i, "interpolation": "linear",
			"value": map[string]any{"position": vec3(0, 0, 0), "rotation": vec3(0, 0, 0)},
		})
	}
	return out
}

func timelineScene(tracks []any, objects []any) []byte {
	b, _ := json.Marshal(map[string]any{
		"schemaVersion": 1, "objects": objects,
		"timeline": map[string]any{"fps": 30, "durationFrames": 450, "tracks": tracks},
	})
	return b
}

func charObject(id string) map[string]any {
	return map[string]any{
		"id": id, "name": "角色", "kind": KindCharacter,
		"transform": transform(vec3(0, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
		"character": map[string]any{"model": ModelAdultMale, "height": 1.8, "body": "standard"},
	}
}

func wantPath(t *testing.T, err error, path string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("期望字段错误 %q，实际 %v", path, err)
	}
	if ve.Path != path {
		t.Fatalf("期望 path %q，实际 %q（%s）", path, ve.Path, ve.Reason)
	}
}

// TestValidate_上限：对象 200 / 关键帧每轨 120、全场景 2000 / 骨骼覆盖 70 / 正文 512 KiB（DR-PARAM-09）。
func TestValidate_上限(t *testing.T) {
	if err := Validate(objectsScene(MaxObjects)); err != nil {
		t.Fatalf("恰好 %d 个对象应当放行：%v", MaxObjects, err)
	}
	wantPath(t, Validate(objectsScene(MaxObjects+1)), "objects")

	objs := []any{charObject("obj_c")}
	ok := timelineScene([]any{map[string]any{"objectId": "obj_c", "channel": "transform",
		"keyframes": keyframes(MaxKeyframesPerTrack)}}, objs)
	if err := Validate(ok); err != nil {
		t.Fatalf("恰好 %d 个关键帧应当放行：%v", MaxKeyframesPerTrack, err)
	}
	over := timelineScene([]any{map[string]any{"objectId": "obj_c", "channel": "transform",
		"keyframes": keyframes(MaxKeyframesPerTrack + 1)}}, objs)
	wantPath(t, Validate(over), "timeline.tracks[0].keyframes")

	// 17 条轨道 × 120 = 2040 > 2000。
	var many []any
	var manyObjs []any
	for i := 0; i < 17; i++ {
		id := fmt.Sprintf("obj_c%d", i)
		manyObjs = append(manyObjs, charObject(id))
		many = append(many, map[string]any{"objectId": id, "channel": "transform", "keyframes": keyframes(MaxKeyframesPerTrack)})
	}
	wantPath(t, Validate(timelineScene(many, manyObjs)), "timeline.tracks")

	bones := map[string]any{}
	for i := 0; i <= MaxBoneOverrides; i++ {
		bones[fmt.Sprintf("bone%02d", i)] = vec3(0, 0, 0)
	}
	ch := charObject("obj_b")
	ch["character"].(map[string]any)["boneOverrides"] = bones
	b, _ := json.Marshal(map[string]any{"schemaVersion": 1, "objects": []any{ch}})
	wantPath(t, Validate(b), "objects[0].character.boneOverrides")

	big := make([]byte, 0, MaxSceneBytes+64)
	big = append(big, `{"schemaVersion":1,"pad":"`...)
	big = append(big, strings.Repeat("x", MaxSceneBytes)...)
	big = append(big, `"}`...)
	if err := Validate(big); !errors.Is(err, ErrSceneTooLarge) {
		t.Fatalf("超过 512 KiB 应返回 ErrSceneTooLarge，实际 %v", err)
	}
}

func TestValidate_输出机位必须存在(t *testing.T) {
	var tree map[string]any
	if err := json.Unmarshal(DefaultScene(), &tree); err != nil {
		t.Fatal(err)
	}
	tree["outputCameraId"] = DefaultCharacterID // 指向一个角色，不是机位
	b, _ := json.Marshal(tree)
	wantPath(t, Validate(b), "outputCameraId")

	tree["outputCameraId"] = nil // 没有输出机位是合法的（节点没有输出）
	b, _ = json.Marshal(tree)
	if err := Validate(b); err != nil {
		t.Fatalf("outputCameraId 为 null 应当合法：%v", err)
	}
}

// TestValidate_姿势码未知放行：前端的姿势资源可能比后端新（dev/01 §4.1）。
func TestValidate_姿势码未知放行(t *testing.T) {
	ch := charObject("obj_p")
	ch["character"].(map[string]any)["posePreset"] = "brand_new_pose_from_frontend"
	b, _ := json.Marshal(map[string]any{"schemaVersion": 1, "objects": []any{ch}})
	if err := Validate(b); err != nil {
		t.Fatalf("未知姿势码应当放行：%v", err)
	}
	if IsKnownPose("brand_new_pose_from_frontend") {
		t.Fatal("IsKnownPose 不该认识前端新加的姿势码")
	}
}

func TestValidate_只有结构版本必填(t *testing.T) {
	if err := Validate([]byte(`{"schemaVersion":1}`)); err != nil {
		t.Fatalf("只有结构版本的场景应当合法：%v", err)
	}
	wantPath(t, Validate([]byte(`{"objects":[]}`)), "schemaVersion")
	wantPath(t, Validate([]byte(`[1,2]`)), "$")
}
