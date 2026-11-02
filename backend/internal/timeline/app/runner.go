package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/li904338203/tabtv/backend/internal/platform/ffbin"

	"github.com/li904338203/tabtv/backend/internal/platform/ffsem"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/timeline/typeset"
)

// 渲染执行（NEW-10、NEW-11）。Worker 侧。
//
// ⚠️ 渲染是**CPU 密集且长耗时**的，与生成任务（等上游 IO）完全不是一类。
// 所以它有独立的队列与独立的并发上限 —— 混在一起的话，
// 一次 4K 渲染会把机器啃满，而所有查询接口跟着从毫秒劣化到几秒
// （旧系统的 HttpClient 泄漏就是这么把库挤垮的，教训是同一类）。

// MediaFetcher 由媒体域实现：把素材取到本地临时文件，并探好有没有画面 / 音轨、几个声道。
type MediaFetcher interface {
	// Fetch 返回本地文件与清理函数。
	//
	// ⚠️ **落临时文件不落内存**：一条片子的素材加起来动辄上 GB，
	// 按内存缓冲并发两路就 OOM（与产物下载同一条）。
	// ⚠️ 探测失败按「没有音轨」处理：按"有"的话滤镜图会引用一条不存在的流，整次渲染当场失败。
	Fetch(ctx context.Context, mediaID int64) (file domain.MediaFile, cleanup func(), err error)
}

// Archiver 由媒体域实现：把成片写进对象存储并登记；Discard 软删一份刚归档的成片
// （归档之后才发现任务被取消了：成片不该留着，SCB-5）。
type Archiver interface {
	Archive(ctx context.Context, workspaceID, userID int64, mime string,
		size int64, body io.Reader) (int64, error)
	Discard(ctx context.Context, workspaceID, mediaID int64) error
}

// Typesetter 文字与字幕排版（timeline/typeset.Typesetter 满足它，SCT-14）。
type Typesetter interface {
	Typeset(ctx context.Context, req typeset.Request) (*typeset.Result, error)
}

// RenderOutcome 告诉消费者怎么 ack。
type RenderOutcome int

const (
	// RenderDone 处理完了（成功或已判失败）。
	RenderDone RenderOutcome = iota
	// RenderDuplicate 已被别人领走或已是终态 —— at-least-once 的正常结局。
	RenderDuplicate
	// RenderRequeue 现在跑不了（并发满），放回队列。
	RenderRequeue
)

// Renderer 跑渲染。
type Renderer struct {
	repo     *repo.Repo
	fetch    MediaFetcher
	archive  Archiver
	refunder Refunder
	nextID   func() int64
	notifier Notifier
	log      *slog.Logger
	// MaxConcurrent 同时跑几路渲染。
	//
	// ⚠️ 默认 1：渲染吃满 CPU，两路并行不会更快，只会让两个都变慢 ——
	// 而用户看到的是两个任务都卡着。要加并发先加机器。
	MaxConcurrent int
	// ffSem 是与预览素材扫描器共用的 ffmpeg 并发信号量（进程级，SCD-30）；nil = 不限。
	//
	// ⚠️ MaxConcurrent 只管渲染自己：一次导出撞上一段长视频的预览转码，两边各自都没超，合起来照样把机器啃满。
	ffSem *ffsem.Sem
	// events 发 render.update（SCT-15，SCB-6）；typeset 排文字与字幕（SCT-14）。
	events  EventPublisher
	typeset Typesetter
}

func NewRenderer(r *repo.Repo, log *slog.Logger) *Renderer {
	return &Renderer{repo: r, log: log, MaxConcurrent: 1}
}

func (x *Renderer) SetFetcher(f MediaFetcher)  { x.fetch = f }
func (x *Renderer) SetArchiver(a Archiver)     { x.archive = a }
func (x *Renderer) SetRefunder(r Refunder)     { x.refunder = r }
func (x *Renderer) SetNextID(f func() int64)   { x.nextID = f }
func (x *Renderer) SetFFmpegSem(s *ffsem.Sem)  { x.ffSem = s }
func (x *Renderer) SetEvents(p EventPublisher) { x.events = p }
func (x *Renderer) SetTypesetter(t Typesetter) { x.typeset = t }

