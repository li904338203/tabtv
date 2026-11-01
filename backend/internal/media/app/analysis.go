package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/ffsem"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 剪辑台的预览素材与分析（docs/agent/smartcut SCT-16，SCD-30）。
//
// 剪辑台与节点卡只加载低清预览版（SCK-10）。Worker 的 AnalysisScanner 为用到的音视频生成预览版、
// 缩略图拼图、波形、识别用音轨与节拍，按素材缓存在 media_analysis，同一份只做一次、多实例也不重复。
//
// 与封面帧扫描器（poster.go）的区别是有意的：那边是「每分钟一批 + 一把全局锁」，一段长转码超过锁的 TTL
// 就会被另一个实例重复领走；这里逐行领取（skip locked + CAS）、执行者活着就续租约，晚到的写回因 CAS 不成立而作废。
// 而且只处理排进来的行、不扫全库 —— 否则一上线就把历史视频全转一遍。

// AnalysisWaker 在排进新的分析之后叫醒扫描器（装配层用 Redis 发布实现）。
//
// ⚠️ 叫醒只是加速：Redis 挂了或消息丢了，扫描器每 5 秒的兜底照样会领到（GO-11）。
type AnalysisWaker interface {
	Wake(ctx context.Context) error
}

// SetAnalysisWaker 装上叫醒端口；不装就只靠兜底扫描。
func (s *Service) SetAnalysisWaker(w AnalysisWaker) { s.analysisWaker = w }

