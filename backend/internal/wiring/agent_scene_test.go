package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	mktdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
)

// sceneStore 是营销用户端仓储的内存版（可见性走 domain.Visible，与 repo 的同义谓词一致）。
type sceneStore struct {
	opts     []marketingrepo.Option
	versions map[string][]marketingrepo.OptionVersion
}

func (s *sceneStore) ListVisibleOptions(_ context.Context, f marketingrepo.VisibleFilter) (
	[]marketingrepo.Option, bool, error) {
	var out []marketingrepo.Option
	for _, o := range s.opts {
		if o.Kind == f.Kind && mktdomain.Visible(o.Option, f.Now) &&
			(f.Category == "" || o.CategoryCode == f.Category) {
			out = append(out, o)
		}
	}
	return out, false, nil
}

func (s *sceneStore) VisibleRecommendedModels(context.Context, mktdomain.Kind, time.Time) ([]string, error) {
	return nil, nil
}

func (s *sceneStore) GetOptionByCode(_ context.Context, code string) (marketingrepo.Option, error) {
	for _, o := range s.opts {
		if o.Code == code {
			return o, nil
		}
	}
	return marketingrepo.Option{}, marketingrepo.ErrNotFound
}

func (s *sceneStore) GetVersionByCode(_ context.Context, code string, v int) (marketingrepo.OptionVersion, error) {
	for _, x := range s.versions[code] {
		if x.Version == v {
			return x, nil
		}
	}
	return marketingrepo.OptionVersion{}, marketingrepo.ErrNotFound
}

func (s *sceneStore) ListCategories(context.Context, mktdomain.Kind) ([]marketingrepo.Category, error) {
	return nil, nil
}

type sceneCatalog struct{}

func (sceneCatalog) UsableModels(_ context.Context, out string) ([]marketingapp.ModelRef, error) {
	if out != mktdomain.OutputVideo {
		return nil, nil
	}
	return []marketingapp.ModelRef{{Code: "seedance_v25", Name: "Seedance 2.5"}}, nil
}

type fakeAssetNames map[int64]string

func (f fakeAssetNames) AssetName(_ context.Context, _ int64, id int64) (string, error) {
	if n, ok := f[id]; ok {
		return n, nil
	}
	return "", errors.New("not found")
}

const secretSnippet = "SNIPPET_原文_不得外泄"

func sceneOption(kind mktdomain.Kind, code, name string, version int, enabled bool) marketingrepo.Option {
	return marketingrepo.Option{ID: int64(len(code)), Option: mktdomain.Option{
		Kind: kind, Code: code, Name: name, Description: name + "（当前说明）", Prompt: secretSnippet + code,
		CoverMediaIDs: []int64{770001}, Tags: []string{"爆款"}, IsEnabled: enabled, Version: version,
	}}
}

func snapshotJSON(t *testing.T, kind mktdomain.Kind, code, name, desc string, v int) marketingrepo.OptionVersion {
	t.Helper()
	b, err := json.Marshal(mktdomain.Snapshot{Pick: mktdomain.Pick{Kind: kind, Code: code, Version: v},
		Name: name, Description: desc, Prompt: secretSnippet})
	if err != nil {
		t.Fatal(err)
	}
	return marketingrepo.OptionVersion{Version: v, Snapshot: b}
}

func newSceneFixture(t *testing.T) AgentSceneContext {
	t.Helper()
	creative := sceneOption(mktdomain.KindCreative, "giant_drop", "巨物落地", 2, true)
	creative.RecommendedModels = []string{"seedance_v25", "gone_model"}
	creative.RecommendedParams = json.RawMessage(`{"aspectRatio":"9:16","durationSec":[10,15]}`)
	hook := sceneOption(mktdomain.KindHook, "ask_open", "提问开场", 1, true)
	offline := sceneOption(mktdomain.KindHook, "old_hook", "已下线 Hook", 1, false)
	start := time.Now().Add(48 * time.Hour)
	future := sceneOption(mktdomain.KindStyle, "future_style", "未开始风格", 1, true)
	future.StartAt = &start
	style := sceneOption(mktdomain.KindStyle, "warm_sun", "暖调日光", 1, true)
	store := &sceneStore{
		opts: []marketingrepo.Option{creative, hook, offline, future, style},
		versions: map[string][]marketingrepo.OptionVersion{
			"giant_drop": {snapshotJSON(t, mktdomain.KindCreative, "giant_drop", "巨物从天而降", "商品放大后落地", 1),
				snapshotJSON(t, mktdomain.KindCreative, "giant_drop", "巨物落地", "第二版说明", 2)},
			"old_hook": {snapshotJSON(t, mktdomain.KindHook, "old_hook", "老 Hook", "前 2 秒抛问题", 1)},
		},
	}
	svc := marketingapp.NewService(store, sceneCatalog{}, nil, marketingapp.Settings{MaxVariants: 4})
	return AgentSceneContext{Marketing: svc, Assets: fakeAssetNames{3301: "林澈"}, Variants: 4}
}

