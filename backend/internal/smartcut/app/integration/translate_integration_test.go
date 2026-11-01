//go:build integration

// 字幕翻译（docs/agent/smartcut SCT-36，SC-F-125 / 126、SC-AC-53）：真库、真时间线写入路径，文本模型是测试替身。
package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// translateReply 按请求里的序号回英文（skip 里的原文不回）。
func translateReply(skip map[string]bool) func(in smartcutapp.TextRequest) string {
	return func(in smartcutapp.TextRequest) string {
		var items []map[string]any
		for _, line := range strings.Split(in.User, "\n") {
			var n int
			var text string
			if _, err := fmt.Sscanf(line, "%d. %s", &n, &text); err != nil || n == 0 || skip[text] {
				continue
			}
			items = append(items, map[string]any{"n": n, "text": "EN " + text})
		}
		b, _ := json.Marshal(map[string]any{"items": items})
		return string(b)
	}
}

// withChineseSubtitles 在 setupPlan 的时间线上放一条中文字幕轨、10 条字幕（每条 1 秒，间隔 0.5 秒）。返回字幕轨 ID。
func (e planEnv) withChineseSubtitles(t *testing.T) int64 {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, e.tid)
	if err != nil {
		t.Fatal(err)
	}
	c := timelineapp.ContentFromView(v)
	track := ""
	for i, tr := range c.Tracks {
		if tr.Kind == domain.TrackSubtitle {
			track = tr.ID
			c.Tracks[i].Lang = "zh-CN"
			break
		}
	}
	if track == "" {
		track = "tmp-sub"
		c.Tracks = append(c.Tracks, domain.ContentTrack{ID: track, Kind: domain.TrackSubtitle, Lang: "zh-CN"})
	}
	for i := 0; i < 10; i++ {
		c.Subtitles = append(c.Subtitles, domain.ContentSubtitle{ID: fmt.Sprintf("tmp-s%d", i), TrackID: track,
			StartMs: i * 1500, EndMs: i*1500 + 1000, Text: fmt.Sprintf("第%d句", i+1)})
	}
	res, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: e.tid, Editor: editor, Content: c})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range res.View.Tracks {
		if tr.Track.Kind == domain.TrackSubtitle {
			return tr.Track.ID
		}
	}
	t.Fatal("没有字幕轨")
	return 0
}

func (e planEnv) translateReq(t *testing.T, track int64) smartcutapp.TranslateRequest {
	return smartcutapp.TranslateRequest{WorkspaceID: ws, ProjectID: e.pid, TimelineID: e.tid, Actor: smartcutapp.Actor{UserID: planUser},
		SubtitleTrackID: track, TargetLang: "en", BaseVersion: e.currentVersion(t)}
}

