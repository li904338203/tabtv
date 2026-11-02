package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	billingdomain "github.com/li904338203/tabtv/backend/internal/billing/domain"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 渲染受理（NEW-10）。
//
// ⚠️ 顺序是**预检 → 报价 → 扣费 → 写任务 + outbox（同一个事务）**。
// 与生成受理逐条对齐 —— 不是照抄，而是同一条规则：
// 一条跑不起来的时间线要在**扣费之前**被拦住，否则用户看到的是
// 一次莫名其妙的扣费与退款，而他什么都没得到。

// RenderTopic 是渲染任务的 outbox topic。
const RenderTopic = "render.requested"

// RenderPayload 投递给 Worker 的消息体。只带 ID —— 快照已经在库里冻结了。
type RenderPayload struct {
	RenderID string `json:"renderId"`
}

// CreditReserver 由账本域实现。
type CreditReserver interface {
	ConsumeInTx(ctx context.Context, tx pgx.Tx, in billingrepo.ConsumeInput) (
		billingrepo.ConsumeResult, error)
}

// RenderQuoter 由目录域实现：按成片时长报价。
//
// ⚠️ 走**同一套定价目录**（operation + pricing），不另写一份价格：
// 另写的话"渲染多少钱"会有两个答案，而运营只会改其中一个。
type RenderQuoter interface {
	QuoteRender(ctx context.Context, durationSec int, memberLevelID int64) (int, error)
}

// UserFlags 由身份域实现：取影响渲染结果的用户开关。
//
// ⚠️ **必须服务端查，不能从请求体收**。水印是「AI 生成内容」标识，
// 让客户端自己报「我关了水印」等于把这个标识变成谁都能关的可选项
// —— 与 GO-27 同一条规则：凡影响产出的东西都按可信来源解析，
// 而不是信客户端报的数。
type UserFlags interface {
	WatermarkOff(ctx context.Context, userID int64) (bool, error)
}

func (s *Service) SetUserFlags(f UserFlags) { s.userFlags = f }

// watermarkOff 查不到时**按"不关"处理**（照常加水印）。
//
// ⚠️ 方向不能反：查不到就不加的话，一次数据库抖动会让那批成片
// 悄悄少了标识，而事后无从辨认。加错了看得见，漏了看不见。
func (s *Service) watermarkOff(ctx context.Context, userID int64) bool {
	if s.userFlags == nil || userID == 0 {
		return false
	}
	off, err := s.userFlags.WatermarkOff(ctx, userID)
	if err != nil {
		s.warn(ctx, "取用户水印开关失败，按加水印处理", "userId", userID, "error", err)
		return false
	}
	return off
}

func (s *Service) SetBilling(pool *pgxpool.Pool, r CreditReserver) {
	s.pool = pool
	s.credits = r
}
func (s *Service) SetRenderQuoter(q RenderQuoter) { s.renderQuote = q }
func (s *Service) SetNextID(f func() int64)       { s.nextID = f }
func (s *Service) SetFFmpegAvailable(ok bool)     { s.ffmpegOK = ok }

// SetEncoderAvailable 注入「本机有没有可用的 H.264 编码器」（TT-T-27）；不注入 = 视为有。
// 按次求值而不是启动时定死：探测结果有缓存，换了 ffmpeg 之后会重新探测。
func (s *Service) SetEncoderAvailable(fn func() bool) { s.encoderOK = fn }

// RenderPrecheck 预检结果（也用于 dryRun 报价）。
type RenderPrecheck struct {
	Issues      []domain.PrecheckIssue
	DurationMs  int
	Credits     int
	ClipCount   int
	SubtitleCnt int
}

// Runnable 能不能跑。
func (p RenderPrecheck) Runnable() bool { return len(p.Issues) == 0 }

type RenderInput struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	TimelineID     int64
	MemberLevelID  int64
	// DryRun 只预检与报价，不扣费、不建任务。
	//
	// ⚠️ 渲染是这条链上最贵的一步（时间与积分都是），所以必须能**先看再决定**。
	DryRun bool
	// IdempotencyKey 防重复提交（SCB-3：同一工作区同一把键只建一个任务）。
	IdempotencyKey string
	// Resolution / FPS 导出规格（SCT-15）：空 / 0 = 按时间线自身的宽高与帧率。
	Resolution string
	FPS        int
}

