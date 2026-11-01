package domain

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

// 渲染（NEW-10、NEW-11）。
//
// ⚠️ 这一层**只拼命令、只算预检**，不跑进程、不碰库 ——
// ffmpeg 的滤镜图是这一域最容易错的东西，而错了的表现几乎都是
// "渲染成功但片子不对"（少一段、音画错位、画面被拉扁）。
// 摊成纯函数之后，每一条都能用 argv 断言验住。

// 渲染任务状态。与生成任务同一套词。
const (
	RenderQueued    = "queued"
	RenderRunning   = "running"
	RenderSuccess   = "success"
	RenderFailed    = "failed"
	RenderCancelled = "cancelled"
)

// 渲染阶段。进度条靠它说"现在在干什么"——
// 只给一个百分比的话，卡在 40% 十分钟时用户不知道是死了还是在编码。
const (
	StageProbing    = "probing"
	StageAssembling = "assembling"
	// StageMixing 音频预渲（SCT-12：两遍 loudnorm，30 分钟的片子要几十秒）。
	StageMixing    = "mixing"
	StageEncoding  = "encoding"
	StageArchiving = "archiving"
	// StageChecking 导出后自检（SCT-15：时长、响度、黑场，只警告）。
	StageChecking = "checking"
)

// Watermark 是一次水印叠加。
//
// ⚠️ 不透明度与缩放都按**百分比**：像素值换画幅就不对了
// （1080p 上合适的 120px 到竖屏上就压住半个画面）。
type Watermark struct {
	MediaID  int64  `json:"mediaId"`
	Position string `json:"position"`
	Opacity  int    `json:"opacity"`
	Scale    int    `json:"scale"`
	// PixelWidth 是按画幅算好的目标宽度（像素）。
	//
	// ⚠️ 由**受理侧按画幅算**而不是在滤镜里算：单路滤镜链里拿不到
	// 主画面的尺寸（那要 `scale2ref`，而它的两路输入顺序极易搞反，
	// 搞反了水印会被缩放到整屏大小压住全片）。
	PixelWidth int `json:"pixelWidth"`
}

// 水印位置。
const (
	WatermarkTopLeft     = "top_left"
	WatermarkTopRight    = "top_right"
	WatermarkBottomLeft  = "bottom_left"
	WatermarkBottomRight = "bottom_right"
)

// ValidWatermarkPosition 位置是否合法。
//
// ⚠️ 与迁移里的 `ck_wm_position` 是同一份取值 —— 加一个位置要
// **同时改两处**，只改一处的症状是后台保存时 PG 报 23514。
func ValidWatermarkPosition(pos string) bool {
	switch pos {
	case WatermarkTopLeft, WatermarkTopRight,
		WatermarkBottomLeft, WatermarkBottomRight:
		return true
	}
	return false
}

// WatermarkMargin 水印离边的距离，按画面短边的百分比。
//
// ⚠️ 贴边（0）的话在有安全区的播放器上会被裁掉一半 ——
// 而"水印被裁掉"等于没有水印。
const WatermarkMargin = 3

// ---------------------------------------------------------------- 预检

// PrecheckIssue 是一条预检问题。
type PrecheckIssue struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	// ClipIdx 指出是画面轨的第几段（0 表示与具体片段无关）。
	ClipIdx int `json:"clipIdx,omitempty"`
}

// 预检问题码。
//
// 以前还有一条 `subtitle_unsupported`（本机 ffmpeg 没有 libass 时拦下带字幕的时间线）：
// 字幕与文字改成服务端排版的透明 PNG 叠加之后（SCT-14、SCD-28）不再需要 libass，这条去掉了。
const (
	PrecheckEmpty        = "empty_timeline"
	PrecheckMediaMissing = "media_missing"
	PrecheckTooLong      = "too_long"
	PrecheckNoFFmpeg     = "ffmpeg_missing"
	// PrecheckClipGenerating 片段级生成还在进行（SC-F-138）：导出的会是旧素材，而用户以为是新的。
	PrecheckClipGenerating = "clip_generating"
)

