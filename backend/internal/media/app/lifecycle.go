package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 媒体生命周期：受保护读取、缩略图、活动引用保护、孤儿回收。

// DefaultMediaRetention 软删后的保留期。**设计建议值**，不是已批准的政策。
const DefaultMediaRetention = 7 * 24 * time.Hour

// UsageType 是引用来源。各域登记引用时用这些常量，别各写各的字符串 ——
// 拼错一个字母的后果是"删除时查不到引用"，于是文件被删、任务随后失败。
const (
	UsageProjectCover  = "project_cover"
	UsageGenerationIn  = "generation_input"
	UsageGenerationOut = "generation_output"
	UsageScriptAsset   = "script_asset"
	UsageTimelineClip  = "timeline_clip"
	UsageMaterialCover = "material_cover"
	UsageVoiceSample   = "voice_sample"
	UsageWorkspaceAvat = "user_avatar"
	// UsageAgentSession / UsageAgentMarketing 只用于计算式检查器的 Kind（MKT-T-23），
	// 不登记 media_usage。分两个是为了文案分得开：营销会话 / Agent 会话。
	UsageAgentSession   = "agent_session"
	UsageAgentMarketing = "agent_session_marketing"
	// UsageMarketingProduct 只用于计算式检查器的 Kind（MKT-T-33）：营销商品库里**未删除**商品的图。
	UsageMarketingProduct = "marketing_product"
	// UsageMarketingCharacter 只用于计算式检查器的 Kind（MKT-T-34）：角色库里**未删除**角色的图片与参考音频。
	UsageMarketingCharacter = "marketing_character"
	// UsageDirectorScene 只用于计算式检查器的 Kind（3D 导演台 DRD-16）：场景正文引用的全景 / 泼溅，
	// 以及导演台媒体记录（截图、控制图、预演）。回收站里的项目同样算引用。
	UsageDirectorScene = "director_scene"
)

// Download 受保护读取。
//
// ⚠️ **归属校验在这里，不在调用方**。P03-04 的实现把校验写成注释交给调用方，
// 而 handler 并没有做 —— 任何登录用户按 id 就能取到别人工作区的文件（GO-27、FE-06）。
// 归属条件现在落在 SQL 里：查不到就是没有，不存在"忘了比对"这种可能。
func (s *Service) Download(ctx context.Context, workspaceID, mediaID int64, width int) (
	io.ReadCloser, repo.MediaObject, error) {
	m, err := s.repo.GetScoped(ctx, workspaceID, mediaID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			// ⚠️ 不区分"不存在"与"不是你的"：区分开等于提供一个
			// 用 id 探测别人有哪些资源的接口。
			return nil, repo.MediaObject{}, httpx.Err(errcode.NotFound)
		}
		return nil, repo.MediaObject{}, httpx.Wrap(errcode.Internal, err)
	}

	return s.openWithThumb(ctx, m, width)
}

// openWithThumb 取字节并按需缩略。
//
// ⚠️ 抽出来是因为**公开取图要用同一段逻辑**（P13-01）：两份实现迟早会
// 在"哪些宽度允许""缩不了怎么办"上分叉，而分叉的表现是
// 同一张图在首页和素材库里一个清楚一个糊。
// 归属校验**不在这里** —— 由各入口自己负责（这正是它们的区别）。
func (s *Service) openWithThumb(ctx context.Context, m repo.MediaObject, width int) (
	io.ReadCloser, repo.MediaObject, error) {
	body, err := s.store.Open(ctx, m.ObjectKey)
	if err != nil {
		return nil, repo.MediaObject{}, httpx.Wrap(errcode.Internal, err)
	}
	out := struct{ Body io.ReadCloser }{body}

	if width <= 0 {
		return out.Body, m, nil
	}
	if !domain.IsAllowedThumbWidth(width) {
		out.Body.Close()
		return nil, repo.MediaObject{}, httpx.Errf(errcode.InvalidArgument,
			"缩略图宽度只接受 160/320/640")
	}
	// 缩略图要先读完再缩，因此只对图片做；视频那种几百 MB 的
	// 绝不能进内存（上传时走临时文件正是同一个理由）。
	if !isBitmap(m.ContentType) {
		return out.Body, m, nil
	}
	// ⚠️ 大图不进本进程解码（docs/agent/imagetools IMGB-1）：一张 6 倍放大的图约 85MP，image.Decode 要
	// ~340MB，几个人同时看就能把 API 打爆。超过门槛的交给 ffmpeg 流式缩放（另一个进程、按需解码、限并发）。
	// 另修一个老问题：此前超过 maxThumbSource 的文件被 LimitReader 截断，解码失败后把**截断的字节**当原图回 ——
	// 用户看到的是一张下半截灰的破图。
	if m.SizeBytes > maxThumbSource || int64(m.Width)*int64(m.Height) > domain.ThumbMaxDecodePixels {
		return s.largeThumb(ctx, out.Body, m, width)
	}
	defer out.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(out.Body, maxThumbSource))
	if err != nil {
		return nil, repo.MediaObject{}, httpx.Wrap(errcode.Internal, err)
	}
	thumb, mime, ok, err := domain.Thumbnail(bytes.NewReader(raw), m.ContentType, width)
	if err != nil || !ok {
		// 缩不了就回原图：缩略图是优化，不能让本来能显示的东西显示不了。
		return io.NopCloser(bytes.NewReader(raw)), m, nil
	}
	m.SizeBytes = int64(len(thumb))
	m.ContentType = mime
	return io.NopCloser(bytes.NewReader(thumb)), m, nil
}

