package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// ---------------------------------------------------------------- MKT-T-28：素材裂变

// newFissionEnv 是一个带源视频的营销会话（20 张商品图、源视频 #777）：
// 模型 v2v 绑了 video2video（10/秒），模型 seedance 没绑（报价判「没绑这个操作」）。
func newFissionEnv(t *testing.T) *ptEnv {
	t.Helper()
	e, _ := newMktEnv(t, 0)
	si, err := app.ParseSceneInput(e.store.session.SceneInput)
	if err != nil {
		t.Fatal(err)
	}
	si.SourceVideo = &app.SceneRef{Kind: app.RefMedia, ID: "777"}
	raw, _ := json.Marshal(si)
	e.store.session.SceneInput = raw
	e.pricer.unit["v2v"] = 10
	e.pricer.opFaults = map[string]app.PricedItem{
		"seedance/video2video": {BlockedCode: "CAPABILITY_UNSUPPORTED", BlockedReason: "该模型不支持此操作",
			ArgFault: app.PlanFaultOperation, Operation: "video2video"},
	}
	return e
}

func TestSubmitPlan_裂变条目文案写明路径与费用(t *testing.T) {
	e := newFissionEnv(t)
	res, err := e.submitPlan(t, `[`+
		`{"type":"video","modelCode":"v2v","operationCode":"ref2video","durationSec":5,"key":"v1","variant":1,"productIndexes":[1,2]},`+
		`{"type":"video","modelCode":"seedance","operationCode":"ref2video","durationSec":10,"key":"v2","variant":2},`+
		`{"type":"video","modelCode":"seedance","operationCode":"ref2video","durationSec":5,"key":"plain","sourceVideo":false},`+
		`{"type":"image","modelCode":"seedance","operationCode":"img2img","key":"cover"}]`)
	if err != nil {
		t.Fatal(err)
	}
	items := e.lastCard(t).Items
	v1, v2, plain, cover := items[0], items[1], items[2], items[3]

	// ① 绑了 video2video 且报得出价 → 走视频生视频：操作改写、输入恰好 1 个视频、不选商品图。
	if v1.FissionRoute != domain.FissionVideo2Video || v1.OperationCode != "video2video" || v1.SourceVideoID != "777" {
		t.Fatalf("v1 应走 video2video：%+v", v1)
	}
	if !reflect.DeepEqual(v1.Inputs, map[string]int{"video": 1}) || len(v1.ProductMediaIDs) != 0 || v1.ImageLimit != nil {
		t.Fatalf("视频生视频只发源视频、不选商品图：%+v", v1)
	}
	if v1.Credits != 50 || !strings.Contains(v1.FissionNote, "视频生视频") || !strings.Contains(v1.FissionNote, "✦50") {
		t.Fatalf("v1 说明应写明路径与费用：%d %q", v1.Credits, v1.FissionNote)
	}

	// ② 没绑 video2video → 重新生成：操作保持模型写的、按商品图出、不发源视频，说明写清原因与费用。
	if v2.FissionRoute != domain.FissionRegenerate || v2.OperationCode != "ref2video" || v2.SourceVideoID != "777" {
		t.Fatalf("v2 应走重新生成：%+v", v2)
	}
	if !reflect.DeepEqual(v2.Inputs, map[string]int{"image": 3}) || len(v2.ProductMediaIDs) != 3 {
		t.Fatalf("重新生成按商品图出、不带 video：%+v", v2.Inputs)
	}
	if !strings.Contains(v2.FissionNote, "重新生成") || !strings.Contains(v2.FissionNote, "不支持以视频为输入") ||
		v2.Credits == 0 {
		t.Fatalf("v2 说明应写明原因：%q", v2.FissionNote)
	}
	if !strings.Contains(v2.FissionNote, "✦"+itoa(v2.Credits)) {
		t.Fatalf("v2 说明里的金额应等于条目报价 %d：%q", v2.Credits, v2.FissionNote)
	}

	// ③ 显式 sourceVideo:false 与图片条目：不是裂变条目，不带 video、没有说明。
	for _, it := range []domain.PlanItem{plain, cover} {
		if it.FissionRoute != "" || it.FissionNote != "" || it.SourceVideoID != "" || it.Inputs["video"] != 0 {
			t.Fatalf("非裂变条目不应带源视频：%+v", it)
		}
	}

	// 回给模型的摘要带路线与说明（Agent 向用户解释时照着说）。
	var content map[string]any
	_ = json.Unmarshal(res.Content, &content)
	briefs, _ := content["items"].([]any)
	b0, _ := briefs[0].(map[string]any)
	if b0["fissionRoute"] != "video2video" || b0["fissionNote"] != v1.FissionNote {
		t.Fatalf("摘要应带路线与说明：%v", b0)
	}
	// 判路线报了一次 video2video 的价（v1、v2 各一次）。
	n := 0
	for _, c := range e.pricer.calls {
		if c.OperationCode == "video2video" {
			n++
		}
	}
	if n != 3 { // v1 判路线 + v1 发卡报价 + v2 判路线
		t.Fatalf("video2video 报价次数应为 3：%d", n)
	}
}