// MaxRenderMs 单次渲染的时长上限。
//
// ⚠️ 上限不是防滥用，是**让失败有边界**：一条两小时的时间线在单机上
// 要编码几十分钟，中途任何一次失败都是几十分钟白等 ——
// 而用户在那期间完全不知道会不会成。
const MaxRenderMs = 30 * 60 * 1000

// PrecheckEnv 预检要的外部事实。
type PrecheckEnv struct {
	// MediaOK 素材还在不在（按工作区）；nil 表示不查。
	MediaOK  func(int64) bool
	FFmpegOK bool
	// GeneratingClips 画面轨上还在片段级生成的段（1 起的段号）。
	GeneratingClips []int
}

// Precheck 在**扣费之前**把跑不起来的情况挑出来。
//
// ⚠️ 顺序是「预检 → 报价 → 扣费 → 入队」。反过来的话，
// 一条缺素材的时间线会先扣钱、再在 Worker 里失败、再退款 ——
// 用户看到的是一次莫名其妙的扣费与退款，而他什么都没得到（NEW-01 的同一条）。
func Precheck(snap RenderSnapshot, env PrecheckEnv) []PrecheckIssue {
	var out []PrecheckIssue
	if !env.FFmpegOK {
		// 本机没有 ffmpeg 时**明说**：不说的话任务会排队然后失败，
		// 而错误信息里只有一句 "exec: not found"。
		out = append(out, PrecheckIssue{PrecheckNoFFmpeg,
			"服务端没有可用的 ffmpeg，无法渲染", 0})
	}
	if len(snap.Clips) == 0 {
		out = append(out, PrecheckIssue{PrecheckEmpty,
			"时间线上还没有画面，先装配分镜或导入素材", 0})
	}
	for _, n := range env.GeneratingClips {
		out = append(out, PrecheckIssue{PrecheckClipGenerating,
			fmt.Sprintf("第 %d 段还在重新生成，等它完成或撤销后再导出", n), n})
	}
	for i, c := range snap.Clips {
		if env.MediaOK != nil && !env.MediaOK(c.MediaID) {
			// ⚠️ 素材在排队期间被删是完全可能的。明确指出是第几段 ——
			// 只说"有素材不存在"的话，几十段的时间线上用户找不到是哪一段。
			out = append(out, PrecheckIssue{PrecheckMediaMissing,
				fmt.Sprintf("第 %d 段的素材已不存在", i+1), i + 1})
		}
	}
	for ti, t := range snap.AudioTracks {
		for i, c := range t.Clips {
			if env.MediaOK != nil && !env.MediaOK(c.MediaID) {
				out = append(out, PrecheckIssue{PrecheckMediaMissing,
					fmt.Sprintf("音频轨 %d 第 %d 段的素材已不存在", ti+1, i+1), 0})
			}
		}
	}
	if total := SnapDuration(snap); total > MaxRenderMs {
		out = append(out, PrecheckIssue{PrecheckTooLong,
			fmt.Sprintf("时间线 %d 分钟，超过单次渲染上限 %d 分钟",
				total/60000, MaxRenderMs/60000), 0})
	}
	return out
}

// SnapDuration 快照的成片时长（算上转场重叠）。与时间线的 EffectiveDuration 同一个实现。
func SnapDuration(snap RenderSnapshot) int {
	_, end := placeVideo(snapSeams(snap.Clips))
	if end < 0 {
		return 0
	}
	return end
}

// snapSeams 快照里画面各段的成片长度与重叠（落点算法见 layout.go 的 placeVideo）。
//
// 时长取快照里存下的 DurationMs（受理时按变速算好的），不现场重算：快照是冻结的。
func snapSeams(clips []SnapClip) []seam {
	out := make([]seam, len(clips))
	for i, c := range clips {
		out[i] = seam{dur: c.DurationMs}
		if i > 0 && c.Transition.IsActive() {
			out[i].overlap = c.Transition.DurationMs
		}
	}
	return out
}

// ---------------------------------------------------------------- 命令

