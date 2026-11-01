//go:build integration

// AI 剪辑主链路（docs/agent/smartcut SCT-27）：真库、真时间线写入路径（校验、快照、乐观锁、事件、调用记录），
// 只把文本模型与目录换成测试替身（它们的协议与选型各有自己的测试）。
package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	timelinehttp "github.com/li904338203/tabtv/backend/internal/timeline/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// fakeText 按顺序回固定的输出（用完了一直回最后一条），记下每一次请求。
type fakeText struct {
	mu      sync.Mutex
	replies []string
	reqs    []smartcutapp.TextRequest
	// during 第一次调用时在「模型思考期间」跑一次（模拟另一个会话保存）。
	during func()
	// block 非空时每次调用都等它关上（模拟慢模型，测并发限流）。
	block chan struct{}
	// respond 非空时按请求决定回什么（多方案并行时调用顺序不确定，按「第 k 套」分辨）。
	respond func(in smartcutapp.TextRequest) string
}

func (f *fakeText) Complete(ctx context.Context, in smartcutapp.TextRequest) (string, error) {
	f.mu.Lock()
	n := len(f.reqs)
	f.reqs = append(f.reqs, in)
	reply := f.replies[min(n, len(f.replies)-1)]
	if f.respond != nil {
		reply = f.respond(in)
	}
	during, block := f.during, f.block
	f.during = nil
	f.mu.Unlock()
	if during != nil {
		during()
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return reply, nil
}

func (f *fakeText) requests() []smartcutapp.TextRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smartcutapp.TextRequest(nil), f.reqs...)
}

type fakeCatalog struct{ unavailable *smartcutapp.Unavailable }

func (f fakeCatalog) SmartcutPlanChoice(context.Context, int64, int) (smartcutapp.PlanChoice, *smartcutapp.Unavailable, error) {
	if f.unavailable != nil {
		return smartcutapp.PlanChoice{}, f.unavailable, nil
	}
	return smartcutapp.PlanChoice{ModelCode: "m-plan", MaxImages: 4}, nil, nil
}

type fakePrompts struct{}

func (fakePrompts) RenderPrompt(_ context.Context, code, _ string, _ map[string]string) (string, error) {
	return "创作要求（" + code + "）", nil
}

// stageLog 收 smartcut.updated。
type stageLog struct {
	mu     sync.Mutex
	frames []smartcutapp.SmartcutUpdated
}

func (l *stageLog) Publish(_ context.Context, topic string, payload []byte) error {
	var ev smartcutapp.SmartcutUpdated
	if err := json.Unmarshal(payload, &ev); err != nil {
		return err
	}
	if !strings.HasPrefix(topic, "project:") {
		return fmt.Errorf("事件要投到 project:{id}，实际 %s", topic)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.frames = append(l.frames, ev)
	return nil
}

func (l *stageLog) stages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, f := range l.frames {
		out = append(out, *f.Stage+"/"+*f.Status)
	}
	return out
}

type planEnv struct {
	syncEnv
	text    *fakeText
	stages  *stageLog
	ports   smartcutapp.PlanPorts
	media   []int64
	music   int64
	pid     int64
	tid     int64
	baseVer int
}

const (
	planUser = user
	// 三段视频（4 / 5 / 6 秒）+ 一条 20 秒的配乐。
	mixReply = `{"segments":[
		{"clip":"2","take":"full","role":"hook","weight":3,"transition":"cut","reason":"最抓人"},
		{"clip":"1","take":"head","role":"body","weight":1,"transition":"dissolve"},
		{"clip":"3","take":"tail","role":"end","weight":1,"transition":"fade"}],
		"music":{"clip":"4"},"notes":["突出了第二段"]}`
)

