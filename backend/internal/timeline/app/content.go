package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 整份保存（docs/agent/smartcut SCT-05、SC-I-02）：一次请求、一个事务保存整条时间线。
// 剪辑台的撤销 / 重做靠它 —— 逐项接口的「反向操作」不可靠（SCD-15）。

// SaveContentInput 整份保存。Content 已过 domain.DecodeContent（形状、上限、同轨重叠、转场）。
//
// AI 落地、输入同步、片段回填、快照恢复都复用这一条写路径（SCD-19），各自给 Reason 与 Snapshot。
type SaveContentInput struct {
	WorkspaceID int64
	TimelineID  int64
	Editor      repo.Editor
	Content     domain.Content
	// Reason 事件里的写入原因；空 = save。
	Reason string
	// Snapshot 非空时先把写入前的完整内容存一份快照（同一事务，SCD-18）。
	Snapshot *SnapshotSpec
	// Fingerprints 非空时同一事务里记下素材指纹（输入同步，SCT-25）。
	Fingerprints *repo.FingerprintWrite
	// skipMedia 恢复快照专用：素材已被删除的片段照常写回、不核素材（它们列进 missing，导出预检会拦）。
	skipMedia map[int64]bool
}

// SnapshotSpec 写入前要存的快照：原因与关联的规划调用 / 生成任务。
type SnapshotSpec struct {
	Reason       string
	CallID       int64
	GenerationID int64
}

// SaveContentResult 保存后的时间线，以及「临时 ID → 正式 ID」（轨道、片段、字幕、文字共用一张表）。
type SaveContentResult struct {
	View  View
	IDMap map[string]string
	// SnapshotID 写入前存下的那份快照（没要求存时为 0）。
	SnapshotID int64
}

// SaveContent 把整条时间线写成给定的样子。
//
// ⚠️ 与逐项接口走同一套规则（SCR-13）：素材归属与种类同 checkKind、取用区间不超出素材、
// 落点同 Layout（画面轨按顺序与转场，其它轨按起点）、编辑者与「剪过」同 bumpVersion。
func (s *Service) SaveContent(ctx context.Context, in SaveContentInput) (SaveContentResult, error) {
	t, err := s.repo.Get(ctx, in.WorkspaceID, in.TimelineID)
	if err != nil {
		return SaveContentResult{}, translate(err)
	}
	c := in.Content
	if err := s.checkContentMedia(ctx, in.WorkspaceID, c, in.skipMedia); err != nil {
		return SaveContentResult{}, err
	}
	var snap *repo.SnapshotWrite
	if in.Snapshot != nil {
		// 快照是**写入前**的内容：按客户端给的基线版本取当前时间线。版本对不上就是别人先写了 ——
		// 那时存下来的不是「这次写入之前」，直接按冲突处理（写入那一步也会撞同一个乐观锁）。
		cur, err := s.Get(ctx, in.WorkspaceID, t.ID)
		if err != nil {
			return SaveContentResult{}, err
		}
		if cur.Timeline.Version != c.Version {
			return SaveContentResult{}, s.conflictError(ctx, t.ID, in.Editor)
		}
		raw, err := json.Marshal(ContentFromView(cur))
		if err != nil {
			return SaveContentResult{}, httpx.Wrap(errcode.Internal, err)
		}
		snap = &repo.SnapshotWrite{ID: s.repo.NewID(), Version: c.Version, Content: raw,
			Reason: in.Snapshot.Reason, CallID: in.Snapshot.CallID, GenerationID: in.Snapshot.GenerationID,
			Editor: in.Editor}
	}

	w, idMap, pathOf := s.contentWrite(t.ID, in.Editor, c)
	w.Snapshot = snap
	w.Fingerprints = in.Fingerprints
	if _, err := s.repo.SaveContent(ctx, w); err != nil {
		var oe *repo.ObjectError
		switch {
		case errors.As(err, &oe):
			return SaveContentResult{}, fieldError(pathOf[oe.ID], oe.Reason)
		case errors.Is(err, repo.ErrVersionConflict):
			return SaveContentResult{}, s.conflictError(ctx, t.ID, in.Editor)
		}
		return SaveContentResult{}, translate(err)
	}
	reason := in.Reason
	if reason == "" {
		reason = ReasonSave
	}
	v, err := s.written(ctx, in.WorkspaceID, t.ID, reason)
	if err != nil {
		return SaveContentResult{}, err
	}
	out := make(map[string]string, len(idMap))
	for ref, id := range idMap {
		out[ref] = strconv.FormatInt(id, 10)
	}
	res := SaveContentResult{View: v, IDMap: out}
	if snap != nil {
		res.SnapshotID = snap.ID
	}
	return res, nil
}

