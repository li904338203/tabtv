package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
)

func TestGenerateArgs_缺模型或操作码拒绝(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		{"缺模型", `{"operationCode":"text2img","nodeType":"image"}`, "modelCode"},
		{"模型空白", `{"modelCode":"  ","operationCode":"text2img","nodeType":"image"}`, "modelCode"},
		{"缺操作码", `{"modelCode":"m","nodeType":"image"}`, "operationCode"},
		{"新建缺节点类型", `{"modelCode":"m","operationCode":"text2img"}`, "nodeType"},
		{"节点类型非法", `{"modelCode":"m","operationCode":"text2img","nodeType":"frame"}`, "nodeType"},
		{"params 不是对象", `{"modelCode":"m","operationCode":"text2img","nodeType":"image","params":[1]}`, "params"},
		{"inputs 不是数组", `{"modelCode":"m","operationCode":"text2img","nodeType":"image","inputs":{}}`, "inputs"},
		{"负数张数", `{"modelCode":"m","operationCode":"text2img","nodeType":"image","count":-1}`, "负"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ParseGenerateArgs(json.RawMessage(c.args))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("应拒绝并提到 %q，得到 %v", c.want, err)
			}
		})
	}
}

func TestGenerateArgs_节点参数与前端同形(t *testing.T) {
	a, p, err := ParseGenerateArgs(json.RawMessage(
		`{"modelCode":"kling","operationCode":"img2video","nodeType":"video","prompt":" 猫跳 ",` +
			`"params":{"ratio":"16:9","seed":9007199254740993}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p["model"] != "kling" || p["prompt"] != "猫跳" || p["mode"] != "img2video" || p["ratio"] != "16:9" {
		t.Fatalf("节点参数不对：%v", p)
	}
	// 雪花号不能被解成 float64 丢精度。
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), "9007199254740993") {
		t.Fatalf("大整数丢精度：%s", b)
	}
	if a.Wait != nil {
		t.Error("没传 wait 时应为 nil（默认等待）")
	}
	_, p, _ = ParseGenerateArgs(json.RawMessage(`{"modelCode":"tts","operationCode":"tts","nodeType":"audio","prompt":"你好"}`))
	if p["text"] != "你好" || p["prompt"] != nil {
		t.Errorf("音频节点的正文在 text：%v", p)
	}
	// 给了 nodeId 时不要求 nodeType。
	if _, _, err := ParseGenerateArgs(json.RawMessage(`{"nodeId":"a_x","modelCode":"m","operationCode":"text2img"}`)); err != nil {
		t.Errorf("给 nodeId 时不该要求 nodeType：%v", err)
	}
}

type outputsTable map[string]string

func (o outputsTable) OperationOutNodeType(_ context.Context, op string) (string, error) {
	return o[op], nil
}

// 会话 105692702233853952 第 15 条：只给 text2video、没给 nodeType 时按目录 out_node_type 推导，不报错。
func TestGenerateArgs_新建节点类型按目录推导(t *testing.T) {
	ctx := context.Background()
	out := outputsTable{"text2video": "video", "text2audio": "audio", "storyboard_gen": "script"}
	a, p, err := parseGenerateArgs(ctx, json.RawMessage(
		`{"modelCode":"grok","operationCode":"text2video","prompt":"雨夜"}`), out)
	if err != nil || a.NodeType != "video" || p["mode"] != "text2video" {
		t.Fatalf("应推导成 video 且节点参数按 video 组（mode）：%+v %v %v", a, p, err)
	}
	_, p, err = parseGenerateArgs(ctx, json.RawMessage(`{"modelCode":"tts","operationCode":"text2audio","prompt":"你好"}`), out)
	if err != nil || p["text"] != "你好" {
		t.Fatalf("推导成 audio 时正文在 text：%v %v", p, err)
	}
	for _, c := range []struct{ args, want string }{
		{`{"modelCode":"m","operationCode":"text2video","nodeType":"image"}`, "不一致"},
		{`{"modelCode":"m","operationCode":"storyboard_gen"}`, "不能直接建节点"},
	} {
		if _, _, err := parseGenerateArgs(ctx, json.RawMessage(c.args), out); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s：应报错并提到 %q，得到 %v", c.args, c.want, err)
		}
	}
	// 给了 nodeId 时不推导（以画布上的类型为准）。
	if _, _, err := parseGenerateArgs(ctx, json.RawMessage(`{"nodeId":"a_x","modelCode":"m","operationCode":"storyboard_gen"}`), out); err != nil {
		t.Fatalf("给 nodeId 时不该校验新建类型：%v", err)
	}
}

func TestCanvasApplyArgs_未知op拒绝(t *testing.T) {
	bad := []struct{ args, want string }{
		{`{"ops":[{"kind":"delete_node","nodeId":"n_1"}]}`, "delete_node"},
		{`{"ops":[{"kind":"add_node","node":{"type":"text"}},{"nodeId":"x"}]}`, "第 2 个"},
		{`{"ops":[]}`, "不能为空"},
		{`{}`, "不能为空"},
		{`{"ops":[1]}`, "不是对象"},
	}
	for _, c := range bad {
		if _, err := ParseCanvasApplyArgs(json.RawMessage(c.args)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s 应拒绝并提到 %q，得到 %v", c.args, c.want, err)
		}
	}
	ops, err := ParseCanvasApplyArgs(json.RawMessage(`{"ops":[{"kind":"add_node","node":{"type":"text","x-未来":1}}]}`))
	if err != nil || len(ops) != 1 {
		t.Fatalf("合法操作被拒：%v", err)
	}
	stampAgentStep(ops[0], "node", 42)
	b, _ := json.Marshal(ops)
	if !strings.Contains(string(b), `"stepId":"42"`) || !strings.Contains(string(b), "x-未来") {
		t.Errorf("应写 agent.stepId 且保留未知字段：%s", b)
	}
}

func TestBatchID_随attempt变(t *testing.T) {
	a := BatchID(app.RunContext{SessionID: 1, StepID: 2, Attempt: 1})
	b := BatchID(app.RunContext{SessionID: 1, StepID: 2, Attempt: 2})
	if a == b || a != "agent:1:2:1" {
		t.Fatalf("批次 ID 应随 attempt 变：%s %s", a, b)
	}
	if BatchID(app.RunContext{SessionID: 1, StepID: 2}) != a {
		t.Error("attempt 缺省按 1")
	}
}

func TestCancelTaskArgs_字符串与数字都收(t *testing.T) {
	for _, s := range []string{`{"generationId":"123"}`, `{"generationId":123}`} {
		id, _, err := ParseCancelTaskArgs(json.RawMessage(s))
		if err != nil || id != 123 {
			t.Errorf("%s → %d %v", s, id, err)
		}
	}
	for _, s := range []string{`{}`, `{"generationId":"abc"}`, `{"generationId":"-1"}`} {
		if _, _, err := ParseCancelTaskArgs(json.RawMessage(s)); err == nil {
			t.Errorf("%s 应拒绝", s)
		}
	}
}
