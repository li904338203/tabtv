//go:build integration

// 预览素材扫描器端到端（docs/agent/smartcut SCT-16 / SCT-17，SC-AC-64）：真的 ffmpeg + 内存对象存储。
// 排进素材 → 两个扫描器同时跑 → 每种只生成一份、产物规格对（ffprobe 量）→ 取用接口的形状；
// 以及执行者活着时（哪怕在排队等 CPU）不被接管。
package integration

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/media/domain"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/ffsem"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

const ws = int64(7)

type scanEnv struct {
	t    *testing.T
	svc  *mediaapp.Service
	repo *mediarepo.Repo
	s3   *testsupport.FakeS3
	dir  string
	log  *slog.Logger
}

func newScanEnv(t *testing.T) scanEnv {
	t.Helper()
	if !domain.FFmpegAvailable() || !domain.ProbeAvailable() {
		t.Skip("本机没有 ffmpeg / ffprobe")
	}
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	store, s3 := testsupport.FakeObjectStore(t)
	r := mediarepo.New(pool)
	svc := mediaapp.New(r, store, gen.Next)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc.SetLogger(log)
	return scanEnv{t: t, svc: svc, repo: r, s3: s3, dir: t.TempDir(), log: log}
}

// make 用 ffmpeg 造一份素材并以 upload_reference 上传，返回媒体 ID。
func (e scanEnv) make(name, mime string, args ...string) int64 {
	e.t.Helper()
	path := filepath.Join(e.dir, name)
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	if out, err := exec.Command("ffmpeg", append(full, path)...).CombinedOutput(); err != nil {
		e.t.Fatalf("造 %s 失败：%v %s", name, err, out)
	}
	f, err := os.Open(path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	up, err := e.svc.Upload(context.Background(), mediaapp.UploadInput{WorkspaceID: ws, UserID: 1,
		Purpose: "upload_reference", DeclaredMIME: mime, FileName: name, Size: st.Size(), Body: f})
	if err != nil {
		e.t.Fatalf("上传 %s 失败：%v", name, err)
	}
	return up.ID
}

// fetch 把派生媒体的字节从对象存储取到本地文件（给 ffprobe 量）。
func (e scanEnv) fetch(mediaID int64, ext string) string {
	e.t.Helper()
	m, err := e.repo.GetScoped(context.Background(), ws, mediaID)
	if err != nil {
		e.t.Fatalf("派生媒体 %d：%v", mediaID, err)
	}
	b, ok := e.s3.Object(m.ObjectKey)
	if !ok {
		e.t.Fatalf("对象存储里没有 %s", m.ObjectKey)
	}
	path := filepath.Join(e.dir, strconv.FormatInt(mediaID, 10)+ext)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e scanEnv) derived(source int64, kind string) []mediarepo.MediaObject {
	e.t.Helper()
	list, err := e.repo.ListDerived(context.Background(), ws, source, kind, domain.AnalysisEngine(kind))
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

// SC-AC-64：排进一段新视频，两个扫描器同时跑，预览版、拼图、波形各一份；无声视频不做波形（标无音轨）；
// 纯音频只做波形、识别用音轨与节拍。
func TestAnalysisScanner_端到端_两个实例不重复(t *testing.T) {
	e := newScanEnv(t)
	ctx := context.Background()
	video := e.make("land.mp4", "video/mp4", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "3.2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest")
	silent := e.make("silent.mp4", "video/mp4", "-f", "lavfi", "-i", "testsrc2=size=720x1280:rate=30",
		"-t", "2.5", "-c:v", "libx264", "-pix_fmt", "yuv420p")
	// 120 BPM 的点击音轨（每 0.5 秒一下）。
	music := e.make("click.mp3", "audio/mpeg", "-f", "lavfi", "-i",
		"aevalsrc='if(lt(mod(t,0.5),0.02),sin(2*PI*1000*t),0)':s=22050:d=8", "-c:a", "libmp3lame", "-b:a", "128k")

	all := []string{domain.AnalysisPreviewProxy, domain.AnalysisFilmstrip, domain.AnalysisPeaks,
		domain.AnalysisSpeechAudio, domain.AnalysisBeats}
	n, err := e.svc.EnqueueAnalyses(ctx, ws, []int64{video, silent, music, video}, all)
	// 视频 4 种（节拍只给音频）× 2 + 音频 3 种（预览版、拼图对音频没有意义）。
	if err != nil || n != 11 {
		t.Fatalf("排进 11 行：%d %v", n, err)
	}
	if _, err := e.svc.EnqueueAnalyses(ctx, ws, []int64{video}, []string{"nope"}); err == nil {
		t.Fatal("不认识的种类要报错")
	}

	var wg sync.WaitGroup
	done := make([]int, 2)
	for i := range done {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done[i] = mediaapp.NewAnalysisScanner(e.svc, e.log, ffsem.New(2)).Drain(ctx)
		}()
	}
	wg.Wait()
	if done[0]+done[1] != 11 {
		t.Fatalf("两个实例合计处理 11 行（不重复）：%v", done)
	}
	t.Logf("两个扫描器分别处理了 %d / %d 行", done[0], done[1])

	// ---- 视频 ----
	pa, err := e.svc.PreviewAssets(ctx, ws, video)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"preview_proxy", "filmstrip", "peaks", "speech_audio"} {
		if pa.Status[k] != "success" {
			t.Fatalf("%s：%q", k, pa.Status[k])
		}
	}
	if pa.Status["beats"] != "none" || pa.Status["asr"] != "none" || pa.Beats != nil {
		t.Fatalf("视频不做节拍，识别还没做：%v", pa.Status)
	}
	if len(e.derived(video, "preview_proxy")) != 1 || len(e.derived(video, "filmstrip")) != 1 ||
		len(e.derived(video, "speech_audio")) != 1 {
		t.Fatal("每种派生只有一份")
	}
	p := pa.Preview
	if p == nil || p.Width != 854 || p.Height != 480 || p.DurationMs < 3100 || p.DurationMs > 3400 {
		t.Fatalf("预览版短边 480：%+v", p)
	}
	proxyPath := e.fetch(p.MediaID, ".mp4")
	if kf := keyframes(t, proxyPath); len(kf) < 4 || !evenlySpaced(kf, 1.0) {
		t.Fatalf("每秒一个关键帧：%v", kf)
	}
	if atoms := topAtoms(t, proxyPath); strings.Index(atoms, "moov") > strings.Index(atoms, "mdat") {
		t.Fatalf("moov 在文件头（faststart）：%s", atoms)
	}
	fs := pa.Filmstrip
	// fps=1 在第 0、1、2 秒各取一帧（3.2 秒的片子，第 3 秒那一帧落在结尾之外）；帧数以实际抽出的为准。
	if fs == nil || fs.FrameW != 160 || fs.FrameH != 90 || fs.IntervalMs != 1000 || fs.Count != 3 || fs.Cols != 3 ||
		len(fs.Sheets) != 1 {
		t.Fatalf("拼图每秒一帧、宽 160：%+v", fs)
	}
	sheet, _ := domain.ProbeStreams(ctx, e.fetch(fs.Sheets[0], ".jpg"))
	if sheet.Width != 160*3 || sheet.Height != 90 {
		t.Fatalf("一行 3 帧：%+v", sheet)
	}
	peaks := decodePeaks(t, pa.Peaks)
	// lavfi 的 sine 默认振幅 1/8：峰值 4096 → 线性量化后约 32（量化的口径也一并核了）。
	if m := maxByte(peaks); pa.Peaks.NoAudio || pa.Peaks.IntervalMs != 10 || len(peaks) < 310 || len(peaks) > 330 ||
		m < 30 || m > 34 {
		t.Fatalf("波形每 10ms 一个、峰值约 32：%d 个、最大 %d", len(peaks), m)
	}
	speech := e.derived(video, "speech_audio")[0]
	if si, _ := domain.ProbeStreams(ctx, e.fetch(speech.ID, ".mp3")); !si.HasAudio || si.HasVideo {
		t.Fatalf("识别用音轨：%+v", si)
	}
	if out, _ := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=sample_rate,channels,bit_rate",
		"-of", "csv=p=0", e.fetch(speech.ID, ".mp3")).Output(); !strings.HasPrefix(strings.TrimSpace(string(out)), "16000,1,32000") {
		t.Fatalf("16k 单声道 32kbps：%s", out)
	}

	// ---- 无声竖屏视频 ----
	ps, _ := e.svc.PreviewAssets(ctx, ws, silent)
	if ps.Preview == nil || ps.Preview.Width != 480 || ps.Preview.Height != 854 {
		t.Fatalf("竖屏预览版：%+v", ps.Preview)
	}
	if ps.Status["peaks"] != "success" || ps.Peaks == nil || !ps.Peaks.NoAudio || ps.Peaks.Values != "" {
		t.Fatalf("无声视频的波形标「无音轨」、不算失败：%v %+v", ps.Status, ps.Peaks)
	}
	if ps.Status["speech_audio"] != "success" || len(e.derived(silent, "speech_audio")) != 0 {
		t.Fatal("无声视频不抽音轨（识别那一侧据此不发起、不收费）")
	}
	if ps.Filmstrip == nil || ps.Filmstrip.FrameH != 284 || ps.Filmstrip.Count != 3 {
		t.Fatalf("竖屏拼图：%+v", ps.Filmstrip)
	}

	// ---- 纯音频 ----
	pm, _ := e.svc.PreviewAssets(ctx, ws, music)
	if pm.Status["preview_proxy"] != "none" || pm.Status["filmstrip"] != "none" || pm.Preview != nil {
		t.Fatalf("纯音频不做预览版与拼图：%v", pm.Status)
	}
	if pm.Status["peaks"] != "success" || pm.Status["speech_audio"] != "success" || pm.Status["beats"] != "success" {
		t.Fatalf("纯音频做波形、音轨与节拍：%v", pm.Status)
	}
	if pm.Beats == nil || math.Abs(pm.Beats.BPM-120) > 2 || len(pm.Beats.BeatsMs) < 10 {
		t.Fatalf("点击音轨 120 BPM：%+v", pm.Beats)
	}
	t.Logf("点击音轨：%.2f BPM、%d 拍、置信度 %.2f", pm.Beats.BPM, len(pm.Beats.BeatsMs), pm.Beats.Confidence)

	// 重复排队不再生成；派生件不进「我的素材」；别的工作区取不到。
	if n, _ := e.svc.EnqueueAnalyses(ctx, ws, []int64{video, silent, music}, all); n != 0 {
		t.Fatalf("已成功的不再排：%d", n)
	}
	mine, _, _ := e.svc.ListMine(ctx, ws, "", 100, 0)
	if len(mine) != 3 {
		t.Fatalf("我的素材只有 3 份上传，派生件不在其中：%d", len(mine))
	}
	var be *httpx.BizError
	if _, err := e.svc.PreviewAssets(ctx, 8, video); !errors.As(err, &be) || be.Code != errcode.NotFound {
		t.Fatalf("跨工作区 404：%v", err)
	}
	// 预览素材不能单独删（随原素材一起删）。
	if err := e.svc.Delete(ctx, ws, p.MediaID); !errors.As(err, &be) || be.Code != errcode.ResourceInUse {
		t.Fatalf("预览版单独删要拒绝：%v", err)
	}
}

