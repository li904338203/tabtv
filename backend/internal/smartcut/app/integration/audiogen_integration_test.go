//go:build integration

// 生成配乐与按画面生成音效（docs/agent/smartcut SCT-39，SC-AC-57）：真实装配的受理（目录报价含协议的时长与参数校验、
// 账本扣费、生成域）+ 真库回填（配乐轨、音效跟随片段）。上游往返由 provider/app 的 music_test.go 覆盖。
package integration

import (
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
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// setupAudioGen 在 setupClip 之上加三个模型：火山豆包音乐（30~60 秒，1 积分/秒）、ElevenLabs（3~300 秒，2 积分/秒）、
// 可灵视频配音效（3~20 秒，只收地址，3 积分/秒）。主存储签得出公网地址；片段级工具的模型清单接上（按模型的时长范围截、拦）。
func setupAudioGen(t *testing.T) clipEnv {
	t.Helper()
	e := setupClip(t)
	e.svc.SetClipToolPorts(wiring.SmartcutClipPorts(wiring.SmartcutClipInput{Gen: e.gen, GenRepo: genrepo.New(e.pool),
		Media: e.media, Timeline: e.tl, Catalog: catalogapp.New(catalogrepo.New(e.pool), e.next)}))
	e.gen.SetMediaURLDelivery(providerapp.MediaDelivery{Main: urlPresigner("https://oss.example.com/b/probe?X-Amz-Signature=1")})
	music, sfx := e.next(), e.next()
	e.exec(t, `insert into operation (id, code, name, domain, in_node_types, out_node_type, status) values
		($1,'text2music','生成配乐','audio','text','audio','enabled'), ($2,'video2audio','视频配音效','audio','video','audio','enabled')`,
		music, sfx)
	// 能力矩阵：配乐的描述在参数里、不算输入（text 0~1）；音效收恰好一段视频。
	for _, m := range []struct {
		code, name, protocol, path string
		op                         int64
		price                      int
		role                       string
		min                        int
	}{
		{"bgm-volc", "豆包音乐", "volc_music", "/?Action=GenBGM&Version=2024-08-12", music, 1, "text", 0},
		{"bgm-eleven", "ElevenLabs Music", "elevenlabs_music", "/v1/music", music, 2, "text", 0},
		{"sfx-kling", "可灵视频配音效", "kling_v2a", "/v1/audio/video-to-audio", sfx, 3, "video", 1},
	} {
		pvd, mdl := e.next(), e.next()
		e.exec(t, `insert into api_protocol (code, name, is_async) values ($1,$1,true) on conflict (code) do nothing`, m.protocol)
		e.exec(t, `insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
			values ($1,$2,$3,'http','https://audio.example.com','none','enabled')`, pvd, "p-"+m.code, m.name)
		e.exec(t, `insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol, submit_path,
			est_seconds, status) values ($1,$2,$3,$4,'audio','m',$5,$6,120,'enabled')`, mdl, m.code, m.name, pvd, m.protocol, m.path)
		e.exec(t, `insert into model_operation (id, model_id, operation_id, is_enabled) values ($1,$2,$3,true)`, e.next(), mdl, m.op)
		e.exec(t, `insert into pricing (id, operation_id, model_id, credits, unit, status) values ($1,$2,$3,$4,'per_second','enabled')`,
			e.next(), m.op, mdl, m.price)
		e.exec(t, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
			values ($1,$2,$3,$4,$5,1)`, e.next(), mdl, m.op, m.role, m.min)
	}
	return e
}

func (e clipEnv) music(key string, dryRun bool, model string, sec, at int, instrumental *bool) (smartcutapp.ClipToolResult, error) {
	return e.svc.GenerateMusic(e.ctx, smartcutapp.MusicRequest{ClipToolRequest: e.clipReq(key, dryRun), Prompt: "轻快的钢琴",
		ModelCode: model, DurationSec: sec, AtMs: at, Instrumental: instrumental})
}

// audioClips 时间线上某个用途的音频轨里的片段（按轨道顺序）。
func audioTracks(t *testing.T, e clipEnv, role string) [][]domain.Clip {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]domain.Clip
	for _, tr := range v.Tracks {
		if tr.Track.Kind == domain.TrackAudio && tr.Track.Role == role {
			out = append(out, tr.Clips)
		}
	}
	return out
}

// editVideo 改画面轨（裁切、调顺序），整份保存。
func (e clipEnv) editVideo(t *testing.T, fn func(clips []domain.ContentClip) []domain.ContentClip) {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	c := timelineapp.ContentFromView(v)
	for i := range c.Tracks {
		if c.Tracks[i].Kind == domain.TrackVideo {
			c.Tracks[i].Clips = fn(c.Tracks[i].Clips)
		}
	}
	if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid, Editor: editor,
		Content: c}); err != nil {
		t.Fatal(err)
	}
}

// SC-AC-57（配乐）：两家各生成一次 —— 产物进配乐轨、起点为发起时的播放头；时长缺省取从播放头到成片结尾
// （成片 9 秒、播放头 1.5 秒 → 7.5 秒，dev/01 R-20 #8），按所选模型的单次范围截：豆包最短 30 秒 → 按 30 秒计费、
// 落下去裁到 7.5 秒（正好收在成片结尾）；ElevenLabs 按 12 秒。
func TestMusic_按播放头放进配乐轨(t *testing.T) {
	e := setupAudioGen(t)
	q, err := e.music("", true, "bgm-volc", 0, 1500, nil)
	if err != nil || q.Quote == nil || q.Quote.Blocker != nil || q.Quote.DurationSec != 30 || q.Quote.Credits != 30 {
		t.Fatalf("播放头到结尾 7.5 秒、豆包最短 30 秒：按 30 秒报价：%+v %v", q.Quote, err)
	}
	before := e.balance(t)
	res, err := e.music("music-ac57-volc", false, "bgm-volc", 0, 1500, nil)
	if err != nil || res.Accepted == nil || res.Accepted.Kind != "music" || before-e.balance(t) != 30 {
		t.Fatalf("受理并扣 30：%+v %v", res.Accepted, err)
	}
	var params string
	_ = e.pool.QueryRow(e.ctx, `select params_snapshot::text from generation where id = $1`, res.Accepted.GenerationID).Scan(&params)
	if !strings.Contains(params, `"duration": 30`) || !strings.Contains(params, `"instrumental": true`) {
		t.Fatalf("发给上游 30 秒、纯音乐：%s", params)
	}
	e.finish(t, res.Accepted.GenerationID, "success", e.mediaOf(t, ws, "audio/mpeg", 30000))
	tracks := audioTracks(t, e, domain.RoleMusic)
	if len(tracks) != 1 || len(tracks[0]) != 1 {
		t.Fatalf("放进配乐轨：%+v", tracks)
	}
	if c := tracks[0][0]; c.StartMs != 1500 || c.SourceInMs != 0 || c.SourceOutMs != 7500 || c.Origin != domain.OriginUser {
		t.Fatalf("起点 = 播放头、裁到播放头到成片结尾的 7.5 秒：%+v", c)
	}

	// ElevenLabs：显式 12 秒、放在 20 秒处（与上一段不重叠，放进同一条配乐轨）。
	q, _ = e.music("", true, "bgm-eleven", 12, 20000, nil)
	if q.Quote.DurationSec != 12 || q.Quote.Credits != 24 {
		t.Fatalf("ElevenLabs 按 12 秒 × 2：%+v", q.Quote)
	}
	res, err = e.music("music-ac57-eleven", false, "bgm-eleven", 12, 20000, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.finish(t, res.Accepted.GenerationID, "success", e.mediaOf(t, ws, "audio/mpeg", 12000))
	tracks = audioTracks(t, e, domain.RoleMusic)
	if len(tracks) != 1 || len(tracks[0]) != 2 || tracks[0][1].StartMs != 20000 || tracks[0][1].SourceOutMs != 12000 {
		t.Fatalf("不重叠时放进同一条配乐轨：%+v", tracks)
	}

	// 豆包只出纯音乐：明确要带人声时在报价阶段就拒（42203），不是悄悄出一段纯音乐。
	vocal := false
	if q, _ := e.music("", true, "bgm-volc", 0, 0, &vocal); q.Quote == nil || q.Quote.Blocker == nil ||
		q.Quote.Blocker.Code != "42203" || !strings.Contains(q.Quote.Blocker.Message, "纯音乐") {
		t.Fatalf("要带人声：报价说明不可用：%+v", q.Quote)
	}
	// 超过豆包的上限：截到 60 秒。
	if q, _ := e.music("", true, "bgm-volc", 90, 0, nil); q.Quote.DurationSec != 60 || q.Quote.Credits != 60 {
		t.Fatalf("90 秒截到豆包的上限 60 秒：%+v", q.Quote)
	}
}

// 配乐与已有配乐重叠时放进新建的配乐轨：不覆盖、不报错。
func TestMusic_重叠时新建配乐轨(t *testing.T) {
	e := setupAudioGen(t)
	first, err := e.music("music-overlap-1", false, "bgm-eleven", 10, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.finish(t, first.Accepted.GenerationID, "success", e.mediaOf(t, ws, "audio/mpeg", 10000))
	second, err := e.music("music-overlap-2", false, "bgm-eleven", 10, 4000, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.finish(t, second.Accepted.GenerationID, "success", e.mediaOf(t, ws, "audio/mpeg", 10000))
	tracks := audioTracks(t, e, domain.RoleMusic)
	if len(tracks) != 2 || len(tracks[0]) != 1 || len(tracks[1]) != 1 || tracks[0][0].StartMs != 0 || tracks[1][0].StartMs != 4000 {
		t.Fatalf("重叠的那段放进新建的配乐轨，原来那段不动：%+v", tracks)
	}
	list, _, _ := e.svc.Generations(e.ctx, ws, e.pid, e.tid, "", 10)
	if len(list) < 2 || !list[0].Landed || !list[1].Landed || list[0].Kind != "music" {
		t.Fatalf("两次都落下去了：%+v", list)
	}
}

// SC-AC-57（音效）：可灵只收 3~20 秒的片段 —— 25 秒的不受理并说明（42203、不扣费）；裁到 8 秒后受理，按 8 秒计价。
func TestSFX_片段超长不受理(t *testing.T) {
	e := setupAudioGen(t)
	long := e.mediaOf(t, ws, "video/mp4", 30000)
	e.pid, e.tid = e.timelineWith(t, long)
	clip := e.clips(t)[0]
	e.editVideo(t, func(cs []domain.ContentClip) []domain.ContentClip {
		cs[0].SourceInMs, cs[0].SourceOutMs = 0, 25000
		return cs
	})
	before := e.balance(t)
	_, err := e.svc.GenerateSfx(e.ctx, smartcutapp.SfxRequest{ClipToolRequest: e.clipReq("sfx-long-1", false), ClipID: clip.ID,
		ModelCode: "sfx-kling", Prompt: "雨声"})
	if !isCode(err, errcode.CapabilityUnsupported) || !strings.Contains(err.Error(), "3~20 秒") || !strings.Contains(err.Error(), "25.0 秒") {
		t.Fatalf("25 秒不受理并说明：%v", err)
	}
	if e.balance(t) != before || e.count(t, `select count(*) from generation g join operation o on o.id = g.operation_id
		where o.code = 'video2audio'`) != 0 {
		t.Fatal("不扣费、不建任务")
	}
	e.editVideo(t, func(cs []domain.ContentClip) []domain.ContentClip {
		cs[0].SourceInMs, cs[0].SourceOutMs = 2000, 10000
		return cs
	})
	q, err := e.svc.GenerateSfx(e.ctx, smartcutapp.SfxRequest{ClipToolRequest: e.clipReq("", true), ClipID: clip.ID,
		ModelCode: "sfx-kling"})
	if err != nil || q.Quote == nil || q.Quote.Blocker != nil || q.Quote.DurationSec != 8 || q.Quote.Credits != 24 {
		t.Fatalf("8 秒按区间报价（8 × 3）：%+v %v", q.Quote, err)
	}
	res, err := e.svc.GenerateSfx(e.ctx, smartcutapp.SfxRequest{ClipToolRequest: e.clipReq("sfx-ok-1", false), ClipID: clip.ID,
		ModelCode: "sfx-kling", Prompt: "雨声"})
	if err != nil || res.Accepted == nil || res.Accepted.Kind != "sfx" || before-e.balance(t) != 24 {
		t.Fatalf("受理并扣 24：%+v %v", res.Accepted, err)
	}
	var inputs string
	_ = e.pool.QueryRow(e.ctx, `select inputs_snapshot::text from generation where id = $1`, res.Accepted.GenerationID).Scan(&inputs)
	if !strings.Contains(inputs, `"rangeMs": [2000, 10000]`) {
		t.Fatalf("只发这一段：%s", inputs)
	}
}

// SC-AC-57（跟随）：音效放进音效轨、跟随那一段（SC-F-56）—— 裁掉那一段的开头，音效跳过对应的一截；
// 把那一段挪到别处，音效跟着走；那一段改得与生成时的区间不相交，音效隐藏（不删除、不进成片），恢复后重新出现。
func TestSFX_回填后跟随片段(t *testing.T) {
	e := setupAudioGen(t)
	first, second := e.clips(t)[0], e.clips(t)[1] // 第一段 6 秒（生成的）、第二段 3 秒（上传的）
	res, err := e.svc.GenerateSfx(e.ctx, smartcutapp.SfxRequest{ClipToolRequest: e.clipReq("sfx-follow-1", false), ClipID: second.ID,
		ModelCode: "sfx-kling", Prompt: "脚步声"})
	if err != nil {
		t.Fatal(err)
	}
	e.finish(t, res.Accepted.GenerationID, "success", e.mediaOf(t, ws, "audio/mpeg", 3000))
	sfx := func() domain.Clip {
		t.Helper()
		tracks := audioTracks(t, e, domain.RoleSfx)
		if len(tracks) != 1 || len(tracks[0]) != 1 {
			t.Fatalf("放进音效轨：%+v", tracks)
		}
		return tracks[0][0]
	}
	got := sfx()
	if got.StartMs != 6000 || got.SourceInMs != 0 || got.SourceOutMs != 3000 || got.Hidden || len(got.Anchor) == 0 {
		t.Fatalf("落在第二段的起点（6 秒）、带锚点：%+v %s", got, got.Anchor)
	}
	// 裁掉第二段开头 1 秒：音效跟着跳过 1 秒、仍从第二段的起点响。
	e.editVideo(t, func(cs []domain.ContentClip) []domain.ContentClip { cs[1].SourceInMs = 1000; return cs })
	if got = sfx(); got.StartMs != 6000 || got.SourceInMs != 1000 || got.SourceOutMs != 3000 || got.Hidden {
		t.Fatalf("裁掉开头后音效跳过那一截：%+v", got)
	}
	// 第二段挪到最前面：音效跟着到 0。
	e.editVideo(t, func(cs []domain.ContentClip) []domain.ContentClip { return []domain.ContentClip{cs[1], cs[0]} })
	if got = sfx(); got.StartMs != 0 || got.Hidden {
		t.Fatalf("挪到最前面后音效跟着走：%+v", got)
	}
	// 那一段改取素材里与生成时（0~3 秒）不相交的区间：隐藏（不删除；不进成片由 timeline/app 的
	// TestSnapshot_跟随片段被裁掉的音效不进成片 核对）。素材时长先放宽到 9 秒，取 5~8 秒才合法。
	e.exec(t, `update media_object set duration_ms = 9000 where id = $1`, second.MediaID)
	e.editVideo(t, func(cs []domain.ContentClip) []domain.ContentClip {
		cs[0].SourceInMs, cs[0].SourceOutMs = 5000, 8000
		return cs
	})
	if got = sfx(); !got.Hidden {
		t.Fatalf("那一段与生成时的区间不相交：隐藏：%+v", got)
	}
	e.editVideo(t, func(cs []domain.ContentClip) []domain.ContentClip {
		cs[0].SourceInMs, cs[0].SourceOutMs = 0, 3000
		return cs
	})
	if got = sfx(); got.Hidden || got.StartMs != 0 || got.SourceInMs != 0 || got.SourceOutMs != 3000 {
		t.Fatalf("恢复区间后重新出现：%+v", got)
	}
	// 第一段（生成出来的那段）不受影响：音效只跟它自己的那一段。
	if v := e.clips(t); len(v) != 2 || v[1].ID != first.ID {
		t.Fatalf("画面轨：%+v", v)
	}
}