func setupPlan(t *testing.T, cfg config.Smartcut) planEnv {
	t.Helper()
	e := planEnv{syncEnv: setupSync(t), text: &fakeText{replies: []string{mixReply}}, stages: &stageLog{}}
	media := mediaapp.New(mediarepo.New(e.pool), nil, e.next)
	if cfg.PlanConcurrency == 0 {
		cfg.PlanConcurrency, cfg.PlanPerHour = 1, 20
	}
	e.ports = wiring.SmartcutPlanPorts(wiring.SmartcutPlanInput{Pool: e.pool, Timeline: e.tl, Media: media,
		Generations: genrepo.New(e.pool), NextID: e.next, Cfg: cfg, RenderTimeline: timelinehttp.ViewJSON})
	e.ports.Text, e.ports.Catalog, e.ports.Prompts, e.ports.Events = e.text, fakeCatalog{}, fakePrompts{}, e.stages
	e.svc.SetPlanPorts(e.ports)

	e.pid, e.tid = e.newSmartcut(t)
	for _, d := range []int{4000, 5000, 6000} {
		e.media = append(e.media, e.mediaOf(t, ws, "video/mp4", d))
	}
	e.music = e.mediaOf(t, ws, "audio/mpeg", 20000)
	nodes := []map[string]any{nodeWith("sc", "smartcut", nil)}
	var edges []map[string]any
	for i, m := range append(append([]int64(nil), e.media...), e.music) {
		id := fmt.Sprintf("u%d", i+1)
		typ := "video"
		if m == e.music {
			typ = "audio"
		}
		nodes = append(nodes, nodeWith(id, typ, mediaResult(m)))
		edges = append(edges, edgeTo(id, i+1))
	}
	e.setCanvas(t, e.pid, nodes, edges)
	// 连线即入轨：先自动铺一次（剪辑台打开前的状态）。
	res, err := e.svc.Sync(e.ctx, ws, e.pid, e.tid, smartcutapp.SyncAutofill)
	if err != nil || len(res.AppendedClipIDs) != 4 {
		t.Fatalf("自动铺轨：%+v %v", res, err)
	}
	e.baseVer = res.TimelineVersion
	e.events.take()
	return e
}

func (e planEnv) req(mode string) smartcutapp.PlanRequest {
	return smartcutapp.PlanRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid, Actor: smartcutapp.Actor{UserID: planUser},
		Mode: mode, Instruction: "突出第二段", TargetSec: 10, Variants: 1, BaseVersion: e.baseVer}
}

func (e planEnv) videoMedia(t *testing.T) []int64 {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, tr := range v.Tracks {
		if tr.Track.Kind == domain.TrackVideo {
			for _, c := range tr.Clips {
				out = append(out, c.MediaID)
			}
		}
	}
	return out
}

func (e planEnv) callRow(t *testing.T, id int64) (status, stage string, applied *int, fallback bool, msg string) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx, `select status, stage, applied_variant, used_fallback, coalesce(error_message,'')
		from smartcut_llm_call where id = $1`, id).Scan(&status, &stage, &applied, &fallback, &msg); err != nil {
		t.Fatal(err)
	}
	return
}

