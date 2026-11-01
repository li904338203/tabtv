//go:build integration

// 语音识别（docs/agent/smartcut SCT-33，SC-AC-49~51）：真实装配的受理链路（画布解析输入、目录按秒报价、账本扣费）
// + 真实的执行器（假上游）+ 回写。只有上游是假的。
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

const speechPayer = int64(9201)

// fakeASR 是识别上游：按设定回一段文字与 meta，或失败；记下提交了几次。
type fakeASR struct {
	mu      sync.Mutex
	submits int
	text    string
	meta    string
	fail    bool
}

func (f *fakeASR) Submit(context.Context, genapp.Job) (genapp.SubmitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits++
	if f.fail {
		return genapp.SubmitResult{Outcome: gendomain.SubmitRejected, HTTPStatus: 400, Message: "火山识别拒绝了请求（45000151）：audio format error"}, nil
	}
	return genapp.SubmitResult{Outcome: gendomain.SubmitCompleted, Outputs: []genapp.ProviderOutput{
		{Type: "text", Text: f.text, Meta: json.RawMessage(f.meta)}}}, nil
}
func (f *fakeASR) Query(context.Context, genapp.Job, string) (genapp.QueryResult, error) {
	return genapp.QueryResult{}, nil
}
func (f *fakeASR) CanQueryByIdempotencyKey() bool { return false }

type countNotifier struct {
	mu sync.Mutex
	n  int
}

func (c *countNotifier) Notify(context.Context, int64, string, string, string, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}

type speechRefunder struct{ r *billingrepo.Repo }

func (a speechRefunder) Refund(ctx context.Context, payerID int64, refType, refID, reason string) error {
	_, err := a.r.Refund(ctx, billingrepo.RefundInput{PayerID: payerID, RefType: refType, RefID: refID, Reason: reason,
		CompensationValidFor: 30 * 24 * time.Hour})
	return err
}

type speechEnv struct {
	syncEnv
	media    *mediaapp.Service
	gen      *genapp.Service
	ledger   *billingrepo.Repo
	executor *genapp.Executor
	up       *fakeASR
	notes    *countNotifier
}

const silentMeta = `{"granularity":"char","durationMs":60000,"utterances":[]}`
const speechMeta = `{"granularity":"char","durationMs":60000,"utterances":[{"text":"大家好","startMs":100,"endMs":900,
	"words":[{"text":"大","startMs":100,"endMs":300},{"text":"家","startMs":300,"endMs":500},{"text":"好","startMs":500,"endMs":900}]}]}`