// Run 执行一次渲染。
func (x *Renderer) Run(ctx context.Context, renderID int64) (RenderOutcome, error) {
	version := ffmpegVersion()
	// ⚠️ 并发上限**读库不读进程内计数**，而且「数」与「领」在一个事务里（SCB-10）。达到上限**不是失败** ——
	// 消息放回队列，任务留在 queued。死线是受理时算好的，领取不改（SCB-4）；只有存量任务没有死线时才补两小时。
	job, err := x.repo.ClaimRender(ctx, renderID, version, time.Now().Add(2*time.Hour), x.MaxConcurrent)
	switch {
	case errors.Is(err, repo.ErrRenderFull):
		return RenderRequeue, nil
	case errors.Is(err, repo.ErrRenderBusy), errors.Is(err, repo.ErrNotFound):
		return RenderDuplicate, nil
	case err != nil:
		return RenderRequeue, err
	}
	pr := &renderProgress{x: x, job: job}
	pr.publish(ctx)

	// 取消即停（SCB-5）：ffmpeg 运行期间每秒查一次状态，被取消就取消子 context —— ffmpeg 随之退出，
	// 临时文件随目录删掉，不归档。以前取消只改了状态，ffmpeg 照样跑完，成片成了孤儿。
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stopped atomic.Bool
	go x.watchCancel(jobCtx, cancel, job.ID, &stopped)

	res, err := x.render(jobCtx, job, pr)
	cancel()
	if stopped.Load() {
		if res.MediaID != 0 {
			x.discard(ctx, job, res.MediaID)
		}
		x.log.Info("渲染已取消，进程已停", "renderId", job.ID)
		return RenderDone, nil
	}
	if err != nil {
		x.failAndRefund(ctx, job, err)
		// ⚠️ 已判失败的任务**不再重投**：重投只会重复失败，
		// 而库里那行已经带着原因（与生成、导出同一条规则）。
		return RenderDone, nil
	}
	if err := x.repo.FinishRender(ctx, job.ID, res.MediaID, res.SelfCheck); err != nil {
		if errors.Is(err, repo.ErrRenderBusy) {
			// 归档之后、写成功之前被取消了（或被恢复扫描判了超时）：这份成片不该留着。
			x.discard(ctx, job, res.MediaID)
			return RenderDone, nil
		}
		// 成片已经归档了但状态没写成 —— 下一轮重投会重渲一次，
		// 上一个成了无引用的孤儿，由媒体回收收掉。
		return RenderRequeue, err
	}
	// 成片进作品库（NEW-11）——只对**没绑画布节点**的时间线（SCK-9）：智能剪辑的成片是画布上的一个中间产物，
	// 用户要不要把它当作品，由他在剪辑台里显式提交（SC-F-79），不是导出一次就进一次作品库。
	//
	// ⚠️ **进作品库不等于公开发布**：落的是 pending，公开可见性是
	// `approved ∧ featured ∧ ¬offline`，三个条件都要运营显式操作。
	//
	// ⚠️ 登记失败**只告警不影响渲染**：片子已经出来了、钱已经结了，
	// 为一条作品记录把任务判失败会把用户的成片一起退掉。
	if x.nextID != nil && job.CanvasNodeID == "" {
		if err := x.repo.InsertWork(ctx, x.nextID(), job.WorkspaceID, job.ID,
			job.TimelineID, res.MediaID); err != nil {
			x.log.Warn("成片登记作品失败", "renderId", job.ID, "error", err)
		}
	}
	x.notify(ctx, job, true, "")
	x.publishLatest(ctx, job.ID)
	x.log.Info("渲染完成", "renderId", job.ID, "mediaId", res.MediaID, "ffmpeg", version)
	return RenderDone, nil
}