// RenderLimits 导出按用户限流（SCK-3：导出免费，SC-PARAM-19 设计建议值）。≤0 表示不限。
type RenderLimits struct {
	UserConcurrency int
	UserPerHour     int
}

// SetRenderLimits 装上导出限流。
func (s *Service) SetRenderLimits(l RenderLimits) { s.renderLimits = l }

// PrecheckRender 预检并报价。
func (s *Service) PrecheckRender(ctx context.Context, in RenderInput) (
	RenderPrecheck, domain.RenderSnapshot, repo.Timeline, error) {
	if in.Resolution != "" {
		if _, ok := domain.ExportResolutions[in.Resolution]; !ok {
			return RenderPrecheck{}, domain.RenderSnapshot{}, repo.Timeline{},
				httpx.Errf(errcode.InvalidArgument, "导出分辨率只支持 720p / 1080p").WithDetails(map[string]any{"path": "resolution"})
		}
	}
	if in.FPS != 0 && !domain.ExportFPS[in.FPS] {
		return RenderPrecheck{}, domain.RenderSnapshot{}, repo.Timeline{},
			httpx.Errf(errcode.InvalidArgument, "导出帧率只支持 24 / 25 / 30").WithDetails(map[string]any{"path": "fps"})
	}
	t, err := s.repo.Get(ctx, in.WorkspaceID, in.TimelineID)
	if err != nil {
		return RenderPrecheck{}, domain.RenderSnapshot{}, repo.Timeline{}, translate(err)
	}
	v, err := s.load(ctx, t)
	if err != nil {
		return RenderPrecheck{}, domain.RenderSnapshot{}, repo.Timeline{}, err
	}

	// ⚠️ 素材存在性**逐个查**：排队期间被删是完全可能的，
	// 而拿着一个失效的 ID 去渲染会得到一段黑屏、任务却是"成功"的。
	alive := map[int64]MediaInfo{}
	dead := map[int64]bool{}
	look := func(id int64) (MediaInfo, bool) {
		if info, ok := alive[id]; ok {
			return info, true
		}
		if dead[id] {
			return MediaInfo{}, false
		}
		info, err := s.media.Lookup(ctx, in.WorkspaceID, id)
		if err != nil {
			dead[id] = true
			return MediaInfo{}, false
		}
		alive[id] = info
		return info, true
	}
	snap, generating, err := s.snapshot(ctx, v, in, look, s.watermarkOff(ctx, in.UserID))
	if err != nil {
		return RenderPrecheck{}, domain.RenderSnapshot{}, repo.Timeline{}, err
	}
	issues := domain.Precheck(snap, domain.PrecheckEnv{
		MediaOK: func(id int64) bool { _, ok := look(id); return ok }, FFmpegOK: s.ffmpegOK, GeneratingClips: generating,
		NoEncoder: s.ffmpegOK && s.encoderOK != nil && !s.encoderOK(),
	})

	subs := 0
	for _, st := range snap.SubtitleTracks {
		subs += len(st.Items)
	}
	out := RenderPrecheck{
		Issues: issues, DurationMs: domain.SnapDuration(snap),
		ClipCount: len(snap.Clips), SubtitleCnt: subs,
	}
	if s.renderQuote != nil && out.Runnable() {
		// 按**秒**报价，不足一秒按一秒（智能剪辑的兜底价是 0：导出免费，SCK-3）。
		secs := (out.DurationMs + 999) / 1000
		credits, err := s.renderQuote.QuoteRender(ctx, secs, in.MemberLevelID)
		if err != nil {
			return RenderPrecheck{}, snap, t, err
		}
		out.Credits = credits
	}
	return out, snap, t, nil
}