// maxThumbSource 单张图进内存的上限。超过这个尺寸的"图片"多半不是给人看的，
// 直接回原图，不冒把进程撑爆的风险。
const maxThumbSource = 32 << 20

func isBitmap(mime string) bool {
	return mime == "image/png" || mime == "image/jpeg"
}

// Info 取媒体元信息（含尺寸与派生溯源）。归属条件在 SQL 里。
//
// ⚠️ 与 `Get` 的区别是**带工作区校验**：`Get` 供 Worker 内部使用
// （它手上只有 mediaID），这个给用户端 —— 少一个条件就是按 id 探测别人的文件。
func (s *Service) Info(ctx context.Context, workspaceID, mediaID int64) (repo.MediaObject, error) {
	m, err := s.repo.GetScoped(ctx, workspaceID, mediaID)
	if err != nil {
		return repo.MediaObject{}, httpx.Err(errcode.NotFound)
	}
	return m, nil
}

// PosterOf 取本工作区一段视频的封面帧（Worker 截的 video_poster，CD-83）；还没截出来时 ok=false。
//
// 前端靠它在不下载整段视频的前提下显示画面、按真实比例摆卡片（design/03 R-5）：一段 30 秒的 480p
// 产出就有 17MB，而封面帧只有十几 KB。与 Worker 截帧前查重用同一个查询（ListDerived），归属条件在 SQL 里。
func (s *Service) PosterOf(ctx context.Context, workspaceID, videoID int64) (repo.MediaObject, bool, error) {
	list, err := s.repo.ListDerived(ctx, workspaceID, videoID, DerivePoster, "")
	if err != nil || len(list) == 0 {
		return repo.MediaObject{}, false, err
	}
	return list[0], true, nil
}

// SignedURL 给本工作区的媒体签发一个短期直链（MCPT-04 media.get，MCP-F-54）。
//
// ⚠️ **先过归属再签名**：签名本身不认识工作区，按 mediaID 直接签的话任何人猜到 ID 就能拿到
// 别人的文件。不存在与不是你的**不区分**（同 Download）。
// 返回 (地址, 过期时刻)；ttl<=0 时用对象存储配置的默认有效期。
func (s *Service) SignedURL(ctx context.Context, workspaceID, mediaID int64, ttl time.Duration) (
	string, time.Time, error) {
	m, err := s.repo.GetScoped(ctx, workspaceID, mediaID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return "", time.Time{}, httpx.Err(errcode.NotFound)
		}
		return "", time.Time{}, httpx.Wrap(errcode.Internal, err)
	}
	if s.store == nil {
		return "", time.Time{}, httpx.Errf(errcode.Internal, "对象存储未接入")
	}
	// 签名用的是客户端配置的桶：对象记录在别的桶里的话签出来的地址必然 404，不如明确报错。
	if m.Bucket != "" && m.Bucket != s.store.Bucket() {
		return "", time.Time{}, httpx.Errf(errcode.Internal, "媒体不在当前对象存储桶中，无法签发地址")
	}
	now := time.Now()
	u, err := s.store.PresignGetTTL(ctx, m.ObjectKey, ttl)
	if err != nil {
		return "", time.Time{}, httpx.Wrap(errcode.Internal, err)
	}
	if ttl <= 0 {
		ttl = s.store.SignTTL()
	}
	return u, now.Add(ttl).UTC(), nil
}

