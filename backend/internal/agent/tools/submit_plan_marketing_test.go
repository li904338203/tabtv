package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 假 PlanScene

type ptOpt struct {
	kind, name    string
	version       int
	tags          []string
	audioRequired bool
}

type ptScene struct {
	limits map[string]int // 模型 → image 上限；没有 = found=false
	// opLimits「模型/操作」→ image 上限（优先于 limits）：生成方式不同，可收的图不同。
	opLimits map[string]int
	live     map[string]ptOpt
	noAudio  map[string]bool
	resolved []string // 走 Resolve（新钉）的 code
	queries  []app.PlanPickQuery
}

func (s *ptScene) ImageLimit(_ context.Context, it domain.PlanItem) (int, bool, error) {
	if n, ok := s.opLimits[it.ModelCode+"/"+it.OperationCode]; ok {
		return n, true, nil
	}
	n, ok := s.limits[it.ModelCode]
	return n, ok, nil
}

func (s *ptScene) PlanPicks(_ context.Context, q app.PlanPickQuery, _ time.Time) ([]app.PlanPickFact, error) {
	s.queries = append(s.queries, q)
	var out []app.PlanPickFact
	for _, r := range q.Refs {
		o := s.live[r.Code]
		f := app.PlanPickFact{Tags: o.tags}
		pinned := false
		for _, p := range q.Pinned {
			if p.Kind == r.Kind && p.Code == r.Code {
				f.Pin, pinned = app.ScenePin{Kind: p.Kind, Code: p.Code, Version: p.Version, Name: o.name}, true
				break
			}
		}
		if !pinned {
			if o.kind != r.Kind {
				return nil, httpx.Errf(errcode.MarketingOptionUnavailable, "Hook「"+r.Code+"」不存在")
			}
			s.resolved = append(s.resolved, r.Code)
			f.Pin, f.New = app.ScenePin{Kind: r.Kind, Code: r.Code, Version: o.version, Name: o.name}, true
		}
		if o.audioRequired && q.OutputType == "video" && s.noAudio[q.ModelCode] {
			f.BlockCode, f.BlockReason, f.UserFixable = "needs_audio_model", "Hook「"+o.name+"」需要能出声的视频模型", true
		}
		out = append(out, f)
	}
	return out, nil
}

func (ptScene) TalkNotice(tags []string) string {
	for _, t := range tags {
		if t == "口播" {
			return "画面为人物对镜讲解，不保证口型与语音逐字同步"
		}
	}
	return ""
}

// newMktEnv 是一个营销会话：20 张商品图，建会话时选了创意 cr_a@1。
func newMktEnv(t *testing.T, characters int) (*ptEnv, *ptScene) {
	t.Helper()
	e := newPtEnv()
	sc := &ptScene{limits: map[string]int{"seedance": 3}, noAudio: map[string]bool{},
		live: map[string]ptOpt{
			"cr_a":      {kind: "creative", name: "痛点解决", version: 5},
			"hk_new":    {kind: "hook", name: "反问开场", version: 2},
			"hk_sound":  {kind: "hook", name: "抓耳音效", version: 1, audioRequired: true},
			"st_talk":   {kind: "style", name: "对镜口播", version: 1, tags: []string{"口播"}},
			"st_simple": {kind: "style", name: "极简白", version: 1},
		}}
	si := app.SceneInput{Characters: []app.SceneRef{}, Pins: []app.ScenePinEntry{},
		Picks: app.ScenePicks{Creative: &app.ScenePick{Code: "cr_a", Version: 1, Name: "痛点解决"}}}
	for i := 1; i <= 20; i++ {
		si.Products = append(si.Products, strconv.Itoa(100+i))
	}
	for i := 0; i < characters; i++ {
		si.Characters = append(si.Characters, app.SceneRef{Kind: "media", ID: strconv.Itoa(900 + i)})
	}
	raw, _ := json.Marshal(si)
	e.store.session.Scene, e.store.session.SceneInput = domain.SceneMarketing, raw
	e.deps.Scene = sc
	return e, sc
}

func (e *ptEnv) submitPlan(t *testing.T, items string) (ToolResult, error) {
	t.Helper()
	return e.tool(domain.ToolSubmitPlan).Run(context.Background(), ptRC(domain.StatePlanning),
		json.RawMessage(`{"title":"营销","items":`+items+`}`))
}