// contentWrite 把内容换成要写进库的样子：给临时 ID 分配正式 ID、改写引用它们的地方、算好落点。
// 返回的 pathOf 把正式 ID 映射回字段路径（库里核对失败时报给前端）。
func (s *Service) contentWrite(timelineID int64, ed repo.Editor, c domain.Content) (
	repo.ContentWrite, map[string]int64, map[int64]string) {
	idMap := map[string]int64{}
	pathOf := map[int64]string{}
	resolve := func(ref, path string) int64 {
		if domain.IsTempID(ref) {
			id, ok := idMap[ref]
			if !ok {
				id = s.repo.NewID()
				idMap[ref] = id
			}
			if path != "" {
				pathOf[id] = path
			}
			return id
		}
		id, _ := strconv.ParseInt(ref, 10, 64) // 形状已校验：数字串
		if path != "" {
			pathOf[id] = path
		}
		return id
	}

	lay := domain.Layout(c)
	w := repo.ContentWrite{
		TimelineID: timelineID, Version: c.Version, Editor: ed,
		AspectRatio: c.AspectRatio, FPS: c.FPS, Width: c.Width, Height: c.Height,
	}
	kindIdx := map[string]int{}
	for i, ct := range c.Tracks {
		tp := fmt.Sprintf("tracks[%d]", i)
		trackID := resolve(ct.ID, tp+".id")
		kindIdx[ct.Kind]++
		vol := domain.DefaultVolume
		if ct.Volume != nil {
			vol = *ct.Volume
		}
		tw := repo.TrackWrite{
			Track: repo.Track{ID: trackID, Kind: ct.Kind, Idx: kindIdx[ct.Kind], Name: ct.Name,
				IsMuted: ct.IsMuted, Volume: vol, Role: ct.Role, Lang: ct.Lang, IsHidden: ct.IsHidden,
				Style: domain.ObjectOrNil(ct.Style)},
			New: domain.IsTempID(ct.ID),
		}
		clips := make([]domain.Clip, len(ct.Clips))
		fresh := map[int64]bool{}
		for j, cc := range ct.Clips {
			clipID := resolve(cc.ID, fmt.Sprintf("%s.clips[%d].id", tp, j))
			fresh[clipID] = domain.IsTempID(cc.ID)
			pl := lay.Clips[cc.ID]
			clip := domain.Clip{
				ID: clipID, TrackID: trackID, Idx: j + 1, MediaID: cc.MediaID,
				SourceInMs: cc.SourceInMs, SourceOutMs: cc.SourceOutMs,
				StartMs: pl.StartMs, DurationMs: pl.DurationMs,
				Speed: valueOr(cc.Speed, domain.DefaultSpeed), Volume: valueOr(cc.Volume, domain.DefaultVolume),
				FadeInMs: cc.FadeInMs, FadeOutMs: cc.FadeOutMs,
				Transform: domain.ObjectOrNil(cc.Transform), Origin: cc.Origin, SourceNodeID: cc.SourceNodeID,
				Alternates: cc.Alternates, Anchor: domain.ObjectOrNil(cc.Anchor),
				// ⚠️ SourceShotID 一律不写：新片段剥掉（复制出来的片段不撞 uk_clip_shot），
				// 已有片段以库里为准（仓储的更新语句不碰这一列）。
			}
			// 跟随片段的音频（SC-F-56）存这一刻按锚点算出的取用区间与速度（落点已在 pl 里）：
			// 那一段之后被删时，音效按最后一次的位置留在原处。隐藏时按客户端给的存。
			if f := pl.Follow; f != nil && !f.Hidden {
				clip.SourceInMs, clip.SourceOutMs, clip.Speed = f.SourceInMs, f.SourceOutMs, f.Speed
			}
			if cc.TransitionIn != nil {
				clip.TransitionIn = *cc.TransitionIn
			}
			if cc.TransitionOut != nil {
				clip.TransitionOut = *cc.TransitionOut
			}
			clips[j] = clip
		}
		// 画面轨的顺序就是内容里的顺序；其它轨按起点放置（idx 按起点重编）。
		if ct.Kind != domain.TrackVideo {
			clips = domain.PlaceByStart(clips)
		}
		for _, cl := range clips {
			tw.Clips = append(tw.Clips, repo.ClipWrite{Clip: cl, New: fresh[cl.ID]})
		}
		w.Tracks = append(w.Tracks, tw)
	}
	// 片段锚点里的临时 ID 换成正式 ID：放在所有轨道都处理完之后（锚点指向的画面片段可能排在后面）。
	for ti := range w.Tracks {
		for ci := range w.Tracks[ti].Clips {
			if a := w.Tracks[ti].Clips[ci].Clip.Anchor; len(a) > 0 {
				w.Tracks[ti].Clips[ci].Clip.Anchor = rewriteAnchor(a, idMap)
			}
		}
	}
	for k, cs := range c.Subtitles {
		id := resolve(cs.ID, fmt.Sprintf("subtitles[%d].id", k))
		// 跟随片段的字幕存这一刻的成片时间：片段之后被删时，它按最后一次的成片时间留在原处（dev/02 §2）。
		pl := lay.Subtitles[cs.ID]
		w.Subtitles = append(w.Subtitles, repo.SubtitleWrite{
			Subtitle: repo.Subtitle{ID: id, TrackID: resolve(cs.TrackID, ""), StartMs: pl.StartMs, EndMs: pl.EndMs,
				Text: cs.Text, Style: cs.Style, Anchor: rewriteAnchor(cs.Anchor, idMap)},
			New: domain.IsTempID(cs.ID),
		})
	}
	for k, ct := range c.Texts {
		id := resolve(ct.ID, fmt.Sprintf("texts[%d].id", k))
		w.Texts = append(w.Texts, repo.TextWrite{
			Text: domain.TextLayer{ID: id, TrackID: resolve(ct.TrackID, ""), StartMs: ct.StartMs, EndMs: ct.EndMs,
				Text: ct.Text, Style: domain.ObjectOrNil(ct.Style), Box: domain.ObjectOrNil(ct.Box)},
			New: domain.IsTempID(ct.ID),
		})
	}
	return w, idMap, pathOf
}

