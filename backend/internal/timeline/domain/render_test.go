package domain

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func snapClip(media int64, dur int) SnapClip {
	return SnapClip{MediaID: media, Kind: "video", SourceInMs: 0, SourceOutMs: dur,
		DurationMs: dur, Speed: 100, Volume: 100}
}

func baseSnap(clips ...SnapClip) RenderSnapshot {
	return RenderSnapshot{Schema: SnapshotSchema, Width: 1920, Height: 1080, FPS: 30, Clips: clips, VideoVolume: 100}
}

// filesFor 每个媒体一个本地文件（与 runner 按媒体去重下载的形状一样）。
func filesFor(snap RenderSnapshot) map[int64]MediaFile {
	out := map[int64]MediaFile{}
	for _, c := range snap.Clips {
		out[c.MediaID] = MediaFile{Path: fmt.Sprintf("/tmp/m%d.mp4", c.MediaID), HasVideo: true, HasAudio: true}
	}
	return out
}

func planOf(t *testing.T, snap RenderSnapshot, io RenderIO) RenderPlan {
	t.Helper()
	if io.Files == nil {
		io.Files = filesFor(snap)
	}
	if io.OutPath == "" {
		io.OutPath = "/tmp/out.mp4"
	}
	if io.WorkDir == "" {
		io.WorkDir = "/tmp/work"
	}
	plan, err := BuildRenderPlan(snap, io)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func argsOf(t *testing.T, snap RenderSnapshot) string {
	t.Helper()
	return strings.Join(planOf(t, snap, RenderIO{}).Args, " ")
}

// ------------------------------------------------------------ 预检

// ⚠️ 顺序是「预检 → 报价 → 扣费 → 入队」：反过来的话一条缺素材的时间线
// 会先扣钱、再在 Worker 里失败、再退款 —— 用户看到一次莫名其妙的扣费与退款，
// 而他什么都没得到。
func TestPrecheck_扣费前挑出跑不起来的情况(t *testing.T) {
	ok := PrecheckEnv{MediaOK: func(int64) bool { return true }, FFmpegOK: true}
	if issues := Precheck(baseSnap(), ok); len(issues) != 1 || issues[0].Code != PrecheckEmpty {
		t.Fatalf("空时间线应被拦：%+v", issues)
	}
	// 没有 ffmpeg 要明说 —— 不说的话任务排队后失败，错误里只有 "exec: not found"。
	if issues := Precheck(baseSnap(snapClip(1, 1000)), PrecheckEnv{}); len(issues) != 1 || issues[0].Code != PrecheckNoFFmpeg {
		t.Fatalf("缺 ffmpeg 应被拦：%+v", issues)
	}
	if got := Precheck(baseSnap(snapClip(1, 1000)), ok); len(got) != 0 {
		t.Fatalf("正常时间线不该被拦：%+v", got)
	}
	// 字幕不再需要 libass（SCT-14）：带字幕的时间线照样放行。
	sub := baseSnap(snapClip(1, 1000))
	sub.SubtitleTracks = []SnapSubtitleTrack{{Order: 1, Items: []SnapSubItem{{StartMs: 0, EndMs: 500, Text: "喂"}}}}
	if got := Precheck(sub, ok); len(got) != 0 {
		t.Fatalf("带字幕不该被拦：%+v", got)
	}
}

// ⚠️ 只说"有素材不存在"的话，几十段的时间线上用户找不到是哪一段。
func TestPrecheck_缺素材与还在生成要指出是第几段(t *testing.T) {
	snap := baseSnap(snapClip(1, 1000), snapClip(2, 1000), snapClip(3, 1000))
	snap.AudioTracks = []SnapAudioTrack{{Role: RoleMusic, Volume: 100, Clips: []SnapClip{snapClip(4, 1000)}}}
	issues := Precheck(snap, PrecheckEnv{FFmpegOK: true, MediaOK: func(id int64) bool { return id != 2 && id != 4 },
		GeneratingClips: []int{3}})
	if len(issues) != 3 {
		t.Fatalf("应报三条：%+v", issues)
	}
	if issues[0].Code != PrecheckClipGenerating || issues[0].ClipIdx != 3 || !strings.Contains(issues[0].Detail, "第 3 段") {
		t.Fatalf("还在生成要指出第 3 段：%+v", issues[0])
	}
	if issues[1].ClipIdx != 2 || !strings.Contains(issues[1].Detail, "第 2 段") {
		t.Fatalf("应指出是第 2 段：%+v", issues[1])
	}
	if issues[2].Code != PrecheckMediaMissing || !strings.Contains(issues[2].Detail, "音频轨 1 第 1 段") {
		t.Fatalf("音频轨也要查：%+v", issues[2])
	}
}

func TestPrecheck_超长被拦(t *testing.T) {
	issues := Precheck(baseSnap(snapClip(1, MaxRenderMs+1000)), PrecheckEnv{FFmpegOK: true})
	if len(issues) != 1 || issues[0].Code != PrecheckTooLong {
		t.Fatalf("超长应被拦：%+v", issues)
	}
}

// ------------------------------------------------------------ 画面

// ⚠️ 每一段都要 scale + pad 到画布：素材分辨率五花八门，不统一的话
// xfade 直接报错，或者把后面的段按第一段拉伸（人物会变胖，而用户看不出是我们干的）。
// 每段输入级裁剪（-ss/-t 在 -i 前），只解码这一段。
func TestBuildRenderPlan_每段输入级裁剪并统一参数(t *testing.T) {
	a := snapClip(1, 4000)
	a.SourceInMs, a.SourceOutMs = 1500, 5500
	got := argsOf(t, baseSnap(a, snapClip(2, 1000)))
	for _, want := range []string{
		"-ss 1.500 -t 4.000 -i /tmp/m1.mp4", "-ss 0.000 -t 1.000 -i /tmp/m2.mp4",
		"[0:v]setpts=PTS-STARTPTS,scale=1920:1080:force_original_aspect_ratio=decrease," +
			"pad=1920:1080:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,fps=30,format=yuv420p,settb=AVTB[v0]",
		"-movflags +faststart", "-an",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "trim=") {
		t.Fatalf("不再在滤镜里 trim（整段解码）：\n%s", got)
	}
}

// 同一份素材剪成好几段：每段一个 -i（各带自己的 -ss/-t），但都指向同一个本地文件（SCB-9）。
func TestBuildRenderPlan_同一素材多段共用一个文件(t *testing.T) {
	var clips []SnapClip
	for i := 0; i < 5; i++ {
		c := snapClip(7, 1000)
		c.SourceInMs, c.SourceOutMs = i*1000, (i+1)*1000
		clips = append(clips, c)
	}
	got := argsOf(t, baseSnap(clips...))
	if strings.Count(got, "-i /tmp/m7.mp4") != 5 || !strings.Contains(got, "-ss 4.000 -t 1.000 -i /tmp/m7.mp4") {
		t.Fatalf("五段都引用同一个文件：\n%s", got)
	}
	if _, err := BuildRenderPlan(baseSnap(snapClip(9, 1000)), RenderIO{Files: map[int64]MediaFile{}}); err == nil {
		t.Fatal("素材没取到本地要报错")
	}
}

// ⚠️ 变速用 setpts 乘**倒数**：2 倍速（speed=200）的 PTS 要乘 0.5。乘反了片子变成慢一倍，而它"渲染成功"。
func TestBuildRenderPlan_变速乘倒数(t *testing.T) {
	c := snapClip(1, 1000)
	c.Speed, c.SourceOutMs = 200, 2000
	if got := argsOf(t, baseSnap(c)); !strings.Contains(got, "setpts=0.5000*PTS") || !strings.Contains(got, "-t 2.000") {
		t.Fatalf("2 倍速：输入取 2 秒素材、PTS 乘 0.5：\n%s", got)
	}
}

// SCB-1：图片要 -loop 1 -framerate -t（成片长度），不变速。
func TestBuildRenderPlan_图片循环成持续画面(t *testing.T) {
	img := SnapClip{MediaID: 3, Kind: "image", SourceOutMs: 3000, DurationMs: 3000, Speed: 100, Volume: 100}
	got := argsOf(t, baseSnap(img, snapClip(4, 2000)))
	if !strings.Contains(got, "-loop 1 -framerate 30 -t 3.000 -i /tmp/m3.mp4") {
		t.Fatalf("图片段要循环 3 秒：\n%s", got)
	}
	img.Speed, img.DurationMs = 200, 1500 // 快照里的成片长度在受理时按变速算好
	if got := argsOf(t, baseSnap(img)); !strings.Contains(got, "-t 1.500") || strings.Contains(got, "*PTS") {
		t.Fatalf("图片按成片长度循环、不做 setpts 变速：\n%s", got)
	}
}

// ⚠️ xfade 的 offset 是这一段的成片起点（前面每一处转场的重叠都已扣掉），与时间线落点同一个实现。
func TestBuildRenderPlan_转场偏移按成片起点(t *testing.T) {
	b, c := snapClip(2, 3000), snapClip(3, 3000)
	b.Transition = Transition{Kind: TransitionFade, DurationMs: 500}
	c.Transition = Transition{Kind: TransitionDissolve, DurationMs: 1000}
	got := argsOf(t, baseSnap(snapClip(1, 4000), b, c))
	if !strings.Contains(got, "[v0][v1]xfade=transition=fade:duration=0.500:offset=3.500[vx1]") ||
		!strings.Contains(got, "[vx1][v2]xfade=transition=dissolve:duration=1.000:offset=5.500[vx2]") {
		t.Fatalf("偏移要累计重叠：\n%s", got)
	}
	if !strings.Contains(argsOf(t, baseSnap(snapClip(1, 1000), snapClip(2, 1000))), "concat=n=2:v=1:a=0") {
		t.Fatal("没有转场时用 concat")
	}
}

func TestXfadeKind_未知类型回落(t *testing.T) {
	if xfadeKind("nope") != "fade" || xfadeKind(TransitionWipe) != "wipeleft" {
		t.Fatal("未知类型回落 fade")
	}
}

// SCB-7（转出）：最后一段淡出到黑场；中间段的转出忽略（接缝只记在后一段的转入上）。
func TestBuildRenderPlan_最后一段淡出到黑场(t *testing.T) {
	a, b := snapClip(1, 4000), snapClip(2, 3000)
	a.TransitionOut = &Transition{Kind: TransitionFade, DurationMs: 800}
	b.TransitionOut = &Transition{Kind: TransitionFade, DurationMs: 1000}
	got := argsOf(t, baseSnap(a, b))
	if strings.Count(got, "fade=t=out") != 1 || !strings.Contains(got, "format=yuv420p,fade=t=out:st=2.000:d=1.000,settb=AVTB[v1]") {
		t.Fatalf("只有最后一段淡出：\n%s", got)
	}
}

// 第一段的转入 = 从黑场淡入（契约 TimelineClip.transitionIn）；后面各段的转入照常是 xfade，不再加淡入。
// 之前第一段的转入被整个忽略：剪辑台预览淡入、成片却是硬切（SCT-21 对照预览时发现）。
func TestBuildRenderPlan_第一段从黑场淡入(t *testing.T) {
	a, b := snapClip(1, 4000), snapClip(2, 3000)
	a.Transition = Transition{Kind: TransitionDissolve, DurationMs: 600}
	b.Transition = Transition{Kind: TransitionFade, DurationMs: 500}
	got := argsOf(t, baseSnap(a, b))
	if strings.Count(got, "fade=t=in") != 1 || !strings.Contains(got, "format=yuv420p,fade=t=in:st=0:d=0.600,settb=AVTB[v0]") {
		t.Fatalf("只有第一段淡入：\n%s", got)
	}
	// 淡入不能长过这一段本身
	short := snapClip(1, 300)
	short.Transition = Transition{Kind: TransitionFade, DurationMs: 1000}
	if got := argsOf(t, baseSnap(short)); !strings.Contains(got, "fade=t=in:st=0:d=0.300") {
		t.Fatalf("淡入夹到片段长度：\n%s", got)
	}
	if strings.Contains(argsOf(t, baseSnap(snapClip(1, 1000))), "fade=t=in") {
		t.Fatal("没有转入就不淡入")
	}
}

// 画面变换（SC-F-43）：裁切 → 适配「画布 × 缩放」→ 旋转（透明）→ 叠到黑底 (x, y)。没有变换的段不走这条。
func TestBuildRenderPlan_画面变换(t *testing.T) {
	c := snapClip(1, 2000)
	c.Transform = &Transform{X: -25, Y: -25, Scale: 0.5, Rotation: 90, Crop: Crop{Left: 10, Right: 10}}
	got := argsOf(t, baseSnap(c, snapClip(2, 1000)))
	for _, want := range []string{
		"color=c=black:s=1920x1080:r=30:d=2.000[bg0]",
		"[0:v]setpts=PTS-STARTPTS,crop=w=iw*0.8:h=ih*1:x=iw*0.1:y=ih*0,fps=30,scale=960:540:force_original_aspect_ratio=decrease," +
			"format=rgba,rotate=a=1.570796:c=none:ow=rotw(1.570796):oh=roth(1.570796)[fg0]",
		"[bg0][fg0]overlay=x=(W-w)/2+W*-0.25:y=(H-h)/2+H*-0.25:eof_action=repeat:format=auto,setsar=1,format=yuv420p,settb=AVTB[v0]",
		"[1:v]setpts=PTS-STARTPTS,scale=1920:1080:force_original_aspect_ratio=decrease,pad=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q：\n%s", want, got)
		}
	}
}

