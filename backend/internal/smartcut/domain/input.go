package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// 素材条（docs/agent/smartcut SC-F-10~18，SCD-22）：智能剪辑节点的上游 → 编号、类型、状态。
//
// 规则只有这一份 Go 实现；前端 `frontend/utils/smartcutInputs.ts` 是同一套，两边读同一批
// api/fixtures/smartcut/inputs/（SCR-11）。改规则时先改 fixture，两边一起过。
//
// 纯计算：上游的事实（生成的真实状态与全部产物、媒体的归属与时长）由调用方经 Lookup 给出 ——
// 服务端回查 generation 表与媒体表，不信画布上的 result（CLAUDE.md §5.4「result 是 UI 快照」）。

// 素材的种类与状态（契约 SmartcutInput）。
const (
	InputVideo     = "video"
	InputImage     = "image"
	InputAudio     = "audio"
	InputText      = "text"
	InputShotGroup = "shotGroup"

	InputReady   = "ready"
	InputPending = "pending"
	InputFailed  = "failed"
	InputInvalid = "invalid"

	RoleMusic = "music"
	RoleVoice = "voice"
)

// 上限（SC-PARAM-20）：单个上游最多取前 9 个产物（SCK-20）；素材条最多 50 条，超出的只报个数（SC-F-17）；
// 文本截到 2000 个字（按码点）。
const (
	MaxOutputsPerSource = 9
	MaxInputItems       = 50
	MaxInputTextChars   = 2000
)

// Source 是一条入边的上游（由 canvas/domain.InboundSources 转来，wiring 适配）。
type Source struct {
	Order  int
	NodeID string
	Type   string
	Title  string
	Params map[string]any
	Result SourceResult
	// FirstInboundOperation 上游节点自己的第一条入边的操作码（音频默认用途看它）。
	FirstInboundOperation string
}

// SourceResult 上游节点 result 的快照（只作兜底与显示，事实以 Lookup 为准）。
type SourceResult struct {
	ID                 string
	SourceGenerationID string
	Status             string
	MediaID            string
	OutputURL          string
	OutputType         string
	OutputText         string
	Error              string
}

// GenerationFact 是回查到的一次生成：Status 为 queued / running / success / failed / cancelled，
// 或 invalid（不是本项目的生成）。OutputsKnown 区分「查过、确实没有产物」与「没查」。
type GenerationFact struct {
	Status       string
	Operation    string
	Reason       string
	Outputs      []OutputFact
	OutputsKnown bool
}

// OutputFact 是一个产物（按 idx）。
type OutputFact struct {
	MediaID       string
	DurationMs    *int
	HasAudio      *bool
	PosterMediaID string
}

// MediaFact 是一份媒体：Status 为 ready，或 invalid（别的工作区 / 平台素材 / 已删除），后者带 Reason。
type MediaFact struct {
	Status        string
	Reason        string
	DurationMs    *int
	HasAudio      *bool
	PosterMediaID string
}

// Lookup 给出上游的事实。查不到（ok=false）时退回快照。
type Lookup interface {
	Generation(id string) (GenerationFact, bool)
	Media(id string) (MediaFact, bool)
}

