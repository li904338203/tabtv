// Package app 编排时间线的读写（NEW-07~NEW-09）。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MediaSource 由媒体域实现：解析素材并给出时长。
//
// ⚠️ 只暴露"这份素材是什么"，**不给 bucket / objectKey**：
// 时间线拿不到定位信息，就不可能绕过受保护读取去直接取对象（GO-27）。
type MediaSource interface {
	// Lookup 按工作区解析素材。查不到与不属于本工作区返回同一个错误。
	Lookup(ctx context.Context, workspaceID, mediaID int64) (MediaInfo, error)
}

type MediaInfo struct {
	Kind string
	// DurationMs 0 表示未知（ffprobe 没装或认不出这个封装）。
	DurationMs int
}

// ShotSource 由脚本域实现：按镜号顺序给出每一镜的成片素材。
//
// ⚠️ 它要回**缺口的原因**，不只是"有没有"。NEW-09 明确要求
// 缺素材或失败镜头不静默跳过 —— 而"为什么没有"只有脚本域知道。
type ShotSource interface {
	ShotClips(ctx context.Context, workspaceID, scriptID int64) ([]domain.ShotClip, error)
}

// ProjectGuard 由画布域实现：校验项目归属。
//
// ⚠️ 方法名与脚本域那个端口**故意相同**：Go 的接口是结构化的，
// 同名同签名就意味着组装根里那个 `projectGuard` 适配器两边都能用。
// 各起一个名字的话要写两个一模一样的适配器，而两份早晚会不一致
// （比如有人给其中一个加了缓存）。
type ProjectGuard interface {
	EnsureProject(ctx context.Context, workspaceID, projectID int64) error
}

type Service struct {
	repo    *repo.Repo
	media   MediaSource
	shots   ShotSource
	project ProjectGuard

	// 渲染那一半（P10-03）。
	pool        *pgxpool.Pool
	credits     CreditReserver
	refunder    Refunder
	renderQuote RenderQuoter
	nextID      func() int64
	userFlags   UserFlags
	mediaExists MediaExistence
	// events 写入成功后发 timeline.updated（SCD-19）。
	events   EventPublisher
	ffmpegOK bool
	// renderLimits 导出按用户限流（SCT-15）。
	renderLimits RenderLimits
	// downloads 成片下载链接（SCT-15，媒体域实现）；downloadTTL 链接有效期。
	downloads   RenderDownloads
	downloadTTL time.Duration
	log         *slog.Logger
}

func (s *Service) SetLogger(l *slog.Logger) { s.log = l }

func (s *Service) warn(ctx context.Context, msg string, args ...any) {
	if s.log != nil {
		s.log.WarnContext(ctx, msg, args...)
	}
}

func New(r *repo.Repo) *Service { return &Service{repo: r} }

func (s *Service) SetMedia(m MediaSource)    { s.media = m }
func (s *Service) SetShots(sh ShotSource)    { s.shots = sh }
func (s *Service) SetProject(p ProjectGuard) { s.project = p }

// View 是一条时间线的完整视图。
type View struct {
	Timeline  repo.Timeline
	Tracks    []TrackView
	Subtitles []repo.Subtitle
	// Texts 文字轨上的文字层（标题、贴字，SC-F-50）。
	Texts []domain.TextLayer
	// TotalMs 视频轨各段时长之和（不算转场重叠）。
	TotalMs int
	// EffectiveMs 算上转场重叠之后的**实际成片长度**。
	//
	// ⚠️ 两个数都要给：用户按 TotalMs 理解"我放了多少素材"，
	// 但配乐与字幕要对齐的是 EffectiveMs —— 只给一个的话，
	// 设了转场之后字幕就会整体偏移，而偏多少没人说得清。
	EffectiveMs int
}

type TrackView struct {
	Track repo.Track
	Clips []domain.Clip
}

type CreateInput struct {
	WorkspaceID int64
	ProjectID   int64
	ScriptID    int64
	Name        string
	AspectRatio string
	FPS         int
	Width       int
	Height      int
}