// watchCancel 每秒查一次任务状态，不再是 running（被取消、被判超时）就停掉这次渲染。
//
// ⚠️ 只在渲染期间查、每个任务一秒一次：量很小，不要改成长连接。
func (x *Renderer) watchCancel(ctx context.Context, cancel context.CancelFunc, id int64, stopped *atomic.Bool) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, err := x.repo.RenderStatus(ctx, id)
			if err != nil {
				continue // 库抖一下不该停掉渲染；真被取消了下一秒再看
			}
			if st != domain.RenderRunning {
				stopped.Store(true)
				cancel()
				return
			}
		}
	}
}

// discard 软删一份不该留下的成片（取消之后才归档完的）。失败只告警：无引用的成片由媒体回收收掉。
func (x *Renderer) discard(ctx context.Context, job repo.RenderJob, mediaID int64) {
	if x.archive == nil {
		return
	}
	if err := x.archive.Discard(ctx, job.WorkspaceID, mediaID); err != nil {
		x.log.Warn("丢弃取消后归档的成片失败", "renderId", job.ID, "mediaId", mediaID, "error", err)
	}
}

// publishLatest 按库里最新的样子发一条 render.update（终态）。
func (x *Renderer) publishLatest(ctx context.Context, id int64) {
	if x.events == nil {
		return
	}
	if j, err := x.repo.RenderForRun(ctx, id); err == nil {
		publishRenderEvent(ctx, x.events, j, func(msg string, args ...any) { x.log.Warn(msg, args...) })
	}
}

// renderProgress 推进进度：写库（带 status='running' 条件），并把 render.update 节流到 1 次 / 秒
// （阶段变了立刻发）。
type renderProgress struct {
	x     *Renderer
	job   repo.RenderJob
	last  time.Time
	stage string
	pct   int
}

func (p *renderProgress) set(ctx context.Context, stage string, pct int) {
	if pct < p.pct {
		pct = p.pct // 进度只增不减（SC-AC-24）
	}
	changed := stage != p.stage
	p.stage, p.pct = stage, pct
	_ = p.x.repo.RenderProgress(ctx, p.job.ID, stage, pct)
	if changed || time.Since(p.last) >= time.Second {
		p.publish(ctx)
	}
}

func (p *renderProgress) publish(ctx context.Context) {
	if p.x.events == nil {
		return
	}
	j := p.job
	j.Status, j.Stage, j.Progress = domain.RenderRunning, p.stage, p.pct
	if j.Stage == "" {
		j.Stage = domain.StageProbing
	}
	p.last = time.Now()
	publishRenderEvent(ctx, p.x.events, j, func(msg string, args ...any) { p.x.log.Warn(msg, args...) })
}

// Notifier 由内容域实现：发一条站内通知。
//
// ⚠️ 端口定义在这里而不是让时间线域去 import 内容域的 service ——
// Worker 不依赖内容域是有意的（进程边界），而通知只是"把一句话交出去"。
type Notifier interface {
	Notify(ctx context.Context, userID int64, typ, title, content, refType, refID string) error
}

func (x *Renderer) SetNotifier(n Notifier) { x.notifier = n }

// notify 把渲染结果告诉发起人。
//
// ⚠️ **通知绝不能牵连主流程**：片子已经出来了、钱已经结了，
// 为一条通知把任务判失败会把用户的成片一起退掉。所以只告警。
//
// ⚠️ 幂等引用是 `render` + 任务 ID：Worker 重投同一条渲染不会发第二条
// （`uk_notification_ref`）。成功与失败是**两个 type**，所以一次失败后被
// 运维重投并成功时，两条都发得出去 —— 只用一个 type 的话第二条会被唯一索引
// 挡掉，用户永远停在"渲染失败"那条通知上。
func (x *Renderer) notify(ctx context.Context, job repo.RenderJob, ok bool, errMsg string) {
	if x.notifier == nil || job.CreatedByUserID == 0 {
		return
	}
	typ, title, body := "render_success", "成片已渲染完成", ""
	if !ok {
		typ, title, body = "render_failed", "成片渲染失败", errMsg
	}
	if err := x.notifier.Notify(ctx, job.CreatedByUserID, typ, title, body,
		"render", fmt.Sprint(job.ID)); err != nil {
		x.log.Warn("发送渲染通知失败", "renderId", job.ID, "error", err)
	}
}

