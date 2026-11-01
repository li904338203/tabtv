package domain

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 素材混剪（SC-F-90~93，dev/01 §4.3 第 1 条）。
//
//  1. 每段的可用长度：整段 → 源时长；前 / 中 / 后 / 围绕 → 源时长的 40%（不少于 1.5 秒，不超过源时长）。
//  2. 目标时长 T（自动 = 各段可用长度之和）按权重 × 角色系数（钩子 1.2、结尾 1.1）分配，单段不超过可用长度。
//  3. 素材不够：如实报告，不放慢、不重复；开关打开时放慢到不低于 0.75×，或从高权重段起重复。
//  4. 有节拍时切换点吸到拍上，误差结转给下一段；吸完短于 100ms 的段并入相邻段。
//  5. 转场不超过相邻两段较短者的一半；钩子段硬切。
//
// 顺序：钩子在前、结尾在后，其余按方案的顺序（「钩子在前」是选段原则，方案把钩子写在中间时以作用为准）。

// mixSeg 是编译时的一段。
type mixSeg struct {
	card Card
	seg  Segment
	// in / out 可用区域（素材时间）。
	in, out int
	repeat  bool
	// kind 转入的转场类型（时间线的写法，"" = 硬切）。
	kind  string
	speed int
}

func newMixSeg(c Card, s Segment) *mixSeg {
	in, out := region(c, s.Take)
	return &mixSeg{card: c, seg: s, in: in, out: out, speed: tl.DefaultSpeed}
}

// cap100 不放慢时最多分多长（成片毫秒）。
func (m *mixSeg) cap100() int {
	if m.card.Kind == InputImage {
		return ImageDefaultMs
	}
	return m.out - m.in
}

// capSlow 放慢到 0.75× 时最多分多长。
func (m *mixSeg) capSlow() int {
	if m.card.Kind == InputImage {
		return ImageSlowMs
	}
	return (m.out - m.in) * 100 / MinSlowSpeed
}

// snapCap 卡点时最多能放多长：视频可以向整段素材扩（几百毫秒的出入换切换点落在拍上），图片到 4 秒。
func (m *mixSeg) snapCap() int {
	if m.card.Kind == InputImage {
		return ImageSlowMs
	}
	return m.card.DurationMs * 100 / max(m.speed, 1)
}

func (m *mixSeg) weight() int {
	f, ok := segFactorPermille[m.seg.Role]
	if !ok {
		f = 1000
	}
	return max(m.seg.Weight, 1) * f
}

// orderByRole 钩子在前、结尾在后，其余保持方案顺序（稳定）。
func orderByRole(segs []*mixSeg) []*mixSeg {
	var hooks, mid, ends []*mixSeg
	for _, s := range segs {
		switch s.seg.Role {
		case SegHook:
			hooks = append(hooks, s)
		case SegEnd:
			ends = append(ends, s)
		default:
			mid = append(mid, s)
		}
	}
	return append(append(hooks, mid...), ends...)
}

// planKinds 每段转入的转场：第一段与钩子段硬切（dev/01 §4.3 第 5 条），其余按方案。
func planKinds(segs []*mixSeg) {
	for k, s := range segs {
		s.kind = ""
		if k == 0 || s.seg.Role == SegHook || s.seg.Transition == TransCut || s.seg.Transition == "" {
			continue
		}
		s.kind = s.seg.Transition
	}
}

func plannedOverlap(segs []*mixSeg) int {
	o := 0
	for k, s := range segs {
		if k > 0 && s.kind != "" {
			o += DefaultTransitionMs
		}
	}
	return o
}

// allocInfo 分配时要说出来的事。
type allocInfo struct {
	short    bool
	repeated []string
}