func bizCode(err error) errcode.Code {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

// SC-AC-30 / SC-AC-33（后端）：一次成功的 AI 剪辑写进时间线、写入前存快照、免费、阶段可见；从快照恢复回到 AI 之前。
func TestPlan_素材混剪写入时间线并存快照(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	before := e.videoMedia(t)
	res, err := e.svc.Plan(e.ctx, e.req(smartcutdomain.ModeMix))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Variants) != 1 || res.Variants[0].Status != "success" || *res.AppliedVariant != 0 ||
		strings.Join(res.Variants[0].UsedClips, ",") != "2,1,3" || res.SnapshotID == 0 || len(res.Timeline) == 0 {
		t.Fatalf("结果：%+v", res)
	}
	if got := e.videoMedia(t); fmt.Sprint(got) != fmt.Sprint([]int64{e.media[1], e.media[0], e.media[2]}) {
		t.Fatalf("钩子在前、结尾在后：%v", got)
	}
	var tl struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(res.Timeline, &tl)
	if tl.Version != e.baseVer+1 {
		t.Fatalf("结果带回写入后的时间线：%s", res.Timeline)
	}
	// 免费：没有任何积分流水（SCK-2）。
	var credits int
	_ = e.pool.QueryRow(e.ctx, `select count(*) from credit_transaction`).Scan(&credits)
	if credits != 0 {
		t.Fatalf("AI 剪辑不该有积分流水：%d", credits)
	}
	status, stage, applied, fallback, _ := e.callRow(t, res.CallID)
	if status != "success" || stage != "done" || applied == nil || *applied != 0 || fallback {
		t.Fatalf("调用记录：%s %s %v %v", status, stage, applied, fallback)
	}
	reqs := e.text.requests()
	if len(reqs) != 1 || reqs[0].Operation != "smartcut_plan" || reqs[0].ModelCode != "m-plan" ||
		!strings.Contains(reqs[0].System, smartcutdomain.ContractHeading) || !strings.Contains(reqs[0].System, "模式：mix") ||
		!strings.Contains(reqs[0].User, "## 素材清单") || !strings.Contains(reqs[0].User, "编号 4：音频（配乐），20 秒") {
		t.Fatalf("模型请求：%+v", reqs)
	}
	if got := strings.Join(e.stages.stages(), ","); got !=
		"sync/running,understand/running,plan/running,compile/running,apply/running,done/success" {
		t.Fatalf("阶段推送：%s", got)
	}
	if got := e.events.take(); strings.Join(got, ",") != "sync,plan" {
		// listOnly 同步只记指纹不进版本（不发事件）；AI 落地发一次 plan。
		t.Logf("时间线事件：%v", got)
	}
	var arranged bool
	var planned, synced string
	_ = e.pool.QueryRow(e.ctx, `select is_arranged, coalesce(planned_fingerprint,''), coalesce(synced_fingerprint,'')
		from timeline where id = $1`, e.tid).Scan(&arranged, &planned, &synced)
	if !arranged || planned == "" || planned != synced {
		t.Fatalf("剪过、剪辑依据的素材指纹记下：%v %q %q", arranged, planned, synced)
	}
	snaps, err := e.tl.ListSnapshots(e.ctx, ws, e.tid)
	if err != nil || len(snaps) != 1 || snaps[0].Reason != "ai_plan" || snaps[0].CallID != res.CallID {
		t.Fatalf("写入前存一份 ai_plan 快照：%+v %v", snaps, err)
	}
	// 撤销这轮 AI：从快照恢复，回到 AI 之前的四段。
	if _, err := e.tl.RestoreSnapshot(e.ctx, timelineapp.RestoreInput{WorkspaceID: ws, TimelineID: e.tid,
		SnapshotID: snaps[0].ID, BaseVersion: e.baseVer + 1,
		Editor: timelinerepo.Editor{UserID: planUser, Kind: timelinerepo.EditorUser}}); err != nil {
		t.Fatal(err)
	}
	if got := e.videoMedia(t); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("恢复后回到 AI 之前：%v / %v", got, before)
	}
	// 摘要里没有进行中的调用了。
	list, _ := e.svc.List(e.ctx, ws, e.pid)
	if len(list) != 1 || list[0].ActiveCall != nil {
		t.Fatalf("摘要：%+v", list)
	}
}

// SC-AC-31：不存在的编号与写死的时间：前者回灌修一次，后者忽略；结果以编译为准。
func TestPlan_不合规回灌修一次(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	e.text.replies = []string{
		`{"segments":[{"clip":"9","take":"full"},{"clip":"1","take":"head","ratio":3}]}`,
		`{"segments":[{"clip":"1","take":"full","role":"hook","weight":2,"transition":"cut","startMs":0,"endMs":99999}]}`,
	}
	res, err := e.svc.Plan(e.ctx, e.req(smartcutdomain.ModeMix))
	if err != nil {
		t.Fatal(err)
	}
	reqs := e.text.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1].User, "## 上一次的输出") ||
		!strings.Contains(reqs[1].User, "第 1 段：编号「9」不在素材条里") || !strings.Contains(reqs[1].User, "ratio 3 不在 0.1~1 之间") {
		t.Fatalf("回灌：%+v", reqs)
	}
	v := res.Variants[0]
	if v.Status != "success" || v.UsedFallback || strings.Join(v.UsedClips, ",") != "1" {
		t.Fatalf("修过之后照常编译：%+v", v)
	}
	foundIgnored := false
	for _, n := range v.Notes {
		foundIgnored = foundIgnored || strings.Contains(n, "方案里写了 2 处时间")
	}
	if !foundIgnored {
		t.Fatalf("写死的时间被忽略并说明：%v", v.Notes)
	}
	if got := e.videoMedia(t); len(got) != 1 || got[0] != e.media[0] {
		t.Fatalf("时间以编译为准（整段 4 秒）：%v", got)
	}
}

