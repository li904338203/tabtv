package domain

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fixtureDir 是前后端与 E2E 共用的营销 fixture（dev/02 §7）。
const fixtureDir = "../../../../api/fixtures/marketing"

// globFixtures 按模式遍历 fixture：新增用例只加文件，不改测试代码。
func globFixtures(t *testing.T, pattern string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(fixtureDir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("没有找到 fixture：%s/%s", fixtureDir, pattern)
	}
	return files
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestCompose_fixture(t *testing.T) {
	for _, f := range globFixtures(t, "compose-order*.json") {
		var fx struct {
			Snapshots  []Snapshot `json:"snapshots"`
			Constraint string     `json:"constraint"`
			Expected   string     `json:"expected"`
		}
		readJSON(t, f, &fx)
		t.Run(filepath.Base(f), func(t *testing.T) {
			got := Compose(fx.Snapshots, fx.Constraint)
			if got != fx.Expected {
				t.Fatalf("拼接结果不一致\n got: %q\nwant: %q", got, fx.Expected)
			}
			// 按出现位置断言顺序（MKT-AC-07 的纯函数部分）。
			pos := -1
			for _, k := range Kinds {
				for _, s := range fx.Snapshots {
					if s.Kind != k {
						continue
					}
					i := strings.Index(got, s.Prompt)
					if i <= pos {
						t.Fatalf("%s 片段位置 %d 不在前一段 %d 之后", k, i, pos)
					}
					pos = i
				}
			}
			if i := strings.Index(got, fx.Constraint); i <= pos {
				t.Fatalf("约束段位置 %d 不在风格段 %d 之后", i, pos)
			}
		})
	}
}

func TestSlotSummary_fixture(t *testing.T) {
	for _, f := range globFixtures(t, "slot-summary*.json") {
		var fx struct {
			SceneInput SummaryInput    `json:"sceneInput"`
			Options    []SummaryOption `json:"options"`
			Expected   string          `json:"expected"`
		}
		readJSON(t, f, &fx)
		t.Run(filepath.Base(f), func(t *testing.T) {
			in := fx.SceneInput
			in.Options = fx.Options
			if got := SlotSummary(in); got != fx.Expected {
				t.Fatalf("摘要与 fixture 不逐字一致\n got: %q\nwant: %q", got, fx.Expected)
			}
		})
	}
}

type issueRef struct {
	Field string `json:"field"`
	Code  string `json:"code"`
}

func refsOf(issues []Issue) []issueRef {
	out := make([]issueRef, 0, len(issues))
	for _, i := range issues {
		out = append(out, issueRef{Field: i.Field, Code: string(i.Code)})
	}
	return out
}

func TestCheckTalkCopy_fixture(t *testing.T) {
	for _, f := range globFixtures(t, "talk-copy*.json") {
		var fx struct {
			Cases []struct {
				Description string            `json:"description"`
				Fields      map[string]string `json:"fields"`
				Tags        []string          `json:"tags"`
				Errors      []issueRef        `json:"errors"`
				Warnings    []issueRef        `json:"warnings"`
			} `json:"cases"`
		}
		readJSON(t, f, &fx)
		if len(fx.Cases) == 0 {
			t.Fatalf("%s 没有用例", f)
		}
		for _, c := range fx.Cases {
			t.Run(c.Description, func(t *testing.T) {
				errs, warns := CheckTalkCopy(c.Fields, c.Tags)
				if g, w := mustJSON(t, refsOf(errs)), mustJSON(t, nonNil(c.Errors)); g != w {
					t.Fatalf("errors\n got: %s\nwant: %s", g, w)
				}
				if g, w := mustJSON(t, refsOf(warns)), mustJSON(t, nonNil(c.Warnings)); g != w {
					t.Fatalf("warnings\n got: %s\nwant: %s", g, w)
				}
				for _, e := range errs {
					if e.Level != LevelError {
						t.Fatalf("禁用词必须是 error：%+v", e)
					}
				}
				for _, w := range warns {
					if w.Level != LevelWarn {
						t.Fatalf("告警必须是 warn：%+v", w)
					}
				}
			})
		}
	}
}

func nonNil(r []issueRef) []issueRef {
	if r == nil {
		return []issueRef{}
	}
	return r
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDomain_不依赖其他业务域：marketing 不 import agent / generation / catalog / canvas
// 或任何其他 internal 包（MKD-1）。domain 层只许用标准库。
func TestDomain_不依赖其他业务域(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range af.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(p, ".") {
				t.Errorf("%s import 了非标准库包 %s：marketing/domain 只许用标准库", f, p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有检查到任何源文件")
	}
}

var longDigits = regexp.MustCompile(`\d{6,}`)

func TestSlotSummary_不含URL与ID(t *testing.T) {
	in := SummaryInput{
		ProductName: "山野冷萃", SellingPoints: "即溶", ProductImageCount: 20, VisibleImageCount: 4,
		Characters: []SummaryCharacter{{AssetName: "林澈"}, {}},
		Options: []SummaryOption{{Kind: KindCreative, Name: "巨物", Description: "落地",
			RecommendedModels: []string{"Seedance 2.5"}, AspectRatio: "9:16", DurationSec: []int{10, 15}}},
		MaxVariants: 4,
	}
	got := SlotSummary(in)
	for _, bad := range []string{"http", "mediaId", "media_id", "objectKey", "【"} {
		if strings.Contains(got, bad) {
			t.Fatalf("摘要不得含 %q：\n%s", bad, got)
		}
	}
	if longDigits.MatchString(got) {
		t.Fatalf("摘要里出现了疑似 ID 的长数字：\n%s", got)
	}
	// 结构体上就没有片段字段：拼进来的只能是名称与说明。
	if strings.Contains(got, "\n\n") {
		t.Fatalf("摘要不应有空行：\n%s", got)
	}
}