// allocateMix 分配时长（第 2、3 条）。返回（可能加了重复段的）列表与每段分到的成片时长。
//
// headroom > 0（要卡点）时，视频先按「上限 − headroom」分：给每段留出向拍点伸长的余地 ——
// 上一处切换点往前吸了、这一处又要往后吸时，一段最多要多出一拍；用满上限的段只能往回缩，卡点就卡不上。
// 留不出余地（素材刚够）时退回按上限分。
func allocateMix(segs []*mixSeg, spec Spec, headroom int) ([]*mixSeg, []int, allocInfo) {
	var info allocInfo
	caps := func(list []*mixSeg, slow bool) []int {
		out := make([]int, len(list))
		for i, s := range list {
			if slow {
				out[i] = s.capSlow()
			} else {
				out[i] = s.cap100()
			}
		}
		return out
	}
	if spec.TargetMs <= 0 {
		return segs, caps(segs, false), info
	}
	try := func(list []*mixSeg) ([]int, bool) {
		need := spec.TargetMs + plannedOverlap(list)
		c := caps(list, false)
		w := make([]int, len(list))
		for i, s := range list {
			w[i] = s.weight()
		}
		if headroom > 0 {
			ch := make([]int, len(c))
			for i, s := range list {
				ch[i] = c[i]
				if s.card.Kind == InputVideo {
					ch[i] = max(c[i]-headroom, min(c[i], 2*tl.MinClipMs))
				}
			}
			if sumInts(ch) >= need {
				return waterFill(need, w, ch), true
			}
		}
		if sumInts(c) >= need {
			return waterFill(need, w, c), true
		}
		if spec.AllowSlow {
			cs := caps(list, true)
			if sumInts(cs) >= need {
				// 按可用长度等比放慢：每段慢得差不多，节奏不会忽快忽慢。
				return waterFill(need, c, cs), true
			}
			return cs, false
		}
		return c, false
	}
	durs, ok := try(segs)
	if !ok && spec.AllowRepeat {
		segs, info.repeated = withRepeats(segs, spec)
		planKinds(segs)
		durs, ok = try(segs)
	}
	info.short = !ok
	return segs, durs, info
}

// withRepeats 从高权重段起依次重复，直到够目标时长（最多把每段再用三遍），重复段放在结尾段之前。
func withRepeats(segs []*mixSeg, spec Spec) ([]*mixSeg, []string) {
	order := make([]int, len(segs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return segs[order[a]].weight() > segs[order[b]].weight() })
	capOf := func(s *mixSeg) int {
		if spec.AllowSlow {
			return s.capSlow()
		}
		return s.cap100()
	}
	total := -plannedOverlap(segs)
	for _, s := range segs {
		total += capOf(s)
	}
	var extra []*mixSeg
	var nos []string
	for k := 0; total < spec.TargetMs && k < 3*len(segs) && len(segs)+len(extra) < tl.MaxClips; k++ {
		src := segs[order[k%len(order)]]
		cp := *src
		cp.repeat = true
		extra = append(extra, &cp)
		total += capOf(&cp)
		if cp.seg.Transition != TransCut && cp.seg.Transition != "" {
			total -= DefaultTransitionMs
		}
		nos = append(nos, cp.card.No)
	}
	if len(extra) == 0 {
		return segs, nil
	}
	cut := len(segs)
	for cut > 0 && segs[cut-1].seg.Role == SegEnd {
		cut--
	}
	out := append(append(append([]*mixSeg(nil), segs[:cut]...), extra...), segs[cut:]...)
	return out, uniqStrings(nos)
}