// EnqueueAnalyses 给一批本工作区的素材排进分析，返回新排进的行数（已在排队 / 进行中 / 成功的不重复排）。
//
// 只排对该类媒体有意义的种类（domain.AnalysisApplies）：纯音频不排预览版，图片什么都不排。
// 不认识的种类直接报错 —— 调用方写错了种类名，静默跳过的话那份素材永远没有预览。
// 由 SCT-25（输入同步）与 SCT-33（识别前抽音轨）调用。
func (s *Service) EnqueueAnalyses(ctx context.Context, workspaceID int64, mediaIDs []int64,
	kinds []string) (int, error) {
	for _, k := range kinds {
		if domain.AnalysisEngine(k) == "" {
			return 0, fmt.Errorf("不认识的分析种类：%q", k)
		}
	}
	briefs, err := s.repo.Briefs(ctx, workspaceID, mediaIDs)
	if err != nil {
		return 0, err
	}
	var keys []repo.AnalysisKey
	seen := map[int64]bool{}
	for _, id := range mediaIDs {
		b, ok := briefs[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		mk := kindOf(b.ContentType)
		for _, k := range kinds {
			if domain.AnalysisApplies(k, mk) {
				keys = append(keys, repo.AnalysisKey{MediaID: id, Kind: k, Engine: domain.AnalysisEngine(k)})
			}
		}
	}
	n, err := s.repo.EnqueueAnalyses(ctx, s.nextID, workspaceID, keys)
	if err != nil {
		return 0, err
	}
	if n > 0 && s.analysisWaker != nil {
		if err := s.analysisWaker.Wake(ctx); err != nil && s.log != nil {
			s.log.Warn("叫醒分析扫描器失败，等兜底扫描", "error", err)
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- 取用

// PreviewAssets 是剪辑台取的预览素材（契约 PreviewAssets）。还没生成好的字段为 nil，Status 说明进度。
type PreviewAssets struct {
	MediaID   int64
	Status    map[string]string
	Preview   *PreviewProxy
	Filmstrip *Filmstrip
	Peaks     *Peaks
	Beats     *Beats
}

// PreviewProxy 预览版。
type PreviewProxy struct {
	MediaID    int64
	Width      int
	Height     int
	DurationMs int
}

// Filmstrip 缩略图拼图：第 i 帧在第 i/100 张、第 (i%100)/Cols 行、第 (i%100)%Cols 列。
type Filmstrip struct {
	FrameW     int    `json:"frameW"`
	FrameH     int    `json:"frameH"`
	IntervalMs int    `json:"intervalMs"`
	Cols       int    `json:"cols"`
	Count      int    `json:"count"`
	Sheets     IDList `json:"sheets"`
}

// IDList 是一组媒体 ID，JSON 里写成字符串数组（契约里 ID 一律是字符串：雪花号超过 2^53，JS 的数字装不下）。
// 读的时候数字与字符串都认。
type IDList []int64

func (l IDList) MarshalJSON() ([]byte, error) {
	out := make([]string, len(l))
	for i, id := range l {
		out[i] = strconv.FormatInt(id, 10)
	}
	return json.Marshal(out)
}

func (l *IDList) UnmarshalJSON(b []byte) error {
	var raw []json.Number
	if err := json.Unmarshal(b, &raw); err != nil {
		var strs []string
		if err2 := json.Unmarshal(b, &strs); err2 != nil {
			return err
		}
		raw = make([]json.Number, len(strs))
		for i, s := range strs {
			raw[i] = json.Number(s)
		}
	}
	out := make(IDList, 0, len(raw))
	for _, n := range raw {
		id, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil {
			return fmt.Errorf("媒体 ID 非法：%q", n)
		}
		out = append(out, id)
	}
	*l = out
	return nil
}

// Peaks 波形：base64(uint8[])，每 IntervalMs 一个峰值。NoAudio 表示素材没有音轨（不是失败）。
type Peaks struct {
	IntervalMs int    `json:"intervalMs"`
	Values     string `json:"values"`
	NoAudio    bool   `json:"noAudio,omitempty"`
}

// Beats 节拍（SCT-17）。NoAudio 同上。
type Beats struct {
	domain.BeatResult
	NoAudio bool `json:"noAudio,omitempty"`
}

// speechResult 抽出的识别用音轨（结果里只记时长与有没有音轨；音频本身是派生媒体）。
type speechResult struct {
	DurationMs int  `json:"durationMs"`
	NoAudio    bool `json:"noAudio,omitempty"`
}

type proxyResult struct {
	Width      int `json:"width"`
	Height     int `json:"height"`
	DurationMs int `json:"durationMs"`
}

// notApplicable 是「这份素材不适用这种分析」的结果（如带 video/mp4 头的纯音频文件）：记成功，免得一直重试。
type notApplicable struct {
	NotApplicable bool `json:"notApplicable"`
}

// previewStatusKinds 是 PreviewAssets.status 里列出的种类（契约的说明里那六种）。
var previewStatusKinds = []string{domain.AnalysisPreviewProxy, domain.AnalysisFilmstrip, domain.AnalysisPeaks,
	domain.AnalysisBeats, domain.AnalysisSpeechAudio, domain.AnalysisASR}

// PreviewAssets 取一份媒体的预览素材。媒体不在本工作区（或已删除）返回 404。
func (s *Service) PreviewAssets(ctx context.Context, workspaceID, mediaID int64) (PreviewAssets, error) {
	if _, err := s.repo.GetScoped(ctx, workspaceID, mediaID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return PreviewAssets{}, httpx.Err(errcode.NotFound)
		}
		return PreviewAssets{}, httpx.Wrap(errcode.Internal, err)
	}
	rows, err := s.repo.ListAnalyses(ctx, workspaceID, mediaID)
	if err != nil {
		return PreviewAssets{}, httpx.Wrap(errcode.Internal, err)
	}
	out := PreviewAssets{MediaID: mediaID, Status: map[string]string{}}
	for _, kind := range previewStatusKinds {
		row := pickAnalysis(rows, kind, domain.AnalysisEngine(kind))
		if row == nil {
			out.Status[kind] = "none"
			continue
		}
		out.Status[kind] = row.Status
		if row.Status != "success" || len(row.Result) == 0 {
			continue
		}
		var na notApplicable
		if json.Unmarshal(row.Result, &na) == nil && na.NotApplicable {
			continue
		}
		switch kind {
		case domain.AnalysisPreviewProxy:
			var r proxyResult
			if json.Unmarshal(row.Result, &r) == nil && row.DerivedMediaID != 0 {
				out.Preview = &PreviewProxy{MediaID: row.DerivedMediaID, Width: r.Width, Height: r.Height,
					DurationMs: r.DurationMs}
			}
		case domain.AnalysisFilmstrip:
			var r Filmstrip
			if json.Unmarshal(row.Result, &r) == nil && len(r.Sheets) > 0 {
				out.Filmstrip = &r
			}
		case domain.AnalysisPeaks:
			var r Peaks
			if json.Unmarshal(row.Result, &r) == nil {
				out.Peaks = &r
			}
		case domain.AnalysisBeats:
			var r Beats
			if json.Unmarshal(row.Result, &r) == nil {
				out.Beats = &r
			}
		}
	}
	return out, nil
}

// pickAnalysis 选出某种类的「当前」那一行：引擎版本是当前的（engine 为空表示不限，识别由模型码区分），
// 优先排队 / 进行中 / 成功的那一行（最多一行，唯一索引保证），没有再取最近一次失败的。
// rows 已按创建时间倒序。
func pickAnalysis(rows []repo.Analysis, kind, engine string) *repo.Analysis {
	var failed *repo.Analysis
	for i := range rows {
		r := &rows[i]
		if r.Kind != kind || (engine != "" && r.Engine != engine) {
			continue
		}
		if r.Status != "failed" {
			return r
		}
		if failed == nil {
			failed = r
		}
	}
	return failed
}

// ---------------------------------------------------------------- 扫描

// AnalysisScanner 领取并执行分析（Worker）。一个实例同一时刻只做一行；多实例靠 skip locked 分摊。
type AnalysisScanner struct {
	svc      *Service
	log      *slog.Logger
	sem      *ffsem.Sem
	interval time.Duration
	lease    time.Duration
	wake     <-chan struct{}
}

// NewAnalysisScanner 建扫描器。sem 是与渲染共用的 ffmpeg 并发信号量（nil = 不限）。
func NewAnalysisScanner(svc *Service, log *slog.Logger, sem *ffsem.Sem) *AnalysisScanner {
	return &AnalysisScanner{svc: svc, log: log, sem: sem, interval: 5 * time.Second, lease: 10 * time.Minute}
}

// SetInterval 兜底扫描间隔（SEEAITV_ANALYSIS_SCAN_INTERVAL，默认 5 秒）。
func (a *AnalysisScanner) SetInterval(d time.Duration) {
	if d > 0 {
		a.interval = d
	}
}

// SetLease 租约时长（SEEAITV_ANALYSIS_LEASE，默认 10 分钟）。执行者每 1/3 租约续一次。
func (a *AnalysisScanner) SetLease(d time.Duration) {
	if d > 0 {
		a.lease = d
	}
}

// SetWake 装上叫醒信号（Redis 订阅转发过来的）。
func (a *AnalysisScanner) SetWake(ch <-chan struct{}) { a.wake = ch }

// Run 一直跑到 ctx 结束：每次被叫醒或到了兜底间隔，就把能领的全部做完。
func (a *AnalysisScanner) Run(ctx context.Context) {
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		a.Drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.wake:
		}
	}
}

// Drain 领到没有为止，返回处理了几行。
func (a *AnalysisScanner) Drain(ctx context.Context) int {
	if n, err := a.svc.repo.ReapAnalyses(ctx, domain.ScannerKinds, domain.AnalysisMaxAttempts); err != nil {
		a.log.Error("回收中断的分析失败", "error", err)
	} else if n > 0 {
		a.log.Warn("有分析中断满次数，已判失败", "count", n)
	}
	done := 0
	for ctx.Err() == nil {
		ok, err := a.Once(ctx)
		if err != nil {
			a.log.Error("领取分析失败", "error", err)
			return done
		}
		if !ok {
			return done
		}
		done++
	}
	return done
}

// Once 领一行并做完；没有可领的返回 false。
func (a *AnalysisScanner) Once(ctx context.Context) (bool, error) {
	job, err := a.svc.repo.ClaimAnalysis(ctx, domain.ScannerKinds, a.lease, domain.AnalysisMaxAttempts)
	if errors.Is(err, repo.ErrNoAnalysis) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	a.process(ctx, job)
	return true, nil
}

// permanentError 是重试也没用的失败（素材已删除、超过体积上限、认不出的格式）：直接判 failed，不白转三遍码。
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

func (a *AnalysisScanner) process(parent context.Context, job repo.Analysis) {
	started := time.Now()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// 续租：执行者活着就一直续，续不上说明这一行已经不归自己了（被接管、被判失败、被删）——
	// 立即停手（ffmpeg 随 ctx 被杀），结果也不写。
	var lost atomic.Bool
	go a.heartbeat(ctx, cancel, job, &lost)

	result, derived, err := a.svc.runAnalysis(ctx, job, a.sem)
	cancel()
	log := a.log.With("analysisId", job.ID, "mediaId", job.MediaObjectID, "kind", job.Kind, "attempt", job.Attempts)
	if lost.Load() {
		log.Warn("分析的租约已被接管，本次结果作废")
		return
	}
	// 进程在退出：放回排队、不算这一次（换个实例接着做），而不是记一次失败。
	if parent.Err() != nil {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		if err := a.svc.repo.ReleaseAnalysis(rctx, job.ID, job.Attempts); err != nil {
			log.Warn("退出时放回分析失败，等租约过期后由别的实例接管", "error", err)
		}
		return
	}
	if err == nil {
		body, merr := json.Marshal(result)
		if merr != nil {
			err = merr
		} else {
			ok, cerr := a.svc.repo.CompleteAnalysis(parent, job.ID, job.Attempts, body, derived)
			switch {
			case cerr != nil:
				log.Error("写分析结果失败", "error", cerr)
			case !ok:
				log.Warn("分析已不归本实例（租约被接管），结果作废")
			default:
				log.Info("预览素材已生成", "elapsedMs", time.Since(started).Milliseconds(), "derivedMediaId", derived)
			}
			return
		}
	}
	var perm permanentError
	permanent := errors.As(err, &perm)
	status, ferr := a.svc.repo.FailAnalysis(parent, job.ID, job.Attempts, err.Error(),
		domain.AnalysisRetryAfter(job.Attempts), domain.AnalysisMaxAttempts, permanent)
	switch {
	case ferr != nil:
		log.Error("记录分析失败时出错", "error", ferr, "cause", err)
	case status == "failed":
		log.Warn("分析失败，不再重试", "error", err)
	case status == "":
		log.Warn("分析已不归本实例（租约被接管），失败不记", "error", err)
	default:
		log.Warn("分析失败，稍后重试", "error", err)
	}
}

func (a *AnalysisScanner) heartbeat(ctx context.Context, cancel context.CancelFunc, job repo.Analysis,
	lost *atomic.Bool) {
	every := a.lease / 3
	if every <= 0 {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok, err := a.svc.repo.RenewAnalysisLease(ctx, job.ID, job.Attempts, a.lease)
			if err != nil {
				// 连不上库：接着做，下一次再续；真的过期了，写回时 CAS 不成立，结果自然作废。
				if ctx.Err() == nil {
					a.log.Warn("续分析租约失败", "analysisId", job.ID, "error", err)
				}
				continue
			}
			if !ok {
				lost.Store(true)
				cancel()
				return
			}
		}
	}
}