func TestSubmitPlan_裂变参数写错退回(t *testing.T) {
	e := newFissionEnv(t)
	if _, err := e.submitPlan(t, `[{"type":"image","modelCode":"seedance","operationCode":"img2img","sourceVideo":true}]`); err == nil ||
		!strings.Contains(err.Error(), "只有视频条目") {
		t.Fatalf("图片条目带 sourceVideo 应退回：%v", err)
	}
	if _, err := e.submitPlan(t, `[{"type":"audio","modelCode":"tts","operationCode":"tts","sourceVideo":true}]`); err == nil {
		t.Fatal("配音条目带 sourceVideo 应退回")
	}
	// 会话没有源视频：显式 true 退回；不写则不是裂变条目（普通营销会话行为不变）。
	e2, _ := newMktEnv(t, 0)
	if _, err := e2.submitPlan(t, `[{"type":"video","modelCode":"seedance","operationCode":"ref2video","sourceVideo":true}]`); err == nil ||
		!strings.Contains(err.Error(), "没有源视频") {
		t.Fatalf("没有源视频时 sourceVideo:true 应退回：%v", err)
	}
	if len(e.store.cards)+len(e2.store.cards) != 0 {
		t.Fatal("退回时不发卡")
	}
	if _, err := e2.submitPlan(t, `[{"type":"video","modelCode":"seedance","operationCode":"ref2video"}]`); err != nil {
		t.Fatal(err)
	}
	if it := e2.lastCard(t).Items[0]; it.FissionRoute != "" || it.Inputs["video"] != 0 {
		t.Fatalf("没有源视频的会话不做裂变：%+v", it)
	}
}

