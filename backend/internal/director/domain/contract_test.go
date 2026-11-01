package domain

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestOutputContract_以固定标题开头(t *testing.T) {
	c := OutputContract()
	if !strings.HasPrefix(c, "## 输出格式（导演台搭景）\n") || ContractHeading != "## 输出格式（导演台搭景）" {
		t.Fatalf("契约必须以固定标题开头（假上游的绊线，dev/01 §5）：%q", c[:40])
	}
	if strings.Contains(c, "分镜") {
		t.Error("契约里不能出现「分镜」：假上游看到它就回分镜 JSON（dev/01 §5）")
	}
	if OutputContract() != c {
		t.Error("同一版本的代码每次返回同一段契约")
	}
}

var codeToken = regexp.MustCompile("`([a-z_]+)`")

// TestOutputContract_码表与目录一致：「可用的码」里每一节的码集合与目录完全相同，多一个少一个都红。
func TestOutputContract_码表与目录一致(t *testing.T) {
	c := OutputContract()
	_, codes, ok := strings.Cut(c, "### 可用的码")
	if !ok {
		t.Fatal("契约里没有「可用的码」一节")
	}
	sections := map[string][]string{}
	for _, part := range strings.Split(codes, "#### ")[1:] {
		title, body, _ := strings.Cut(part, "\n")
		for _, m := range codeToken.FindAllStringSubmatch(body, -1) {
			sections[strings.TrimSpace(title)] = append(sections[strings.TrimSpace(title)], m[1])
		}
	}
	labeledCodes := func(list []Labeled) []string {
		var out []string
		for _, x := range list {
			out = append(out, x.Code)
		}
		return out
	}
	var modelCodes, poseCodes, shotCodes []string
	for _, m := range models {
		modelCodes = append(modelCodes, m.Code)
	}
	for _, p := range poses {
		poseCodes = append(poseCodes, p.Code)
	}
	for _, s := range shotSizes {
		shotCodes = append(shotCodes, s.Code)
	}
	want := map[string][]string{
		"素模 model":           modelCodes,
		"体型 body":            labeledCodes(bodies),
		"姿势 posePreset":      poseCodes,
		"道具形状 shape":         propShapes,
		"位置关系 relation.type": relations,
		"景别 shotSize":        shotCodes,
		"角度 angle":           labeledCodes(angles),
		"方位 side":            labeledCodes(sides),
	}
	if len(sections) != len(want) {
		t.Errorf("「可用的码」应有 %d 节，实际 %d：%v", len(want), len(sections), keysOf(sections))
	}
	for title, codes := range want {
		got := append([]string(nil), sections[title]...)
		exp := append([]string(nil), codes...)
		sort.Strings(got)
		sort.Strings(exp)
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("「%s」的码与目录不一致：\n契约 %v\n目录 %v", title, got, exp)
		}
	}
	// 补充说明表的键必须与目录一致：目录加了码而说明没加，契约里那一行就只剩一个码。
	for name, m := range map[string]map[string]string{"propShapeNames": propShapeNames, "shotSizeHints": shotSizeHints,
		"relationHints": relationHints, "sideHints": sideHints} {
		var keys []string
		for k, v := range m {
			if v == "" {
				t.Errorf("%s[%s] 是空的", name, k)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var exp []string
		switch name {
		case "propShapeNames":
			exp = append(exp, propShapes...)
		case "shotSizeHints":
			exp = append(exp, shotCodes...)
		case "relationHints":
			exp = append(exp, relations...)
		case "sideHints":
			exp = labeledCodes(sides)
		}
		sort.Strings(exp)
		if !reflect.DeepEqual(keys, exp) {
			t.Errorf("%s 的键与目录不一致：%v / %v", name, keys, exp)
		}
	}
	// 景别要带中文名：避免模型把英文 close-up 理解成近景。
	for _, s := range shotSizes {
		if !strings.Contains(c, fmt.Sprintf("`%s` %s：", s.Code, s.Name)) {
			t.Errorf("景别 %s 要带中文名「%s」与画面说明", s.Code, s.Name)
		}
	}
}

func keysOf(m map[string][]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestOutputContract_示例能被解析且无丢弃：示例里出现目录外的码，等于教模型写会被丢掉的东西。
func TestOutputContract_示例能被解析且无丢弃(t *testing.T) {
	c := OutputContract()
	if !strings.Contains(c, contractExample) {
		t.Fatal("契约要带示例")
	}
	d, dropped, err := ParseDraft(contractExample)
	if err != nil || len(dropped) != 0 {
		t.Fatalf("示例应能干净地解析：%v %v", err, dropped)
	}
	if len(d.Objects) != 4 || d.Camera == nil || len(d.Notes) != 1 {
		t.Errorf("示例应覆盖增改删、机位与 notes：%+v", d)
	}
}

func TestSceneSummary_标出可替换与锁定(t *testing.T) {
	s := SceneSummary(mixedScene(t))
	for _, want := range []string{
		"obj_user｜用户角色｜角色｜成年男｜姿势未设置｜用户摆的",
		"obj_locked｜锁定道具｜道具｜方块｜已锁定",
		"obj_ai｜AI 角色｜角色｜成年男｜姿势 walk（行走）｜可替换",
		"obj_cam｜机位 1｜机位｜35mm｜16:9｜可替换｜输出机位",
		"对象（共 4 个）", "机位（共 1 个）", "地面高度 0 米",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("摘要里应有「%s」：\n%s", want, s)
		}
	}
	if strings.Contains(s, "分镜") {
		t.Error("摘要里不要出现「分镜」（假上游的判断词）")
	}
	// 空场景。
	empty := SceneSummary(emptyScene(t))
	if !strings.Contains(empty, "还没有角色、群众或道具") || !strings.Contains(empty, "还没有机位") {
		t.Errorf("空场景要说清楚：\n%s", empty)
	}
	// 群众、隐藏、目录外的姿势码（前端比后端新）照样列出。
	crowd := sceneOf(t, map[string]any{"schemaVersion": 1, "objects": []any{
		map[string]any{"id": "obj_c", "name": "人群", "kind": KindCrowd, "visible": false, "source": SourceAI,
			"transform": transform(vec3(0, 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
			"crowd":     map[string]any{"model": ModelYouthFemale, "rows": 3, "cols": 4, "spacing": 1, "posePreset": "future_pose"}},
	}})
	if got := SceneSummary(crowd); !strings.Contains(got, "obj_c｜人群｜群众｜3×4｜少年女｜姿势 future_pose｜可替换｜隐藏") {
		t.Errorf("群众行不对：\n%s", got)
	}
}

func TestSceneSummary_对象太多时截断(t *testing.T) {
	var objs []any
	for i := 0; i < 75; i++ {
		objs = append(objs, map[string]any{"id": fmt.Sprintf("obj_%d", i), "name": fmt.Sprintf("道具 %d", i), "kind": KindProp,
			"transform": transform(vec3(float64(i), 0, 0), vec3(0, 0, 0), vec3(1, 1, 1)),
			"prop":      map[string]any{"shape": PropSphere, "size": vec3(1, 1, 1)}})
	}
	s := SceneSummary(sceneOf(t, map[string]any{"schemaVersion": 1, "objects": objs}))
	if !strings.Contains(s, "对象（共 75 个）") || !strings.Contains(s, "- 其余 15 个略") {
		t.Errorf("超过 60 个要截断并注明：\n%s", s)
	}
	if strings.Contains(s, "obj_60｜") || !strings.Contains(s, "obj_59｜") {
		t.Error("只列前 60 个")
	}
}
