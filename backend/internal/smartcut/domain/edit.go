package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 追加指令（SC-F-110、111）：在已剪好的时间线上接着提要求，AI 只输出对现有片段的原子操作。
//
// ⚠️ 「用户改过的片段不改」在这里兜底（SCT-29 风险条）：不能只靠提示词约束模型。origin=user（以及空，
// 老时间线的片段按 user 处理）的片段，trim / swap / replace / speed / transition / delete 一律跳过并说明。
// ⚠️ 改动清单用模型看到的编号（c1~cN）说「第 N 段」：中间删了、插了的话位置会变，但用户读到的是他刚才看到的那一版。

// EditRef 是交给模型的当前片段（c1~cN，画面轨按顺序）。
type EditRef struct {
	Ref         string `json:"ref"`
	ClipID      string `json:"clipId"`
	CardNo      string `json:"cardNo,omitempty"`
	Kind        string `json:"kind"`
	MediaID     int64  `json:"mediaId,string"`
	SourceInMs  int    `json:"sourceInMs"`
	SourceOutMs int    `json:"sourceOutMs"`
	PlayMs      int    `json:"playMs"`
	Speed       int    `json:"speed"`
	Origin      string `json:"origin"`
	Transition  string `json:"transition,omitempty"`
}

// TextRef 是交给模型的文字层（t1~tN，按开始时间）。
type TextRef struct {
	Ref     string `json:"ref"`
	ID      string `json:"id"`
	Text    string `json:"text"`
	StartMs int    `json:"startMs"`
	EndMs   int    `json:"endMs"`
}

// EditRefs 当前时间线的片段清单与文字层清单。片段的素材编号按（来源节点，媒体）在卡片里找，找不到为空。
func EditRefs(base tl.Content, cards []Card) ([]EditRef, []TextRef) {
	var refs []EditRef
	if vi := videoTrackIndex(base); vi >= 0 {
		for i, c := range base.Tracks[vi].Clips {
			card, _ := cardOfClip(c, cards)
			kind := card.Kind
			if kind == "" {
				kind = InputVideo
			}
			tr := ""
			if c.TransitionIn != nil && c.TransitionIn.IsActive() {
				tr = c.TransitionIn.Kind
			}
			refs = append(refs, EditRef{Ref: "c" + strconv.Itoa(i+1), ClipID: c.ID, CardNo: card.No, Kind: kind,
				MediaID: c.MediaID, SourceInMs: c.SourceInMs, SourceOutMs: c.SourceOutMs, PlayMs: c.PlayLen(),
				Speed: speedOf(c), Origin: originOf(c), Transition: tr})
		}
	}
	texts := append([]tl.ContentText(nil), base.Texts...)
	sortByStart(texts)
	var trefs []TextRef
	for i, t := range texts {
		trefs = append(trefs, TextRef{Ref: "t" + strconv.Itoa(i+1), ID: t.ID, Text: t.Text, StartMs: t.StartMs, EndMs: t.EndMs})
	}
	return refs, trefs
}

// EditRefNames 解析用的编号表。
func EditRefNames(refs []EditRef, texts []TextRef) (clips, textRefs []string) {
	for _, r := range refs {
		clips = append(clips, r.Ref)
	}
	for _, t := range texts {
		textRefs = append(textRefs, t.Ref)
	}
	return clips, textRefs
}

func sortByStart(texts []tl.ContentText) {
	for i := 1; i < len(texts); i++ {
		for j := i; j > 0 && (texts[j].StartMs < texts[j-1].StartMs ||
			(texts[j].StartMs == texts[j-1].StartMs && texts[j].ID < texts[j-1].ID)); j-- {
			texts[j], texts[j-1] = texts[j-1], texts[j]
		}
	}
}

func videoTrackIndex(c tl.Content) int {
	for i, t := range c.Tracks {
		if t.Kind == tl.TrackVideo {
			return i
		}
	}
	return -1
}

func cardOfClip(c tl.ContentClip, cards []Card) (Card, bool) {
	for _, card := range cards {
		if card.MediaID == c.MediaID && (c.SourceNodeID == "" || card.SourceNodeID == c.SourceNodeID) {
			return card, true
		}
	}
	return Card{}, false
}

func speedOf(c tl.ContentClip) int {
	if c.Speed == nil || *c.Speed <= 0 {
		return tl.DefaultSpeed
	}
	return *c.Speed
}

// originOf 空串按 user 处理（timeline/domain.Clip.Origin 的约定）。
func originOf(c tl.ContentClip) string {
	if c.Origin == "" {
		return tl.OriginUser
	}
	return c.Origin
}