func (x *Renderer) failAndRefund(ctx context.Context, job repo.RenderJob, cause error) {
	changed, err := x.repo.FailRender(ctx, job.ID, "render_failed", cause.Error())
	if err != nil {
		x.log.Error("标记渲染失败时出错", "renderId", job.ID, "error", err)
		return
	}
	x.log.Warn("渲染失败", "renderId", job.ID, "error", cause)
	// ⚠️ **只有本次真的改成 failed 才退款**：不看这个返回值的话，
	// 重复消息会退两次钱。（账本的唯一约束是第二道防线，但别指望它。）
	if changed && job.CreditsCost > 0 && x.refunder != nil {
		if err := x.refunder.Refund(ctx, job.PayerAccountID, "render",
			fmt.Sprint(job.ID), job.CreditsCost, "渲染失败"); err != nil {
			x.log.Error("渲染失败后退款出错", "renderId", job.ID, "error", err)
		}
	}
	// ⚠️ 只有**本次真的改成 failed** 才通知：重复消息不该给用户再发一条
	// "渲染失败"，而那条任务可能早就被运维重投并成功了。
	if changed {
		x.notify(ctx, job, false, cause.Error())
		x.publishLatest(ctx, job.ID)
	}
}

// renderResult 一次渲染的产物：成片媒体与导出后自检（只是警告）。
type renderResult struct {
	MediaID   int64
	SelfCheck json.RawMessage
}