// RenderPlan 是拼好的 ffmpeg 调用：片段多时先分块渲成中间文件（Chunks，逐个跑），再由 Args 收尾。
type RenderPlan struct {
	Chunks []ChunkPass
	// Args 收尾那一遍（不含 "ffmpeg" 本身）：画面（或拼接分块）→ 文字与字幕叠加流 → 水印 → 映射预渲的音频。
	Args []string
	// ConcatList 分块时要先写出的 concat 脚本（路径与内容）。
	ConcatList     string
	ConcatListBody string
}

// ChunkPass 一块：Args 渲出 Output（只有画面）。
type ChunkPass struct {
	Args       []string
	Output     string
	DurationMs int
}

// RenderIO 渲染要用到的本机文件。
type RenderIO struct {
	// Files 按媒体去重下载的素材（SCB-9：同一份素材剪成 50 段也只下载一次）。
	Files map[int64]MediaFile
	// OverlayList 文字与字幕合成的 concat 脚本（SCT-14）；空 = 没有文字与字幕。
	OverlayList string
	// WatermarkPath 水印图；空 = 不加。
	WatermarkPath string
	// AudioPath 预渲好的成片音频（SCT-12）；空 = 成片没有声音。
	AudioPath string
	// WorkDir 分块的中间文件放这里（与其余临时文件同一个目录，整体删）。
	WorkDir string
	OutPath string
}

// ChunkClips 超过这么多段就分块渲染：每段一个 `-i`，几百个解码器同时开着会把 Worker 内存撑爆。
const ChunkClips = 60

// ChunkCRF 中间文件的编码质量：收尾那一遍还要再编码一次，中间文件给得高一些，免得两代压缩糊掉。
const ChunkCRF = 16