// Input 是素材条里的一条（契约 SmartcutInput，JSON 形状逐字一致）。
type Input struct {
	No             string `json:"no"`
	SourceNodeID   string `json:"sourceNodeId"`
	SourceNodeType string `json:"sourceNodeType"`
	Title          string `json:"title,omitempty"`
	Kind           string `json:"kind"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	MediaID        string `json:"mediaId,omitempty"`
	DurationMs     *int   `json:"durationMs,omitempty"`
	HasAudio       *bool  `json:"hasAudio,omitempty"`
	AudioRole      string `json:"audioRole,omitempty"`
	PosterMediaID  string `json:"posterMediaId,omitempty"`
	Text           string `json:"text,omitempty"`
	Used           bool   `json:"used"`
	// Changed 同一来源节点的产物与片段当前用的素材不同（上游重新生成过，SCT-30）。
	Changed bool `json:"changed,omitempty"`

	// ScriptID / GroupID 镜头组的来源（分镜成片按它装配，SCT-28）。不进契约：素材条上看不到，也不该让前端拿它去查别的。
	ScriptID string `json:"-"`
	GroupID  string `json:"-"`
}

// UsedClip 时间线里的一段（判 used 用）：来源节点与素材。
type UsedClip struct {
	SourceNodeID string
	MediaID      string
}

// ResolveInputs 把上游解析成素材条（按连线顺序编号，多产物拆成 3-1、3-2……）。
// omitted 是超出 50 条、没进素材条的条数（要说出来）。
func ResolveInputs(sources []Source, lookup Lookup, clips []UsedClip) (items []Input, omitted int) {
	var all []Input
	for i, src := range sources {
		drafts := resolveSource(src, lookup)
		for k, d := range drafts {
			if len(drafts) > 1 {
				d.No = strconv.Itoa(i+1) + "-" + strconv.Itoa(k+1)
			} else {
				d.No = strconv.Itoa(i + 1)
			}
			for _, c := range clips {
				if c.SourceNodeID == d.SourceNodeID && (d.MediaID == "" || c.MediaID == d.MediaID) {
					d.Used = true
					break
				}
			}
			all = append(all, d)
		}
	}
	markChanged(all, clips)
	if len(all) > MaxInputItems {
		return all[:MaxInputItems], len(all) - MaxInputItems
	}
	return all, 0
}

// markChanged 「同一来源节点的产物与片段当前用的素材不同」（SCT-30）：节点上有片段用的媒体已经不在它现在的产物里
// （上游重新生成过），那么这个节点没被用到的新产物标 changed —— 原位替换换进去的就是它们。
// ⚠️ 只按来源节点与媒体 ID 判（SCR-11）：「变没变」不看画面、不看时长，也不在前端算。
func markChanged(items []Input, clips []UsedClip) {
	outputs := map[string]map[string]bool{}
	for _, it := range items {
		if it.Status == InputReady && it.MediaID != "" && Placeable(it.Kind) {
			if outputs[it.SourceNodeID] == nil {
				outputs[it.SourceNodeID] = map[string]bool{}
			}
			outputs[it.SourceNodeID][it.MediaID] = true
		}
	}
	stale := map[string]bool{}
	for _, c := range clips {
		if out, ok := outputs[c.SourceNodeID]; ok && c.MediaID != "" && !out[c.MediaID] {
			stale[c.SourceNodeID] = true
		}
	}
	for i := range items {
		it := &items[i]
		if stale[it.SourceNodeID] && !it.Used && it.Status == InputReady && it.MediaID != "" && Placeable(it.Kind) {
			it.Changed = true
		}
	}
}

// InputsStale 「素材有变化」（SCT-30）：剪辑依据的素材指纹（AI 剪辑或自动铺轨时记下的）与最近一次同步看到的不同。
// 还没剪过（没有剪辑依据）时不算。
func InputsStale(planned, synced string) bool {
	return planned != "" && synced != "" && planned != synced
}

// ReplaceClip 原位替换的一段：把引用旧产物的片段换成同一来源节点的新产物（SCT-30）。
type ReplaceClip struct {
	ClipID       string
	SourceNodeID string
	OldMediaID   string
	NewMediaID   string
	// NewDurationMs 新产物的时长（0 = 未知；图片为 0）。
	NewDurationMs int
}

// ClipUse 时间线里的一段（原位替换用）：片段 ID、来源节点、素材。按时间线里的顺序给。
type ClipUse struct {
	ID           string
	SourceNodeID string
	MediaID      string
}

// PlanReplaceChanged 原位替换的计划：每个来源节点里引用已不在产物里的媒体的片段，按时间线顺序依次换成这个节点
// 没被用到的新产物（多产物时第 k 段换第 k 个，不够就从头再用）。只换「同一来源节点的新产物」，不去智能匹配别的素材
// —— 那会把用户的选择悄悄换掉。来源已断开（节点不在素材条里）的片段不动。
func PlanReplaceChanged(items []Input, clips []ClipUse) []ReplaceClip {
	type output struct {
		id  string
		dur int
	}
	fresh := map[string][]output{}
	current := map[string]map[string]bool{}
	for _, it := range items {
		if it.Status != InputReady || it.MediaID == "" || !Placeable(it.Kind) {
			continue
		}
		if current[it.SourceNodeID] == nil {
			current[it.SourceNodeID] = map[string]bool{}
		}
		current[it.SourceNodeID][it.MediaID] = true
		if !it.Used {
			d := 0
			if it.DurationMs != nil {
				d = *it.DurationMs
			}
			fresh[it.SourceNodeID] = append(fresh[it.SourceNodeID], output{it.MediaID, d})
		}
	}
	var out []ReplaceClip
	next := map[string]int{}
	for _, c := range clips {
		cur, connected := current[c.SourceNodeID]
		news := fresh[c.SourceNodeID]
		if !connected || cur[c.MediaID] || len(news) == 0 {
			continue
		}
		o := news[next[c.SourceNodeID]%len(news)]
		next[c.SourceNodeID]++
		out = append(out, ReplaceClip{ClipID: c.ID, SourceNodeID: c.SourceNodeID, OldMediaID: c.MediaID,
			NewMediaID: o.id, NewDurationMs: o.dur})
	}
	return out
}

// InputKindOf 节点作为素材时的种类；不认的为空。导演台给的是截图（图片），脚本与视频组是镜头组。
func InputKindOf(nodeType string, params map[string]any) string {
	switch nodeType {
	case InputVideo, InputImage, InputAudio, InputText:
		return nodeType
	case "director":
		return InputImage
	case "script":
		return InputShotGroup
	case "group":
		if g, _ := params["groupType"].(string); g == "video_generator" {
			return InputShotGroup
		}
	}
	return ""
}

// OpTextToSpeech 文字转语音的操作码（音频节点的默认操作）。
const OpTextToSpeech = "text2audio"

// DefaultAudioRole 音频的默认用途（SC-F-11）：文字转语音（text2audio）来的默认人声，其余（上传、配乐、音效）默认配乐。
// operation 以回查到的为准；没有时按画布推断：没有生成的（上传）是配乐，有生成的看它自己第一条入边的操作，
// 缺省就是 text2audio（音频节点的默认操作）。
func DefaultAudioRole(src Source, operation string) string {
	if operation != "" {
		if operation == OpTextToSpeech {
			return RoleVoice
		}
		return RoleMusic
	}
	if src.Result.ID == "" && src.Result.SourceGenerationID == "" {
		return RoleMusic
	}
	if op := src.FirstInboundOperation; op == "" || op == OpTextToSpeech {
		return RoleVoice
	}
	return RoleMusic
}

var generationFailed = map[string]bool{"failed": true, "cancelled": true}

func resolveSource(src Source, lookup Lookup) []Input {
	base := Input{SourceNodeID: src.NodeID, SourceNodeType: src.Type, Title: src.Title}
	kind := InputKindOf(src.Type, src.Params)
	if kind == "" {
		d := base
		d.Kind, d.Status, d.Reason = InputVideo, InputInvalid, "不支持的输入："+src.Type
		switch src.Result.OutputType {
		case InputImage, InputAudio, InputText:
			d.Kind = src.Result.OutputType
		}
		return []Input{d}
	}
	base.Kind = kind

	if kind == InputShotGroup {
		key, reason := "groupId", "视频组还没建好"
		if src.Type == "script" {
			key, reason = "scriptId", "脚本还没有分镜"
		}
		d := base
		if paramID(src.Params[key]) != "" {
			d.Status = InputReady
			d.ScriptID, d.GroupID = paramID(src.Params["scriptId"]), paramID(src.Params["groupId"])
		} else {
			d.Status, d.Reason = InputPending, reason
		}
		return []Input{d}
	}

	resultID, snap := src.Result.ID, src.Result.Status
	if kind == InputText {
		return []Input{resolveText(base, src)}
	}

	withRole := func(d Input, operation string) Input {
		if kind == InputAudio {
			d.AudioRole = DefaultAudioRole(src, operation)
		}
		return d
	}

	// 导演台节点：给下游的是输出机位的最新截图（params.outputMediaId）。
	if src.Type == "director" {
		if id := digits(src.Params["outputMediaId"]); id != "" {
			d := base
			d.Status, d.MediaID = InputReady, id
			return []Input{withMedia(d, lookup)}
		}
		d := base
		d.Status, d.Reason = InputPending, "请先在导演台截图"
		return []Input{d}
	}

	if resultID != "" {
		fact, known := lookup.Generation(resultID)
		status := snap
		if known {
			status = fact.Status
		}
		d := base
		switch {
		case status == InputInvalid:
			d.Status, d.Reason = InputInvalid, orDefault(fact.Reason, "这个生成不属于本项目")
			return []Input{d}
		case generationFailed[status]:
			d.Status, d.Reason = InputFailed, orDefault(fact.Reason, orDefault(src.Result.Error, "上游生成失败"))
			return []Input{d}
		case status != "success":
			// 排队、运行中与认不出的状态一律当「还在生成」：认不出的状态不能当成功，也不该当失败去吓用户。
			d.Status, d.Reason = InputPending, "上游还在生成"
			return []Input{d}
		}
		if known && fact.OutputsKnown && len(fact.Outputs) == 0 {
			d.Status, d.Reason = InputFailed, "上游任务完成但没有产物"
			return []Input{d}
		}
		outputs := fact.Outputs
		if !known || !fact.OutputsKnown {
			outputs = nil
			if id := resultMediaID(src.Result); id != "" {
				outputs = []OutputFact{{MediaID: id}}
			}
		}
		if len(outputs) == 0 {
			d.Status = InputReady
			return []Input{withRole(d, fact.Operation)}
		}
		if len(outputs) > MaxOutputsPerSource {
			outputs = outputs[:MaxOutputsPerSource]
		}
		out := make([]Input, 0, len(outputs))
		for _, o := range outputs {
			d := base
			d.Status, d.MediaID, d.DurationMs, d.HasAudio, d.PosterMediaID = InputReady, o.MediaID, o.DurationMs,
				o.HasAudio, o.PosterMediaID
			out = append(out, withRole(d, fact.Operation))
		}
		return out
	}

	// 没有生成 ID：上传节点、复制出来的节点、发布节点 —— 按媒体声明。
	mediaID := ""
	if snap == "" || snap == "success" {
		mediaID = resultMediaID(src.Result)
	}
	if mediaID == "" {
		mediaID = staticMediaID(src.Params)
	}
	d := base
	if mediaID != "" {
		d.Status, d.MediaID = InputReady, mediaID
		return []Input{withMedia(withRole(d, ""), lookup)}
	}
	if generationFailed[snap] {
		d.Status, d.Reason = InputFailed, orDefault(src.Result.Error, "上传失败")
		return []Input{d}
	}
	d.Status, d.Reason = InputPending, "还没有产出"
	if snap != "" {
		d.Reason = "还在上传"
	}
	return []Input{d}
}

// resolveText 文本：有成功的生成时取产出（与前端 generationInputs 一致），否则取节点上写的文字。
func resolveText(d Input, src Source) Input {
	resultID, snap := src.Result.ID, src.Result.Status
	if out := strings.TrimSpace(src.Result.OutputText); resultID != "" && snap == "success" && out != "" {
		d.Status, d.Text = InputReady, truncateRunes(out, MaxInputTextChars)
		return d
	}
	// 与前端的 `params.text ?? params.prompt` 同义：text 存在（哪怕是空白）就不再看 prompt。
	value, ok := src.Params["text"]
	if !ok || value == nil {
		value = src.Params["prompt"]
	}
	if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
		d.Status, d.Text = InputReady, truncateRunes(strings.TrimSpace(s), MaxInputTextChars)
		return d
	}
	if resultID != "" && generationFailed[snap] {
		d.Status, d.Reason = InputFailed, orDefault(src.Result.Error, "上游生成失败")
		return d
	}
	d.Status, d.Reason = InputPending, "文本还是空的"
	if resultID != "" {
		d.Reason = "上游还在生成"
	}
	return d
}

// withMedia 按媒体声明的素材：服务端说归属不合法时记 invalid（不进轨，也不带用途），否则补上时长、声音与封面。
func withMedia(d Input, lookup Lookup) Input {
	fact, ok := lookup.Media(d.MediaID)
	if !ok {
		return d
	}
	if fact.Status == InputInvalid {
		d.AudioRole = ""
		d.Status, d.Reason = InputInvalid, orDefault(fact.Reason, "素材不属于本工作区")
		return d
	}
	if fact.DurationMs != nil {
		d.DurationMs = fact.DurationMs
	}
	if fact.HasAudio != nil {
		d.HasAudio = fact.HasAudio
	}
	if fact.PosterMediaID != "" {
		d.PosterMediaID = fact.PosterMediaID
	}
	return d
}

var mediaURLPattern = regexp.MustCompile(`^/api/v1/media/(\d+)(?:[?#].*)?$`)

// resultMediaID 产物的媒体 ID：result 里存了就用；没存的从 outputUrl（/api/v1/media/{id}）取。
func resultMediaID(r SourceResult) string {
	if isDigits(r.MediaID) {
		return r.MediaID
	}
	if m := mediaURLPattern.FindStringSubmatch(r.OutputURL); m != nil {
		return m[1]
	}
	return ""
}

// staticMediaID 发布节点上的静态来源：导演台发布（directorSource）与成片发布（smartcutSource）。
func staticMediaID(params map[string]any) string {
	for _, key := range []string{"directorSource", "smartcutSource"} {
		if src, ok := params[key].(map[string]any); ok {
			if id := digits(src["mediaId"]); id != "" {
				return id
			}
		}
	}
	return ""
}

func digits(v any) string {
	if s, ok := v.(string); ok && isDigits(s) {
		return s
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// paramID 参数里的 ID：字符串原样；整数按十进制（前端 `String(x ?? "")` 对数字给的也是十进制）。
func paramID(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return ""
}

func orDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// ---------------------------------------------------------------- 断开、未使用、指纹

// ClipRef 时间线里的一段（判断「来源已断开」用）。
type ClipRef struct {
	ID           string
	SourceNodeID string
}

// DisconnectedClipIDs 「来源已断开」的片段（SC-F-15）：片段记着来源节点，而那个节点已不在入边里。
// 片段本身保留、不删；没有来源节点的片段（剪辑台里直接加的）不算。按传入顺序返回。
func DisconnectedClipIDs(clips []ClipRef, sources []Source) []string {
	connected := make(map[string]bool, len(sources))
	for _, s := range sources {
		connected[s.NodeID] = true
	}
	out := []string{}
	for _, c := range clips {
		if c.SourceNodeID != "" && !connected[c.SourceNodeID] {
			out = append(out, c.ID)
		}
	}
	return out
}

// UnusedCount 「有 N 段新素材未使用」（SC-F-14）：就绪、能上轨（视频 / 图片 / 音频）、还没被用到的条数。
func UnusedCount(items []Input) int {
	n := 0
	for _, it := range items {
		if it.Status == InputReady && !it.Used && Placeable(it.Kind) {
			n++
		}
	}
	return n
}

// Placeable 能直接放上轨道的素材种类。
func Placeable(kind string) bool {
	return kind == InputVideo || kind == InputImage || kind == InputAudio
}

// InputFingerprint 素材指纹（dev/01 §4.6）：按编号顺序把「编号|媒体 ID|时长」串起来取 sha256。
// 只在 Go 里算（同导演台 DRD-13）。没就绪的素材不计媒体与时长 —— 就绪与否的变化本身就是「素材有变化」。
func InputFingerprint(items []Input) string {
	h := sha256.New()
	for _, it := range items {
		media, dur := "", 0
		if it.Status == InputReady {
			media = it.MediaID
			if it.DurationMs != nil {
				dur = *it.DurationMs
			}
		}
		h.Write([]byte(it.No + "|" + media + "|" + strconv.Itoa(dur) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
