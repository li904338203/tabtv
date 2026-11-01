package app

import (
	"context"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 快照的列出与恢复（docs/agent/smartcut SCD-18、SC-F-144~146）。

// ListSnapshots 快照列表（不含正文），新的在前。
func (s *Service) ListSnapshots(ctx context.Context, workspaceID, timelineID int64) ([]repo.Snapshot, error) {
	if _, err := s.repo.Get(ctx, workspaceID, timelineID); err != nil {
		return nil, translate(err)
	}
	list, err := s.repo.ListSnapshots(ctx, timelineID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// RestoreInput 恢复到某份快照。BaseVersion 是客户端看到的当前版本（乐观锁）。
type RestoreInput struct {
	WorkspaceID int64
	TimelineID  int64
	SnapshotID  int64
	BaseVersion int
	Editor      repo.Editor
}

// MissingClip 快照里引用、但素材已被删除的片段（SC-AC-60）。ClipID 是恢复之后的正式 ID。
type MissingClip struct {
	ClipID    string
	MediaID   int64
	TrackKind string
}

// RestoreResult 恢复后的时间线、恢复前存下的那份快照（再撤销一次就回到恢复前）、缺素材的片段。
type RestoreResult struct {
	View       View
	SnapshotID int64
	Missing    []MissingClip
}

// RestoreSnapshot 把时间线恢复成某份快照的内容。
//
// ⚠️ 走整份保存的同一条写路径（不另写一份写库代码），并且先给当前内容存一份 `restore` 快照。
// 快照之后被删掉的对象，它们的正式 ID 在库里已经不存在了：原样写回会被当成「别的时间线的对象」拒掉，
// 所以先换成临时 ID（引用它们的字幕锚点、所属轨道一起换），保存时分配新的正式 ID。
// 素材已被删除的片段**照常恢复**，列进 missing（导出预检会拦下它们，SCK-18）。
func (s *Service) RestoreSnapshot(ctx context.Context, in RestoreInput) (RestoreResult, error) {
	t, err := s.repo.Get(ctx, in.WorkspaceID, in.TimelineID)
	if err != nil {
		return RestoreResult{}, translate(err)
	}
	snap, err := s.repo.GetSnapshot(ctx, t.ID, in.SnapshotID)
	if err != nil {
		return RestoreResult{}, translate(err)
	}
	c, err := domain.DecodeContent(snap.Content)
	if err != nil {
		// 快照是这条时间线自己存下的；对不上现在的规则多半是规则后来收紧了。说清是哪一处。
		return RestoreResult{}, ContentError(err)
	}
	cur, err := s.Get(ctx, in.WorkspaceID, t.ID)
	if err != nil {
		return RestoreResult{}, err
	}
	if cur.Timeline.Version != in.BaseVersion {
		return RestoreResult{}, s.conflictError(ctx, t.ID, in.Editor)
	}
	c = reviveIDs(c, existingIDs(cur))
	c.Version = in.BaseVersion
	missing, skip := s.missingMedia(ctx, in.WorkspaceID, c)

	res, err := s.SaveContent(ctx, SaveContentInput{
		WorkspaceID: in.WorkspaceID, TimelineID: t.ID, Editor: in.Editor, Content: c,
		Reason: ReasonRestore, Snapshot: &SnapshotSpec{Reason: repo.SnapshotRestore}, skipMedia: skip,
	})
	if err != nil {
		return RestoreResult{}, err
	}
	for i := range missing {
		if id, ok := res.IDMap[missing[i].ClipID]; ok {
			missing[i].ClipID = id
		}
	}
	return RestoreResult{View: res.View, SnapshotID: res.SnapshotID, Missing: missing}, nil
}

// existingIDs 当前时间线上所有对象的 ID（轨道、片段、字幕、文字）。
func existingIDs(v View) map[string]bool {
	out := map[string]bool{}
	for _, tr := range v.Tracks {
		out[strconv.FormatInt(tr.Track.ID, 10)] = true
		for _, c := range tr.Clips {
			out[strconv.FormatInt(c.ID, 10)] = true
		}
	}
	for _, sub := range v.Subtitles {
		out[strconv.FormatInt(sub.ID, 10)] = true
	}
	for _, x := range v.Texts {
		out[strconv.FormatInt(x.ID, 10)] = true
	}
	return out
}

// reviveIDs 把快照里「现在已经不在」的正式 ID 换成临时 ID（`tmp-r` + 原 ID），
// 引用它们的地方（字幕与文字的所属轨道、字幕锚点）一起换。
//
// 锚点指向的片段既不在现在、也不在快照里（快照之前就删了）时原样留着：那是失效锚点，本来就对不上。
func reviveIDs(c domain.Content, exist map[string]bool) domain.Content {
	renamed := map[string]string{}
	revive := func(id string) string {
		if domain.IsTempID(id) || exist[id] {
			return id
		}
		ref := "tmp-r" + id
		renamed[id] = ref
		return ref
	}
	for i := range c.Tracks {
		t := &c.Tracks[i]
		t.ID = revive(t.ID)
		for j := range t.Clips {
			t.Clips[j].ID = revive(t.Clips[j].ID)
		}
	}
	rename := func(id string) string {
		if ref, ok := renamed[id]; ok {
			return ref
		}
		return id
	}
	for i := range c.Subtitles {
		sub := &c.Subtitles[i]
		sub.ID = revive(sub.ID)
		sub.TrackID = rename(sub.TrackID)
		if a, ok := anchorClipRef(sub.Anchor); ok {
			if ref, renamedClip := renamed[a]; renamedClip {
				sub.Anchor = rewriteAnchorRef(sub.Anchor, ref)
			}
		}
	}
	for i := range c.Texts {
		x := &c.Texts[i]
		x.ID = revive(x.ID)
		x.TrackID = rename(x.TrackID)
	}
	return c
}

// missingMedia 内容里素材已经取不到的片段（被删除，或不再属于本工作区）与要跳过核对的素材。
// 备选的素材取不到时只跳过核对、不算缺失（它不在画面里）。
func (s *Service) missingMedia(ctx context.Context, workspaceID int64, c domain.Content) ([]MissingClip, map[int64]bool) {
	skip := map[int64]bool{}
	known := map[int64]bool{}
	gone := func(mediaID int64) bool {
		if ok, seen := known[mediaID]; seen {
			return !ok
		}
		ok := s.media != nil
		if ok {
			_, err := s.media.Lookup(ctx, workspaceID, mediaID)
			ok = err == nil
		}
		known[mediaID] = ok
		if !ok {
			skip[mediaID] = true
		}
		return !ok
	}
	var missing []MissingClip
	for _, t := range c.Tracks {
		for _, cl := range t.Clips {
			if gone(cl.MediaID) {
				missing = append(missing, MissingClip{ClipID: cl.ID, MediaID: cl.MediaID, TrackKind: t.Kind})
			}
			for _, a := range cl.Alternates {
				gone(a.MediaID)
			}
		}
	}
	return missing, skip
}

// ContentFromView 把一条时间线写成整份内容（快照正文、恢复、AI 剪辑的基线都用这一份形状）。
//
// 画面轨的片段不带 startMs（保存时按顺序与转场重算）；没生效的转场不带；溯源原样带上（保存时忽略，留作追溯）。
func ContentFromView(v View) domain.Content {
	c := domain.Content{
		Version: v.Timeline.Version, AspectRatio: v.Timeline.AspectRatio,
		FPS: v.Timeline.FPS, Width: v.Timeline.Width, Height: v.Timeline.Height,
	}
	for _, tv := range v.Tracks {
		vol := tv.Track.Volume
		ct := domain.ContentTrack{
			ID: strconv.FormatInt(tv.Track.ID, 10), Kind: tv.Track.Kind, Name: tv.Track.Name,
			Role: tv.Track.Role, Lang: tv.Track.Lang, IsHidden: tv.Track.IsHidden, IsMuted: tv.Track.IsMuted,
			Volume: &vol, Style: tv.Track.Style,
		}
		for _, cl := range tv.Clips {
			speed, volume := cl.Speed, cl.Volume
			cc := domain.ContentClip{
				ID: strconv.FormatInt(cl.ID, 10), MediaID: cl.MediaID,
				SourceInMs: cl.SourceInMs, SourceOutMs: cl.SourceOutMs, Speed: &speed, Volume: &volume,
				FadeInMs: cl.FadeInMs, FadeOutMs: cl.FadeOutMs, Transform: cl.Transform, Origin: cl.Origin,
				SourceNodeID: cl.SourceNodeID, Alternates: cl.Alternates, Anchor: cl.Anchor,
			}
			if tv.Track.Kind != domain.TrackVideo {
				start := cl.StartMs
				cc.StartMs = &start
			}
			if cl.TransitionIn.IsActive() {
				tin := cl.TransitionIn
				cc.TransitionIn = &tin
			}
			if cl.TransitionOut.IsActive() {
				tout := cl.TransitionOut
				cc.TransitionOut = &tout
			}
			if cl.SourceShotID != 0 {
				cc.SourceShotID = strconv.FormatInt(cl.SourceShotID, 10)
			}
			ct.Clips = append(ct.Clips, cc)
		}
		c.Tracks = append(c.Tracks, ct)
	}
	for _, sub := range v.Subtitles {
		c.Subtitles = append(c.Subtitles, domain.ContentSubtitle{
			ID: strconv.FormatInt(sub.ID, 10), TrackID: strconv.FormatInt(sub.TrackID, 10),
			StartMs: sub.StartMs, EndMs: sub.EndMs, Text: sub.Text,
			Style: domain.ObjectOrNil(sub.Style), Anchor: sub.Anchor,
		})
	}
	for _, x := range v.Texts {
		c.Texts = append(c.Texts, domain.ContentText{
			ID: strconv.FormatInt(x.ID, 10), TrackID: strconv.FormatInt(x.TrackID, 10),
			StartMs: x.StartMs, EndMs: x.EndMs, Text: x.Text, Style: x.Style, Box: x.Box,
		})
	}
	return c
}