// Create 建一条时间线。
func (s *Service) Create(ctx context.Context, in CreateInput) (View, error) {
	if s.project != nil {
		if err := s.project.EnsureProject(ctx, in.WorkspaceID, in.ProjectID); err != nil {
			return View{}, httpx.Err(errcode.NotFound)
		}
	}
	if in.Name == "" {
		in.Name = "未命名时间线"
	}
	// 画幅与帧率给默认值，别让用户第一步就面对一堆参数。
	if in.AspectRatio == "" {
		in.AspectRatio = "16:9"
	}
	if in.FPS <= 0 {
		in.FPS = 30
	}
	if in.Width <= 0 || in.Height <= 0 {
		// 「跟随素材」的自定义尺寸只能由调用方按素材算好再给：回落成 1920×1080 的话，
		// 用户选的是「跟随素材」，拿到的却是一块横屏画布。
		if in.AspectRatio == domain.AspectCustom {
			return View{}, httpx.Errf(errcode.InvalidArgument, "自定义画幅要给出宽高").
				WithDetails(map[string]any{"path": "width"})
		}
		// ⚠️ 默认尺寸**跟着画幅走**，不能一律 1920×1080：
		// 只传 `aspectRatio: "9:16"` 的调用方（脚本、将来的其它客户端）
		// 会拿到一个横屏画布 —— 用户选了竖屏、渲出来是横的，
		// 而两边都"没报错"。前端恰好每次都带尺寸，掩盖了这一点。
		in.Width, in.Height = defaultSize(in.AspectRatio)
	}
	if err := validateFormat(in.AspectRatio, in.FPS, in.Width, in.Height); err != nil {
		return View{}, err
	}

	t, err := s.repo.Create(ctx, repo.Timeline{
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, ScriptID: in.ScriptID,
		Name: in.Name, AspectRatio: in.AspectRatio, FPS: in.FPS,
		Width: in.Width, Height: in.Height,
	})
	if err != nil {
		return View{}, httpx.Wrap(errcode.Internal, err)
	}
	return s.load(ctx, t)
}

func (s *Service) Get(ctx context.Context, workspaceID, id int64) (View, error) {
	t, err := s.repo.Get(ctx, workspaceID, id)
	if err != nil {
		return View{}, translate(err)
	}
	return s.load(ctx, t)
}

