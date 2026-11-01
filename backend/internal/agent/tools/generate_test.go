package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// ---------------------------------------------------------------- link_generation 只能由 generate 产生

func TestCanvasApply_不允许link_generation(t *testing.T) {
	if canvasOpKinds["link_generation"] {
		t.Fatal("canvas_apply 的允许清单里不得有 link_generation：result 不该由模型伪造")
	}
	_, err := ParseCanvasApplyArgs(json.RawMessage(
		`{"ops":[{"kind":"link_generation","nodeId":"a_1","patch":{"generationId":"9"}}]}`))
	if err == nil || !strings.Contains(err.Error(), "link_generation") {
		t.Fatalf("应拒绝 link_generation：%v", err)
	}
	if strings.Contains(string(CanvasApply{}.Schema()), "link_generation") {
		t.Error("canvas_apply 的 schema 不应出现 link_generation")
	}
}

// ---------------------------------------------------------------- generate 受理后挂到节点

type genCanvas struct {
	data     json.RawMessage
	applied  []app.ApplyOpsInput
	failLink bool
}

func (c *genCanvas) Read(context.Context, int64, int64) (app.CanvasView, error) {
	nodes, edges := app.SummarizeCanvas(c.data)
	return app.CanvasView{Version: 1, CanvasData: c.data, Nodes: nodes, EdgeCount: edges}, nil
}

func (c *genCanvas) Apply(_ context.Context, in app.ApplyOpsInput) (app.ApplyOpsResult, error) {
	c.applied = append(c.applied, in)
	if c.failLink && strings.Contains(string(in.Ops), "link_generation") {
		return app.ApplyOpsResult{}, errors.New("画布写入失败")
	}
	return app.ApplyOpsResult{NodeIDs: []string{"a_new"}}, nil
}

type genQuoter struct{ got app.QuoteInput }

func (q *genQuoter) QuoteGeneration(_ context.Context, in app.QuoteInput) (app.Quote, error) {
	q.got = in
	return app.Quote{Credits: 30}, nil
}

func (q *genQuoter) QuoteTurn(context.Context, string, int64) (app.Quote, error) {
	return app.Quote{}, nil
}

type genGenerator struct{ got app.GenerateInput }

func (g *genGenerator) Accept(_ context.Context, in app.GenerateInput) (app.GenerateResult, error) {
	g.got = in
	return app.GenerateResult{GenerationID: 9007199254740993, Credits: 30, Status: "queued"}, nil
}

func (g *genGenerator) Status(context.Context, int64, int64) (app.TaskStatus, error) {
	return app.TaskStatus{}, nil
}

func (g *genGenerator) Cancel(context.Context, int64, int64, string) error { return nil }

type genBudget struct{}

func (genBudget) PaidCallTool(_ context.Context, _ app.RunContext, _ string, _ int,
	run func(string) (app.PaidResult, error)) (app.PaidResult, *app.Suspension, error) {
	r, err := run("idem")
	return r, nil, err
}

type genParams map[string][]string

func (p genParams) NodeParamCodes(_ context.Context, nodeType string) ([]string, error) {
	return p[nodeType], nil
}