// compileMix 素材混剪。
func compileMix(in PlanInput, d Draft, _ bool) (Result, error) {
	idx := cardIndex(in.Cards)
	var segs []*mixSeg
	for _, s := range d.Segments {
		c, ok := idx[s.Clip]
		if !ok || !c.IsPicture() {
			continue
		}
		segs = append(segs, newMixSeg(c, s))
	}
	if len(segs) == 0 {
		return Result{}, &ErrUnsuitable{Reason: UnsuitableNoPicture, Detail: "没有可用的画面素材"}
	}
	segs = orderByRole(segs)
	planKinds(segs)

	var res Result
	// 配乐与节拍先定：要卡点时分配给每段留出伸长的余地。
	music, hasMusic, musicNote := pickMusic(in.Cards, d.Music)
	if musicNote != "" {
		res.Notes = append(res.Notes, musicNote)
	}
	var grid beatGrid
	snapping := false
	if hasMusic && !in.Spec.IgnoreBeat {
		if g, ok := newBeatGrid(in.Beats[music.No]); ok {
			grid, snapping = g, true
		} else {
			res.Notes = append(res.Notes, "配乐（编号 "+music.No+"）还没有节拍，这一版没有卡点")
		}
	}
	headroom := 0
	if snapping {
		headroom = 2 * grid.half
	}
	segs, durs, info := allocateMix(segs, in.Spec, headroom)
	for i, s := range segs {
		if s.card.Kind == InputVideo && durs[i] > s.cap100() {
			s.speed = max(MinSlowSpeed, s.cap100()*100/durs[i])
		}
	}
	overlaps := planOverlaps(segs, durs)
	if in.Spec.TargetMs > 0 && !info.short {
		durs, overlaps = fitTarget(segs, durs, overlaps, in.Spec.TargetMs)
	}
	spans := make([]span, len(segs))
	for i, s := range segs {
		spans[i] = span{dur: durs[i], overlap: overlaps[i], snapCap: max(s.snapCap(), durs[i])}
	}
	switches, end := nominalSwitches(spans)
	if snapping {
		sr := snapToBeats(spans, grid)
		switches, end = sr.switches, sr.end
	}
	onBeat := func(int) bool { return false }
	if snapping {
		onBeat = func(sw int) bool { return grid.nearestDistance(sw) == 0 }
	}
	segs, switches, overlaps, end = mergeTiny(segs, switches, end, overlaps, func(s *mixSeg) int { return s.snapCap() }, onBeat)
	overlaps = clampOverlaps(switches, end, overlaps)
	durs = layoutSpans(switches, end, overlaps)
	var missed []missedBeat
	if snapping {
		missed = missedBeats(grid, switches, in.Base.FPS)
	}

	cuts := make([]cut, len(segs))
	for k, s := range segs {
		l := srcLenFor(durs[k], s.speed)
		if s.card.Kind == InputVideo {
			l = min(l, s.card.DurationMs)
		}
		inMs, outMs := window(s.card, s.seg.Take, s.in, s.out, l)
		cuts[k] = cut{card: s.card, inMs: inMs, outMs: outMs, speed: s.speed, subtitle: s.seg.Subtitle}
		if overlaps[k] > 0 {
			cuts[k].trans = tl.Transition{Kind: s.kind, DurationMs: overlaps[k]}
		}
	}
	starts, total := placeCuts(cuts)

	a := arrangement{}
	for k, c := range cuts {
		a.video = append(a.video, c.clipOf(videoClipID(k)))
	}
	if in.Spec.WithSubtitles {
		a.subtitles = segmentSubtitles(cuts, starts, overlaps)
		a.replaceSubtitles = len(a.subtitles) > 0
		a.subtitleLang = in.Spec.SubtitleLang
	} else if hasSegmentSubtitles(d.Segments) {
		res.Notes = append(res.Notes, "没有打开字幕，方案里写的字幕没有加")
	}
	a.title = titleLayer(d.Title, total)
	if hasMusic {
		clip, warn := musicClip(music, total)
		a.music = clip
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
	}

	// 要说的话：素材不够、放慢、重复、没卡上的拍点、没用上的素材。
	res.Warnings = append(res.Warnings, shortageWarnings(in.Spec, segs, cuts, total, info)...)
	if w := missedBeatWarning(missed); w != "" {
		res.Warnings = append(res.Warnings, w)
	}
	used := usedOrder(cuts)
	if w := unusedPictureWarning(in.Cards, used); w != "" {
		res.Warnings = append(res.Warnings, w)
	}
	seen := map[string]bool{}
	for _, s := range segs {
		if !s.repeat && s.seg.Reason != "" && !seen[s.card.No] {
			seen[s.card.No] = true
			res.Reasons = append(res.Reasons, ClipReason{Clip: s.card.No, Reason: s.seg.Reason})
		}
	}
	res.UsedClips = used
	if in.Spec.TargetMs > 0 && info.short {
		res.Shortage = &Shortage{TotalMs: total, TargetMs: in.Spec.TargetMs}
	}
	return finish(in, a, cuts, res), nil
}