// runAnalysis 执行一行分析，返回写进 result 的结构与派生媒体 ID。
func (s *Service) runAnalysis(ctx context.Context, job repo.Analysis, sem *ffsem.Sem) (any, int64, error) {
	m, err := s.repo.GetScoped(ctx, job.WorkspaceID, job.MediaObjectID)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, 0, permanentError{errors.New("素材已删除或不存在")}
	}
	if err != nil {
		return nil, 0, err
	}
	if m.WorkspaceID != job.WorkspaceID {
		// 平台素材（工作区 0）不做：派生件会落在工作区 0 上，谁都能读到。排队时就过滤了，这里是兜底。
		return nil, 0, permanentError{errors.New("平台素材不生成预览")}
	}
	if !domain.AnalysisApplies(job.Kind, kindOf(m.ContentType)) {
		return notApplicable{NotApplicable: true}, 0, nil
	}
	if s.store == nil {
		return nil, 0, errors.New("对象存储未配置")
	}
	ctx, cancel := context.WithTimeout(ctx, domain.AnalysisTimeout(m.DurationMs))
	defer cancel()

	dir, err := os.MkdirTemp("", "seeaitv-analysis-*")
	if err != nil {
		return nil, 0, err
	}
	// ⚠️ 源文件与产物放同一个目录、整体删：逐个删的话中途 return 会漏下几百 MB 的文件。
	defer func() { _ = os.RemoveAll(dir) }()

	src := filepath.Join(dir, "source"+mediaExt(m.ContentType))
	if err := s.download(ctx, m.ID, src); err != nil {
		return nil, 0, fmt.Errorf("取素材失败：%w", err)
	}
	info, err := domain.ProbeStreams(ctx, src)
	if err != nil {
		return nil, 0, err
	}
	run := analysisRun{svc: s, ctx: ctx, sem: sem, dir: dir, src: src, media: m, info: info,
		engine: domain.AnalysisEngine(job.Kind)}
	switch job.Kind {
	case domain.AnalysisPreviewProxy:
		return run.previewProxy()
	case domain.AnalysisFilmstrip:
		return run.filmstrip()
	case domain.AnalysisPeaks:
		return run.peaks()
	case domain.AnalysisSpeechAudio:
		return run.speechAudio()
	case domain.AnalysisBeats:
		return run.beats()
	}
	return nil, 0, permanentError{fmt.Errorf("扫描器不处理这种分析：%s", job.Kind)}
}