// BuildRenderPlan 把快照拼成 ffmpeg 命令（SCD-27）。
//
// ⚠️ 用 `-filter_complex` 而不是把素材直接交给 concat 分离器：素材来自不同模型、不同分辨率，
// 分离器要求编码参数完全一致，用它的话要么报错，要么把第一段的参数硬套到后面所有段上（画面被拉扁）。
// 分块之后的中间文件是我们自己按同一套参数编出来的，那时才用分离器拼接。
func BuildRenderPlan(snap RenderSnapshot, io RenderIO) (RenderPlan, error) {
	if len(snap.Clips) == 0 {
		return RenderPlan{}, fmt.Errorf("时间线上没有画面")
	}
	for i, c := range snap.Clips {
		if f, ok := io.Files[c.MediaID]; !ok || f.Path == "" {
			return RenderPlan{}, fmt.Errorf("第 %d 段的素材没有取到本地", i+1)
		}
	}
	plan := RenderPlan{}
	chunks := ChunkBounds(snap.Clips, ChunkClips)
	var head []string
	var filters []string
	var base string
	next := 0 // 下一个输入的下标
	if len(chunks) == 1 {
		head, filters, base = videoGraph(snap, io.Files, snap.Clips, 0)
		next = len(snap.Clips)
	} else {
		var list strings.Builder
		list.WriteString("ffconcat version 1.0\n")
		for k, b := range chunks {
			clips := snap.Clips[b[0]:b[1]]
			in, fs, label := videoGraph(snap, io.Files, clips, 0)
			out := filepath.Join(io.WorkDir, fmt.Sprintf("chunk-%03d.mp4", k+1))
			args := append([]string{"-hide_banner", "-nostdin", "-y"}, in...)
			args = append(args, "-filter_complex", strings.Join(fs, ";"), "-map", label,
				"-c:v", "libx264", "-preset", "veryfast", "-crf", fmt.Sprint(ChunkCRF), "-pix_fmt", "yuv420p",
				"-r", fmt.Sprint(snap.FPS), "-an", out)
			_, dur := placeVideo(snapSeams(clips))
			plan.Chunks = append(plan.Chunks, ChunkPass{Args: args, Output: out, DurationMs: dur})
			fmt.Fprintf(&list, "file '%s'\n", escapeConcatPath(out))
		}
		plan.ConcatList = filepath.Join(io.WorkDir, "chunks.txt")
		plan.ConcatListBody = list.String()
		head = []string{"-f", "concat", "-safe", "0", "-i", plan.ConcatList}
		base = "[0:v]"
		next = 1
	}

	args := append([]string{"-hide_banner", "-nostdin", "-y"}, head...)
	lastV := base
	// 文字与字幕（SCT-14）：一路透明 PNG 流，只叠一次。⚠️ 在水印之前：水印是标识，压在内容上面。
	if io.OverlayList != "" {
		args = append(args, "-f", "concat", "-safe", "0", "-i", io.OverlayList)
		filters = append(filters,
			fmt.Sprintf("[%d:v]format=rgba,fps=%d[ovl]", next, snap.FPS),
			// ⚠️ eof_action=pass：叠加流比画面短一帧时（时长取整）画面照常继续，不重复最后一张字幕。
			fmt.Sprintf("%s[ovl]overlay=0:0:eof_action=pass:format=auto,format=yuv420p[vtxt]", lastV))
		lastV = "[vtxt]"
		next++
	}
	// 水印叠在**字幕之后**：字幕是内容、水印是标识，标识压在内容上是对的（反过来字幕会把水印挡掉一半）。
	if snap.Watermark != nil && io.WatermarkPath != "" {
		args = append(args, "-i", io.WatermarkPath)
		filters = append(filters, watermarkFilters(*snap.Watermark, next, lastV, "[vwm]")...)
		lastV = "[vwm]"
		next++
	}
	audioIdx := -1
	if io.AudioPath != "" {
		args = append(args, "-i", io.AudioPath)
		audioIdx = next
	}
	if len(filters) > 0 {
		args = append(args, "-filter_complex", strings.Join(filters, ";"))
	} else {
		// 分块拼接、又没有叠加与水印时没有滤镜图：这时只能按流说明符映射（`0:v`），
		// 带方括号的标签只认滤镜图的输出，ffmpeg 报「Output with label '0:v' does not exist」。
		lastV = strings.Trim(lastV, "[]")
	}
	args = append(args, "-map", lastV)
	if audioIdx >= 0 {
		args = append(args, "-map", fmt.Sprintf("%d:a", audioIdx))
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-r", fmt.Sprint(snap.FPS),
		// ⚠️ `+faststart` 让 moov 原子挪到文件头：不加的话浏览器要把整个
		// 文件下完才能起播，用户点下载预览时以为卡住了。
		"-movflags", "+faststart",
	)
	if audioIdx >= 0 {
		args = append(args, "-c:a", "aac", "-b:a", "192k")
	} else {
		// 没有声音时显式说明，否则 ffmpeg 会给成片挑一路它自己找到的音频。
		args = append(args, "-an")
	}
	plan.Args = append(args, io.OutPath)
	return plan, nil
}

// ChunkBounds 把画面轨切成块：每块至少 limit 段时在下一个**硬切**处断开（分块边界不落在转场上 ——
// 转场要两段同时在一个滤镜图里才能叠化）。返回每块的 [起, 止) 下标；不超过 limit 段时只有一块。
func ChunkBounds(clips []SnapClip, limit int) [][2]int {
	if limit <= 0 || len(clips) <= limit {
		return [][2]int{{0, len(clips)}}
	}
	var out [][2]int
	start := 0
	for i := 1; i < len(clips); i++ {
		if i-start >= limit && !clips[i].Transition.IsActive() {
			out = append(out, [2]int{start, i})
			start = i
		}
	}
	return append(out, [2]int{start, len(clips)})
}