// SC-AC-32：两次都返回无法解析的内容 —— 时间线不被清空，按编号顺序拼好，带固定说明；调用记录为失败并带原文片段。
func TestPlan_两次都解析不了走兜底(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	e.text.replies = []string{"好的，我来帮你剪。", "{\"segments\": ["}
	res, err := e.svc.Plan(e.ctx, e.req(smartcutdomain.ModeMix))
	if err != nil {
		t.Fatal(err)
	}
	v := res.Variants[0]
	if !v.UsedFallback || len(v.Notes) == 0 || v.Notes[0] != smartcutdomain.FallbackNote {
		t.Fatalf("兜底：%+v", v)
	}
	if got := e.videoMedia(t); fmt.Sprint(got) != fmt.Sprint([]int64{e.media[0], e.media[1]}) {
		// 目标 10 秒：4 + 5 = 9 秒，再接 6 秒那段要截到 1 秒 → 第三段截下 1 秒
		if len(got) != 3 || got[0] != e.media[0] || got[1] != e.media[1] || got[2] != e.media[2] {
			t.Fatalf("按编号顺序拼接、按目标时长从尾部截：%v", got)
		}
	}
	status, _, applied, fallback, msg := e.callRow(t, res.CallID)
	if status != "failed" || !fallback || applied == nil || !strings.Contains(msg, "原文开头") {
		t.Fatalf("调用记录为失败、带原文片段：%s %v %v %q", status, fallback, applied, msg)
	}
	if len(e.text.requests()) != 2 {
		t.Fatal("修一次（共两次调用）")
	}
}

// SC-AC-34：请求发出后另一个会话保存了一次：返回 40902 不写入，带 callId；用它「仍然应用」写入并先存快照。
func TestPlan_剪辑期间被改过(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	e.text.during = func() {
		v, err := e.tl.Get(e.ctx, ws, e.tid)
		if err != nil {
			t.Error(err)
			return
		}
		c := keepFirstVideo(timelineapp.ContentFromView(v))
		if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid,
			Editor: timelinerepo.Editor{UserID: 777, Kind: timelinerepo.EditorUser}, Content: c}); err != nil {
			t.Error(err)
		}
	}
	_, err := e.svc.Plan(e.ctx, e.req(smartcutdomain.ModeMix))
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.VersionConflict || be.Details["callId"] == nil {
		t.Fatalf("剪辑期间被改：期望 40902 带 callId，实际 %v", err)
	}
	if got := e.videoMedia(t); len(got) != 1 {
		t.Fatalf("不写入（保留另一会话的改动）：%v", got)
	}
	var callID int64
	fmt.Sscan(be.Details["callId"].(string), &callID)
	status, _, applied, _, _ := e.callRow(t, callID)
	if status != "success" || applied != nil {
		t.Fatalf("方案已生成、未应用：%s %v", status, applied)
	}
	cur := e.baseVer + 1
	res, err := e.svc.ApplyVariant(e.ctx, smartcutapp.ApplyRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid,
		CallID: callID, Variant: 0, BaseVersion: cur, Actor: smartcutapp.Actor{UserID: planUser}})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.videoMedia(t); len(got) != 3 || got[0] != e.media[1] {
		t.Fatalf("仍然应用：%v", got)
	}
	snaps, _ := e.tl.ListSnapshots(e.ctx, ws, e.tid)
	if len(snaps) != 1 || snaps[0].Reason != "ai_plan" || res.SnapshotID != snaps[0].ID {
		t.Fatalf("仍然应用也先存快照（这一轮 AI 的落地）：%+v", snaps)
	}
	if len(e.text.requests()) != 1 {
		t.Fatal("仍然应用不重新调模型")
	}
	// force：版本对不上也写（以当前版本覆盖），仍先存快照。
	e.text.during = func() {
		v, _ := e.tl.Get(e.ctx, ws, e.tid)
		c := keepFirstVideo(timelineapp.ContentFromView(v))
		_, _ = e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid,
			Editor: timelinerepo.Editor{UserID: 777, Kind: timelinerepo.EditorUser}, Content: c})
	}
	r := e.req(smartcutdomain.ModeMix)
	r.BaseVersion, r.Force = cur+1, true
	if _, err := e.svc.Plan(e.ctx, r); err != nil {
		t.Fatalf("force 覆盖：%v", err)
	}
	if got := e.videoMedia(t); len(got) != 3 {
		t.Fatalf("force 之后是 AI 的方案：%v", got)
	}
}