// planOverlaps 每处转场的时长：默认 500ms，不超过相邻两段较短者的一半（dev/01 §4.3 第 5 条），
// 不到 tl.MinTransitionMs 改硬切。按分到的时长算，排布才不会出现负的段长。
func planOverlaps(segs []*mixSeg, durs []int) []int {
	out := make([]int, len(segs))
	for k, s := range segs {
		if k == 0 || s.kind == "" {
			continue
		}
		o := min(DefaultTransitionMs, durs[k-1]/2, durs[k]/2)
		if o < tl.MinTransitionMs {
			o = 0
		}
		out[k] = o
	}
	return out
}

// fitTarget 转场收短之后成片会比目标长（分配时按默认转场留了重叠）：按时长等比把多出来的收回来，
// 每段不短于 2×100ms；收短后转场可能还要再收，反复几次到对齐为止。
func fitTarget(segs []*mixSeg, durs, overlaps []int, target int) ([]int, []int) {
	durs = append([]int(nil), durs...)
	for iter := 0; iter < 4; iter++ {
		diff := sumInts(durs) - sumInts(overlaps) - target
		if diff <= 0 {
			break
		}
		room := make([]int, len(durs))
		for i, d := range durs {
			room[i] = max(d-2*tl.MinClipMs, 0)
		}
		if sumInts(room) == 0 {
			break
		}
		cut := waterFill(min(diff, sumInts(room)), room, room)
		for i := range durs {
			durs[i] -= cut[i]
		}
		next := planOverlaps(segs, durs)
		for i := range next {
			next[i] = min(next[i], overlaps[i])
		}
		overlaps = next
	}
	return durs, overlaps
}

// mergeTiny 吸完之后切换点之间不到 100ms 的段并入相邻段（dev/01 §4.3 第 4 条）：先给前一段，
// 前一段放不下给后一段，都放不下就把它撑到 100ms（从后一段里让；最后一段就把结尾往后挪）。
// 按切换点的间隔判（不算转场 —— 转场随后可能被收短成硬切，那时一段的长度就是这个间隔）。
//
// onBeat 判一个切换点是否在拍上：并入的两种方向各去掉一个切换点，留下在拍上的那个（都在或都不在时先给前一段）。
func mergeTiny[S any](segs []S, switches []int, end int, overlaps []int, capOf func(S) int, onBeat func(int) bool) ([]S, []int, []int, int) {
	for iter := 0; len(segs) > 1 && iter < 3*len(segs)+3; iter++ {
		n := len(segs)
		k := -1
		for i := 0; i < n && k < 0; i++ {
			lo, hi := 0, end
			if i > 0 {
				lo = switches[i]
			}
			if i+1 < n {
				hi = switches[i+1]
			}
			if hi-lo < tl.MinClipMs {
				k = i
			}
		}
		if k < 0 {
			break
		}
		// try 去掉第 seam 个接缝与第 k 段，并入后的那一段（原下标 keep）放得下就采用。
		try := func(seam, keep int) bool {
			sw := withoutIndex(switches, seam)
			ov := withoutIndex(overlaps, seam)
			sg := append(append([]S(nil), segs[:k]...), segs[k+1:]...)
			if keep > k {
				keep--
			}
			if layoutSpans(sw, end, ov)[keep] > capOf(sg[keep]) {
				return false
			}
			segs, switches, overlaps = sg, sw, ov
			return true
		}
		// 并给前一段 = 去掉第 k 处切换点；并给后一段 = 去掉第 k+1 处。第 k 处在拍上而第 k+1 处不在时，先并给后一段。
		nextFirst := k > 0 && k+1 < n && onBeat(switches[k]) && !onBeat(switches[k+1])
		if nextFirst && try(k+1, k+1) {
			continue
		}
		if k > 0 && try(k, k-1) {
			continue
		}
		if k+1 < n && try(k+1, k+1) {
			continue
		}
		lo := 0
		if k > 0 {
			lo = switches[k]
		}
		if k+1 < n {
			switches[k+1] = lo + tl.MinClipMs
		} else {
			end = lo + tl.MinClipMs
		}
	}
	return segs, switches, overlaps, end
}