// editClip 应用过程中的一段。
type editClip struct {
	ref     string
	clip    tl.ContentClip
	card    Card
	known   bool
	user    bool
	deleted bool
	// replaced 换过素材：原来跟随它的字幕要重新挂（见 rehomeSubtitles）。
	replaced bool
	oldIn    int
	oldOut   int
}

var transitionLabels = map[string]string{TransCut: "硬切", tl.TransitionFade: "淡入淡出", tl.TransitionDissolve: "叠化",
	tl.TransitionSlideLeft: "左滑", tl.TransitionWipe: "划像"}

// ApplyEdit 应用追加指令的原子操作。返回改动清单（人话）、跳过的说明与新内容。
func ApplyEdit(base tl.Content, d Draft, cards []Card) (Result, error) {
	vi := videoTrackIndex(base)
	if vi < 0 || len(base.Tracks[vi].Clips) == 0 {
		return Result{}, &ErrUnsuitable{Reason: UnsuitableNoPicture, Detail: "时间线上还没有片段，先剪一版再追加要求"}
	}
	var list []*editClip
	for i, c := range base.Tracks[vi].Clips {
		card, known := cardOfClip(c, cards)
		list = append(list, &editClip{ref: "c" + strconv.Itoa(i+1), clip: c, card: card, known: known,
			user: originOf(c) == tl.OriginUser})
	}
	texts := append([]tl.ContentText(nil), base.Texts...)
	_, trefs := EditRefs(base, cards)
	textByRef := map[string]string{}
	for _, t := range trefs {
		textByRef[t.Ref] = t.ID
	}
	idx := cardIndex(cards)
	find := func(ref string) int {
		for i, c := range list {
			if c.ref == ref && !c.deleted {
				return i
			}
		}
		return -1
	}
	num := func(ref string) string { return strings.TrimPrefix(ref, "c") }

	var res Result
	skipUser := map[string]bool{}
	skip := func(c *editClip) bool {
		if c.user {
			if !skipUser[c.ref] {
				skipUser[c.ref] = true
				res.Warnings = append(res.Warnings, "第 "+num(c.ref)+" 段是你改过的，没有动")
			}
			return true
		}
		return false
	}
	newID := 0
	nextID := func() string {
		newID++
		return "tmp-e" + strconv.Itoa(newID)
	}

	for _, op := range d.Ops {
		switch op.Op {
		case OpTrim:
			i := find(op.Clip)
			if i < 0 || skip(list[i]) {
				continue
			}
			c := list[i]
			before := c.clip.PlayLen()
			if !trimClip(c, *op.Take) {
				res.Warnings = append(res.Warnings, "第 "+num(c.ref)+" 段的素材时长未知，没法改成整段")
				continue
			}
			c.clip.Origin = tl.OriginAI
			res.Changes = append(res.Changes, fmt.Sprintf("第 %s 段：%s（%s 秒 → %s 秒）", num(c.ref), takeLabel(*op.Take),
				secText(before), secText(c.clip.PlayLen())))
		case OpSwap:
			a, b := find(op.A), find(op.B)
			if a < 0 || b < 0 {
				continue
			}
			if skip(list[a]) || skip(list[b]) {
				continue
			}
			list[a], list[b] = list[b], list[a]
			res.Changes = append(res.Changes, fmt.Sprintf("第 %s 段与第 %s 段互换了位置", num(op.A), num(op.B)))
		case OpReplace:
			i := find(op.Clip)
			card, ok := idx[op.With]
			if i < 0 || !ok || skip(list[i]) {
				continue
			}
			c := list[i]
			replaceClip(c, card, op.Take)
			res.Changes = append(res.Changes, fmt.Sprintf("第 %s 段换成了编号 %s", num(c.ref), card.No))
		case OpSpeed:
			targets := []int{}
			if op.Clip == "*" {
				for i, c := range list {
					if !c.deleted {
						targets = append(targets, i)
					}
				}
			} else if i := find(op.Clip); i >= 0 {
				targets = append(targets, i)
			}
			changed := 0
			for _, i := range targets {
				c := list[i]
				if skip(c) || (c.known && c.card.Kind == InputImage) {
					continue
				}
				c.clip.Speed = nil
				if op.SpeedPct != tl.DefaultSpeed {
					c.clip.Speed = intPtr(min(max(op.SpeedPct, 25), 400))
				}
				// 变速后太短（成片里不到 100ms）就不改这一段。
				if c.clip.PlayLen() < tl.MinClipMs {
					c.clip.Speed = nil
					continue
				}
				c.clip.Origin = tl.OriginAI
				changed++
			}
			if changed == 0 {
				continue
			}
			if op.Clip == "*" {
				res.Changes = append(res.Changes, fmt.Sprintf("%d 段的速度改为 %s×", changed, speedText(op.SpeedPct)))
			} else {
				res.Changes = append(res.Changes, fmt.Sprintf("第 %s 段的速度改为 %s×", num(op.Clip), speedText(op.SpeedPct)))
			}
		case OpTransition:
			i := find(op.Clip)
			if i < 0 || skip(list[i]) {
				continue
			}
			c := list[i]
			if op.Kind == TransCut {
				c.clip.TransitionIn = nil
			} else {
				ms := op.DurationMs
				if ms <= 0 {
					ms = DefaultTransitionMs
				}
				c.clip.TransitionIn = &tl.Transition{Kind: op.Kind, DurationMs: ms}
			}
			c.clip.Origin = tl.OriginAI
			res.Changes = append(res.Changes, fmt.Sprintf("第 %s 段的转场改成「%s」", num(c.ref), transitionLabels[op.Kind]))
		case OpText:
			if op.Target != "" {
				id := textByRef[op.Target]
				for k := range texts {
					if texts[k].ID == id {
						res.Changes = append(res.Changes, fmt.Sprintf("文字「%s」改成「%s」", headRunes(texts[k].Text, 20), op.Text))
						texts[k].Text = op.Text
					}
				}
				continue
			}
			res.Changes = append(res.Changes, fmt.Sprintf("加了文字「%s」", op.Text))
			texts = append(texts, tl.ContentText{ID: nextID(), Text: op.Text, Style: titleStyle, Box: titleBox,
				TrackID: "@" + op.Clip})
		case OpInsert:
			card, ok := idx[op.With]
			if !ok {
				continue
			}
			pos := len(list)
			switch op.After {
			case "start":
				pos = 0
			case "end", "":
			default:
				pos = -1
				for i, c := range list {
					if c.ref == op.After {
						pos = i + 1
					}
				}
				if pos < 0 {
					continue
				}
			}
			take := Take{Kind: TakeHead}
			if op.Take != nil {
				take = *op.Take
			}
			rin, rout := region(card, take)
			ec := &editClip{ref: "", card: card, known: true,
				clip: tl.ContentClip{ID: nextID(), MediaID: card.MediaID, SourceInMs: rin, SourceOutMs: rout,
					Origin: tl.OriginAI, SourceNodeID: card.SourceNodeID}}
			list = append(list[:pos], append([]*editClip{ec}, list[pos:]...)...)
			where := "最前面"
			switch {
			case op.After == "end" || op.After == "":
				where = "最后"
			case op.After != "start":
				where = "第 " + num(op.After) + " 段后面"
			}
			res.Changes = append(res.Changes, fmt.Sprintf("在%s插入了编号 %s", where, card.No))
		case OpDelete:
			i := find(op.Clip)
			if i < 0 || skip(list[i]) {
				continue
			}
			alive := 0
			for _, c := range list {
				if !c.deleted {
					alive++
				}
			}
			if alive <= 1 {
				res.Warnings = append(res.Warnings, "至少要留一段，第 "+num(op.Clip)+" 段没有删")
				continue
			}
			list[i].deleted = true
			res.Changes = append(res.Changes, "删掉了第 "+num(op.Clip)+" 段")
		}
	}

	// 收尾：去掉删掉的段，转场按相邻两段较短者的一半收短（第一段的转入是从黑场淡入，只看自己）。
	var clips []tl.ContentClip
	var alive []*editClip
	for _, c := range list {
		if !c.deleted {
			alive = append(alive, c)
			clips = append(clips, c.clip)
		}
	}
	for k := range clips {
		t := clips[k].TransitionIn
		if t == nil || !t.IsActive() {
			clips[k].TransitionIn = nil
			continue
		}
		lim := clips[k].PlayLen() / 2
		if k > 0 {
			lim = min(lim, clips[k-1].PlayLen()/2)
		}
		d := min(t.DurationMs, lim, tl.MaxTransitionMs)
		if d < tl.MinTransitionMs {
			clips[k].TransitionIn = nil
			continue
		}
		clips[k].TransitionIn = &tl.Transition{Kind: t.Kind, DurationMs: d}
	}
	out := base
	out.Tracks = append([]tl.ContentTrack(nil), base.Tracks...)
	out.Tracks[vi].Clips = clips

	// 跟随片段的字幕：删掉的段带走它的字幕；换了素材的段，铺满整段的字幕重新挂到新素材上，其余的去掉。
	subs, note := rehomeSubtitles(base.Subtitles, list)
	out.Subtitles = subs
	if note != "" {
		res.Notes = append(res.Notes, note)
	}
	// 新加的文字：跟着某一段出现（或放在开头 3 秒），放进第一条不重叠的文字轨，放不下就不加。
	starts, total := tl.VideoPlacement(contentClipsToClips(clips))
	spans := map[string][2]int{}
	for k, c := range alive {
		if c.ref != "" {
			spans[c.ref] = [2]int{starts[k], starts[k] + clips[k].PlayLen()}
		}
	}
	out, textNotes := placeNewTexts(out, texts, spans, total)
	res.Notes = append(res.Notes, textNotes...)
	res.Content = out
	res.TotalMs = total
	refs, _ := EditRefs(out, cards)
	var used []string
	seen := map[string]bool{}
	for _, r := range refs {
		if r.CardNo != "" && !seen[r.CardNo] {
			seen[r.CardNo] = true
			used = append(used, r.CardNo)
		}
	}
	res.UsedClips = used
	res.Summary = d.Summary
	if res.Summary == "" {
		res.Summary = fmt.Sprintf("改了 %d 处，成片 %s 秒", len(res.Changes), secText(total))
	}
	if res.Warnings == nil {
		res.Warnings = []string{}
	}
	if len(res.Changes) == 0 {
		res.Warnings = append(res.Warnings, "这次要求没有改动任何片段")
	}
	return res, nil
}