// MKT-AC-05 / 06（上下文部分）：营销输入段不含 URL、mediaId、片段原文。
func TestAgentSceneContext_营销输入不含URL与片段原文(t *testing.T) {
	sc := newSceneFixture(t)
	si := agentapp.SceneInput{
		Products: []string{"900111", "900222", "900333"},
		Product:  agentapp.SceneProduct{Name: "山野冷萃咖啡液", SellingPoints: "0 蔗糖\n冷水 3 秒即溶 https://evil.example"},
		Characters: []agentapp.SceneRef{{Kind: "asset", ID: "3301"}, {Kind: "media", ID: "900444"},
			{Kind: "asset", ID: "3399"}},
		Picks: agentapp.ScenePicks{
			Creative: &agentapp.ScenePick{Code: "giant_drop", Version: 1, Name: "巨物从天而降"},
			// 钉住的是已下线的 Hook：照常按钉住的版本写名称与说明。
			Hook: &agentapp.ScenePick{Code: "old_hook", Version: 1, Name: "老 Hook"},
		},
	}
	got, err := sc.MarketingSummary(context.Background(), 11, si, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "商品：山野冷萃咖啡液\n" +
		"卖点：0 蔗糖 冷水 3 秒即溶 https://evil.example\n" +
		"商品图：共 3 张，#1 为主图。本条消息未附图；用 view_media 按序号查看。\n" +
		"角色：林澈、上传的人物图 #1、" + unknownCharacterAsset + "\n" +
		"所选创意：巨物从天而降（code giant_drop） —— 商品放大后落地（推荐：Seedance 2.5 · 9:16 · 10~15 秒）\n" +
		"所选 Hook：老 Hook（code old_hook） —— 前 2 秒抛问题\n" +
		"所选风格：未选择\n" +
		"单次最多 4 个变体；每个生成项选用的商品图不超过该模型的参考图上限，规划时逐项写明用了哪几张。"
	if got != want {
		t.Errorf("营销输入：\n%s\n期望：\n%s", got, want)
	}
	// 系统给出的部分不含 URL / 媒体 ID / 片段原文 / 模型 code（用户自己在卖点里写的 URL 是他的原文，
	// 只放在「卖点：」标签之后，SlotSummary 已压掉换行）。
	// ⚠️ 选项 code 要有（submit_plan 的 picks 填的就是它；list_marketing_options 本来也回 code）：
	// 原先这里连选项 code 也排除，模型只好把名称当 code 传（会话 107398945801502720）。
	sys := got[strings.Index(got, "\n商品图："):]
	for _, bad := range []string{"http", "900111", "900444", "3301", "3399", "770001", "SNIPPET", "seedance_v25",
		"gone_model", "第二版说明"} {
		if strings.Contains(sys, bad) {
			t.Errorf("营销输入不应含 %q：\n%s", bad, got)
		}
	}
	// 拼进系统提示词后同样只在「## 营销输入」之下。
	out := agentapp.BuildContext(agentapp.ContextInput{State: domain.StatePlanning, Scene: domain.SceneMarketing,
		SceneNote: "场景要求", SceneInput: got, Budget: 100})
	if !strings.Contains(out.System, "## 场景：营销\n场景要求\n\n## 营销输入\n商品：山野冷萃咖啡液") {
		t.Errorf("系统提示词：\n%s", out.System)
	}
}

// 商品库 / 角色库会话（MKT-T-37）：描述行 + 角色库角色的名称与形象 / 音色摘要；参考音频只说「有」，
// 主图与音频的媒体 ID 都不进上下文（MKT-AC-45 接口部分）。
func TestAgentSceneContext_商品库会话带描述与角色文字(t *testing.T) {
	sc := newSceneFixture(t)
	si := agentapp.SceneInput{
		ProductID: "190001", ProductVersion: 3,
		Products: []string{"900111", "900222"},
		Product:  agentapp.SceneProduct{Name: "保温杯", SellingPoints: "24 小时", Description: "双层真空\n不锈钢"},
		Characters: []agentapp.SceneRef{
			{Kind: "media", ID: "900444", CharacterID: "194901", Name: "柚子君", Appearance: "圆脸柚子拟人",
				VoiceText: "活泼少年音", VoiceMediaID: "900555"},
			{Kind: "asset", ID: "3301"},
		},
	}
	got, err := sc.MarketingSummary(context.Background(), 11, si, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"卖点：24 小时\n描述：双层真空 不锈钢\n",
		"角色：柚子君（形象：圆脸柚子拟人；音色：活泼少年音；有参考音频，不发送给模型）、林澈\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q：\n%s", want, got)
		}
	}
	for _, bad := range []string{"900444", "900555", "194901", "190001", "http"} {
		if strings.Contains(got, bad) {
			t.Errorf("上下文不应含 %q：\n%s", bad, got)
		}
	}
}

