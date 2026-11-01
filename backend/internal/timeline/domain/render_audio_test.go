package domain

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 参数与滤镜图（不跑进程）

// audSnap 一段 n 毫秒、没有原声的画面（图片），再加若干条音频轨。
func audSnap(videoMs int, tracks ...SnapAudioTrack) RenderSnapshot {
	return RenderSnapshot{Schema: SnapshotSchema, Width: 1280, Height: 720, FPS: 30, VideoVolume: 100,
		Clips:       []SnapClip{{MediaID: 1, Kind: "image", SourceOutMs: videoMs, DurationMs: videoMs, Speed: 100, Volume: 100}},
		AudioTracks: tracks}
}

func audClip(media int64, inMs, outMs, startMs, speed int) SnapClip {
	c := SnapClip{MediaID: media, Kind: "audio", SourceInMs: inMs, SourceOutMs: outMs, StartMs: startMs,
		Speed: speed, Volume: 100}
	c.DurationMs = c.PlayLenOf()
	return c
}

func audMusic(clips ...SnapClip) SnapAudioTrack {
	return SnapAudioTrack{Role: RoleMusic, Volume: 100, Clips: clips}
}

var audFiles = map[int64]MediaFile{
	1: {Path: "/tmp/still.png", HasVideo: true},
	2: {Path: "/tmp/music.wav", HasAudio: true, AudioChannels: 2},
	3: {Path: "/tmp/voice.wav", HasAudio: true, AudioChannels: 1},
	4: {Path: "/tmp/sfx.wav", HasAudio: true, AudioChannels: 2},
	5: {Path: "/tmp/a.mp4", HasVideo: true, HasAudio: true, AudioChannels: 2},
	6: {Path: "/tmp/b.mp4", HasVideo: true, HasAudio: true, AudioChannels: 2},
	7: {Path: "/tmp/silent.mp4", HasVideo: true},
}

func audPlan(t *testing.T, snap RenderSnapshot, files map[int64]MediaFile) AudioPlan {
	t.Helper()
	p, ok := BuildAudioPlan(snap, files)
	if !ok {
		t.Fatal("应当有声音")
	}
	return p
}

// AC-12：配乐放在 2.5 秒处就从 2.5 秒响起（不再从 0 秒开始首尾相接，SCB-2）；只有一路时不过 amix；
// 补齐到成片总长再截断；输入级裁剪只读用到的那一段。
func TestBuildAudioPlan_按成片起点放置(t *testing.T) {
	p := audPlan(t, audSnap(6000, audMusic(audClip(2, 1000, 4000, 2500, 100))), audFiles)
	in := strings.Join(p.Inputs, " ")
	if in != "-ss 1.000 -t 3.000 -i /tmp/music.wav" {
		t.Fatalf("输入级裁剪：%s", in)
	}
	for _, want := range []string{"[0:a]asetpts=PTS-STARTPTS,aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo",
		"adelay=2500:all=1[s0]", "[s0]apad=whole_dur=6.000,atrim=0:6.000[mix]"} {
		if !strings.Contains(p.Graph, want) {
			t.Errorf("缺少 %q：%s", want, p.Graph)
		}
	}
	if strings.Contains(p.Graph, "amix") || strings.Contains(p.Graph, "concat") || p.TotalMs != 6000 {
		t.Fatalf("单路不过 amix、不再 concat：%s", p.Graph)
	}
}