func (e *ptEnv) lastCard(t *testing.T) domain.PlanConfirmPayload {
	t.Helper()
	if len(e.store.cards) == 0 {
		t.Fatal("没有发卡")
	}
	var p domain.PlanConfirmPayload
	if err := json.Unmarshal(e.store.cards[len(e.store.cards)-1].Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *ptEnv) scenePins(t *testing.T) []app.ScenePinEntry {
	t.Helper()
	si, err := app.ParseSceneInput(e.store.session.SceneInput)
	if err != nil {
		t.Fatal(err)
	}
	return si.Pins
}

// ---------------------------------------------------------------- 用例

func TestSubmitPlan_营销_超上限不出卡(t *testing.T) {
	e, _ := newMktEnv(t, 0)
	_, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance","operationCode":"ref2video","key":"v1",`+
		`"productIndexes":[1,2,3,4],"picks":{"hook":"hk_new"}}]`)
	var rej *domain.ToolRejection
	if !errors.As(err, &rej) || rej.Code != domain.ToolErrTooManyImages {
		t.Fatalf("应回 too_many_images：%v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(rej.Content(), &m)
	if m["error"] != "too_many_images" || m["item"] != "v1" || m["max"].(float64) != 3 || m["total"].(float64) != 20 {
		t.Fatalf("工具结果应带条目键与上限：%v", m)
	}
	if len(e.store.cards) != 0 || len(e.pricer.calls) != 0 {
		t.Fatalf("超上限不出卡、不报价：cards=%d calls=%d", len(e.store.cards), len(e.pricer.calls))
	}
	if len(e.scenePins(t)) != 0 {
		t.Fatal("没发卡时不钉新版本")
	}
}

func TestSubmitPlan_营销_缺省取前Y张(t *testing.T) {
	e, _ := newMktEnv(t, 1) // 1 个角色占掉一张图：商品图上限 = 3 − 1 = 2
	if _, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance","operationCode":"ref2video","durationSec":10,`+
		`"inputs":{"image":9}}]`); err != nil {
		t.Fatal(err)
	}
	it := e.lastCard(t).Items[0]
	if it.Key != "item1" || it.ProductTotal != 20 || it.ImageLimit == nil || *it.ImageLimit != 2 {
		t.Fatalf("条目键 / 总数 / 上限不对：%+v", it)
	}
	if !reflect.DeepEqual(it.ProductIndexes, []int{1, 2}) || !reflect.DeepEqual(it.ProductMediaIDs, []string{"101", "102"}) {
		t.Fatalf("缺省按槽顺序取前 2 张：%v %v", it.ProductIndexes, it.ProductMediaIDs)
	}
	// 输入计数由服务端算（商品图 2 + 角色 1），不信模型写的 image:9；报价按的也是它。
	if !reflect.DeepEqual(it.Inputs, map[string]int{"image": 3}) ||
		!reflect.DeepEqual(e.pricer.calls[0].Inputs, map[string]int{"image": 3}) {
		t.Fatalf("输入计数应为 image:3：卡片 %v 报价 %v", it.Inputs, e.pricer.calls[0].Inputs)
	}
	if it.Credits != 40 {
		t.Fatalf("报价应照常：%d", it.Credits)
	}
	// 继承会话的创意，版本是建会话时钉住的 v1（不是当前的 v5）。
	if len(it.Picks) != 1 || it.Picks[0] != (domain.PlanPick{Kind: "creative", Code: "cr_a", Version: 1, Name: "痛点解决"}) {
		t.Fatalf("picks 应继承会话：%+v", it.Picks)
	}
}

func TestSubmitPlan_营销_新选项钉版本且不重钉(t *testing.T) {
	e, sc := newMktEnv(t, 0)
	items := `[{"type":"video","modelCode":"seedance","key":"v1","productIndexes":[2],"picks":{"hook":"hk_new"}},` +
		`{"type":"video","modelCode":"seedance","key":"v2","productIndexes":[3,1],"picks":{"hook":"hk_new","creative":""}}]`
	if _, err := e.submitPlan(t, items); err != nil {
		t.Fatal(err)
	}
	p := e.lastCard(t)
	if !reflect.DeepEqual(sc.resolved, []string{"hk_new"}) {
		t.Fatalf("同一张卡里同一个新 code 只钉一次：%v", sc.resolved)
	}
	want1 := []domain.PlanPick{{Kind: "creative", Code: "cr_a", Version: 1, Name: "痛点解决"},
		{Kind: "hook", Code: "hk_new", Version: 2, Name: "反问开场"}}
	if !reflect.DeepEqual(p.Items[0].Picks, want1) {
		t.Fatalf("第 1 项 picks：%+v", p.Items[0].Picks)
	}
	// 第 2 项：creative 填空串 = 本项不用；hook 用同一版本；选图顺序即发出顺序。
	if !reflect.DeepEqual(p.Items[1].Picks, want1[1:]) || !reflect.DeepEqual(p.Items[1].ProductMediaIDs, []string{"103", "101"}) {
		t.Fatalf("第 2 项：%+v", p.Items[1])
	}
	pins := e.scenePins(t)
	if len(pins) != 1 || pins[0] != (app.ScenePinEntry{Kind: "hook", Code: "hk_new", Version: 2, Source: app.PinSourcePlan}) {
		t.Fatalf("新钉的版本应并进 scene_input.pins：%+v", pins)
	}

	// 运营把 hk_new 改到 v3 之后再规划：仍是 v2（同会话同 code 只认第一次）。
	o := sc.live["hk_new"]
	o.version = 3
	sc.live["hk_new"] = o
	if _, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance","key":"v1","picks":{"hook":"hk_new"}}]`); err != nil {
		t.Fatal(err)
	}
	if got := e.lastCard(t).Items[0].Picks[1]; got.Version != 2 || len(sc.resolved) != 1 {
		t.Fatalf("已钉住的不重钉：%+v resolved=%v", got, sc.resolved)
	}
	if len(e.scenePins(t)) != 1 {
		t.Fatalf("pins 不应重复：%+v", e.scenePins(t))
	}
}