func TestSubmitPlan_裂变_视频生视频不可用说明原因(t *testing.T) {
	e := newFissionEnv(t)
	// 绑了但报不出价（如缺价，运营问题）：走重新生成，原因照报价说。
	e.pricer.opFaults["v2v/video2video"] = app.PricedItem{BlockedCode: "PRICING_MISSING", BlockedReason: "未配置价格。"}
	if _, err := e.submitPlan(t, `[{"type":"video","modelCode":"v2v","operationCode":"video2video","durationSec":5}]`); err != nil {
		t.Fatal(err)
	}
	it := e.lastCard(t).Items[0]
	// 模型写的是 video2video：重新生成时退回按类型取的默认操作（报价映射里补）。
	if it.FissionRoute != domain.FissionRegenerate || it.OperationCode != "" ||
		!strings.Contains(it.FissionNote, "视频生视频暂不可用（未配置价格）") {
		t.Fatalf("应走重新生成并说明原因：%+v", it)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// ---------------------------------------------------------------- generate：按条目发源视频

func fissionPlan() *fakePlans {
	p := approvedPlan()
	p.plan.Input.SourceVideo = &app.SceneRef{Kind: app.RefMedia, ID: "777"}
	v2v := domain.Video2VideoItem(domain.PlanItem{Kind: "video", Key: "f1", Label: "裂变 1", ModelCode: "sd",
		Quantity: 5, Unit: "second", Credits: 50}, "777")
	regen := p.plan.Items[0]
	regen.Key = "f2"
	regen = domain.RegenerateItem(regen, "777", "所选模型不支持以视频为输入（视频生视频）")
	p.plan.Items = append(p.plan.Items, v2v, regen)
	return p
}

func generatedInputs(t *testing.T, g *genGenerator) []map[string]any {
	t.Helper()
	var inputs []map[string]any
	if err := json.Unmarshal(g.got.Inputs, &inputs); err != nil {
		t.Fatal(err)
	}
	return inputs
}

func TestGenerate_裂变_按条目发源视频(t *testing.T) {
	// video2video：只发源视频（1 个、以 reference 发），操作取条目。
	tool, _, _, g := newMarketingGenerate(fissionPlan())
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"f1","wait":false}`)); err != nil {
		t.Fatal(err)
	}
	if g.got.OperationCode != "video2video" ||
		!reflect.DeepEqual(generatedInputs(t, g), []map[string]any{{"kind": "reference", "referenceId": "777", "order": float64(1)}}) {
		t.Fatalf("视频生视频只发源视频：%s %s", g.got.OperationCode, g.got.Inputs)
	}

	// 重新生成：商品图 + 角色，不发源视频。
	tool, _, _, g = newMarketingGenerate(fissionPlan())
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"f2","wait":false}`)); err != nil {
		t.Fatal(err)
	}
	for _, in := range generatedInputs(t, g) {
		if in["referenceId"] == "777" {
			t.Fatalf("重新生成不发源视频：%s", g.got.Inputs)
		}
	}
	if n := len(generatedInputs(t, g)); n != 3 {
		t.Fatalf("重新生成 = 2 张商品图 + 1 个角色：%s", g.got.Inputs)
	}

	// 模型自己给了别的视频 → 与条目不一致，拒绝且无副作用。
	tool, c, _, g := newMarketingGenerate(fissionPlan())
	_, err := tool.Run(context.Background(), mkRC, json.RawMessage(
		`{"planItemKey":"f1","inputs":[{"kind":"reference","referenceId":"888","order":1}]}`))
	rejection(t, err, c, g, "f1", domain.ToolErrPlanItemMismatch)

	// 条目上的源视频与会话对不上（卡片被改过）→ 失败、不受理。
	p := fissionPlan()
	p.plan.Items[2].SourceVideoID = "999"
	tool, c, _, g = newMarketingGenerate(p)
	if _, err := tool.Run(context.Background(), mkRC, json.RawMessage(`{"planItemKey":"f1","wait":false}`)); err == nil ||
		g.got.ModelCode != "" || len(c.applied) != 0 {
		t.Fatalf("源视频对不上应失败且无副作用：%v %+v", err, g.got)
	}
}

// 04 文档 P-47：视频生视频的条目里商品在源视频画面中，选项的「需要商品」要算满足。
// 原先按「商品 0 张」判，部署数据里的选项全部要求商品，裂变一个选项都用不了。
func TestSubmitPlan_裂变_视频生视频的商品算满足(t *testing.T) {
	e := newFissionEnv(t)
	sc := e.deps.Scene.(*ptScene)
	if _, err := e.submitPlan(t, `[{"type":"video","modelCode":"v2v","operationCode":"ref2video","durationSec":5,`+
		`"key":"v1","picks":{"hook":"hk_new"}}]`); err != nil {
		t.Fatal(err)
	}
	q := sc.queries[len(sc.queries)-1]
	if q.Slots["product"] != 1 || q.Slots["source_video"] != 1 {
		t.Fatalf("视频生视频条目的槽位：商品算 1（在源视频里）、源视频 1：%v", q.Slots)
	}
}