// SignedDownloadURL 给本工作区的媒体签一个「下载为附件」的短期直链（智能剪辑成片下载，SCK-10）。
// 同 SignedURL：先过归属再签名；fileName 写进 Content-Disposition（浏览器直接存成这个名字）。
func (s *Service) SignedDownloadURL(ctx context.Context, workspaceID, mediaID int64, ttl time.Duration,
	fileName string) (string, time.Time, error) {
	m, err := s.repo.GetScoped(ctx, workspaceID, mediaID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return "", time.Time{}, httpx.Err(errcode.NotFound)
		}
		return "", time.Time{}, httpx.Wrap(errcode.Internal, err)
	}
	if s.store == nil {
		return "", time.Time{}, httpx.Errf(errcode.Internal, "对象存储未接入")
	}
	if m.Bucket != "" && m.Bucket != s.store.Bucket() {
		return "", time.Time{}, httpx.Errf(errcode.Internal, "媒体不在当前对象存储桶中，无法签发地址")
	}
	if ttl <= 0 {
		ttl = s.store.SignTTL()
	}
	now := time.Now()
	u, err := s.store.PresignGetDownload(ctx, m.ObjectKey, ttl, fileName)
	if err != nil {
		return "", time.Time{}, httpx.Wrap(errcode.Internal, err)
	}
	return u, now.Add(ttl).UTC(), nil
}