// 会话 107398945801502720：模型把会话所选创意的**名称**写进 picks，原先按 code 查不到、报「创意不存在」。
func TestSubmitPlan_营销_picks写成会话选项名称时换成code(t *testing.T) {
	e, sc := newMktEnv(t, 0)
	// 第 1 项写会话所选创意的名称；第 2 项先用 code 新钉一个 Hook，第 3 项再写它的名称（同一张卡里刚钉的也认）。
	items := `[{"type":"video","modelCode":"seedance","key":"v1","picks":{"creative":"痛点解决"}},` +
		`{"type":"video","modelCode":"seedance","key":"v2","picks":{"hook":"hk_new"}},` +
		`{"type":"video","modelCode":"seedance","key":"v3","picks":{"hook":" 反问开场 "}}]`
	if _, err := e.submitPlan(t, items); err != nil {
		t.Fatal(err)
	}
	p := e.lastCard(t)
	if want := (domain.PlanPick{Kind: "creative", Code: "cr_a", Version: 1, Name: "痛点解决"}); len(p.Items[0].Picks) != 1 ||
		p.Items[0].Picks[0] != want {
		t.Fatalf("名称应换成会话钉住的 code 与版本：%+v", p.Items[0].Picks)
	}
	if got := p.Items[2].Picks; len(got) != 2 || got[1] != (domain.PlanPick{Kind: "hook", Code: "hk_new", Version: 2, Name: "反问开场"}) {
		t.Fatalf("同卡刚钉的选项写名称也认：%+v", got)
	}
	if !reflect.DeepEqual(sc.resolved, []string{"hk_new"}) {
		t.Fatalf("只有用 code 新钉的那一次走 Resolve：%v", sc.resolved)
	}

	// 没在本会话出现过的名称不去全库匹配：照原样交给 Resolve，报错并给改法。
	if _, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance","picks":{"style":"极简白"}}]`); err == nil ||
		!strings.Contains(err.Error(), "改法") {
		t.Fatalf("未确认过的名称应照常报错：%v", err)
	}
}

func TestPickCode_只认本会话且不猜重名(t *testing.T) {
	mk := &marketingPlan{pinned: []app.ScenePin{
		{Kind: "creative", Code: "cr_a", Name: "痛点解决"},
		{Kind: "hook", Code: "hk_1", Name: "开场"},
		{Kind: "hook", Code: "hk_2", Name: "开场"},
		{Kind: "style", Code: "暖光", Name: "st_x"}, // 名称恰好等于另一个值：code 精确命中优先
		{Kind: "style", Code: "st_x", Name: ""},
	}}
	for _, c := range []struct{ kind, in, want string }{
		{"creative", "痛点解决", "cr_a"},
		{"creative", "cr_a", "cr_a"},
		{"hook", "痛点解决", "痛点解决"},  // 维度不对不换
		{"hook", "开场", "开场"},      // 同名两个 code：不猜
		{"style", "st_x", "st_x"}, // 已是钉住的 code
		{"creative", "", ""},
		{"creative", "cr_new", "cr_new"},
	} {
		if got := mk.pickCode(c.kind, c.in); got != c.want {
			t.Errorf("%s/%q → %q，want %q", c.kind, c.in, got, c.want)
		}
	}
}

func TestSubmitPlan_营销_不出声Blocked(t *testing.T) {
	e, sc := newMktEnv(t, 0)
	sc.noAudio["seedance"] = true
	res, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance","key":"v1","picks":{"hook":"hk_sound"}},`+
		`{"type":"video","modelCode":"seedance","key":"v2","picks":{"hook":"hk_new"}}]`)
	if err != nil {
		t.Fatal(err)
	}
	p := e.lastCard(t)
	b := p.Items[0]
	if b.BlockedCode != errcode.MarketingOptionUnavailable.Name() || !b.UserFixable || b.Credits != 0 ||
		!strings.Contains(b.BlockedReason, "出声") {
		t.Fatalf("依赖出声的 Hook 配不出声模型应 Blocked(userFixable)：%+v", b)
	}
	if !p.Items[1].Available() || p.Items[1].Credits != 20 || p.CanApprove || p.Total != 20 {
		t.Fatalf("其余项照常报价、整卡不能批准：%+v", p)
	}
	if res.Suspend == nil {
		t.Fatal("照常发卡挂起")
	}
}

