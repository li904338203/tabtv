//go:build integration

// 识别字幕（docs/agent/smartcut SCT-36，SC-F-120、SC-AC-19）：识别时选了「生成字幕」，识别完成后由回写把字幕写进
// 时间线（快照 subtitles、编辑者 system）；已有识别结果的素材在请求时当场写；写入撞上版本冲突时重读重放。
package integration

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	smartcutapp "github.com/li904338203/tabtv/backend/internal/smartcut/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// twoLines 识别结果：「大家好」100~900、「今天讲剪辑」1500~2500（字级）。
const twoLines = `{"granularity":"char","durationMs":10000,"utterances":[
	{"text":"大家好","startMs":100,"endMs":900,"words":[{"text":"大","startMs":100,"endMs":300},{"text":"家","startMs":300,"endMs":500},{"text":"好","startMs":500,"endMs":900}]},
	{"text":"今天讲剪辑","startMs":1500,"endMs":2500,"words":[{"text":"今","startMs":1500,"endMs":1700},{"text":"天","startMs":1700,"endMs":1900},{"text":"讲","startMs":1900,"endMs":2100},{"text":"剪","startMs":2100,"endMs":2300},{"text":"辑","startMs":2300,"endMs":2500}]}]}`

// bumpOnce 第一次写入之前先替「另一个窗口」存一次（推进版本），让这次写入撞上版本冲突。
type bumpOnce struct {
	inner interface {
		Content(ctx context.Context, ws, tid int64) (domain.Content, error)
		Save(ctx context.Context, in smartcutapp.SavePlanInput) (smartcutapp.SavedPlan, error)
	}
	bump  func()
	saves atomic.Int32
}

func (b *bumpOnce) Content(ctx context.Context, ws, tid int64) (domain.Content, error) {
	return b.inner.Content(ctx, ws, tid)
}

func (b *bumpOnce) Save(ctx context.Context, in smartcutapp.SavePlanInput) (smartcutapp.SavedPlan, error) {
	if b.saves.Add(1) == 1 {
		b.bump()
	}
	return b.inner.Save(ctx, in)
}