// videoGraph 一组画面段的输入与滤镜图（第一个输入的下标为 first），返回最后的画面标签。
// 段在这一组里的成片起点从 0 算（分块时每块各自从 0 起）。
func videoGraph(snap RenderSnapshot, files map[int64]MediaFile, clips []SnapClip, first int) (
	inputs, filters []string, last string) {
	starts, _ := placeVideo(snapSeams(clips))
	for i, c := range clips {
		inputs = append(inputs, clipInput(c, files[c.MediaID].Path, snap.FPS)...)
		out := fmt.Sprintf("[v%d]", i)
		// ⚠️ 这一组的第一段带着生效的转入 = 成片的第一段「从黑场淡入」：分块只在硬切处断开，
		// 后面各块的第一段不可能带转入（SCT-21 对照预览时发现：之前第一段的转入被整个忽略，预览淡入、成片硬切）。
		edge := chainEdge{first: i == 0, last: i == len(clips)-1}
		chain := clipChain(snap, c, edge)
		if c.Transform != nil {
			// 变换要叠到同尺寸黑底上：底的时长就是这一段的成片长度（图片、视频一样准）。
			filters = append(filters,
				fmt.Sprintf("color=c=black:s=%dx%d:r=%d:d=%s[bg%d]", snap.Width, snap.Height, snap.FPS,
					sec(c.PlayLenOf()), i),
				fmt.Sprintf("[%d:v]%s[fg%d]", first+i, strings.Join(chain, ","), i),
				fmt.Sprintf("[bg%d][fg%d]%s%s", i, i, strings.Join(placeChain(snap, c, edge), ","), out))
		} else {
			filters = append(filters, fmt.Sprintf("[%d:v]%s%s", first+i, strings.Join(chain, ","), out))
		}
		if i == 0 {
			last = out
			continue
		}
		next := fmt.Sprintf("[vx%d]", i)
		if c.Transition.IsActive() {
			// ⚠️ `offset` 是**转场开始的时刻**，也就是这一段的成片起点（上一段的成片结束减去转场时长，
			// 前面每一处转场的重叠都已扣掉）—— 算错的话转场要么切在画面中间、要么整段被吃掉，
			// 而两种都"渲染成功"。与时间线的落点同一个实现（placeVideo，SCK-15）。
			filters = append(filters, fmt.Sprintf("%s%sxfade=transition=%s:duration=%s:offset=%s%s",
				last, out, xfadeKind(c.Transition.Kind), sec(c.Transition.DurationMs), sec(max(starts[i], 0)), next))
		} else {
			filters = append(filters, fmt.Sprintf("%s%sconcat=n=2:v=1:a=0%s", last, out, next))
		}
		last = next
	}
	return inputs, filters, last
}

// clipInput 一段的输入参数：**输入级裁剪**，只解码这一段（`-ss/-t` 放在 `-i` 前面）。
//
// ⚠️ 图片要 `-loop 1 -framerate {fps} -t {成片长度}`：一张图只有一帧，不循环的话 trim 之后这一段约等于 1 帧，
// 图片 3 秒 + 视频 2 秒渲出来 2.03 秒，而任务是「成功」的（SCB-1）。
func clipInput(c SnapClip, path string, fps int) []string {
	if c.IsImage() {
		return []string{"-loop", "1", "-framerate", fmt.Sprint(fps), "-t", sec(c.PlayLenOf()), "-i", path}
	}
	return []string{"-ss", sec(c.SourceInMs), "-t", sec(c.SourceOutMs - c.SourceInMs), "-i", path}
}

// clipChain 一段自己的滤镜链（不含叠到黑底那一步）。
// chainEdge 一段在这一组里是不是第一段 / 最后一段（决定从黑场淡入、淡出到黑场）。
type chainEdge struct{ first, last bool }

