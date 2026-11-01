package domain

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

func readRaw(t *testing.T, name string) string {
	t.Helper()
	return string(readFixture(t, "build", name))
}

func mustParse(t *testing.T, raw string) (Draft, []string) {
	t.Helper()
	d, dropped, err := ParseDraft(raw)
	if err != nil {
		t.Fatalf("应当解析成功：%v", err)
	}
	return d, dropped
}

func joined(list []string) string { return strings.Join(list, "\n") }

func TestParseDraft_代码围栏与首尾杂文(t *testing.T) {
	for _, name := range []string{"raw-fence.txt", "raw-prose.txt"} {
		d, dropped := mustParse(t, readRaw(t, name))
		if len(d.Objects) != 2 || d.Camera == nil {
			t.Errorf("%s：应解出 2 个对象和机位，得到 %d 个对象、机位 %v", name, len(d.Objects), d.Camera != nil)
		}
		if len(dropped) != 0 {
			t.Errorf("%s：干净的方案不该有丢弃说明：%v", name, dropped)
		}
	}
	d, _ := mustParse(t, readRaw(t, "raw-fence.txt"))
	if d.Objects[0].Relation == nil || d.Objects[0].Relation.Type != RelLeftOf || d.Objects[0].Relation.Target != "苏然" {
		t.Errorf("关系没解出来：%+v", d.Objects[0].Relation)
	}
	if len(d.Notes) != 1 || d.Notes[0] != "未指定时间与天气，按白天户外" {
		t.Errorf("模型的 notes 要原样保留：%v", d.Notes)
	}
	// 后面的说明里有 {老板} 这种大括号：按括号配对取，不能取到说明里去。
	p, _ := mustParse(t, readRaw(t, "raw-prose.txt"))
	if p.Objects[1].Prop == nil || p.Objects[1].Prop.Size == nil || p.Objects[1].Prop.Size.Y != 0.75 {
		t.Errorf("道具尺寸没解出来：%+v", p.Objects[1].Prop)
	}
}

func TestParseDraft_末尾逗号(t *testing.T) {
	d, dropped := mustParse(t, readRaw(t, "raw-trailing-comma.txt"))
	if len(d.Objects) != 2 || d.Camera == nil || d.Camera.Angle != "low" {
		t.Fatalf("末尾逗号应当救回来：%+v", d)
	}
	if len(dropped) != 0 {
		t.Errorf("不该有丢弃说明：%v", dropped)
	}
}

func TestParseDraft_白名单外的码丢弃并说明(t *testing.T) {
	d, dropped := mustParse(t, readRaw(t, "raw-unknown-codes.txt"))
	all := joined(dropped)
	for _, code := range []string{"robot", "yoga_tree", "animal", "balloon", "above", "teleport", "wide", "bird_eye", "500"} {
		if !strings.Contains(all, code) {
			t.Errorf("丢弃说明里要写出「%s」：\n%s", code, all)
		}
	}
	if len(d.Objects) != 2 {
		t.Fatalf("未知 kind 与未知 op 的项整项丢弃，剩 2 项，得到 %d", len(d.Objects))
	}
	teacher := d.Objects[0]
	if teacher.Model != "" || teacher.PosePreset != "" {
		t.Errorf("白名单外的素模与姿势要丢掉，交给默认值：%+v", teacher)
	}
	balloon := d.Objects[1]
	if balloon.Kind != KindProp || balloon.Prop == nil || balloon.Prop.Shape != "" || balloon.Relation != nil {
		t.Errorf("气球保留为道具，但形状与关系丢掉：%+v", balloon)
	}
	if d.Camera.ShotSize != "" || d.Camera.Angle != "" || d.Camera.Side != "front" || d.Camera.FocalLengthMm != 0 {
		t.Errorf("机位里白名单外的字段丢掉、合法的保留：%+v", d.Camera)
	}
}

func TestParseDraft_坐标一律忽略并说明(t *testing.T) {
	d, dropped := mustParse(t, readRaw(t, "raw-coordinates.txt"))
	all := joined(dropped)
	for _, want := range []string{"「哨兵」带了坐标（position、rotation）", "「岗亭」带了坐标（x、z）", "camera 带了坐标（position）"} {
		if !strings.Contains(all, want) {
			t.Errorf("丢弃说明里要有「%s」：\n%s", want, all)
		}
	}
	if len(d.Camera.Targets) != 1 || d.Camera.Targets[0] != "哨兵" {
		t.Errorf("机位目标写成单个字符串也要认：%+v", d.Camera)
	}
}