// RegisterUsage 登记活动引用。跨域调用一律走这个端口，
// 不要直接写 media_usage 表 —— 那样引用类型会散落成各写各的字符串。
func (s *Service) RegisterUsage(ctx context.Context, mediaID int64, usageType string, refID int64) error {
	if mediaID == 0 || refID == 0 || usageType == "" {
		return nil // 没有引用要登记不是错误
	}
	if err := s.repo.AddUsage(ctx, s.nextID(), mediaID, usageType, refID); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

// ReleaseUsage 解除某业务对象的全部引用（项目被彻底删除、任务被取消等）。
// 返回解除的条数，供调用方写日志。
func (s *Service) ReleaseUsage(ctx context.Context, usageType string, refID int64) (int64, error) {
	n, err := s.repo.ReleaseUsage(ctx, usageType, refID)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return n, nil
}

func (s *Service) Usages(ctx context.Context, workspaceID, mediaID int64) ([]repo.Usage, error) {
	if _, err := s.repo.GetScoped(ctx, workspaceID, mediaID); err != nil {
		return nil, httpx.Err(errcode.NotFound)
	}
	u, err := s.repo.ListUsages(ctx, mediaID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return u, nil
}

// Delete 软删媒体。被活动引用占用时返回 40906 并**说明是什么占着**。
//
// ⚠️ 两道闸**都要**：`media_usage` 表（短命的跨进程占用，如进行中的生成任务）
// 与各域的**当场查询**（见 references.go）。只留前者的话，删一张正被时间线
// 引用的素材会成功 —— 而成片会在渲染时才失败。
func (s *Service) Delete(ctx context.Context, workspaceID, mediaID int64) error {
	// 先问各域"还在用吗"。放在软删之前：软删成功之后再发现有引用就得回滚，
	// 而那一步本身也可能失败。
	refs, err := s.liveReferences(ctx, mediaID)
	if err != nil {
		// 查不动时**不允许删**（liveReferences 已记日志）。删除不可逆，
		// 拿不准时只能倒向"删不掉"—— 用户会重试，而误删没有下一次。
		return httpx.Errf(errcode.Internal, "暂时无法确认该文件是否仍被引用，请稍后再试")
	}
	if len(refs) > 0 {
		return httpx.Errf(errcode.ResourceInUse,
			"该文件正被 "+describeReferences(refs)+" 引用，无法删除").
			WithDetails(map[string]any{"usageCount": len(refs)})
	}

	// 剪辑台的预览素材不能单独删（SCT-16）：它是源素材的附属品，分析缓存还记着它 ——
	// 单独删掉的话缓存仍说「已生成」、指着一份不存在的预览，剪辑台永远取不到、也不会重做。随源素材一起删（SCD-37）。
	if m, err := s.repo.GetScoped(ctx, workspaceID, mediaID); err == nil && m.WorkspaceID == workspaceID &&
		isPreviewDerive(m.DeriveKind) {
		return httpx.Errf(errcode.ResourceInUse, "这是剪辑台的预览素材，会随原素材一起删除，不能单独删")
	}

	err = s.repo.SoftDelete(ctx, workspaceID, mediaID)
	switch {
	case errors.Is(err, repo.ErrInUse):
		u, _ := s.repo.ListUsages(ctx, mediaID)
		return httpx.Errf(errcode.ResourceInUse,
			"该文件正被 "+describeUsages(u)+" 引用，无法删除").
			WithDetails(map[string]any{"usageCount": len(u)})
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Err(errcode.NotFound)
	case err != nil:
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

func describeUsages(u []repo.Usage) string {
	label := map[string]string{
		UsageProjectCover:  "项目封面",
		UsageGenerationIn:  "进行中的生成任务",
		UsageGenerationOut: "生成产物",
		UsageScriptAsset:   "脚本资产",
		UsageTimelineClip:  "时间线 / 智能剪辑",
		UsageMaterialCover: "素材封面",
		UsageVoiceSample:   "音色试听",
	}
	seen := map[string]bool{}
	var parts []string
	for _, x := range u {
		l := label[x.UsageType]
		if l == "" {
			l = x.UsageType
		}
		if !seen[l] {
			seen[l] = true
			parts = append(parts, l)
		}
	}
	if len(parts) == 0 {
		return "其他资源"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "、" + p
	}
	return out
}

// ---------------------------------------------------------------- 清理队列

// Collector 回收孤儿媒体：**无引用 且 超保留期**。
//
// 与回收站清理（canvas/app.Cleaner）分开跑：那个删的是数据库行，
// 这个要调对象存储，失败模式和重试策略都不一样。
type Collector struct {
	svc       *Service
	log       *slog.Logger
	retention time.Duration
	batch     int
	interval  time.Duration
}

func NewCollector(svc *Service, log *slog.Logger) *Collector {
	return &Collector{
		svc: svc, log: log,
		retention: DefaultMediaRetention,
		batch:     200,
		interval:  time.Hour,
	}
}

func (c *Collector) SetRetention(d time.Duration) { c.retention = d }
func (c *Collector) SetInterval(d time.Duration)  { c.interval = d }

func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	c.Once(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Once(ctx)
		}
	}
}

// Once 跑一轮回收，返回真正删掉的数量。
func (c *Collector) Once(ctx context.Context) int {
	before := time.Now().Add(-c.retention)
	list, err := c.svc.repo.FindCollectable(ctx, before, c.batch)
	if err != nil {
		c.log.Error("媒体回收扫描失败", "error", err)
		return 0
	}
	deleted := 0
	for _, m := range list {
		// ⚠️ **永久删除前最后一道闸**：再问一次各域还在不在用。
		//
		// `FindCollectable` 只看 `media_usage`，而那张表是登记式的、会漏。
		// 这一步是计算式的，且这里的代价不对称到极点 —— 回收是**不可逆**的，
		// 而多查一次只是慢一点。
		//
		// 少了这一步就会出现"API 说删不掉、回收器照样收走"，
		// 而两边各自看起来都没错。
		if refs, err := c.svc.liveReferences(ctx, m.ID); err != nil {
			c.log.Warn("回收前确认引用失败，本轮跳过", "mediaId", m.ID, "error", err)
			continue
		} else if len(refs) > 0 {
			c.log.Warn("已软删但仍被引用，跳过回收",
				"mediaId", m.ID, "refs", len(refs), "kind", refs[0].Kind)
			continue
		}
		// ⚠️ 顺序：**先删对象，再删库行**。反过来的话对象存储里会留下
		// 谁也不认识的文件，只能靠对账脚本捞。
		if err := c.svc.store.Delete(ctx, m.ObjectKey); err != nil {
			// 删对象失败就留着行，下一轮再试 —— 库行是唯一还能找到这个对象的线索。
			c.log.Warn("删除对象失败，保留记录待下轮重试",
				"mediaId", m.ID, "key", m.ObjectKey, "error", err)
			continue
		}
		if err := c.svc.repo.DeleteRow(ctx, m.ID); err != nil {
			c.log.Error("删除媒体记录失败", "mediaId", m.ID, "error", err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		// 只记条数与字节数，不记对象键：清理日志不该成为已删内容的索引。
		c.log.Info("媒体回收完成", "deleted", deleted,
			"retentionDays", int(c.retention.Hours()/24))
	}
	return deleted
}

// LookupInfo 是给其他域的最小视图。
//
// ⚠️ 刻意只回"是什么类型"，不回 bucket/objectKey ——
// 调用方拿不到定位信息，就不可能绕过受保护读取去直接取对象（GO-27）。
type LookupInfo struct {
	// WorkspaceID 是媒体所属工作区：0 = 平台素材（GetScoped 也会把它们查出来）。
	// 调用方要「只认本工作区的文件」时据此再拒（3D 导演台场景引用，DRD-16）。
	WorkspaceID int64
	Kind        string
	Purpose     string
	// Width / Height 图片像素尺寸（0 = 未知）。
	//
	// ⚠️ 给尺寸不违反上面那条：它不是定位信息，拿到 1024×768
	// 也没法取到文件。而蒙版校验必须有它 —— 否则只能让上游去报错。
	Width  int
	Height int
	// DurationMs 音视频时长（毫秒，0 = 未知）：输入区间据此校验终点（docs/agent/smartcut SCD-34）。
	DurationMs int
}

// LookupInWorkspace 供画布域解析参考输入。查不到与不属于本工作区**返回同一个错误**。
func (s *Service) LookupInWorkspace(ctx context.Context, workspaceID, mediaID int64) (LookupInfo, error) {
	m, err := s.repo.GetScoped(ctx, workspaceID, mediaID)
	if err != nil {
		return LookupInfo{}, err
	}
	return LookupInfo{
		WorkspaceID: m.WorkspaceID, Kind: kindOf(m.ContentType), Purpose: m.Purpose,
		Width: m.Width, Height: m.Height, DurationMs: m.DurationMs,
	}, nil
}

// KindOf 把 MIME 归到媒体大类，供传输层复用。
func KindOf(mime string) string { return kindOf(mime) }

func kindOf(mime string) string {
	switch {
	case len(mime) >= 6 && mime[:6] == "image/":
		return "image"
	case len(mime) >= 6 && mime[:6] == "audio/":
		return "audio"
	case len(mime) >= 6 && mime[:6] == "video/":
		return "video"
	}
	return ""
}

// ListMine 列出本工作区的上传参考（个人资产选取）。
func (s *Service) ListMine(ctx context.Context, workspaceID int64, kind string, limit int,
	cursorID int64) ([]repo.MediaObject, bool, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	// 多查一条判断 hasMore，与项目列表同一套做法。
	rows, err := s.repo.ListByWorkspace(ctx, workspaceID, kind, limit+1, cursorID)
	if err != nil {
		return nil, false, httpx.Wrap(errcode.Internal, err)
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// OpenPublic 读取一份媒体，**不做归属校验**（P13-01）。
//
// ⚠️ 只给内容域的公开取图用，而且必须在它判过作品可见性之后。
// 名字里带 Public 是有意的：在调用处一眼能看出这里没有租户条件。
//
// 仍然会做的两件事：`status='active'`（已删的取不到）与缩略图宽度校验
// （开放任意宽度等于给人一个把 CPU 打满的 URL 参数）。
func (s *Service) OpenPublic(ctx context.Context, mediaID int64, width int) (
	io.ReadCloser, repo.MediaObject, error) {
	m, err := s.repo.Get(ctx, mediaID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, repo.MediaObject{}, httpx.Err(errcode.NotFound)
		}
		return nil, repo.MediaObject{}, httpx.Wrap(errcode.Internal, err)
	}
	return s.openWithThumb(ctx, m, width)
}

// OpenPublicCover 读取一份公开媒体的**封面图**，同 OpenPublic 一样不做归属校验（调用方已判过可见性）。
//
// 图片就是它自己的缩略图；视频是 Worker 截的第一帧（video_poster，CD-83）按宽度缩；其余类型、或视频的封面帧
// 还没截出来，一律 404 —— 由前端退回别的显示方式，而不是吐出整段视频。
//
// 为什么单开（2026-11-01）：公开作品墙的视频卡此前用 `<video preload=metadata>` 取封面，而公开媒体端点不支持
// Range，浏览器只能把整段（一段 10~20MB）下完；几段同时下就占满浏览器对同一主机的 6 个连接，页面上别的接口
// 跟着排队好几秒（营销页的余额与模板列表实测 7.9 秒，服务端处理只要 20ms）。封面帧只有十几 KB。
// 没有复用 OpenPublic 的宽度参数：公开取图端点不带 `w` 时默认按 320 缩，视频播放也走它 —— 「有宽度就给封面」
// 会让视频播放拿到一张图。
func (s *Service) OpenPublicCover(ctx context.Context, mediaID int64, width int) (
	io.ReadCloser, repo.MediaObject, error) {
	m, err := s.repo.Get(ctx, mediaID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, repo.MediaObject{}, httpx.Err(errcode.NotFound)
		}
		return nil, repo.MediaObject{}, httpx.Wrap(errcode.Internal, err)
	}
	switch kindOf(m.ContentType) {
	case "image":
		return s.openWithThumb(ctx, m, width)
	case "video":
		poster, ok, err := s.PosterOf(ctx, m.WorkspaceID, m.ID)
		if err != nil {
			return nil, repo.MediaObject{}, httpx.Wrap(errcode.Internal, err)
		}
		if !ok {
			return nil, repo.MediaObject{}, httpx.Errf(errcode.NotFound, "视频的封面帧还没截好")
		}
		return s.openWithThumb(ctx, poster, width)
	}
	return nil, repo.MediaObject{}, httpx.Errf(errcode.NotFound, "这类媒体没有封面图")
}

// MediaBrief 是给别的域用的媒体概况：类型（image / video / audio）与封面帧。
type MediaBrief struct {
	Kind     string
	PosterID int64
}

// Briefs 一次查多份本工作区媒体的类型与封面帧（智能剪辑节点卡，SCT-07）。查不到的不在结果里。
func (s *Service) Briefs(ctx context.Context, workspaceID int64, ids []int64) (map[int64]MediaBrief, error) {
	rows, err := s.repo.Briefs(ctx, workspaceID, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]MediaBrief, len(rows))
	for id, b := range rows {
		out[id] = MediaBrief{Kind: kindOf(b.ContentType), PosterID: b.PosterID}
	}
	return out, nil
}

// isPreviewDerive 是不是剪辑台的预览素材（与 repo 的 previewDeriveKinds 一致）。
func isPreviewDerive(kind string) bool {
	switch kind {
	case DerivePreviewProxy, DeriveFilmstrip, DeriveSpeechAudio:
		return true
	}
	return false
}

// InputFact 是智能剪辑素材条要的媒体事实（SCT-25）。Invalid 非空表示不能用，并说明为什么。
type InputFact struct {
	Kind       string
	DurationMs int
	PosterID   int64
	// LastFrameID 末帧（补镜的参考图）；还没截出来为 0。
	LastFrameID int64
	HasAudio    *bool
	Invalid     string
}

// InputFacts 一次查一批媒体给素材条用：本工作区、未删除的给出类型、时长、封面帧与有没有音轨；
// 平台素材（工作区 0）与别的工作区 / 不存在 / 已删除的记不能用（SC-AC-06）。
//
// ⚠️ 平台素材要单独拒：受理侧的参考解析会放行工作区 0（导演台 DRB-10 记过），时间线若也收它，
// 平台素材就借智能剪辑进了用户的成片。
func (s *Service) InputFacts(ctx context.Context, workspaceID int64, ids []int64) (map[int64]InputFact, error) {
	rows, err := s.repo.Facts(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]InputFact, len(ids))
	for _, id := range ids {
		f, ok := rows[id]
		switch {
		case ok && f.Status == "active" && f.WorkspaceID == 0:
			out[id] = InputFact{Invalid: "平台素材不能直接使用"}
		case !ok || f.Status != "active" || f.WorkspaceID != workspaceID:
			// 不区分「不存在」与「不是你的」：区分开等于提供一个按 ID 探测的接口。
			out[id] = InputFact{Invalid: "素材不存在或不属于本工作区"}
		default:
			out[id] = InputFact{Kind: kindOf(f.ContentType), DurationMs: f.DurationMs, PosterID: f.PosterID,
				LastFrameID: f.LastFrameID, HasAudio: f.HasAudio}
		}
	}
	return out, nil
}
