package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/platform/ffbin"
	"github.com/li904338203/tabtv/backend/pkg/h264"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/platform/ffsem"
)

// 输入区间截取（docs/agent/smartcut SCD-34）。
//
// 受理时声明的输入可以带 `rangeMs: [起点, 终点]`：去字幕、按画面配音效只把片段用到的那一段发给上游，
// 并按这一段计价。截取放在 Worker 侧（重新编码是 CPU 活，API 进程不跑长任务），发给上游前做：
//
//  1. 取整段素材到临时文件（流式，不进内存 —— 源视频可能几百 MB）；
//  2. ffmpeg 重新编码截出 [起点, 终点]（`-ss` 放在 `-i` 前面且重新编码，精确到帧）；
//  3. 按**原来的规则**给上游：主存储签得出上游够得着的地址就放上去发地址；协议只收地址时走公网中转；
//     否则内联（同样受内联上限约束）。
//
// ⚠️ 临时文件截完就删；放到对象存储的那份**用完即删**：同步协议提交返回就删，异步协议等查询到终态再删
// （上游可能排队几分钟才来取）。Worker 崩了、任务被取消这类没走到终态的，由 ScratchSweeper 按时间兜底。

// MediaOpener（可选）：按媒体 ID 流式打开字节。没实现时截取回落到 Inline（读进内存）。
type MediaOpener interface {
	Open(ctx context.Context, mediaID int64) (io.ReadCloser, string, error)
}

