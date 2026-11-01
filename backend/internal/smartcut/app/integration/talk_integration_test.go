//go:build integration

// 口播精剪（docs/agent/smartcut SCT-34，SC-AC-42）：识别结果（字级，含 3 次「嗯」与一句重复）→ 固定的规划回包 →
// 编译成剪点 → 写进时间线并自动生成跟随片段的字幕；裁掉片段开头 1 秒后字幕跟着移、被裁区间里的字幕隐藏。
// 真库、真时间线写入路径；文本模型与识别目录是测试替身，识别结果取自火山识别的 fixture。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// talkTranscript 10 秒的口播（字级、保留口癖），取自 api/fixtures/smartcut/asr/volc_talk_fillers.json 的统一结果 ——
// 那份 fixture 由 provider/protocol 的 TestASRFixtures 核对「火山原始响应 → 这份结果」，这里接着用它剪：
//
//	s1 嗯，大家好，               嗯 100~300，大家好 600~1200
//	s2 今天讲剪辑。               1500~2500
//	s3 今天讲剪辑。（重复）       3300~4300
//	s4 嗯，第一步，嗯，导入素材。  嗯 4900~5100，第一步 5400~6000，嗯 6300~6500，导入素材 6800~7600
//
// 火山极速版不回语言：识别时指定的 zh-CN 记在参数里（params_key），字幕轨的语言取它。
func talkTranscript(t *testing.T) string { return asrFixtureMeta(t, "volc_talk_fillers.json") }