func (e speechEnv) subtitleView(t *testing.T, tid int64) timelineapp.View {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, tid)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// 识别完成后写字幕：剪辑台在识别期间存过一次（版本变了）也照样写进去（重读重放），写入前有快照、编辑者记 system；
// 裁掉片段开头 1 秒后落在被裁区间的那条隐藏、其余前移 1 秒，恢复区间后重新出现（SC-AC-19 端到端）。
func TestBinder_识别完成后写字幕且冲突重放(t *testing.T) {
	e := setupSpeech(t)
	e.up.meta, e.up.text = twoLines, "大家好今天讲剪辑"
	v, _ := e.video(t, 10000, "success")
	pid, tid := e.timelineWith(t, v)
	ports := wiring.SmartcutSpeechPorts(wiring.SmartcutSpeechInput{Catalog: catalogapp.New(catalogrepo.New(e.pool), e.next),
		Gen: e.gen, Media: e.media, Timeline: e.tl})
	conflict := &bumpOnce{inner: ports.Timelines, bump: func() {
		cur := e.subtitleView(t, tid)
		if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: tid, Editor: editor,
			Content: timelineapp.ContentFromView(cur)}); err != nil {
			t.Errorf("另一个窗口的保存: %v", err)
		}
	}}
	ports.Timelines = conflict
	e.svc.SetTranscribePorts(ports)

	req := e.req(pid, tid, "subtitles-after-asr-1")
	req.Subtitles = true
	res, err := e.svc.Transcribe(e.ctx, req)
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("受理: %+v %v", res, err)
	}
	before := e.subtitleView(t, tid).Timeline.Version
	if out, err := e.executor.Execute(e.ctx, res.Items[0].GenerationID); err != nil || out != genapp.OutcomeDone {
		t.Fatalf("执行: %v %v", out, err)
	}
	if n := conflict.saves.Load(); n != 2 {
		t.Fatalf("第一次撞上冲突、重读之后再写一次：%d 次", n)
	}
	view := e.subtitleView(t, tid)
	if view.Timeline.Version != before+2 || view.Timeline.LastEditorKind != "system" {
		t.Fatalf("另一个窗口存了一次、字幕写了一次，编辑者记 system: v%d→v%d %q", before, view.Timeline.Version,
			view.Timeline.LastEditorKind)
	}
	var texts []string
	start := map[string]int{}
	clip := clipsOfView(view, domain.TrackVideo)[0].ID
	for _, s := range view.Subtitles {
		texts = append(texts, s.Text)
		start[s.Text] = s.StartMs
		if a, ok := domain.ParseAnchor(s.Anchor); !ok || a.ClipID != clip {
			t.Fatalf("字幕跟随片段: %+v", s)
		}
	}
	if strings.Join(texts, "|") != "大家好|今天讲剪辑" || start["今天讲剪辑"] != 1500 {
		t.Fatalf("逐句、成片时间: %v %v", texts, start)
	}
	if n := e.count(t, `select count(*) from timeline_snapshot where timeline_id = $1 and reason = 'subtitles'`, tid); n != 1 {
		t.Fatalf("写入前存快照: %d", n)
	}

	// 裁掉开头 1 秒 → 「大家好」隐藏、「今天讲剪辑」前移 1 秒；恢复区间 → 重新出现。
	trim := func(in int) timelineapp.View {
		c := timelineapp.ContentFromView(e.subtitleView(t, tid))
		for i := range c.Tracks {
			if c.Tracks[i].Kind == domain.TrackVideo {
				c.Tracks[i].Clips[0].SourceInMs = in
			}
		}
		if _, err := e.tl.SaveContent(e.ctx, timelineapp.SaveContentInput{WorkspaceID: ws, TimelineID: tid, Editor: editor,
			Content: c}); err != nil {
			t.Fatal(err)
		}
		return e.subtitleView(t, tid)
	}
	cut := trim(1000)
	for _, s := range cut.Subtitles {
		if (s.Text == "大家好") != s.Hidden || (s.Text == "今天讲剪辑" && s.StartMs != 500) {
			t.Fatalf("裁掉开头 1 秒: %+v", cut.Subtitles)
		}
	}
	back := trim(0)
	for _, s := range back.Subtitles {
		if s.Hidden || s.StartMs != start[s.Text] {
			t.Fatalf("恢复区间后重新出现: %+v", back.Subtitles)
		}
	}
}

// 已有识别结果的素材：请求时就写字幕（不会再有识别完成来触发它）；不再收费、不建任务。
func TestTranscribe_已有识别结果时当场写字幕(t *testing.T) {
	e := setupSpeech(t)
	e.up.meta, e.up.text = twoLines, "大家好今天讲剪辑"
	v, _ := e.video(t, 10000, "success")
	pid, tid := e.timelineWith(t, v)
	res, err := e.svc.Transcribe(e.ctx, e.req(pid, tid, "subtitles-cached-1"))
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("受理: %+v %v", res, err)
	}
	if out, err := e.executor.Execute(e.ctx, res.Items[0].GenerationID); err != nil || out != genapp.OutcomeDone {
		t.Fatalf("执行: %v %v", out, err)
	}
	if n := len(e.subtitleView(t, tid).Subtitles); n != 0 {
		t.Fatalf("没选「生成字幕」时不写: %d", n)
	}
	before := e.balance(t)

	pid2, tid2 := e.timelineWith(t, v)
	req := e.req(pid2, tid2, "subtitles-cached-2")
	req.Subtitles = true
	res2, err := e.svc.Transcribe(e.ctx, req)
	if err != nil || len(res2.Items) != 0 || len(res2.Skipped) != 1 || res2.SubtitleError != "" {
		t.Fatalf("命中缓存、不受理: %+v %v", res2, err)
	}
	view := e.subtitleView(t, tid2)
	if len(view.Subtitles) != 2 || view.Timeline.LastEditorKind != "system" || e.balance(t) != before {
		t.Fatalf("当场写字幕、不收费: %+v", view.Subtitles)
	}
	// 再点一次：照样换掉这几段的字幕（不叠第二份）。
	if _, err := e.svc.Transcribe(e.ctx, req); err != nil {
		t.Fatal(err)
	}
	if n := len(e.subtitleView(t, tid2).Subtitles); n != 2 {
		t.Fatalf("重新识别字幕是换掉，不是叠加: %d", n)
	}
}
