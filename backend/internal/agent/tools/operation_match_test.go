package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 04 文档「技能和模型参数」：带了参考图 / 商品图，却走了文生视频。

// ptOps 是假的模型生成方式目录。
type ptOps map[string][]domain.OperationFit

func (o ptOps) ModelOperations(_ context.Context, model string) ([]domain.OperationFit, error) {
	return o[model], nil
}

func videoFits() []domain.OperationFit {
	return []domain.OperationFit{
		{Code: "text2video", Output: "video", Roles: map[string]domain.RoleRange{"text": {Min: 0, Max: 8}}},
		{Code: "img2video", Output: "video", Roles: map[string]domain.RoleRange{"image": {Min: 1, Max: 1},
			"text": {Min: 0, Max: 8}}, Limits: "单次时长 4~30 秒"},
		{Code: "ref2video", Output: "video", Roles: map[string]domain.RoleRange{"image": {Min: 0, Max: 9},
			"text": {Min: 0, Max: 8}}},
	}
}

func TestQuote_带图没写生成方式时按模型能力挑(t *testing.T) {
	e := newPtEnv()
	e.deps.Ops = ptOps{"seedance": videoFits()}
	res, err := run(t, e.tool(domain.ToolQuote), ptRC(domain.StatePlanning),
		`{"type":"video","modelCode":"seedance","durationSec":5,"inputs":{"image":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	if last := e.pricer.calls[len(e.pricer.calls)-1]; last.OperationCode != "img2video" {
		t.Fatalf("没写生成方式、带 1 张图应按图生视频报价（原先一律文生视频）：%s", last.OperationCode)
	}
	if !strings.Contains(string(res.Content), `"operationCode":"img2video"`) {
		t.Fatalf("结果要告诉模型实际用的生成方式：%s", res.Content)
	}
}

func TestQuote_收不下时把可用的生成方式一并告诉模型(t *testing.T) {
	e := newPtEnv()
	e.deps.Ops = ptOps{"grok": videoFits()[:1]} // 只会文生视频：换不了
	e.pricer.faults = map[string]app.PricedItem{"grok": {BlockedReason: "该模型此操作不接受 image 输入",
		ArgFault: app.PlanFaultInputs, Operation: "text2video"}}
	res, err := run(t, e.tool(domain.ToolQuote), ptRC(domain.StatePlanning),
		`{"type":"video","modelCode":"grok","operationCode":"text2video","inputs":{"image":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Content), "该模型能用的生成方式") || !strings.Contains(string(res.Content), "text2video：只收文字") {
		t.Fatalf("改法里要列出该模型能用的生成方式，而不是只说「去掉 inputs」：%s", res.Content)
	}
}

func TestSubmitPlan_带图写成文生视频时换成收图的方式并在卡片上说明(t *testing.T) {
	e := newPtEnv()
	e.deps.Ops = ptOps{"seedance": videoFits()}
	res, err := run(t, e.tool(domain.ToolSubmitPlan), ptRC(domain.StatePlanning),
		`{"items":[{"type":"video","modelCode":"seedance","operationCode":"text2video","durationSec":5,`+
			`"inputs":{"image":1},"title":"开场"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	card := planPayload(t, e.store.cards[len(e.store.cards)-1])
	if card.Items[0].OperationCode != "img2video" {
		t.Fatalf("卡片上的条目应改成图生视频：%+v", card.Items[0])
	}
	if len(card.Notices) != 1 || !strings.Contains(card.Notices[0], "已改为图生视频（img2video）") {
		t.Fatalf("换了生成方式要在卡片上说明：%v", card.Notices)
	}
	var content struct {
		Items []struct {
			OperationCode string `json:"operationCode"`
		} `json:"items"`
	}
	_ = json.Unmarshal(res.Content, &content)
	if len(content.Items) != 1 || content.Items[0].OperationCode != "img2video" {
		t.Fatalf("结果要把实际生成方式告诉模型（执行时照着用）：%s", res.Content)
	}
}

func TestSubmitPlan_收得下就不换(t *testing.T) {
	e := newPtEnv()
	e.deps.Ops = ptOps{"seedance": videoFits()}
	if _, err := run(t, e.tool(domain.ToolSubmitPlan), ptRC(domain.StatePlanning),
		`{"items":[{"type":"video","modelCode":"seedance","operationCode":"ref2video","durationSec":5,"inputs":{"image":1}}]}`); err != nil {
		t.Fatal(err)
	}
	card := planPayload(t, e.store.cards[len(e.store.cards)-1])
	if card.Items[0].OperationCode != "ref2video" || len(card.Notices) != 0 {
		t.Fatalf("模型选的方式收得下就照用、不写说明：%+v %v", card.Items[0], card.Notices)
	}
}

// 实测会话 106895426245885952：选了商品图的条目配 text2video，提示「最多用 0 张」，模型把商品图、
// 再把要求商品图的营销选项一并去掉。所选模型明明支持图生视频。
func TestSubmitPlan_营销_选了商品图的文生视频换成收图的方式(t *testing.T) {
	e, sc := newMktEnv(t, 0)
	e.deps.Ops = ptOps{"seedance": videoFits()}
	sc.opLimits = map[string]int{"seedance/text2video": 0, "seedance/img2video": 1, "seedance/ref2video": 9}
	if _, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance","operationCode":"text2video",`+
		`"durationSec":5,"productIndexes":[1]}]`); err != nil {
		t.Fatal(err)
	}
	card := e.lastCard(t)
	it := card.Items[0]
	if it.OperationCode != "img2video" || len(it.ProductMediaIDs) != 1 {
		t.Fatalf("应换成图生视频并带上商品图 #1：%+v", it)
	}
	var noted bool
	for _, n := range card.Notices {
		noted = noted || strings.Contains(n, "已改为图生视频")
	}
	if !noted {
		t.Fatalf("卡片上要说明换了生成方式：%v", card.Notices)
	}

	// 没写 productIndexes、也没写生成方式：至少带主图，挑收得下的方式（原先默认文生视频 → 0 张）。
	e2, sc2 := newMktEnv(t, 0)
	e2.deps.Ops = ptOps{"seedance": videoFits()}
	sc2.opLimits = sc.opLimits
	if _, err := e2.submitPlan(t, `[{"type":"video","modelCode":"seedance","durationSec":5}]`); err != nil {
		t.Fatal(err)
	}
	if it := e2.lastCard(t).Items[0]; it.OperationCode != "img2video" || len(it.ProductMediaIDs) == 0 {
		t.Fatalf("有商品图时缺省要用上：%+v", it)
	}
}

func TestSubmitPlan_营销_明确不用商品图时不换(t *testing.T) {
	e, sc := newMktEnv(t, 0)
	e.deps.Ops = ptOps{"seedance": videoFits()}
	sc.opLimits = map[string]int{"seedance/text2video": 0, "seedance/img2video": 1}
	if _, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance","operationCode":"text2video",`+
		`"durationSec":5,"productIndexes":[]}]`); err != nil {
		t.Fatal(err)
	}
	if it := e.lastCard(t).Items[0]; it.OperationCode != "text2video" || len(it.ProductMediaIDs) != 0 {
		t.Fatalf("productIndexes 写成 [] 是明确不用商品图（空镜头），照模型写的来：%+v", it)
	}
}

func TestSubmitPlan_营销_模型不收图时不说减到0张(t *testing.T) {
	e, sc := newMktEnv(t, 0)
	e.deps.Ops = ptOps{"t2v": videoFits()[:1]}
	sc.opLimits = map[string]int{"t2v/text2video": 0}
	_, err := e.submitPlan(t, `[{"type":"video","modelCode":"t2v","operationCode":"text2video",`+
		`"durationSec":5,"productIndexes":[1]}]`)
	var rej *domain.ToolRejection
	if !errors.As(err, &rej) || rej.Code != domain.ToolErrTooManyImages {
		t.Fatalf("应退回 too_many_images：%v", err)
	}
	if strings.Contains(rej.Message, "减到 0 张") || !strings.Contains(rej.Message, "不收图片") ||
		!strings.Contains(rej.Message, "该模型能用的生成方式") {
		t.Fatalf("要说清是生成方式不收图、该换方式，而不是让模型把商品图去掉：%s", rej.Message)
	}
}
