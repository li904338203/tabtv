//go:build integration

// 解说成片（docs/agent/smartcut SCT-35，SC-AC-43）：配音来自画布音频节点（文字转语音的产物）→ 识别结果（火山 fixture，
// 有同音错字与漏字）→ 解说稿对齐 → 固定的规划回包 → 以配音时长为准配画面、字幕用原稿文字并跟随配音片段。
// 真库、真时间线写入路径；文本模型是测试替身。
package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// narrScript 解说稿（与 volc_narr_voice.json 念的是同一份：那里「节目」识别成了「节木」、漏了「的」）。
const narrScript = "大家好，欢迎来到今天的节目。我们先看第一个画面。接下来是第二部分，内容很精彩。最后感谢大家的收看！"

// 编号：解说稿 1、配音 2、六段画面 3~8。
const narrReply = `{"lines":[{"sentence":"s1","clips":["3","4"],"take":"head"},{"sentence":"s2","clips":["5"]},
	{"sentence":"s3","clips":["6","7"],"take":"middle"},{"sentence":"s4","clips":["8"]}]}`

type narrEnv struct {
	planEnv
	voice int64
	tts   int64
	pid   int64
	tid   int64
}

// setupNarr 一条 20 秒的配音（文字转语音产出，已识别）+ 六段 4 秒的视频；script 非空时再连一段解说稿。
func setupNarr(t *testing.T, script string, withVoice bool) narrEnv {
	t.Helper()
	e := narrEnv{planEnv: setupPlan(t, config.Smartcut{})}
	e.text.replies = []string{narrReply}
	e.pid, e.tid = e.newSmartcut(t)
	nodes := []map[string]any{nodeWith("sc", "smartcut", nil)}
	var edges []map[string]any
	order := 1
	connect := func(n map[string]any) {
		nodes = append(nodes, n)
		edges = append(edges, edgeTo(n["id"].(string), order))
		order++
	}
	if script != "" {
		text := nodeWith("t1", "text", nil)
		text["params"] = map[string]any{"text": script}
		connect(text)
	}
	if withVoice {
		e.voice = e.mediaOf(t, ws, "audio/mpeg", 20000)
		e.tts = e.generation(t, ws, e.pid, "success", 9301, e.voice)
		connect(nodeWith("a1", "audio", genResult(e.tts)))
		e.exec(t, `insert into media_analysis (id, workspace_id, media_object_id, kind, engine, params_key, status, result)
			values ($1,$2,$3,'asr','asr-m','f0.zh-CN','success',$4::jsonb)`, e.next(), ws, e.voice,
			asrFixtureMeta(t, "volc_narr_voice.json"))
	}
	for i := 0; i < 6; i++ {
		connect(nodeWith(fmt.Sprintf("v%d", i+1), "video", mediaResult(e.mediaOf(t, ws, "video/mp4", 4000))))
	}
	e.setCanvas(t, e.pid, nodes, edges)
	if _, err := e.svc.Sync(e.ctx, ws, e.pid, e.tid, smartcutapp.SyncAutofill); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e narrEnv) view(t *testing.T) timelineapp.View {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e narrEnv) narrReq(t *testing.T) smartcutapp.PlanRequest {
	t.Helper()
	return smartcutapp.PlanRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid, Actor: smartcutapp.Actor{UserID: planUser},
		Mode: smartcutdomain.ModeNarr, Variants: 1, BaseVersion: e.view(t).Timeline.Version}
}