func newGenerate(c *genCanvas) (Generate, *genQuoter, *genGenerator) {
	q, g := &genQuoter{}, &genGenerator{}
	return Generate{Canvas: c, Quoter: q, Generator: g, Budget: genBudget{},
		Params: genParams{"video": {"video_mode", "prompt", "model", "duration", "resolution", "aspect_ratio"}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, q, g
}

var genRC = app.RunContext{WorkspaceID: 1, ProjectID: 2, SessionID: 3, StepID: 4, Attempt: 1, UserID: 5,
	State: domain.StateExecuting}

func opsOf(t *testing.T, in app.ApplyOpsInput) []map[string]any {
	t.Helper()
	var ops []map[string]any
	if err := json.Unmarshal(in.Ops, &ops); err != nil {
		t.Fatal(err)
	}
	return ops
}

func TestGenerate_受理后把任务挂到节点且参数用画布编码(t *testing.T) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[]}`)}
	tool, q, g := newGenerate(c)
	res, err := tool.Run(context.Background(), genRC, json.RawMessage(
		`{"nodeType":"video","modelCode":"sd","operationCode":"text2video","prompt":"海边",`+
			`"params":{"aspectRatio":"9:16","resolution":"720p"},"durationSec":15,"wait":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.applied) != 2 {
		t.Fatalf("应写两批画布（建节点 + 挂任务），实际 %d", len(c.applied))
	}
	// ① 建节点：参数是画布编码。
	node, _ := opsOf(t, c.applied[0])[0]["node"].(map[string]any)
	params, _ := node["params"].(map[string]any)
	want := map[string]any{"model": "sd", "prompt": "海边", "mode": "text2video",
		"aspect_ratio": "9:16", "resolution": "720p", "duration": "15"}
	if !reflect.DeepEqual(params, want) {
		t.Errorf("节点参数应为画布编码：%v", params)
	}
	// 受理与报价用同一份参数与时长。
	var acc map[string]any
	_ = json.Unmarshal(g.got.Params, &acc)
	if !reflect.DeepEqual(acc, want) || g.got.DurationSec != 15 || q.got.DurationSec != 15 {
		t.Errorf("受理参数不一致：%v dur=%d quote.dur=%d", acc, g.got.DurationSec, q.got.DurationSec)
	}
	// ② 挂任务：独立批次、只有 link_generation、ID 是字符串。
	link := c.applied[1]
	if link.BatchID == c.applied[0].BatchID || link.BatchID != "agent:3:4:1:link" {
		t.Errorf("挂任务那一批必须用独立的批次 ID（否则命中幂等什么都没写）：%s", link.BatchID)
	}
	ops := opsOf(t, link)
	if len(ops) != 1 || ops[0]["kind"] != "link_generation" || ops[0]["nodeId"] != "a_new" {
		t.Fatalf("应发一条 link_generation：%v", ops)
	}
	if p, _ := ops[0]["patch"].(map[string]any); p["generationId"] != "9007199254740993" {
		t.Errorf("generationId 应为字符串且不丢精度：%v", ops[0]["patch"])
	}
	if !strings.Contains(string(res.Content), `"generationId":"9007199254740993"`) {
		t.Errorf("结果应带 generationId：%s", res.Content)
	}
}

func TestGenerate_挂任务失败只告警不影响本轮(t *testing.T) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[]}`), failLink: true}
	tool, _, _ := newGenerate(c)
	res, err := tool.Run(context.Background(), genRC, json.RawMessage(
		`{"nodeType":"video","modelCode":"sd","operationCode":"text2video","wait":true}`))
	if err != nil {
		t.Fatalf("任务已受理已扣费，挂节点失败不能让工具报错（模型会再生成一次）：%v", err)
	}
	if res.GenerationID != 9007199254740993 || res.Suspend == nil {
		t.Errorf("结果照常：%+v", res)
	}
}

func TestGenerate_改已有节点按节点类型归一并删掉别名键(t *testing.T) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[{"id":"a_v","type":"video","title":"v","params":{},` +
		`"agent":{"sessionId":"3"}}]}`)}
	tool, _, _ := newGenerate(c)
	if _, err := tool.Run(context.Background(), genRC, json.RawMessage(
		`{"nodeId":"a_v","modelCode":"sd","operationCode":"img2video","params":{"aspectRatio":"9:16"},`+
			`"durationSec":10,"wait":false}`)); err != nil {
		t.Fatal(err)
	}
	up := opsOf(t, c.applied[0])[0]
	if up["kind"] != "update_node" {
		t.Fatalf("应更新已有节点：%v", up)
	}
	params, _ := up["patch"].(map[string]any)["params"].(map[string]any)
	if params["aspect_ratio"] != "9:16" || params["duration"] != "10" || params["mode"] != "img2video" {
		t.Errorf("已有视频节点也要归一（含 mode）：%v", params)
	}
	if v, ok := params["aspectRatio"]; !ok || v != nil {
		t.Errorf("别名键应以 null 删除（merge patch）：%v", params)
	}
	if ops := opsOf(t, c.applied[1]); ops[0]["nodeId"] != "a_v" {
		t.Errorf("挂到已有节点上：%v", ops)
	}
}