// 变速用 atempo（2 倍一级，4 倍两级），淡入淡出的起点按变速之后的成片长度算；起点为 0 时不加 adelay。
func TestBuildAudioPlan_变速与淡入淡出(t *testing.T) {
	c := audClip(2, 0, 8000, 0, 200)
	c.FadeInMs, c.FadeOutMs = 500, 1000
	g := audPlan(t, audSnap(6000, audMusic(c)), audFiles).Graph
	for _, want := range []string{"atempo=2.0000", "afade=t=in:st=0.000:d=0.500", "afade=t=out:st=3.000:d=1.000"} {
		if !strings.Contains(g, want) {
			t.Errorf("缺少 %q：%s", want, g)
		}
	}
	if strings.Contains(g, "adelay") {
		t.Fatalf("起点 0 不加 adelay：%s", g)
	}
	g = audPlan(t, audSnap(6000, audMusic(audClip(2, 0, 8000, 0, 400))), audFiles).Graph
	if !strings.Contains(g, "atempo=2.0,atempo=2.0000") {
		t.Fatalf("4 倍速要串两级：%s", g)
	}
	// 有起点又变速：推后放在变速之前，按倍速折算成样本（0.5 倍速推后 1 秒 = 变速前推后 24000 个样本）；
	// 淡入的时刻写成片里的绝对时间。
	c = audClip(2, 0, 2000, 1000, 50)
	c.FadeInMs = 200
	g = audPlan(t, audSnap(6000, audMusic(c)), audFiles).Graph
	if !strings.Contains(g, "adelay=24000S:all=1,atempo=0.5000,afade=t=in:st=1.000:d=0.200") {
		t.Fatalf("推后在变速之前：%s", g)
	}
	// 淡入 + 淡出超过片段本身时夹回（否则 afade 把整段压成静音）。
	c = audClip(2, 0, 2000, 0, 100)
	c.FadeInMs, c.FadeOutMs = 1500, 1500
	g = audPlan(t, audSnap(6000, audMusic(c)), audFiles).Graph
	if !strings.Contains(g, "afade=t=in:st=0.000:d=1.500") || !strings.Contains(g, "afade=t=out:st=1.500:d=0.500") {
		t.Fatalf("淡出夹到剩下的 500ms：%s", g)
	}
}

// 音量 = 片段 × 轨道；静音的轨、音量为 0 的段、成片结束之后才开始的段都不进图；单声道复制成双声道。
func TestBuildAudioPlan_音量静音与声道(t *testing.T) {
	c := audClip(3, 0, 2000, 1000, 100)
	c.Volume = 80
	tr := SnapAudioTrack{Role: RoleVoice, Volume: 50, Clips: []SnapClip{c}}
	g := audPlan(t, audSnap(6000, tr), audFiles).Graph
	if !strings.Contains(g, "volume=0.4000") || !strings.Contains(g, "pan=stereo|c0=c0|c1=c0") {
		t.Fatalf("音量相乘、单声道复制：%s", g)
	}
	muted := tr
	muted.Muted = true
	zero := audClip(2, 0, 2000, 0, 100)
	zero.Volume = 0
	late := audClip(2, 0, 2000, 6000, 100)
	if _, ok := BuildAudioPlan(audSnap(6000, muted, audMusic(zero, late)), audFiles); ok {
		t.Fatal("全被静音 / 落在成片之外时没有声音")
	}
}

// 画面原声按成片起点放，不再 concat（SCB-8）：叠化区间里前一段淡出、后一段淡入；没有音轨的段跳过、不补静音；
// 图片没有原声；画面轨静音时一段都不进。
func TestBuildAudioPlan_画面原声随转场重叠(t *testing.T) {
	a := SnapClip{MediaID: 5, Kind: "video", SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100}
	b := SnapClip{MediaID: 6, Kind: "video", SourceInMs: 1000, SourceOutMs: 5000, DurationMs: 4000, Speed: 100,
		Volume: 100, Transition: Transition{Kind: TransitionDissolve, DurationMs: 1000}}
	b.FadeInMs = 300 // 用户自己的淡入比转场短：取较大者（转场的 1 秒）
	silent := SnapClip{MediaID: 7, Kind: "video", SourceOutMs: 2000, DurationMs: 2000, Speed: 100, Volume: 100}
	snap := RenderSnapshot{Schema: SnapshotSchema, Width: 1280, Height: 720, FPS: 30, VideoVolume: 100,
		Clips: []SnapClip{a, b, silent}}
	p := audPlan(t, snap, audFiles)
	if n := strings.Count(strings.Join(p.Inputs, " "), "-i "); n != 2 {
		t.Fatalf("没有音轨的段不进输入：%v", p.Inputs)
	}
	for _, want := range []string{
		"[0:a]asetpts=PTS-STARTPTS,aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo,afade=t=out:st=3.000:d=1.000[s0]",
		"adelay=3000:all=1,afade=t=in:st=3.000:d=1.000[s1]",
		"[s0][s1]amix=inputs=2:normalize=0:duration=longest[bo]",
		"[bo]apad=whole_dur=9.000,atrim=0:9.000[mix]",
	} {
		if !strings.Contains(p.Graph, want) {
			t.Errorf("缺少 %q：%s", want, p.Graph)
		}
	}
	if strings.Contains(p.Graph, "concat") || strings.Contains(p.Graph, "anullsrc") {
		t.Fatalf("不再首尾相接、不再补静音：%s", p.Graph)
	}
	snap.VideoMuted = true
	if _, ok := BuildAudioPlan(snap, audFiles); ok {
		t.Fatal("画面轨静音时没有声音")
	}
}

