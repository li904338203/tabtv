//go:build integration

// 智能剪辑的时间线模型（docs/agent/smartcut SCT-03）：00067 新列的读写。
//
// ⚠️ jsonb 读回来与写进去不是逐字节相同（PG 会重排键、去掉空白），「读回一致」按语义比；
// 未知字段必须还在（GO-35）。
package integration

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

func sameJSON(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("读回的不是合法 JSON：%s", got)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatalf("期望值不是合法 JSON：%s", want)
	}
	return reflect.DeepEqual(a, b)
}

func TestIntegration_智能剪辑片段新列存得进读得出(t *testing.T) {
	r, pool, _ := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)

	transform := json.RawMessage(`{"x": -12.5, "y": 8, "scale": 1.2, "rotation": -15,
		"crop": {"left": 5, "futureCrop": "x"}, "futureTransform": {"keep": 1}}`)
	alternates := []domain.Alternate{
		{MediaID: 70, SourceInMs: 0, SourceOutMs: 3000, Kind: domain.AlternateOriginal},
		{MediaID: 71, SourceInMs: 100, SourceOutMs: 2200, Kind: domain.AlternateRegen, GenerationID: 9001},
	}
	v, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, []domain.Clip{
		{Idx: 1, MediaID: 101, SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100,
			Transform: transform, Origin: domain.OriginAI, SourceNodeID: "node_abc",
			Alternates: alternates, PendingGenerationID: 55},
		{Idx: 2, MediaID: 102, SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Clips(ctx, track.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := got[0]
	if !sameJSON(t, c.Transform, transform) {
		t.Errorf("变换没读回来（含未知字段）：%s", c.Transform)
	}
	if c.Origin != domain.OriginAI || c.SourceNodeID != "node_abc" || !reflect.DeepEqual(c.Alternates, alternates) {
		t.Errorf("来源 / 来源节点 / 备选没读回来：%+v", c)
	}
	// 进行中的生成只由生成链路置上：新段插入时一律为空。
	if c.PendingGenerationID != 0 {
		t.Errorf("插入时不该写进行中的生成，实际 %d", c.PendingGenerationID)
	}
	// 没设的那一段：来源默认 user、没有变换、没有备选。
	if got[1].Origin != domain.OriginUser || got[1].Transform != nil || len(got[1].Alternates) != 0 ||
		got[1].SourceNodeID != "" {
		t.Errorf("缺省值不对：%+v", got[1])
	}

	// 生成链路在编辑链路之外置上标记；随后一次普通保存（拿着读出来时的旧值）不能把它抹掉。
	if _, err := pool.Exec(ctx, `update timeline_clip set pending_generation_id = 77 where id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	got[0].SourceOutMs = 3500
	got[0].DurationMs = 3500
	if _, err := r.SaveClips(ctx, tl.ID, track.ID, v, repo.Editor{}, got); err != nil {
		t.Fatal(err)
	}
	after, _ := r.Clips(ctx, track.ID)
	if after[0].PendingGenerationID != 77 {
		t.Fatalf("一次裁切把进行中的生成标记抹掉了：%d", after[0].PendingGenerationID)
	}
	if after[0].SourceOutMs != 3500 || !sameJSON(t, after[0].Transform, transform) ||
		!reflect.DeepEqual(after[0].Alternates, alternates) {
		t.Fatalf("更新后新列应原样保留：%+v", after[0])
	}
}

// 存量片段（00067 之前写的行）读出来：来源 user、备选为空、没有变换。
func TestIntegration_存量片段读出默认值(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)
	// 只写 00011 就有的列，模拟迁移之前的行。
	if _, err := pool.Exec(ctx, `insert into timeline_clip (id, track_id, idx, media_object_id,
		source_in_ms, source_out_ms, start_ms, duration_ms) values ($1,$2,1,101,0,4000,0,4000)`,
		next(), track.ID); err != nil {
		t.Fatal(err)
	}
	got, err := r.Clips(ctx, track.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Origin != domain.OriginUser || len(got[0].Alternates) != 0 || got[0].Transform != nil ||
		got[0].PendingGenerationID != 0 {
		t.Fatalf("存量片段的缺省值不对：%+v", got[0])
	}
}

func TestIntegration_轨道字幕文字与时间线新列读得出(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	clipTrack := videoTrack(t, r, tl.ID)

	musicStyle := json.RawMessage(`{"duck": {"enabled": true, "depthDb": 12, "futureDuck": 1}, "futureMusic": "keep"}`)
	subStyle := json.RawMessage(`{"fontSizePct": 4.5, "color": "#FFFFFF", "bottomPct": 8, "futureSub": [1, 2]}`)
	var musicID, enID, textID int64 = next(), next(), next()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`update timeline_track set role = 'voice' where id = $1`, []any{clipTrack.ID}},
		{`update timeline_track set role = 'music', style = $2::jsonb where timeline_id = $1 and kind = 'audio'`,
			[]any{tl.ID, string(musicStyle)}},
		{`insert into timeline_track (id, timeline_id, kind, idx, name, role) values ($1,$2,'audio',2,'配乐 2','sfx')`,
			[]any{musicID, tl.ID}},
		{`insert into timeline_track (id, timeline_id, kind, idx, name, lang, is_hidden, style)
			values ($1,$2,'subtitle',2,'English','en',true,$3::jsonb)`, []any{enID, tl.ID, string(subStyle)}},
		{`insert into timeline_track (id, timeline_id, kind, idx, name) values ($1,$2,'text',1,'文字')`,
			[]any{textID, tl.ID}},
		{`update timeline set canvas_node_id = 'node_sc', is_arranged = false, last_editor_user_id = 42,
			last_editor_kind = 'agent', planned_fingerprint = 'p1', synced_fingerprint = 's1' where id = $1`, []any{tl.ID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s：%v", q.sql, err)
		}
	}

	got, err := r.Get(ctx, ws, tl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanvasNodeID != "node_sc" || got.IsArranged || got.LastEditorUserID != 42 ||
		got.LastEditorKind != "agent" || got.PlannedFingerprint != "p1" || got.SyncedFingerprint != "s1" {
		t.Fatalf("时间线新列没读回来：%+v", got)
	}
	if fresh := newTimeline(t, r); !fresh.IsArranged {
		// Create 没回填，读一次确认库里的默认值。
		again, _ := r.Get(ctx, ws, fresh.ID)
		if !again.IsArranged || again.CanvasNodeID != "" || again.LastEditorKind != "" {
			t.Fatalf("新建时间线的缺省值不对：%+v", again)
		}
	}

	tracks, err := r.Tracks(ctx, tl.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]repo.Track{}
	for _, tr := range tracks {
		byID[tr.ID] = tr
	}
	if byID[clipTrack.ID].Role != domain.RoleVoice {
		t.Errorf("画面轨可以标人声：%+v", byID[clipTrack.ID])
	}
	if tr := byID[musicID]; tr.Role != domain.RoleSfx || tr.Name != "配乐 2" {
		t.Errorf("第二条音频轨：%+v", tr)
	}
	en := byID[enID]
	if en.Lang != "en" || !en.IsHidden || !sameJSON(t, en.Style, subStyle) {
		t.Errorf("字幕轨的语言 / 隐藏 / 样式没读回来：%+v %s", en, en.Style)
	}
	if byID[textID].Kind != domain.TrackText {
		t.Errorf("文字轨：%+v", byID[textID])
	}
	for _, tr := range tracks {
		if tr.Kind == domain.TrackAudio && tr.Role == domain.RoleMusic && !sameJSON(t, tr.Style, musicStyle) {
			t.Errorf("配乐轨样式（含未知字段）没读回来：%s", tr.Style)
		}
	}
	one, err := r.Track(ctx, tl.ID, enID)
	if err != nil || one.Lang != "en" || !one.IsHidden {
		t.Errorf("按 ID 取轨道也要带新列：%+v %v", one, err)
	}

	// 字幕：轨道与锚点；文字层：样式与位置框。
	anchor := json.RawMessage(`{"clipId": "123", "srcStartMs": 0, "srcEndMs": 1800, "futureAnchor": true}`)
	textStyle := json.RawMessage(`{"fontSizePct": 8, "color": "#FFD400", "futureText": {"glow": true}}`)
	box := json.RawMessage(`{"xPct": 50, "yPct": 20, "anchor": "center", "futureBox": 7}`)
	if _, err := pool.Exec(ctx, `insert into timeline_subtitle (id, timeline_id, track_id, start_ms, end_ms, text, anchor)
		values ($1,$2,$3,0,1800,'Hello',$4::jsonb)`, next(), tl.ID, enID, string(anchor)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into timeline_text (id, timeline_id, track_id, start_ms, end_ms, text, style, box)
		values ($1,$2,$3,0,3000,'新品上市',$4::jsonb,$5::jsonb)`, next(), tl.ID, textID, string(textStyle), string(box)); err != nil {
		t.Fatal(err)
	}
	subs, err := r.Subtitles(ctx, tl.ID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("字幕：%+v %v", subs, err)
	}
	if subs[0].TrackID != enID || !sameJSON(t, subs[0].Anchor, anchor) {
		t.Errorf("字幕的轨道与锚点没读回来：%+v %s", subs[0], subs[0].Anchor)
	}
	if a, ok := domain.ParseAnchor(subs[0].Anchor); !ok || a.ClipID != 123 || a.SrcEndMs != 1800 {
		t.Errorf("锚点解析不对：%+v", a)
	}
	texts, err := r.Texts(ctx, tl.ID)
	if err != nil || len(texts) != 1 {
		t.Fatalf("文字层：%+v %v", texts, err)
	}
	if texts[0].TrackID != textID || texts[0].Text != "新品上市" || !sameJSON(t, texts[0].Style, textStyle) ||
		!sameJSON(t, texts[0].Box, box) {
		t.Errorf("文字层没读回来：%+v", texts[0])
	}
}

// 旧的整批字幕接口只动默认字幕轨：其它语言的字幕轨原样不动。
func TestIntegration_旧字幕接口只动默认字幕轨(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	enID := next()
	if _, err := pool.Exec(ctx, `insert into timeline_track (id, timeline_id, kind, idx, lang)
		values ($1,$2,'subtitle',2,'en')`, enID, tl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into timeline_subtitle (id, timeline_id, track_id, start_ms, end_ms, text)
		values ($1,$2,$3,0,2000,'Hello')`, next(), tl.ID, enID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveSubtitles(ctx, tl.ID, 1, repo.Editor{}, []repo.Subtitle{{StartMs: 0, EndMs: 2000, Text: "你好"}}); err != nil {
		t.Fatal(err)
	}
	subs, _ := r.Subtitles(ctx, tl.ID)
	texts := map[string]int64{}
	for _, s := range subs {
		texts[s.Text] = s.TrackID
	}
	if len(subs) != 2 || texts["Hello"] != enID || texts["你好"] == 0 || texts["你好"] == enID {
		t.Fatalf("英文轨的字幕应原样保留、中文写进默认轨：%+v", subs)
	}
}

// ⚠️ 新的 jsonb 列同样可能留着脏值：坏了当「没有」，不让整条时间线打不开。
func TestIntegration_坏的新列值不影响读取(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	tl := newTimeline(t, r)
	track := videoTrack(t, r, tl.ID)
	if _, err := r.SaveClips(ctx, tl.ID, track.ID, 1, repo.Editor{}, []domain.Clip{
		{Idx: 1, MediaID: 101, SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100},
	}); err != nil {
		t.Fatal(err)
	}
	textID := next()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`update timeline_clip set transform = '"zoom"'::jsonb, alternates = '{"a":1}'::jsonb where track_id = $1`,
			[]any{track.ID}},
		{`update timeline_track set style = '[1,2]'::jsonb where id = $1`, []any{track.ID}},
		{`insert into timeline_subtitle (id, timeline_id, track_id, start_ms, end_ms, text, anchor)
			select $1, $2, k.id, 0, 1000, '坏锚点', '{"clipId":5}'::jsonb from timeline_track k
			where k.timeline_id = $2 and k.kind = 'subtitle'`, []any{next(), tl.ID}},
		{`insert into timeline_track (id, timeline_id, kind, idx) values ($1,$2,'text',1)`, []any{textID, tl.ID}},
		{`insert into timeline_text (id, timeline_id, track_id, start_ms, end_ms, text, style, box)
			values ($1,$2,$3,0,1000,'坏样式','"bad"'::jsonb,'7'::jsonb)`, []any{next(), tl.ID, textID}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s：%v", q.sql, err)
		}
	}
	clips, err := r.Clips(ctx, track.ID)
	if err != nil || clips[0].Transform != nil || clips[0].Alternates != nil {
		t.Fatalf("坏的变换与备选应当当成没有：%+v %v", clips, err)
	}
	tr, err := r.Track(ctx, tl.ID, track.ID)
	if err != nil || tr.Style != nil {
		t.Fatalf("坏的轨道样式应当当成没有：%+v %v", tr, err)
	}
	subs, err := r.Subtitles(ctx, tl.ID)
	if err != nil || len(subs) != 1 || subs[0].Anchor != nil {
		t.Fatalf("坏锚点应当当成不跟随：%+v %v", subs, err)
	}
	texts, err := r.Texts(ctx, tl.ID)
	if err != nil || len(texts) != 1 || texts[0].Style != nil || texts[0].Box != nil {
		t.Fatalf("坏的文字样式与位置框应当当成没有：%+v %v", texts, err)
	}
}
