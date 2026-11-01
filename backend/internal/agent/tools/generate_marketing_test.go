package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- MKT-T-22：generate 营销分支

type fakePlans struct {
	plan  app.SessionPlan
	err   error
	calls int
	chars map[string]int64
}

func (f *fakePlans) SessionPlan(context.Context, int64) (app.SessionPlan, error) {
	f.calls++
	return f.plan, f.err
}

func (f *fakePlans) CharacterMedia(_ context.Context, _ int64, ref app.SceneRef) (int64, error) {
	if id, ok := f.chars[ref.Kind+":"+ref.ID]; ok {
		return id, nil
	}
	return 0, errors.New("角色资产还没有图片")
}

func five() *int { n := 5; return &n }

// approvedPlan：会话 3 张商品图、一个资产角色；v1 用 #3、#1（换过图），v2 是配音条目。
func approvedPlan() *fakePlans {
	return &fakePlans{
		plan: app.SessionPlan{
			Scene: domain.SceneMarketing, Approved: true,
			Input: app.SceneInput{Products: []string{"101", "102", "103"},
				Characters: []app.SceneRef{{Kind: app.RefAsset, ID: "201"}}},
			Items: []domain.PlanItem{
				{Kind: "video", Key: "v1", Label: "开箱 · 变体 1", ModelCode: "sd", OperationCode: "img2video",
					Quantity: 10, Unit: "second", Credits: 300, ProductIndexes: []int{3, 1},
					ProductMediaIDs: []string{"103", "101"}, ProductTotal: 3, ImageLimit: five(),
					Inputs: map[string]int{"image": 3},
					Picks: []domain.PlanPick{{Kind: "creative", Code: "unbox", Version: 2, Name: "开箱"},
						{Kind: "hook", Code: "q3", Version: 1, Name: "三问"}}},
				{Kind: "audio", Key: "a1", Label: "旁白", ModelCode: "tts", OperationCode: "tts", Quantity: 1, Unit: "time"},
			},
		},
		chars: map[string]int64{"asset:201": 901},
	}
}

var mkRC = app.RunContext{WorkspaceID: 1, ProjectID: 2, SessionID: 3, StepID: 4, Attempt: 1, UserID: 5,
	State: domain.StateExecuting, Scene: domain.SceneMarketing}