// SC-AC-53：10 条中文字幕译成英文 —— 新字幕轨 10 条、时间与原文一致；原字幕不变；写入前有快照；积分不变（免费）。
func TestTranslatePlan_十条中文译成英文(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	e.text.respond = translateReply(nil)
	track := e.withChineseSubtitles(t)
	res, err := e.svc.Translate(e.ctx, e.translateReq(t, track))
	if err != nil {
		t.Fatal(err)
	}
	if res.Translated != 10 || len(res.Failed) != 0 || res.TrackID == track || res.TrackID == 0 || len(res.Timeline) == 0 {
		t.Fatalf("%+v", res)
	}
	v, _ := e.tl.Get(e.ctx, ws, e.tid)
	var lang string
	for _, tr := range v.Tracks {
		if tr.Track.ID == res.TrackID {
			lang = tr.Track.Lang
		}
	}
	orig, en := map[int]string{}, map[int]string{}
	for _, s := range v.Subtitles {
		switch s.TrackID {
		case track:
			orig[s.StartMs] = s.Text
		case res.TrackID:
			en[s.StartMs] = s.Text
		}
	}
	if lang != "en" || len(en) != 10 || len(orig) != 10 {
		t.Fatalf("新轨（英文）10 条、原轨 10 条: %s %d %d", lang, len(en), len(orig))
	}
	for at, text := range orig {
		if en[at] != "EN "+text {
			t.Fatalf("时间与原文一致: %d %q → %q", at, text, en[at])
		}
	}
	if n := e.count(t, `select count(*) from timeline_snapshot where timeline_id = $1 and reason = 'translate'`, e.tid); n != 1 {
		t.Fatalf("写入前存快照: %d", n)
	}
	var kind, status string
	if err := e.pool.QueryRow(e.ctx, `select kind, status from smartcut_llm_call where id = $1`, res.CallID).Scan(&kind, &status); err != nil ||
		kind != "translate" || status != "success" {
		t.Fatalf("调用记录: %s %s %v", kind, status, err)
	}
	if n := e.count(t, `select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("翻译免费，不动账本: %d", n)
	}
}

// SC-AC-53 / SC-F-126：部分条目失败 —— 说明是哪几条，新轨只放译出来的；替换原文时没译出来的原样不动。
func TestTranslatePlan_部分失败说明是哪几条(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	e.text.respond = translateReply(map[string]bool{"第3句": true, "第7句": true})
	track := e.withChineseSubtitles(t)
	res, err := e.svc.Translate(e.ctx, e.translateReq(t, track))
	if err != nil {
		t.Fatal(err)
	}
	if res.Translated != 8 || fmt.Sprint(res.Failed) != "[{3 AI 没有给出这一条的译文} {7 AI 没有给出这一条的译文}]" {
		t.Fatalf("%+v", res)
	}
	req := e.translateReq(t, track)
	req.Mode = smartcutdomain.TranslateReplace
	if _, err := e.svc.Translate(e.ctx, req); err != nil {
		t.Fatal(err)
	}
	v, _ := e.tl.Get(e.ctx, ws, e.tid)
	var texts []string
	for _, s := range v.Subtitles {
		if s.TrackID == track {
			texts = append(texts, s.Text)
		}
	}
	joined := strings.Join(texts, "|")
	if !strings.Contains(joined, "|第3句|") || !strings.Contains(joined, "|第7句|") || !strings.Contains(joined, "EN 第1句") {
		t.Fatalf("没译出来的原样不动: %s", joined)
	}
}

// SC-F-126：一条都没译出来时不改时间线（版本不动、没有快照），结果说清每一条。
func TestTranslatePlan_全部失败不改时间线(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	e.text.respond = func(smartcutapp.TextRequest) string { return "我翻译不了" }
	track := e.withChineseSubtitles(t)
	before := e.currentVersion(t)
	res, err := e.svc.Translate(e.ctx, e.translateReq(t, track))
	if err != nil || res.Translated != 0 || len(res.Failed) != 10 || len(res.Timeline) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if e.currentVersion(t) != before || e.count(t, `select count(*) from timeline_snapshot where reason = 'translate'`) != 0 {
		t.Fatal("时间线不改、没有快照")
	}
}

// 翻译与 AI 剪辑共用限流：AI 剪辑进行中时（同时进行上限 1）翻译回 42902。
func TestTranslatePlan_与AI剪辑共用限流(t *testing.T) {
	e := setupPlan(t, config.Smartcut{})
	track := e.withChineseSubtitles(t)
	e.text.block = make(chan struct{})
	done := make(chan error, 1)
	planReq := e.req(smartcutdomain.ModeMix)
	planReq.BaseVersion = e.currentVersion(t)
	go func() { _, err := e.svc.Plan(e.ctx, planReq); done <- err }()
	deadline := time.Now().Add(10 * time.Second)
	for e.count(t, `select count(*) from smartcut_llm_call where status = 'running'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("AI 剪辑没有开始")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err := e.svc.Translate(e.ctx, e.translateReq(t, track))
	if bizCode(err) != errcode.ConcurrencyLimited {
		t.Fatalf("同时进行上限 1：翻译应回 42902，实际 %v", err)
	}
	close(e.text.block)
	if err := <-done; err != nil {
		t.Fatalf("AI 剪辑照常完成: %v", err)
	}
}
