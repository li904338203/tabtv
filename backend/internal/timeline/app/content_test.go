package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

func TestRewriteAnchor_只改临时ID并保留未知字段(t *testing.T) {
	idMap := map[string]int64{"tmp-b": 42}
	got := rewriteAnchor(json.RawMessage(`{"clipId":"tmp-b","srcStartMs":10,"srcEndMs":20,"future":{"x":1}}`), idMap)
	a, ok := domain.ParseAnchor(got)
	if !ok || a.ClipID != 42 || a.SrcStartMs != 10 || !strings.Contains(string(got), `"future"`) {
		t.Fatalf("应改写成 42 并保留未知字段：%s", got)
	}
	// 指向正式 ID（含已被删掉的失效锚点）的原样不动。
	stale := json.RawMessage(`{"clipId":"777","srcStartMs":0,"srcEndMs":5}`)
	if string(rewriteAnchor(stale, idMap)) != string(stale) {
		t.Fatal("正式 ID 的锚点应原样保留")
	}
	if rewriteAnchor(nil, idMap) != nil {
		t.Fatal("没有锚点就是没有")
	}
}

func TestBySelf_只认本人亲手(t *testing.T) {
	me := repo.Editor{UserID: 7, Kind: repo.EditorUser}
	cases := []struct {
		st   repo.EditorState
		ed   repo.Editor
		want bool
	}{
		{repo.EditorState{EditorID: 7, EditorKind: repo.EditorUser}, me, true},
		{repo.EditorState{EditorID: 8, EditorKind: repo.EditorUser}, me, false},
		// Agent / MCP 的写入也带着本人的 ID：不算「本人另一个窗口」。
		{repo.EditorState{EditorID: 7, EditorKind: repo.EditorAgent}, me, false},
		{repo.EditorState{EditorID: 7, EditorKind: repo.EditorMCP}, me, false},
		// 这一次不是本人亲手（Agent 撞上冲突）也不算。
		{repo.EditorState{EditorID: 7, EditorKind: repo.EditorUser}, repo.Editor{UserID: 7, Kind: repo.EditorAgent}, false},
		// 编辑者说不清（两列为空）：不猜。
		{repo.EditorState{}, me, false},
	}
	for i, c := range cases {
		if got := bySelf(c.st, c.ed); got != c.want {
			t.Errorf("第 %d 条：期望 %v，实际 %v", i, c.want, got)
		}
	}
	if msg := conflictMessage(repo.EditorState{EditorKind: repo.EditorAgent}, false); !strings.Contains(msg, "Agent") {
		t.Errorf("Agent 改的要说出来：%s", msg)
	}
}

// contentWrite：临时 ID 分到正式 ID、引用它们的地方一起改写；新片段剥掉溯源；落点按 Layout。
func TestContentWrite_分配ID改写引用并剥掉溯源(t *testing.T) {
	next := int64(1000)
	s := &Service{repo: repo.New(nil, func() int64 { next++; return next })}
	raw := []byte(`{"version":3,"aspectRatio":"9:16","fps":30,"width":1080,"height":1920,
	  "tracks":[
	    {"id":"11","kind":"video","clips":[
	      {"id":"21","mediaId":"77","sourceInMs":0,"sourceOutMs":4000,"sourceShotId":"5"},
	      {"id":"tmp-c","mediaId":"78","sourceInMs":0,"sourceOutMs":4000,"sourceShotId":"5",
	       "transitionIn":{"kind":"fade","durationMs":1000}}]},
	    {"id":"tmp-t","kind":"subtitle"},
	    {"id":"tmp-a","kind":"audio","clips":[
	      {"id":"tmp-m2","mediaId":"88","startMs":5000,"sourceInMs":0,"sourceOutMs":1000},
	      {"id":"tmp-m1","mediaId":"88","startMs":0,"sourceInMs":0,"sourceOutMs":1000}]}],
	  "subtitles":[{"id":"tmp-s","trackId":"tmp-t","startMs":0,"endMs":10,"text":"跟随",
	    "anchor":{"clipId":"tmp-c","srcStartMs":0,"srcEndMs":2000}}]}`)
	c, err := domain.DecodeContent(raw)
	if err != nil {
		t.Fatal(err)
	}
	w, idMap, pathOf := s.contentWrite(9, repo.Editor{UserID: 1, Kind: repo.EditorUser}, c)
	if len(idMap) != 6 || w.Version != 3 || w.Tracks[0].New || !w.Tracks[1].New {
		t.Fatalf("六个临时 ID 都要分配、已有轨道不是新建：%v %+v", idMap, w.Tracks)
	}
	video := w.Tracks[0].Clips
	if video[0].New || video[0].Clip.SourceShotID != 0 || !video[1].New || video[1].Clip.SourceShotID != 0 {
		t.Fatalf("溯源一律不由客户端写：%+v", video)
	}
	if video[1].Clip.StartMs != 3000 || video[1].Clip.ID != idMap["tmp-c"] {
		t.Fatalf("第二段成片起点 3000：%+v", video[1].Clip)
	}
	// 音频轨按起点排序重编 idx。
	audio := w.Tracks[2].Clips
	if audio[0].Clip.ID != idMap["tmp-m1"] || audio[0].Clip.Idx != 1 || audio[1].Clip.StartMs != 5000 {
		t.Fatalf("音频轨按起点放置：%+v", audio)
	}
	sub := w.Subtitles[0]
	a, ok := domain.ParseAnchor(sub.Anchor)
	if !ok || a.ClipID != idMap["tmp-c"] || sub.TrackID != idMap["tmp-t"] || sub.StartMs != 3000 || sub.EndMs != 5000 {
		t.Fatalf("字幕的轨道与锚点改写成正式 ID、存下成片时间 3000~5000：%+v %s", sub.Subtitle, sub.Anchor)
	}
	if pathOf[21] != "tracks[0].clips[0].id" || pathOf[11] != "tracks[0].id" ||
		pathOf[idMap["tmp-s"]] != "subtitles[0].id" {
		t.Fatalf("正式 ID 要能映射回字段路径：%v", pathOf)
	}
}