type analysisRun struct {
	svc    *Service
	ctx    context.Context
	sem    *ffsem.Sem
	dir    string
	src    string
	media  repo.MediaObject
	info   domain.StreamInfo
	engine string
}

func (r analysisRun) previewProxy() (any, int64, error) {
	if !r.info.HasVideo {
		return notApplicable{NotApplicable: true}, 0, nil
	}
	// 上一次做到一半（已上传、没来得及写结果就崩了）：直接用那一份，不重转。
	if existing, ok := r.existing(DerivePreviewProxy, 1); ok {
		var res proxyResult
		if json.Unmarshal(existing.DeriveMeta, &res) == nil && res.Width > 0 {
			return res, existing.ID, nil
		}
	}
	out := filepath.Join(r.dir, "preview.mp4")
	if err := r.ffmpeg(domain.ProxyArgs(r.src, out, r.info.FPS), nil); err != nil {
		return nil, 0, err
	}
	got, err := domain.ProbeStreams(r.ctx, out)
	if err != nil {
		return nil, 0, err
	}
	res := proxyResult{Width: got.Width, Height: got.Height, DurationMs: got.DurationMs}
	meta, _ := json.Marshal(res)
	id, err := r.upload(out, "video/mp4", "preview.mp4", domain.KindVideo, DerivePreviewProxy, 1, meta)
	if err != nil {
		return nil, 0, err
	}
	return res, id, nil
}

