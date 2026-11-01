//go:build integration

// 片段级生成（docs/agent/smartcut SCT-37，SC-AC-54 / 55 / 58 / 59）：真实装配的受理（目录报价、账本扣费、生成域）+
// 真库回填（整份保存、快照、乐观锁、备选）。上游执行由生成域自己的集成测试覆盖，这里直接把终态交给回写。
package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

type clipEnv struct {
	speechEnv
	videoModel  int64
	text2video  int64
	img2video   int64
	source      int64 // 模板：产出画面第一段素材的那次生成
	sourceMedia int64
	pid, tid    int64
}

// setupClip 一个视频模型（文生 / 图生视频，按秒 3 积分）、一次成功的文生视频（产出 6 秒的素材）、
// 一条智能剪辑时间线：第一段是那份生成出来的素材，第二段是上传的 3 秒素材。
func setupClip(t *testing.T) clipEnv {
	t.Helper()
	e := clipEnv{speechEnv: setupSpeech(t)}
	e.svc.SetClipToolPorts(wiring.SmartcutClipPorts(wiring.SmartcutClipInput{Gen: e.gen, GenRepo: genrepo.New(e.pool),
		Media: e.media, Timeline: e.tl}))
	pvd := e.next()
	e.videoModel, e.text2video, e.img2video = e.next(), e.next(), e.next()
	e.exec(t, `insert into api_protocol (code, name, is_async) values ('generic_sync','通用同步',false) on conflict (code) do nothing`)
	e.exec(t, `insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
		values ($1,'p-vid','视频上游','http','https://video.example.com','none','enabled')`, pvd)
	e.exec(t, `insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol, submit_path,
		est_seconds, status) values ($1,'vid-m','测试视频',$2,'video','vid','generic_sync','/v1/video',60,'enabled')`,
		e.videoModel, pvd)
	e.exec(t, `insert into operation (id, code, name, domain, in_node_types, out_node_type, status) values
		($1,'text2video','文生视频','video','text','video','enabled'), ($2,'img2video','图生视频','video','text,image','video','enabled')`,
		e.text2video, e.img2video)
	for _, op := range []int64{e.text2video, e.img2video} {
		e.exec(t, `insert into model_operation (id, model_id, operation_id, is_enabled) values ($1,$2,$3,true)`, e.next(), e.videoModel, op)
		e.exec(t, `insert into pricing (id, operation_id, model_id, credits, unit, status) values ($1,$2,$3,3,'per_second','enabled')`,
			e.next(), op, e.videoModel)
	}
	// 能力矩阵必配（每个模型 × 操作都要声明收哪些输入）：文生视频只收文字，图生视频收一张图加文字。
	e.exec(t, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		values ($1,$2,$3,'image',1,1), ($4,$2,$3,'text',0,9), ($5,$2,$6,'text',0,9)`,
		e.next(), e.videoModel, e.img2video, e.next(), e.next(), e.text2video)

	e.sourceMedia = e.mediaOf(t, ws, "video/mp4", 6000)
	upload := e.mediaOf(t, ws, "video/mp4", 3000)
	e.pid, e.tid = e.timelineWith(t, e.sourceMedia, upload)
	e.source = e.next()
	e.exec(t, `insert into generation (id, workspace_id, payer_account_id, created_by_user_id, project_id, node_id,
		operation_id, model_id, result_type, idempotency_key, status, params_snapshot, inputs_snapshot)
		values ($1,$2,$3,$4,$5,'v1',$6,$7,'video',$8,'success',
		        '{"prompt":"原来的提示词","duration":5,"_snapshot":{"modelCode":"vid-m","operationCode":"text2video"}}','[]')`,
		e.source, ws, speechPayer, user, e.pid, e.text2video, e.videoModel, fmt.Sprintf("src-%d", e.source))
	e.exec(t, `insert into generation_output (id, generation_id, idx, media_object_id, output_type) values ($1,$2,0,$3,'video')`,
		e.next(), e.source, e.sourceMedia)
	return e
}

func (e clipEnv) clipReq(key string, dryRun bool) smartcutapp.ClipToolRequest {
	return smartcutapp.ClipToolRequest{WorkspaceID: ws, PayerAccountID: speechPayer, ProjectID: e.pid, TimelineID: e.tid,
		UserID: user, DryRun: dryRun, IdempotencyKey: key}
}

func (e clipEnv) clips(t *testing.T) []domain.Clip {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	return clipsOfView(v, domain.TrackVideo)
}

func (e clipEnv) target(t *testing.T, genID int64) json.RawMessage {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(e.ctx, `select smartcut_target from generation where id = $1`, genID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// finish 把一次片段级生成的终态交给回写（成功时带一份新产物）—— 先照执行器的样子把终态与产物落库（产物要能反查到这次生成）。
func (e clipEnv) finish(t *testing.T, genID int64, status string, media ...int64) {
	t.Helper()
	e.exec(t, `update generation set status = $2 where id = $1`, genID, status)
	for i, m := range media {
		e.exec(t, `insert into generation_output (id, generation_id, idx, media_object_id, output_type) values ($1,$2,$3,$4,'video')`,
			e.next(), genID, i, m)
	}
	if err := e.svc.BindSmartcut(e.ctx, smartcutapp.BindInput{GenerationID: genID, WorkspaceID: ws, ProjectID: e.pid,
		Target: e.target(t, genID), Status: status, MediaIDs: media}); err != nil {
		t.Fatal(err)
	}
}

// SC-AC-54：改提示词重新生成 —— 按原模型报价并扣费；完成后这一段换成新素材、原素材进备选；
// 上游节点的产物与参数不变；画布上没有多出节点；任务不挂节点、来源 smartcut。
func TestRegenerate_报价受理替换并保留原素材进备选(t *testing.T) {
	e := setupClip(t)
	first := e.clips(t)[0]
	canvasBefore := e.count(t, `select jsonb_array_length(canvas_data->'nodes') from project where id = $1`, e.pid)

	q, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("", true), ClipID: first.ID,
		Prompt: "换成黄昏的海边"})
	if err != nil || q.Quote == nil || q.Quote.Credits != 15 || q.Quote.Blocker != nil {
		t.Fatalf("按原模型、原时长报价（5 秒 × 3）：%+v %v", q.Quote, err)
	}
	before := e.balance(t)
	res, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("regen-ac54-1", false),
		ClipID: first.ID, Prompt: "换成黄昏的海边"})
	if err != nil || res.Accepted == nil {
		t.Fatalf("受理：%+v %v", res, err)
	}
	gid := res.Accepted.GenerationID
	if before-e.balance(t) != 15 || res.Accepted.Kind != "regen" || res.Accepted.ClipID != fmt.Sprint(first.ID) {
		t.Fatalf("扣 15：%d %+v", before-e.balance(t), res.Accepted)
	}
	var node *string
	var prompt, origin, model string
	if err := e.pool.QueryRow(e.ctx, `select node_id, params_snapshot->>'prompt', params_snapshot->'_snapshot'->>'origin',
		params_snapshot->'_snapshot'->>'modelCode' from generation where id = $1`, gid).Scan(&node, &prompt, &origin, &model); err != nil {
		t.Fatal(err)
	}
	if node != nil || prompt != "换成黄昏的海边" || origin != "smartcut" || model != "vid-m" {
		t.Fatalf("不挂节点、换了提示词、沿用原模型：%v %q %q %q", node, prompt, origin, model)
	}
	if p, _ := e.tl.ClipPending(e.ctx, ws, e.tid, first.ID); p != gid {
		t.Fatalf("这一段标成生成中：%d", p)
	}

	out := e.mediaOf(t, ws, "video/mp4", 4000)
	versionBefore := e.subtitleView(t, e.tid).Timeline.Version
	e.finish(t, gid, "success", out)
	after := e.clips(t)[0]
	if after.ID != first.ID || after.MediaID != out || after.SourceInMs != 0 || after.SourceOutMs != 4000 {
		t.Fatalf("换成新素材、从头取 min(4 秒, 原 6 秒)：%+v", after)
	}
	if len(after.Alternates) != 1 || after.Alternates[0] != (domain.Alternate{MediaID: e.sourceMedia, SourceInMs: 0, SourceOutMs: 6000,
		Kind: domain.AlternateOriginal, GenerationID: e.source}) {
		t.Fatalf("原素材连同原区间进备选：%+v", after.Alternates)
	}
	if after.PendingGenerationID != 0 {
		t.Fatal("回填后清掉生成中")
	}
	view := e.subtitleView(t, e.tid)
	if view.Timeline.Version != versionBefore+1 || view.Timeline.LastEditorKind != "system" {
		t.Fatalf("回填是一次写入、编辑者 system：v%d → v%d %q", versionBefore, view.Timeline.Version, view.Timeline.LastEditorKind)
	}
	if n := e.count(t, `select count(*) from timeline_snapshot where timeline_id = $1 and reason = 'clip_fill'`, e.tid); n != 1 {
		t.Fatalf("写入前存快照：%d", n)
	}
	if !strings.Contains(string(e.target(t, gid)), `"landed": true`) {
		t.Fatalf("记下已回填：%s", e.target(t, gid))
	}
	if n := e.count(t, `select jsonb_array_length(canvas_data->'nodes') from project where id = $1`, e.pid); n != canvasBefore {
		t.Fatalf("画布上没有多出节点：%d → %d", canvasBefore, n)
	}
	var srcPrompt string
	_ = e.pool.QueryRow(e.ctx, `select params_snapshot->>'prompt' from generation where id = $1`, e.source).Scan(&srcPrompt)
	if srcPrompt != "原来的提示词" || e.count(t, `select count(*) from generation_output where generation_id = $1`, e.source) != 1 {
		t.Fatal("上游那次生成的参数与产物不变")
	}
	if n := e.count(t, `select count(*) from work`); n != 0 {
		t.Fatalf("片段级生成不登记作品：%d", n)
	}

	// 再重拍一次：被换下来的是上一次重拍的产物，备选里记成 regen。
	res2, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("regen-ac54-2", false),
		ClipID: first.ID, Prompt: "再换成雪地"})
	if err != nil {
		t.Fatal(err)
	}
	e.finish(t, res2.Accepted.GenerationID, "success", e.mediaOf(t, ws, "video/mp4", 5000))
	alts := e.clips(t)[0].Alternates
	if len(alts) != 2 || alts[1].MediaID != out || alts[1].Kind != domain.AlternateRegen || alts[1].GenerationID != gid {
		t.Fatalf("上一次重拍的产物记 regen：%+v", alts)
	}
}

// 上传的素材没有生成信息：回 42217 并说清楚；不扣费、不建任务。
func TestRegenerate_上传素材没有生成信息(t *testing.T) {
	e := setupClip(t)
	second := e.clips(t)[1]
	_, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("regen-upload-1", false),
		ClipID: second.ID, Prompt: "随便"})
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.SmartcutInputUnsuitable || !strings.Contains(be.Message, "上传的素材") ||
		be.Details["reason"] != "not_regenerable" {
		t.Fatalf("42217：%v", err)
	}
	if n := e.count(t, `select count(*) from generation where smartcut_target is not null`); n != 0 {
		t.Fatal("不建任务")
	}
	o, err := e.svc.ClipOriginOf(e.ctx, ws, e.pid, e.tid, second.ID)
	if err != nil || o.Kind != "upload" || o.CanRegenerate || o.Reason == "" {
		t.Fatalf("生成信息：%+v %v", o, err)
	}
	o, err = e.svc.ClipOriginOf(e.ctx, ws, e.pid, e.tid, e.clips(t)[0].ID)
	if err != nil || o.Kind != "generation" || !o.CanRegenerate || o.Prompt != "原来的提示词" || o.ModelCode != "vid-m" ||
		o.Params["duration"] != float64(5) {
		t.Fatalf("生成出来的：%+v %v", o, err)
	}
}

// SC-AC-58：重拍失败 —— 片段保持原样、生成中清掉、按原批次退款（生成域负责）；再点一次是新任务（新的幂等键），
// 不会命中上一次的失败；同一次确认的重试（同一个键）回同一个任务。
func TestRegenerate_失败后再点是新任务(t *testing.T) {
	e := setupClip(t)
	first := e.clips(t)[0]
	a, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("regen-fail-a", false),
		ClipID: first.ID, Prompt: "第一次"})
	if err != nil {
		t.Fatal(err)
	}
	// 生成中再点一次：同一段同时只能有一次。
	if _, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("regen-fail-busy", false),
		ClipID: first.ID, Prompt: "插队"}); bizCode(err) != errcode.InvalidStateTransition {
		t.Fatalf("生成中不能再发起：%v", err)
	}
	e.finish(t, a.Accepted.GenerationID, "failed")
	if c := e.clips(t)[0]; c.MediaID != first.MediaID || c.PendingGenerationID != 0 || len(c.Alternates) != 0 {
		t.Fatalf("失败时片段保持原样：%+v", c)
	}
	b, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("regen-fail-b", false),
		ClipID: first.ID, Prompt: "第二次"})
	if err != nil || b.Accepted.GenerationID == a.Accepted.GenerationID {
		t.Fatalf("新任务：%+v %v", b.Accepted, err)
	}
	e.exec(t, `update timeline_clip set pending_generation_id = null where id = $1`, first.ID)
	replay, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("regen-fail-b", false),
		ClipID: first.ID, Prompt: "第二次"})
	if err != nil || replay.Accepted.GenerationID != b.Accepted.GenerationID {
		t.Fatalf("同一个键回同一个任务：%+v %v", replay.Accepted, err)
	}
}

// SC-AC-55：补镜插在发起时记下的位置之后；图生视频用相邻那段的末帧作参考图；期间那一段被删时产物只进生成记录。
func TestFill_插在记下的位置与相邻末帧作参考(t *testing.T) {
	e := setupClip(t)
	cs := e.clips(t)
	// 只有封面帧（第一帧）时不能拿它补镜：用第一帧去接，补出来的镜头会从上一段的开头重新开始。
	cover := e.next()
	e.exec(t, `insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes, purpose, created_by_user_id,
		source_media_id, derive_kind, derive_index) values ($1,$2,'b',$3,'image/jpeg',10,'generation_output',$4,$5,'video_poster',0)`,
		cover, ws, fmt.Sprintf("k/%d", cover), user, cs[0].MediaID)
	if _, err := e.svc.FillGap(e.ctx, smartcutapp.FillRequest{ClipToolRequest: e.clipReq("fill-ac55-0", false),
		AfterClipID: cs[0].ID, Prompt: "过渡镜头", ModelCode: "vid-m", Operation: "img2video", DurationSec: 4,
		RefFromClipID: cs[0].ID}); err == nil || !strings.Contains(err.Error(), "末帧还没准备好") {
		t.Fatalf("只有封面帧时应报末帧未就绪：%v", err)
	}
	poster := e.next()
	e.exec(t, `insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes, purpose, created_by_user_id,
		source_media_id, derive_kind, derive_index) values ($1,$2,'b',$3,'image/jpeg',10,'generation_output',$4,$5,'video_lastframe',0)`,
		poster, ws, fmt.Sprintf("k/%d", poster), user, cs[0].MediaID)
	res, err := e.svc.FillGap(e.ctx, smartcutapp.FillRequest{ClipToolRequest: e.clipReq("fill-ac55-1", false),
		AfterClipID: cs[0].ID, Prompt: "过渡镜头", ModelCode: "vid-m", Operation: "img2video", DurationSec: 4,
		RefFromClipID: cs[0].ID})
	if err != nil || res.Accepted == nil || res.Accepted.Kind != "fill" {
		t.Fatalf("受理：%+v %v", res, err)
	}
	var inputs string
	_ = e.pool.QueryRow(e.ctx, `select inputs_snapshot::text from generation where id = $1`, res.Accepted.GenerationID).Scan(&inputs)
	if !strings.Contains(inputs, fmt.Sprint(poster)) {
		t.Fatalf("参考图是第一段的末帧：%s", inputs)
	}
	e.finish(t, res.Accepted.GenerationID, "success", e.mediaOf(t, ws, "video/mp4", 4000))
	after := e.clips(t)
	if len(after) != 3 || after[0].ID != cs[0].ID || after[2].ID != cs[1].ID || after[1].SourceOutMs != 4000 ||
		after[1].Origin != domain.OriginUser {
		t.Fatalf("插在第一段之后：%+v", after)
	}

	// 第二次补在第二段之后，期间第二段被删：不自动插入，产物只进生成记录。
	res2, err := e.svc.FillGap(e.ctx, smartcutapp.FillRequest{ClipToolRequest: e.clipReq("fill-ac55-2", false),
		AfterClipID: cs[1].ID, Prompt: "结尾", ModelCode: "vid-m", Operation: "text2video"})
	if err != nil {
		t.Fatal(err)
	}
	c := timelineapp.ContentFromView(e.subtitleView(t, e.tid))
	for i := range c.Tracks {
		if c.Tracks[i].Kind == domain.TrackVideo {
			c.Tracks[i].Clips = c.Tracks[i].Clips[:2]
		}
	}
	if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid, Editor: editor,
		Content: c}); err != nil {
		t.Fatal(err)
	}
	version := e.subtitleView(t, e.tid).Timeline.Version
	e.finish(t, res2.Accepted.GenerationID, "success", e.mediaOf(t, ws, "video/mp4", 5000))
	if v := e.subtitleView(t, e.tid).Timeline.Version; v != version || len(e.clips(t)) != 2 {
		t.Fatalf("位置不在了：时间线不动（v%d → v%d）", version, v)
	}
	list, _, err := e.svc.Generations(e.ctx, ws, e.pid, e.tid, "", 10)
	if err != nil || len(list) != 2 || list[0].GenerationID != res2.Accepted.GenerationID || list[0].Landed ||
		list[1].GenerationID != res.Accepted.GenerationID || !list[1].Landed || list[0].Kind != "fill" {
		t.Fatalf("生成记录（新的在前）：%+v %v", list, err)
	}
}

// SC-AC-59：有一段还在重拍时，导出预检报「第 N 段还在生成」。
func TestPrecheck_生成中的片段拦下导出(t *testing.T) {
	e := setupClip(t)
	first := e.clips(t)[0]
	if _, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("regen-pre-1", false),
		ClipID: first.ID, Prompt: "重拍"}); err != nil {
		t.Fatal(err)
	}
	pre, _, _, err := e.tl.PrecheckRender(e.ctx, timelineapp.RenderInput{WorkspaceID: ws, PayerAccountID: speechPayer, UserID: user,
		TimelineID: e.tid, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range pre.Issues {
		if is.Code == domain.PrecheckClipGenerating && is.ClipIdx == 1 {
			found = true
		}
	}
	if !found || pre.Runnable() {
		t.Fatalf("第 1 段还在生成：%+v", pre.Issues)
	}
}

// 回填与用户的整份保存同时写同一条时间线：版本冲突时回填重读重放，最后产物落下去、用户那一次（若成功）也在。
// 预热连接池、跑多轮（顺序调用永远先撞上状态检查，测不出并发）。
func TestBind_与整份保存并发时重放成功(t *testing.T) {
	e := setupClip(t)
	var warm sync.WaitGroup
	for i := 0; i < 8; i++ {
		warm.Add(1)
		go func() { defer warm.Done(); _ = e.count(t, `select 1`) }()
	}
	warm.Wait()
	for round := 0; round < 5; round++ {
		first := e.clips(t)[0]
		res, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{
			ClipToolRequest: e.clipReq(fmt.Sprintf("regen-race-%d", round), false), ClipID: first.ID, Prompt: "并发"})
		if err != nil {
			t.Fatal(err)
		}
		out := e.mediaOf(t, ws, "video/mp4", 3000)
		base := timelineapp.ContentFromView(e.subtitleView(t, e.tid))
		vol := 40 + round
		for i := range base.Tracks {
			if base.Tracks[i].Kind == domain.TrackVideo {
				base.Tracks[i].Clips[1].Volume = &vol
			}
		}
		var wg sync.WaitGroup
		var saveErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			e.finish(t, res.Accepted.GenerationID, "success", out)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, saveErr = e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid, Editor: editor,
				Content: base})
		}()
		close(start)
		wg.Wait()
		after := e.clips(t)
		if after[0].MediaID != out {
			t.Fatalf("第 %d 轮：产物一定落下去：%+v", round, after[0])
		}
		if saveErr == nil && after[1].Volume != vol {
			t.Fatalf("第 %d 轮：用户那一次保存成功了，它的改动也要在：%d", round, after[1].Volume)
		}
		if saveErr != nil && bizCode(saveErr) != errcode.VersionConflict {
			t.Fatalf("第 %d 轮：用户的保存只能是成功或版本冲突：%v", round, saveErr)
		}
	}
}

// 重拍期间这一段被删了：不改时间线（版本不动），产物只进生成记录（landed=false）。
func TestBind_片段已删只进生成记录(t *testing.T) {
	e := setupClip(t)
	first := e.clips(t)[0]
	res, err := e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("regen-gone-1", false),
		ClipID: first.ID, Prompt: "重拍"})
	if err != nil {
		t.Fatal(err)
	}
	c := timelineapp.ContentFromView(e.subtitleView(t, e.tid))
	for i := range c.Tracks {
		if c.Tracks[i].Kind == domain.TrackVideo {
			c.Tracks[i].Clips = c.Tracks[i].Clips[1:]
		}
	}
	if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid, Editor: editor,
		Content: c}); err != nil {
		t.Fatal(err)
	}
	version := e.subtitleView(t, e.tid).Timeline.Version
	out := e.mediaOf(t, ws, "video/mp4", 4000)
	e.finish(t, res.Accepted.GenerationID, "success", out)
	if v := e.subtitleView(t, e.tid).Timeline.Version; v != version || len(e.clips(t)) != 1 {
		t.Fatalf("这一段不在了：时间线不动（v%d → v%d）", version, v)
	}
	list, _, err := e.svc.Generations(e.ctx, ws, e.pid, e.tid, "", 10)
	if err != nil || len(list) != 1 || list[0].Landed || list[0].OutputMediaID != out || list[0].Status != "success" ||
		list[0].Kind != "regen" {
		t.Fatalf("产物在生成记录里、没回填：%+v %v", list, err)
	}
}