// ScratchStore（可选）：放 Worker 现场截出来的片段给上游取。主存储与公网中转各接一个（objstore.Client）。
type ScratchStore interface {
	PutFile(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error
	PresignGet(ctx context.Context, key string) (string, error)
	Delete(ctx context.Context, key string) error
}

// ScratchPrefix 是截出来的片段在对象存储里的前缀；清理作业只扫这个前缀。
const ScratchPrefix = "scratch/range/"

// cutTimeout 是一次截取的上限：几十秒的片段重新编码只要几秒，卡住的 ffmpeg 不能一直占着 CPU 名额。
const cutTimeout = 5 * time.Minute

// SetScratch 接上截取片段的存放处：main 是主存储，relay 是公网中转（可为 nil）。
func (p *HTTPProvider) SetScratch(main, relay ScratchStore) { p.scratch, p.relayScratch = main, relay }

// SetFFmpegSem 接上进程级 ffmpeg 并发信号量（与渲染、预览素材扫描器共用，SCD-30）。
func (p *HTTPProvider) SetFFmpegSem(s *ffsem.Sem) { p.ffSem = s }

// hasRange 判断一条输入要不要截取。只有音视频截得了：图片带区间在受理时就被拒了，这里再防一手。
func hasRange(in resolvedInput) bool {
	return len(in.RangeMs) == 2 && (in.MediaKind == "video" || in.MediaKind == "audio") &&
		in.RangeMs[0] >= 0 && in.RangeMs[1] > in.RangeMs[0]
}

func scratchKey(generationID int64, in resolvedInput) string {
	return fmt.Sprintf("%s%d/%d-%d-%d%s", ScratchPrefix, generationID, in.Order, in.RangeMs[0], in.RangeMs[1],
		cutExt(in.MediaKind))
}

func cutExt(kind string) string {
	if kind == "audio" {
		return ".mp3"
	}
	return ".mp4"
}

func cutMIME(kind string) string {
	if kind == "audio" {
		return "audio/mpeg"
	}
	return "video/mp4"
}

// cutArgs 是截取的 ffmpeg 参数（纯函数，argv 断言测试）。
//
//   - `-ss` 放在 `-i` 前面：输入级定位；重新编码时 ffmpeg 会把定位点之前的帧解码后丢掉，所以是**精确到帧**的
//     （只有 `-c copy` 才会落到关键帧上，那样起点会提前最多一个 GOP，片段里多出一截别的画面）。
//   - 视频统一成 H.264 + yuv420p + AAC、宽高取偶数：上游收得最广，奇数宽高的源在 yuv420p 下直接编码失败。
//   - 只取第一路画面与（可选的）第一路声音：字幕流、数据流混进去，有的上游会拒收整个文件。
//   - 音频出 MP3：识别、配乐类上游都收。
func cutArgs(src, dst, kind string, inMs, outMs int, enc h264.Encoder) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-ss", msSeconds(inMs), "-i", src, "-t", msSeconds(outMs - inMs)}
	if kind == "audio" {
		return append(args, "-map", "0:a:0", "-vn", "-c:a", "libmp3lame", "-b:a", "192k", dst)
	}
	args = append(args, "-map", "0:v:0", "-map", "0:a:0?",
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2")
	args = append(args, h264.EncodeArgs(enc, h264.Intent{Tier: h264.TierCut})...)
	return append(args, "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", dst)
}

func msSeconds(ms int) string { return fmt.Sprintf("%d.%03d", ms/1000, ms%1000) }

// rangeValue 截出一条输入的区间，按原规则变成上游能用的形态（签名外链 / 公网中转 / 内联）。
func (p *HTTPProvider) rangeValue(ctx context.Context, generationID int64, in resolvedInput, urlOnly bool) (
	string, string, error) {
	key, mime := scratchKey(generationID, in), cutMIME(in.MediaKind)
	// 先签名再截：签出来的地址上游够不着的话，不必白截、白传一趟。
	if p.scratch != nil {
		if u, err := p.scratch.PresignGet(ctx, key); err == nil && upstreamReachable(u) {
			if err := p.cutToStore(ctx, in, p.scratch, key, mime); err != nil {
				return "", "", err
			}
			return u, mime, nil
		}
	}
	if urlOnly {
		why := "主存储签出的地址上游访问不到"
		if p.relayScratch != nil {
			u, err := p.relayScratch.PresignGet(ctx, key)
			switch {
			case err != nil:
				why += "；签发公网中转地址失败：" + err.Error()
			case !upstreamReachable(u):
				why += "；公网中转存储签出的地址同样访问不到（检查 SEEAITV_RELAY_STORAGE_ENDPOINT）"
			default:
				if err := p.cutToStore(ctx, in, p.relayScratch, key, mime); err != nil {
					return "", "", err
				}
				return u, mime, nil
			}
		}
		return "", "", fmt.Errorf("该模型的上游只收公网可访问的媒体地址，而参考媒体 %d 截出来的片段%s；"+
			"请配置公网可访问的对象存储（SEEAITV_STORAGE_*）或公网中转存储（SEEAITV_RELAY_STORAGE_*）",
			in.MediaID, why)
	}

	path, cleanup, err := p.cutFile(ctx, in)
	if err != nil {
		return "", "", err
	}
	defer cleanup()
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("读取截好的片段失败: %w", err)
	}
	if int64(len(data)) > p.maxInlineBytes() {
		return "", "", fmt.Errorf("参考媒体 %d 截出来的片段有 %d 字节，超过内联上限 %d；请配置可对外签发地址的对象存储",
			in.MediaID, len(data), p.maxInlineBytes())
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), mime, nil
}

// cutToStore 截好之后放进对象存储。临时文件一定删。
func (p *HTTPProvider) cutToStore(ctx context.Context, in resolvedInput, store ScratchStore, key, mime string) error {
	path, cleanup, err := p.cutFile(ctx, in)
	if err != nil {
		return err
	}
	defer cleanup()
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开截好的片段失败: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("读取截好的片段失败: %w", err)
	}
	if err := store.PutFile(ctx, key, f, st.Size(), mime); err != nil {
		return fmt.Errorf("上传截好的片段失败: %w", err)
	}
	return nil
}