// trimClip 改取段（相对当前的取用区间）。整段 = 素材本身（要知道素材时长）。
func trimClip(c *editClip, t Take) bool {
	in, out := c.clip.SourceInMs, c.clip.SourceOutMs
	l := out - in
	speed := speedOf(c.clip)
	minSrc := (tl.MinClipMs*speed + 99) / 100
	switch t.Kind {
	case TakeFull:
		if !c.known || c.card.Kind == InputImage {
			if c.known && c.card.Kind == InputImage {
				c.clip.SourceInMs, c.clip.SourceOutMs = 0, ImageDefaultMs
				return true
			}
			return false
		}
		c.clip.SourceInMs, c.clip.SourceOutMs = 0, c.card.DurationMs
		return true
	}
	ratio := t.RatioPermille
	if ratio <= 0 {
		ratio = TakePartPermille
	}
	nl := max((l*ratio+500)/1000, minSrc)
	nl = min(nl, l)
	switch t.Kind {
	case TakeHead:
		c.clip.SourceOutMs = in + nl
	case TakeTail:
		c.clip.SourceInMs = out - nl
	case TakeMiddle:
		c.clip.SourceInMs = in + (l-nl)/2
		c.clip.SourceOutMs = c.clip.SourceInMs + nl
	case TakeAround:
		limit := out
		if c.known && c.card.DurationMs > 0 {
			limit = c.card.DurationMs
		}
		ni := min(max(t.AroundMs-nl/2, 0), max(limit-nl, 0))
		c.clip.SourceInMs, c.clip.SourceOutMs = ni, ni+nl
	}
	return true
}