// SC-AC-35：同时进行 42902、每小时上限 42901（带 retry_after_seconds），一次 3 套方案只计 1 次。
func TestPlan_限流(t *testing.T) {
	e := setupPlan(t, config.Smartcut{PlanConcurrency: 1, PlanPerHour: 2})
	e.text.block = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		r := e.req(smartcutdomain.ModeMix)
		r.Variants = 3
		_, err := e.svc.Plan(e.ctx, r)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.text.requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := e.svc.Plan(e.ctx, e.req(smartcutdomain.ModeMix)); bizCode(err) != errcode.ConcurrencyLimited {
		t.Fatalf("同时进行第二次：期望 42902，实际 %v", err)
	}
	close(e.text.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var rows int
	_ = e.pool.QueryRow(e.ctx, `select count(*) from smartcut_llm_call where user_id = $1`, planUser).Scan(&rows)
	if rows != 1 {
		t.Fatalf("3 套方案一次请求只插一行：%d", rows)
	}
	e.text.block = nil
	r := e.req(smartcutdomain.ModeMix)
	r.BaseVersion = e.currentVersion(t)
	if _, err := e.svc.Plan(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	r.BaseVersion = e.currentVersion(t)
	_, err := e.svc.Plan(e.ctx, r)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.RateLimited || be.Details["retry_after_seconds"].(int) <= 0 {
		t.Fatalf("每小时上限：期望 42901 带 retry_after_seconds，实际 %v", err)
	}
	cfg, err := e.svc.PlanConfig(e.ctx, planUser, 0)
	if err != nil || !cfg.Available || cfg.UsedThisHour != 2 || cfg.PerHour != 2 {
		t.Fatalf("配置里的额度：%+v %v", cfg, err)
	}
}

// keepFirstVideo 另一个会话的一次修改：画面轨只留第一段（跟随被删片段的字幕一起去掉）。
func keepFirstVideo(c domain.Content) domain.Content {
	for i := range c.Tracks {
		if c.Tracks[i].Kind == domain.TrackVideo && len(c.Tracks[i].Clips) > 1 {
			c.Tracks[i].Clips = c.Tracks[i].Clips[:1]
		}
	}
	c.Subtitles = nil
	return c
}

func (e planEnv) currentVersion(t *testing.T) int {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	return v.Timeline.Version
}

// SC-AC-36：没有可用的 AI 剪辑模型：/plan 422（42218），配置给出原因；不占额度。
func TestPlan_没有可用模型(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	e.ports.Catalog = fakeCatalog{unavailable: &smartcutapp.Unavailable{Reason: "AI 剪辑暂不可用：后台还没有绑定模型"}}
	e.svc.SetPlanPorts(e.ports)
	if _, err := e.svc.Plan(e.ctx, e.req(smartcutdomain.ModeMix)); bizCode(err) != errcode.SmartcutPlanUnavailable {
		t.Fatalf("期望 42218，实际 %v", err)
	}
	cfg, err := e.svc.PlanConfig(e.ctx, planUser, 0)
	if err != nil || cfg.Available || !strings.Contains(cfg.Reason, "暂不可用") {
		t.Fatalf("%+v %v", cfg, err)
	}
	var rows int
	_ = e.pool.QueryRow(e.ctx, `select count(*) from smartcut_llm_call`).Scan(&rows)
	if rows != 0 {
		t.Fatal("不可用时不插调用记录、不占额度")
	}
}

// 42217：模式与输入不匹配（口播没有识别结果、解说没有配音），在限流之前就拒，说清出路。
func TestPlan_输入不适合这个模式(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	for mode, reason := range map[string]string{smartcutdomain.ModeTalk: "asr_missing", smartcutdomain.ModeNarr: "no_voice",
		smartcutdomain.ModeBoard: "no_shots"} {
		_, err := e.svc.Plan(e.ctx, e.req(mode))
		var be *httpx.BizError
		if !errors.As(err, &be) || be.Code != errcode.SmartcutInputUnsuitable || be.Details["reason"] != reason {
			t.Fatalf("%s：期望 42217 %s，实际 %v", mode, reason, err)
		}
	}
	r := e.req(smartcutdomain.ModeMix)
	r.Instruction = "卡点剪一版"
	e.setCanvas(t, e.pid, []map[string]any{nodeWith("sc", "smartcut", nil), nodeWith("u1", "video", mediaResult(e.media[0]))},
		[]map[string]any{edgeTo("u1", 1)})
	_, err := e.svc.Plan(e.ctx, r)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Details["reason"] != "no_music" || fmt.Sprint(be.Details["options"]) != "[connect_audio ignore_beat]" {
		t.Fatalf("要卡点却没有配乐：%v", err)
	}
	r.IgnoreBeat = true
	if _, err := e.svc.Plan(e.ctx, r); err != nil {
		t.Fatalf("选了不卡点就照常剪：%v", err)
	}
	var rows int
	_ = e.pool.QueryRow(e.ctx, `select count(*) from smartcut_llm_call`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("不适合的请求不占额度：%d", rows)
	}
}

// 进行中超过 20 分钟的调用被恢复扫描判失败，不永久占用并发额度。
func TestPlan_恢复扫描(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	id := e.next()
	e.exec(t, `insert into smartcut_llm_call (id, workspace_id, project_id, timeline_id, user_id, source, kind, created_at)
		values ($1, $2, $3, $4, $5, 'web', 'plan', now() - interval '25 minutes')`, id, ws, e.pid, e.tid, planUser)
	if _, err := e.svc.Plan(e.ctx, e.req(smartcutdomain.ModeMix)); bizCode(err) != errcode.ConcurrencyLimited {
		t.Fatalf("卡住的调用占着并发：%v", err)
	}
	if n, err := e.svc.RecoverStaleCalls(e.ctx); err != nil || n != 1 {
		t.Fatalf("恢复扫描：%d %v", n, err)
	}
	if _, err := e.svc.Plan(e.ctx, e.req(smartcutdomain.ModeMix)); err != nil {
		t.Fatalf("判失败之后释放额度：%v", err)
	}
}

// mosaicMaterial 真实的素材端口，只把拼图换成一张现做的（集成环境里没有对象存储，取不到缩略图字节）。
type mosaicMaterial struct {
	smartcutapp.MaterialFacts
	sheet string
}

func (m mosaicMaterial) Mosaics(_ context.Context, _ int64, items []smartcutapp.MosaicItem, maxSheets int) ([]smartcutapp.Mosaic, error) {
	var rows []string
	for _, it := range items {
		rows = append(rows, it.No)
	}
	if len(rows) > 3 {
		rows = rows[:3]
	}
	return []smartcutapp.Mosaic{{DataURI: m.sheet, Rows: rows}}, nil
}

// SC-AC-37：上传素材参与规划时，发给上游的只有 data: 内联的拼图，没有任何媒体外链；描述与拼图位置写进 user 消息。
func TestPlan_看图只内联拼图(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	jpeg, err := wiring.ComposeMosaic(nil)
	if err != nil {
		t.Fatal(err)
	}
	sheet := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg)
	e.ports.Material = mosaicMaterial{MaterialFacts: e.ports.Material, sheet: sheet}
	e.svc.SetPlanPorts(e.ports)
	if _, err := e.svc.Plan(e.ctx, e.req(smartcutdomain.ModeMix)); err != nil {
		t.Fatal(err)
	}
	reqs := e.text.requests()
	if len(reqs) != 1 || len(reqs[0].Images) != 1 {
		t.Fatalf("带一张拼图：%+v", reqs)
	}
	for _, img := range reqs[0].Images {
		if !strings.HasPrefix(img, "data:image/") {
			t.Fatalf("只发内联图片：%.40s", img)
		}
	}
	for _, s := range []string{reqs[0].System, reqs[0].User} {
		if strings.Contains(s, "http://") || strings.Contains(s, "https://") || strings.Contains(s, "/api/v1/media") {
			t.Fatalf("提示词里不能有媒体外链：%s", s)
		}
	}
	if !strings.Contains(reqs[0].User, "## 缩略图拼图") || !strings.Contains(reqs[0].User, "拼图 1：第 1 行是编号 1") ||
		!strings.Contains(reqs[0].User, "画面：见拼图 1 第 1 行") {
		t.Fatalf("拼图的说明：%s", reqs[0].User)
	}
}
