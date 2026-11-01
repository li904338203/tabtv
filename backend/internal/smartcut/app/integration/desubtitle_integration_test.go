//go:build integration

// 去字幕（docs/agent/smartcut SCT-38，SC-AC-56）：真实装配的受理（目录报价、账本扣费、生成域、参考媒体送达判断）
// + 真库回填。截取与上游往返（发出去的是截出的 3 秒、结果取回存档）由 provider/app 的
// TestMediaKitErase_区间片段发出3秒并取回结果 覆盖。
package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	providerapp "github.com/li904338203/tabtv/backend/internal/provider/app"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

type eraseEnv struct {
	clipEnv
	eraseOp int64
}

// urlPresigner 按固定的地址签名（判「上游够不够得着」只看端点）。
type urlPresigner string

func (u urlPresigner) PresignGet(context.Context, string) (string, error) { return string(u), nil }

// setupErase 在 setupClip 之上加一个去字幕模型（火山 MediaKit 协议：只收公网地址，按秒 2 积分），
// 主存储签出的地址是 mainURL（公网 / 内网决定只收地址的上游收不收得到素材）。
func setupErase(t *testing.T, mainURL string) eraseEnv {
	t.Helper()
	e := eraseEnv{clipEnv: setupClip(t)}
	e.gen.SetMediaURLDelivery(providerapp.MediaDelivery{Main: urlPresigner(mainURL)})
	pvd, mdl := e.next(), e.next()
	e.eraseOp = e.next()
	e.exec(t, `insert into api_protocol (code, name, is_async) values ('volc_mediakit_erase','火山去字幕',true) on conflict (code) do nothing`)
	e.exec(t, `insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
		values ($1,'p-erase','去字幕上游','http','https://mediakit.example.com','none','enabled')`, pvd)
	e.exec(t, `insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol, submit_path, query_path,
		est_seconds, status) values ($1,'erase-m','火山去字幕',$2,'video','erase','volc_mediakit_erase',
		'/api/v1/tools/erase-video-subtitle-pro','/api/v1/tasks/{taskId}',120,'enabled')`, mdl, pvd)
	e.exec(t, `insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,'video_desubtitle','去字幕','video','video','video','enabled')`, e.eraseOp)
	e.exec(t, `insert into model_operation (id, model_id, operation_id, is_enabled) values ($1,$2,$3,true)`, e.next(), mdl, e.eraseOp)
	e.exec(t, `insert into pricing (id, operation_id, model_id, credits, unit, status) values ($1,$2,$3,2,'per_second','enabled')`,
		e.next(), e.eraseOp, mdl)
	e.exec(t, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		values ($1,$2,$3,'video',1,1)`, e.next(), mdl, e.eraseOp)
	return e
}

// takeRange 让第一段只取用素材的 [in, out]（用户在剪辑台里裁过）。
func (e eraseEnv) takeRange(t *testing.T, in, out int) domain.Clip {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	c := timelineapp.ContentFromView(v)
	for i := range c.Tracks {
		if c.Tracks[i].Kind == domain.TrackVideo {
			c.Tracks[i].Clips[0].SourceInMs, c.Tracks[i].Clips[0].SourceOutMs = in, out
		}
	}
	if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid, Editor: editor,
		Content: c}); err != nil {
		t.Fatal(err)
	}
	return e.clips(t)[0]
}

func (e eraseEnv) desubtitle(key string, dryRun bool, clipID int64) (smartcutapp.ClipToolResult, error) {
	return e.svc.DesubtitleClip(e.ctx, smartcutapp.DesubtitleRequest{ClipToolRequest: e.clipReq(key, dryRun), ClipID: clipID,
		ModelCode: "erase-m"})
}

// SC-AC-56（报价与受理）：一段取用了 2~5 秒的片段 —— 报价按 3 秒（2 积分/秒 = 6），受理扣 6；
// 输入快照里是这份素材 + 区间 [2000, 5000]（Worker 据此截出这 3 秒再发）；这一段标成生成中。
func TestDesubtitle_按区间报价与计价(t *testing.T) {
	e := setupErase(t, "https://oss.example.com/bucket/probe?X-Amz-Signature=1")
	first := e.takeRange(t, 2000, 5000)

	q, err := e.desubtitle("", true, first.ID)
	if err != nil || q.Quote == nil || q.Quote.Blocker != nil || q.Quote.Credits != 6 || q.Quote.DurationSec != 3 ||
		q.Quote.UnitPrice != 2 || q.Quote.Unit != "per_second" {
		t.Fatalf("按区间 3 秒报价：%+v %v", q.Quote, err)
	}
	before := e.balance(t)
	res, err := e.desubtitle("erase-ac56-1", false, first.ID)
	if err != nil || res.Accepted == nil || res.Accepted.Kind != "desubtitle" {
		t.Fatalf("受理：%+v %v", res, err)
	}
	gid := res.Accepted.GenerationID
	if spent := before - e.balance(t); spent != 6 || res.Accepted.Credits != 6 {
		t.Fatalf("扣 6：%d %+v", spent, res.Accepted)
	}
	var node *string
	var op, mode, origin, inputs string
	if err := e.pool.QueryRow(e.ctx, `select g.node_id, o.code, g.params_snapshot->>'mode', g.params_snapshot->'_snapshot'->>'origin',
		g.inputs_snapshot::text from generation g join operation o on o.id = g.operation_id where g.id = $1`, gid).
		Scan(&node, &op, &mode, &origin, &inputs); err != nil {
		t.Fatal(err)
	}
	if node != nil || op != "video_desubtitle" || mode != "subtitle" || origin != "smartcut" {
		t.Fatalf("不挂节点、操作 video_desubtitle、缺省只去字幕、来源 smartcut：%v %q %q %q", node, op, mode, origin)
	}
	if !strings.Contains(inputs, `"rangeMs": [2000, 5000]`) || !strings.Contains(inputs, `"mediaKind": "video"`) {
		t.Fatalf("输入快照带区间：%s", inputs)
	}
	target := string(e.target(t, gid))
	if !strings.Contains(target, `"tool": "desubtitle"`) || !strings.Contains(target, `"kind": "replace"`) {
		t.Fatalf("回写目标：替换这一段：%s", target)
	}
	if p, _ := e.tl.ClipPending(e.ctx, ws, e.tid, first.ID); p != gid {
		t.Fatalf("这一段标成生成中：%d", p)
	}
	// 同一段正在去字幕时不能再点一次（40905）。
	if _, err := e.desubtitle("erase-ac56-2", false, first.ID); !isCode(err, errcode.InvalidStateTransition) {
		t.Fatalf("同一段同时只能有一次：%v", err)
	}
	// 不存在的片段回 404。
	if _, err := e.desubtitle("erase-ac56-3", true, first.ID+999); !isCode(err, errcode.NotFound) {
		t.Fatalf("没有这一段：%v", err)
	}
}

// SC-AC-56（回填）：完成后这一段换成新素材（从头取 3 秒），原素材连同原区间 [2000, 5000] 进备选；
// 去字幕的结果不能再「重新生成」（那样重拍的是去字幕这一步）。
func TestDesubtitle_完成后替换并保留原区间进备选(t *testing.T) {
	e := setupErase(t, "https://oss.example.com/bucket/probe?X-Amz-Signature=1")
	first := e.takeRange(t, 2000, 5000)
	res, err := e.desubtitle("erase-land-1", false, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	gid := res.Accepted.GenerationID
	out := e.mediaOf(t, ws, "video/mp4", 3000)
	e.finish(t, gid, "success", out)

	after := e.clips(t)[0]
	if after.ID != first.ID || after.MediaID != out || after.SourceInMs != 0 || after.SourceOutMs != 3000 {
		t.Fatalf("换成去字幕后的素材、从头取区间长度：%+v", after)
	}
	if len(after.Alternates) != 1 || after.Alternates[0] != (domain.Alternate{MediaID: e.sourceMedia, SourceInMs: 2000,
		SourceOutMs: 5000, Kind: domain.AlternateOriginal, GenerationID: e.source}) {
		t.Fatalf("原素材连同原区间进备选：%+v", after.Alternates)
	}
	if after.PendingGenerationID != 0 || !strings.Contains(string(e.target(t, gid)), `"landed": true`) {
		t.Fatal("回填后清掉生成中、记下已回填")
	}
	list, _, err := e.svc.Generations(e.ctx, ws, e.pid, e.tid, "", 10)
	if err != nil || len(list) == 0 || list[0].Kind != "desubtitle" || !list[0].Landed || list[0].OutputMediaID != out {
		t.Fatalf("生成记录：%+v %v", list, err)
	}
	_, err = e.svc.RegenerateClip(e.ctx, smartcutapp.RegenerateRequest{ClipToolRequest: e.clipReq("erase-land-2", true),
		ClipID: first.ID, Prompt: "再来一次"})
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.SmartcutInputUnsuitable || !strings.Contains(be.Error(), "去字幕的结果") {
		t.Fatalf("去字幕的结果要先切回原片才能重拍：%v", err)
	}
}

// 只收地址的上游、主存储签出的是内网地址、又没有公网中转：报价说明不可用；受理直接拒 —— 不扣费、不建任务、
// 不标生成中（Worker 也就不会发上游）。
func TestDesubtitle_无可达地址时受理即失败不扣费(t *testing.T) {
	e := setupErase(t, "http://127.0.0.1:9100/bucket/probe?X-Amz-Signature=1")
	first := e.takeRange(t, 2000, 5000)

	q, err := e.desubtitle("", true, first.ID)
	if err != nil || q.Quote == nil || q.Quote.Blocker == nil || q.Quote.Blocker.Code != "42204" ||
		!strings.Contains(q.Quote.Blocker.Message, "公网") || q.Quote.Credits != 0 {
		t.Fatalf("报价阶段说明收不到素材：%+v %v", q.Quote, err)
	}
	before := e.balance(t)
	txBefore := e.count(t, `select count(*) from credit_transaction`)
	_, err = e.desubtitle("erase-unreachable-1", false, first.ID)
	if !isCode(err, errcode.ModelUnavailable) {
		t.Fatalf("受理要拒：%v", err)
	}
	if e.balance(t) != before || e.count(t, `select count(*) from credit_transaction`) != txBefore {
		t.Fatal("不扣费")
	}
	if n := e.count(t, `select count(*) from generation where operation_id = $1`, e.eraseOp); n != 0 {
		t.Fatalf("不建任务：%d", n)
	}
	if p, _ := e.tl.ClipPending(e.ctx, ws, e.tid, first.ID); p != 0 {
		t.Fatalf("不标生成中：%d", p)
	}
}

func isCode(err error, code errcode.Code) bool {
	var be *httpx.BizError
	return errors.As(err, &be) && be.Code == code
}

// /smartcut/config 的片段级工具模型（SCT-38~40）：与受理同一个报价函数，按工具要的输入报价；报不出价的不列。
func TestClipModels_按工具要的输入列可选模型(t *testing.T) {
	e := setupErase(t, "https://oss.example.com/bucket/probe?X-Amz-Signature=1")
	ports := wiring.SmartcutClipPorts(wiring.SmartcutClipInput{Gen: e.gen, GenRepo: genrepo.New(e.pool), Media: e.media,
		Timeline: e.tl, Catalog: catalogapp.New(catalogrepo.New(e.pool), e.next)})
	if ports.Models == nil {
		t.Fatal("API 侧要接上模型清单")
	}
	erase, err := ports.Models.ClipModels(e.ctx, "video_desubtitle", 0, map[string]int{"video": 1})
	if err != nil || len(erase) != 1 || erase[0].Code != "erase-m" || erase[0].Name != "火山去字幕" ||
		erase[0].UnitPrice != 2 || erase[0].Unit != "per_second" {
		t.Fatalf("去字幕模型: %+v %v", erase, err)
	}
	// 文生视频只收文字：按「一段视频」去问就报不出价，不列。
	if list, _ := ports.Models.ClipModels(e.ctx, "text2video", 0, map[string]int{"video": 1}); len(list) != 0 {
		t.Fatalf("报不出价的不列: %+v", list)
	}
	fill, _ := ports.Models.ClipModels(e.ctx, "img2video", 0, map[string]int{"text": 1, "image": 1})
	if len(fill) != 1 || fill[0].Code != "vid-m" || fill[0].UnitPrice != 3 {
		t.Fatalf("补镜（图生视频）模型: %+v", fill)
	}
}