func newMarketingGenerate(p *fakePlans) (Generate, *genCanvas, *genQuoter, *genGenerator) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[]}`)}
	tool, q, g := newGenerate(c)
	tool.Plans = p
	return tool, c, q, g
}

// rejection 断言错误是带计划项键的结构化拒绝，且没有任何副作用（不建节点、不报价、不受理）。
func rejection(t *testing.T, err error, c *genCanvas, g *genGenerator, key, code string) *domain.ToolRejection {
	t.Helper()
	var se *app.StepError
	var rej *domain.ToolRejection
	if !errors.As(err, &se) || !errors.As(err, &rej) {
		t.Fatalf("应是带步骤标记的结构化拒绝：%T %v", err, err)
	}
	if rej.Code != code || se.PlanItemKey != key {
		t.Fatalf("码 / 键不对：code=%s key=%q（期望 %s / %q）", rej.Code, se.PlanItemKey, code, key)
	}
	if code2, _ := stepErrorCodeOf(err); code2 != code {
		t.Fatalf("步骤错误码应为 %s，得到 %s", code, code2)
	}
	if len(c.applied) != 0 || g.got.ModelCode != "" {
		t.Fatalf("拒绝不应有副作用：画布 %d 批、受理 %+v", len(c.applied), g.got)
	}
	return rej
}

// stepErrorCodeOf 取 StepError 上落库的码（没给时取拒绝的码，与 Runner 同一规则）。
func stepErrorCodeOf(err error) (string, string) {
	var se *app.StepError
	if errors.As(err, &se) && se.ErrorCode != "" {
		return se.ErrorCode, se.PlanItemKey
	}
	var rej *domain.ToolRejection
	if errors.As(err, &rej) {
		return rej.Code, ""
	}
	return "", ""
}

// 没带 planItemKey（高频，R-16）：唯一对得上就照条目执行并告诉模型；对不上 / 不唯一才退回，并列出可选条目。
func TestGenerate_营销_没带planItemKey(t *testing.T) {
	// 会话 107492880150429696 的写法：给了类型 / 模型 / 操作，唯一对得上 v1。
	tool, _, _, g := newMarketingGenerate(approvedPlan())
	res, err := tool.Run(context.Background(), mkRC, json.RawMessage(
		`{"nodeType":"video","modelCode":"sd","operationCode":"img2video","prompt":"开箱","wait":false}`))
	if err != nil {
		t.Fatalf("唯一对得上应照条目执行：%v", err)
	}
	if res.PlanItemKey != "v1" || g.got.ModelCode != "sd" || g.got.DurationSec != 10 {
		t.Fatalf("应按 v1 执行（步骤记上条目键）：key=%q %+v", res.PlanItemKey, g.got)
	}
	var content map[string]any
	_ = json.Unmarshal(res.Content, &content)
	if notes, _ := content["notes"].([]any); len(notes) != 1 || !strings.Contains(notes[0].(string), "v1") {
		t.Fatalf("要告诉模型按哪一项执行了：%s", res.Content)
	}

	// 什么都没给：视频与配音两项都是候选 → 退回，列出可选条目。
	tool, c, _, g := newMarketingGenerate(approvedPlan())
	_, err = tool.Run(context.Background(), mkRC, json.RawMessage(`{"prompt":"开箱"}`))
	rej := rejection(t, err, c, g, "", domain.ToolErrPlanItemNotApproved)
	if keys, _ := rej.Details["approvedKeys"].([]string); !reflect.DeepEqual(keys, []string{"v1", "a1"}) {
		t.Errorf("应告诉模型能用哪些键：%v", rej.Details)
	}
	for _, want := range []string{"planItemKey", "没有扣费", "v1「开箱 · 变体 1」（sd · img2video · 10 秒）", "a1「旁白」"} {
		if !strings.Contains(rej.Message, want) {
			t.Errorf("文案应列出可选条目（缺 %q）：%s", want, rej.Message)
		}
	}
	// ⚠️ 它只是漏了一个参数：叫它「调整规划」的话，用户会再收到一张一模一样的确认卡。
	if strings.Contains(rej.Message, "调整规划") {
		t.Errorf("漏了键不该叫它重新规划：%s", rej.Message)
	}

	// 已在跑的条目不算候选：一次漏了键的调用不能把它再生成一遍。
	p := approvedPlan()
	p.plan.Steps = []domain.PlanStep{{Key: "v1", Status: "waiting"}}
	tool, c, _, g = newMarketingGenerate(p)
	_, err = tool.Run(context.Background(), mkRC, json.RawMessage(
		`{"nodeType":"video","modelCode":"sd","operationCode":"img2video"}`))
	rej = rejection(t, err, c, g, "", domain.ToolErrPlanItemNotApproved)
	if !strings.Contains(rej.Message, "a1「旁白」") || strings.Contains(rej.Message, "v1「") {
		t.Errorf("可选条目里不该有已在跑的 v1：%s", rej.Message)
	}
}

func TestGenerate_营销_未批准条目拒(t *testing.T) {
	// 键不在已批准的计划里。
	tool, c, _, g := newMarketingGenerate(approvedPlan())
	_, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v9","modelCode":"sd","operationCode":"img2video"}`))
	rejection(t, err, c, g, "v9", domain.ToolErrPlanItemNotApproved)

	// 还没有任何已批准的规划卡（卡片待确认 / 被要求调整）。
	p := approvedPlan()
	p.plan.Approved, p.plan.Items = false, nil
	tool, c, _, g = newMarketingGenerate(p)
	_, err = tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v1"}`))
	rej := rejection(t, err, c, g, "v1", domain.ToolErrPlanItemNotApproved)
	if !strings.Contains(rej.Message, "请先调整规划") {
		t.Errorf("文案：%s", rej.Message)
	}

	// 营销会话而计划端口未接入：拒绝，不退回通用路径。
	tool, c, _, g = newMarketingGenerate(approvedPlan())
	tool.Plans = nil
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v1"}`)); err == nil ||
		len(c.applied) != 0 || g.got.ModelCode != "" {
		t.Fatalf("未接入时不能放行：%v", err)
	}
}

