package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestValidateProjectName_按字符数不按字节数(t *testing.T) {
	// 100 个中文字＝300 字节。用 len() 判长会在第 34 个字就拒掉。
	name := strings.Repeat("镜", 100)
	if _, err := ValidateProjectName(name); err != nil {
		t.Fatalf("100 个中文字应合法，实际 %v", err)
	}
	if _, err := ValidateProjectName(strings.Repeat("镜", 101)); err == nil {
		t.Error("101 字应被拒")
	}
}

func TestValidateProjectName_去空白后为空即非法(t *testing.T) {
	for _, s := range []string{"", "   ", "\t\n"} {
		if _, err := ValidateProjectName(s); err == nil {
			t.Errorf("%q 应被拒", s)
		}
	}
	got, err := ValidateProjectName("  我的项目  ")
	if err != nil || got != "我的项目" {
		t.Fatalf("应去掉首尾空白，得到 %q err=%v", got, err)
	}
}

// 后缀必须加在截断之后，否则长名字复制出来与原项目同名。
func TestCopyName_超长时仍保留副本后缀(t *testing.T) {
	origin := strings.Repeat("长", MaxProjectNameLen)
	got := CopyName(origin)
	if n := utf8.RuneCountInString(got); n > MaxProjectNameLen {
		t.Fatalf("副本名 %d 字超上限", n)
	}
	if !strings.HasSuffix(got, " 副本") {
		t.Fatalf("副本名必须带后缀，实际 %q", got)
	}
	if got == origin {
		t.Fatal("副本名不能与原名相同")
	}
}