func (e narrEnv) calls(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `select count(*) from smartcut_llm_call where timeline_id = $1`, e.tid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// subtitlesOn 字幕文字（按顺序）与是否都跟随配音片段。
func subtitlesOn(t *testing.T, v timelineapp.View) ([]string, bool) {
	t.Helper()
	voiceClips := map[int64]bool{}
	for _, tr := range v.Tracks {
		if tr.Track.Kind == domain.TrackAudio && tr.Track.Role == smartcutdomain.RoleVoice {
			for _, c := range tr.Clips {
				voiceClips[c.ID] = true
			}
		}
	}
	var texts []string
	anchored := len(v.Subtitles) > 0
	for _, s := range v.Subtitles {
		texts = append(texts, s.Text)
		if a, ok := domain.ParseAnchor(s.Anchor); !ok || !voiceClips[a.ClipID] || s.Hidden {
			anchored = false
		}
	}
	return texts, anchored
}

// SC-AC-43：20 秒配音 + 解说稿 + 6 段素材 —— 成片总长等于配音（误差不超过一帧），字幕与解说稿逐字一致
// （「节木」按原稿出成「节目」、漏念的「的」照原稿补上，标点按原稿），每条字幕跟随配音片段。
func TestNarrPlan_端到端字幕与原稿一致(t *testing.T) {
	e := setupNarr(t, narrScript, true)
	res, err := e.svc.Plan(e.ctx, e.narrReq(t))
	if err != nil {
		t.Fatal(err)
	}
	v0 := res.Variants[0]
	if v0.UsedFallback || v0.Status != "success" {
		t.Fatalf("固定回包合规，不走兜底：%+v", v0)
	}
	for _, w := range v0.Warnings {
		if strings.Contains(w, "对不上") || strings.Contains(w, "没有找到") {
			t.Fatalf("同音错字与漏字不算对不上：%q", v0.Warnings)
		}
	}
	// 交给模型的是按句编号的配音稿（原稿文字 + 时长）。
	user := e.text.requests()[0].User
	for _, want := range []string{"## 配音稿（按句编号）", "- s1（", "大家好，欢迎来到今天的节目。", "- s4（"} {
		if !strings.Contains(user, want) {
			t.Fatalf("user 消息缺 %q：\n%s", want, user)
		}
	}

	v := e.view(t)
	frame := 1000 / v.Timeline.FPS
	if d := v.EffectiveMs - 20000; d > frame || d < -frame {
		t.Fatalf("成片总长等于配音 20 秒（误差不超过一帧 %dms）：%d", frame, v.EffectiveMs)
	}
	var voice []domain.Clip
	for _, tr := range v.Tracks {
		if tr.Track.Kind == domain.TrackAudio && tr.Track.Role == smartcutdomain.RoleVoice {
			voice = tr.Clips
		}
	}
	if len(voice) != 1 || voice[0].MediaID != e.voice || voice[0].StartMs != 0 || voice[0].SourceOutMs != 20000 {
		t.Fatalf("配音放进人声轨、从 0 起：%+v", voice)
	}
	texts, anchored := subtitlesOn(t, v)
	if strings.Join(texts, "") != narrScript || len(texts) != 4 {
		t.Fatalf("字幕与解说稿逐字一致：%q", texts)
	}
	if !anchored {
		t.Fatalf("每条字幕跟随配音片段：%+v", v.Subtitles)
	}
	if !strings.Contains(v0.Summary, "解说 4 句") {
		t.Fatalf("摘要：%s", v0.Summary)
	}
}

// 没有文本素材：原稿从配音那次文字转语音的快照还原（按连线顺序的文本输入 + 节点自己的提示词），不读画布上节点现在的文字。
func TestNarrPlan_没有文本素材时用配音快照的原稿(t *testing.T) {
	e := setupNarr(t, "", true)
	e.exec(t, `update generation set inputs_snapshot = $2::jsonb, params_snapshot = $3::jsonb where id = $1`, e.tts,
		`[{"kind":"text","order":1,"text":"大家好，欢迎来到今天的节目。我们先看第一个画面。"}]`,
		`{"prompt":"接下来是第二部分，内容很精彩。最后感谢大家的收看！"}`)
	if _, err := e.svc.Plan(e.ctx, e.narrReq(t)); err != nil {
		t.Fatal(err)
	}
	texts, anchored := subtitlesOn(t, e.view(t))
	if strings.Join(texts, "") != narrScript || !anchored {
		t.Fatalf("字幕用快照里的原稿：%q", texts)
	}
}

// 原稿与配音有一句对不上：那一段字幕用识别出来的文字，并在结果的警告里点名是原稿第几句。
func TestNarrPlan_对不上的句子退回识别文字(t *testing.T) {
	e := setupNarr(t, strings.Replace(narrScript, "我们先看第一个画面。", "这一句和配音完全不一样。", 1), true)
	res, err := e.svc.Plan(e.ctx, e.narrReq(t))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Variants[0].Warnings {
		if strings.Contains(w, "解说稿第 2 句「这一句和配音完全不一样」和配音对不上") && strings.Contains(w, "我们先看第一个画面") {
			found = true
		}
	}
	if !found {
		t.Fatalf("警告里点名：%q", res.Variants[0].Warnings)
	}
	texts, _ := subtitlesOn(t, e.view(t))
	if len(texts) != 4 || texts[1] != "我们先看第一个画面" || texts[0] != "大家好，欢迎来到今天的节目。" {
		t.Fatalf("那一段字幕用识别文字、其余照原稿：%q", texts)
	}
}

// SC-AC-43：没有配音时说明原因（42217 no_voice），不发起规划、不计入限流（没有调用记录）。
func TestNarrPlan_没有配音不发起规划(t *testing.T) {
	e := setupNarr(t, narrScript, false)
	_, err := e.svc.Plan(e.ctx, e.narrReq(t))
	if bizCode(err) != 42217 || !strings.Contains(err.Error(), "解说成片需要一条配音") {
		t.Fatalf("42217 并说明原因：%v", err)
	}
	if e.calls(t) != 0 || len(e.text.requests()) != 0 {
		t.Fatal("不发起规划、不占额度")
	}
}