// StartRender 受理一次渲染。
//
// 顺序：幂等（已有就返回那一条，不计入限流）→ 预检 → 报价 → 事务里「用户锁 → 限流 → 扣费 → 写任务 + outbox」。
func (s *Service) StartRender(ctx context.Context, in RenderInput) (
	repo.RenderJob, RenderPrecheck, error) {
	if !in.DryRun && in.IdempotencyKey != "" {
		if j, err := s.repo.RenderByIdem(ctx, in.WorkspaceID, in.IdempotencyKey); err == nil {
			// ⚠️ 同一把键重复提交（双击、网络重试）返回同一个任务，不再建、不再计入限流（SCB-3）。
			return j, RenderPrecheck{DurationMs: domain.SnapDuration(snapOf(j))}, nil
		} else if !errors.Is(err, repo.ErrNotFound) {
			return repo.RenderJob{}, RenderPrecheck{}, httpx.Wrap(errcode.Internal, err)
		}
	}
	pre, snap, t, err := s.PrecheckRender(ctx, in)
	if err != nil {
		return repo.RenderJob{}, RenderPrecheck{}, err
	}
	if in.DryRun {
		return repo.RenderJob{}, pre, nil
	}
	if !pre.Runnable() {
		// ⚠️ 把**每一条**原因回给用户，不是一句"无法渲染" ——
		// 他要知道是哪一段缺素材才改得动。
		return repo.RenderJob{}, pre, httpx.Errf(errcode.InvalidArgument,
			pre.Issues[0].Detail).WithDetails(map[string]any{"issues": pre.Issues})
	}
	if s.pool == nil || s.credits == nil || s.nextID == nil {
		return repo.RenderJob{}, pre, httpx.Errf(errcode.Internal, "渲染未接入账本")
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		return repo.RenderJob{}, pre, httpx.Wrap(errcode.Internal, err)
	}
	specMap := map[string]any{"fps": snap.FPS, "width": snap.Width, "height": snap.Height}
	if in.Resolution != "" {
		specMap["resolution"] = in.Resolution
	}
	spec, _ := json.Marshal(specMap)
	// ⚠️ 死线按成片时长的倍数算并**存下来**：恢复扫描不得按当前配置重算旧任务
	// （与生成任务同一条，GO-24）。编码大约是实时的几分之一，但素材下载、
	// 归档都要时间 —— 取 3 倍再加 2 分钟的底。领取时也不改它（SCB-4）。
	deadline := time.Now().Add(
		time.Duration(pre.DurationMs*3)*time.Millisecond + 2*time.Minute)

	job := repo.RenderJob{
		ID: s.nextID(), WorkspaceID: in.WorkspaceID, TimelineID: t.ID,
		TimelineVersion: t.Version, Snapshot: raw,
		PayerAccountID: in.PayerAccountID, CreatedByUserID: in.UserID,
		CreditsCost: pre.Credits, DeadlineAt: &deadline,
		IdempotencyKey: in.IdempotencyKey, Spec: spec,
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return repo.RenderJob{}, pre, httpx.Wrap(errcode.Internal, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.checkUserLimits(ctx, tx, in.UserID); err != nil {
		return repo.RenderJob{}, pre, err
	}
	if pre.Credits > 0 {
		// ⚠️ 幂等由账本的 `(ref_type, ref_id)` 唯一约束兜底：同一次渲染
		// 重复提交只会扣一次。refId 用渲染任务 ID —— 它在这个事务里刚生成，
		// 所以"重复提交"实际是被上层的幂等键挡住的；这里是第二道。
		if _, err := s.credits.ConsumeInTx(ctx, tx, billingrepo.ConsumeInput{
			PayerID: in.PayerAccountID, Amount: pre.Credits,
			RefType: "render", RefID: fmt.Sprint(job.ID),
			UserID: in.UserID, Reason: "时间线渲染",
		}); err != nil {
			return repo.RenderJob{}, pre, translateBilling(err)
		}
	}
	payload, _ := json.Marshal(RenderPayload{RenderID: fmt.Sprint(job.ID)})
	if err := s.repo.InsertRenderInTx(ctx, tx, job, s.nextID(),
		RenderTopic, payload); err != nil {
		if errors.Is(err, repo.ErrRenderIdemTaken) {
			// 两个同键请求并发：唯一索引挡住后一个，回查前一个建好的那条返回。
			_ = tx.Rollback(ctx)
			if j, err := s.repo.RenderByIdem(ctx, in.WorkspaceID, in.IdempotencyKey); err == nil {
				return j, pre, nil
			}
		}
		return repo.RenderJob{}, pre, httpx.Wrap(errcode.Internal, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return repo.RenderJob{}, pre, httpx.Wrap(errcode.Internal, err)
	}
	job.Status = domain.RenderQueued
	job.ProjectID, job.CanvasNodeID = t.ProjectID, t.CanvasNodeID
	s.publishRender(ctx, job)
	return job, pre, nil
}

// checkUserLimits 导出按用户限流（SCK-3）：同时进行（排队 + 渲染中）与每小时次数。
// 在受理事务里先拿用户锁再数 —— 两次并发受理各数到 0、各建一个的话，「同时只能 1 个」就破了。
func (s *Service) checkUserLimits(ctx context.Context, tx pgx.Tx, userID int64) error {
	l := s.renderLimits
	if userID == 0 || (l.UserConcurrency <= 0 && l.UserPerHour <= 0) {
		return nil
	}
	if err := s.repo.LockUserRenders(ctx, tx, userID); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	active, lastHour, oldest, err := s.repo.UserRenderLoad(ctx, tx, userID)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if l.UserConcurrency > 0 && active >= l.UserConcurrency {
		return httpx.Errf(errcode.ConcurrencyLimited,
			fmt.Sprintf("还有导出在进行（同时最多 %d 个），等它完成后再导出", l.UserConcurrency)).
			WithDetails(map[string]any{"retry_after_seconds": 10, "scope": "concurrency", "limit": l.UserConcurrency})
	}
	if l.UserPerHour > 0 && lastHour >= l.UserPerHour {
		secs := int(time.Until(oldest.Add(time.Hour)).Seconds()) + 1
		if secs < 1 {
			secs = 1
		}
		return httpx.Errf(errcode.RateLimited,
			fmt.Sprintf("这一小时的导出次数已用完（每小时 %d 次），约 %d 分钟后再试", l.UserPerHour, (secs+59)/60)).
			WithDetails(map[string]any{"retry_after_seconds": secs, "scope": "hourly", "limit": l.UserPerHour})
	}
	return nil
}

// snapOf 读出任务里冻结的快照（坏了当空的：只用来回一个成片时长）。
func snapOf(j repo.RenderJob) domain.RenderSnapshot {
	var snap domain.RenderSnapshot
	_ = json.Unmarshal(j.Snapshot, &snap)
	return snap
}

// RenderDownloads 由媒体域实现：给成片签一个「下载为附件」的短期直链（先过归属再签）。
type RenderDownloads interface {
	SignedDownloadURL(ctx context.Context, workspaceID, mediaID int64, ttl time.Duration, fileName string) (
		string, time.Time, error)
}

// SetRenderDownloads 装上下载链接端口与有效期（SEEAITV_RENDER_DOWNLOAD_TTL，默认 5 分钟）。
func (s *Service) SetRenderDownloads(d RenderDownloads, ttl time.Duration) {
	s.downloads, s.downloadTTL = d, ttl
}

// RenderDownload 成片的下载链接（GET /renders/{id}/download，SCK-10）：只对成功的任务；链接 5 分钟有效、
// 只对这一个文件有效，浏览器直接下载，不把整份成片读进内存（媒体下载端点是整份读的）。
//
// ⚠️ 链接本身就是凭据：不进日志、不缓存（前端在点击的那一刻再来取）。
func (s *Service) RenderDownload(ctx context.Context, workspaceID, id int64) (string, time.Time, string, error) {
	j, err := s.repo.Render(ctx, workspaceID, id)
	if err != nil {
		return "", time.Time{}, "", translate(err)
	}
	if j.Status != domain.RenderSuccess || j.OutputMediaID == 0 {
		return "", time.Time{}, "", httpx.Errf(errcode.Conflict, "这次导出还没有完成，没有可下载的成片")
	}
	if s.downloads == nil {
		return "", time.Time{}, "", httpx.Errf(errcode.Internal, "下载链接未接入")
	}
	name := "成片"
	if t, err := s.repo.Get(ctx, workspaceID, j.TimelineID); err == nil && strings.TrimSpace(t.Name) != "" {
		name = t.Name
	}
	fileName := safeFileName(name) + "-v" + fmt.Sprint(j.TimelineVersion) + ".mp4"
	url, exp, err := s.downloads.SignedDownloadURL(ctx, workspaceID, j.OutputMediaID, s.downloadTTL, fileName)
	if err != nil {
		return "", time.Time{}, "", err
	}
	return url, exp, fileName, nil
}

// safeFileName 去掉文件名里不能用的字符（路径分隔符、控制字符），最长 60 个字。
func safeFileName(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if n >= 60 {
			break
		}
		switch {
		case r < 0x20, r == '/', r == '\\', r == ':', r == '*', r == '?', r == '"', r == '<', r == '>', r == '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
		n++
	}
	if b.Len() == 0 {
		return "成片"
	}
	return b.String()
}

func (s *Service) GetRender(ctx context.Context, workspaceID, id int64) (
	repo.RenderJob, error) {
	j, err := s.repo.Render(ctx, workspaceID, id)
	if err != nil {
		return repo.RenderJob{}, translate(err)
	}
	return j, nil
}

func (s *Service) ListRenders(ctx context.Context, workspaceID, timelineID int64,
	limit int) ([]repo.RenderJob, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	list, err := s.repo.ListRenders(ctx, workspaceID, timelineID, limit)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// CancelRender 取消。
//
// ⚠️ 取消要**退款**，且退款只能成功一次 —— `CancelRender` 返回
// "是不是本次真的改成了 cancelled"，据此决定退不退。
// 不看这个返回值的话，连点两次取消就退两次钱。
func (s *Service) CancelRender(ctx context.Context, workspaceID, id int64) (
	repo.RenderJob, error) {
	j, err := s.repo.Render(ctx, workspaceID, id)
	if err != nil {
		return repo.RenderJob{}, translate(err)
	}
	changed, err := s.repo.CancelRender(ctx, workspaceID, id)
	if err != nil {
		return repo.RenderJob{}, httpx.Wrap(errcode.Internal, err)
	}
	if !changed {
		// 明确告诉用户"已经跑完了"，而不是静默成功然后让他等一个不会来的取消。
		return repo.RenderJob{}, httpx.Errf(errcode.Conflict,
			"这次渲染已经结束，取消不了了")
	}
	if j.CreditsCost > 0 && s.refunder != nil {
		if err := s.refunder.Refund(ctx, j.PayerAccountID, "render",
			fmt.Sprint(j.ID), j.CreditsCost, "渲染已取消"); err != nil {
			// ⚠️ 退款失败**只告警不回滚取消**：任务已经不跑了，
			// 把它拉回 running 更糟。退款由对账兜底。
			s.warn(ctx, "渲染取消后退款失败", "renderId", j.ID, "error", err)
		}
	}
	out, err := s.GetRender(ctx, workspaceID, id)
	if err == nil {
		// Worker 那边每秒查一次状态，看到 cancelled 就停掉 ffmpeg（SCB-5）；这里先把终态推给前端。
		s.publishRender(ctx, out)
	}
	return out, err
}

// Refunder 由账本域实现。
type Refunder interface {
	Refund(ctx context.Context, payerID int64, refType, refID string,
		amount int, reason string) error
}

func (s *Service) SetRefunder(r Refunder) { s.refunder = r }

// snapshot 把当前时间线冻结成渲染快照（Schema 2，SCD-27）。返回还在片段级生成的段号（预检用）。
//
// 时间一律是成片时间：画面轨按顺序与转场（load 已经排好），音频轨按存储的起点（不首尾相接，SCB-2），
// 跟随片段的字幕按片段当前的位置换算、被裁掉的不进快照（load 已经算好 Hidden）。
func (s *Service) snapshot(ctx context.Context, v View, in RenderInput,
	look func(int64) (MediaInfo, bool), watermarkOff bool) (domain.RenderSnapshot, []int, error) {
	t := v.Timeline
	w, h := domain.ExportSize(t.AspectRatio, t.Width, t.Height, in.Resolution)
	fps := t.FPS
	if in.FPS != 0 {
		fps = in.FPS
	}
	snap := domain.RenderSnapshot{
		Schema: domain.SnapshotSchema, TimelineID: t.ID, TimelineVersion: t.Version,
		Width: w, Height: h, FPS: fps, Resolution: in.Resolution, VideoVolume: domain.DefaultVolume,
	}
	var generating []int
	videoSeen, subOrder, textOrder := false, 0, 0
	for _, tv := range v.Tracks {
		tr := tv.Track
		switch tr.Kind {
		case domain.TrackVideo:
			if videoSeen {
				continue // 画面轨只有一条（ValidateContent 拦着）；多出来的历史数据不渲染
			}
			videoSeen = true
			snap.VideoRole, snap.VideoMuted, snap.VideoVolume = tr.Role, tr.IsMuted, tr.Volume
			for i, c := range tv.Clips {
				sc := toSnap(c)
				if info, ok := look(c.MediaID); ok && info.Kind == "image" {
					sc.Kind = "image"
				} else {
					sc.Kind = "video"
				}
				sc.Transform = domain.ParseTransform(c.Transform)
				if i == len(tv.Clips)-1 && c.TransitionOut.IsActive() {
					out := c.TransitionOut
					sc.TransitionOut = &out
				}
				if c.PendingGenerationID != 0 {
					generating = append(generating, i+1)
				}
				snap.Clips = append(snap.Clips, sc)
			}
		case domain.TrackAudio:
			role := tr.Role
			if role == "" {
				role = domain.RoleMusic
			}
			at := domain.SnapAudioTrack{Role: role, Muted: tr.IsMuted, Volume: tr.Volume, Duck: duckOf(tr.Style)}
			for _, c := range tv.Clips {
				if c.Hidden {
					continue // 跟随的那一段被裁掉了（SC-F-56）：隐藏不删除，不进成片
				}
				sc := toSnap(c)
				sc.Kind = "audio"
				at.Clips = append(at.Clips, sc)
			}
			snap.AudioTracks = append(snap.AudioTracks, at)
		case domain.TrackSubtitle:
			if tr.IsHidden {
				continue // 隐藏的字幕轨不进成片（剪辑台里的眼睛开关）
			}
			subOrder++
			st := domain.SnapSubtitleTrack{TrackID: tr.ID, Lang: tr.Lang, Style: domain.ObjectOrNil(tr.Style), Order: subOrder}
			for _, sub := range v.Subtitles {
				if sub.TrackID != tr.ID || sub.Hidden || sub.EndMs <= sub.StartMs {
					continue
				}
				st.Items = append(st.Items, domain.SnapSubItem{StartMs: sub.StartMs, EndMs: sub.EndMs, Text: sub.Text,
					Style: domain.ObjectOrNil(sub.Style)})
			}
			if len(st.Items) > 0 {
				snap.SubtitleTracks = append(snap.SubtitleTracks, st)
			}
		case domain.TrackText:
			if tr.IsHidden {
				continue
			}
			textOrder++
			for _, x := range v.Texts {
				if x.TrackID != tr.ID || x.EndMs <= x.StartMs {
					continue
				}
				snap.Texts = append(snap.Texts, domain.SnapText{TrackOrder: textOrder, StartMs: x.StartMs, EndMs: x.EndMs,
					Text: x.Text, Style: domain.ObjectOrNil(x.Style), Box: domain.ObjectOrNil(x.Box)})
			}
		}
	}
	// ⚠️ 水印配置**也要进快照**：不冻结的话，排队期间运营换了 logo，
	// 出来的片子带的是新 logo 而任务记的是旧的 —— 事后对不上。
	//
	// ⚠️ 用户关掉水印（`is_ai_watermark_off`）时不叠。这个开关是产品决定，
	// 不是这里能改的；但"这次到底加没加"要落到任务上（见 MarkWatermarked）。
	if !watermarkOff {
		set, err := s.repo.RenderSetting(ctx)
		if err != nil {
			return domain.RenderSnapshot{}, nil, httpx.Wrap(errcode.Internal, err)
		}
		if set.WatermarkMediaID != 0 {
			snap.Watermark = &domain.Watermark{
				MediaID: set.WatermarkMediaID, Position: set.Position,
				Opacity: set.Opacity, Scale: set.Scale,
				// ⚠️ 目标宽度按**导出画幅**算好传进去：单路滤镜链里拿不到
				// 主画面尺寸（那要 scale2ref，而它的两路输入顺序极易搞反，
				// 搞反了水印会被缩放到整屏大小压住全片）。
				PixelWidth: pixelWidth(w, set.Scale),
			}
		}
	}
	return snap, generating, nil
}

// duckOf 配乐轨 style 里的人声闪避设置；没设过返回 nil（快照里按缺省开启处理）。
func duckOf(style json.RawMessage) *domain.Duck {
	var st struct {
		Duck *struct {
			Enabled *bool `json:"enabled"`
			DepthDb *int  `json:"depthDb"`
		} `json:"duck"`
	}
	if len(style) == 0 || json.Unmarshal(style, &st) != nil || st.Duck == nil {
		return nil
	}
	d := domain.Duck{Enabled: true, DepthDb: domain.DefaultDuckDepthDb}
	if st.Duck.Enabled != nil {
		d.Enabled = *st.Duck.Enabled
	}
	if st.Duck.DepthDb != nil {
		d.DepthDb = *st.Duck.DepthDb
	}
	return &d
}

// pixelWidth 水印按画面宽度的百分比算目标宽度，并对齐到偶数。
//
// ⚠️ 对齐偶数：yuv420p 的色度平面是 2×2 采样，奇数宽的叠加层
// 在部分 ffmpeg 版本上会报 "width not divisible by 2"。
func pixelWidth(canvasWidth, scalePct int) int {
	if canvasWidth <= 0 {
		canvasWidth = 1920
	}
	if scalePct <= 0 {
		scalePct = 12
	}
	w := canvasWidth * scalePct / 100
	if w < 16 {
		w = 16
	}
	return w / 2 * 2
}

func toSnap(c domain.Clip) domain.SnapClip {
	return domain.SnapClip{
		MediaID: c.MediaID, SourceInMs: c.SourceInMs, SourceOutMs: c.SourceOutMs,
		StartMs: c.StartMs, DurationMs: c.DurationMs, Speed: c.Speed,
		Volume: c.Volume, FadeInMs: c.FadeInMs, FadeOutMs: c.FadeOutMs,
		Transition: c.TransitionIn,
	}
}

func translateBilling(err error) error {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return err
	}
	if errors.Is(err, billingdomain.ErrInsufficient) {
		return httpx.Errf(errcode.InsufficientCredits, "积分不足，无法渲染")
	}
	return httpx.Wrap(errcode.Internal, err)
}

// ---------------------------------------------------------------- 运维补偿

// OpsRenderFacts 是运维补偿判定所需的最小事实。
type OpsRenderFacts struct {
	Finished   bool
	DeadlineAt *time.Time
	Credits    int
}

// DescribeRenderForOps 取一条渲染任务的补偿判定事实。**不带工作区条件**。
//
// ⚠️ 后台运维没有工作区上下文（手上只有一个任务 ID），归属由 `ops:*`
// 权限位把关。复用 `RenderForRun` —— Worker 本来就只按 id 取。
func (s *Service) DescribeRenderForOps(ctx context.Context, id int64) (OpsRenderFacts, error) {
	j, err := s.repo.RenderForRun(ctx, id)
	if err != nil {
		return OpsRenderFacts{}, translate(err)
	}
	return OpsRenderFacts{
		Finished: j.Status == "success" || j.Status == "failed" || j.Status == "cancelled",
		// 排队中还没派发的任务没有死线，这里如实回 nil 而不是补一个 ——
		// 补出来的话判定会把"刚提交"当成"已超时"。
		DeadlineAt: j.DeadlineAt,
		Credits:    j.CreditsCost,
	}, nil
}

// CompensateRender 由运维把一条卡住的渲染判失败并退款。
//
// ⚠️ 走 `failed` 而不是 `cancelled`：取消是"用户决定不要了"，补偿是
// "平台判定它死了"。混用的话用户会在自己的列表里看到一条他从没取消过的
// "已取消"，而成功率指标里会多出一批不存在的用户取消。
//
// ⚠️ 看 `FailRender` 的返回值决定退不退款：ffmpeg 可能正好在这一刻跑完了，
// CAS 会拦下来 —— 不看的话就会给一条已经出片的任务退款。
func (s *Service) CompensateRender(ctx context.Context, id int64, reason string) (bool, error) {
	j, err := s.repo.RenderForRun(ctx, id)
	if err != nil {
		return false, translate(err)
	}
	changed, err := s.repo.FailRender(ctx, id, "ops_compensated", "运维补偿："+reason)
	if err != nil {
		return false, httpx.Wrap(errcode.Internal, err)
	}
	if !changed {
		return false, nil
	}
	if j.CreditsCost > 0 && s.refunder != nil {
		if err := s.refunder.Refund(ctx, j.PayerAccountID, "render", fmt.Sprint(j.ID),
			j.CreditsCost, "运维补偿："+reason); err != nil {
			// 退款失败只告警不回滚：任务已经不跑了，拉回 running 更糟。
			s.warn(ctx, "补偿退款失败，需对账补偿", "renderId", j.ID, "error", err)
		}
	}
	return true, nil
}