func TestGenerate_营销_冲突拒绝(t *testing.T) {
	cases := []struct {
		name, args string
		fields     []string
	}{
		{"模型", `{"planItemKey":"v1","modelCode":"kling"}`, []string{"modelCode"}},
		{"操作", `{"planItemKey":"v1","modelCode":"sd","operationCode":"text2video"}`, []string{"operationCode"}},
		{"时长", `{"planItemKey":"v1","durationSec":5}`, []string{"durationSec"}},
		// 模型自己报的图：少一张、换了一张、顺序反了都算不一致（确认卡上的图就是发给上游的图）。
		{"少图", `{"planItemKey":"v1","inputs":[{"kind":"reference","referenceId":"103","order":1}]}`, []string{"inputs"}},
		{"换图", `{"planItemKey":"v1","inputs":[{"kind":"reference","referenceId":"102","order":1},` +
			`{"kind":"reference","referenceId":"101","order":2},{"kind":"reference","referenceId":"901","order":3}]}`, []string{"inputs"}},
		{"顺序", `{"planItemKey":"v1","inputs":[{"kind":"reference","referenceId":"101","order":1},` +
			`{"kind":"reference","referenceId":"103","order":2},{"kind":"reference","referenceId":"901","order":3}]}`, []string{"inputs"}},
		{"一次报完", `{"planItemKey":"v1","modelCode":"kling","operationCode":"text2video","inputs":[]}`,
			[]string{"modelCode", "operationCode", "inputs"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, c, _, g := newMarketingGenerate(approvedPlan())
			_, err := tool.Run(context.Background(), mkRC, json.RawMessage(tc.args))
			rej := rejection(t, err, c, g, "v1", domain.ToolErrPlanItemMismatch)
			conflicts, _ := rej.Details["conflicts"].([]map[string]any)
			var got []string
			for _, x := range conflicts {
				got = append(got, x["field"].(string))
			}
			if !reflect.DeepEqual(got, tc.fields) {
				t.Fatalf("冲突字段 %v，期望 %v", got, tc.fields)
			}
		})
	}

	// 与条目一致的输入（ID 写成数字也认）放行。
	tool, _, _, g := newMarketingGenerate(approvedPlan())
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v1","modelCode":"sd",`+
		`"inputs":[{"kind":"reference","referenceId":103,"order":1},{"kind":"reference","referenceId":"101","order":2},`+
		`{"kind":"reference","referenceId":"901","order":3}],"wait":false}`)); err != nil {
		t.Fatalf("与条目一致不该拒：%v", err)
	}
	if g.got.ModelCode != "sd" {
		t.Fatalf("应已受理：%+v", g.got)
	}
}