func (r analysisRun) filmstrip() (any, int64, error) {
	if !r.info.HasVideo {
		return notApplicable{NotApplicable: true}, 0, nil
	}
	framesDir := filepath.Join(r.dir, "frames")
	if err := os.Mkdir(framesDir, 0o700); err != nil {
		return nil, 0, err
	}
	if err := r.ffmpeg(domain.FilmstripFramesArgs(r.src, filepath.Join(framesDir, "f-%06d.jpg")), nil); err != nil {
		return nil, 0, err
	}
	names, err := filepath.Glob(filepath.Join(framesDir, "f-*.jpg"))
	if err != nil {
		return nil, 0, err
	}
	slices.Sort(names)
	if len(names) == 0 {
		// ffmpeg 一帧都没解出来时什么都不写、退出码却是 0。
		return nil, 0, errors.New("没有抽出画面")
	}
	frameW, frameH, err := jpegSize(names[0])
	if err != nil {
		return nil, 0, err
	}
	cols := domain.FilmstripColsFor(len(names))
	res := Filmstrip{FrameW: frameW, FrameH: frameH, IntervalMs: domain.FilmstripIntervalMs, Cols: cols,
		Count: len(names)}
	start := 0
	for i, n := range domain.FilmstripSheets(len(names)) {
		index := i + 1
		if existing, ok := r.existing(DeriveFilmstrip, index); ok {
			res.Sheets = append(res.Sheets, existing.ID)
			start += n
			continue
		}
		id, err := r.filmstripSheet(names[start:start+n], cols, frameW, frameH, index)
		if err != nil {
			return nil, 0, err
		}
		res.Sheets = append(res.Sheets, id)
		start += n
	}
	return res, res.Sheets[0], nil
}

