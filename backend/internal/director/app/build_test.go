package app

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/director/domain"
	"github.com/li904338203/tabtv/backend/internal/director/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// AI 搭景编排（DRT-19）的单测：全部端口都是假的，断言调用顺序与次数。

const goodDraft = `{"objects":[
	{"op":"add","ref":"阿强","kind":"character","posePreset":"fight_stance","facing":{"target":"老刀"}},
	{"op":"add","ref":"老刀","kind":"character","posePreset":"fight_stance","relation":{"type":"facing","target":"阿强","distance":2.0}}],
	"camera":{"angle":"low","side":"front"},"notes":["街道环境不在导演台里搭，只摆人物"]}`

type fakeScenes struct {
	mu        sync.Mutex
	scene     SceneState
	reads     int
	saves     int
	conflicts int // 前几次保存回冲突
	saved     json.RawMessage
}

func (f *fakeScenes) SceneForBuild(context.Context, int64, int64, int64, collab.Role) (SceneState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.scene, nil
}

func (f *fakeScenes) SaveScene(_ context.Context, _, _, _ int64, _ Actor, _ collab.Role, version int,
	body json.RawMessage) (SceneState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	if f.conflicts > 0 {
		f.conflicts--
		f.scene.Version++ // 别人刚存了一版
		return SceneState{}, httpx.Errf(errcode.VersionConflict, "冲突")
	}
	if version != f.scene.Version {
		return SceneState{}, httpx.Errf(errcode.VersionConflict, "冲突")
	}
	f.scene.Version++
	f.scene.Body, f.saved = body, body
	return f.scene, nil
}

type fakeText struct {
	out   string
	err   error
	block bool
	calls []TextRequest
}

func (f *fakeText) Complete(ctx context.Context, in TextRequest) (string, error) {
	f.calls = append(f.calls, in)
	if f.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return f.out, f.err
}

type fakeCatalog struct {
	choice      Choice
	unavailable *Unavailable
	asked       []int
}

func (f *fakeCatalog) SceneBuildChoice(_ context.Context, _ int64, images int) (Choice, *Unavailable, error) {
	f.asked = append(f.asked, images)
	return f.choice, f.unavailable, nil
}

type fakeImages struct{ bad map[int64]bool }

func (f fakeImages) InlineImage(_ context.Context, _, id int64) (string, error) {
	if f.bad[id] {
		return "", errors.New("不是你的")
	}
	return "data:image/png;base64,AAAA", nil
}

type fakePrompts struct{}

func (fakePrompts) RenderPrompt(context.Context, string, string, map[string]string) (string, error) {
	return "你是一名导演。", nil
}

type fakeCalls struct {
	mu       sync.Mutex
	inserted []repo.LLMCall
	finished map[int64]repo.FinishInput
	reject   repo.LimitDecision
	used     int
}

func (f *fakeCalls) InsertWithLimits(_ context.Context, c repo.LLMCall, _ repo.Limits) (repo.LimitDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reject.Reject != repo.RejectNone {
		return f.reject, nil
	}
	f.inserted = append(f.inserted, c)
	return repo.LimitDecision{}, nil
}

func (f *fakeCalls) Finish(_ context.Context, id int64, in repo.FinishInput) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finished == nil {
		f.finished = map[int64]repo.FinishInput{}
	}
	f.finished[id] = in
	return true, nil
}

func (f *fakeCalls) Usage(context.Context, int64) (int, int, error) { return 0, f.used, nil }
func (f *fakeCalls) RecoverStale(context.Context, time.Duration, int) (int, error) {
	return 0, nil
}
func (f *fakeCalls) DeleteOlderThan(context.Context, time.Time, int) (int, error) { return 0, nil }

type harness struct {
	svc     *Service
	scenes  *fakeScenes
	text    *fakeText
	catalog *fakeCatalog
	calls   *fakeCalls
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		scenes:  &fakeScenes{scene: SceneState{ID: 3, ProjectID: 2, CanvasNodeID: "n_dir", Version: 4, Body: domain.DefaultScene()}},
		text:    &fakeText{out: goodDraft},
		catalog: &fakeCatalog{choice: Choice{ModelCode: "vision-1", MaxImages: 4}},
		calls:   &fakeCalls{},
	}
	var seq int64
	h.svc = New(Ports{Scenes: h.scenes, Text: h.text, Catalog: h.catalog, Images: fakeImages{},
		Prompts: fakePrompts{}, Calls: h.calls}, Config{CallTimeout: time.Second},
		func() int64 { seq++; return 1000 + seq }, nil)
	return h
}