func takeLabel(t Take) string {
	pct := t.RatioPermille / 10
	if pct <= 0 {
		pct = TakePartPermille / 10
	}
	switch t.Kind {
	case TakeFull:
		return "改用整段"
	case TakeHead:
		return fmt.Sprintf("只取前 %d%%", pct)
	case TakeTail:
		return fmt.Sprintf("只取后 %d%%", pct)
	case TakeMiddle:
		return fmt.Sprintf("只取中间 %d%%", pct)
	case TakeAround:
		return fmt.Sprintf("围绕第 %s 秒取 %d%%", secText(t.AroundMs), pct)
	}
	return "改了取段"
}

// replaceClip 换素材：尽量保持原来在成片里的长度（速度回到 1×），原素材连同原区间进备选（可以切回）。
func replaceClip(c *editClip, card Card, take *Take) {
	play := c.clip.PlayLen()
	c.oldIn, c.oldOut = c.clip.SourceInMs, c.clip.SourceOutMs
	c.clip.Alternates = tl.PushAlternate(c.clip.Alternates, tl.Alternate{MediaID: c.clip.MediaID,
		SourceInMs: c.clip.SourceInMs, SourceOutMs: c.clip.SourceOutMs, Kind: tl.AlternateOriginal})
	t := Take{Kind: TakeFull}
	if take != nil {
		t = *take
	}
	rin, rout := region(card, t)
	l := play
	if card.Kind == InputVideo {
		l = min(l, card.DurationMs)
	}
	in, out := window(card, t, rin, rout, max(l, tl.MinClipMs))
	c.clip.MediaID, c.clip.SourceInMs, c.clip.SourceOutMs = card.MediaID, in, out
	c.clip.Speed = nil
	c.clip.SourceNodeID = card.SourceNodeID
	c.clip.Origin = tl.OriginAI
	c.card, c.known, c.replaced = card, true, true
}

