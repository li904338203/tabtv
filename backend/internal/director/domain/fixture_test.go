package domain

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fixtureDir 是前后端共用的 fixture 目录（同 canvas/domain/ops_test.go 读 canvas-ops 的写法）。
const fixtureDir = "../../../../api/fixtures/director"

// updateFixtures 为真时，由代码重写「代码产出的那几份」fixture（catalog.json、scenes/valid-default.json）。
// 只在有意改目录或默认场景时用：DIRECTOR_FIXTURE_UPDATE=1 go test ./internal/director/domain/...
// 重写之后要同步前端 utils/directorCatalog.ts（DRT-11），否则前端的绊线会红 —— 这是有意的。
var updateFixtures = os.Getenv("DIRECTOR_FIXTURE_UPDATE") == "1"

func fixturePath(parts ...string) string {
	return filepath.Join(append([]string{fixtureDir}, parts...)...)
}

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath(parts...))
	if err != nil {
		t.Fatalf("读 fixture %v 失败：%v", parts, err)
	}
	return b
}

func writeFixture(t *testing.T, data []byte, parts ...string) {
	t.Helper()
	p := fixturePath(parts...)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func indentJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

func asAny(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("不是合法 JSON：%v", err)
	}
	return v
}

// TestCatalog_与共享fixture一致：Go 常量与 catalog.json 逐项相同（DRD-29）。
// 比的是解析后的值（键集合、顺序、数值），多一项、少一项、改一个字都红。
func TestCatalog_与共享fixture一致(t *testing.T) {
	got, err := json.MarshalIndent(BuildCatalog(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if updateFixtures {
		writeFixture(t, append(got, '\n'), "catalog.json")
	}
	want := readFixture(t, "catalog.json")
	if !reflect.DeepEqual(asAny(t, got), asAny(t, want)) {
		t.Fatalf("Go 目录与 api/fixtures/director/catalog.json 不一致；有意修改时用 DIRECTOR_FIXTURE_UPDATE=1 重写，并同步前端")
	}
}

func TestCatalog_姿势覆盖六类且不少于30个(t *testing.T) {
	c := BuildCatalog()
	if len(c.Poses) < 30 {
		t.Fatalf("姿势只有 %d 个，DR-PARAM-15 要求不少于 30", len(c.Poses))
	}
	cats := map[string]int{}
	codes := map[string]bool{}
	for _, p := range c.Poses {
		if codes[p.Code] {
			t.Errorf("姿势码重复：%s", p.Code)
		}
		codes[p.Code] = true
		cats[p.Category]++
		if len([]rune(p.Description)) > 40 {
			t.Errorf("姿势 %s 的说明超过 40 字", p.Code)
		}
	}
	for _, cat := range c.PoseCategories {
		if cats[cat.Code] == 0 {
			t.Errorf("类别 %s 没有姿势", cat.Code)
		}
	}
	if len(cats) != len(c.PoseCategories) {
		t.Errorf("有姿势用了不存在的类别：%v", cats)
	}
}

func TestCatalog_景别按coverage从远到近(t *testing.T) {
	c := BuildCatalog()
	if len(c.ShotSizes) != 8 {
		t.Fatalf("景别应为 8 档，实际 %d", len(c.ShotSizes))
	}
	for i := 1; i < len(c.ShotSizes); i++ {
		if c.ShotSizes[i].Coverage >= c.ShotSizes[i-1].Coverage {
			t.Errorf("景别 %s 的 coverage 没有小于前一档", c.ShotSizes[i].Code)
		}
	}
}

// TestDefaultScene_与fixture逐字相等：fixture 是 DefaultScene() 经 json.Indent（两空格）的结果加换行。
func TestDefaultScene_与fixture逐字相等(t *testing.T) {
	got := indentJSON(t, DefaultScene())
	if updateFixtures {
		writeFixture(t, got, "scenes", "valid-default.json")
	}
	want := readFixture(t, "scenes", "valid-default.json")
	if !bytes.Equal(got, want) {
		t.Fatalf("DefaultScene() 与 scenes/valid-default.json 不一致：\n%s", got)
	}
	if err := Validate(DefaultScene()); err != nil {
		t.Fatalf("默认场景没有通过校验：%v", err)
	}
}

func TestDefaultScene_一个输出机位一个角色(t *testing.T) {
	s, err := ParseScene(DefaultScene())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.OutputCameraID(); got != DefaultCameraID {
		t.Fatalf("输出机位应为 %s，实际 %q", DefaultCameraID, got)
	}
	if len(s.Cameras()) != 1 || len(s.Objects()) != 2 {
		t.Fatalf("默认场景应为 1 机位 1 角色，实际 %d 个对象", len(s.Objects()))
	}
	if s.SchemaVersion() != CurrentSchemaVersion {
		t.Fatalf("结构版本应为 %d", CurrentSchemaVersion)
	}
}