func setupSpeech(t *testing.T) speechEnv {
	t.Helper()
	base := setupSync(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	media := mediaapp.New(mediarepo.New(base.pool), nil, base.next)
	catalog := catalogapp.New(catalogrepo.New(base.pool), base.next)
	core, err := wiring.BuildCore(wiring.CoreInput{Cfg: config.Config{},
		Deps: &bootstrap.Deps{DB: &db.Pool{Pool: base.pool}}, NextID: base.next, Logger: logger,
		Audit: audit.New(base.pool, base.next, logger), Media: media, Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	e := speechEnv{syncEnv: base, media: media, gen: core.Gen, ledger: billingrepo.New(base.pool, base.next),
		up: &fakeASR{text: "大家好", meta: speechMeta}, notes: &countNotifier{}}
	e.svc.SetTranscribePorts(wiring.SmartcutSpeechPorts(wiring.SmartcutSpeechInput{Catalog: catalog, Gen: core.Gen,
		Media: media, Timeline: e.tl}))
	binder := wiring.SmartcutBinder{Svc: e.svc}
	core.Gen.SetSmartcutBinder(binder)
	e.executor = genapp.NewExecutor(core.GenRepo, e.up, logger)
	e.executor.SetIDGen(base.next)
	e.executor.SetRefunder(speechRefunder{e.ledger})
	e.executor.SetNotifier(e.notes)
	e.executor.SetSmartcutBinder(binder)

	// 识别模型：火山极速版，按秒 2 积分；只收一段音频（能力矩阵必配）。
	e.exec(t, `insert into api_protocol (code, name, is_async) values ('volc_asr','火山识别',false) on conflict (code) do nothing`)
	pvd, mdl, op := base.next(), base.next(), base.next()
	e.exec(t, `insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
		values ($1,'p-asr','识别上游','http','https://openspeech.example.com','none','enabled')`, pvd)
	e.exec(t, `insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol, submit_path,
		est_seconds, status) values ($1,'asr-m','火山极速版',$2,'text','volc.bigasr.auc_turbo','volc_asr','/api/v3/auc/bigmodel/recognize/flash',30,'enabled')`, mdl, pvd)
	e.exec(t, `insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,'speech2text','语音识别','text','audio','text','enabled')`, op)
	e.exec(t, `insert into model_operation (id, model_id, operation_id, is_enabled) values ($1,$2,$3,true)`, base.next(), mdl, op)
	e.exec(t, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		values ($1,$2,$3,'audio',1,1)`, base.next(), mdl, op)
	e.exec(t, `insert into pricing (id, operation_id, model_id, credits, unit, status) values ($1,$2,$3,2,'per_second','enabled')`,
		base.next(), op, mdl)
	if _, err := e.ledger.Grant(e.ctx, billingrepo.GrantInput{PayerID: speechPayer, Source: "purchase",
		SourceRefID: "speech-fund", Amount: 1000}); err != nil {
		t.Fatal(err)
	}
	return e
}

// video 建一份视频素材与它抽好的识别用音轨（noAudio 时音轨分析记「没有音轨」）。
func (e speechEnv) video(t *testing.T, ms int, speech string) (video, audio int64) {
	t.Helper()
	video = e.mediaOf(t, ws, "video/mp4", ms)
	if speech == "" {
		return video, 0
	}
	audio = e.mediaOf(t, ws, "audio/mpeg", ms)
	result := fmt.Sprintf(`{"durationMs":%d}`, ms)
	if speech == "noAudio" {
		result, speech = `{"durationMs":0,"noAudio":true}`, "success"
	}
	e.exec(t, `insert into media_analysis (id, workspace_id, media_object_id, kind, engine, status, result, derived_media_id)
		values ($1,$2,$3,'speech_audio','speech-v1',$4,$5::jsonb,nullif($6::bigint,0))`,
		e.next(), ws, video, speech, result, audio)
	return video, audio
}

// timelineWith 建一个智能剪辑并把这些视频连进来、铺上轨道。
func (e speechEnv) timelineWith(t *testing.T, videos ...int64) (pid, tid int64) {
	t.Helper()
	pid, tid = e.newSmartcut(t)
	nodes := []map[string]any{nodeWith("sc", "smartcut", nil)}
	var edges []map[string]any
	for i, v := range videos {
		id := fmt.Sprintf("v%d", i+1)
		nodes = append(nodes, nodeWith(id, "video", mediaResult(v)))
		edges = append(edges, edgeTo(id, i+1))
	}
	e.setCanvas(t, pid, nodes, edges)
	if _, err := e.svc.Sync(e.ctx, ws, pid, tid, smartcutapp.SyncAutofill); err != nil {
		t.Fatal(err)
	}
	return pid, tid
}

func (e speechEnv) req(pid, tid int64, key string) smartcutapp.TranscribeRequest {
	return smartcutapp.TranscribeRequest{WorkspaceID: ws, PayerAccountID: speechPayer, ProjectID: pid, TimelineID: tid,
		UserID: user, ModelCode: "asr-m", KeepFillers: true, Language: "zh-CN", IdempotencyKey: key}
}

func (e speechEnv) balance(t *testing.T) int {
	t.Helper()
	b, err := e.ledger.Balance(e.ctx, speechPayer)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e speechEnv) asrRow(t *testing.T, mediaID int64) (status string, result string, genID int64) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx, `select status, coalesce(result::text,''), coalesce(generation_id,0) from media_analysis
		where media_object_id = $1 and kind = 'asr' order by created_at desc, id desc limit 1`, mediaID).
		Scan(&status, &result, &genID); err != nil {
		t.Fatal(err)
	}
	return
}

// SC-AC-49：60 秒素材报价按 60 秒、扣费一次；换一条时间线再识别不再扣费；结果写进源素材的缓存。
// 识别任务在任务中心可见（generation 一行）、不挂画布节点、不登记作品、没有站内通知。
func TestTranscribe_按秒报价只扣一次并跨时间线复用(t *testing.T) {
	e := setupSpeech(t)
	v, audio := e.video(t, 60000, "success")
	pid, tid := e.timelineWith(t, v)

	q, err := e.svc.QuoteTranscribe(e.ctx, smartcutapp.TranscribeRequest{WorkspaceID: ws, PayerAccountID: speechPayer,
		ProjectID: pid, TimelineID: tid, UserID: user, ModelCode: "asr-m", KeepFillers: true, DryRun: true})
	if err != nil || q.Credits != 120 || q.DurationSec != 60 || q.Items != 1 || q.UnitPrice != 2 || q.Unit != "per_second" {
		t.Fatalf("报价应按 60 秒 = 120 积分: %+v %v", q, err)
	}
	if e.balance(t) != 1000 {
		t.Fatal("报价不扣费")
	}

	res, err := e.svc.Transcribe(e.ctx, e.req(pid, tid, "transcribe-ac49-1"))
	if err != nil || len(res.Items) != 1 || res.Items[0].Credits != 120 || res.Items[0].MediaID != v {
		t.Fatalf("应受理一个任务: %+v %v", res, err)
	}
	gid := res.Items[0].GenerationID
	if e.balance(t) != 880 {
		t.Fatalf("应扣 120，余额 %d", e.balance(t))
	}
	var node *string
	var target, inputs string
	if err := e.pool.QueryRow(e.ctx, `select node_id, smartcut_target::text, inputs_snapshot::text from generation where id = $1`,
		gid).Scan(&node, &target, &inputs); err != nil {
		t.Fatal(err)
	}
	if node != nil || !strings.Contains(target, `"kind": "asr"`) || !strings.Contains(inputs, fmt.Sprint(audio)) {
		t.Fatalf("不挂节点、带回写目标、输入是识别用音轨: node=%v target=%s inputs=%s", node, target, inputs)
	}
	if st, _, g := e.asrRow(t, v); st != "running" || g != gid {
		t.Fatalf("分析行应挂上任务: %s %d", st, g)
	}

	if out, err := e.executor.Execute(e.ctx, gid); err != nil || out != genapp.OutcomeDone {
		t.Fatalf("执行: %v %v", out, err)
	}
	st, result, _ := e.asrRow(t, v)
	if st != "success" || !strings.Contains(result, `"text": "大家好"`) || !strings.Contains(result, `"granularity": "char"`) {
		t.Fatalf("识别结果写进源素材的缓存: %s %s", st, result)
	}
	if e.notes.n != 0 {
		t.Fatalf("智能剪辑名下的任务不发站内通知: %d", e.notes.n)
	}
	if n := e.count(t, `select count(*) from work`); n != 0 {
		t.Fatalf("识别不登记作品: %d", n)
	}

	// 另一条时间线用同一份素材：不再受理、不再扣费。
	pid2, tid2 := e.timelineWith(t, v)
	q2, err := e.svc.QuoteTranscribe(e.ctx, smartcutapp.TranscribeRequest{WorkspaceID: ws, ProjectID: pid2, TimelineID: tid2,
		ModelCode: "asr-m", KeepFillers: true, Language: "zh-CN", DryRun: true})
	if err != nil || q2.Credits != 0 || q2.Items != 0 || len(q2.Skipped) != 1 || q2.Skipped[0].Reason != "已有识别结果，不再收费" {
		t.Fatalf("已有结果的素材报 0: %+v %v", q2, err)
	}
	res2, err := e.svc.Transcribe(e.ctx, e.req(pid2, tid2, "transcribe-ac49-2"))
	if err != nil || len(res2.Items) != 0 || len(res2.Skipped) != 1 || e.balance(t) != 880 {
		t.Fatalf("换一条时间线再识别不再扣费: %+v %v 余额 %d", res2, err, e.balance(t))
	}
	if n := e.count(t, `select count(*) from generation`); n != 1 {
		t.Fatalf("只有一个识别任务: %d", n)
	}
}

// SC-AC-49：两个并发识别请求只建一个生成任务、只扣一次（多轮，每轮一份新素材）。
func TestTranscribe_并发请求只建一个任务(t *testing.T) {
	e := setupSpeech(t)
	// 预热连接池：并发用例不预热的话，第一轮多半是排队建连接，根本没有真并发（删锁也绿）。
	var warm sync.WaitGroup
	for i := 0; i < 8; i++ {
		warm.Add(1)
		go func() { defer warm.Done(); _ = e.count(t, `select 1`) }()
	}
	warm.Wait()
	for round := 0; round < 4; round++ {
		v, _ := e.video(t, 10000, "success")
		pid, tid := e.timelineWith(t, v)
		before := e.balance(t)
		var wg sync.WaitGroup
		results := make([]smartcutapp.TranscribeResult, 4)
		errs := make([]error, 4)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i], errs[i] = e.svc.Transcribe(e.ctx, e.req(pid, tid, fmt.Sprintf("transcribe-conc-%d-%d", round, i)))
			}(i)
		}
		wg.Wait()
		accepted := 0
		for i, r := range results {
			if errs[i] != nil {
				t.Fatalf("第 %d 轮请求 %d 失败: %v", round, i, errs[i])
			}
			accepted += len(r.Items)
		}
		if accepted != 1 || before-e.balance(t) != 20 {
			t.Fatalf("第 %d 轮：应只受理一次、扣 20，实际受理 %d 次、扣 %d", round, accepted, before-e.balance(t))
		}
		if n := e.count(t, `select count(*) from generation where smartcut_target->>'mediaId' = $1`, fmt.Sprint(v)); n != 1 {
			t.Fatalf("第 %d 轮：只建一个任务，实际 %d", round, n)
		}
		// 第一道闸是分析行的唯一索引（排队与进行中也唯一）：只有一行，其余请求看到它就不再受理。
		// 第二道是生成的幂等键（同一把 asr:{素材}:{摘要}:1）：前者失效时仍只扣一次，但会白跑报价与受理。
		if n := e.count(t, `select count(*) from media_analysis where media_object_id = $1 and kind = 'asr'`, v); n != 1 {
			t.Fatalf("第 %d 轮：只建一行识别，实际 %d", round, n)
		}
	}
}

// SC-AC-50：上游识别失败 → 按原批次全额退款、流水恰好一条退款；重试是新任务（新的幂等键）。
func TestTranscribe_失败退款且重试换新键(t *testing.T) {
	e := setupSpeech(t)
	v, _ := e.video(t, 30000, "success")
	pid, tid := e.timelineWith(t, v)
	e.up.fail = true
	res, err := e.svc.Transcribe(e.ctx, e.req(pid, tid, "transcribe-fail-1"))
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("受理: %+v %v", res, err)
	}
	first := res.Items[0].GenerationID
	if e.balance(t) != 940 {
		t.Fatalf("先扣 60: %d", e.balance(t))
	}
	if _, err := e.executor.Execute(e.ctx, first); err != nil {
		t.Fatal(err)
	}
	if e.balance(t) != 1000 {
		t.Fatalf("失败后全额退回: %d", e.balance(t))
	}
	if n := e.count(t, `select count(*) from credit_transaction where type = 'refund' and ref_id = $1`, fmt.Sprint(first)); n != 1 {
		t.Fatalf("退款流水恰好一条: %d", n)
	}
	if st, _, _ := e.asrRow(t, v); st != "failed" {
		t.Fatalf("分析行记失败: %s", st)
	}

	e.up.fail = false
	res2, err := e.svc.Transcribe(e.ctx, e.req(pid, tid, "transcribe-fail-2"))
	if err != nil || len(res2.Items) != 1 || res2.Items[0].GenerationID == first {
		t.Fatalf("重试应是新任务: %+v %v", res2, err)
	}
	var k1, k2 string
	_ = e.pool.QueryRow(e.ctx, `select idempotency_key from generation where id = $1`, first).Scan(&k1)
	_ = e.pool.QueryRow(e.ctx, `select idempotency_key from generation where id = $1`, res2.Items[0].GenerationID).Scan(&k2)
	if !strings.HasSuffix(k1, ":1") || !strings.HasSuffix(k2, ":2") {
		t.Fatalf("幂等键按失败次数换新: %q %q", k1, k2)
	}
}

// SC-AC-51：没有音轨的不发起、不收费；有音轨但静音的照常结算，结果为空，落库不报错（SCB-12）。
func TestTranscribe_没有音轨不收费且静音照常结算(t *testing.T) {
	e := setupSpeech(t)
	mute, _ := e.video(t, 20000, "noAudio")
	silent, _ := e.video(t, 20000, "success")
	pid, tid := e.timelineWith(t, mute, silent)
	e.up.text, e.up.meta = "", silentMeta
	res, err := e.svc.Transcribe(e.ctx, e.req(pid, tid, "transcribe-silent-1"))
	if err != nil || len(res.Items) != 1 || res.Items[0].MediaID != silent {
		t.Fatalf("只受理有音轨的那份: %+v %v", res, err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].MediaID != mute || res.Skipped[0].Reason != "没有音轨" {
		t.Fatalf("没有音轨的跳过并说明: %+v", res.Skipped)
	}
	if e.balance(t) != 960 {
		t.Fatalf("只扣有音轨的那份 40: %d", e.balance(t))
	}
	if out, err := e.executor.Execute(e.ctx, res.Items[0].GenerationID); err != nil || out != genapp.OutcomeDone {
		t.Fatalf("静音的识别应正常完成（空串不能被存成 NULL）: %v %v", out, err)
	}
	var text *string
	if err := e.pool.QueryRow(e.ctx, `select text_content from generation_output where generation_id = $1`,
		res.Items[0].GenerationID).Scan(&text); err != nil || text == nil || *text != "" {
		t.Fatalf("文本产物存空串: %v %v", text, err)
	}
	if st, result, _ := e.asrRow(t, silent); st != "success" || !strings.Contains(result, `"text": ""`) {
		t.Fatalf("静音的识别结果为空: %s %s", st, result)
	}
	if e.balance(t) != 960 {
		t.Fatalf("静音照常结算（不退款）: %d", e.balance(t))
	}
}

// 识别用音轨还没抽好：先排队，抽好后续接扫描替用户受理；抽不出来就判失败。
func TestTranscribe_等音轨抽好后自动受理(t *testing.T) {
	e := setupSpeech(t)
	v, _ := e.video(t, 15000, "queued")
	pid, tid := e.timelineWith(t, v)
	res, err := e.svc.Transcribe(e.ctx, e.req(pid, tid, "transcribe-wait-1"))
	if err != nil || len(res.Items) != 0 || len(res.Waiting) != 1 || e.balance(t) != 1000 {
		t.Fatalf("音轨没好时排队、不扣费: %+v %v", res, err)
	}
	if n, err := e.svc.ResumePendingTranscribes(e.ctx); err != nil || n != 0 {
		t.Fatalf("音轨还没好时不受理: %d %v", n, err)
	}
	audio := e.mediaOf(t, ws, "audio/mpeg", 15000)
	e.exec(t, `update media_analysis set status = 'success', derived_media_id = $2, result = '{"durationMs":15000}'::jsonb
		where media_object_id = $1 and kind = 'speech_audio'`, v, audio)
	if n, err := e.svc.ResumePendingTranscribes(e.ctx); err != nil || n != 1 {
		t.Fatalf("音轨好了应受理: %d %v", n, err)
	}
	st, _, gid := e.asrRow(t, v)
	if st != "running" || gid == 0 || e.balance(t) != 970 {
		t.Fatalf("续接受理后挂上任务并扣 30: %s %d 余额 %d", st, gid, e.balance(t))
	}
	if n, _ := e.svc.ResumePendingTranscribes(e.ctx); n != 0 {
		t.Fatal("受理过的不再受理")
	}

	// 抽音轨失败：识别判失败并说明原因，不扣费。
	v2, _ := e.video(t, 15000, "queued")
	pid2, tid2 := e.timelineWith(t, v2)
	if _, err := e.svc.Transcribe(e.ctx, e.req(pid2, tid2, "transcribe-wait-2")); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `update media_analysis set status = 'failed', error_message = '解码失败' where media_object_id = $1 and kind = 'speech_audio'`, v2)
	if n, _ := e.svc.ResumePendingTranscribes(e.ctx); n != 1 {
		t.Fatal("抽音轨失败要判失败")
	}
	if st, _, _ := e.asrRow(t, v2); st != "failed" || e.balance(t) != 970 {
		t.Fatalf("识别判失败、不扣费: %s %d", st, e.balance(t))
	}
}

// 余额不足：一份都没受理成时原样返回错误（前端给充值入口），不留下受理过的任务。
func TestTranscribe_余额不足原样报错(t *testing.T) {
	e := setupSpeech(t)
	v, _ := e.video(t, 600000, "success") // 600 秒 = 1200 积分，余额只有 1000
	pid, tid := e.timelineWith(t, v)
	_, err := e.svc.Transcribe(e.ctx, e.req(pid, tid, "transcribe-broke-1"))
	if bizCode(err) != errcode.InsufficientCredits {
		t.Fatalf("应报余额不足: %v", err)
	}
	if n := e.count(t, `select count(*) from generation`); n != 0 || e.balance(t) != 1000 {
		t.Fatalf("不留任务、不扣费: %d %d", n, e.balance(t))
	}
	if st, _, _ := e.asrRow(t, v); st != "failed" {
		t.Fatalf("分析行记失败（之后可以重新识别）: %s", st)
	}
}

// /smartcut/config 的识别模型清单（SC-AC-52 后半）：与报价同一个目录函数；粒度标明能不能用于口播精剪
// （火山是字级，OpenAI 只有 whisper-1 是词级，其它只有整段文字）。
func TestSpeechModels_标明粒度(t *testing.T) {
	e := setupSpeech(t)
	e.exec(t, `insert into api_protocol (code, name, is_async) values ('openai_transcribe','OpenAI 转写',false) on conflict (code) do nothing`)
	pvd, mdl := e.next(), e.next()
	e.exec(t, `insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
		values ($1,'p-oai','OpenAI','http','https://api.example.com/v1','none','enabled')`, pvd)
	e.exec(t, `insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol, submit_path,
		est_seconds, status, sort) values ($1,'oai-text','gpt-4o 转写',$2,'text','gpt-4o-transcribe','openai_transcribe','/audio/transcriptions',30,'enabled',5)`, mdl, pvd)
	e.exec(t, `insert into model_operation (id, model_id, operation_id, is_enabled)
		select $1, $2, o.id, true from operation o where o.code = 'speech2text'`, e.next(), mdl)
	e.exec(t, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		select $1, $2, o.id, 'audio', 1, 1 from operation o where o.code = 'speech2text'`, e.next(), mdl)
	e.exec(t, `insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, $2, 1, 'per_second', 'enabled' from operation o where o.code = 'speech2text'`, e.next(), mdl)
	catalog := catalogapp.New(catalogrepo.New(e.pool), e.next)
	list, err := catalog.SpeechModels(e.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range list {
		got[m.Code] = fmt.Sprintf("%s %d %s", m.Granularity, m.UnitPrice, m.Unit)
	}
	if got["asr-m"] != "char 2 per_second" || got["oai-text"] != "text 1 per_second" {
		t.Fatalf("识别模型清单: %v", got)
	}
}