// 三条总线与人声闪避：人声拆成「进混音的一路 + 键」，键补齐到成片总长后找平再接配乐的侧链；
// 音效与普通原声不参与；最后 amix normalize=0。口播素材（画面轨标 voice）的原声也算人声。
func TestBuildAudioPlan_总线与闪避(t *testing.T) {
	video := SnapClip{MediaID: 5, Kind: "video", SourceOutMs: 8000, DurationMs: 8000, Speed: 100, Volume: 100}
	snap := RenderSnapshot{Schema: SnapshotSchema, Width: 1280, Height: 720, FPS: 30, VideoVolume: 100,
		Clips: []SnapClip{video},
		AudioTracks: []SnapAudioTrack{
			{Role: RoleVoice, Volume: 100, Clips: []SnapClip{audClip(3, 0, 2000, 1000, 100)}},
			audMusic(audClip(2, 0, 8000, 0, 100)),
			{Role: RoleSfx, Volume: 100, Clips: []SnapClip{audClip(4, 0, 500, 3000, 100)}},
		}}
	g := audPlan(t, snap, audFiles).Graph
	for _, want := range []string{
		"[s1]asplit=2[bvx][bk0]",
		"[bk0]apad=whole_dur=8.000," + duckLeveler + "[bkk0]",
		fmt.Sprintf("[s2][bkk0]sidechaincompress=threshold=%.6f:ratio=20:attack=50:release=300[bmd0]", duckThreshold(12)),
		"[bvx][bmd0][s3][s0]amix=inputs=4:normalize=0:duration=longest,apad=whole_dur=8.000,atrim=0:8.000[mix]",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("缺少 %q：\n%s", want, g)
		}
	}
	// 画面原声标成人声：与人声轨合成一条人声总线。
	snap.VideoRole = RoleVoice
	g = audPlan(t, snap, audFiles).Graph
	if !strings.Contains(g, "[s0][s1]amix=inputs=2:normalize=0:duration=longest[bv]") || !strings.Contains(g, "[bv]asplit=2") {
		t.Fatalf("口播原声进人声总线：%s", g)
	}
	// 关掉闪避：不拆键、不接侧链。
	snap.AudioTracks[1].Duck = &Duck{Enabled: false, DepthDb: 12}
	g = audPlan(t, snap, audFiles).Graph
	if strings.Contains(g, "asplit") || strings.Contains(g, "sidechaincompress") {
		t.Fatalf("关闭闪避后不接侧链：%s", g)
	}
	// 没有人声时，开着闪避也不接侧链（没有键可接）。
	snap.VideoRole = ""
	snap.AudioTracks = snap.AudioTracks[1:]
	snap.AudioTracks[0].Duck = nil
	if g = audPlan(t, snap, audFiles).Graph; strings.Contains(g, "sidechaincompress") {
		t.Fatalf("没有人声不接侧链：%s", g)
	}
}

// 闪避深度换算成阈值：越深阈值越低；夹在 sidechaincompress 的合法范围里。
func TestDuckThreshold(t *testing.T) {
	if got := duckThreshold(12); math.Abs(got-0.0738) > 0.001 {
		t.Fatalf("12dB 对应阈值 %.4f", got)
	}
	if !(duckThreshold(20) < duckThreshold(12) && duckThreshold(12) < duckThreshold(6)) {
		t.Fatal("越深阈值越低")
	}
	if duckThreshold(0) != duckThreshold(DefaultDuckDepthDb) || duckThreshold(80) < 0.000976563 {
		t.Fatal("缺省与下限")
	}
}

