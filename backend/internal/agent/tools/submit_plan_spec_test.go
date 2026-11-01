package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 会话 107419662995685376（2026-10-24）：画幅 / 清晰度 / 时长都是「智能」，Agent 一项没问就自己定了
// 9:16 · 15 秒 · 720p；规划文档写着「15 秒」，提交的却是 5 秒 + 10 秒两段。

func (e *ptEnv) submitPlanWith(t *testing.T, spec app.VideoSpecState, items string) (ToolResult, error) {
	t.Helper()
	rc := ptRC(domain.StatePlanning)
	rc.Spec = spec
	return e.tool(domain.ToolSubmitPlan).Run(context.Background(), rc,
		json.RawMessage(`{"title":"营销","items":`+items+`}`))
}

func TestSubmitPlan_营销_规格是智能且没确认时退回(t *testing.T) {
	e, _ := newMktEnv(t, 0)
	unconfirmed := app.ResolveVideoSpec(app.GenPrefs{}, nil) // 三项都是「智能」
	_, err := e.submitPlanWith(t, unconfirmed, `[{"type":"video","modelCode":"seedance","durationSec":15}]`)
	var rej *domain.ToolRejection
	if !errors.As(err, &rej) || rej.Code != domain.ToolErrSpecUnconfirmed {
		t.Fatalf("应按 %s 退回：%v", domain.ToolErrSpecUnconfirmed, err)
	}
	for _, want := range []string{"画幅、清晰度、时长", "ask_user", "aspect_ratio / resolution / duration"} {
		if !strings.Contains(rej.Message, want) {
			t.Fatalf("退回原因应包含 %q：%s", want, rej.Message)
		}
	}
	if len(e.store.cards) != 0 {
		t.Fatal("退回时不发卡")
	}
	// 只有图片条目：不涉及视频规格，照常出卡。
	if _, err := e.submitPlanWith(t, unconfirmed, `[{"type":"image","modelCode":"img","key":"p1"}]`); err != nil {
		t.Fatalf("纯图片规划不要求确认视频规格：%v", err)
	}
	// 确认过（或偏好里选定）就放行。
	confirmed := app.ResolveVideoSpec(app.GenPrefs{}, map[string]string{
		domain.SpecRatio: "9:16", domain.SpecResolution: "720p", domain.SpecDuration: "15 秒"})
	if _, err := e.submitPlanWith(t, confirmed, `[{"type":"video","modelCode":"seedance","durationSec":15}]`); err != nil {
		t.Fatalf("确认过的应放行：%v", err)
	}
}

func TestSubmitPlan_营销_一支视频不拆段(t *testing.T) {
	e, _ := newMktEnv(t, 0)
	spec := app.ResolveVideoSpec(app.GenPrefs{Ratio: "9:16", Resolution: "720p", Duration: "15"}, nil)
	// 会话 107419662995685376 的原样提交：5 秒 + 10 秒。
	_, err := e.submitPlanWith(t, spec, `[`+
		`{"type":"video","modelCode":"seedance","operationCode":"ref2video","durationSec":5,"title":"开场痛点 + 商品亮相（0-5秒）"},`+
		`{"type":"video","modelCode":"seedance","operationCode":"ref2video","durationSec":10,"title":"细节展示 + 使用场景 + 收尾（5-15秒）"}]`)
	if err == nil || !strings.Contains(err.Error(), "第 1 项：时长 5 秒与用户选定的 15 秒不一致") ||
		!strings.Contains(err.Error(), "不要拆成几段") {
		t.Fatalf("拆段应退回并说清：%v", err)
	}
	if len(e.store.cards) != 0 {
		t.Fatal("退回时不发卡")
	}
	// 没写时长：补成确认的 15 秒（按它报价）；批量变体各自都是完整的 15 秒。
	if _, err := e.submitPlanWith(t, spec, `[{"type":"video","modelCode":"seedance","key":"v1","variant":1},`+
		`{"type":"video","modelCode":"seedance","key":"v2","variant":2,"durationSec":15}]`); err != nil {
		t.Fatal(err)
	}
	for _, it := range e.lastCard(t).Items {
		if it.Quantity != 15 || it.Unit != "second" {
			t.Fatalf("每一条都是完整的 15 秒视频：%+v", it)
		}
	}
}

func TestSubmitPlan_营销_裂变条目不按确认时长核对(t *testing.T) {
	e := newFissionEnv(t)
	spec := app.ResolveVideoSpec(app.GenPrefs{Ratio: "9:16", Resolution: "720p", Duration: "15"}, nil)
	if _, err := e.submitPlanWith(t, spec,
		`[{"type":"video","modelCode":"v2v","operationCode":"ref2video","durationSec":5,"key":"v1"}]`); err != nil {
		t.Fatalf("素材裂变的时长跟着源视频走，不核对：%v", err)
	}
	if it := e.lastCard(t).Items[0]; it.FissionRoute == "" || it.Quantity != 5 {
		t.Fatalf("裂变条目保持模型写的时长：%+v", it)
	}
}

// ---------------------------------------------------------------- 通用会话（2026-10-24 用户确认扩展）