func TestParseDraft_全部丢光报错(t *testing.T) {
	for _, name := range []string{"raw-empty.txt", "raw-all-invalid.txt"} {
		_, dropped, err := ParseDraft(readRaw(t, name))
		var invalid *ErrDraftInvalid
		if !errors.As(err, &invalid) {
			t.Fatalf("%s：应返回 ErrDraftInvalid，得到 %v", name, err)
		}
		if !strings.Contains(invalid.Reason, "模型没有给出可用的搭景方案") {
			t.Errorf("%s：原因要说清楚：%q", name, invalid.Reason)
		}
		if invalid.Snippet == "" {
			t.Errorf("%s：要带原文片段", name)
		}
		if name == "raw-all-invalid.txt" && len(dropped) != 3 {
			t.Errorf("每一项为什么被丢都要说：%v", dropped)
		}
	}
	// 只有说明、没有对象也没有机位：同样是失败。
	if _, _, err := ParseDraft(`{"notes":["我觉得这样就挺好"]}`); err == nil {
		t.Error("只有 notes 的方案不能当成功")
	}
	// notes 写成一句话也认。
	d, _ := mustParse(t, `{"objects":[{"op":"add","ref":"甲","kind":"character"}],"notes":"按白天处理"}`)
	if len(d.Notes) != 1 || d.Notes[0] != "按白天处理" {
		t.Errorf("notes 写成字符串也要认：%v", d.Notes)
	}
}

func TestParseDraft_截断原文带片段(t *testing.T) {
	raw := readRaw(t, "raw-truncated.txt")
	if utf8.RuneCountInString(raw) <= snippetChars {
		t.Fatalf("fixture 要超过 %d 字才能核对片段长度", snippetChars)
	}
	_, _, err := ParseDraft(raw)
	var invalid *ErrDraftInvalid
	if !errors.As(err, &invalid) {
		t.Fatalf("截断的输出救不回来，应返回 ErrDraftInvalid：%v", err)
	}
	want := string([]rune(strings.TrimSpace(raw))[:snippetChars])
	if invalid.Snippet != want {
		t.Errorf("Snippet 应为原文前 %d 字，得到 %d 字", snippetChars, utf8.RuneCountInString(invalid.Snippet))
	}
	if !strings.Contains(invalid.Error(), invalid.Snippet) {
		t.Error("错误信息要带上片段")
	}
}

func TestParseDraft_通篇文字说清楚(t *testing.T) {
	_, _, err := ParseDraft(readRaw(t, "raw-text-only.txt"))
	var invalid *ErrDraftInvalid
	if !errors.As(err, &invalid) || !strings.Contains(invalid.Reason, "没有按要求输出 JSON") {
		t.Fatalf("模型在反问时要说清「没写 JSON」：%v", err)
	}
	if strings.Contains(invalid.Error(), "invalid character") {
		t.Errorf("不能把 encoding/json 的原始错误（像乱码）给用户：%v", invalid)
	}
}

func TestParseDraft_缺省字段的推断(t *testing.T) {
	d, dropped := mustParse(t, `{"objects":[
		{"ref":"桌子","shape":"box"},
		{"ref":"人群","rows":2,"cols":3},
		{"ref":"阿青"},
		{"op":"update","posePreset":"wave"},
		{"op":"add","ref":"阿蓝","kind":"character","facing":"camera"}
	]}`)
	if len(d.Objects) != 4 {
		t.Fatalf("没写 ref 的 update 要丢掉，其余保留：%+v", d.Objects)
	}
	// 没写 op、也没写 kind：op 留给 Solve 按 ref 推断，kind 按字段推断。
	if d.Objects[0].Op != "" || d.Objects[0].Prop == nil {
		t.Errorf("没写 op 的项保留为空，交给 Solve：%+v", d.Objects[0])
	}
	if d.Objects[3].Facing == nil || d.Objects[3].Facing.Target != FacingCamera {
		t.Errorf("facing 直接写成字符串也要认：%+v", d.Objects[3].Facing)
	}
	if !strings.Contains(joined(dropped), "没有写 ref") {
		t.Errorf("丢弃说明里要写为什么丢：%v", dropped)
	}
}