// rewriteAnchor 把锚点里指向临时 ID 的片段改写成正式 ID；其余字段原样保留（GO-35）。
// 指向正式 ID 的（含已被删掉的失效锚点）原样不动。
func rewriteAnchor(raw json.RawMessage, idMap map[string]int64) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	ref, ok := anchorClipRef(raw)
	if !ok || !domain.IsTempID(ref) {
		return raw
	}
	id, ok := idMap[ref]
	if !ok {
		return raw // 校验已保证临时 ID 指向本内容里的片段；走到这里说明两边规则不一致，原样留着更安全
	}
	return rewriteAnchorRef(raw, strconv.FormatInt(id, 10))
}

// anchorClipRef 锚点里的片段 ID。
func anchorClipRef(raw json.RawMessage) (string, bool) {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return "", false
	}
	var ref string
	if json.Unmarshal(m["clipId"], &ref) != nil {
		return "", false
	}
	return ref, true
}

// rewriteAnchorRef 把锚点的片段 ID 换成 ref，其余字段原样保留。
func rewriteAnchorRef(raw json.RawMessage, ref string) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	m["clipId"], _ = json.Marshal(ref)
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// checkContentMedia 素材归属（本工作区）、种类与轨道匹配、取用区间不超出素材本身 —— 与逐项接口同一套（checkKind、ValidateClip）。
// 备选的素材同样要核：切回备选时它会原样进画面，别处工作区的素材不能借这条路混进来。
func (s *Service) checkContentMedia(ctx context.Context, workspaceID int64, c domain.Content, skip map[int64]bool) error {
	cache := map[int64]MediaInfo{}
	look := func(mediaID int64, path string) (MediaInfo, error) {
		if info, ok := cache[mediaID]; ok {
			return info, nil
		}
		if s.media == nil {
			return MediaInfo{}, httpx.Errf(errcode.Internal, "媒体来源未接入")
		}
		info, err := s.media.Lookup(ctx, workspaceID, mediaID)
		if err != nil {
			// 不区分「不存在」与「不是你的」：区分开等于提供一个按 id 探测的接口。
			return MediaInfo{}, fieldError(path, "素材不存在或不属于当前工作区")
		}
		cache[mediaID] = info
		return info, nil
	}
	check := func(kind string, mediaID int64, out int, base string) error {
		if skip[mediaID] {
			return nil
		}
		info, err := look(mediaID, base+".mediaId")
		if err != nil {
			return err
		}
		if err := checkKind(kind, info.Kind); err != nil {
			return fieldError(base+".mediaId", err.Error())
		}
		// 素材时长未知时放行（同 ValidateClip）。
		if info.DurationMs > 0 && out > info.DurationMs {
			return fieldError(base+".sourceOutMs", fmt.Sprintf(
				"取用区间超出素材本身：素材 %d 毫秒，取到 %d 毫秒", info.DurationMs, out))
		}
		return nil
	}
	for i, t := range c.Tracks {
		for j, cl := range t.Clips {
			base := fmt.Sprintf("tracks[%d].clips[%d]", i, j)
			if err := check(t.Kind, cl.MediaID, cl.SourceOutMs, base); err != nil {
				return err
			}
			for k, a := range cl.Alternates {
				if err := check(t.Kind, a.MediaID, a.SourceOutMs, fmt.Sprintf("%s.alternates[%d]", base, k)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// fieldError 带字段路径的 40001（前端按 path 定位到具体的轨道 / 片段 / 字段）。
func fieldError(path, reason string) error {
	if path == "" {
		return httpx.Errf(errcode.InvalidArgument, reason)
	}
	return httpx.Errf(errcode.InvalidArgument, reason).WithDetails(map[string]any{"path": path})
}

// ContentError 把内容校验的字段错误换成 40001（transport 解析请求体时用）。
func ContentError(err error) error {
	var ve *domain.ValidationError
	if errors.As(err, &ve) {
		return fieldError(ve.Path, ve.Reason)
	}
	return httpx.Errf(errcode.InvalidArgument, err.Error())
}

// ---------------------------------------------------------------- 冲突详情

// ConflictError 时间线当前的冲突详情（AI 剪辑发起时 baseVersion 就对不上，用它说清是谁改的，SCT-27）。
func (s *Service) ConflictError(ctx context.Context, timelineID int64, ed repo.Editor) error {
	return s.conflictError(ctx, timelineID, ed)
}

// conflictError 版本冲突：说清是谁、从哪条路改的（SCD-16，同导演台 directorConflictDetails）。
//
// ⚠️ `bySelf` 只认「上一次与这一次都是本人亲手（user）」：Agent / MCP 的写入也带着用户本人的 ID，
// 只比编辑者 ID 的话会把「Agent 刚剪的」说成「你在另一个窗口改的」，用户点「以我为准」就把它覆盖掉。
// 取不到当前状态时退回中性说法，不猜。
func (s *Service) conflictError(ctx context.Context, timelineID int64, ed repo.Editor) error {
	st, err := s.repo.CurrentEditor(ctx, timelineID)
	if err != nil {
		return translate(repo.ErrVersionConflict)
	}
	self := bySelf(st, ed)
	editorID := ""
	if st.EditorID != 0 {
		editorID = strconv.FormatInt(st.EditorID, 10)
	}
	return httpx.Errf(errcode.VersionConflict, conflictMessage(st, self)).WithDetails(map[string]any{
		"currentVersion":    st.Version,
		"currentUpdatedAt":  st.UpdatedAt.UTC().Format(time.RFC3339),
		"currentEditorId":   editorID,
		"currentEditorName": st.EditorName,
		// currentSource 与 editorKind 同值、两个都给（同导演台 DRD-46：前端的冲突文案函数读前者，
		// Agent / MCP 的重放逻辑认后者）。
		"currentSource": st.EditorKind,
		"editorKind":    st.EditorKind,
		"bySelf":        self,
	})
}

// bySelf 最后一次写入是不是当前用户**本人亲手**做的，而这一次也是本人亲手。
func bySelf(st repo.EditorState, ed repo.Editor) bool {
	return st.EditorKind == repo.EditorUser && ed.Kind == repo.EditorUser &&
		st.EditorID != 0 && st.EditorID == ed.UserID
}

func conflictMessage(st repo.EditorState, bySelf bool) string {
	switch {
	case bySelf:
		return "你在另一个窗口修改过这条时间线，请载入最新或选择以当前内容为准"
	case st.EditorKind == repo.EditorAI:
		return "AI 剪辑刚修改了这条时间线，请载入最新后再保存"
	case st.EditorKind == repo.EditorAgent:
		return "Agent 刚修改了这条时间线，请载入最新后再保存"
	case st.EditorKind == repo.EditorMCP && st.EditorName != "":
		return "外部 AI 客户端（代 " + st.EditorName + "）刚修改了这条时间线，请载入最新后再保存"
	case st.EditorKind == repo.EditorMCP:
		return "外部 AI 客户端刚修改了这条时间线，请载入最新后再保存"
	case st.EditorKind == repo.EditorSync:
		return "连进来的素材有变化，时间线刚自动更新过，请载入最新后再保存"
	case st.EditorName != "":
		return st.EditorName + " 修改了这条时间线，请载入最新后再保存"
	default:
		return "时间线已被改动，请载入最新后再保存"
	}
}

func valueOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// SetClipPending 记下这一段正在被片段级生成替换（docs/agent/smartcut SCT-37）：不推进版本、不发事件（见 repo）。
// 时间线不在本工作区时 404；片段不在这条时间线上时返回 false。
func (s *Service) SetClipPending(ctx context.Context, workspaceID, timelineID, clipID, generationID int64) (bool, error) {
	if _, err := s.repo.Get(ctx, workspaceID, timelineID); err != nil {
		return false, translate(err)
	}
	return s.repo.SetClipPending(ctx, timelineID, clipID, generationID)
}

// ClearClipPending 清掉这一次生成的标记（只清 pending = generationID 的）。
func (s *Service) ClearClipPending(ctx context.Context, workspaceID, timelineID, clipID, generationID int64) (bool, error) {
	if _, err := s.repo.Get(ctx, workspaceID, timelineID); err != nil {
		return false, translate(err)
	}
	return s.repo.ClearClipPending(ctx, timelineID, clipID, generationID)
}

// ClipPending 这一段现在挂着的片段级生成（0 = 没有）。
func (s *Service) ClipPending(ctx context.Context, workspaceID, timelineID, clipID int64) (int64, error) {
	if _, err := s.repo.Get(ctx, workspaceID, timelineID); err != nil {
		return 0, translate(err)
	}
	return s.repo.ClipPending(ctx, timelineID, clipID)
}
