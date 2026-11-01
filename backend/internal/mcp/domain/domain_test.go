package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

func TestResolveToolsets_请求头只能收窄凭据(t *testing.T) {
	grant := []string{ToolsetContext, ToolsetCanvas}
	cases := []struct {
		name, header string
		want         []string
	}{
		{"头里点名凭据外的工具集被丢掉", "canvas,generation,script", []string{ToolsetCanvas}},
		{"全在凭据外得到空集", "generation", []string{}},
		{"头里是凭据子集", " context ", []string{ToolsetContext}},
		{"只有逗号等于没写", " , ,", []string{ToolsetContext, ToolsetCanvas}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveToolsets(grant, c.header, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("得到 %v，期望 %v", got, c.want)
			}
		})
	}
}

func TestResolveToolsets_路径优先于请求头(t *testing.T) {
	grant := Toolsets()
	got, err := ResolveToolsets(grant, "canvas,projects", "script")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{ToolsetScript}) {
		t.Fatalf("得到 %v", got)
	}
	// 路径也不能放大凭据。
	got, err = ResolveToolsets([]string{ToolsetCanvas}, "", "script")
	if err != nil || len(got) != 0 {
		t.Fatalf("路径放大了凭据：%v %v", got, err)
	}
}

func TestResolveToolsets_未知工具集报错(t *testing.T) {
	cases := []struct{ header, path, source string }{
		{"canvas,nope", "", "header"},
		{"", "writing", "path"},
		{"nope", "canvas", ""}, // 有路径时头不参与 → 不报错
	}
	for _, c := range cases {
		_, err := ResolveToolsets(Toolsets(), c.header, c.path)
		var ue *UnknownToolsetError
		if c.source == "" {
			if err != nil {
				t.Fatalf("%+v 不该报错：%v", c, err)
			}
			continue
		}
		if !errors.As(err, &ue) || ue.Source != c.source {
			t.Fatalf("%+v 期望 %s 来源的未知工具集错误，得到 %v", c, c.source, err)
		}
		if !strings.Contains(err.Error(), "可选") {
			t.Fatalf("错误要列出可选值：%v", err)
		}
	}
	// 凭据里的未知名丢弃不报错（库里的历史值）。
	got, err := ResolveToolsets([]string{"legacy", ToolsetContext}, "", "")
	if err != nil || !reflect.DeepEqual(got, []string{ToolsetContext}) {
		t.Fatalf("凭据未知名：%v %v", got, err)
	}
}

func TestResolveToolsets_默认开启集(t *testing.T) {
	want := []string{ToolsetContext, ToolsetProjects, ToolsetCanvas, ToolsetCatalog, ToolsetGeneration}
	if !reflect.DeepEqual(DefaultToolsets(), want) {
		t.Fatalf("默认开启集 %v", DefaultToolsets())
	}
	got, err := ResolveToolsets(Toolsets(), "", "")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("凭据全开时 = 默认集：%v %v", got, err)
	}
	// 默认集也要与凭据取交集；返回按定义顺序而非凭据顺序。
	got, _ = ResolveToolsets([]string{ToolsetScript, ToolsetGeneration, ToolsetContext}, "", "")
	if !reflect.DeepEqual(got, []string{ToolsetContext, ToolsetGeneration}) {
		t.Fatalf("得到 %v", got)
	}
}

type page struct {
	Items []item `json:"items"`
	Next  string `json:"nextCursor"`
}
type item struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func bigPage(n int) page {
	p := page{Next: "c1"}
	for i := 0; i < n; i++ {
		p.Items = append(p.Items, item{ID: strings.Repeat("9", 19), Text: strings.Repeat("中文<&>", 20)})
	}
	return p
}

