package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 片段级生成的回填（docs/agent/smartcut SCT-37，SCD-33）：生成域在任务到终态时调 BindSmartcut。
//
//   - 成功：读时间线 → 目标还在就把产物放进去（替换这一段、原素材进备选；或插在记下的位置）→ 存快照（clip_fill）
//     并以读到的版本整份写入（推 timeline.updated，编辑者 system）→ 版本冲突时重读重放，最多 clipWriteAttempts 次 →
//     记下已回填。目标已经不在（这一段被删了）时不改时间线，产物只进生成记录。
//   - 失败 / 取消：只清「生成中」的标记，素材不动（退款由生成域按原批次完成）。
//
// ⚠️ 四条终态路径都会调到这里（成功、失败或超时或不确定、取消、运维补偿）：只接成功的话，失败的片段会一直显示
// 「生成中」，导出也一直被预检拦着（同组子项回写那个坑）。

// clipWriteAttempts 回填的最多尝试次数（版本冲突时重读重放）。
const clipWriteAttempts = 3

func (s *Service) bindClip(ctx context.Context, in BindInput) error {
	p := s.clipTools
	if p.Timelines == nil || p.Pending == nil {
		return fmt.Errorf("片段级生成链路未接入")
	}
	var t clipTarget
	if err := json.Unmarshal(in.Target, &t); err != nil {
		return fmt.Errorf("片段级生成的回写目标解析失败: %w", err)
	}
	clear := func() {
		if t.Kind == clipKindReplace && t.ClipID != 0 {
			if _, err := p.Pending.ClearClipPending(ctx, in.WorkspaceID, t.TimelineID, t.ClipID, in.GenerationID); err != nil {
				p.Log.WarnContext(ctx, "清「生成中」标记失败", "clipId", t.ClipID, "generationId", in.GenerationID, "error", err)
			}
		}
	}
	if in.Status != "success" || len(in.MediaIDs) == 0 {
		clear()
		return nil
	}
	media := in.MediaIDs[0]
	infos, err := p.Media.Info(ctx, in.WorkspaceID, []int64{media})
	if err != nil {
		clear()
		return err
	}
	mediaMs := infos[media].DurationMs
	landed := false
	for attempt := 1; ; attempt++ {
		content, err := p.Timelines.Content(ctx, in.WorkspaceID, t.TimelineID)
		if err != nil {
			clear()
			return err
		}
		next, ok, err := s.landClip(ctx, in, t, content, media, mediaMs)
		if err != nil || !ok {
			// 目标已经不在（这一段被删了 / 插入位置那一段被删了）：不改时间线，产物在生成记录里。
			clear()
			return err
		}
		_, err = p.Timelines.Save(ctx, SavePlanInput{WorkspaceID: in.WorkspaceID, TimelineID: t.TimelineID,
			Editor: Editor{UserID: t.UserID, Kind: "system"}, Content: next, Reason: "clip_fill", SnapshotReason: "clip_fill"})
		if err == nil {
			landed = true
			break
		}
		if !isVersionConflict(err) || attempt >= clipWriteAttempts {
			clear()
			return err
		}
	}
	clear()
	if landed {
		if err := p.Gen.MarkLanded(ctx, in.GenerationID); err != nil {
			p.Log.WarnContext(ctx, "产物已回填但没记下「已回填」", "generationId", in.GenerationID, "error", err)
		}
	}
	return nil
}

// landClip 按回写目标把产物放进内容。目标已经不在时 ok=false。
func (s *Service) landClip(ctx context.Context, in BindInput, t clipTarget, c timelinedomain.Content, media int64,
	mediaMs int) (timelinedomain.Content, bool, error) {
	switch t.Kind {
	case clipKindReplace:
		clipID := strconv.FormatInt(t.ClipID, 10)
		cur, _, ok := domain.FindClip(c, clipID)
		if !ok {
			return c, false, nil
		}
		old, err := s.alternateOf(ctx, in.WorkspaceID, cur.MediaID)
		if err != nil {
			return c, false, err
		}
		next, ok := domain.LandReplace(c, clipID, media, mediaMs, old)
		return next, ok, nil
	case clipKindInsert:
		after := ""
		if t.AfterClipID != 0 {
			after = strconv.FormatInt(t.AfterClipID, 10)
		}
		next, ok := domain.LandInsert(c, after, media, mediaMs, "tmp-fill-"+strconv.FormatInt(in.GenerationID, 10))
		return next, ok, nil
	case clipKindMusic:
		// 配乐轨、起点为发起时的播放头；与已有配乐重叠时放进新建的配乐轨（音频轨满了只进生成记录）。
		gen := strconv.FormatInt(in.GenerationID, 10)
		next, ok := domain.LandMusic(c, media, mediaMs, t.AtMs, t.DurationMs, "tmp-music-"+gen, "tmp-music-track-"+gen)
		return next, ok, nil
	case clipKindSfx:
		// 音效轨、跟随发起时的那一段（那一段已经不在画面轨上时只进生成记录）。
		gen := strconv.FormatInt(in.GenerationID, 10)
		next, ok := domain.LandSfx(c, strconv.FormatInt(t.ClipID, 10), t.RangeMs, media, mediaMs, "tmp-sfx-"+gen,
			"tmp-sfx-track-"+gen)
		return next, ok, nil
	}
	return c, false, nil
}

// alternateOf 被换下来的那份素材进备选时记成哪一类：智能剪辑重拍出来的记 regen、去字幕出来的记 desubtitle，
// 其余（画布上来的原片）记 original；连同产出它的那次生成（切回去之后还能看生成信息）。
func (s *Service) alternateOf(ctx context.Context, ws, mediaID int64) (timelinedomain.Alternate, error) {
	sources, err := s.clipTools.Gen.Sources(ctx, ws, []int64{mediaID})
	if err != nil {
		return timelinedomain.Alternate{}, err
	}
	a := timelinedomain.Alternate{Kind: timelinedomain.AlternateOriginal}
	if src, ok := sources[mediaID]; ok {
		a.GenerationID = src.GenerationID
		switch src.SmartcutTool {
		case toolRegen:
			a.Kind = timelinedomain.AlternateRegen
		case "desubtitle":
			a.Kind = timelinedomain.AlternateDesubtitle
		}
	}
	return a, nil
}