// 会话 107440483873062912：模型只看得到商品图序号，照着写了 referenceId:"1" —— 按会话商品图顺序换算后再比。
func TestGenerate_营销_商品图按序号写也认(t *testing.T) {
	ordinal := func(ids ...string) string {
		var b strings.Builder
		for i, id := range ids {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"kind":"reference","referenceId":"` + id + `","order":` + strconv.Itoa(i+1) + `}`)
		}
		return `{"planItemKey":"v1","inputs":[` + b.String() + `],"wait":false}`
	}
	// v1 用 #3、#1 + 角色（mediaId 901）：各种序号写法都认（会话 107492880150429696 写的是 "product_1"），
	// 角色写明「角色」/ character 的也按会话角色换算。
	for _, args := range []string{ordinal("3", "1", "901"), ordinal("#3", "#1", "901"),
		ordinal("product_3", "product_1", "character_1"), ordinal("商品图3", "商品图#1", "角色1"),
		ordinal("image-3", "第1张", "role 1"), ordinal("Product3", "IMG_1", "901")} {
		tool, _, _, g := newMarketingGenerate(approvedPlan())
		if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(args)); err != nil {
			t.Fatalf("序号换算后与条目一致，不该拒：%v（%s）", err, args)
		}
		if g.got.ModelCode != "sd" {
			t.Fatalf("应已受理：%+v", g.got)
		}
	}
	// 换算后仍不一致（顺序反了 / 换了图 / 序号越界 / 角色序号越界）照常退回：那是真想换图。
	for _, args := range []string{ordinal("1", "3", "901"), ordinal("2", "1", "901"), ordinal("4", "1", "901"),
		ordinal("3", "1", "角色2"),
		// 认不出的占位符混着真实引用：说不清它要什么，退回（不静默以哪一边为准）。
		ordinal("main_image", "103")} {
		tool, c, _, g := newMarketingGenerate(approvedPlan())
		_, err := tool.Run(context.Background(), mkRC, json.RawMessage(args))
		rejection(t, err, c, g, "v1", domain.ToolErrPlanItemMismatch)
	}

	// 全是认不出的占位符（模型看不到真实 ID，只能编）：没表达换图的意图 → 按条目发图，并告诉模型。
	tool, _, _, g := newMarketingGenerate(approvedPlan())
	res, err := tool.Run(context.Background(), mkRC, json.RawMessage(ordinal("main_image", "sofa.png")))
	if err != nil {
		t.Fatalf("全是占位符时按条目发图，不该拒：%v", err)
	}
	if got := generatedRefIDs(t, g); !reflect.DeepEqual(got, []string{"103", "101", "901"}) {
		t.Fatalf("发出去的应是条目的图（#3、#1 + 角色）：%v", got)
	}
	var content map[string]any
	_ = json.Unmarshal(res.Content, &content)
	if notes, _ := content["notes"].([]any); len(notes) != 1 || !strings.Contains(notes[0].(string), `"main_image"`) {
		t.Fatalf("要告诉模型忽略了哪些 inputs：%s", res.Content)
	}
}

func TestGenerate_营销_省略用条目(t *testing.T) {
	tool, c, q, g := newMarketingGenerate(approvedPlan())
	res, err := tool.Run(context.Background(), mkRC, json.RawMessage(
		`{"planItemKey":"v1","prompt":"桌面开箱特写","title":"模型起的名","wait":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if g.got.ModelCode != "sd" || g.got.OperationCode != "img2video" || g.got.DurationSec != 10 || q.got.DurationSec != 10 {
		t.Fatalf("模型 / 操作 / 时长取条目：%+v quote.dur=%d", g.got, q.got.DurationSec)
	}
	var inputs []map[string]any
	if err := json.Unmarshal(g.got.Inputs, &inputs); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"kind": "reference", "referenceId": "103", "order": float64(1)},
		{"kind": "reference", "referenceId": "101", "order": float64(2)},
		{"kind": "reference", "referenceId": "901", "order": float64(3)},
	}
	if !reflect.DeepEqual(inputs, want) {
		t.Fatalf("输入 = 条目的商品图（按条目顺序）+ 角色图：%v", inputs)
	}
	node, _ := opsOf(t, c.applied[0])[0]["node"].(map[string]any)
	if node["title"] != "开箱 · 变体 1" || node["type"] != "video" {
		t.Errorf("节点标题取条目 label、类型按操作推导：%v", node)
	}
	if res.PlanItemKey != "v1" {
		t.Errorf("成功结果带计划项键：%+v", res)
	}
}

