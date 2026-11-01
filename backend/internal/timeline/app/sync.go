package app

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 输入同步的「连线即入轨」（docs/agent/smartcut SCT-25，SCD-23、SC-F-13）：时间线还没剪过时，
// 把新连进来的素材按编号接到轨道末尾。走整份保存同一条写路径（CAS、编辑者 sync、timeline.updated）。

// SyncImageMs 图片铺上画面轨时的默认时长。
const SyncImageMs = 3000

// SyncItem 一条要铺上轨道的素材（按素材编号的顺序给）。
type SyncItem struct {
	MediaID int64
	// Kind 是 video / image / audio。
	Kind string
	// DurationMs 视频与音频的时长（未知时不铺：猜一个数的话成片长度对不上，而对不上多少没人说得清）。
	DurationMs int
	// AudioRole 音频的用途（music / voice / sfx），决定进哪条音频轨。
	AudioRole    string
	SourceNodeID string
}

// SyncAppendInput 一次铺轨。BaseVersion 是调用方读到的版本（乐观锁）。
type SyncAppendInput struct {
	WorkspaceID int64
	TimelineID  int64
	BaseVersion int
	Items       []SyncItem
	// Fingerprint 本次同步看到的素材指纹，写进 synced。
	Fingerprint string
	// Planned 同时把指纹记成「剪辑依据的素材」（自动铺轨时为 true；只更新素材条时为 false）。
	// 只在还没剪过时生效（条件在写入的 SQL 里）。
	Planned bool
}

// SyncAppendResult 铺轨结果。Appended 与 Items 一一对应：没铺上的为 0（时长未知、超出上限）。
type SyncAppendResult struct {
	View     View
	Appended []int64
}

// syncTrackName 按用途新建音频轨时的名字。
var syncTrackName = map[string]string{"music": "配乐", "voice": "人声", "sfx": "音效"}

// AppendSynced 把素材接到轨道末尾：视频、图片进画面轨（图片 3 秒），音频按用途进对应的音频轨
// （没有该用途的轨时先认领一条还没定用途的，再没有就新建一条），接在那条轨现有内容之后。
//
// ⚠️ 只在「还没剪过」时动轨道：剪过之后自动铺轨会把用户排好的东西推翻（SCD-17）。剪过的时间线
// 只记指纹、不动内容。逐条试加并校验上限（成片 30 分钟、片段 500 个、音频轨 6 条）：超出的那条不铺，
// 其余照铺 —— 一条超限不该让整批都铺不上。
func (s *Service) AppendSynced(ctx context.Context, in SyncAppendInput) (SyncAppendResult, error) {
	v, err := s.Get(ctx, in.WorkspaceID, in.TimelineID)
	if err != nil {
		return SyncAppendResult{}, err
	}
	if v.Timeline.Version != in.BaseVersion {
		return SyncAppendResult{}, s.conflictError(ctx, v.Timeline.ID, repo.Editor{Kind: repo.EditorSync})
	}
	appended := make([]int64, len(in.Items))
	fp := repo.FingerprintWrite{Synced: in.Fingerprint, Planned: in.Planned}
	if v.Timeline.IsArranged || len(in.Items) == 0 {
		if err := s.repo.SetFingerprints(ctx, v.Timeline.ID, fp); err != nil {
			return SyncAppendResult{}, httpx.Wrap(errcode.Internal, err)
		}
		return SyncAppendResult{View: v, Appended: appended}, nil
	}

	c := ContentFromView(v)
	refs := make([]string, len(in.Items))
	for i, it := range in.Items {
		next, ref, ok := withSyncClip(c, it, i)
		if !ok {
			continue
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return SyncAppendResult{}, httpx.Wrap(errcode.Internal, err)
		}
		if domain.ValidateContent(raw) != nil {
			continue // 超出上限（或这一条本身不合法）：不铺，留在素材条里当「未使用」
		}
		c, refs[i] = next, ref
	}
	if !anyRef(refs) {
		if err := s.repo.SetFingerprints(ctx, v.Timeline.ID, fp); err != nil {
			return SyncAppendResult{}, httpx.Wrap(errcode.Internal, err)
		}
		return SyncAppendResult{View: v, Appended: appended}, nil
	}
	res, err := s.SaveContent(ctx, SaveContentInput{
		WorkspaceID: in.WorkspaceID, TimelineID: v.Timeline.ID, Editor: repo.Editor{Kind: repo.EditorSync},
		Content: c, Reason: ReasonSync, Fingerprints: &fp,
	})
	if err != nil {
		return SyncAppendResult{}, err
	}
	for i, ref := range refs {
		if ref == "" {
			continue
		}
		if id, err := strconv.ParseInt(res.IDMap[ref], 10, 64); err == nil {
			appended[i] = id
		}
	}
	return SyncAppendResult{View: res.View, Appended: appended}, nil
}