// 渲染场景段的变量必须覆盖用途契约里登记的全部占位符：少给一个，运营一引用就整条渲染失败、静默回落兜底。
func TestMarketingSceneVars_覆盖用途契约(t *testing.T) {
	spec, ok := catalogdomain.LookupPromptSpec(agentapp.MarketingSceneTemplate)
	if !ok {
		t.Fatalf("%s 未登记进提示词用途表（管理端无法编辑）", agentapp.MarketingSceneTemplate)
	}
	want := append(append([]string{}, spec.Required...), spec.Optional...)
	sort.Strings(want)
	vars := agentapp.MarketingSceneVars(domain.StatePlanning, "m", 4, 2)
	got := make([]string, 0, len(vars))
	for k := range vars {
		got = append(got, k)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("渲染变量 %v 与用途契约 %v 不一致", got, want)
	}
	// 用全部占位符写一个模板，按这份变量渲染必须成功。
	tpl := ""
	for _, k := range want {
		tpl += "{{" + k + "}} "
	}
	if err := catalogdomain.ValidatePromptContent(agentapp.MarketingSceneTemplate, tpl); err != nil {
		t.Fatal(err)
	}
	if _, err := catalogdomain.RenderPrompt(tpl, vars); err != nil {
		t.Errorf("给全变量应能渲染：%v", err)
	}
}

// MKT-AC-06：工具只列此刻在投的选项，结果里没有片段原文与媒体 ID。
func TestListMarketingOptions_只列可见且无片段(t *testing.T) {
	sc := newSceneFixture(t)
	tool := agenttools.ListMarketingOptions{Options: sc}
	res, err := tool.Run(context.Background(), agentapp.RunContext{}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw := string(res.Content)
	var got agentapp.MarketingOptionsResult
	if err := json.Unmarshal(res.Content, &got); err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, o := range got.Items {
		codes = append(codes, o.Code)
	}
	if want := []string{"giant_drop", "ask_open", "warm_sun"}; !reflect.DeepEqual(codes, want) {
		t.Errorf("应只列在投的（按创意 → Hook → 风格）：%v，期望 %v", codes, want)
	}
	for _, bad := range []string{"SNIPPET", "prompt", "770001", "cover", "old_hook", "future_style"} {
		if strings.Contains(raw, bad) {
			t.Errorf("工具结果不应含 %q：%s", bad, raw)
		}
	}
	// 每项只有约定的字段。
	var generic struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(res.Content, &generic); err != nil || len(generic.Items) != 3 {
		t.Fatalf("结果形状不对：%s", raw)
	}
	items := generic.Items
	allowed := map[string]bool{"kind": true, "code": true, "name": true, "description": true, "categoryCode": true,
		"tags": true, "isAudioRequired": true}
	for _, it := range items {
		for k := range it {
			if !allowed[k] {
				t.Errorf("工具结果出现约定外的字段 %q", k)
			}
		}
	}
	// 按 kind 筛。
	res, err = tool.Run(context.Background(), agentapp.RunContext{}, json.RawMessage(`{"kind":"hook"}`))
	if err != nil || !strings.Contains(string(res.Content), "ask_open") || strings.Contains(string(res.Content), "giant_drop") {
		t.Errorf("按 kind 筛：%s %v", res.Content, err)
	}
	if _, err := tool.Run(context.Background(), agentapp.RunContext{}, json.RawMessage(`{"kind":"bogus"}`)); err == nil {
		t.Error("非法 kind 应报错")
	}
}