// 旧快照（只有 audio / audioMuted / audioVolume）解析后变成一条配乐轨，照样出计划，音量保留。
func TestBuildAudioPlan_旧快照(t *testing.T) {
	raw := `{"timelineId":1,"timelineVersion":3,"width":1280,"height":720,"fps":30,
		"clips":[{"mediaId":7,"sourceInMs":0,"sourceOutMs":5000,"startMs":0,"durationMs":5000,"speed":100,"volume":100,"fadeInMs":0,"fadeOutMs":0,"transition":{"kind":"","durationMs":0}}],
		"audio":[{"mediaId":2,"sourceInMs":0,"sourceOutMs":2000,"startMs":0,"durationMs":2000,"speed":100,"volume":100,"fadeInMs":0,"fadeOutMs":0,"transition":{"kind":"","durationMs":0}},
		         {"mediaId":2,"sourceInMs":0,"sourceOutMs":2000,"startMs":2000,"durationMs":2000,"speed":100,"volume":100,"fadeInMs":0,"fadeOutMs":0,"transition":{"kind":"","durationMs":0}}],
		"subtitles":[],"audioMuted":false,"audioVolume":60}`
	var snap RenderSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatal(err)
	}
	p := audPlan(t, snap, audFiles)
	for _, want := range []string{"volume=0.6000", "adelay=2000:all=1", "[s0][s1]amix=inputs=2:normalize=0:duration=longest[bm0]"} {
		if !strings.Contains(p.Graph, want) {
			t.Errorf("缺少 %q：%s", want, p.Graph)
		}
	}
}

// 两遍的参数：第一遍只测量（不能压低日志级别，统计按 info 打印）、第二遍带测量值线性归一到 48k 立体声 WAV。
func TestAudioPlan_两遍参数(t *testing.T) {
	p := audPlan(t, audSnap(6000, audMusic(audClip(2, 0, 3000, 0, 100))), audFiles)
	m := strings.Join(p.MeasureArgs(), " ")
	if !strings.HasPrefix(m, "-hide_banner -nostdin") || strings.Contains(m, "-loglevel") ||
		!strings.Contains(m, "[mix]loudnorm=I=-14:TP=-1.5:LRA=11:print_format=json[aout]") || !strings.HasSuffix(m, "-f null -") {
		t.Fatalf("第一遍：%s", m)
	}
	stats := LoudnormStats{InputI: -24.83, InputTP: -16.35, InputLRA: 7, InputThresh: -34.94, TargetOffset: -1.03}
	r := strings.Join(p.RenderArgs(stats, "/tmp/out.wav"), " ")
	for _, want := range []string{"measured_I=-24.83:measured_TP=-16.35:measured_LRA=7.00:measured_thresh=-34.94:offset=-1.03:linear=true",
		"-map [aout] -ar 48000 -ac 2 -c:a pcm_s16le /tmp/out.wav"} {
		if !strings.Contains(r, want) {
			t.Errorf("第二遍缺少 %q：%s", want, r)
		}
	}
	// 整段近乎无声：不归一（把底噪拉到 -14 LUFS 只会得到一段很响的噪声）。
	silent := LoudnormStats{InputI: math.Inf(-1), InputTP: math.Inf(-1), InputThresh: -70}
	if r = strings.Join(p.RenderArgs(silent, "/tmp/out.wav"), " "); strings.Contains(r, "loudnorm") ||
		!strings.Contains(r, "[mix]anull[aout]") {
		t.Fatalf("无声时不归一：%s", r)
	}
}

func TestParseLoudnorm(t *testing.T) {
	stderr := []byte(`[Parsed_loudnorm_9 @ 0x1]
{"x":"不是这一块"}
[Parsed_loudnorm_9 @ 0x2]
{
	"input_i" : "-24.83",
	"input_tp" : "-16.35",
	"input_lra" : "7.00",
	"input_thresh" : "-34.94",
	"output_i" : "-12.97",
	"normalization_type" : "dynamic",
	"target_offset" : "-1.03"
}
`)
	m, err := ParseLoudnorm(stderr)
	if err != nil || m != (LoudnormStats{-24.83, -16.35, 7, -34.94, -1.03}) || !m.Usable() {
		t.Fatalf("取最后一个 JSON 块：%+v %v", m, err)
	}
	silent, err := ParseLoudnorm([]byte(`{"input_i":"-inf","input_tp":"-inf","input_lra":"0.00","input_thresh":"-70.00","target_offset":"inf"}`))
	if err != nil || silent.Usable() {
		t.Fatalf("-inf 能解析、但不能拿来归一：%+v %v", silent, err)
	}
	for _, bad := range []string{"", "没有统计", `{"input_i":"-20"}`, `{"input_i":"x","input_tp":"0","input_lra":"0","input_thresh":"0","target_offset":"0"}`, `{oops}`} {
		if _, err := ParseLoudnorm([]byte(bad)); err == nil {
			t.Errorf("%q 应当报错，不能默默当 0", bad)
		}
	}
}