func webInput() BuildInput {
	return BuildInput{WorkspaceID: 1, ProjectID: 2, SceneID: 3, Role: collab.RoleEditor, Actor: Actor{UserID: 7},
		Prompt: "两个人在街头对峙，低角度仰拍", SceneVersion: 4}
}

func codeOf(t *testing.T, err error) errcode.Code {
	t.Helper()
	var be *httpx.BizError
	if !errors.As(err, &be) {
		t.Fatalf("应返回业务错误，得到 %v", err)
	}
	return be.Code
}

func TestBuild_成功回提案并记succeeded(t *testing.T) {
	h := newHarness(t)
	res, err := h.svc.Build(context.Background(), webInput())
	if err != nil {
		t.Fatal(err)
	}
	if res.BaseVersion != 4 || res.ModelCode != "vision-1" || len(res.Proposal.Changes.Added) == 0 {
		t.Fatalf("提案不对：%+v", res)
	}
	if h.scenes.saves != 0 {
		t.Fatal("网页路径只回提案，不写场景")
	}
	if len(h.calls.inserted) != 1 || h.calls.finished[res.CallID].Status != repo.StatusSucceeded {
		t.Fatalf("应插 1 条记录并记 succeeded：%+v", h.calls.finished)
	}
	c := h.calls.inserted[0]
	if c.Source != "web" || c.UserID != 7 || c.ModelCode != "vision-1" || c.PromptChars == 0 {
		t.Errorf("调用记录内容不对：%+v", c)
	}
	req := h.text.calls[0]
	if req.Operation != OperationSceneBuild || !strings.Contains(req.System, domain.ContractHeading) ||
		!strings.HasPrefix(req.System, "你是一名导演。") || !strings.Contains(req.User, "两个人在街头对峙") ||
		!strings.Contains(req.User, "### 当前场景") {
		t.Fatalf("请求形状不对：%+v", req)
	}
}

func TestBuild_场景版本不一致409(t *testing.T) {
	h := newHarness(t)
	in := webInput()
	in.SceneVersion = 3
	_, err := h.svc.Build(context.Background(), in)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.VersionConflict || be.Details["currentVersion"] != 4 {
		t.Fatalf("版本不一致应 40902 并带当前版本，得到 %v", err)
	}
	if len(h.text.calls) != 0 || len(h.calls.inserted) != 0 {
		t.Fatal("版本不一致时不该调模型、不该记调用")
	}
}

func TestBuild_没有可用模型42203(t *testing.T) {
	h := newHarness(t)
	h.catalog.unavailable = &Unavailable{Reason: "AI 搭景暂不可用：没有绑定模型"}
	_, err := h.svc.Build(context.Background(), webInput())
	if codeOf(t, err) != errcode.CapabilityUnsupported || !strings.Contains(err.Error(), "没有绑定模型") {
		t.Fatalf("没有可用模型应 42203 并带原因，得到 %v", err)
	}
	if len(h.text.calls) != 0 || len(h.calls.inserted) != 0 {
		t.Fatal("不可用时不该调模型、不该记调用")
	}
}

// 免费靠配置成立：报价 > 0 时目录域给出「按约定免费」的不可用原因，这里原样转成 42203，**不调账本**。
func TestBuild_报价大于0拒绝(t *testing.T) {
	h := newHarness(t)
	h.catalog.unavailable = &Unavailable{Reason: "AI 搭景按约定免费，当前配置了价格，请联系运营改为 0"}
	_, err := h.svc.Build(context.Background(), webInput())
	if codeOf(t, err) != errcode.CapabilityUnsupported || !strings.Contains(err.Error(), "按约定免费") {
		t.Fatalf("报价大于 0 应 42203 并写明按约定免费，得到 %v", err)
	}
}