// asrFixtureMeta 识别 fixture（api/fixtures/smartcut/asr/）里的统一结果 —— provider/protocol 的 TestASRFixtures
// 核对过「上游原始响应 → 这份结果」，集成测试接着拿它剪。
func asrFixtureMeta(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "api", "fixtures", "smartcut", "asr", name))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Expect struct {
			Meta json.RawMessage `json:"meta"`
		} `json:"expect"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil || len(fx.Expect.Meta) == 0 {
		t.Fatalf("fixture %s：%v", name, err)
	}
	return string(fx.Expect.Meta)
}

const talkReply = `{"keep":[{"from":"s1","to":"s2"},{"from":"s4","to":"s4"}],"drop":[{"sentence":"s3","reason":"重复"}],"subtitle":true}`

// talkSpeechCatalog 识别目录：一个字级模型，按秒 2 积分（42217 的报价用）。
type talkSpeechCatalog struct{}

func (talkSpeechCatalog) SpeechModels(context.Context, int64) ([]smartcutapp.ModelChoice, error) {
	return []smartcutapp.ModelChoice{{Code: "asr-m", Granularity: "char"}}, nil
}

func (talkSpeechCatalog) QuoteSpeech(_ context.Context, _ string, _ int64, sec int) (smartcutapp.SpeechQuote,
	*smartcutapp.Unavailable, error) {
	return smartcutapp.SpeechQuote{Credits: sec * 2, UnitPrice: 2, Unit: "per_second"}, nil, nil
}

type talkEnv struct {
	planEnv
	video int64
	pid   int64
	tid   int64
}

// setupTalk 一个只连了一段 10 秒视频的智能剪辑（已自动铺轨）；params 非空时给这段视频插一行成功的识别结果。
func setupTalk(t *testing.T, params string) talkEnv {
	t.Helper()
	e := talkEnv{planEnv: setupPlan(t, config.Smartcut{})}
	e.text.replies = []string{talkReply}
	e.svc.SetTranscribePorts(smartcutapp.TranscribePorts{Catalog: talkSpeechCatalog{}})
	e.video = e.mediaOf(t, ws, "video/mp4", 10000)
	e.pid, e.tid = e.newSmartcut(t)
	e.setCanvas(t, e.pid, []map[string]any{nodeWith("sc", "smartcut", nil), nodeWith("v1", "video", mediaResult(e.video))},
		[]map[string]any{edgeTo("v1", 1)})
	if _, err := e.svc.Sync(e.ctx, ws, e.pid, e.tid, smartcutapp.SyncAutofill); err != nil {
		t.Fatal(err)
	}
	if params != "" {
		e.exec(t, `insert into media_analysis (id, workspace_id, media_object_id, kind, engine, params_key, status, result)
			values ($1,$2,$3,'asr','asr-m',$4,'success',$5::jsonb)`, e.next(), ws, e.video, params, talkTranscript(t))
	}
	return e
}

func (e talkEnv) view(t *testing.T) timelineapp.View {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e talkEnv) talkReq(t *testing.T) smartcutapp.PlanRequest {
	t.Helper()
	return smartcutapp.PlanRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid, Actor: smartcutapp.Actor{UserID: planUser},
		Mode: smartcutdomain.ModeTalk, Variants: 1, BaseVersion: e.view(t).Timeline.Version}
}

func (e talkEnv) talkCalls(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `select count(*) from smartcut_llm_call where timeline_id = $1`, e.tid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func videoRanges(v timelineapp.View) (text string, ranges [][2]int) {
	var parts []string
	for _, c := range clipsOfView(v, domain.TrackVideo) {
		parts = append(parts, fmt.Sprintf("%d-%d", c.SourceInMs, c.SourceOutMs))
		ranges = append(ranges, [2]int{c.SourceInMs, c.SourceOutMs})
	}
	return strings.Join(parts, " "), ranges
}

func clipsOfView(v timelineapp.View, kind string) []domain.Clip {
	var out []domain.Clip
	for _, tr := range v.Tracks {
		if tr.Track.Kind == kind {
			out = append(out, tr.Clips...)
		}
	}
	return out
}

// SC-AC-42：口癖与重复句剪掉，剪点落在字与字之间（外扩不进字），字幕自动生成且每条跟随片段。
func TestTalkPlan_端到端剪掉口癖与重复句(t *testing.T) {
	e := setupTalk(t, "f1.zh-CN")
	res, err := e.svc.Plan(e.ctx, e.talkReq(t))
	if err != nil {
		t.Fatal(err)
	}
	v0 := res.Variants[0]
	if v0.UsedFallback || v0.Status != "success" {
		t.Fatalf("固定回包是合规的，不该走兜底: %+v", v0)
	}
	// 交给模型的是按句编号的识别稿（s1~s4），重复的那句也在稿子里、由模型决定去留。
	user := e.text.requests()[0].User
	for _, want := range []string{"## 口播识别稿（按句编号）", "- s1（", "- s3（", "- s4（"} {
		if !strings.Contains(user, want) {
			t.Fatalf("user 消息缺 %q：\n%s", want, user)
		}
	}

	v := e.view(t)
	got, ranges := videoRanges(v)
	// 大家好今天讲剪辑（中间停顿 300，一段）| 第一步 | 导入素材（两个「嗯」把 s4 切成两截）。
	// 剪点外扩 min(max(停顿/2, 30), 200, 停顿)：[600-150, 2500+200]、[5400-150, 6000+150]、[6800-150, 7600+200]。
	if got != "450-2700 5250-6150 6650-7800" {
		t.Fatalf("剪点: %s", got)
	}
	inside := func(lo, hi int) bool {
		for _, r := range ranges {
			if lo < r[1] && hi > r[0] {
				return true
			}
		}
		return false
	}
	for _, filler := range [][2]int{{100, 300}, {4900, 5100}, {6300, 6500}} {
		if inside(filler[0], filler[1]) {
			t.Fatalf("口癖 %v 要剪掉: %s", filler, got)
		}
	}
	if inside(3300, 4300) {
		t.Fatalf("重复的那句要剪掉: %s", got)
	}
	// 每个剪点都不切进字里：任何一个字要么整个在段里，要么整个在段外。
	var tr smartcutdomain.Transcript
	if err := json.Unmarshal([]byte(talkTranscript(t)), &tr); err != nil {
		t.Fatal(err)
	}
	for _, u := range tr.Utterances {
		for _, w := range u.Words {
			for _, r := range ranges {
				if (r[0] > w.StartMs && r[0] < w.EndMs) || (r[1] > w.StartMs && r[1] < w.EndMs) {
					t.Fatalf("剪点 %v 切进了「%s」%d~%d", r, w.Text, w.StartMs, w.EndMs)
				}
			}
		}
	}
	clipIDs := map[int64]bool{}
	for _, c := range clipsOfView(v, domain.TrackVideo) {
		if c.FadeInMs != smartcutdomain.CutFadeMs || c.FadeOutMs != smartcutdomain.CutFadeMs || c.MediaID != e.video {
			t.Fatalf("每个剪点 30ms 声音淡入淡出: %+v", c)
		}
		clipIDs[c.ID] = true
	}
	if !strings.Contains(v0.Summary, "保留了 3 句（共 4 句）") || !strings.Contains(v0.Summary, "去掉口癖 3 处") {
		t.Fatalf("结果要说保留了几句、去掉了几处口癖: %q", v0.Summary)
	}

	// 字幕自动生成：四条（不带口癖），每条带锚点、指向自己所在的那一段。
	var texts []string
	for _, s := range v.Subtitles {
		texts = append(texts, s.Text)
		a, ok := domain.ParseAnchor(s.Anchor)
		if !ok || !clipIDs[a.ClipID] || s.Hidden {
			t.Fatalf("字幕要跟随片段: %+v", s)
		}
	}
	if strings.Join(texts, "|") != "大家好|今天讲剪辑|第一步|导入素材" {
		t.Fatalf("字幕文字: %v", texts)
	}
	for _, tr := range v.Tracks {
		if tr.Track.Kind == domain.TrackSubtitle && tr.Track.Lang != "zh-CN" {
			t.Fatalf("字幕轨语言取识别语言: %q", tr.Track.Lang)
		}
	}
	if n := e.talkCalls(t); n != 1 {
		t.Fatalf("一次规划一条调用记录: %d", n)
	}
}

// SC-AC-42：裁掉第一段开头 1 秒 —— 那一秒里的「大家好」隐藏（不删），后面的字幕都跟着前移 1 秒。
func TestTalkPlan_字幕跟随裁切(t *testing.T) {
	e := setupTalk(t, "f1.zh-CN")
	if _, err := e.svc.Plan(e.ctx, e.talkReq(t)); err != nil {
		t.Fatal(err)
	}
	before := e.view(t)
	start := map[string]int{}
	for _, s := range before.Subtitles {
		start[s.Text] = s.StartMs
	}
	c := timelineapp.ContentFromView(before)
	for i := range c.Tracks {
		if c.Tracks[i].Kind == domain.TrackVideo {
			c.Tracks[i].Clips[0].SourceInMs += 1000
		}
	}
	if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid, Editor: editor,
		Content: c}); err != nil {
		t.Fatal(err)
	}
	after := e.view(t)
	if len(after.Subtitles) != 4 {
		t.Fatalf("被裁区间里的字幕隐藏而不是删掉: %+v", after.Subtitles)
	}
	for _, s := range after.Subtitles {
		switch s.Text {
		case "大家好":
			if !s.Hidden {
				t.Fatalf("「大家好」整句落在被裁掉的那一秒里，要隐藏: %+v", s)
			}
		default:
			if s.Hidden || start[s.Text]-s.StartMs != 1000 {
				t.Fatalf("「%s」要整体前移 1 秒: 前 %d 后 %+v", s.Text, start[s.Text], s)
			}
		}
	}
}

// 没有识别结果、识别时去掉了口癖、只有整段文字：回 42217 并说清是哪一份、为什么不能用，带识别报价；
// 不发起规划、不计入限流次数（没有调用记录）。
func TestTalkPlan_识别结果不能用时回42217(t *testing.T) {
	for _, c := range []struct {
		params, granularity, want string
	}{
		{"", "", "编号 1：还没有识别"},
		{"f0.zh-CN", "", "编号 1：识别时去掉了口癖"},
		{"f1.zh-CN", "text", "编号 1：识别结果只有整句或整段文字"},
	} {
		e := setupTalk(t, c.params)
		if c.granularity != "" {
			e.exec(t, `update media_analysis set result = jsonb_set(result, '{granularity}', to_jsonb($1::text)) where kind = 'asr'`,
				c.granularity)
		}
		_, err := e.svc.Plan(e.ctx, e.talkReq(t))
		var be *httpx.BizError
		if !errors.As(err, &be) || be.Code != errcode.SmartcutInputUnsuitable ||
			be.Details["reason"] != smartcutdomain.UnsuitableASRMissing || !strings.Contains(be.Message, c.want) {
			t.Fatalf("%q：期望 42217 并说「%s」，实际 %v", c.params, c.want, err)
		}
		q, _ := be.Details["quote"].(map[string]any)
		if fmt.Sprint(be.Details["missing"]) != "[1]" || q["credits"] != 20 || q["keepFillers"] != true {
			t.Fatalf("%q：缺的是编号 1、按 10 秒报价且保留口癖: %v", c.params, be.Details)
		}
		if e.talkCalls(t) != 0 || len(e.text.requests()) != 0 {
			t.Fatalf("%q：不发起规划、不占额度", c.params)
		}
	}
}

// 模型两次都不合规：走兜底（一句不删，只去掉口癖与长停顿），时间线没被清空，结果写明是兜底、去掉了什么。
func TestTalkPlan_两次不合规走去停顿兜底(t *testing.T) {
	e := setupTalk(t, "f1.zh-CN")
	e.text.replies = []string{"我觉得都挺好的", "还是都挺好的"}
	res, err := e.svc.Plan(e.ctx, e.talkReq(t))
	if err != nil {
		t.Fatal(err)
	}
	v := res.Variants[0]
	// s2 → s3 停顿 800ms（> 600）剪掉；重复的 s3 也保留（兜底不删句）。
	if !v.UsedFallback || len(v.Notes) == 0 || v.Notes[0] != smartcutdomain.FallbackNote ||
		!strings.Contains(v.Summary, "保留了 4 句（共 4 句）") || !strings.Contains(v.Summary, "去掉口癖 3 处") ||
		!strings.Contains(v.Summary, "剪掉长停顿 1 处") {
		t.Fatalf("兜底结果: %+v", v)
	}
	if n := len(e.text.requests()); n != 2 {
		t.Fatalf("回灌修一次，共调用 2 次: %d", n)
	}
	if got, _ := videoRanges(e.view(t)); got != "450-2700 3100-4500 5250-6150 6650-7800" {
		t.Fatalf("时间线没被清空，按规则剪好: %s", got)
	}
}