// 每一处 apad 都要有界（apad=whole_dur=…）：无界的 apad + atrim 在 ffmpeg 8.1.2 上有一定概率永远不结束（见 padTo）。
func TestBuildAudioPlan_apad一律有界(t *testing.T) {
	video := SnapClip{MediaID: 5, Kind: "video", SourceOutMs: 8000, DurationMs: 8000, Speed: 100, Volume: 100}
	snap := RenderSnapshot{Schema: SnapshotSchema, Width: 1280, Height: 720, FPS: 30, VideoVolume: 100, VideoRole: RoleVoice,
		Clips:       []SnapClip{video},
		AudioTracks: []SnapAudioTrack{audMusic(audClip(2, 0, 8000, 0, 100)), audMusic(audClip(4, 0, 500, 3000, 100))}}
	g := audPlan(t, snap, audFiles).Graph
	if n, bounded := strings.Count(g, "apad"), strings.Count(g, "apad=whole_dur=8.000"); n == 0 || n != bounded {
		t.Fatalf("apad %d 处，有界的 %d 处：%s", n, bounded, g)
	}
}

// ---------------------------------------------------------------- 真 ffmpeg 量产物

func audNeedFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("本机没有 ffmpeg")
	}
}

// audCmd 跑 ffmpeg / ffprobe，带 2 分钟上限：滤镜图写错导致进程不结束时，测试应当失败而不是挂住。
func audCmd(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return exec.CommandContext(ctx, name, args...)
}

// audLavfi 用 lavfi 造一段音频（filters 是对源的处理，-af 里做：放进 lavfi 输入里的 adelay / apad 不按预期生效）。
func audLavfi(t *testing.T, dir, name, src, filters string, channels int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", src}
	if filters != "" {
		args = append(args, "-af", filters)
	}
	args = append(args, "-ac", fmt.Sprint(channels), "-ar", "48000", "-c:a", "pcm_s16le", path)
	if out, err := audCmd(t, "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("造 %s 失败：%v %s", name, err, out)
	}
	return path
}

// audRenderMix 只渲到 [mix]（loudnorm 之前），用来量相对电平：loudnorm 退回动态模式时增益随时间变，会把对比搅乱。
func audRenderMix(t *testing.T, p AudioPlan, out string) {
	t.Helper()
	args := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, p.Inputs...)
	args = append(args, "-filter_complex", p.Graph, "-map", "[mix]", "-ar", "48000", "-ac", "2", "-c:a", "pcm_f32le", out)
	if b, err := audCmd(t, "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("渲染失败：%v %s", err, b)
	}
}

// audRenderTwoPass 照 runner 的做法跑两遍。
func audRenderTwoPass(t *testing.T, p AudioPlan, out string) {
	t.Helper()
	b, err := audCmd(t, "ffmpeg", p.MeasureArgs()...).CombinedOutput()
	if err != nil {
		t.Fatalf("第一遍失败：%v %s", err, b)
	}
	m, err := ParseLoudnorm(b)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := audCmd(t, "ffmpeg", p.RenderArgs(m, out)...).CombinedOutput(); err != nil {
		t.Fatalf("第二遍失败：%v %s", err, b)
	}
}