func TestGenerate_营销_服务端写marketing覆盖(t *testing.T) {
	tool, c, _, g := newMarketingGenerate(approvedPlan())
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v1","wait":false,`+
		`"params":{"aspectRatio":"9:16","marketing":{"picks":[{"kind":"creative","code":"evil","version":9}],"x":1}}}`)); err != nil {
		t.Fatal(err)
	}
	var acc map[string]any
	_ = json.Unmarshal(g.got.Params, &acc)
	want := map[string]any{"picks": []any{
		map[string]any{"kind": "creative", "code": "unbox", "version": float64(2)},
		map[string]any{"kind": "hook", "code": "q3", "version": float64(1)},
	}}
	if !reflect.DeepEqual(acc["marketing"], want) {
		t.Fatalf("params.marketing 应被覆盖成卡片上的 picks（不含名称）：%v", acc["marketing"])
	}
	if acc["aspect_ratio"] != "9:16" {
		t.Errorf("其余参数照常归一：%v", acc)
	}
	node, _ := opsOf(t, c.applied[0])[0]["node"].(map[string]any)
	if p, _ := node["params"].(map[string]any); !reflect.DeepEqual(p["marketing"], want) {
		t.Errorf("节点上同样只有 code + version：%v", p["marketing"])
	}

	// 没选任何选项的视频条目也写空 picks（受理据此追加场景约束段）；配音条目不写、模型给的也删掉。
	p := approvedPlan()
	p.plan.Items[0].Picks = nil
	tool, _, _, g = newMarketingGenerate(p)
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v1","wait":false}`)); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(g.got.Params, &acc)
	if !reflect.DeepEqual(acc["marketing"], map[string]any{"picks": []any{}}) {
		t.Fatalf("空 picks：%v", acc["marketing"])
	}
	tool, _, _, g = newMarketingGenerate(approvedPlan())
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"a1","prompt":"你好",`+
		`"params":{"marketing":{"picks":[]}},"wait":false}`)); err != nil {
		t.Fatal(err)
	}
	acc = nil
	_ = json.Unmarshal(g.got.Params, &acc)
	if _, ok := acc["marketing"]; ok || g.got.OperationCode != "tts" || g.got.Count != 1 {
		t.Fatalf("配音条目不带营销段：%v %+v", acc, g.got)
	}
}

func TestGenerate_营销_受理失败记错误码(t *testing.T) {
	tool, _, _, _ := newMarketingGenerate(approvedPlan())
	tool.Generator = failingGenerator{err: &app.InsufficientCreditsError{Balance: 20,
		Cause: httpx.Err(errcode.InsufficientCredits)}}
	_, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v1"}`))
	code, key := stepErrorCodeOf(err)
	if code != "INSUFFICIENT_CREDITS" || key != "v1" || !strings.Contains(err.Error(), "还差 10 积分") {
		t.Fatalf("余额不足按码记（文案不变）：%s %s %v", code, key, err)
	}

	tool.Generator = failingGenerator{err: httpx.Err(errcode.CapabilityUnsupported)}
	_, err = tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v1"}`))
	if code, _ := stepErrorCodeOf(err); code != "CAPABILITY_UNSUPPORTED" {
		t.Fatalf("受理业务错误记错误码名：%s %v", code, err)
	}

	// 资产角色还没有图：不悄悄少发一张参考图，也不扣费。
	p := approvedPlan()
	p.chars = nil
	tool, c, _, g := newMarketingGenerate(p)
	_, err = tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v1"}`))
	if err == nil || len(c.applied) != 0 || g.got.ModelCode != "" {
		t.Fatalf("角色取不到图应失败且无副作用：%v", err)
	}
}

// overBudget 模拟额度外壳判定超额：发追加额度卡、挂起，什么都没受理。
type overBudget struct{}

func (overBudget) PaidCallTool(context.Context, app.RunContext, string, int,
	func(string) (app.PaidResult, error)) (app.PaidResult, *app.Suspension, error) {
	return app.PaidResult{}, &app.Suspension{Event: domain.EvOverBudget, CardID: 77}, nil
}

// 04 文档 P-04：超额时这一步什么都没提交，却会被记成成功步骤。带上计划项键的话，计划进度把这一项
// 判成「已成功」，用户拒绝追加额度后一直显示成功。
func TestGenerate_营销_超额挂起的结果不带计划项键(t *testing.T) {
	tool, _, _, g := newMarketingGenerate(approvedPlan())
	tool.Budget = overBudget{}
	res, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"v1","wait":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Suspend == nil || res.Suspend.Event != domain.EvOverBudget {
		t.Fatalf("应挂起等用户追加额度：%+v", res)
	}
	if res.PlanItemKey != "" || g.got.ModelCode != "" {
		t.Fatalf("超额的结果不带计划项键、也没有受理：key=%q 受理=%+v", res.PlanItemKey, g.got)
	}
	// 这一项在计划进度里仍是「未开始」：不带键的步骤不参与计算。
	v, ok := domain.PlanProgress([]domain.PlanItem{{Key: "v1", Label: "开箱"}},
		[]domain.PlanStep{{Key: res.PlanItemKey, Status: "success"}}, true)
	if !ok || v.Items[0].State != domain.PlanItemPending {
		t.Fatalf("超额的步骤不该让这一项变成已成功：%+v", v)
	}
}

type failingGenerator struct {
	*genGenerator
	err error
}

func (f failingGenerator) Accept(context.Context, app.GenerateInput) (app.GenerateResult, error) {
	return app.GenerateResult{}, f.err
}