func anyRef(refs []string) bool {
	for _, r := range refs {
		if r != "" {
			return true
		}
	}
	return false
}

// withSyncClip 在内容的副本上加一段，返回新内容与这一段的临时 ID。放不了（时长未知、种类不对）时 ok=false。
func withSyncClip(c domain.Content, it SyncItem, i int) (domain.Content, string, bool) {
	clip := domain.ContentClip{
		ID: "tmp-sync-" + strconv.Itoa(i+1), MediaID: it.MediaID, Origin: domain.OriginSync, SourceNodeID: it.SourceNodeID,
	}
	switch it.Kind {
	case "image":
		clip.SourceOutMs = SyncImageMs
	case "video", "audio":
		if it.DurationMs <= 0 {
			return c, "", false
		}
		clip.SourceOutMs = it.DurationMs
	default:
		return c, "", false
	}
	next := cloneContent(c)
	if it.Kind != "audio" {
		for ti := range next.Tracks {
			if next.Tracks[ti].Kind == domain.TrackVideo {
				next.Tracks[ti].Clips = append(next.Tracks[ti].Clips, clip)
				return next, clip.ID, true
			}
		}
		return c, "", false
	}
	ti := audioTrackFor(&next, it.AudioRole, i)
	end := 0
	for _, cl := range next.Tracks[ti].Clips {
		if cl.StartMs != nil {
			end = max(end, *cl.StartMs+cl.PlayLen())
		}
	}
	clip.StartMs = &end
	next.Tracks[ti].Clips = append(next.Tracks[ti].Clips, clip)
	return next, clip.ID, true
}

// audioTrackFor 找（或认领、或新建）这个用途的音频轨，返回下标。
// 认领：默认建的那条「音频」轨没有定用途，第一份音频来时就把它定成那个用途。
func audioTrackFor(c *domain.Content, role string, i int) int {
	if role == "" {
		role = "music"
	}
	free := -1
	for ti, t := range c.Tracks {
		if t.Kind != domain.TrackAudio {
			continue
		}
		if t.Role == role {
			return ti
		}
		if t.Role == "" && free < 0 {
			free = ti
		}
	}
	if free >= 0 {
		c.Tracks[free].Role = role
		return free
	}
	c.Tracks = append(c.Tracks, domain.ContentTrack{
		ID: "tmp-sync-track-" + strconv.Itoa(i+1), Kind: domain.TrackAudio, Name: syncTrackName[role], Role: role,
	})
	return len(c.Tracks) - 1
}

// cloneContent 复制到轨道与片段这一层（逐条试加时失败要能丢掉这一步，不能改坏上一步的内容）。
func cloneContent(c domain.Content) domain.Content {
	out := c
	out.Tracks = make([]domain.ContentTrack, len(c.Tracks))
	for i, t := range c.Tracks {
		t.Clips = append([]domain.ContentClip(nil), t.Clips...)
		out.Tracks[i] = t
	}
	return out
}
