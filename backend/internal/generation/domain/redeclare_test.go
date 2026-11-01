package domain

import (
	"encoding/json"
	"testing"
)

// ⚠️ 这条钉住的是一个**真实缺陷**：重试把解析后的快照原样当成声明形态发回
// 受理入口，于是任何带输入的任务重试都报「inputs 不是合法的输入数组」——
// 而那句话指向"客户端传错了参数"，可这次请求里根本没有客户端传的 inputs。
// 纯文生任务没有输入，恰好绕过了它，所以一直没暴露。
func TestRedeclareInputs_数字ID还原成字符串(t *testing.T) {
	snap := json.RawMessage(`[
		{"kind":"reference","order":1,"width":64,"height":32,
		 "mediaId":95967381049835520,"mediaKind":"image",
		 "referenceId":95967381049835520}
	]`)
	out := RedeclareInputs(snap)

	var got []map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("还原结果应是合法 JSON：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应还原 1 条，得到 %d", len(got))
	}
	if got[0]["referenceId"] != "95967381049835520" {
		t.Fatalf("ID 应还原成字符串，得到 %#v", got[0]["referenceId"])
	}
	// ⚠️ **不把解析结果带回去**：重试要重新解析一遍（归属可能变了、产物
	// 可能被删了），带回去的话一个已经失效的 mediaId 会被当成仍然有效。
	for _, k := range []string{"mediaId", "mediaKind", "width", "height"} {
		if _, ok := got[0][k]; ok {
			t.Fatalf("不该把解析结果 %q 带回声明形态：%#v", k, got[0])
		}
	}
}

// ⚠️ 0 要变成**空串**而不是 "0"：写成 "0" 的话受理侧会拿着一个不存在的 ID
// 去查，报"引用的产出不存在"—— 而真相是这条输入本来就没有产出引用。
func TestRedeclareInputs_零值ID不写成0(t *testing.T) {
	snap := json.RawMessage(`[{"kind":"text","order":1,"text":"一段词","generationId":0}]`)
	var got []map[string]any
	if err := json.Unmarshal(RedeclareInputs(snap), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got[0]["generationId"]; ok {
		t.Fatalf("零值 ID 应被省略，得到 %#v", got[0])
	}
	if got[0]["text"] != "一段词" {
		t.Fatalf("文本应保留：%#v", got[0])
	}
}

func TestRedeclareInputs_保留顺序与角色(t *testing.T) {
	// 顺序即首尾帧/参考图的锚定顺序，丢了的话模型会把 A 的特征画到 B 身上。
	// 角色 `mask` 丢了的话涂抹重绘会把蒙版当成第二张原图。
	snap := json.RawMessage(`[
		{"kind":"generation","order":2,"generationId":222,"role":"mask"},
		{"kind":"generation","order":1,"generationId":111}
	]`)
	var got []map[string]any
	if err := json.Unmarshal(RedeclareInputs(snap), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应还原 2 条，得到 %d", len(got))
	}
	if got[0]["order"] != float64(2) || got[0]["role"] != "mask" {
		t.Fatalf("顺序与角色应原样保留：%#v", got[0])
	}
	if got[1]["generationId"] != "111" {
		t.Fatalf("第二条的 ID 不对：%#v", got[1])
	}
}

// ⚠️ 解不开时回 `[]` 而不是报错：快照是历史数据，格式可能比代码旧。
// 让整条重试因为一段解不开的历史 JSON 而失败，比"按没有输入重试一次"更糟。
func TestRedeclareInputs_坏快照退回空数组(t *testing.T) {
	for _, bad := range []string{"", "不是 JSON", `{"kind":"text"}`, "null"} {
		out := RedeclareInputs(json.RawMessage(bad))
		if string(out) != "[]" {
			t.Fatalf("坏快照 %q 应退回空数组，得到 %s", bad, out)
		}
	}
}

func TestRedeclareInputs_素材与音色照样带回去(t *testing.T) {
	// 风格/特效/音色只贡献提示词，但**丢了就换了一个画面** ——
	// 用户重试出来的图会突然没有他选的那个风格。
	snap := json.RawMessage(`[
		{"kind":"material","order":1,"materialCode":"ink","materialType":"style"},
		{"kind":"voice","order":2,"voiceCode":"v1"}
	]`)
	var got []map[string]any
	if err := json.Unmarshal(RedeclareInputs(snap), &got); err != nil {
		t.Fatal(err)
	}
	if got[0]["materialCode"] != "ink" || got[0]["materialType"] != "style" {
		t.Fatalf("素材应带回去：%#v", got[0])
	}
	if got[1]["voiceCode"] != "v1" {
		t.Fatalf("音色应带回去：%#v", got[1])
	}
}

// 区间（docs/agent/smartcut SCD-34）：重试照样只用那一段、按那一段计价；没有区间的输入还原出来不多一个字段。
func TestRedeclareInputs_带上区间(t *testing.T) {
	snap := json.RawMessage(`[
		{"kind":"reference","order":1,"referenceId":31,"mediaId":31,"mediaKind":"video","rangeMs":[2000,5000]},
		{"kind":"text","order":2,"text":"去掉字幕"}
	]`)
	out := RedeclareInputs(snap)
	var got []map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	r, _ := got[0]["rangeMs"].([]any)
	if len(r) != 2 || r[0] != float64(2000) || r[1] != float64(5000) {
		t.Fatalf("区间要原样带回去：%#v", got[0])
	}
	if _, has := got[1]["rangeMs"]; has {
		t.Fatalf("没有区间的输入不该多出 rangeMs：%s", out)
	}
}