func TestBuild_参考图超过模型上限(t *testing.T) {
	h := newHarness(t)
	h.catalog.choice.MaxImages = 1
	in := webInput()
	in.ReferenceMediaIDs = []int64{11, 12}
	_, err := h.svc.Build(context.Background(), in)
	if codeOf(t, err) != errcode.InvalidArgument || !strings.Contains(err.Error(), "最多 1 张") {
		t.Fatalf("超过模型的图片上限应 40001 并说明上限，得到 %v", err)
	}
	if h.catalog.asked[0] != 2 {
		t.Errorf("选模型时要按实际张数问（能力矩阵），得到 %v", h.catalog.asked)
	}
	// 超过 DR-PARAM-14 的 4 张：连模型都不用问。
	in.ReferenceMediaIDs = []int64{1, 2, 3, 4, 5}
	if _, err := h.svc.Build(context.Background(), in); codeOf(t, err) != errcode.InvalidArgument {
		t.Fatalf("超过 4 张应 40001，得到 %v", err)
	}
}

func TestBuild_参考图内联(t *testing.T) {
	h := newHarness(t)
	in := webInput()
	in.ReferenceMediaIDs = []int64{11, 12}
	if _, err := h.svc.Build(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	req := h.text.calls[0]
	if len(req.Images) != 2 {
		t.Fatalf("两张参考图都要带上，得到 %d", len(req.Images))
	}
	for _, img := range req.Images {
		if !strings.HasPrefix(img, "data:image/") || strings.Contains(img, "http") {
			t.Fatalf("参考图只能内联，不能外链（DR-N-11），得到 %q", img)
		}
	}
	if !strings.Contains(req.User, "图 1 到图 2") {
		t.Errorf("user 消息要说明参考图编号：%s", req.User)
	}
	// 不可用的图（不是你的 / 不是图片）：在限流之前就拒，不占额度。
	h2 := newHarness(t)
	h2.svc.p.Images = fakeImages{bad: map[int64]bool{12: true}}
	_, err := h2.svc.Build(context.Background(), in)
	if codeOf(t, err) != errcode.InvalidArgument || !strings.Contains(err.Error(), "第 2 张") {
		t.Fatalf("不可用的参考图应 40001 并点名第几张，得到 %v", err)
	}
	if len(h2.calls.inserted) != 0 {
		t.Fatal("参考图不可用时不该占一次额度")
	}
}

func TestBuild_解析失败不改场景并记failed(t *testing.T) {
	h := newHarness(t)
	h.text.out = "好的，我来帮你搭一个街头对峙的场景，两个人面对面站着。"
	_, err := h.svc.Build(context.Background(), webInput())
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.DirectorDraftInvalid {
		t.Fatalf("模型回的是文字应 42216，得到 %v", err)
	}
	if snippet, _ := be.Details["snippet"].(string); !strings.Contains(snippet, "街头对峙") {
		t.Errorf("详情要带原文片段，得到 %v", be.Details)
	}
	fin := h.calls.finished[h.calls.inserted[0].ID]
	if fin.Status != repo.StatusFailed || fin.ErrorCode != callErrDraftInvalid {
		t.Fatalf("调用记录应为 failed / draft_invalid，得到 %+v", fin)
	}
	if h.scenes.saves != 0 {
		t.Fatal("解析失败不能改场景")
	}
}

func TestBuild_上游超时映射50202(t *testing.T) {
	h := newHarness(t)
	h.svc.cfg.CallTimeout = 30 * time.Millisecond
	h.text.block = true
	_, err := h.svc.Build(context.Background(), webInput())
	if codeOf(t, err) != errcode.ProviderTimeout {
		t.Fatalf("模型调用超时应 50202，得到 %v", err)
	}
	if fin := h.calls.finished[h.calls.inserted[0].ID]; fin.Status != repo.StatusFailed || fin.ErrorCode != callErrTimeout {
		t.Fatalf("调用记录应为 failed / provider_timeout，得到 %+v", fin)
	}
	// 其余上游错误：50201。
	h2 := newHarness(t)
	h2.text.err = errors.New("upstream 500")
	if _, err := h2.svc.Build(context.Background(), webInput()); codeOf(t, err) != errcode.ProviderError {
		t.Fatalf("上游报错应 50201，得到 %v", err)
	}
}

func TestBuild_限流映射429(t *testing.T) {
	h := newHarness(t)
	h.calls.reject = repo.LimitDecision{Reject: repo.RejectHourly, RetryAfter: 125 * time.Second}
	_, err := h.svc.Build(context.Background(), webInput())
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.RateLimited || be.Details["retry_after_seconds"] != 125 ||
		be.Details["scope"] != "hourly" || be.Details["limit"] != 20 {
		t.Fatalf("小时额度用完应 42901 并带 retry_after_seconds / scope / limit，得到 %v", err)
	}
	h.calls.reject = repo.LimitDecision{Reject: repo.RejectConcurrency}
	if _, err := h.svc.Build(context.Background(), webInput()); codeOf(t, err) != errcode.ConcurrencyLimited {
		t.Fatalf("并发超限应 42902，得到 %v", err)
	}
	if len(h.text.calls) != 0 {
		t.Fatal("被限流时不该调模型")
	}
}

func TestBuild_只读成员被拒(t *testing.T) {
	h := newHarness(t)
	in := webInput()
	in.Role = collab.RoleViewer
	if _, err := h.svc.Build(context.Background(), in); codeOf(t, err) != errcode.PermissionDenied {
		t.Fatalf("viewer 搭景应 403，得到 %v", err)
	}
}

func TestBuildConfig_与Build同一判断(t *testing.T) {
	h := newHarness(t)
	h.calls.used = 3
	cfg, err := h.svc.BuildConfig(context.Background(), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Available || cfg.MaxImages != 4 || cfg.UsedThisHour != 3 || cfg.PerHour != 20 ||
		cfg.Concurrency != 1 || cfg.MaxPrompt != MaxPromptChars {
		t.Fatalf("可用时的配置不对：%+v", cfg)
	}
	if len(h.calls.inserted) != 0 {
		t.Fatal("build-config 只读，不插调用记录")
	}
	h.catalog.unavailable = &Unavailable{Reason: "需要会员", UserFixable: true}
	cfg, _ = h.svc.BuildConfig(context.Background(), 7, 0)
	_, buildErr := h.svc.Build(context.Background(), webInput())
	if cfg.Available || cfg.Reason != "需要会员" || !cfg.UserFixable || !strings.Contains(buildErr.Error(), "需要会员") {
		t.Fatalf("build-config 与 build 应给出同一个结论：%+v / %v", cfg, buildErr)
	}
	if len(h.catalog.asked) != 3 {
		t.Errorf("两条路都要问同一个目录函数，得到 %d 次", len(h.catalog.asked))
	}
}

func TestBuildAndApply_冲突重做一次(t *testing.T) {
	h := newHarness(t)
	h.scenes.conflicts = 1
	in := webInput()
	in.Actor = Actor{UserID: 7, AgentSessionID: 99}
	in.SceneVersion = 0
	out, err := h.svc.BuildAndApply(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.text.calls) != 1 {
		t.Fatalf("冲突重做不重新调模型，得到 %d 次", len(h.text.calls))
	}
	if h.scenes.saves != 2 || h.scenes.reads != 2 {
		t.Fatalf("应读两次场景、存两次（第一次冲突），得到读 %d 存 %d", h.scenes.reads, h.scenes.saves)
	}
	if out.Notice != Notice || len(out.Changes.Added) == 0 || out.Version != 6 {
		t.Fatalf("结果不对：%+v", out)
	}
	if h.calls.inserted[0].Source != "agent" {
		t.Errorf("调用记录来源应为 agent，得到 %s", h.calls.inserted[0].Source)
	}
	// 再冲突就把冲突返回。
	h2 := newHarness(t)
	h2.scenes.conflicts = 2
	_, err = h2.svc.BuildAndApply(context.Background(), in)
	if codeOf(t, err) != errcode.VersionConflict {
		t.Fatalf("重做一次仍冲突应返回 40902，得到 %v", err)
	}
	if fin := h2.calls.finished[h2.calls.inserted[0].ID]; fin.ErrorCode != callErrSceneConflict {
		t.Fatalf("调用记录应记 scene_conflict，得到 %+v", fin)
	}
	// 网页身份不能走直接写入。
	if _, err := h2.svc.BuildAndApply(context.Background(), webInput()); codeOf(t, err) != errcode.InvalidArgument {
		t.Fatalf("网页身份调 BuildAndApply 应拒绝，得到 %v", err)
	}
}

// 免费：这个包不依赖账本（DRK-4）。依赖了的话，迟早有人在这里「顺手」加一个预扣。
func TestDirectorApp_不依赖账本(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list 失败：%v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "/internal/billing") {
			t.Fatalf("director/app 不能依赖账本，却依赖了 %s", dep)
		}
	}
}