func (s *Service) List(ctx context.Context, workspaceID, projectID int64) (
	[]repo.Timeline, error) {
	list, err := s.repo.ListByProject(ctx, workspaceID, projectID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

func (s *Service) load(ctx context.Context, t repo.Timeline) (View, error) {
	tracks, err := s.repo.Tracks(ctx, t.ID)
	if err != nil {
		return View{}, httpx.Wrap(errcode.Internal, err)
	}
	v := View{Timeline: t}
	byID := map[int64]domain.Clip{}
	videoByID := map[int64]domain.Clip{}
	for _, tr := range tracks {
		clips, err := s.repo.Clips(ctx, tr.ID)
		if err != nil {
			return View{}, httpx.Wrap(errcode.Internal, err)
		}
		clips = place(tr.Kind, clips)
		if tr.Kind == domain.TrackVideo {
			v.TotalMs = domain.TotalDuration(clips)
			v.EffectiveMs = domain.EffectiveDuration(clips)
		}
		for _, c := range clips {
			byID[c.ID] = c
			if tr.Kind == domain.TrackVideo {
				videoByID[c.ID] = c
			}
		}
		v.Tracks = append(v.Tracks, TrackView{Track: tr, Clips: clips})
	}
	followAnchored(&v, videoByID, byID)
	subs, err := s.repo.Subtitles(ctx, t.ID)
	if err != nil {
		return View{}, httpx.Wrap(errcode.Internal, err)
	}
	// 跟随片段的字幕按片段当前的位置与取用区间换算（SC-F-56）：片段经逐项接口裁切、移动、变速之后，
	// 字幕不必再存一遍。所属片段已被删时锚点失效，按存储的起止留在原处。
	for i, sub := range subs {
		a, ok := domain.ParseAnchor(sub.Anchor)
		if !ok {
			continue
		}
		c, found := byID[a.ClipID]
		if !found {
			continue
		}
		pl := domain.PlaceAnchored(a.SrcStartMs, a.SrcEndMs, c.StartMs, c,
			domain.SpanPlace{StartMs: sub.StartMs, EndMs: sub.EndMs})
		subs[i].StartMs, subs[i].EndMs, subs[i].Hidden = pl.StartMs, pl.EndMs, pl.Hidden
	}
	v.Subtitles = subs
	texts, err := s.repo.Texts(ctx, t.ID)
	if err != nil {
		return View{}, httpx.Wrap(errcode.Internal, err)
	}
	v.Texts = texts
	return v, nil
}

// followAnchored 跟随片段的音频（按画面生成的音效，SC-F-56）按锚点与那一段当前的位置、取用区间、速度现算
// （domain.FollowClip）：片段经逐项接口裁切、移动、变速之后，音效不必再存一遍。所属片段已被删、不在画面轨上时
// 锚点失效，按存储值留在原处。算完按起点重排这条轨（idx 跟着落点）。
func followAnchored(v *View, videoByID, byID map[int64]domain.Clip) {
	for ti, tv := range v.Tracks {
		if tv.Track.Kind != domain.TrackAudio {
			continue
		}
		changed := false
		for ci, c := range tv.Clips {
			a, ok := domain.ParseAnchor(c.Anchor)
			if !ok {
				continue
			}
			target, found := videoByID[a.ClipID]
			if !found {
				continue
			}
			pl := domain.FollowClip(a.SrcStartMs, a.SrcEndMs, target.StartMs, target, c)
			c.StartMs, c.DurationMs = pl.StartMs, pl.DurationMs
			c.SourceInMs, c.SourceOutMs, c.Speed, c.Hidden = pl.Follow.SourceInMs, pl.Follow.SourceOutMs,
				pl.Follow.Speed, pl.Follow.Hidden
			tv.Clips[ci] = c
			if !c.Hidden {
				byID[c.ID] = c // 跟随这段音频的字幕按它算出来的位置换算
			}
			changed = true
		}
		if changed {
			v.Tracks[ti].Clips = domain.PlaceByStart(tv.Clips)
		}
	}
}

// AddClipInput 往轨道上加一段。
type AddClipInput struct {
	WorkspaceID int64
	// Editor 这次写入是谁、从哪条路来的（SCD-16）。
	Editor     repo.Editor
	TimelineID int64
	TrackID    int64
	Version    int
	MediaID    int64
	// SourceInMs / SourceOutMs 为 0 表示"整段"，由探测到的时长填。
	SourceInMs  int
	SourceOutMs int
	// AfterClipID 插在哪一段之后；0 表示追加到末尾。
	AfterClipID int64
}

// AddClip 导入一段素材。
func (s *Service) AddClip(ctx context.Context, in AddClipInput) (View, error) {
	t, track, err := s.owned(ctx, in.WorkspaceID, in.TimelineID, in.TrackID)
	if err != nil {
		return View{}, err
	}
	info, err := s.lookupMedia(ctx, in.WorkspaceID, in.MediaID)
	if err != nil {
		return View{}, err
	}
	if err := checkKind(track.Kind, info.Kind); err != nil {
		return View{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}

	out := in.SourceOutMs
	if out <= 0 {
		out = info.DurationMs
	}
	if out <= 0 {
		// ⚠️ 明说而不是猜一个时长：猜的话成片长度对不上，
		// 而"对不上多少"没人说得清。
		return View{}, httpx.Errf(errcode.InvalidArgument,
			"无法确定素材时长（检查 ffprobe 是否可用），请手动指定取用区间")
	}
	fresh := domain.Clip{
		MediaID: in.MediaID, SourceInMs: in.SourceInMs, SourceOutMs: out,
		Speed: domain.DefaultSpeed, Volume: domain.DefaultVolume,
	}
	if err := domain.ValidateClip(fresh, info.DurationMs); err != nil {
		return View{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}

	clips, err := s.repo.Clips(ctx, track.ID)
	if err != nil {
		return View{}, httpx.Wrap(errcode.Internal, err)
	}
	if track.Kind != domain.TrackVideo {
		// 按起点放置的轨道（SCD-15）：默认放在这条轨最后一段之后；指定了「插在哪一段之后」就紧跟它，
		// 与后面那一段撞上时明说，不把后面的段往后挤（那会悄悄改掉用户摆好的配乐位置）。
		clips = domain.PlaceByStart(clips)
		fresh.StartMs = domain.TrackEnd(clips)
		if in.AfterClipID != 0 {
			for _, c := range clips {
				if c.ID == in.AfterClipID {
					fresh.StartMs = c.StartMs + c.PlayLen()
				}
			}
		}
		next, err := placed(append(clips, fresh))
		if err != nil {
			return View{}, err
		}
		return s.commit(ctx, t, track.ID, in.Version, in.Editor, ReasonEdit, next)
	}
	next := domain.InsertAfter(clips, in.AfterClipID, fresh)
	return s.commit(ctx, t, track.ID, in.Version, in.Editor, ReasonEdit, next)
}

// TrimInput 裁切一段。
type TrimInput struct {
	WorkspaceID int64
	// Editor 这次写入是谁、从哪条路来的（SCD-16）。
	Editor      repo.Editor
	TimelineID  int64
	TrackID     int64
	ClipID      int64
	Version     int
	SourceInMs  int
	SourceOutMs int
}

func (s *Service) TrimClip(ctx context.Context, in TrimInput) (View, error) {
	t, track, clips, target, err := s.locate(ctx, in.WorkspaceID, in.TimelineID,
		in.TrackID, in.ClipID)
	if err != nil {
		return View{}, err
	}
	info, err := s.lookupMedia(ctx, in.WorkspaceID, target.MediaID)
	if err != nil {
		return View{}, err
	}
	trimmed, err := domain.Trim(target, in.SourceInMs, in.SourceOutMs, info.DurationMs)
	if err != nil {
		return View{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	next, err := rearrange(track.Kind, replace(clips, trimmed))
	if err != nil {
		return View{}, err
	}
	return s.commit(ctx, t, track.ID, in.Version, in.Editor, ReasonEdit, next)
}

// SplitInput 在时间线上的某个偏移把一段切成两段。
type SplitInput struct {
	WorkspaceID int64
	// Editor 这次写入是谁、从哪条路来的（SCD-16）。
	Editor     repo.Editor
	TimelineID int64
	TrackID    int64
	ClipID     int64
	Version    int
	// AtMs 相对**这一段起点**的偏移。
	AtMs int
}

func (s *Service) SplitClip(ctx context.Context, in SplitInput) (View, error) {
	t, track, clips, target, err := s.locate(ctx, in.WorkspaceID, in.TimelineID,
		in.TrackID, in.ClipID)
	if err != nil {
		return View{}, err
	}
	left, right, err := domain.Split(target, in.AtMs)
	if err != nil {
		return View{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if track.Kind != domain.TrackVideo {
		// 右半段紧接左半段：两段合起来仍占原来那一段的位置。
		right.StartMs = left.StartMs + left.PlayLen()
		next, err := placed(append(replace(clips, left), right))
		if err != nil {
			return View{}, err
		}
		return s.commit(ctx, t, track.ID, in.Version, in.Editor, ReasonEdit, next)
	}
	next := domain.InsertAfter(replace(clips, left), left.ID, right)
	return s.commit(ctx, t, track.ID, in.Version, in.Editor, ReasonEdit, next)
}

// MoveInput 移动一段。
type MoveInput struct {
	WorkspaceID int64
	// Editor 这次写入是谁、从哪条路来的（SCD-16）。
	Editor     repo.Editor
	TimelineID int64
	TrackID    int64
	ClipID     int64
	Version    int
	// ToPosition 画面轨用：排到第几位（从 1 起）。
	ToPosition int
	// StartMs 非画面轨用：放到哪个成片时间（SCD-15）。指针：0 是合法的起点。
	StartMs *int
}

func (s *Service) MoveClip(ctx context.Context, in MoveInput) (View, error) {
	t, track, clips, target, err := s.locate(ctx, in.WorkspaceID, in.TimelineID,
		in.TrackID, in.ClipID)
	if err != nil {
		return View{}, err
	}
	if track.Kind != domain.TrackVideo {
		if in.StartMs == nil || *in.StartMs < 0 {
			return View{}, httpx.Errf(errcode.InvalidArgument,
				"音频轨按成片时间放置，要给出不为负的 startMs").WithDetails(map[string]any{"path": "startMs"})
		}
		target.StartMs = *in.StartMs
		next, err := placed(replace(clips, target))
		if err != nil {
			return View{}, err
		}
		return s.commit(ctx, t, track.ID, in.Version, in.Editor, ReasonEdit, next)
	}
	next, err := domain.Reorder(clips, in.ClipID, in.ToPosition)
	if err != nil {
		return View{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return s.commit(ctx, t, track.ID, in.Version, in.Editor, ReasonEdit, next)
}

// RemoveClip 删一段。
func (s *Service) RemoveClip(ctx context.Context, workspaceID, timelineID,
	trackID, clipID int64, version int, ed repo.Editor) (View, error) {
	t, track, clips, _, err := s.locate(ctx, workspaceID, timelineID, trackID, clipID)
	if err != nil {
		return View{}, err
	}
	next := make([]domain.Clip, 0, len(clips))
	for _, c := range clips {
		if c.ID != clipID {
			next = append(next, c)
		}
	}
	// 删掉一段不改变其它段的位置：画面轨后面的段往前补，音频轨原地不动（SCD-15）。
	return s.commit(ctx, t, track.ID, version, ed, ReasonEdit, place(track.Kind, next))
}

// ClipPropsInput 改片段属性：变速、音量、淡入淡出、转场。
type ClipPropsInput struct {
	WorkspaceID int64
	// Editor 这次写入是谁、从哪条路来的（SCD-16）。
	Editor     repo.Editor
	TimelineID int64
	TrackID    int64
	ClipID     int64
	Version    int
	Props      domain.ClipProps
}

// UpdateClip 改一段的属性。
//
// ⚠️ 转场要在**知道相邻段有多长**之后才能判：500ms 的叠化会吃掉两段各 500ms，
// 超过任一侧的一半就会把那一侧整段吃光 —— 渲染出来直接少一个镜头，
// 而时间线上那一段还好端端地画着。
func (s *Service) UpdateClip(ctx context.Context, in ClipPropsInput) (View, error) {
	t, track, clips, target, err := s.locate(ctx, in.WorkspaceID, in.TimelineID,
		in.TrackID, in.ClipID)
	if err != nil {
		return View{}, err
	}
	next := domain.ApplyProps(target, in.Props)

	info, err := s.lookupMedia(ctx, in.WorkspaceID, next.MediaID)
	if err != nil {
		return View{}, err
	}
	if err := domain.ValidateClip(next, info.DurationMs); err != nil {
		return View{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	// 上一段有多长 —— 第一段传 0（那是"从黑场淡入"，没有上一段可吃）。
	// 只有画面轨有「上一段」：其它轨按起点放置，转场不与谁衔接。
	prevLen := 0
	for i, c := range clips {
		if c.ID == in.ClipID && i > 0 && track.Kind == domain.TrackVideo {
			prevLen = clips[i-1].PlayLen()
		}
	}
	if err := domain.ValidateTransition(next.TransitionIn, prevLen, next.PlayLen()); err != nil {
		return View{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	// 变速会改变这一段的长度：音频轨上要重新看一次会不会压到后一段。
	arranged, err := rearrange(track.Kind, replace(clips, next))
	if err != nil {
		return View{}, err
	}
	return s.commit(ctx, t, track.ID, in.Version, in.Editor, ReasonEdit, arranged)
}

// AssembleInput 按分镜自动装配。
type AssembleInput struct {
	WorkspaceID int64
	// Editor 这次写入是谁、从哪条路来的（SCD-16）。
	Editor     repo.Editor
	TimelineID int64
	Version    int
	// Force 重建自动装配出来的段；手工加的段永远不动。
	Force bool
	// DryRun 只看计划、不写库。
	//
	// ⚠️ 装配会改用户的时间线，所以要能**先看再决定** ——
	// 尤其是有缺口的时候，他可能想先去把那几镜补出来。
	DryRun bool
}

type AssembleResult struct {
	Plan domain.AssemblePlan
	View View
}

func (s *Service) Assemble(ctx context.Context, in AssembleInput) (AssembleResult, error) {
	t, err := s.repo.Get(ctx, in.WorkspaceID, in.TimelineID)
	if err != nil {
		return AssembleResult{}, translate(err)
	}
	if t.ScriptID == 0 {
		return AssembleResult{}, httpx.Errf(errcode.Conflict,
			"这条时间线没有关联脚本，无法按分镜装配")
	}
	shots, err := s.shotClips(ctx, in.WorkspaceID, t.ScriptID)
	if err != nil {
		return AssembleResult{}, err
	}

	track, err := s.videoTrack(ctx, t.ID)
	if err != nil {
		return AssembleResult{}, err
	}
	clips, err := s.repo.Clips(ctx, track.ID)
	if err != nil {
		return AssembleResult{}, httpx.Wrap(errcode.Internal, err)
	}

	plan := domain.PlanAssemble(shots, clips, in.Force)
	if in.DryRun {
		view, err := s.load(ctx, t)
		if err != nil {
			return AssembleResult{}, err
		}
		return AssembleResult{Plan: plan, View: view}, nil
	}

	kept := clips
	if in.Force {
		// 重建时只删**自动装配出来的**那些段；手工加的永远不动。
		kept = kept[:0]
		for _, c := range clips {
			if c.SourceShotID == 0 {
				kept = append(kept, c)
			}
		}
	}
	next := append(append([]domain.Clip{}, kept...), plan.Append...)
	view, err := s.commit(ctx, t, track.ID, in.Version, in.Editor, ReasonAssemble, domain.Sequence(next))
	if err != nil {
		return AssembleResult{}, err
	}
	return AssembleResult{Plan: plan, View: view}, nil
}

// shotClips 取脚本每一镜可装配的视频与时长（装配与「还没有时间线时的预览」共用这一份）。
func (s *Service) shotClips(ctx context.Context, workspaceID, scriptID int64) ([]domain.ShotClip, error) {
	if s.shots == nil {
		return nil, httpx.Errf(errcode.Internal, "脚本来源未接入")
	}
	shots, err := s.shots.ShotClips(ctx, workspaceID, scriptID)
	if err != nil {
		return nil, err
	}
	// ⚠️ **时长归媒体域查，不归脚本域**。让脚本域顺手把 duration_ms 带出来的话，
	// "这份素材属不属于这个工作区"这条会在两个域各判一遍，而两遍迟早不一致。
	//
	// ⚠️ 这一步漏掉的症状极其具体：每一镜都被判成"素材时长未知"，
	// 于是装配永远装不进任何东西 —— 而单测里用假实现填了时长，一路是绿的
	// （P10-01 实测，是联调抓出来的）。
	for i := range shots {
		if shots[i].MediaID == 0 {
			continue
		}
		info, err := s.lookupMedia(ctx, workspaceID, shots[i].MediaID)
		if err != nil {
			// 素材查不到（被删了）也是缺口，而不是整次装配失败。
			shots[i].MediaID = 0
			shots[i].Reason = "视频素材已不存在"
			continue
		}
		shots[i].DurationMs = info.DurationMs
	}
	return shots, nil
}

// PreviewAssemble 是脚本**还没有时间线**时的装配预览（MCPT-16：外部客户端的 timeline.assemble 默认空跑，
// 空跑不能为了看一眼计划就先建一条时间线）。与 Assemble 同一个 PlanAssemble，已有片段视为空。只读。
func (s *Service) PreviewAssemble(ctx context.Context, workspaceID, scriptID int64, force bool) (
	domain.AssemblePlan, error) {
	shots, err := s.shotClips(ctx, workspaceID, scriptID)
	if err != nil {
		return domain.AssemblePlan{}, err
	}
	return domain.PlanAssemble(shots, nil, force), nil
}

// SaveSubtitles 整批替换字幕。
func (s *Service) SaveSubtitles(ctx context.Context, workspaceID, timelineID int64,
	version int, ed repo.Editor, subs []repo.Subtitle) (View, error) {
	if _, err := s.repo.Get(ctx, workspaceID, timelineID); err != nil {
		return View{}, translate(err)
	}
	if err := validateSubtitles(subs); err != nil {
		return View{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if _, err := s.repo.SaveSubtitles(ctx, timelineID, version, ed, subs); err != nil {
		return View{}, s.writeErr(ctx, timelineID, ed, err)
	}
	return s.written(ctx, workspaceID, timelineID, ReasonSubtitles)
}

// UpdateTrack 改轨道的静音与音量。
func (s *Service) UpdateTrack(ctx context.Context, workspaceID, timelineID,
	trackID int64, version int, ed repo.Editor, muted bool, volume int) (View, error) {
	if _, err := s.repo.Get(ctx, workspaceID, timelineID); err != nil {
		return View{}, translate(err)
	}
	if volume < 0 || volume > 200 {
		return View{}, httpx.Errf(errcode.InvalidArgument, "音量只支持 0~200")
	}
	if _, err := s.repo.UpdateTrack(ctx, timelineID, trackID, version, ed, muted, volume); err != nil {
		return View{}, s.writeErr(ctx, timelineID, ed, err)
	}
	return s.written(ctx, workspaceID, timelineID, ReasonEdit)
}

// UpdateMeta 改名与画幅。
func (s *Service) UpdateMeta(ctx context.Context, workspaceID, timelineID int64,
	version int, ed repo.Editor, name, aspect string, fps, width, height int) (View, error) {
	t, err := s.repo.Get(ctx, workspaceID, timelineID)
	if err != nil {
		return View{}, translate(err)
	}
	if name == "" {
		name = t.Name
	}
	if aspect == "" {
		aspect = t.AspectRatio
	}
	if fps <= 0 {
		fps = t.FPS
	}
	if width <= 0 || height <= 0 {
		width, height = t.Width, t.Height
	}
	if err := validateFormat(aspect, fps, width, height); err != nil {
		return View{}, err
	}
	if _, err := s.repo.UpdateMeta(ctx, timelineID, version, ed, name, aspect,
		fps, width, height); err != nil {
		return View{}, s.writeErr(ctx, timelineID, ed, err)
	}
	return s.written(ctx, workspaceID, timelineID, ReasonEdit)
}

// ---------------------------------------------------------------- 内部

func (s *Service) commit(ctx context.Context, t repo.Timeline, trackID int64,
	version int, ed repo.Editor, reason string, clips []domain.Clip) (View, error) {
	if _, err := s.repo.SaveClips(ctx, t.ID, trackID, version, ed, clips); err != nil {
		return View{}, s.writeErr(ctx, t.ID, ed, err)
	}
	return s.written(ctx, t.WorkspaceID, t.ID, reason)
}

// written 写入提交之后：重新读一遍时间线，发 timeline.updated（每条写路径的唯一出口，SCD-19）。
//
// ⚠️ 读失败也不影响「已经写成功」这件事 —— 但那时拿不到新版本号，事件也就不发了：
// 发一个版本号不对的对账信号，比不发更糟（前端会拿它去比本地版本）。
func (s *Service) written(ctx context.Context, workspaceID, timelineID int64, reason string) (View, error) {
	v, err := s.Get(ctx, workspaceID, timelineID)
	if err != nil {
		return View{}, err
	}
	s.publishUpdated(ctx, v.Timeline, reason)
	return v, nil
}

// writeErr 写路径的错误：版本冲突带上「是谁改的」（SCD-16），其余照 translate。
func (s *Service) writeErr(ctx context.Context, timelineID int64, ed repo.Editor, err error) error {
	if errors.Is(err, repo.ErrVersionConflict) {
		return s.conflictError(ctx, timelineID, ed)
	}
	return translate(err)
}

func (s *Service) owned(ctx context.Context, workspaceID, timelineID, trackID int64) (
	repo.Timeline, repo.Track, error) {
	t, err := s.repo.Get(ctx, workspaceID, timelineID)
	if err != nil {
		return repo.Timeline{}, repo.Track{}, translate(err)
	}
	track, err := s.repo.Track(ctx, timelineID, trackID)
	if err != nil {
		return repo.Timeline{}, repo.Track{}, translate(err)
	}
	return t, track, nil
}

func (s *Service) locate(ctx context.Context, workspaceID, timelineID, trackID,
	clipID int64) (repo.Timeline, repo.Track, []domain.Clip, domain.Clip, error) {
	t, track, err := s.owned(ctx, workspaceID, timelineID, trackID)
	if err != nil {
		return repo.Timeline{}, repo.Track{}, nil, domain.Clip{}, err
	}
	clips, err := s.repo.Clips(ctx, track.ID)
	if err != nil {
		return repo.Timeline{}, repo.Track{}, nil, domain.Clip{},
			httpx.Wrap(errcode.Internal, err)
	}
	clips = place(track.Kind, clips)
	for _, c := range clips {
		if c.ID == clipID {
			return t, track, clips, c, nil
		}
	}
	return repo.Timeline{}, repo.Track{}, nil, domain.Clip{}, httpx.Err(errcode.NotFound)
}

func (s *Service) videoTrack(ctx context.Context, timelineID int64) (repo.Track, error) {
	tracks, err := s.repo.Tracks(ctx, timelineID)
	if err != nil {
		return repo.Track{}, httpx.Wrap(errcode.Internal, err)
	}
	for _, t := range tracks {
		if t.Kind == domain.TrackVideo {
			return t, nil
		}
	}
	return repo.Track{}, httpx.Errf(errcode.Internal, "这条时间线没有画面轨")
}

func (s *Service) lookupMedia(ctx context.Context, workspaceID, mediaID int64) (
	MediaInfo, error) {
	if s.media == nil {
		return MediaInfo{}, httpx.Errf(errcode.Internal, "媒体来源未接入")
	}
	info, err := s.media.Lookup(ctx, workspaceID, mediaID)
	if err != nil {
		// 不区分"不存在"与"不是你的"：区分开等于提供一个按 id 探测的接口。
		return MediaInfo{}, httpx.Errf(errcode.NotFound, "素材不存在或不属于当前工作区")
	}
	return info, nil
}

// place 按轨道种类算落点：画面轨按顺序与转场排（Reflow），其它轨按起点放置、起点不动（SCD-15）。
//
// ⚠️ 读出来也要算一次：落点是算出来的，不是存出来的。库里那份可能是上一次写入时的快照，
// 变速 / 裁切改过之后就不对了。以算出来的为准，库里那份只是缓存。
func place(kind string, clips []domain.Clip) []domain.Clip {
	if kind == domain.TrackVideo {
		return domain.Reflow(clips)
	}
	return domain.PlaceByStart(clips)
}

// rearrange 改完一段之后重新排：画面轨重排落点；其它轨按起点放置并检查同轨重叠与成片上限。
func rearrange(kind string, clips []domain.Clip) ([]domain.Clip, error) {
	if kind == domain.TrackVideo {
		return domain.Reflow(clips), nil
	}
	return placed(clips)
}

// placed 按起点放置的轨道：排好序、检查同轨不重叠、不超出成片最长时长（SCD-15、SC-PARAM-21）。
func placed(clips []domain.Clip) ([]domain.Clip, error) {
	next := domain.PlaceByStart(clips)
	if i := domain.OverlapIndex(next); i >= 0 {
		return nil, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf(
			"与同一条音频轨上的另一段重叠（%d 毫秒起）", next[i].StartMs)).
			WithDetails(map[string]any{"path": "startMs"})
	}
	if end := domain.TrackEnd(next); end > domain.MaxTimelineMs {
		return nil, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf(
			"超出成片最长 %d 分钟", domain.MaxTimelineMs/60000)).WithDetails(map[string]any{"path": "startMs"})
	}
	return next, nil
}

func replace(clips []domain.Clip, next domain.Clip) []domain.Clip {
	out := make([]domain.Clip, len(clips))
	copy(out, clips)
	for i := range out {
		if out[i].ID == next.ID {
			out[i] = next
		}
	}
	return out
}

// checkKind 轨道只收对得上的素材。
//
// ⚠️ 不拦的话一段音频会被放进画面轨，渲染时 ffmpeg 拿它当视频输入 ——
// 出来的成片那一段是黑屏，而任务是"成功"的。
func checkKind(trackKind, mediaKind string) error {
	switch trackKind {
	case domain.TrackVideo:
		if mediaKind != "video" && mediaKind != "image" {
			return fmt.Errorf("画面轨只接受视频或图片，这份素材是%s", label(mediaKind))
		}
	case domain.TrackAudio:
		if mediaKind != "audio" {
			return fmt.Errorf("音频轨只接受音频，这份素材是%s", label(mediaKind))
		}
	case domain.TrackSubtitle:
		return fmt.Errorf("字幕轨不放素材，直接编辑字幕文本")
	case domain.TrackText:
		return fmt.Errorf("文字轨不放素材，直接编辑文字")
	}
	return nil
}

func label(kind string) string {
	switch kind {
	case "video":
		return "视频"
	case "audio":
		return "音频"
	case "image":
		return "图片"
	}
	return "未知类型"
}

// defaultSize 画幅对应的默认尺寸。取值只有一份：timeline/domain.AspectPresets
// （前端那一份由 api/fixtures/smartcut/content/aspects.json 绊住，SC-N-15）。
//
// 未知画幅与 custom 回落横屏 —— 这里不该报错，validateFormat 会拦；
// custom 的尺寸必须由调用方给（Create 里先拦）。
func defaultSize(aspect string) (int, int) {
	if w, h, ok := domain.DefaultSize(aspect); ok {
		return w, h
	}
	return 1920, 1080
}

// validateFormat 校验画幅、帧率与尺寸（规则在 domain.ValidateFormat）：
// 宽高要是偶数（yuv420p 的要求，否则 ffmpeg 在用户等完一整轮渲染之后才报错）；
// 固定画幅要求宽高自洽（容差 2%，留给编码器对齐）—— 声明 9:16 却给 1920×1080 的话，
// 界面按画幅画预览框、ffmpeg 按尺寸出片，用户看到的预览与拿到的成片是两个形状。
func validateFormat(aspect string, fps, width, height int) error {
	if ve := domain.ValidateFormat(aspect, fps, width, height); ve != nil {
		return httpx.Errf(errcode.InvalidArgument, ve.Reason).WithDetails(map[string]any{"path": ve.Path})
	}
	return nil
}

// validateSubtitles 字幕的时间轴校验。
func validateSubtitles(subs []repo.Subtitle) error {
	for i, s := range subs {
		if s.EndMs <= s.StartMs {
			return fmt.Errorf("第 %d 条字幕的结束时间不晚于开始时间", i+1)
		}
		if s.Text == "" {
			return fmt.Errorf("第 %d 条字幕没有内容", i+1)
		}
		// ⚠️ 允许重叠但要拦住**完全相同的时间段**：重叠是正当的
		// （两个人同时说话），而完全相同多半是重复提交。
		if i > 0 && subs[i-1].StartMs == s.StartMs && subs[i-1].EndMs == s.EndMs {
			return fmt.Errorf("第 %d 条与上一条的时间完全相同", i+1)
		}
	}
	return nil
}

func translate(err error) error {
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Err(errcode.NotFound)
	case errors.Is(err, repo.ErrVersionConflict):
		// ⚠️ 冲突要让前端**保住本地草稿并提示重载**，不能静默丢弃
		// （与画布保存同一条，GAP/FLOW-09）。
		return httpx.Errf(errcode.VersionConflict,
			"时间线已被改动，请重新加载后再试")
	case err != nil:
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}