func TestSubmitPlan_营销_口播notices(t *testing.T) {
	e, _ := newMktEnv(t, 0)
	if _, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance","key":"v1","picks":{"style":"st_simple"}}]`); err != nil {
		t.Fatal(err)
	}
	if n := e.lastCard(t).Notices; len(n) != 0 {
		t.Fatalf("没用到口播类选项不加提示：%v", n)
	}
	res, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance","key":"v1","picks":{"style":"st_talk"}}]`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"画面为人物对镜讲解，不保证口型与语音逐字同步"}
	if n := e.lastCard(t).Notices; !reflect.DeepEqual(n, want) {
		t.Fatalf("口播类选项应追加如实提示：%v", n)
	}
	if !strings.Contains(string(res.Content), want[0]) {
		t.Fatalf("回给模型的结果也带提示：%s", res.Content)
	}
	// 当前技能带「口播」标签同样追加。
	e.store.skills["talk"] = repo.Skill{Code: "talk", Tags: json.RawMessage(`["口播"]`)}
	e.store.session.SkillCodes = json.RawMessage(`["talk"]`)
	if _, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance","key":"v1"}]`); err != nil {
		t.Fatal(err)
	}
	if n := e.lastCard(t).Notices; !reflect.DeepEqual(n, want) {
		t.Fatalf("技能带口播标签应追加提示：%v", n)
	}
}

func TestSubmitPlan_营销_参数退回给模型(t *testing.T) {
	e, _ := newMktEnv(t, 0)
	for _, c := range []struct{ items, msg string }{
		{`[{"type":"video","modelCode":"seedance","productIndexes":[21]}]`, "#21 不存在"},
		{`[{"type":"video","modelCode":"seedance","productIndexes":[1,1]}]`, "重复"},
		{`[{"type":"video","modelCode":"seedance","key":"a"},{"type":"video","modelCode":"seedance","key":"a"}]`, "重复"},
		{`[{"type":"video","modelCode":"seedance","key":"有中文"}]`, "key 只能是"},
		{`[{"type":"audio","modelCode":"tts","productIndexes":[1]}]`, "只有 video / image"},
		{`[{"type":"video","modelCode":"seedance","picks":{"music":"x"}}]`, "picks 只能有"},
		{`[{"type":"video","modelCode":"seedance","picks":{"hook":"st_simple"}}]`, "改法"},
	} {
		if _, err := e.submitPlan(t, c.items); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s 应退回并含「%s」：%v", c.items, c.msg, err)
		}
	}
	if len(e.store.cards) != 0 {
		t.Fatalf("参数错误不发卡：%d", len(e.store.cards))
	}
	// 模型 × 操作查不到上限：不猜，交给报价（这里的假报价照常给价）；不选图。
	if _, err := e.submitPlan(t, `[{"type":"image","modelCode":"img","key":"p1"}]`); err != nil {
		t.Fatal(err)
	}
	if it := e.lastCard(t).Items[0]; it.ImageLimit != nil || len(it.ProductMediaIDs) != 0 || it.Key != "p1" {
		t.Fatalf("查不到上限时不选图：%+v", it)
	}
}

func TestSubmitPlan_营销_未接入不退回通用规划(t *testing.T) {
	e, _ := newMktEnv(t, 0)
	e.deps.Scene = nil
	if _, err := e.submitPlan(t, `[{"type":"video","modelCode":"seedance"}]`); err == nil ||
		!strings.Contains(err.Error(), "营销规划未接入") {
		t.Fatalf("营销会话没有 Scene 端口时应报错：%v", err)
	}
}

func TestSubmitPlan_通用会话不解营销字段(t *testing.T) {
	e := newPtEnv()
	if _, err := e.tool(domain.ToolSubmitPlan).Run(context.Background(), ptRC(domain.StatePlanning),
		json.RawMessage(`{"items":[{"type":"video","modelCode":"seedance","key":"v1","productIndexes":[1],"picks":{"hook":"x"}}]}`)); err != nil {
		t.Fatal(err)
	}
	var p domain.PlanConfirmPayload
	_ = json.Unmarshal(e.store.cards[0].Payload, &p)
	if p.Items[0].Key != "" || p.Items[0].ProductIndexes != nil || p.Notices != nil ||
		strings.Contains(string(e.store.cards[0].Payload), "productTotal") {
		t.Fatalf("通用会话的卡片不带营销字段：%s", e.store.cards[0].Payload)
	}
}