func TestParseTransform(t *testing.T) {
	if ParseTransform(nil) != nil || ParseTransform(json.RawMessage(`{"scale":1,"x":0}`)) != nil ||
		ParseTransform(json.RawMessage(`"bad"`)) != nil {
		t.Fatal("缺省、坏值都当没有变换")
	}
	tr := ParseTransform(json.RawMessage(`{"x":150,"scale":9,"rotation":-200,"crop":{"left":60},"future":1}`))
	if tr == nil || tr.X != 100 || tr.Scale != 5 || tr.Rotation != -180 || tr.Crop.Left != 45 {
		t.Fatalf("越界夹回：%+v", tr)
	}
}

// ------------------------------------------------------------ 分块

func TestChunkBounds_在硬切处断开(t *testing.T) {
	clips := make([]SnapClip, 70)
	for i := range clips {
		clips[i] = snapClip(int64(i+1), 500)
	}
	if b := ChunkBounds(clips, 60); len(b) != 2 || b[0] != [2]int{0, 60} || b[1] != [2]int{60, 70} {
		t.Fatalf("70 段切成 60 + 10：%v", b)
	}
	// 第 61、62 段（下标 60、61）带转场：边界顺延到下一个硬切。
	clips[60].Transition = Transition{Kind: TransitionFade, DurationMs: 200}
	clips[61].Transition = Transition{Kind: TransitionFade, DurationMs: 200}
	if b := ChunkBounds(clips, 60); b[0] != [2]int{0, 62} {
		t.Fatalf("边界不落在转场上：%v", b)
	}
	if b := ChunkBounds(clips[:60], 60); len(b) != 1 {
		t.Fatalf("不超过 60 段只有一块：%v", b)
	}
}