// 复制的第一条规则：后端没定义的字段一个都不能丢。
func TestCopyCanvasData_保留未知字段(t *testing.T) {
	raw := json.RawMessage(`{
		"schemaVersion": 2,
		"viewport": {"x": 10, "y": 20, "zoom": 1.5},
		"未来字段": {"a": [1, 2, 3]},
		"nodes": [{"id": "n1", "type": "image", "params": {"prompt": "x", "新参数": true}}],
		"edges": [{"id": "e1", "source": "n1", "target": "n2", "order": 1}]
	}`)
	out, stats, err := CopyCanvasData(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	if err := json.Unmarshal(out, &tree); err != nil {
		t.Fatal(err)
	}
	if _, ok := tree["未来字段"]; !ok {
		t.Error("顶层未知字段被吃掉了")
	}
	if tree["schemaVersion"].(float64) != 2 {
		t.Error("schemaVersion 应原样保留")
	}
	node := tree["nodes"].([]any)[0].(map[string]any)
	params := node["params"].(map[string]any)
	if _, ok := params["新参数"]; !ok {
		t.Error("节点参数里的未知字段被吃掉了")
	}
	edge := tree["edges"].([]any)[0].(map[string]any)
	if edge["order"].(float64) != 1 {
		t.Error("edge order 不能在复制中丢失")
	}
	if stats.Nodes != 1 || stats.Edges != 1 {
		t.Errorf("统计不对: %+v", stats)
	}
}

// 复制的第二条规则：产物必须解绑，但预览要留着。
func TestCopyCanvasData_解绑产物并保留预览(t *testing.T) {
	raw := json.RawMessage(`{"nodes": [
		{"id": "n1", "result": {"id": "9001", "status": "success",
		                        "outputUrl": "https://x/y.png", "outputType": "image"}},
		{"id": "n2", "result": {"status": "queued"}},
		{"id": "n3"}
	]}`)
	out, stats, err := CopyCanvasData(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	_ = json.Unmarshal(out, &tree)
	r := tree["nodes"].([]any)[0].(map[string]any)["result"].(map[string]any)

	if _, ok := r["id"]; ok {
		t.Error("副本仍带着原项目的 generationId —— 它能预览却不能作输入，是个点了才失败的坑")
	}
	if r["sourceGenerationId"] != "9001" {
		t.Errorf("来源应被记下，实际 %v", r["sourceGenerationId"])
	}
	if r["outputUrl"] != "https://x/y.png" || r["outputType"] != "image" {
		t.Error("预览字段不应被清掉，否则副本里全是空白节点")
	}
	if stats.UnboundResults != 1 || stats.PreservedPreview != 1 {
		t.Errorf("统计不对: %+v", stats)
	}
	// 没有 id 的 result、以及根本没有 result 的节点都不应被改动。
	if _, ok := tree["nodes"].([]any)[1].(map[string]any)["result"].(map[string]any)["sourceGenerationId"]; ok {
		t.Error("没有 id 的 result 不该凭空长出 sourceGenerationId")
	}
}

type fakeRebinder struct{ m map[string]string }

func (f fakeRebinder) Rebind(id any) (string, bool) {
	s, _ := id.(string)
	v, ok := f.m[s]
	return v, ok
}

// 生成域接上重绑定后，副本里的产物应拿到工作区级的媒体 ID。
func TestCopyCanvasData_重绑定接缝(t *testing.T) {
	raw := json.RawMessage(`{"nodes": [{"result": {"id": "9001"}}, {"result": {"id": "9002"}}]}`)
	out, _, err := CopyCanvasData(raw, fakeRebinder{m: map[string]string{"9001": "m-1"}})
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	_ = json.Unmarshal(out, &tree)
	nodes := tree["nodes"].([]any)
	if got := nodes[0].(map[string]any)["result"].(map[string]any)["mediaId"]; got != "m-1" {
		t.Errorf("应重绑定到媒体 ID，实际 %v", got)
	}
	// 映射不到的保持解绑态，不能瞎填。
	if _, ok := nodes[1].(map[string]any)["result"].(map[string]any)["mediaId"]; ok {
		t.Error("映射不到时不该编一个 mediaId 出来")
	}
}

func TestCopyCanvasData_空画布与坏数据(t *testing.T) {
	out, _, err := CopyCanvasData(nil, nil)
	if err != nil || string(out) != `{}` {
		t.Fatalf("空画布应得到空对象，实际 %q err=%v", out, err)
	}
	if _, _, err := CopyCanvasData(json.RawMessage(`[1,2]`), nil); err == nil {
		t.Error("画布数据不是对象时应报错，而不是产出半个副本")
	}
}

// 保留期算 deletedAt 不算 updatedAt：这是「删了又恢复又删」的项目永远清不掉的根因。
func TestRetention_按删除时间起算(t *testing.T) {
	const retention = 30 * 24 * time.Hour
	deletedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if got := RetentionDeadline(deletedAt, retention); !got.Equal(want) {
		t.Fatalf("期望 %v，实际 %v", want, got)
	}

	cases := []struct {
		now  time.Time
		want int
	}{
		{deletedAt, 30},
		// ⚠️ 刚删完几毫秒仍应显示 30 天。向下取整会显示 29，
		// 保留期明明是 30 天，用户看到的第一眼就少一天。
		{deletedAt.Add(time.Millisecond), 30},
		{deletedAt.Add(12 * time.Hour), 30},
		{deletedAt.Add(24 * time.Hour), 29},
		{deletedAt.Add(29 * 24 * time.Hour), 1},
		// 不足一天按 1 天显示：显示 0 天会让用户以为已经没救了。
		{deletedAt.Add(retention - time.Hour), 1},
		{deletedAt.Add(retention), 0},
		{deletedAt.Add(retention + 24*time.Hour), 0},
	}
	for _, c := range cases {
		if got := DaysLeft(deletedAt, c.now, retention); got != c.want {
			t.Errorf("now=%v 期望剩余 %d 天，实际 %d", c.now, c.want, got)
		}
	}
}

func TestCanvasIsEmpty(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"空字节", ``, true},
		{"空对象（新建项目的初始画布）", `{}`, true},
		{"空数组", `{"schemaVersion":1,"nodes":[],"edges":[]}`, true},
		{"null", `{"nodes":null,"edges":null}`, true},
		{"只有视口", `{"viewport":{"x":0,"y":0,"zoom":1}}`, true},
		{"有节点", `{"nodes":[{"id":"n1","type":"image"}],"edges":[]}`, false},
		{"只有连线（残留）", `{"nodes":[],"edges":[{"id":"e1"}]}`, false},
		// 读不懂的画布不能当成空白覆盖掉
		{"不是 JSON", `not json`, false},
		{"nodes 不是数组", `{"nodes":{"a":1}}`, false},
	}
	for _, c := range cases {
		if got := CanvasIsEmpty(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("%s：期望 %v，得到 %v", c.name, c.want, got)
		}
	}
}