func TestSubmitPlan_通用会话_规格没确认与拆段都退回(t *testing.T) {
	e := newPtEnv()
	// 规格都是「智能」（含 MCP 建的会话：没有输入框偏好）：带视频的规划退回，纯图片照常。
	_, err := e.submitPlanWith(t, app.ResolveVideoSpec(app.GenPrefs{}, nil), `[{"type":"video","modelCode":"seedance","durationSec":15}]`)
	var rej *domain.ToolRejection
	if !errors.As(err, &rej) || rej.Code != domain.ToolErrSpecUnconfirmed || !strings.Contains(rej.Message, "会话没有设置") {
		t.Fatalf("通用会话同样要求先确认：%v", err)
	}
	if _, err := e.submitPlanWith(t, app.ResolveVideoSpec(app.GenPrefs{}, nil), `[{"type":"image","modelCode":"img"}]`); err != nil {
		t.Fatalf("纯图片规划不要求确认视频规格：%v", err)
	}
	spec := app.ResolveVideoSpec(app.GenPrefs{Ratio: "16:9", Resolution: "1080p", Duration: "15"}, nil)
	// 没有成片渲染项：每个视频条目都是完整的一支。
	before := len(e.store.cards)
	_, err = e.submitPlanWith(t, spec, `[{"type":"video","modelCode":"seedance","durationSec":5},`+
		`{"type":"video","modelCode":"seedance","durationSec":10}]`)
	if err == nil || !strings.Contains(err.Error(), "与用户选定的 15 秒不一致") {
		t.Fatalf("通用会话拆段同样退回：%v", err)
	}
	if len(e.store.cards) != before {
		t.Fatal("退回时不发卡")
	}
	if _, err := e.submitPlanWith(t, spec, `[{"type":"video","modelCode":"seedance"}]`); err != nil {
		t.Fatal(err)
	}
	if it := e.lastCard(t).Items[0]; it.Quantity != 15 {
		t.Fatalf("没写时长补成选定的 15 秒：%+v", it)
	}
}

// 脚本线做长片（有成片渲染项、且不止一个视频条目）：只有一次生成放不下时才允许拆。
func TestSubmitPlan_通用会话_长片只在一次放不下时才拆(t *testing.T) {
	segments := func(n, sec int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(`{"type":"video","modelCode":"seedance","operationCode":"img2video","durationSec":` +
				strconv.Itoa(sec) + `,"inputs":{"image":1}},`)
		}
		return b.String()
	}
	plan := func(film, n, sec int) string {
		return `[{"type":"storyboard","modelCode":"llm","operationCode":"storyboard_gen"},` + segments(n, sec) +
			`{"type":"render","durationSec":` + strconv.Itoa(film) + `}]`
	}

	// 15 秒的片子一次就能生成：拆成 3 段走脚本线 → 退回。
	e := newPtEnv()
	spec15 := app.ResolveVideoSpec(app.GenPrefs{Ratio: "9:16", Resolution: "720p", Duration: "15"}, nil)
	_, err := e.submitPlanWith(t, spec15, plan(15, 3, 5))
	if err == nil || !strings.Contains(err.Error(), "一次就能生成") || !strings.Contains(err.Error(), "不要拆成 3 段") {
		t.Fatalf("放得下时不许拆：%v", err)
	}

	// 60 秒的片子而模型单次最长 30 秒：放不下，按脚本线拆成 4 段 → 照常出卡（分段不逐条核对时长）。
	e = newPtEnv()
	e.pricer.maxSec = map[string]int{"seedance": 30}
	spec60 := app.ResolveVideoSpec(app.GenPrefs{Ratio: "9:16", Resolution: "720p", Duration: "60"}, nil)
	if _, err := e.submitPlanWith(t, spec60, plan(60, 4, 15)); err != nil {
		t.Fatalf("一次放不下时允许走脚本线：%v", err)
	}
	if n := len(e.lastCard(t).Items); n != 6 {
		t.Fatalf("分镜 + 4 段 + 渲染：%d 项", n)
	}

	// 成片时长与选定的不一致 → 退回。
	if _, err := e.submitPlanWith(t, spec60, plan(50, 4, 15)); err == nil || !strings.Contains(err.Error(), "成片时长 50 秒与用户选定的 60 秒不一致") {
		t.Fatalf("成片时长要等于选定的：%v", err)
	}
}

// 只有一条视频时不核对时长：用户在对话里说「改成 10 秒」是正常的改动（还要过规划确认卡），不该被选定的 15 秒挡住。
func TestSubmitPlan_单条视频改时长不拦(t *testing.T) {
	spec := app.ResolveVideoSpec(app.GenPrefs{Ratio: "9:16", Resolution: "720p", Duration: "15"}, nil)
	e := newPtEnv()
	if _, err := e.submitPlanWith(t, spec, `[{"type":"video","modelCode":"seedance","durationSec":10}]`); err != nil {
		t.Fatalf("通用会话单条视频改时长：%v", err)
	}
	if it := e.lastCard(t).Items[0]; it.Quantity != 10 {
		t.Fatalf("按模型写的 10 秒：%+v", it)
	}
	m, _ := newMktEnv(t, 0)
	if _, err := m.submitPlanWith(t, spec, `[{"type":"video","modelCode":"seedance","durationSec":10}]`); err != nil {
		t.Fatalf("营销会话单条视频改时长：%v", err)
	}
	// 两条都是完整的 15 秒（几支独立的视频 / 变体）：放行。
	if _, err := e.submitPlanWith(t, spec, `[{"type":"video","modelCode":"seedance","durationSec":15},`+
		`{"type":"video","modelCode":"seedance","durationSec":15}]`); err != nil {
		t.Fatalf("几支完整的视频不算拆段：%v", err)
	}
}
