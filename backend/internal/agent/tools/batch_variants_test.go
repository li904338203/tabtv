package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// ---------------------------------------------------------------- MKT-T-27：批量变体

func variantsJSON(n int) string {
	parts := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		parts = append(parts, fmt.Sprintf(`{"type":"video","modelCode":"seedance","operationCode":"ref2video",`+
			`"durationSec":5,"key":"v%d","variant":%d,"variantLabel":"变体 %d"}`, i, i, i))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestSubmitPlan_营销_超变体上限不出卡(t *testing.T) {
	e, _ := newMktEnv(t, 0)
	e.deps.MaxVariants = 4
	_, err := e.submitPlan(t, variantsJSON(5))
	var rej *domain.ToolRejection
	if !errors.As(err, &rej) || rej.Code != domain.ToolErrTooManyVariants {
		t.Fatalf("5 个变体应回 too_many_variants：%v", err)
	}
	if !strings.Contains(rej.Message, "单次最多 4 个变体") || !strings.Contains(rej.Message, "没有发卡") {
		t.Fatalf("工具结果应说明「单次最多 4 个变体」：%s", rej.Message)
	}
	var m map[string]any
	_ = json.Unmarshal(rej.Content(), &m)
	if m["error"] != domain.ToolErrTooManyVariants || m["max"].(float64) != 4 || m["variants"].(float64) != 5 {
		t.Fatalf("结构化结果带上限与变体数：%v", m)
	}
	if len(e.store.cards) != 0 || len(e.pricer.calls) != 0 || len(e.scenePins(t)) != 0 {
		t.Fatalf("超上限不出卡、不报价、不钉版本：cards=%d calls=%d", len(e.store.cards), len(e.pricer.calls))
	}

	// 4 个照常出卡：逐项报价、合计 = 各行之和（MKT-AC-17）。
	if _, err := e.submitPlan(t, variantsJSON(4)); err != nil {
		t.Fatal(err)
	}
	p := e.lastCard(t)
	sum := 0
	for _, it := range p.Items {
		sum += it.Credits
	}
	if len(p.Items) != 4 || p.Total != sum || sum == 0 {
		t.Fatalf("4 行报价、合计等于各行之和：%d 行 total=%d sum=%d", len(p.Items), p.Total, sum)
	}
}

func TestSubmitPlan_营销_变体上限取配置(t *testing.T) {
	e, _ := newMktEnv(t, 0)
	e.deps.MaxVariants = 2
	_, err := e.submitPlan(t, variantsJSON(3))
	if err == nil || !strings.Contains(err.Error(), "单次最多 2 个变体") {
		t.Fatalf("数字取配置：%v", err)
	}
	e.deps.MaxVariants = 0 // 未配置按缺省 4
	if _, err := e.submitPlan(t, variantsJSON(4)); err != nil {
		t.Fatalf("缺省上限 4：%v", err)
	}
	// 通用会话不看变体字段。
	g := newPtEnv()
	g.deps.MaxVariants = 1
	if _, err := g.tool(domain.ToolSubmitPlan).Run(context.Background(), ptRC(domain.StatePlanning),
		json.RawMessage(`{"items":`+variantsJSON(3)+`}`)); err != nil {
		t.Fatalf("通用会话不受变体上限影响：%v", err)
	}
}

func TestStepErrInsufficientCredits_与错误码名一致(t *testing.T) {
	if domain.StepErrInsufficientCredits != errcode.InsufficientCredits.Name() {
		t.Fatalf("停后续按码判：%s ≠ %s", domain.StepErrInsufficientCredits, errcode.InsufficientCredits.Name())
	}
	if stepErrorCode(&app.InsufficientCreditsError{}) != domain.StepErrInsufficientCredits {
		t.Fatal("余额不足写进 error_code 的值必须与 ShouldStop 认的一致")
	}
}

// batchPlan 是三个变体的已批准计划（v1 已成功、v2 余额不足）。
func batchPlan(steps ...domain.PlanStep) *fakePlans {
	p := approvedPlan()
	v1 := p.plan.Items[0]
	items := []domain.PlanItem{}
	for i := 1; i <= 3; i++ {
		it := v1
		it.Key, it.Variant = fmt.Sprintf("v%d", i), i
		it.VariantLabel = fmt.Sprintf("Hook：反问开场 · 风格 %d", i)
		items = append(items, it)
	}
	p.plan.Items, p.plan.Steps, p.plan.CardID, p.plan.Title = items, steps, 77, "保温杯批量"
	return p
}

func TestGenerate_营销_余额不足停后续(t *testing.T) {
	steps := []domain.PlanStep{{Key: "v1", Status: "waiting"},
		{Key: "v2", Status: "failed", ErrorCode: domain.StepErrInsufficientCredits}}
	tool, c, _, g := newMarketingGenerate(batchPlan(steps...))
	_, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v3"}`))
	rej := rejection(t, err, c, g, "v3", domain.ToolErrPlanStopped)
	if !strings.Contains(rej.Message, "余额不足，后续变体已停止") || rej.Details["stoppedBy"] != "v2" {
		t.Fatalf("说清是谁停的：%+v", rej)
	}

	// 文案相同而码不同：不停（按码不按文案）。
	other := []domain.PlanStep{{Key: "v2", Status: "failed", ErrorCode: "PROVIDER_FAILED"}}
	tool, _, _, g = newMarketingGenerate(batchPlan(other...))
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v3"}`)); err != nil ||
		g.got.ModelCode == "" {
		t.Fatalf("非余额失败不停后续：%v", err)
	}

	// 被余额挡住的那一项自己可以重试（充值之后）。
	tool, _, _, g = newMarketingGenerate(batchPlan(steps...))
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v2"}`)); err != nil ||
		g.got.ModelCode == "" {
		t.Fatalf("v2 自己的重试应受理：%v", err)
	}
}

func TestGenerate_营销_变体节点标题与画框(t *testing.T) {
	tool, c, _, _ := newMarketingGenerate(batchPlan())
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v2"}`)); err != nil {
		t.Fatal(err)
	}
	// 第一批是建画框（带 planCardId），之后是建节点（标题 = 变体说明，位置在框里第 2 列）。
	if len(c.applied) < 2 || !strings.Contains(string(c.applied[0].Ops), `"add_frame"`) ||
		!strings.Contains(string(c.applied[0].Ops), `"planCardId":"77"`) {
		t.Fatalf("应先建计划画框：%d 批 %s", len(c.applied), c.applied[0].Ops)
	}
	var ops []map[string]any
	_ = json.Unmarshal(c.applied[1].Ops, &ops)
	node, _ := ops[0]["node"].(map[string]any)
	if node["title"] != "Hook：反问开场 · 风格 2" {
		t.Fatalf("节点标题 = 变体说明：%v", ops[0])
	}
	pos, _ := ops[0]["position"].(map[string]any)
	if pos == nil || pos["x"].(float64) != float64(variantPadX+variantCellW) || pos["y"].(float64) != variantPadTop {
		t.Fatalf("v2 落在画框第 2 列：%v", ops[0])
	}
}

