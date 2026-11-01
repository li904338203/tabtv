package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// fakeSceneCtx 是 SceneContext 的假实现：记录渲染变量与摘要入参。
type fakeSceneCtx struct {
	renderErr   error
	fallbackErr error
	summaryErr  error
	vars        map[string]string
	summaryIn   *SceneInput
	visible     int
	calls       int
}

func (f *fakeSceneCtx) RenderScenePrompt(_ context.Context, code, _ string, vars map[string]string) (string, error) {
	f.calls++
	f.vars = vars
	if code != MarketingSceneTemplate {
		return "", errors.New("unexpected code " + code)
	}
	if f.renderErr != nil {
		return "", f.renderErr
	}
	return "DB 场景要求（阶段 " + vars["state"] + "）", nil
}

func (f *fakeSceneCtx) FallbackScenePrompt(_ string, vars map[string]string) (string, error) {
	f.vars = vars
	if f.fallbackErr != nil {
		return "", f.fallbackErr
	}
	return "兜底场景要求", nil
}

func (f *fakeSceneCtx) MaxVariants() int { return 4 }

func (f *fakeSceneCtx) MarketingSummary(_ context.Context, _ int64, in SceneInput, visible int) (string, error) {
	f.calls++
	f.summaryIn, f.visible = &in, visible
	if f.summaryErr != nil {
		return "", f.summaryErr
	}
	return "商品：山野冷萃\n卖点：0 蔗糖\n商品图：共 2 张，#1 为主图。", nil
}

func goldenGeneralInput(st domain.State) ContextInput {
	return ContextInput{
		State:      st,
		Skills:     []SkillPrompt{{Code: "video_single", Name: "单段视频", Body: "技能正文"}},
		Memory:     "记忆",
		CanvasData: json.RawMessage(`{"nodes":[{"id":"n1","type":"video","title":"成片"}],"edges":[]}`),
		Messages:   []repo.Message{msg(1, repo.RoleUser, "做个视频"), msg(2, repo.RoleAssistant, "好")},
		Budget:     1000,
		VideoModel: "- 模型：vm-1",
	}
}

func marketingSession() repo.Session {
	si, _ := json.Marshal(SceneInput{
		Products: []string{"9001", "9002"}, Product: SceneProduct{Name: "山野冷萃", SellingPoints: "0 蔗糖"},
		Characters: []SceneRef{}, Pins: []ScenePinEntry{},
		Picks: ScenePicks{Creative: &ScenePick{Code: "giant_drop", Version: 3, Name: "巨物从天而降"}},
	})
	return repo.Session{ID: 7, WorkspaceID: 11, Scene: domain.SceneMarketing, SceneInput: si}
}

// MKR-10：营销改动不得泄漏到创造页 —— 通用会话的输出与改动前**逐字相等**。
// testdata 里的 golden 是改动前的 BuildContext 输出（MKT-T-18 开工前生成，不要重新生成它）。
// 唯一一处手工改动：骨架里「删除确认卡」那一句（04 文档 P-31，对所有会话生效，不是营销泄漏），只替换了那一句。
func TestBuildContext_通用会话不含场景段(t *testing.T) {
	for _, st := range []domain.State{domain.StatePlanning, domain.StateExecuting} {
		want, err := os.ReadFile("testdata/general_context_" + string(st) + ".golden.json")
		if err != nil {
			t.Fatal(err)
		}
		for _, sc := range []domain.Scene{"", domain.SceneGeneral} {
			in := goldenGeneralInput(st)
			in.Scene = sc
			// 即使误填了两段正文，通用会话也一个字都不写。
			in.SceneNote, in.SceneInput = "不该出现", "也不该出现"
			got, _ := json.Marshal(BuildContext(in))
			if !bytes.Equal(got, bytes.TrimSpace(want)) {
				t.Errorf("通用会话（%s，scene=%q）的上下文与改动前不一致：\n%s\n期望：\n%s", st, sc, got, want)
			}
		}
	}
	// 通用会话不问营销域（一次都不调用端口）。
	f := &fakeSceneCtx{}
	note, input := sceneSections(context.Background(), f, nil, repo.Session{Scene: domain.SceneGeneral},
		domain.StatePlanning, "m", 0)
	if note != "" || input != "" || f.calls != 0 {
		t.Errorf("通用会话不应有场景段：note=%q input=%q calls=%d", note, input, f.calls)
	}
}