func TestBuildRenderPlan_分块渲染再拼接(t *testing.T) {
	clips := make([]SnapClip, 70)
	for i := range clips {
		clips[i] = snapClip(int64(i%3+1), 500)
	}
	plan := planOf(t, baseSnap(clips...), RenderIO{AudioPath: "/tmp/work/mix.wav"})
	if len(plan.Chunks) != 2 || plan.Chunks[0].DurationMs != 30000 || plan.Chunks[1].DurationMs != 5000 {
		t.Fatalf("两块：%+v", plan.Chunks)
	}
	first := strings.Join(plan.Chunks[0].Args, " ")
	if strings.Count(first, " -i ") != 60 || !strings.Contains(first, "-crf 16") || !strings.HasSuffix(first, "-an "+filepath.Join("/tmp/work", "chunk-001.mp4")) {
		t.Fatalf("第一块 60 个输入、高质量中间文件、不带声音：\n%s", first)
	}
	if !strings.Contains(plan.ConcatListBody, "file '"+filepath.Join("/tmp/work", "chunk-002.mp4")+"'") || plan.ConcatList != filepath.Join("/tmp/work", "chunks.txt") {
		t.Fatalf("concat 脚本：%s", plan.ConcatListBody)
	}
	final := strings.Join(plan.Args, " ")
	if !strings.HasPrefix(final, "-hide_banner -nostdin -y -f concat -safe 0 -i "+filepath.Join("/tmp/work", "chunks.txt")+" -i /tmp/work/mix.wav") ||
		!strings.Contains(final, "-map 0:v -map 1:a") {
		t.Fatalf("收尾那一遍拼接分块并映射预渲音频：\n%s", final)
	}
}