// filmstripSheet 拼一张并上传。一张最多 100 帧、每帧解码后几十 KB，整张的帧同时在内存里也只有几 MB。
func (r analysisRun) filmstripSheet(names []string, cols, frameW, frameH, index int) (int64, error) {
	frames := make([]image.Image, 0, len(names))
	for _, name := range names {
		img, err := decodeJPEG(name)
		if err != nil {
			return 0, err
		}
		frames = append(frames, img)
	}
	sheet := domain.ComposeSheet(frames, cols, frameW, frameH)
	path := filepath.Join(r.dir, "sheet-"+strconv.Itoa(index)+".jpg")
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	if err := jpeg.Encode(f, sheet, &jpeg.Options{Quality: 75}); err != nil {
		_ = f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	return r.upload(path, "image/jpeg", "filmstrip-"+strconv.Itoa(index)+".jpg", domain.KindImage,
		DeriveFilmstrip, index, nil)
}

func (r analysisRun) peaks() (any, int64, error) {
	if !r.info.HasAudio {
		// 无声视频不是失败（生成出来的视频十有八九没有音轨）：标「无音轨」，前端画一条平线。
		return Peaks{IntervalMs: domain.PeaksIntervalMs, Values: "", NoAudio: true}, 0, nil
	}
	var values []byte
	err := r.ffmpeg(domain.PeaksPCMArgs(r.src), func(out io.Reader) error {
		v, err := domain.PeaksFromPCM(out)
		values = v
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return Peaks{IntervalMs: domain.PeaksIntervalMs, Values: base64.StdEncoding.EncodeToString(values)}, 0, nil
}

func (r analysisRun) speechAudio() (any, int64, error) {
	if !r.info.HasAudio {
		// 没有音轨就不发起识别、不收费（SC-AC-51）：识别那一侧看这个标记。
		return speechResult{NoAudio: true}, 0, nil
	}
	if existing, ok := r.existing(DeriveSpeechAudio, 1); ok {
		var res speechResult
		if json.Unmarshal(existing.DeriveMeta, &res) == nil {
			return res, existing.ID, nil
		}
	}
	out := filepath.Join(r.dir, "speech.mp3")
	if err := r.ffmpeg(domain.SpeechAudioArgs(r.src, out), nil); err != nil {
		return nil, 0, err
	}
	got, err := domain.ProbeStreams(r.ctx, out)
	if err != nil {
		return nil, 0, err
	}
	res := speechResult{DurationMs: got.DurationMs}
	meta, _ := json.Marshal(res)
	id, err := r.upload(out, "audio/mpeg", "speech.mp3", domain.KindAudio, DeriveSpeechAudio, 1, meta)
	if err != nil {
		return nil, 0, err
	}
	return res, id, nil
}

func (r analysisRun) beats() (any, int64, error) {
	if !r.info.HasAudio {
		return Beats{BeatResult: domain.BeatResult{BeatsMs: []int{}, DownbeatsMs: []int{}, DownbeatApprox: true},
			NoAudio: true}, 0, nil
	}
	var res domain.BeatResult
	err := r.ffmpeg(domain.BeatsPCMArgs(r.src), func(out io.Reader) error {
		b, err := domain.DetectBeats(out)
		res = b
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return Beats{BeatResult: res}, 0, nil
}

// existing 找上一次已经上传过的派生件（同一引擎版本、同一编号）。
func (r analysisRun) existing(kind string, index int) (repo.MediaObject, bool) {
	list, err := r.svc.repo.ListDerived(r.ctx, r.media.WorkspaceID, r.media.ID, kind, r.engine)
	if err != nil {
		return repo.MediaObject{}, false
	}
	for _, m := range list {
		if m.DeriveIndex == index {
			return m, true
		}
	}
	return repo.MediaObject{}, false
}

// upload 把产物写成源素材的派生媒体：沿用源的 purpose（不进「我的素材」，SCD-37），引擎版本记在 derive_key 上。
//
// ⚠️ 体积超限、格式认不出这类失败重试也没用，判永久失败（不白转三遍码）。
func (r analysisRun) upload(path, mime, name string, kind domain.Kind, deriveKind string, index int,
	meta []byte) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() == 0 {
		return 0, errors.New("产物为空文件")
	}
	up, err := r.svc.Upload(r.ctx, UploadInput{
		WorkspaceID: r.media.WorkspaceID, UserID: r.media.CreatedByUserID, Purpose: r.media.Purpose,
		DeclaredMIME: mime, FileName: name, Size: st.Size(), Body: f, ExpectKind: kind,
		Derive: &DeriveInfo{SourceMediaID: r.media.ID, Kind: deriveKind, Key: r.engine, Index: index, Meta: meta},
	})
	if err != nil {
		var be *httpx.BizError
		if errors.As(err, &be) && (be.Code == errcode.PayloadTooLarge || be.Code == errcode.UnsupportedMediaType ||
			be.Code == errcode.FileContentMismatch) {
			return 0, permanentError{err}
		}
		return 0, err
	}
	return up.ID, nil
}

// ffmpeg 拿到信号量后跑一次 ffmpeg。consume 非空时从标准输出读（管道），读完再等进程退出。
//
// ⚠️ 失败时带 stderr 的尾巴（真正的原因在最后几行）；管道读失败时先杀进程再等，不然 ffmpeg 会卡在写管道上。
func (r analysisRun) ffmpeg(args []string, consume func(io.Reader) error) error {
	release, err := r.sem.Acquire(r.ctx)
	if err != nil {
		return err
	}
	defer release()
	var stderr strings.Builder
	cmd := exec.CommandContext(r.ctx, "ffmpeg", args...)
	cmd.Stderr = &tailBuffer{w: &stderr, max: 4 << 10}
	if consume == nil {
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ffmpeg 失败：%w %s", err, domain.StderrTail(stderr.String(), 300))
		}
		return nil
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	cerr := consume(out)
	if cerr != nil {
		_ = cmd.Process.Kill()
	}
	werr := cmd.Wait()
	if cerr != nil {
		return cerr
	}
	if werr != nil {
		return fmt.Errorf("ffmpeg 失败：%w %s", werr, domain.StderrTail(stderr.String(), 300))
	}
	return nil
}

// tailBuffer 只留最后约 max 字节（真正的原因在最后几行；出错时 stderr 可能刷几千行）。
type tailBuffer struct {
	w   *strings.Builder
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.w.Write(p)
	if t.w.Len() > t.max*2 {
		s := t.w.String()
		t.w.Reset()
		t.w.WriteString(s[len(s)-t.max:])
	}
	return len(p), nil
}

func jpegSize(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("抽出的画面解不开：%w", err)
	}
	return cfg.Width, cfg.Height, nil
}

func decodeJPEG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("抽出的画面解不开：%w", err)
	}
	return img, nil
}

// mediaExt 给临时文件一个对的扩展名：ffmpeg 靠它和文件头一起判封装格式。
func mediaExt(mime string) string {
	switch mime {
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	}
	return videoExt(mime)
}

// AnalysisResults 每份媒体某种分析最近一次成功的结果（智能剪辑的规划取节拍、波形、识别、看图描述用）。
func (s *Service) AnalysisResults(ctx context.Context, workspaceID int64, mediaIDs []int64, kind, engine string) (
	map[int64]json.RawMessage, error) {
	return s.repo.LatestResults(ctx, workspaceID, mediaIDs, kind, engine)
}

// SaveCaption 写回模型对一段素材的看图描述（kind=caption，engine=模型码）。
func (s *Service) SaveCaption(ctx context.Context, workspaceID, mediaID int64, engine, text string) error {
	raw, err := json.Marshal(map[string]string{"text": text, "model": engine})
	if err != nil {
		return err
	}
	return s.repo.SaveCaption(ctx, s.nextID(), workspaceID, mediaID, engine, raw)
}