// cutFile 把素材取到临时目录并截出区间，返回截好的文件与清理函数（删掉整个临时目录）。
func (p *HTTPProvider) cutFile(ctx context.Context, in resolvedInput) (string, func(), error) {
	dir, err := os.MkdirTemp("", "seeaitv-cut-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("建临时目录失败: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	src := filepath.Join(dir, "src")
	if err := p.fetchSource(ctx, in.MediaID, src); err != nil {
		cleanup()
		return "", func() {}, err
	}
	dst := filepath.Join(dir, "cut"+cutExt(in.MediaKind))
	if err := p.runCut(ctx, cutArgs(src, dst, in.MediaKind, in.RangeMs[0], in.RangeMs[1], ffbin.H264().Encoder)); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("截取参考媒体 %d 的 %s~%s 秒失败: %w", in.MediaID,
			msSeconds(in.RangeMs[0]), msSeconds(in.RangeMs[1]), err)
	}
	if st, err := os.Stat(dst); err != nil || st.Size() == 0 {
		// 0 字节当失败：发一个空文件出去，上游回的错误与原因毫不相干。
		cleanup()
		return "", func() {}, fmt.Errorf("截取参考媒体 %d 得到的是空文件", in.MediaID)
	}
	return dst, cleanup, nil
}

// fetchSource 把整段素材写到 dst：能流式打开就流式，否则回落 Inline。
func (p *HTTPProvider) fetchSource(ctx context.Context, mediaID int64, dst string) error {
	if p.media == nil {
		return fmt.Errorf("媒体解析器未接入，无法取参考媒体 %d", mediaID)
	}
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("建临时文件失败: %w", err)
	}
	defer f.Close()
	if o, ok := p.media.(MediaOpener); ok {
		body, _, err := o.Open(ctx, mediaID)
		if err != nil {
			return fmt.Errorf("取参考媒体 %d 失败: %w", mediaID, err)
		}
		defer body.Close()
		if _, err := io.Copy(f, body); err != nil {
			return fmt.Errorf("取参考媒体 %d 失败: %w", mediaID, err)
		}
		return nil
	}
	data, _, err := p.media.Inline(ctx, mediaID)
	if err != nil {
		return fmt.Errorf("取参考媒体 %d 失败: %w", mediaID, err)
	}
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("写临时文件失败: %w", err)
	}
	return nil
}

// runCut 跑一次 ffmpeg：占一个 CPU 名额、带超时，失败时带 stderr 的尾巴回来。
func (p *HTTPProvider) runCut(ctx context.Context, args []string) error {
	if _, err := exec.LookPath(ffbin.FFmpeg()); err != nil {
		return errors.New("Worker 上没有 ffmpeg，截不出输入区间")
	}
	release, err := p.ffSem.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	cctx, cancel := context.WithTimeout(ctx, cutTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, ffbin.FFmpeg(), args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 400 {
			tail = "…" + tail[len(tail)-400:]
		}
		if tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

// dropScratch 删掉这个任务放在对象存储里的截取片段（主存储与公网中转各删一次，不存在的键删了也无妨）。
//
// ⚠️ 用独立的 context：任务的 ctx 可能正因为超时被取消，而那恰恰是该清理的时候。删不掉只记日志 ——
// 留下的那份由 ScratchSweeper 按时间兜底，不能为了清理失败让一个已经成功的任务变成失败。
func (p *HTTPProvider) dropScratch(ctx context.Context, job genapp.Job) {
	if p.scratch == nil && p.relayScratch == nil {
		return
	}
	inputs, err := parseInputs(job.Inputs)
	if err != nil {
		return
	}
	var keys []string
	for _, in := range inputs {
		if in.MediaID != 0 && hasRange(in) {
			keys = append(keys, scratchKey(job.GenerationID, in))
		}
	}
	if len(keys) == 0 {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for _, store := range []ScratchStore{p.scratch, p.relayScratch} {
		if store == nil {
			continue
		}
		for _, k := range keys {
			if err := store.Delete(dctx, k); err != nil {
				p.logf("删除截取片段失败，留给清理作业", "generationId", job.GenerationID, "key", k, "error", err)
			}
		}
	}
}

// keepScratch 判断提交之后截取片段还要不要留着：只有异步任务被受理（上游稍后才来取）时要留。
func keepScratch(res genapp.SubmitResult) bool { return res.Outcome == gendomain.SubmitAccepted }