// ------------------------------------------------------------ 收尾那一遍

// 文字与字幕的叠加流在水印之前；预渲音频直接映射；没有声音时 -an。
func TestBuildRenderPlan_叠加流水印与音频的顺序(t *testing.T) {
	snap := baseSnap(snapClip(1, 1000))
	snap.Watermark = &Watermark{MediaID: 9, Position: WatermarkBottomRight, Opacity: 60, Scale: 12, PixelWidth: 230}
	got := strings.Join(planOf(t, snap, RenderIO{OverlayList: "/tmp/work/overlay.txt", WatermarkPath: "/tmp/logo.png",
		AudioPath: "/tmp/work/mix.wav"}).Args, " ")
	for _, want := range []string{
		"-i /tmp/m1.mp4 -f concat -safe 0 -i /tmp/work/overlay.txt -i /tmp/logo.png -i /tmp/work/mix.wav",
		"[1:v]format=rgba,fps=30[ovl];[v0][ovl]overlay=0:0:eof_action=pass:format=auto,format=yuv420p[vtxt]",
		"[2:v]scale=230:-1", "[vtxt][wms]overlay=", "-map [vwm] -map 3:a", "-c:a aac -b:a 192k",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "subtitles=") || strings.Contains(got, " -an ") {
		t.Fatalf("不再走 libass；有声音时不带 -an：\n%s", got)
	}
	if !strings.Contains(argsOf(t, baseSnap(snapClip(1, 1000))), " -an ") {
		t.Fatal("没有声音时 -an")
	}
}