func withoutIndex(v []int, i int) []int {
	return append(append([]int(nil), v[:i]...), v[i+1:]...)
}

// segmentSubtitles 混剪按段落的字幕：每段一条，跟随该段，范围从本段的切换点到下一段的切换点
// （转场两边各让一半，相邻两条不重叠）。
func segmentSubtitles(cuts []cut, starts, overlaps []int) []tl.ContentSubtitle {
	var out []tl.ContentSubtitle
	for k, c := range cuts {
		if strings.TrimSpace(c.subtitle) == "" {
			continue
		}
		next := 0
		if k+1 < len(cuts) {
			next = overlaps[k+1]
		}
		play := c.playLen()
		filmIn := overlaps[k] / 2
		filmOut := play - (next - next/2)
		srcIn := c.inMs + filmIn*c.speed/100
		srcOut := c.inMs + filmOut*c.speed/100
		if s, ok := anchoredSubtitle("tmp-s"+strconv.Itoa(len(out)+1), c.subtitle, videoClipID(k), starts[k],
			clipOfCut(c), srcIn, min(srcOut, c.outMs)); ok {
			out = append(out, s)
		}
	}
	return out
}

func hasSegmentSubtitles(segs []Segment) bool {
	for _, s := range segs {
		if s.Subtitle != "" {
			return true
		}
	}
	return false
}

// shortageWarnings 素材不够、放慢、重复（SC-F-92）。
func shortageWarnings(spec Spec, segs []*mixSeg, cuts []cut, total int, info allocInfo) []string {
	var out []string
	if spec.TargetMs > 0 && info.short {
		out = append(out, fmt.Sprintf("素材共 %s 秒，不足目标 %s 秒，已按 %s 秒剪", secText(total), secText(spec.TargetMs), secText(total)))
	}
	slowed, slowest := 0, tl.DefaultSpeed
	for _, c := range cuts {
		if c.speed < tl.DefaultSpeed {
			slowed++
			slowest = min(slowest, c.speed)
		}
	}
	if slowed > 0 {
		out = append(out, fmt.Sprintf("素材不够，放慢了 %d 段（最慢 %s×）", slowed, speedText(slowest)))
	}
	if len(info.repeated) > 0 {
		out = append(out, "素材不够，重复用了编号 "+joinNos(info.repeated))
	}
	_ = segs
	return out
}

// missedBeatWarning 没卡上节拍的切换点（SCR-5：不硬吸，说出来）。
func missedBeatWarning(missed []missedBeat) string {
	if len(missed) == 0 {
		return ""
	}
	var where []string
	worst := 0
	for i, m := range missed {
		if i < 3 {
			where = append(where, fmt.Sprintf("第 %d→%d 段", m.k, m.k+1))
		}
		worst = max(worst, m.distance)
	}
	s := fmt.Sprintf("有 %d 处切换点附近没有合适的节拍，没有卡点：%s", len(missed), strings.Join(where, "、"))
	if len(missed) > 3 {
		s += " 等"
	}
	if worst > 0 {
		s += fmt.Sprintf("（最多差 %s 秒）", secText2(worst))
	}
	return s
}

// speedText 速度百分比 → 「0.75」。
func speedText(pct int) string {
	s := strconv.Itoa(pct/100) + "." + fmt.Sprintf("%02d", pct%100)
	return strings.TrimSuffix(strings.TrimSuffix(s, "0"), ".0")
}

// secText2 毫秒 → 两位小数的秒（偏差这类小数字用）。
func secText2(ms int) string {
	h := (ms + 5) / 10
	return fmt.Sprintf("%d.%02d", h/100, h%100)
}

func sumInts(v []int) int {
	s := 0
	for _, x := range v {
		s += x
	}
	return s
}

func uniqStrings(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