func TestParseDraft_数值超出范围丢掉该项(t *testing.T) {
	d, dropped := mustParse(t, `{"objects":[
		{"op":"add","ref":"甲","kind":"character","relation":{"type":"near","target":"乙","distance":999}},
		{"op":"add","ref":"阵","kind":"crowd","rows":20,"cols":20,"spacing":0.1},
		{"op":"add","ref":"板","kind":"prop","shape":"plane","size":{"x":0,"y":1,"z":1}}
	],"camera":{"focalLengthMm":5}}`)
	all := joined(dropped)
	for _, want := range []string{"关系距离「999」超出范围", "行列数", "间距「0.1」超出范围", "道具尺寸", "焦距「5」超出范围"} {
		if !strings.Contains(all, want) {
			t.Errorf("丢弃说明里要有「%s」：\n%s", want, all)
		}
	}
	if d.Objects[0].Relation == nil || d.Objects[0].Relation.Distance != 0 {
		t.Errorf("超范围的距离丢掉（按默认距离），关系本身保留：%+v", d.Objects[0].Relation)
	}
	if c := d.Objects[1].Crowd; c == nil || c.Rows != 0 || c.Cols != 0 || c.Spacing != 0 {
		t.Errorf("超范围的群众参数丢掉：%+v", c)
	}
}

// TestParseDraft_raw用例全部符合期望：遍历 build/raw-*.txt，期望写在 raw-expect.json（DRT-30 的假上游也用这些原文）。
func TestParseDraft_raw用例全部符合期望(t *testing.T) {
	var expect map[string]struct {
		OK              bool     `json:"ok"`
		Objects         int      `json:"objects"`
		Camera          bool     `json:"camera"`
		Notes           *int     `json:"notes"`
		DroppedCount    *int     `json:"droppedCount"`
		DroppedContains []string `json:"droppedContains"`
		ReasonContains  string   `json:"reasonContains"`
	}
	if err := json.Unmarshal(readFixture(t, "build", "raw-expect.json"), &expect); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(fixturePath("build", "raw-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 6 {
		t.Fatalf("raw 用例只有 %d 个，至少要 6 个", len(files))
	}
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	sort.Strings(names)
	for _, name := range names {
		e, ok := expect[name]
		if !ok {
			t.Errorf("%s 没有在 raw-expect.json 里写期望", name)
			continue
		}
		raw, err := os.ReadFile(fixturePath("build", name))
		if err != nil {
			t.Fatal(err)
		}
		d, dropped, err := ParseDraft(string(raw))
		if !e.OK {
			var invalid *ErrDraftInvalid
			if !errors.As(err, &invalid) {
				t.Errorf("%s：应返回 ErrDraftInvalid，得到 %v", name, err)
				continue
			}
			if !strings.Contains(invalid.Reason, e.ReasonContains) {
				t.Errorf("%s：原因应含「%s」，得到 %q", name, e.ReasonContains, invalid.Reason)
			}
			if e.DroppedCount != nil && len(dropped) != *e.DroppedCount {
				t.Errorf("%s：丢弃说明应有 %d 条，得到 %v", name, *e.DroppedCount, dropped)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s：应当解析成功：%v", name, err)
			continue
		}
		if len(d.Objects) != e.Objects || (d.Camera != nil) != e.Camera {
			t.Errorf("%s：期望 %d 个对象、机位 %v，得到 %d 个、%v", name, e.Objects, e.Camera, len(d.Objects), d.Camera != nil)
		}
		if e.Notes != nil && len(d.Notes) != *e.Notes {
			t.Errorf("%s：notes 应有 %d 条，得到 %v", name, *e.Notes, d.Notes)
		}
		if e.DroppedCount != nil && len(dropped) != *e.DroppedCount {
			t.Errorf("%s：丢弃说明应有 %d 条，得到 %v", name, *e.DroppedCount, dropped)
		}
		for _, want := range e.DroppedContains {
			if !strings.Contains(joined(dropped), want) {
				t.Errorf("%s：丢弃说明里要有「%s」：\n%s", name, want, joined(dropped))
			}
		}
	}
	for name := range expect {
		if _, err := os.Stat(fixturePath("build", name)); err != nil {
			t.Errorf("raw-expect.json 里的 %s 没有对应的文件", name)
		}
	}
}