func TestTruncate_未超限原样返回(t *testing.T) {
	v := bigPage(2)
	got, cut := Truncate(v, 6000)
	if cut {
		t.Fatal("不该截断")
	}
	want, _ := marshal(v)
	if string(got) != string(want) {
		t.Fatalf("原样返回被改写：%s", got)
	}
	if strings.Contains(string(got), "\\u003c") {
		t.Fatal("不该转义 HTML 字符")
	}
	// maxTokens <= 0 视为不限。
	if _, cut := Truncate(bigPage(500), 0); cut {
		t.Fatal("0 应不限")
	}
}

func TestTruncate_超限后仍是合法JSON(t *testing.T) {
	cases := []any{
		bigPage(500),
		map[string]any{"a": []any{1, 2, 3}, "b": map[string]any{"c": []any{strings.Repeat("x", 9000), "y"}}},
		[]any{strings.Repeat("x", 5000), strings.Repeat("y", 5000)},
		map[string]any{"nested": []any{[]any{strings.Repeat("x", 5000)}, []any{strings.Repeat("y", 5000)}}},
		map[string]any{"big": json.Number("12345678901234567890123"), "list": []any{strings.Repeat("z", 9000)}},
	}
	for i, c := range cases {
		got, cut := Truncate(c, 1000)
		if !cut {
			t.Fatalf("#%d 应截断", i)
		}
		if !json.Valid(got) {
			t.Fatalf("#%d 截断后不是合法 JSON：%s", i, got)
		}
		if EstimateTokens(got) > 1000 {
			t.Fatalf("#%d 截断后仍超限：%d", i, EstimateTokens(got))
		}
	}
	// 大整数不丢精度。
	got, _ := Truncate(cases[4], 1000)
	if !strings.Contains(string(got), "12345678901234567890123") {
		t.Fatalf("大整数精度丢失：%s", got)
	}
	// 同一输入两次截断结果一致。
	a, _ := Truncate(bigPage(500), 1000)
	b, _ := Truncate(bigPage(500), 1000)
	if string(a) != string(b) {
		t.Fatal("截断结果不稳定")
	}
}

func TestTruncate_带truncated与提示(t *testing.T) {
	got, cut := Truncate(bigPage(500), 1000)
	if !cut {
		t.Fatal("应截断")
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if m[FieldTruncated] != true {
		t.Fatalf("缺 truncated：%v", m[FieldTruncated])
	}
	h, _ := m[FieldTruncationHint].(string)
	if !strings.Contains(h, "items 只保留前") || !strings.Contains(h, "/500 项") || !strings.Contains(h, "cursor") {
		t.Fatalf("提示要说清哪个数组保留了几项、怎么精确读取：%q", h)
	}
	items, _ := m["items"].([]any)
	if len(items) == 0 || len(items) >= 500 {
		t.Fatalf("应保留部分项：%d", len(items))
	}
	if m["nextCursor"] != "c1" {
		t.Fatal("非数组字段不该被动")
	}
	// 没有数组可删时原样返回、不标 truncated。
	raw, cut := Truncate(map[string]any{"s": strings.Repeat("x", 9000)}, 100)
	if cut || strings.Contains(string(raw), FieldTruncated) {
		t.Fatal("没有数组时不该标截断")
	}
}

func TestScopeFor_四个动作一一对应(t *testing.T) {
	want := map[collab.Action]string{
		collab.ActionView: ScopeView, collab.ActionEdit: ScopeEdit,
		collab.ActionGenerate: ScopeGenerate, collab.ActionExport: ScopeExport,
	}
	seen := map[string]bool{}
	for a, s := range want {
		got, ok := ScopeFor(a)
		if !ok || got != s {
			t.Fatalf("%s → %q,%v", a, got, ok)
		}
		seen[got] = true
	}
	if len(seen) != len(Scopes()) {
		t.Fatal("scope 与动作不是一一对应")
	}
	for _, a := range []collab.Action{collab.ActionManage, collab.ActionTransfer, ""} {
		if _, ok := ScopeFor(a); ok {
			t.Fatalf("%q 不该有 scope", a)
		}
	}
}