func TestSnapDuration_算上转场重叠(t *testing.T) {
	b := snapClip(2, 4000)
	b.Transition = Transition{Kind: TransitionFade, DurationMs: 500}
	if got := SnapDuration(baseSnap(snapClip(1, 4000), b)); got != 7500 {
		t.Fatalf("应为 7500，实际 %d", got)
	}
}

// ------------------------------------------------------------ 快照

// 排队中的旧任务在升级之后照样能渲：一组 Audio 变成一条配乐轨（音量、静音、起点保留），字幕变成一条字幕轨。
func TestSnapshot_旧格式升级(t *testing.T) {
	raw := `{"timelineId":1,"timelineVersion":3,"width":1920,"height":1080,"fps":30,
	  "clips":[{"mediaId":5,"sourceInMs":0,"sourceOutMs":2000,"startMs":0,"durationMs":2000,"speed":100,"volume":100,"transition":{}}],
	  "audio":[{"mediaId":6,"sourceInMs":0,"sourceOutMs":3000,"startMs":0,"durationMs":3000,"speed":100,"volume":80,"transition":{}},
	           {"mediaId":7,"sourceInMs":0,"sourceOutMs":1000,"startMs":3000,"durationMs":1000,"speed":100,"volume":100,"transition":{}}],
	  "audioMuted":false,"audioVolume":50,"subtitles":[{"startMs":0,"endMs":900,"text":"你好"}]}`
	var s RenderSnapshot
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if s.Schema != SnapshotSchema || len(s.AudioTracks) != 1 || s.AudioTracks[0].Role != RoleMusic ||
		s.AudioTracks[0].Volume != 50 || len(s.AudioTracks[0].Clips) != 2 || s.AudioTracks[0].Clips[1].StartMs != 3000 ||
		s.AudioTracks[0].Clips[0].Kind != "audio" {
		t.Fatalf("音频轨：%+v", s.AudioTracks)
	}
	if len(s.SubtitleTracks) != 1 || s.SubtitleTracks[0].Items[0].Text != "你好" || s.Clips[0].Kind != "video" ||
		s.VideoVolume != 100 || s.Audio != nil || s.Subtitles != nil {
		t.Fatalf("字幕轨与画面：%+v", s)
	}
	// 新格式往返：画面原声音量 0（静音）保留，不被当成缺省 100。
	n := baseSnap(snapClip(1, 1000))
	n.VideoVolume = 0
	b, _ := json.Marshal(n)
	var back RenderSnapshot
	if err := json.Unmarshal(b, &back); err != nil || back.VideoVolume != 0 || strings.Contains(string(b), `"audio"`) {
		t.Fatalf("新格式往返：%s %+v %v", b, back, err)
	}
}