func clipChain(snap RenderSnapshot, c SnapClip, edge chainEdge) []string {
	chain := []string{"setpts=PTS-STARTPTS"}
	if !c.IsImage() && c.Speed > 0 && c.Speed != 100 {
		// ⚠️ 变速用 setpts 乘**倒数**：2 倍速（speed=200）是把时间压缩一半，
		// 所以 PTS 要乘 0.5。乘反了片子会变成慢一倍，而它"渲染成功"。
		chain = append(chain, fmt.Sprintf("setpts=%s*PTS", ratio(100, c.Speed)))
	}
	if t := c.Transform; t != nil {
		if t.Crop != (Crop{}) {
			// 裁切按素材本身的宽高（剪辑台的裁切框也是），在缩放之前做。
			cw := 1 - (t.Crop.Left+t.Crop.Right)/100
			ch := 1 - (t.Crop.Top+t.Crop.Bottom)/100
			chain = append(chain, fmt.Sprintf("crop=w=iw*%s:h=ih*%s:x=iw*%s:y=ih*%s",
				num(cw), num(ch), num(t.Crop.Left/100), num(t.Crop.Top/100)))
		}
		// 帧率在叠到黑底**之前**统一（黑底本来就按画布帧率生成，overlay 跟着主输入走）。
		// ⚠️ 不能放到 overlay 之后：素材帧率与画布不同时（30fps 的素材、25fps 的画布 —— 生成出来的视频十有八九如此），
		//    ffmpeg 8 在「overlay → fps」这一串上空转、永不结束，渲染一直卡到死线（SCT-44 量产物时撞上：
		//    原来的单测素材恰好是 25fps，所以一直是绿的）。
		chain = append(chain, fmt.Sprintf("fps=%d", snap.FPS))
		// 等比适配「画布 × 缩放」：裁切之后的画面整体放进这个框里（不拉伸）。
		bw, bh := even(float64(snap.Width)*t.Scale), even(float64(snap.Height)*t.Scale)
		chain = append(chain, fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", bw, bh), "format=rgba")
		if t.Rotation != 0 {
			// ⚠️ c=none：旋转后露出的角要透明（叠到黑底上就是黑），不能是旋转滤镜默认的黑色不透明块 ——
			// 那样会在别的段的画面上压出一块黑。输出尺寸放大到能装下整张旋转后的图，否则四角被裁掉。
			a := num(t.Rotation * math.Pi / 180)
			chain = append(chain, fmt.Sprintf("rotate=a=%s:c=none:ow=rotw(%s):oh=roth(%s)", a, a, a))
		}
		return chain
	}
	// ⚠️ 每一段都要 `scale` + `pad` 到画布：素材分辨率五花八门，
	// 不统一的话 xfade/concat 直接报错，或者把后面的段按第一段拉伸。
	// `force_original_aspect_ratio=decrease` + `pad` ＝ 等比缩放后补黑边，
	// **不是拉伸** —— 拉伸出来的人物会变胖，而用户看不出是我们干的。
	chain = append(chain,
		fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", snap.Width, snap.Height),
		fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black", snap.Width, snap.Height))
	return append(chain, tailChain(snap, c, edge, true)...)
}

// placeChain 变换过的段：叠到黑底的 (x, y)，再统一参数。
func placeChain(snap RenderSnapshot, c SnapClip, edge chainEdge) []string {
	t := c.Transform
	// 中心相对画面中心偏移 (X%, Y%)：x 向右、y 向下为正（与剪辑台拖动的方向一致）。
	x := fmt.Sprintf("(W-w)/2+W*%s", num(t.X/100))
	y := fmt.Sprintf("(H-h)/2+H*%s", num(t.Y/100))
	// ⚠️ 黑底是主输入：成片长度跟着黑底走（= 这一段的成片长度）；素材那一路短一帧时重复最后一帧，不会提前结束。
	out := []string{fmt.Sprintf("overlay=x=%s:y=%s:eof_action=repeat:format=auto", x, y)}
	return append(out, tailChain(snap, c, edge, false)...)
}

// tailChain 统一像素宽高比、帧率与像素格式；第一段带「从黑场淡入」，最后一段带「淡出到黑场」。
// withFPS=false 用于叠到黑底之后的那一串：帧率已在叠之前统一过（见 clipChain 里变换那一支）。
func tailChain(snap RenderSnapshot, c SnapClip, edge chainEdge, withFPS bool) []string {
	out := []string{
		// ⚠️ `setsar=1` 不能省：不同素材的像素宽高比不一样，
		// 不统一的话 xfade 会报 "Input link parameters do not match"。
		"setsar=1",
	}
	if withFPS {
		out = append(out, fmt.Sprintf("fps=%d", snap.FPS))
	}
	// ⚠️ 统一像素格式：xfade 要求两路输入格式一致。
	out = append(out, "format=yuv420p")
	// 从黑场淡入（契约：第一段的 transitionIn ＝ 从黑场淡入）。其它转场类型在第一段上一律按淡入处理 ——
	// 第一段没有上一段可以叠化 / 滑动 / 擦除。
	if edge.first && c.Transition.IsActive() {
		out = append(out, fmt.Sprintf("fade=t=in:st=0:d=%s", sec(min(c.Transition.DurationMs, c.PlayLenOf()))))
	}
	// 淡出到黑场（SC-F-44）只认最后一段（SCB-7 的转出部分：以前存了不渲染）。中间段的转出忽略：
	// 接缝的转场只记在后一段的转入上。
	if edge.last && c.TransitionOut != nil && c.TransitionOut.IsActive() {
		d := min(c.TransitionOut.DurationMs, c.PlayLenOf())
		out = append(out, fmt.Sprintf("fade=t=out:st=%s:d=%s", sec(c.PlayLenOf()-d), sec(d)))
	}
	// ⚠️ 统一时间基：concat 的输出是 1/1000000，单段是 1/帧率。硬切之后紧跟一个转场时，
	// xfade 的两路时间基对不上，报「First input link main timebase do not match」整次渲染失败 ——
	// 旧实现就有这个坑，只是没有人在一个硬切后面接过转场（SCT-11 分块测试里第一次撞上）。
	return append(out, "settb=AVTB")
}

// even 取不小于 2 的偶数（yuv420p 要求宽高为偶数；缩放框是奇数的话编码器直接报错）。
func even(v float64) int {
	n := int(math.Round(v))
	if n < 2 {
		return 2
	}
	return n / 2 * 2
}

// num 滤镜参数里的小数：最多 6 位、去掉末尾的 0（argv 断言稳定，也不写出 1e-05 这种科学计数法）。
func num(v float64) string {
	s := strconv.FormatFloat(v, 'f', 6, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// escapeConcatPath concat 脚本里 `file '...'` 的转义：单引号写成 `'\”`。
func escapeConcatPath(p string) string { return strings.ReplaceAll(p, "'", `'\''`) }

// atempoChain 把变速拆成合法的 atempo 串。
//
// ⚠️ atempo 单次只支持 0.5~2.0。4 倍速直接写 `atempo=4.0` 会报错，
// 而报错信息是 "Value 4.000000 for parameter 'tempo' out of range" ——
// 看着像我们传错了参数，其实是要串联。
func atempoChain(speed int) []string {
	if speed <= 0 || speed == 100 {
		return nil
	}
	f := float64(speed) / 100
	var out []string
	for f > 2.0 {
		out = append(out, "atempo=2.0")
		f /= 2
	}
	for f < 0.5 {
		out = append(out, "atempo=0.5")
		f *= 2
	}
	out = append(out, fmt.Sprintf("atempo=%.4f", f))
	return out
}

// xfadeKind 把我们的转场名翻成 ffmpeg 的 xfade transition 名。
//
// ⚠️ 名字对不上时回落到 `fade` 而不是报错：转场是锦上添花，
// 为一个名字让整次渲染失败不值 —— 而用户等的是片子。
func xfadeKind(kind string) string {
	switch kind {
	case TransitionDissolve:
		return "dissolve"
	case TransitionSlideLeft:
		return "slideleft"
	case TransitionWipe:
		return "wipeleft"
	}
	return "fade"
}

func sec(ms int) string { return fmt.Sprintf("%.3f", float64(ms)/1000) }

func ratio(a, b int) string {
	if b == 0 {
		return "1.0000"
	}
	return fmt.Sprintf("%.4f", float64(a)/float64(b))
}

// watermarkFilters 拼水印的滤镜链。
//
// ⚠️ 三步都不能省：
//  1. `scale` 按**画面宽度的百分比**缩放（`-1` 保持比例，
//     写死像素的话换画幅就不对了）；
//  2. `format=rgba` + `colorchannelmixer=aa=` 调不透明度 ——
//     不转 rgba 的话 alpha 通道根本不存在，调了等于没调；
//  3. `overlay` 按位置摆放，离边留出安全距离。
//
// ⚠️ `overlay` 的 `shortest=1` **不能加**：水印是一张静态图，
// 加了之后成片长度会被水印那一"帧"截断 —— 出来一个 0 秒的文件，
// 而任务是"成功"的。
func watermarkFilters(wm Watermark, inputIdx int, videoLabel, outLabel string) []string {
	scaled := "[wms]"
	alpha := float64(wm.Opacity) / 100
	if alpha <= 0 || alpha > 1 {
		alpha = 1
	}
	if wm.PixelWidth <= 0 {
		wm.PixelWidth = 160
	}
	return []string{
		// ⚠️ 这里只能按**水印图自身**缩放（`iw*k`），不能按主画面宽度：
		// 单路滤镜链里拿不到主画面的尺寸（那要 `scale2ref`，
		// 而它的两路输入顺序极易搞反）。所以缩放比由调用方按画幅算好传进来。
		fmt.Sprintf("[%d:v]scale=%d:-1,format=rgba,colorchannelmixer=aa=%.3f%s",
			inputIdx, wm.PixelWidth, alpha, scaled),
		fmt.Sprintf("%s%soverlay=%s%s", videoLabel, scaled,
			overlayPos(wm.Position), outLabel),
	}
}

// overlayPos 把位置翻成 overlay 的 x:y 表达式。
//
// ⚠️ 用**表达式**而不是算好的数字：overlay 的 `W/H` 是主画面尺寸、
// `w/h` 是水印尺寸，而水印缩放后的尺寸在拼命令时还不知道
// （它按主画面宽度的百分比算，取决于素材）。用表达式让 ffmpeg 自己算。
func overlayPos(pos string) string {
	// ⚠️ `overlay` 的参数用 `:` 分隔，而表达式里的 `,` 要转义成 `\,`——
	// 不转的话 ffmpeg 把 min 的两个参数当成两个 overlay 选项。
	m := fmt.Sprintf(`(min(W\,H)*%d/100)`, WatermarkMargin)
	switch pos {
	case WatermarkTopLeft:
		return m + ":" + m
	case WatermarkTopRight:
		return "W-w-" + m + ":" + m
	case WatermarkBottomLeft:
		return m + ":H-h-" + m
	}
	// 默认右下角：那是最不挡内容的位置。
	return "W-w-" + m + ":H-h-" + m
}

// ---------------------------------------------------------------- 导出规格（SCT-15）

// 导出档位（SCK-11：只做 720p / 1080p）与帧率。
var (
	ExportResolutions = map[string]int{"720p": 720, "1080p": 1080}
	ExportFPS         = map[int]bool{24: true, 25: true, 30: true}
)

// ExportSize 按画幅换算导出尺寸（dev/01 R-4 第 2 条，前端 smartcutRender 同一个算法）：取该画幅 1080p 档的默认尺寸，
// 按短边缩到 720 / 1080，宽高取偶数。`custom` 画幅按时间线自己的尺寸缩；resolution 为空时原样返回。
// 例：21:9 的 720p 是 1706×720，9:16 的 720p 是 720×1280。
func ExportSize(aspect string, width, height int, resolution string) (int, int) {
	short, ok := ExportResolutions[resolution]
	if !ok {
		return width, height
	}
	pw, ph, preset := DefaultSize(aspect)
	if !preset {
		pw, ph = width, height
	}
	if pw <= 0 || ph <= 0 {
		return width, height
	}
	k := float64(short) / float64(min(pw, ph))
	return evenDown(float64(pw) * k), evenDown(float64(ph) * k)
}

// evenDown 向下取偶数（1706.67 → 1706，与前端的取法一致）。
func evenDown(v float64) int { return int(v) / 2 * 2 }