func TestVariantCell_按变体序排列(t *testing.T) {
	items := []domain.PlanItem{{Key: "b", Variant: 2}, {Key: "a", Variant: 1}, {Key: "a2", Variant: 1},
		{Key: "c", Variant: 3}, {Key: "x"}}
	cases := map[string][2]int{"a": {0, 0}, "a2": {0, 1}, "b": {1, 0}, "c": {2, 0}, "x": {3, 0}}
	for k, want := range cases {
		col, row, cols, rows, ok := variantCell(items, k)
		if !ok || col != want[0] || row != want[1] || cols != 4 || rows != 2 {
			t.Errorf("%s：col=%d row=%d cols=%d rows=%d ok=%v", k, col, row, cols, rows, ok)
		}
	}
	if _, _, _, _, ok := variantCell([]domain.PlanItem{{Key: "v1", Variant: 1}}, "v1"); ok {
		t.Error("单个变体不排画框")
	}
	if _, _, _, _, ok := variantCell(items, "nope"); ok {
		t.Error("不在计划里的键不排")
	}
}

func TestCellPosition_被占往下顺延(t *testing.T) {
	x0, y0 := float64(1000+variantPadX), float64(500+variantPadTop)
	nodes := canvasNodes(json.RawMessage(fmt.Sprintf(`{"nodes":[`+
		`{"id":"f","type":"frame","position":{"x":%v,"y":%v}},`+
		`{"id":"old","type":"video","position":{"x":%v,"y":%v}}]}`, x0, y0, x0, y0)))
	x, y := cellPosition(nodes, 1000, 500, 0, 0)
	if x != x0 || y != y0+variantCellH {
		t.Fatalf("格子被重试前的旧节点占着，应往下一格：%v,%v", x, y)
	}
	if fx, fy := newFramePosition(nodes[1:]); fx != x0+330+variantFrameGap || fy != y0 {
		t.Fatalf("新画框放在现有内容右侧：%v,%v", fx, fy)
	}
	if fx, fy := newFramePosition(nil); fx != 0 || fy != 0 {
		t.Fatal("空画布从原点开始")
	}
}