// ------------------------------------------------------------ 水印（P10-04）

func wmSnap(wm *Watermark) RenderSnapshot {
	snap := baseSnap(snapClip(1, 1000))
	snap.Watermark = wm
	return snap
}

func wmArgs(t *testing.T, snap RenderSnapshot, wmPath string) string {
	t.Helper()
	return strings.Join(planOf(t, snap, RenderIO{WatermarkPath: wmPath}).Args, " ")
}

// ⚠️ `shortest=1` **不能加**：水印是一张静态图，加了之后成片长度
// 会被水印那一"帧"截断 —— 出来一个 0 秒的文件，而任务是"成功"的。
func TestBuildRenderPlan_水印不能截断成片(t *testing.T) {
	got := wmArgs(t, wmSnap(&Watermark{MediaID: 9, Position: WatermarkBottomRight,
		Opacity: 60, Scale: 12, PixelWidth: 230}), "/tmp/logo.png")
	if !strings.Contains(got, "overlay=") {
		t.Fatalf("应叠加水印：\n%s", got)
	}
	if strings.Contains(got, "shortest") {
		t.Fatalf("水印 overlay 不能带 shortest（会把成片截成 0 秒）：\n%s", got)
	}
}

// ⚠️ 不透明度要真的生效：`format=rgba` 漏了的话 colorchannelmixer
// 改不动 alpha —— 水印以 100% 不透明糊在画面上，而命令行看着是对的。
func TestBuildRenderPlan_水印透明度按百分比(t *testing.T) {
	got := wmArgs(t, wmSnap(&Watermark{MediaID: 9, Position: WatermarkTopLeft,
		Opacity: 40, Scale: 10, PixelWidth: 192}), "/tmp/logo.png")
	if !strings.Contains(got, "format=rgba") {
		t.Fatalf("要先转 rgba 才改得动 alpha：\n%s", got)
	}
	if !strings.Contains(got, "colorchannelmixer=aa=0.400") {
		t.Fatalf("40%% 应译成 aa=0.400：\n%s", got)
	}
	// 目标宽度由受理侧按画幅算好传进来。
	if !strings.Contains(got, "scale=192:-1") {
		t.Fatalf("应按传入的像素宽度缩放：\n%s", got)
	}
}

// ⚠️ 四个位置各测一遍：`W-w` 与 `H-h` 写反的话水印会跑到画面外
// （画面上完全看不到），而 ffmpeg 不报错。
func TestOverlayPos_四个角(t *testing.T) {
	cases := map[string][2]bool{
		// [x 是否从右算, y 是否从下算]
		WatermarkTopLeft:     {false, false},
		WatermarkTopRight:    {true, false},
		WatermarkBottomLeft:  {false, true},
		WatermarkBottomRight: {true, true},
	}
	for pos, want := range cases {
		got := overlayPos(pos)
		x, y, ok := strings.Cut(got, ":")
		if !ok {
			t.Fatalf("%s 的表达式应以 : 分隔 x/y：%s", pos, got)
		}
		if strings.HasPrefix(x, "W-w") != want[0] {
			t.Fatalf("%s 的 x 方向不对：%s", pos, got)
		}
		if strings.Contains(y, "H-h") != want[1] {
			t.Fatalf("%s 的 y 方向不对：%s", pos, got)
		}
		// ⚠️ 表达式里的逗号必须转义：不转的话 ffmpeg 会把 min 的
		// 两个参数当成两个 overlay 选项，报 "Option not found"。
		if strings.Contains(got, "min(W,H)") {
			t.Fatalf("%s 的表达式里逗号没转义：%s", pos, got)
		}
		if !strings.Contains(got, `min(W\,H)`) {
			t.Fatalf("%s 应按短边算留边：%s", pos, got)
		}
	}
	// 未知位置回落右下角：那是最不挡内容的位置。
	if overlayPos("center") != overlayPos(WatermarkBottomRight) {
		t.Fatal("未知位置应回落到右下角")
	}
}