// render 真正跑一遍（SCD-27）：取素材 → 文字与字幕叠加流 → 音频预渲 → 画面（分块）→ 自检 → 归档。
func (x *Renderer) render(ctx context.Context, job repo.RenderJob, pr *renderProgress) (renderResult, error) {
	if x.fetch == nil || x.archive == nil {
		return renderResult{}, fmt.Errorf("渲染依赖未接入")
	}
	var snap domain.RenderSnapshot
	if err := json.Unmarshal(job.Snapshot, &snap); err != nil {
		return renderResult{}, fmt.Errorf("渲染快照已损坏: %w", err)
	}

	// ⚠️ 所有临时文件放**同一个目录**并整体删掉：逐个删的话，
	// 中途 return 会漏下几个几百 MB 的文件，几天就把 Worker 的磁盘写满 ——
	// 而症状是"所有渲染都失败了"，看着完全不像磁盘问题（与导出同一条）。
	dir, err := os.MkdirTemp("", "seeaitv-render-*")
	if err != nil {
		return renderResult{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	pr.set(ctx, domain.StageProbing, 5)
	files, cleanup, err := x.fetchMedia(ctx, snap)
	defer cleanup()
	if err != nil {
		return renderResult{}, err
	}
	// 几段引用、取回几份（SCB-9 的可观测面）：同一份素材剪成 50 段，这里应是 refs=50、media=1。
	x.log.Info("渲染取回素材", "renderId", job.ID, "refs", snapMediaRefs(snap), "media", len(files))
	total := domain.SnapDuration(snap)

	pr.set(ctx, domain.StageAssembling, 15)
	overlay, err := x.buildOverlay(ctx, dir, snap, total)
	if err != nil {
		return renderResult{}, err
	}

	pr.set(ctx, domain.StageMixing, 20)
	audioPath, err := x.mixAudio(ctx, dir, snap, files)
	if err != nil {
		return renderResult{}, err
	}

	// 水印图也要取到本地。⚠️ 取不回来时**不加水印而不是失败** ——
	// 为一张 logo 让整条渲染链停掉不值；但要在任务上记下没加，
	// 否则事后「这个片子怎么没水印」只能靠猜。
	watermarkPath := ""
	if snap.Watermark != nil {
		f, done, err := x.fetch.Fetch(ctx, snap.Watermark.MediaID)
		if err != nil {
			x.log.Warn("水印图取不回来，本次不加水印", "renderId", job.ID, "mediaId", snap.Watermark.MediaID, "error", err)
			snap.Watermark = nil
		} else {
			defer done()
			watermarkPath = f.Path
			if err := x.repo.MarkWatermarked(ctx, job.ID); err != nil {
				x.log.Warn("记录水印状态失败", "renderId", job.ID, "error", err)
			}
		}
	}

	out := filepath.Join(dir, "out.mp4")
	plan, err := domain.BuildRenderPlan(snap, domain.RenderIO{Files: files, OverlayList: overlay,
		WatermarkPath: watermarkPath, AudioPath: audioPath, WorkDir: dir, OutPath: out})
	if err != nil {
		return renderResult{}, err
	}

	pr.set(ctx, domain.StageEncoding, 30)
	// 编码占进度条的 30→88：分块时每块按时长分一份，收尾那一遍占最后一份。
	spans := len(plan.Chunks) + 1
	step := 58.0 / float64(spans)
	for k, c := range plan.Chunks {
		lo := 30 + step*float64(k)
		if err := x.runFFmpeg(ctx, dir, c.Args, c.DurationMs, func(f float64) {
			pr.set(ctx, domain.StageEncoding, int(lo+step*f))
		}); err != nil {
			return renderResult{}, fmt.Errorf("第 %d 块渲染失败: %w", k+1, err)
		}
	}
	if plan.ConcatList != "" {
		if err := os.WriteFile(plan.ConcatList, []byte(plan.ConcatListBody), 0o600); err != nil {
			return renderResult{}, err
		}
	}
	lo := 30 + step*float64(len(plan.Chunks))
	if err := x.runFFmpeg(ctx, dir, plan.Args, total, func(f float64) {
		pr.set(ctx, domain.StageEncoding, int(lo+step*f))
	}); err != nil {
		return renderResult{}, err
	}

	f, err := os.Open(out)
	if err != nil {
		return renderResult{}, fmt.Errorf("成片文件不见了: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return renderResult{}, err
	}
	// ⚠️ 0 字节的成片要当失败：ffmpeg 有时会以 0 退出却写出一个空文件
	// （滤镜图有问题时），不判的话用户下到一个打不开的文件而任务是"成功"的。
	if st.Size() == 0 {
		return renderResult{}, fmt.Errorf("渲染产出为空文件")
	}

	pr.set(ctx, domain.StageChecking, 88)
	check := x.selfCheck(ctx, out, total, snap.FPS, audioPath != "")

	pr.set(ctx, domain.StageArchiving, 92)
	mediaID, err := x.archive.Archive(ctx, job.WorkspaceID, job.CreatedByUserID, "video/mp4", st.Size(), f)
	if err != nil {
		return renderResult{}, err
	}
	return renderResult{MediaID: mediaID, SelfCheck: check}, nil
}

// snapMediaRefs 快照里引用素材的段数（画面段 + 没静音的音频段），与 fetchMedia 走的是同一个范围。
func snapMediaRefs(snap domain.RenderSnapshot) int {
	n := len(snap.Clips)
	for _, t := range snap.AudioTracks {
		if !t.Muted {
			n += len(t.Clips)
		}
	}
	return n
}

// fetchMedia 按媒体去重下载（SCB-9）：同一份素材剪成 50 段也只下载一次、只建一个临时文件，
// 多段在滤镜图里各自 `-ss/-t` 引用同一个文件。
func (x *Renderer) fetchMedia(ctx context.Context, snap domain.RenderSnapshot) (map[int64]domain.MediaFile, func(), error) {
	files := map[int64]domain.MediaFile{}
	var cleanups []func()
	done := func() {
		for _, c := range cleanups {
			if c != nil {
				c()
			}
		}
	}
	get := func(id int64, what string) error {
		if _, ok := files[id]; ok {
			return nil
		}
		f, cleanup, err := x.fetch.Fetch(ctx, id)
		if err != nil {
			// 明确指出是哪一段：只说"取素材失败"的话，几十段的时间线上用户找不到是哪一段。
			return fmt.Errorf("%s的素材取不回来: %w", what, err)
		}
		cleanups = append(cleanups, cleanup)
		files[id] = f
		return nil
	}
	for i, c := range snap.Clips {
		if err := get(c.MediaID, fmt.Sprintf("第 %d 段", i+1)); err != nil {
			return files, done, err
		}
	}
	for ti, t := range snap.AudioTracks {
		if t.Muted {
			continue
		}
		for i, c := range t.Clips {
			if err := get(c.MediaID, fmt.Sprintf("音频轨 %d 第 %d 段", ti+1, i+1)); err != nil {
				return files, done, err
			}
		}
	}
	return files, done, nil
}

// mixAudio 音频预渲（SCT-12）：第一遍 loudnorm 只测量，第二遍带测量值输出 48k 立体声 WAV。
// 没有任何声音时返回空路径（成片 -an）。
func (x *Renderer) mixAudio(ctx context.Context, dir string, snap domain.RenderSnapshot,
	files map[int64]domain.MediaFile) (string, error) {
	plan, ok := domain.BuildAudioPlan(snap, files)
	if !ok {
		return "", nil
	}
	var stderr bytes.Buffer
	if err := x.runFFmpegOut(ctx, dir, plan.MeasureArgs(), &stderr); err != nil {
		return "", fmt.Errorf("音频响度测量失败: %w", err)
	}
	stats, err := domain.ParseLoudnorm(stderr.Bytes())
	if err != nil {
		return "", fmt.Errorf("音频响度测量结果解析失败: %w", err)
	}
	out := filepath.Join(dir, "mix.wav")
	if err := x.runFFmpeg(ctx, dir, plan.RenderArgs(stats, out), 0, nil); err != nil {
		return "", fmt.Errorf("音频预渲失败: %w", err)
	}
	return out, nil
}

// selfCheck 导出后自检（SCT-15，只警告）：ffprobe 时长、ebur128 响度、blackdetect 黑场。
// 自检本身出错不影响成片（记一条「自检没做成」的提醒）。
func (x *Renderer) selfCheck(ctx context.Context, path string, wantMs, fps int, hasAudio bool) json.RawMessage {
	gotMs := 0
	if out, err := exec.CommandContext(ctx, ffbin.FFprobe(), "-v", "error", "-show_entries", "format=duration",
		"-of", "csv=p=0", "--", path).Output(); err == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil {
			gotMs = int(f * 1000)
		}
	}
	var stderr bytes.Buffer
	var check domain.SelfCheck
	if err := x.runFFmpegOut(ctx, filepath.Dir(path), domain.SelfCheckArgs(path, hasAudio), &stderr); err != nil {
		check = domain.BuildSelfCheck(gotMs, wantMs, fps, nil, nil)
		check.Warnings = append(check.Warnings, "导出后自检没有做成（不影响成片）")
	} else {
		blacks, lufs := domain.ParseSelfCheck(stderr.Bytes())
		check = domain.BuildSelfCheck(gotMs, wantMs, fps, lufs, blacks)
	}
	raw, _ := json.Marshal(check)
	return raw
}

// runFFmpeg 拿到信号量后跑 ffmpeg；totalMs > 0 时解析 `-progress` 回报编码进度（0~1）。失败时把 stderr 的尾巴带回来。
//
// ⚠️ **必须带上 stderr 的尾巴**：ffmpeg 的错误全在 stderr 里，
// 只回一个 exit status 的话，排查时只能自己把命令拼出来重跑一遍。
// 但只取**尾部**——完整的 stderr 有几千行进度刷屏，塞进 error_message 会撑爆那一列。
func (x *Renderer) runFFmpeg(ctx context.Context, dir string, args []string, totalMs int, onProgress func(float64)) error {
	release, err := x.ffSem.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("等待 ffmpeg 名额时被取消: %w", err)
	}
	defer release()
	full := args
	if onProgress != nil && totalMs > 0 {
		full = append([]string{"-progress", "pipe:1", "-nostats"}, args...)
	}
	var tail strings.Builder
	cmd := exec.CommandContext(ctx, ffbin.FFmpeg(), full...)
	cmd.Dir = dir
	cmd.Stderr = &tailWriter{w: &tail, max: 2000}
	var stdout io.ReadCloser
	if onProgress != nil && totalMs > 0 {
		if stdout, err = cmd.StdoutPipe(); err != nil {
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if stdout != nil {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "out_time_us="); ok {
				if us, err := strconv.ParseInt(v, 10, 64); err == nil && us > 0 {
					onProgress(min(float64(us)/1000/float64(totalMs), 1))
				}
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(tail.String())
		if ctx.Err() != nil {
			return fmt.Errorf("渲染被中止: %w", ctx.Err())
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ffmpeg 失败: %s", msg)
	}
	return nil
}

// runFFmpegOut 跑一遍只要 stderr 的 ffmpeg（响度测量、自检：结论在 stderr 里）。
func (x *Renderer) runFFmpegOut(ctx context.Context, dir string, args []string, stderr *bytes.Buffer) error {
	release, err := x.ffSem.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	cmd := exec.CommandContext(ctx, ffbin.FFmpeg(), args...)
	cmd.Dir = dir
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w %s", err, tailOf(stderr.String(), 400))
	}
	return nil
}

func tailOf(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// tailWriter 只留最后 max 个字节。
type tailWriter struct {
	w   *strings.Builder
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.w.Write(p)
	if t.w.Len() > t.max*2 {
		s := t.w.String()
		t.w.Reset()
		t.w.WriteString(s[len(s)-t.max:])
	}
	return len(p), nil
}

// ffmpegVersion 记下用的是哪个版本。
//
// ⚠️ `render_job` 同时存 timeline_version + snapshot + ffmpeg_version：
// 只记时间线版本的话，升级 ffmpeg 后**同一个版本渲染出不同结果**就无法解释
// （8.x 的 filter 行为跨版本有差异）。
func ffmpegVersion() string {
	out, err := exec.Command(ffbin.FFmpeg(), "-version").Output()
	if err != nil {
		return ""
	}
	line := strings.SplitN(string(out), "\n", 2)[0]
	if len(line) > 64 {
		line = line[:64]
	}
	return strings.TrimSpace(line)
}

// FFmpegAvailable 本机有没有 ffmpeg。启动时查一次写进日志 ——
// 没有它时渲染一律失败，而"为什么所有渲染都失败"是个很难猜的问题。
func FFmpegAvailable() bool {
	_, err := exec.LookPath(ffbin.FFmpeg())
	return err == nil
}

// ---------------------------------------------------------------- 恢复

// RecoverStale 把撞了死线的渲染判失败并退款。
//
// ⚠️ 必须有：ffmpeg 可能卡死（畸形输入、磁盘满），而卡着的任务会一直占着
// 并发额度 —— 后面排队的所有渲染都跟着不动，而它们各自看都是"排队中"。
func (x *Renderer) RecoverStale(ctx context.Context) int {
	list, err := x.repo.StaleRenders(ctx, 20)
	if err != nil {
		x.log.Error("扫描超时渲染失败", "error", err)
		return 0
	}
	n := 0
	for _, j := range list {
		x.failAndRefund(ctx, j, fmt.Errorf("渲染超时未完成"))
		n++
	}
	if n > 0 {
		x.log.Warn("超时渲染已判失败并退款", "count", n)
	}
	return n
}
