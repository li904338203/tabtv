package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
)

// DerivePoster 是视频封面帧的派生种类（CD-83），取**第一帧**。
//
// 封面帧挂在视频上（`source_media_id` = 视频），项目封面函数 `project_display_cover()`
// 按它把视频与成片算进候选 —— 封面走的仍是图片，前端与缩略图都不用改。
const DerivePoster = "video_poster"

// DeriveLastFrame 是视频末帧的派生种类：智能剪辑「补镜」拿上一段的末帧当参考图。
//
// ⚠️ 不能拿封面帧代替：封面是第一帧，用它去接会让补出来的镜头从上一段的开头重新开始。
// 迁移 00075 之前封面取的就是末帧，所以那批行直接改成了这个种类。
const DeriveLastFrame = "video_lastframe"

// 剪辑台的预览素材（docs/agent/smartcut SCT-16，SCD-30）：预览版、缩略图拼图、抽出的识别用音轨。
// ⚠️ 它们是源素材的附属品，不是独立素材（与宫格切块、共享导入不同，NEW-16）：不进「我的素材」，
// 源素材被删时一起软删，源已经不在时随回收处理（SCD-37，SCT-10）。
const (
	DerivePreviewProxy = "preview_proxy"
	DeriveFilmstrip    = "filmstrip"
	DeriveSpeechAudio  = "speech_audio"
)

// Locker 是跨实例的互斥（多个 Worker 只该有一个在截同一批）。
type Locker interface {
	TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// videoFrame 是一种要从视频里截出来的静态帧。
type videoFrame struct {
	kind    string
	file    string
	extract func(ctx context.Context, dir, src string, durationMs int) ([]byte, error)
}

// videoFrames 每段视频都要有的两张帧：封面（第一帧）与末帧。
var videoFrames = []videoFrame{
	{kind: DerivePoster, file: "poster.jpg", extract: extractFirstFrame},
	{kind: DeriveLastFrame, file: "lastframe.jpg", extract: extractLastFrame},
}

// EnsureFrames 给一段视频补齐封面帧与末帧（已有的跳过，幂等），视频只下载一次。
//
// ⚠️ 帧与视频同一个 purpose：它不该出现在「我的素材」里（素材库只列上传参考），
// 也不该被当成一次新的产出 —— 它只是那段视频的一张代表画面。
// 一张截失败不影响另一张：先截好的照样存下，返回的错误说明哪一张没成。
func (s *Service) EnsureFrames(ctx context.Context, video repo.MediaObject) error {
	var missing []videoFrame
	for _, f := range videoFrames {
		existing, err := s.repo.ListDerived(ctx, video.WorkspaceID, video.ID, f.kind, "")
		if err != nil {
			return err
		}
		if len(existing) == 0 {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if s.store == nil {
		return errors.New("对象存储未配置")
	}

	dir, err := os.MkdirTemp("", "seeaitv-poster-*")
	if err != nil {
		return err
	}
	// ⚠️ 视频与截出来的帧放同一个目录、整体删：每轮留下一段几十 MB 的视频，几天就把 Worker 磁盘写满。
	defer func() { _ = os.RemoveAll(dir) }()

	src := filepath.Join(dir, "video"+videoExt(video.ContentType))
	if err := s.download(ctx, video.ID, src); err != nil {
		return err
	}
	duration := video.DurationMs
	if duration <= 0 {
		duration, _ = domain.ProbeDuration(ctx, src, video.ContentType)
	}
	var errs []error
	for _, f := range missing {
		frame, err := f.extract(ctx, dir, src, duration)
		if err == nil {
			_, err = s.Upload(ctx, UploadInput{
				WorkspaceID: video.WorkspaceID, UserID: video.CreatedByUserID,
				Purpose: video.Purpose, DeclaredMIME: "image/jpeg", FileName: f.file,
				Size: int64(len(frame)), Body: bytes.NewReader(frame), ExpectKind: domain.KindImage,
				Derive: &DeriveInfo{SourceMediaID: video.ID, Kind: f.kind, Index: 1},
			})
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s：%w", f.kind, err))
		}
	}
	return errors.Join(errs...)
}

// download 把视频落到本地临时文件（ffmpeg 要能 seek，管道里的流做不到从结尾往前找）。
func (s *Service) download(ctx context.Context, mediaID int64, dst string) error {
	body, _, err := s.Open(ctx, mediaID)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// extractFirstFrame 截第一帧（封面）；第一帧是黑场（淡入）就往后找，全是黑场才如实用第一帧。
func extractFirstFrame(ctx context.Context, dir, src string, durationMs int) ([]byte, error) {
	return pickFrame(ctx, dir, src, domain.FirstFrameArgs, domain.FirstFrameFallbacks(durationMs))
}

// extractLastFrame 截最后一帧（补镜的参考）；最后一帧是黑场（成片淡出到黑）就往前找。
func extractLastFrame(ctx context.Context, dir, src string, durationMs int) ([]byte, error) {
	return pickFrame(ctx, dir, src, domain.LastFrameArgs, domain.FallbackPositions(durationMs))
}

// pickFrame 先截首选的那一帧；是黑场就依次试 fallbacks 里的位置，都不行才如实用首选的那一帧。
func pickFrame(ctx context.Context, dir, src string, primary func(in, out string) []string,
	fallbacks []float64) ([]byte, error) {
	first := filepath.Join(dir, "primary.jpg")
	if err := runFFmpeg(ctx, primary(src, first)); err != nil {
		return nil, err
	}
	data, img, err := readFrame(first)
	if err != nil {
		return nil, err
	}
	if !domain.MostlyBlack(img) {
		return data, nil
	}
	for i, at := range fallbacks {
		alt := filepath.Join(dir, fmt.Sprintf("at-%d.jpg", i))
		if runFFmpeg(ctx, domain.FrameAtArgs(src, alt, at)) != nil {
			continue
		}
		if d, im, err := readFrame(alt); err == nil && !domain.MostlyBlack(im) {
			return d, nil
		}
	}
	return data, nil
}

func runFFmpeg(ctx context.Context, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, domain.PosterTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:] // 只留尾巴：真正的原因在最后几行
		}
		return fmt.Errorf("ffmpeg 截帧失败：%w %s", err, msg)
	}
	return nil
}

// readFrame 读截出来的帧。
//
// ⚠️ 文件不存在或是空的要当失败：ffmpeg 一帧都没解出来时什么都不写、退出码却是 0。
func readFrame(path string) ([]byte, image.Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("没有截出画面：%w", err)
	}
	if len(data) == 0 {
		return nil, nil, errors.New("没有截出画面（输出为空）")
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("截出来的画面解不开：%w", err)
	}
	return data, img, nil
}