func TestGenerate_通用会话不变(t *testing.T) {
	p := approvedPlan()
	tool, c, _, g := newMarketingGenerate(p)
	rc := genRC
	rc.Scene = domain.SceneGeneral
	args := `{"nodeType":"video","modelCode":"sd","operationCode":"text2video","prompt":"海边",` +
		`"params":{"aspectRatio":"9:16","resolution":"720p","marketing":{"picks":[]}},"durationSec":15,"wait":false,"planItemKey":"v1"}`
	res, err := tool.Run(context.Background(), rc, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Errorf("通用会话不读计划：%d", p.calls)
	}
	var acc map[string]any
	_ = json.Unmarshal(g.got.Params, &acc)
	want := map[string]any{"model": "sd", "prompt": "海边", "mode": "text2video",
		"aspect_ratio": "9:16", "resolution": "720p", "duration": "15", "marketing": map[string]any{"picks": []any{}}}
	if !reflect.DeepEqual(acc, want) || g.got.DurationSec != 15 || g.got.Inputs != nil {
		t.Fatalf("通用会话参数逐字不变（模型给什么发什么，不碰 marketing）：%v %+v", acc, g.got)
	}
	if res.PlanItemKey != "" || len(c.applied) != 2 {
		t.Errorf("通用会话不带计划项键：%+v", res)
	}

	// 通用会话的错误不包 StepError（文案与码都不变）。
	tool2, _, _, _ := newMarketingGenerate(p)
	tool2.Generator = failingGenerator{err: errors.New("boom")}
	_, err = tool2.Run(context.Background(), rc, json.RawMessage(`{"nodeType":"video","modelCode":"sd","operationCode":"text2video"}`))
	var se *app.StepError
	if err == nil || errors.As(err, &se) || err.Error() != "节点 a_new 已建好，但提交生成失败：boom" {
		t.Fatalf("通用会话错误逐字不变：%v", err)
	}

	// RunContext 没带场景时读会话判定（营销会话不能因为一个空字段绕过计划项约束）。
	rc.Scene = ""
	tool3, c3, _, g3 := newMarketingGenerate(approvedPlan())
	_, err = tool3.Run(context.Background(), rc, json.RawMessage(`{"nodeType":"video","modelCode":"sd","operationCode":"text2video"}`))
	rejection(t, err, c3, g3, "", domain.ToolErrPlanItemNotApproved)
}

// 04 文档：模型拿 generate 去「渲染」，每次先建一个空节点、再在报价时失败。
func TestGenerate_渲染不走generate且不建节点(t *testing.T) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[]}`)}
	tool, _, g := newGenerate(c)
	for _, op := range []string{"render", "timeline_render", "render_timeline"} {
		_, err := tool.Run(context.Background(), genRC, json.RawMessage(
			`{"operationCode":"`+op+`","modelCode":"render","nodeType":"video"}`))
		var rej *domain.ToolRejection
		if !errors.As(err, &rej) || rej.Code != domain.ToolErrWrongTool || !strings.Contains(rej.Message, "timeline_assemble") {
			t.Fatalf("%s：应指路 timeline_assemble + render：%v", op, err)
		}
	}
	if len(c.applied) != 0 || g.got.ModelCode != "" {
		t.Fatalf("拦在建节点之前：画布 %d 批、受理 %+v", len(c.applied), g.got)
	}

	// 营销会话：已批准计划里的渲染条目同样指路。
	p := approvedPlan()
	p.plan.Items = append(p.plan.Items, domain.PlanItem{Kind: "render", Key: "r1", Label: "成片渲染",
		Quantity: 15, Unit: "second"})
	mt, mc, _, mg := newMarketingGenerate(p)
	_, err := mt.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"r1"}`))
	var rej *domain.ToolRejection
	if !errors.As(err, &rej) || rej.Code != domain.ToolErrWrongTool || !strings.Contains(rej.Message, "render") {
		t.Fatalf("渲染条目应指路：%v", err)
	}
	if len(mc.applied) != 0 || mg.got.ModelCode != "" {
		t.Fatal("拦在建节点之前")
	}
}

// generatedRefIDs 是受理请求里参考输入的 ID（数组顺序，即发给上游的顺序）。
func generatedRefIDs(t *testing.T, g *genGenerator) []string {
	t.Helper()
	var out []string
	for _, in := range generatedInputs(t, g) {
		out = append(out, fmt.Sprint(in["referenceId"]))
	}
	return out
}