// ⚠️ 快照里有水印但**图取不回来**时（被删了/存储抖动），要照常出片、
// 只是不加水印 —— 为一张 logo 让整条渲染链停掉不值。
func TestBuildRenderPlan_水印图取不回来时照常出片(t *testing.T) {
	got := wmArgs(t, wmSnap(&Watermark{MediaID: 9, Position: WatermarkBottomRight,
		Opacity: 60, Scale: 12, PixelWidth: 230}), "")
	if strings.Contains(got, "overlay=") {
		t.Fatalf("没有水印文件时不该叠加：\n%s", got)
	}
	if !strings.Contains(got, "-map") {
		t.Fatalf("仍应正常出片：\n%s", got)
	}
}

// ⚠️ 用户关掉水印（`is_ai_watermark_off`）时快照里就没有 Watermark，
// 滤镜链里一个字都不该有 —— 靠的是受理侧不写进快照，不是 Worker 里再判一次。
func TestBuildRenderPlan_没有水印时不加滤镜(t *testing.T) {
	got := wmArgs(t, wmSnap(nil), "/tmp/logo.png")
	if strings.Contains(got, "overlay=") || strings.Contains(got, "colorchannelmixer") {
		t.Fatalf("快照里没有水印时不该叠加：\n%s", got)
	}
}

func TestValidWatermarkPosition(t *testing.T) {
	for _, ok := range []string{WatermarkTopLeft, WatermarkTopRight,
		WatermarkBottomLeft, WatermarkBottomRight} {
		if !ValidWatermarkPosition(ok) {
			t.Fatalf("%s 应合法", ok)
		}
	}
	for _, bad := range []string{"", "center", "TOP_LEFT", "bottom-right"} {
		if ValidWatermarkPosition(bad) {
			t.Fatalf("%q 不该合法", bad)
		}
	}
}

// ⚠️ atempo 单次只支持 0.5~2.0：4 倍速直接写 atempo=4.0 会报错，要串联。
func TestAtempoChain_超范围要串联(t *testing.T) {
	if got := strings.Join(atempoChain(400), ","); got != "atempo=2.0,atempo=2.0000" {
		t.Fatalf("4 倍速：%s", got)
	}
	if got := strings.Join(atempoChain(25), ","); got != "atempo=0.5,atempo=0.5000" {
		t.Fatalf("0.25 倍速：%s", got)
	}
	if atempoChain(100) != nil {
		t.Fatal("原速不加")
	}
}

func TestExportSize(t *testing.T) {
	cases := []struct {
		aspect       string
		w, h         int
		res          string
		wantW, wantH int
	}{
		{"9:16", 1080, 1920, "720p", 720, 1280},
		{"9:16", 1080, 1920, "1080p", 1080, 1920},
		{"16:9", 1920, 1080, "720p", 1280, 720},
		{"21:9", 2560, 1080, "720p", 1706, 720},
		{"4:3", 1440, 1080, "720p", 960, 720},
		{"custom", 1000, 750, "720p", 960, 720},
		{"16:9", 1920, 1080, "", 1920, 1080},
		{"16:9", 1920, 1080, "4k", 1920, 1080},
	}
	for _, c := range cases {
		if w, h := ExportSize(c.aspect, c.w, c.h, c.res); w != c.wantW || h != c.wantH {
			t.Errorf("%s %s：%dx%d，应为 %dx%d", c.aspect, c.res, w, h, c.wantW, c.wantH)
		}
	}
}

// 有 ffmpeg 但没有任何 H.264 编码器（随包 LGPL 构建 + 硬件与 openh264 都不可用）：受理前拦下并说出路（TT-T-27）。
func TestPrecheck_没有H264编码器(t *testing.T) {
	env := PrecheckEnv{MediaOK: func(int64) bool { return true }, FFmpegOK: true, NoEncoder: true}
	issues := Precheck(baseSnap(snapClip(1, 1000)), env)
	if len(issues) != 1 || issues[0].Code != PrecheckNoEncoder || !strings.Contains(issues[0].Detail, "设置") {
		t.Fatalf("应只报无编码器并给出路：%+v", issues)
	}
	// 连 ffmpeg 都没有时只报那一条（编码器无从谈起）。
	env.FFmpegOK = false
	if issues := Precheck(baseSnap(snapClip(1, 1000)), env); len(issues) != 1 || issues[0].Code != PrecheckNoFFmpeg {
		t.Fatalf("没有 ffmpeg 时只报 ffmpeg：%+v", issues)
	}
}