// rehomeSubtitles 跟随片段的字幕随编辑调整。
func rehomeSubtitles(subs []tl.ContentSubtitle, list []*editClip) ([]tl.ContentSubtitle, string) {
	byID := map[string]*editClip{}
	for _, c := range list {
		byID[c.clip.ID] = c
	}
	var out []tl.ContentSubtitle
	removed := 0
	for _, s := range subs {
		id, ok := anchorClipID(s.Anchor)
		c, found := byID[id]
		if !ok || !found {
			out = append(out, s)
			continue
		}
		if c.deleted {
			removed++
			continue
		}
		if c.replaced {
			var a struct {
				SrcStartMs int `json:"srcStartMs"`
				SrcEndMs   int `json:"srcEndMs"`
			}
			_ = json.Unmarshal(s.Anchor, &a)
			if old := c.oldOut - c.oldIn; old > 0 && (min(a.SrcEndMs, c.oldOut)-max(a.SrcStartMs, c.oldIn))*10 >= old*9 {
				s.Anchor = []byte(fmt.Sprintf(`{"clipId":%q,"srcStartMs":%d,"srcEndMs":%d}`, id, c.clip.SourceInMs, c.clip.SourceOutMs))
				out = append(out, s)
				continue
			}
			removed++
			continue
		}
		out = append(out, s)
	}
	if removed > 0 {
		return out, fmt.Sprintf("跟随被删掉或换掉的片段的 %d 条字幕一起去掉了", removed)
	}
	return out, ""
}

// placeNewTexts 新加的文字（TrackID 写成 "@cN" 或 "@" 的占位）：时间跟着那一段（或开头 3 秒），
// 放进第一条在那段时间里空着的文字轨；都占着且还能加轨就新建一条，否则不加并说明。
// spans 是按模型看到的编号（cN）给出的那一段在新成片里的起止。
func placeNewTexts(c tl.Content, texts []tl.ContentText, spans map[string][2]int, total int) (tl.Content, []string) {
	var notes []string
	var keep []tl.ContentText
	var pending []tl.ContentText
	for _, t := range texts {
		if strings.HasPrefix(t.TrackID, "@") {
			pending = append(pending, t)
		} else {
			keep = append(keep, t)
		}
	}
	c.Texts = keep
	for _, t := range pending {
		ref := strings.TrimPrefix(t.TrackID, "@")
		if r, ok := spans[ref]; ok && ref != "" {
			t.StartMs, t.EndMs = r[0], r[1]
		} else {
			t.StartMs, t.EndMs = 0, min(3000, total)
		}
		if t.EndMs-t.StartMs < tl.MinClipMs {
			notes = append(notes, "文字「"+headRunes(t.Text, 20)+"」没有地方放")
			continue
		}
		placed := false
		for _, tr := range c.Tracks {
			if tr.Kind != tl.TrackText || overlapsTrack(c.Texts, tr.ID, t.StartMs, t.EndMs) {
				continue
			}
			t.TrackID = tr.ID
			placed = true
			break
		}
		if !placed {
			n := 0
			for _, tr := range c.Tracks {
				if tr.Kind == tl.TrackText {
					n++
				}
			}
			if n >= tl.MaxTextTracks {
				notes = append(notes, "文字轨已满，文字「"+headRunes(t.Text, 20)+"」没有加")
				continue
			}
			id := "tmp-track-text" + strconv.Itoa(n+1)
			c.Tracks = append(c.Tracks, tl.ContentTrack{ID: id, Kind: tl.TrackText})
			t.TrackID = id
		}
		c.Texts = append(c.Texts, t)
	}
	return c, notes
}

func overlapsTrack(texts []tl.ContentText, trackID string, start, end int) bool {
	for _, t := range texts {
		if t.TrackID == trackID && t.StartMs < end && start < t.EndMs {
			return true
		}
	}
	return false
}

func contentClipsToClips(cs []tl.ContentClip) []tl.Clip {
	out := make([]tl.Clip, len(cs))
	for i, c := range cs {
		out[i] = tl.Clip{SourceInMs: c.SourceInMs, SourceOutMs: c.SourceOutMs, Speed: speedOf(c)}
		if c.TransitionIn != nil {
			out[i].TransitionIn = *c.TransitionIn
		}
	}
	return out
}