func TestBuildContext_营销场景段在阶段说明之后技能之前(t *testing.T) {
	f := &fakeSceneCtx{}
	sess := marketingSession()
	in := goldenGeneralInput(domain.StatePlanning)
	in.Scene = sess.Scene
	in.SceneNote, in.SceneInput = sceneSections(context.Background(), f, nil, sess, domain.StatePlanning, "gpt_x", 0)
	sys := BuildContext(in).System

	marks := []string{
		"你是 seeAitv 画布里的创作助手",
		domain.PlanningModeNote,
		domain.SceneHeading(domain.SceneMarketing) + "\nDB 场景要求",
		domain.MarketingInputHeading + "\n商品：山野冷萃",
		"## 已加载技能：",
		"## 项目记忆",
		"## 当前视频模型",
		"## 画布摘要",
	}
	prev := -1
	for _, m := range marks {
		i := strings.Index(sys, m)
		if i < 0 {
			t.Fatalf("系统提示词缺少 %q：\n%s", m, sys)
		}
		if i <= prev {
			t.Errorf("%q 的位置（%d）应在前一段（%d）之后：\n%s", m, i, prev, sys)
		}
		prev = i
	}
	if !strings.HasPrefix(sys, domain.SystemPromptSkeleton+"\n\n"+domain.PlanningModeNote+"\n\n## 场景：营销\n") {
		t.Errorf("骨架与阶段说明应原样在前、场景段紧随其后：\n%s", sys)
	}
	if strings.Count(sys, "## 场景：营销") != 1 || strings.Count(sys, "## 营销输入") != 1 {
		t.Errorf("两段标题各只出现一次：\n%s", sys)
	}
	// 执行态同样排在阶段说明之后。
	in.State = domain.StateExecuting
	sys = BuildContext(in).System
	if strings.Index(sys, domain.ExecutingModeNote) > strings.Index(sys, "## 场景：营销") {
		t.Error("执行态：场景段应在阶段说明之后")
	}
	// 摘要拿到的是会话上钉住的 scene_input，附图张数原样传过去。
	if f.summaryIn == nil || f.summaryIn.Product.Name != "山野冷萃" || len(f.summaryIn.Products) != 2 || f.visible != 0 {
		t.Errorf("摘要入参不对：%+v visible=%d", f.summaryIn, f.visible)
	}
}

func TestBuildContext_营销输入不含URL与片段原文(t *testing.T) {
	f := &fakeSceneCtx{}
	sess := marketingSession()
	in := goldenGeneralInput(domain.StatePlanning)
	in.Scene = sess.Scene
	in.SceneNote, in.SceneInput = sceneSections(context.Background(), f, nil, sess, domain.StatePlanning, "m", 0)
	sys := BuildContext(in).System
	seg := sys[strings.Index(sys, domain.MarketingInputHeading):strings.Index(sys, "## 已加载技能")]
	for _, bad := range []string{"http", "9001", "9002", "giant_drop", "mediaId", "objectKey"} {
		if strings.Contains(seg, bad) {
			t.Errorf("营销输入段不应含 %q：\n%s", bad, seg)
		}
	}
	// 摘要取不到时写一句中性说明，不编造、不中断。
	f2 := &fakeSceneCtx{summaryErr: errors.New("db down http://x/9001")}
	_, input := sceneSections(context.Background(), f2, nil, sess, domain.StatePlanning, "m", 0)
	if input != sceneInputUnavailable {
		t.Errorf("摘要失败应写中性说明，得到 %q", input)
	}
	// scene_input 坏了同样（并且不去问营销域）。
	bad := sess
	bad.SceneInput = json.RawMessage(`{"products":`)
	f3 := &fakeSceneCtx{}
	_, input = sceneSections(context.Background(), f3, nil, bad, domain.StatePlanning, "m", 0)
	if input != sceneInputUnavailable || f3.summaryIn != nil {
		t.Errorf("scene_input 坏了应写中性说明且不调摘要：%q", input)
	}
}

func TestBuildContext_场景模板渲染失败回落兜底且告警(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := &fakeSceneCtx{renderErr: errors.New("渲染缺少变量：x")}
	note, _ := sceneSections(context.Background(), f, log, marketingSession(), domain.StateExecuting, "gpt_x", 0)
	if note != "兜底场景要求" {
		t.Errorf("渲染失败应回落兜底，得到 %q", note)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "回落代码兜底") {
		t.Errorf("回落应记 Warn：%s", buf.String())
	}
	// 变量给全（缺一个，运营一引用就整条渲染失败）。
	keys := make([]string, 0, len(f.vars))
	for k := range f.vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"maxVariants", "productCount", "reasoningModel", "state"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("渲染变量 = %v，期望 %v", keys, want)
	}
	if f.vars["state"] != "executing" || f.vars["reasoningModel"] != "gpt_x" || f.vars["maxVariants"] != "4" ||
		f.vars["productCount"] != "2" {
		t.Errorf("渲染变量取值不对：%v", f.vars)
	}
	// 兜底也失败：场景段留空，但标题与营销输入照常（模型仍知道这是营销会话）。
	buf.Reset()
	f = &fakeSceneCtx{renderErr: errors.New("x"), fallbackErr: errors.New("y")}
	sess := marketingSession()
	note, input := sceneSections(context.Background(), f, log, sess, domain.StatePlanning, "m", 0)
	if note != "" || input == "" || !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("兜底失败：note=%q input=%q log=%s", note, input, buf.String())
	}
	sys := BuildContext(ContextInput{State: domain.StatePlanning, Scene: sess.Scene, SceneNote: note,
		SceneInput: input, Budget: 100}).System
	if !strings.Contains(sys, "## 场景：营销\n\n## 营销输入\n商品：") {
		t.Errorf("场景段为空时标题照写：\n%s", sys)
	}
	// 端口未接入：只写标题并告警，不 panic。
	buf.Reset()
	if n, i := sceneSections(context.Background(), nil, log, sess, domain.StatePlanning, "m", 0); n != "" || i != "" ||
		!strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("端口未接入：%q %q %s", n, i, buf.String())
	}
}
