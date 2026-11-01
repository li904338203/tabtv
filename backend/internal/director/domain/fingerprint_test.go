package domain

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRenderFingerprint_只看画面内容：遍历 fingerprint/*.json，same 与否必须与 fixture 一致。
func TestRenderFingerprint_只看画面内容(t *testing.T) {
	files, err := filepath.Glob(fixturePath("fingerprint", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 6 {
		t.Fatalf("指纹用例只有 %d 组，至少要 6 组", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Before json.RawMessage `json:"before"`
			After  json.RawMessage `json:"after"`
			Same   bool            `json:"same"`
			Why    string          `json:"why"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s：%v", f, err)
		}
		for _, s := range []json.RawMessage{c.Before, c.After} {
			if err := Validate(s); err != nil {
				t.Fatalf("%s 的场景本身不合法：%v", filepath.Base(f), err)
			}
		}
		a, err := RenderFingerprint(c.Before)
		if err != nil {
			t.Fatal(err)
		}
		b, err := RenderFingerprint(c.After)
		if err != nil {
			t.Fatal(err)
		}
		if (a == b) != c.Same {
			t.Errorf("%s（%s）：期望 same=%v，实际 %s vs %s", filepath.Base(f), c.Why, c.Same, a, b)
		}
		if len(a) != 32 {
			t.Errorf("指纹应为 32 位 hex，实际 %q", a)
		}
	}
}

// TestRenderFingerprint_键序与浮点噪声不影响（DRR-14）：JS 写出的数字末位可能有噪声。
func TestRenderFingerprint_键序与浮点噪声不影响(t *testing.T) {
	a := []byte(`{"schemaVersion":1,"ground":{"visible":true,"opacity":1,"height":0},
		"objects":[{"id":"o1","kind":"prop","name":"x","transform":{"position":{"x":1,"y":0,"z":0},
		"rotation":{"x":0,"y":0,"z":0},"scale":{"x":1,"y":1,"z":1}},"prop":{"shape":"box","size":{"x":1,"y":1,"z":1}}}]}`)
	b := []byte(`{"objects":[{"prop":{"size":{"z":1,"y":1,"x":1},"shape":"box"},"transform":{
		"scale":{"z":1,"y":1,"x":1},"rotation":{"z":-0,"y":0,"x":0},"position":{"z":0,"y":0,"x":1.0000000001}},
		"name":"y","kind":"prop","id":"o1"}],"ground":{"height":0,"opacity":1.0000000002,"visible":true},"schemaVersion":1}`)
	fa, err := RenderFingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := RenderFingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fa != fb {
		t.Fatalf("键序、-0 与 1e-10 级噪声不该改变指纹：%s vs %s", fa, fb)
	}
	c := []byte(`{"schemaVersion":1,"ground":{"visible":true,"opacity":1,"height":0},
		"objects":[{"id":"o1","kind":"prop","name":"x","transform":{"position":{"x":1.001,"y":0,"z":0},
		"rotation":{"x":0,"y":0,"z":0},"scale":{"x":1,"y":1,"z":1}},"prop":{"shape":"box","size":{"x":1,"y":1,"z":1}}}]}`)
	fc, _ := RenderFingerprint(c)
	if fc == fa {
		t.Fatal("挪动 1 毫米应当改变指纹")
	}
}

// TestRenderFingerprint_隐藏对象不计入：隐藏的非机位对象不出现在画面里，改它不影响指纹。
func TestRenderFingerprint_隐藏对象不计入(t *testing.T) {
	scene := func(x float64) []byte {
		b, _ := json.Marshal(map[string]any{"schemaVersion": 1, "objects": []any{map[string]any{
			"id": "o1", "name": "x", "kind": KindProp, "visible": false,
			"transform": transform(vec3(x, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
			"prop":      map[string]any{"shape": PropBox, "size": vec3(1, 1, 1)},
		}}})
		return b
	}
	a, _ := RenderFingerprint(scene(0))
	b, _ := RenderFingerprint(scene(5))
	if a != b {
		t.Fatal("隐藏对象的位置变化不该改变指纹")
	}
}

func TestRenderFingerprint_非对象报错(t *testing.T) {
	if _, err := RenderFingerprint([]byte(`[1]`)); !errors.Is(err, ErrNotObject) {
		t.Fatalf("期望 ErrNotObject，实际 %v", err)
	}
}