// videoExt 给临时文件一个对的扩展名：ffmpeg 靠它和文件头一起判封装格式。
func videoExt(mime string) string {
	switch mime {
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	}
	return ".mp4"
}

// ---------------------------------------------------------------- 扫描

// PosterScanner 定期给还没有封面帧 / 末帧的视频截帧（CD-83）。
//
// ⚠️ 用扫描而不是在出片的主路径上截：封面是锦上添花，截帧失败（畸形文件、ffmpeg 卡住）
// 不能让一段已经扣过费、已经归档的视频变成「失败」；一条路径同时覆盖新视频与历史视频，
// 也不会出现「新出的有封面、以前的永远没有」。
type PosterScanner struct {
	svc      *Service
	log      *slog.Logger
	lock     Locker
	interval time.Duration
	batch    int

	mu     sync.Mutex
	failed map[int64]time.Time
}

func NewPosterScanner(svc *Service, log *slog.Logger, lock Locker) *PosterScanner {
	return &PosterScanner{
		svc: svc, log: log, lock: lock,
		// 新视频一分钟内就有封面；一轮最多截 10 段，截帧是 CPU 活，不和渲染抢太多
		interval: time.Minute,
		batch:    10,
		failed:   map[int64]time.Time{},
	}
}

func (p *PosterScanner) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	p.Once(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Once(ctx)
		}
	}
}

// failRetryAfter 截失败的视频过多久再试一次（畸形文件每分钟重试只会空耗 CPU）。
const failRetryAfter = time.Hour

// Once 跑一轮，返回截出了几张。
func (p *PosterScanner) Once(ctx context.Context) int {
	if p.lock != nil {
		if ok, err := p.lock.TryLock(ctx, "scan:video-poster", p.interval); err == nil && !ok {
			return 0
		}
	}
	list, err := p.svc.repo.FindVideosWithoutFrames(ctx, []string{DerivePoster, DeriveLastFrame}, p.skipList(), p.batch)
	if err != nil {
		p.log.Error("封面帧扫描失败", "error", err)
		return 0
	}
	done := 0
	for _, v := range list {
		if err := p.svc.EnsureFrames(ctx, v); err != nil {
			p.markFailed(v.ID)
			p.log.Warn("视频截封面帧失败，一小时后再试", "mediaId", v.ID, "error", err)
			continue
		}
		done++
	}
	if done > 0 {
		p.log.Info("视频封面帧已生成", "count", done)
	}
	return done
}

func (p *PosterScanner) skipList() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int64, 0, len(p.failed))
	for id, at := range p.failed {
		if time.Since(at) > failRetryAfter {
			delete(p.failed, id)
			continue
		}
		out = append(out, id)
	}
	return out
}

func (p *PosterScanner) markFailed(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed[id] = time.Now()
}