// audPCM 解码成 48k 单声道 float32（左右声道平均），用来量时间与电平。
func audPCM(t *testing.T, path, filters string) []float32 {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-i", path}
	if filters != "" {
		args = append(args, "-af", filters)
	}
	args = append(args, "-ac", "1", "-ar", "48000", "-f", "f32le", "-")
	b, err := audCmd(t, "ffmpeg", args...).Output()
	if err != nil {
		t.Fatalf("解码 %s 失败：%v", path, err)
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

// audRMS 一段样本的 RMS（dBFS）；全零为 -inf。
func audRMS(x []float32, fromMs, toMs int) float64 {
	a, b := fromMs*48, min(toMs*48, len(x))
	if a >= b {
		return math.Inf(-1)
	}
	var sum float64
	for _, v := range x[a:b] {
		sum += float64(v) * float64(v)
	}
	return 10 * math.Log10(sum/float64(b-a))
}

// audSpan 第一个与最后一个超过阈值的样本（毫秒）。
func audSpan(x []float32, threshold float32) (int, int) {
	first, last := -1, -1
	for i, v := range x {
		if v > threshold || v < -threshold {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	return first / 48, last / 48
}

// AC-12：配乐放在 2.5 秒处，0~2.5 秒没有配乐、2.5 秒起有（0.5 秒窗）；成片总长不变。
func TestAudioRender_配乐起点(t *testing.T) {
	audNeedFFmpeg(t)
	dir := t.TempDir()
	music := audLavfi(t, dir, "music.wav", "sine=frequency=330:duration=8", "volume=4", 2)
	files := map[int64]MediaFile{1: {Path: "/dev/null", HasVideo: true}, 2: {Path: music, HasAudio: true, AudioChannels: 2}}
	p := audPlan(t, audSnap(6000, audMusic(audClip(2, 0, 3000, 2500, 100))), files)
	out := filepath.Join(dir, "mix.wav")
	audRenderMix(t, p, out)
	x := audPCM(t, out, "")
	if got := len(x) / 48; got < 5990 || got > 6010 {
		t.Fatalf("成片总长 6 秒：%dms", got)
	}
	var windows []string
	for w := 0; w < 12; w++ {
		windows = append(windows, fmt.Sprintf("%.1f", audRMS(x, w*500, w*500+500)))
	}
	for w := 0; w < 5; w++ {
		if audRMS(x, w*500, w*500+500) > -80 {
			t.Fatalf("2.5 秒之前不该有配乐：%v", windows)
		}
	}
	for w := 5; w < 11; w++ {
		if audRMS(x, w*500, w*500+500) < -30 {
			t.Fatalf("2.5 秒起应有配乐：%v", windows)
		}
	}
	if first, _ := audSpan(x, 0.01); first < 2490 || first > 2510 {
		t.Fatalf("起点 2500ms：%d", first)
	}
	t.Logf("0.5 秒窗 RMS(dB)：%v", windows)
}

// AC-16：人声段配乐至少低 8dB（缺省 12dB 的闪避），关掉闪避后差不超过 1dB；两遍之后整片 -14±1 LUFS。
// 人声用 1kHz 以上、4Hz 起伏的粉噪模拟，配乐是 220Hz 正弦：量配乐时在 220Hz 窄带里量，人声漏不进来。
func TestAudioRender_人声闪避与响度(t *testing.T) {
	audNeedFFmpeg(t)
	dir := t.TempDir()
	music := audLavfi(t, dir, "music.wav", "sine=frequency=220:duration=10", "volume=4", 2)
	files := map[int64]MediaFile{1: {Path: "/dev/null", HasVideo: true}, 2: {Path: music, HasAudio: true, AudioChannels: 2}}
	// 两档人声电平（-20 / -32dBFS 左右的 RMS）：找平之后压低的量应当差不多。
	for _, amp := range []string{"0.6", "0.15"} {
		voice := audLavfi(t, dir, "voice"+amp+".wav", "anoisesrc=color=pink:amplitude="+amp+":duration=3",
			"highpass=f=1000,tremolo=f=4:d=0.6", 1)
		files[3] = MediaFile{Path: voice, HasAudio: true, AudioChannels: 1}
		snap := audSnap(10000,
			SnapAudioTrack{Role: RoleVoice, Volume: 100, Clips: []SnapClip{audClip(3, 0, 3000, 4000, 100)}},
			audMusic(audClip(2, 0, 10000, 0, 100)))
		measure := func(duck *Duck) (float64, float64, float64) {
			snap.AudioTracks[1].Duck = duck
			out := filepath.Join(dir, "mix.wav")
			audRenderMix(t, audPlan(t, snap, files), out)
			band := audPCM(t, out, "bandpass=f=220:width_type=q:w=8")
			all := audPCM(t, out, "")
			return audRMS(band, 1000, 3000), audRMS(band, 5000, 6500), audRMS(audPCM(t, voice, ""), 500, 2500) + 0*audRMS(all, 0, 1)
		}
		off, on, voiceDB := measure(nil)
		if off-on < 8 {
			t.Fatalf("人声 %.1fdBFS：闪避开启时人声段配乐只低 %.1fdB（%.1f → %.1f）", voiceDB, off-on, off, on)
		}
		offD, onD, _ := measure(&Duck{Enabled: false, DepthDb: 12})
		if math.Abs(offD-onD) > 1 {
			t.Fatalf("关闭闪避后差 %.1fdB", offD-onD)
		}
		t.Logf("人声 RMS %.1fdBFS：闪避开 %.1f→%.1fdB（压低 %.1fdB），关 %.1f→%.1fdB", voiceDB, off, on, off-on, offD, onD)
	}

	// 整片响度：两遍 loudnorm 之后 -14±1 LUFS（ebur128 量）。
	snap := audSnap(10000,
		SnapAudioTrack{Role: RoleVoice, Volume: 100, Clips: []SnapClip{audClip(3, 0, 3000, 4000, 100)}},
		audMusic(audClip(2, 0, 10000, 0, 100)))
	out := filepath.Join(dir, "final.wav")
	audRenderTwoPass(t, audPlan(t, snap, files), out)
	b, _ := audCmd(t, "ffmpeg", "-hide_banner", "-nostats", "-i", out, "-af", "ebur128=framelog=quiet", "-f", "null", "-").CombinedOutput()
	i := strings.LastIndex(string(b), "I:")
	var lufs float64
	if i < 0 {
		t.Fatalf("量不出响度：%s", b)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b[i+2:])), "%f", &lufs); err != nil || math.Abs(lufs-LoudnessTargetI) > 1 {
		t.Fatalf("整片响度 %.1f LUFS（%v）", lufs, err)
	}
	if info, err := audCmd(t, "ffprobe", "-v", "error", "-show_entries", "stream=sample_rate,channels,codec_name",
		"-of", "csv=p=0", out).Output(); err != nil || strings.TrimSpace(string(info)) != "pcm_s16le,48000,2" {
		t.Fatalf("输出 48k 立体声 16 位：%s %v", info, err)
	}
	t.Logf("两遍之后整片 %.1f LUFS", lufs)
}

// 单路配乐在 loudnorm 之前电平不变（amix normalize=0、单声道复制成双声道）：与原素材差 ≤ 1dB。
func TestAudioRender_单路电平不变(t *testing.T) {
	audNeedFFmpeg(t)
	dir := t.TempDir()
	for _, ch := range []int{1, 2} {
		src := audLavfi(t, dir, fmt.Sprintf("m%d.wav", ch), "sine=frequency=440:duration=4", "volume=3", ch)
		files := map[int64]MediaFile{1: {Path: "/dev/null", HasVideo: true}, 2: {Path: src, HasAudio: true, AudioChannels: ch}}
		// 与一条只有一段很轻的音效并存：最后那一步 amix 有两路，单路配乐也不能被压低。
		sfx := audLavfi(t, dir, "sfx.wav", "sine=frequency=3000:duration=0.2", "volume=0.01", 2)
		files[4] = MediaFile{Path: sfx, HasAudio: true, AudioChannels: 2}
		snap := audSnap(4000, audMusic(audClip(2, 0, 4000, 0, 100)),
			SnapAudioTrack{Role: RoleSfx, Volume: 100, Clips: []SnapClip{audClip(4, 0, 200, 3500, 100)}})
		out := filepath.Join(dir, "mix.wav")
		audRenderMix(t, audPlan(t, snap, files), out)
		want := audRMS(audPCM(t, src, "pan=mono|c0=c0"), 500, 3000)
		got := audRMS(audPCM(t, out, "pan=mono|c0=c0"), 500, 3000)
		if math.Abs(want-got) > 1 {
			t.Fatalf("%d 声道：原素材 %.2fdB，混音后 %.2fdB", ch, want, got)
		}
		t.Logf("%d 声道：原素材 %.2fdB → 混音后 %.2fdB", ch, want, got)
	}
}

// 0.5× / 2× / 4× 变速：成片里这段从起点开始、占的时长是素材长度除以倍速。
//
// atempo 是按窗口（约 40ms）重叠相加的，起止会被抹开约一个窗口：本机实测 0.5 倍速推后 1 秒的一段，
// 第一个过阈值的样本在 958ms、最后一个在 4982ms。所以起点与时长都按 ±50ms（约 24fps 下一帧多一点）判。
func TestAudioRender_变速时长(t *testing.T) {
	audNeedFFmpeg(t)
	dir := t.TempDir()
	src := audLavfi(t, dir, "tone.wav", "sine=frequency=500:duration=3", "volume=4", 2)
	files := map[int64]MediaFile{1: {Path: "/dev/null", HasVideo: true}, 2: {Path: src, HasAudio: true, AudioChannels: 2}}
	for _, c := range []struct{ speed, want int }{{50, 4000}, {200, 1000}, {400, 500}} {
		snap := audSnap(6000, audMusic(audClip(2, 0, 2000, 1000, c.speed)))
		out := filepath.Join(dir, fmt.Sprintf("s%d.wav", c.speed))
		audRenderMix(t, audPlan(t, snap, files), out)
		first, last := audSpan(audPCM(t, out, ""), 0.01)
		if math.Abs(float64(first-1000)) > 50 || math.Abs(float64(last-first-c.want)) > 50 {
			t.Fatalf("%d%% 变速：%d~%dms，应从 1000ms 起占 %dms", c.speed, first, last, c.want)
		}
		t.Logf("%d%% 变速：%d~%dms（应为 1000~%dms）", c.speed, first, last, 1000+c.want)
	}
}

// AC-13：两段各带原声、中间 1 秒叠化：声音总长等于画面总长；第 2 段的同步标记落在它的成片位置（±1 帧）。
func TestAudioRender_原声随转场对齐(t *testing.T) {
	audNeedFFmpeg(t)
	dir := t.TempDir()
	// 第 1 段素材在 1.0 秒有一声「哔」，第 2 段素材在 2.0 秒有一声；其余是很轻的底噪（模拟现场环境声）。
	mk := func(name string, beepSec float64) string {
		bed := audLavfi(t, dir, name+"-bed.wav", "anoisesrc=color=white:amplitude=0.001:duration=5", "", 2)
		beep := audLavfi(t, dir, name+"-beep.wav", "sine=frequency=1000:duration=0.1", fmt.Sprintf("volume=6,adelay=%d:all=1", int(beepSec*1000)), 2)
		out := filepath.Join(dir, name+".wav")
		if b, err := audCmd(t, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", bed, "-i", beep,
			"-filter_complex", "[0:a][1:a]amix=inputs=2:normalize=0:duration=first[a]", "-map", "[a]", "-c:a", "pcm_s16le", out).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, b)
		}
		return out
	}
	files := map[int64]MediaFile{5: {Path: mk("a", 1.0), HasVideo: true, HasAudio: true, AudioChannels: 2},
		6: {Path: mk("b", 2.0), HasVideo: true, HasAudio: true, AudioChannels: 2}}
	a := SnapClip{MediaID: 5, Kind: "video", SourceOutMs: 4000, DurationMs: 4000, Speed: 100, Volume: 100}
	b := SnapClip{MediaID: 6, Kind: "video", SourceInMs: 1000, SourceOutMs: 5000, DurationMs: 4000, Speed: 100,
		Volume: 100, Transition: Transition{Kind: TransitionDissolve, DurationMs: 1000}}
	snap := RenderSnapshot{Schema: SnapshotSchema, Width: 1280, Height: 720, FPS: 30, VideoVolume: 100,
		Clips: []SnapClip{a, b}}
	out := filepath.Join(dir, "mix.wav")
	audRenderMix(t, audPlan(t, snap, files), out)
	x := audPCM(t, out, "")
	if got, want := len(x)/48, SnapDuration(snap); math.Abs(float64(got-want)) > 33 {
		t.Fatalf("声音总长 %dms，画面 %dms", got, want)
	}
	// 第 2 段从成片 3.0 秒开始、取素材 1.0 秒起，素材 2.0 秒的哔声应在成片 4.0 秒。
	first, _ := audSpan(x[3500*48:], 0.05)
	if got := 3500 + first; got < 3967 || got > 4033 {
		t.Fatalf("第 2 段的同步标记在 %dms，应在 4000ms（±1 帧）", got)
	}
	if f1, _ := audSpan(x, 0.05); f1 < 967 || f1 > 1033 {
		t.Fatalf("第 1 段的同步标记在 %dms，应在 1000ms", f1)
	}
}
