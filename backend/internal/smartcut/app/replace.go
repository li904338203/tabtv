package app

import (
	"context"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 素材过期与原位替换（docs/agent/smartcut SCT-30，SC-F-150~153）：上游重新生成之后，只把同一来源节点的新产物
// 换进引用旧产物的片段 —— 保留裁切、变速、转场与变换，原素材连同原区间进备选（可以切回）。**不自动重剪、不自动重新导出**。

// ReplaceRequest 原位替换（POST .../sync {mode: replaceChanged, baseVersion}）。
type ReplaceRequest struct {
	WorkspaceID int64
	ProjectID   int64
	TimelineID  int64
	BaseVersion int
	Actor       Actor
}

// Shortened 新素材比取用区间短、被截到素材长度的片段。
type Shortened struct {
	ClipID int64
	FromMs int
	ToMs   int
}

// ReplaceChanged 原位替换变了的素材。没有要换的时是空操作（不进位、不存快照）；有的话写入前存快照 `replace_changed`，
// 按 baseVersion 做 CAS（冲突回 40902），写入后把「剪辑依据的素材」记成现在这一份（「素材有变化」随之消失）。
func (s *Service) ReplaceChanged(ctx context.Context, req ReplaceRequest) (SyncResult, error) {
	if s.plan.Timelines == nil {
		return SyncResult{}, httpx.Errf(errcode.Internal, "原位替换链路未接入")
	}
	if err := s.projectWritable(ctx, req.WorkspaceID, req.ProjectID, "替换智能剪辑的素材"); err != nil {
		return SyncResult{}, err
	}
	nodeID, err := s.boundNode(ctx, req.WorkspaceID, req.ProjectID, req.TimelineID)
	if err != nil {
		return SyncResult{}, err
	}
	sources, err := s.inputs.Canvas.Sources(ctx, req.WorkspaceID, req.ProjectID, nodeID)
	if err != nil {
		return SyncResult{}, canvasError(err)
	}
	lookup, err := s.lookup(ctx, req.WorkspaceID, req.ProjectID, sources)
	if err != nil {
		return SyncResult{}, err
	}
	editor := Editor{UserID: req.Actor.UserID, Kind: "user"}
	base, err := s.plan.Timelines.Content(ctx, req.WorkspaceID, req.TimelineID)
	if err != nil {
		return SyncResult{}, err
	}
	if req.BaseVersion > 0 && base.Version != req.BaseVersion {
		return SyncResult{}, s.plan.Timelines.Conflict(ctx, req.TimelineID, editor)
	}
	items, omitted := domain.ResolveInputs(sources, lookup, contentUses(base))
	fp := domain.InputFingerprint(items)
	plan := domain.PlanReplaceChanged(items, contentClipUses(base))
	res := SyncResult{Inputs: items, Omitted: omitted, Fingerprint: fp, TimelineVersion: base.Version,
		AppendedClipIDs: []int64{}, DisconnectedClipIDs: []int64{}, ReplacedClipIDs: []int64{}, Shortened: []Shortened{}}
	if len(plan) == 0 {
		res.UnusedCount = domain.UnusedCount(items)
		return res, nil
	}
	next, shortened := applyReplacements(base, plan)
	saved, err := s.plan.Timelines.Save(ctx, SavePlanInput{WorkspaceID: req.WorkspaceID, TimelineID: req.TimelineID,
		Editor: editor, Content: next, Reason: "sync", SnapshotReason: "replace_changed", Fingerprint: fp})
	if err != nil {
		return SyncResult{}, err
	}
	for _, r := range plan {
		if id, err := strconv.ParseInt(r.ClipID, 10, 64); err == nil {
			res.ReplacedClipIDs = append(res.ReplacedClipIDs, id)
		}
	}
	res.Shortened = shortened
	res.TimelineVersion = saved.Version
	// 换完之后再解析一遍：新产物成了「用到了」，不再是「变了」。
	res.Inputs, res.Omitted = domain.ResolveInputs(sources, lookup, contentUses(next))
	res.UnusedCount = domain.UnusedCount(res.Inputs)
	return res, nil
}

func contentUses(c timelinedomain.Content) []domain.UsedClip {
	var out []domain.UsedClip
	for _, t := range c.Tracks {
		for _, cl := range t.Clips {
			out = append(out, domain.UsedClip{SourceNodeID: cl.SourceNodeID, MediaID: strconv.FormatInt(cl.MediaID, 10)})
		}
	}
	return out
}

func contentClipUses(c timelinedomain.Content) []domain.ClipUse {
	var out []domain.ClipUse
	for _, t := range c.Tracks {
		for _, cl := range t.Clips {
			out = append(out, domain.ClipUse{ID: cl.ID, SourceNodeID: cl.SourceNodeID, MediaID: strconv.FormatInt(cl.MediaID, 10)})
		}
	}
	return out
}

// applyReplacements 按计划换素材：取用区间、变速、转场、变换不动；新素材更短时把终点截到素材长度（截完太短就整体往前挪，
// 尽量保住原来的长度），淡入淡出与转场按新长度收短；原素材连同原区间进备选。
func applyReplacements(base timelinedomain.Content, plan []domain.ReplaceClip) (timelinedomain.Content, []Shortened) {
	byClip := map[string]domain.ReplaceClip{}
	for _, r := range plan {
		byClip[r.ClipID] = r
	}
	out := base
	out.Tracks = make([]timelinedomain.ContentTrack, len(base.Tracks))
	var shortened []Shortened
	for i, t := range base.Tracks {
		t.Clips = append([]timelinedomain.ContentClip(nil), t.Clips...)
		for k := range t.Clips {
			c := &t.Clips[k]
			r, ok := byClip[c.ID]
			if !ok {
				continue
			}
			newID, err := strconv.ParseInt(r.NewMediaID, 10, 64)
			if err != nil || newID <= 0 {
				continue
			}
			c.Alternates = timelinedomain.PushAlternate(c.Alternates, timelinedomain.Alternate{MediaID: c.MediaID,
				SourceInMs: c.SourceInMs, SourceOutMs: c.SourceOutMs, Kind: timelinedomain.AlternateOriginal})
			c.MediaID = newID
			if d := r.NewDurationMs; d >= timelinedomain.MinClipMs && c.SourceOutMs > d {
				from, length := c.SourceOutMs, c.SourceOutMs-c.SourceInMs
				c.SourceOutMs = d
				speed := timelinedomain.DefaultSpeed
				if c.Speed != nil && *c.Speed > 0 {
					speed = *c.Speed
				}
				if minSrc := (timelinedomain.MinClipMs*speed + 99) / 100; c.SourceOutMs-c.SourceInMs < minSrc {
					c.SourceInMs = max(0, c.SourceOutMs-length)
				}
				if id, err := strconv.ParseInt(c.ID, 10, 64); err == nil {
					shortened = append(shortened, Shortened{ClipID: id, FromMs: from, ToMs: c.SourceOutMs})
				}
			}
			if play := c.PlayLen(); c.FadeInMs+c.FadeOutMs > play {
				c.FadeInMs, c.FadeOutMs = min(c.FadeInMs, play/2), min(c.FadeOutMs, play/2)
			}
		}
		if t.Kind == timelinedomain.TrackVideo {
			clampTransitions(t.Clips)
		}
		out.Tracks[i] = t
	}
	return out, shortened
}

// clampTransitions 转场不超过相邻两段较短者的一半（第一段的转入只看自己），不到下限就去掉。
func clampTransitions(clips []timelinedomain.ContentClip) {
	for k := range clips {
		tr := clips[k].TransitionIn
		if tr == nil || !tr.IsActive() {
			continue
		}
		lim := clips[k].PlayLen() / 2
		if k > 0 {
			lim = min(lim, clips[k-1].PlayLen()/2)
		}
		d := min(tr.DurationMs, lim)
		if d < timelinedomain.MinTransitionMs {
			clips[k].TransitionIn = nil
			continue
		}
		clips[k].TransitionIn = &timelinedomain.Transition{Kind: tr.Kind, DurationMs: d}
	}
}