// 执行者在等 CPU（信号量被占满）时一直续租：另一个实例领不走；名额放开后它照常做完，attempts 只有 1。
func TestAnalysisScanner_活着的执行者不被接管(t *testing.T) {
	e := newScanEnv(t)
	ctx := context.Background()
	video := e.make("v.mp4", "video/mp4", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25", "-t", "2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p")
	if n, err := e.svc.EnqueueAnalyses(ctx, ws, []int64{video}, []string{domain.AnalysisPreviewProxy}); err != nil || n != 1 {
		t.Fatalf("排队：%d %v", n, err)
	}

	busy := ffsem.New(1)
	hold, _ := busy.Acquire(ctx) // 渲染占着唯一的名额
	a := mediaapp.NewAnalysisScanner(e.svc, e.log, busy)
	a.SetLease(600 * time.Millisecond)
	doneA := make(chan bool, 1)
	go func() {
		ok, err := a.Once(ctx)
		doneA <- ok && err == nil
	}()

	// 等 A 领到（进行中、卡在等名额上）再让 B 来抢。
	for wait := time.Now().Add(5 * time.Second); ; {
		rows, _ := e.repo.ListAnalyses(ctx, ws, video)
		if len(rows) == 1 && rows[0].Status == "running" {
			break
		}
		if time.Now().After(wait) {
			t.Fatal("A 没有领到")
		}
		time.Sleep(20 * time.Millisecond)
	}
	b := mediaapp.NewAnalysisScanner(e.svc, e.log, nil)
	b.SetLease(600 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second) // 三倍以上的租约
	for time.Now().Before(deadline) {
		if ok, err := b.Once(ctx); err != nil || ok {
			t.Fatalf("执行者还活着，另一个实例不该领到：%v %v", ok, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	hold()
	if !<-doneA {
		t.Fatal("执行者应做完")
	}
	rows, _ := e.repo.ListAnalyses(ctx, ws, video)
	if len(rows) != 1 || rows[0].Status != "success" || rows[0].Attempts != 1 {
		t.Fatalf("只做了一次：%+v", rows)
	}
}

func keyframes(t *testing.T, path string) []float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-skip_frame", "nokey",
		"-show_entries", "frame=pts_time", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var kf []float64
	for _, line := range strings.Fields(strings.ReplaceAll(string(out), ",", " ")) {
		if v, err := strconv.ParseFloat(line, 64); err == nil {
			kf = append(kf, v)
		}
	}
	return kf
}

func evenlySpaced(ts []float64, step float64) bool {
	for i := 1; i < len(ts); i++ {
		if math.Abs(ts[i]-ts[i-1]-step) > 0.05 {
			return false
		}
	}
	return true
}

// topAtoms 列出 MP4 顶层 box 的顺序（ftyp moov free mdat）。
func topAtoms(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := 0; i+8 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[i:]))
		names = append(names, string(b[i+4:i+8]))
		if size == 1 && i+16 <= len(b) {
			size = int(binary.BigEndian.Uint64(b[i+8:]))
		}
		if size < 8 {
			break
		}
		i += size
	}
	return strings.Join(names, " ")
}

func decodePeaks(t *testing.T, p *mediaapp.Peaks) []byte {
	t.Helper()
	if p == nil {
		t.Fatal("没有波形")
	}
	b, err := base64.StdEncoding.DecodeString(p.Values)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func maxByte(b []byte) byte {
	var m byte
	for _, v := range b {
		m = max(m, v)
	}
	return m
}
