package main

// 智能剪辑的 AI 剪辑 / 追加指令 / 翻译（docs/agent/smartcut SCT-42）。
//
// 这类请求的系统提示词里一定有输出契约的标题 `## 输出格式（智能剪辑）`，第二行是模式码
// （`模式：mix（素材混剪）`，smartcut/domain.OutputContract，代码固定追加）。看到它就按 user 消息里的
// 素材清单 / 识别稿 / 片段清单 / 字幕**当场拼一份合规的回包**：
//   - mix：每个视频 / 图片编号整段用一次（转场一律 cut），第一个当钩子、最后一个当结尾；有配乐就用第一条；
//   - board：不动镜头，只给第一镜写一句字幕；
//   - talk：保留全部句子，只删「和前面某句一模一样」的重复句（口癖由系统按字级时间剪）；
//   - narr：每句按顺序轮流配一段画面；
//   - edit：有两段以上就交换 c1 与 c2，否则全部改成 1.25 倍速；
//   - translate：每条译成「[语言码] 原文」（原文带 `#译不出` 的那条跳过）。
//
// ⚠️ 不读固定 fixture：素材的编号与条数随 E2E 的数据变，写死的回包只能对上一种数据 —— 回包与断言对不上时，
//    红的是断言而查起来像产品（DRT-30 的教训）。拼法写在这里，E2E 的断言按同一个拼法写。
// ⚠️ 必须**先于**「分镜」启发式判断（同导演台，见 director.go 文件头）。
//
// 魔法词（写在 user 文本里，即剪辑要求）：
//   - `#剪辑解析失败`：回一段不是 JSON 的文字（服务端让模型修一次，仍不合规时退回确定性方案）；
//   - `#剪辑慢`：先等 3 秒再回（验同时只能跑一个，42902）；
//   - 字幕原文里有 `#译不出`：翻译时跳过那一条（验部分失败时说清是哪几条、原文不动，SC-AC-53）。

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	smartcutContractHeading = "## 输出格式（智能剪辑）"
	magicCutParseFail       = "#剪辑解析失败"
	magicCutSlow            = "#剪辑慢"
	magicCutNoTranslate     = "#译不出"
	smartcutProbeKeep       = 50
)

// smartcutSlowDelay 是 `#剪辑慢` 的等待时长（测试里调短）。
var smartcutSlowDelay = 3 * time.Second

var (
	reCutMode     = regexp.MustCompile(`模式：([a-z]+)`)
	reCutCard     = regexp.MustCompile(`(?m)^- 编号 (\S+?)：(视频|图片|音频（配乐）|音频（人声）)`)
	reCutSentence = regexp.MustCompile(`(?m)^- (s\d+)（[^）]*）：(.*)$`)
	reCutShot     = regexp.MustCompile(`(?m)^- (b\d+)：`)
	reCutEditClip = regexp.MustCompile(`(?m)^- (c\d+)：`)
	reCutSubtitle = regexp.MustCompile(`(?m)^(\d+)\. (.*)$`)
	reCutLang     = regexp.MustCompile(`（([a-z]{2})）`)
	reCutMosaic   = regexp.MustCompile(`第 \d+ 行是编号 ([^，\s]+)`)
)

// isSmartcutPlan 判断是不是智能剪辑的请求（系统提示词里有输出契约的标题）。
func isSmartcutPlan(system string) bool { return strings.Contains(system, smartcutContractHeading) }

// smartcutMode 系统提示词第二行的模式码（mix / board / talk / narr / edit / translate）。
func smartcutMode(system string) string {
	i := strings.Index(system, smartcutContractHeading)
	if i < 0 {
		return ""
	}
	if m := reCutMode.FindStringSubmatch(system[i:]); m != nil {
		return m[1]
	}
	return ""
}

// smartcutReply 决定回什么：回复文本与要先等多久。
func smartcutReply(system, user string) (string, time.Duration) {
	if strings.Contains(user, magicCutParseFail) {
		return "这一次没想好怎么剪：大概是把最好看的那段放在开头，后面接产品特写。", 0
	}
	var delay time.Duration
	if strings.Contains(user, magicCutSlow) {
		delay = smartcutSlowDelay
	}
	var reply any
	switch smartcutMode(system) {
	case "mix":
		reply = mixReply(user)
	case "board":
		reply = boardReply(user)
	case "talk":
		reply = talkReply(user)
	case "narr":
		reply = narrReply(user)
	case "edit":
		reply = editReply(user)
	case "translate":
		reply = translateReply(user)
	default:
		return "认不出的智能剪辑模式", delay
	}
	b, _ := json.Marshal(reply)
	return string(b), delay
}

// cutCards 素材清单里的编号：画面（视频 / 图片）与配乐，按出现顺序。
func cutCards(user string) (pictures, music []string) {
	for _, m := range reCutCard.FindAllStringSubmatch(user, -1) {
		switch m[2] {
		case "视频", "图片":
			pictures = append(pictures, m[1])
		case "音频（配乐）":
			music = append(music, m[1])
		}
	}
	return pictures, music
}

// clipNotes 有拼图时给每段素材写一句画面描述（服务端要求在 clipNotes 里描述看到的画面）。
func clipNotes(user string) map[string]string {
	notes := map[string]string{}
	for _, m := range reCutMosaic.FindAllStringSubmatch(user, -1) {
		notes[m[1]] = "假上游看到的第 " + m[1] + " 段画面"
	}
	return notes
}

func mixReply(user string) map[string]any {
	pictures, music := cutCards(user)
	segs := make([]map[string]any, 0, len(pictures))
	for i, no := range pictures {
		// 转场一律 cut：溶解 / 淡入淡出会吃掉两段之间的重叠，成片比素材总长短一截 —— SC-AC-38 的「素材共 14.x 秒，
		// 不足目标 20 秒」说的是素材总长，转场一多 E2E 就对不上那句话（SCT-42 首跑：7 段 2 秒剪出 11 秒）。
		role, weight, transition := "body", 2, "cut"
		switch {
		case i == 0:
			role, weight = "hook", 3
		case i == len(pictures)-1:
			role = "end"
		}
		segs = append(segs, map[string]any{"clip": no, "take": "full", "role": role, "weight": weight,
			"transition": transition, "reason": "假上游：编号 " + no + " 整段用一次"})
	}
	out := map[string]any{"segments": segs, "notes": []string{"假上游的固定剪法：每段整段用一次"}}
	if len(music) > 0 {
		out["music"] = map[string]any{"clip": music[0]}
	}
	if notes := clipNotes(user); len(notes) > 0 {
		out["clipNotes"] = notes
	}
	return out
}

func boardReply(user string) map[string]any {
	shots := reCutShot.FindAllStringSubmatch(user, -1)
	segs := []map[string]any{}
	if len(shots) > 0 {
		segs = append(segs, map[string]any{"clip": shots[0][1], "transition": "cut", "subtitle": "假上游的开场字幕"})
	}
	return map[string]any{"segments": segs}
}

func talkReply(user string) map[string]any {
	var keep, drop []map[string]any
	seen := map[string]bool{}
	from, to := "", ""
	flush := func() {
		if from != "" {
			keep = append(keep, map[string]any{"from": from, "to": to})
		}
		from, to = "", ""
	}
	for _, m := range reCutSentence.FindAllStringSubmatch(user, -1) {
		text := strings.TrimSpace(m[2])
		if seen[text] {
			flush()
			drop = append(drop, map[string]any{"sentence": m[1], "reason": "与前面一句重复"})
			continue
		}
		seen[text] = true
		if from == "" {
			from = m[1]
		}
		to = m[1]
	}
	flush()
	if drop == nil {
		drop = []map[string]any{}
	}
	return map[string]any{"keep": keep, "drop": drop, "subtitle": true}
}

func narrReply(user string) map[string]any {
	pictures, music := cutCards(user)
	lines := []map[string]any{}
	for i, m := range reCutSentence.FindAllStringSubmatch(user, -1) {
		if len(pictures) == 0 {
			break
		}
		lines = append(lines, map[string]any{"sentence": m[1], "clips": []string{pictures[i%len(pictures)]}, "take": "head"})
	}
	out := map[string]any{"lines": lines}
	if len(music) > 0 {
		out["music"] = map[string]any{"clip": music[0]}
	}
	return out
}

func editReply(user string) map[string]any {
	clips := reCutEditClip.FindAllStringSubmatch(user, -1)
	if len(clips) >= 2 {
		return map[string]any{"ops": []any{map[string]any{"op": "swap", "a": clips[0][1], "b": clips[1][1]}},
			"summary": "交换了 " + clips[0][1] + " 与 " + clips[1][1]}
	}
	return map[string]any{"ops": []any{map[string]any{"op": "speed", "clip": "*", "value": 1.25}},
		"summary": "全部改成 1.25 倍速"}
}

func translateReply(user string) map[string]any {
	lang := "xx"
	if m := reCutLang.FindStringSubmatch(user); m != nil {
		lang = m[1]
	}
	items := []map[string]any{}
	for _, m := range reCutSubtitle.FindAllStringSubmatch(user, -1) {
		if strings.Contains(m[2], magicCutNoTranslate) {
			continue // 这一条「没译出来」：服务端应把它列进 failed、原文不动（SC-AC-53）
		}
		n, _ := strconv.Atoi(m[1])
		items = append(items, map[string]any{"n": n, "text": "[" + lang + "] " + strings.TrimSpace(m[2])})
	}
	return map[string]any{"items": items}
}

// serveSmartcutPlan 回一次智能剪辑请求（OpenAI Chat 形状），并记进探针。
func (s *server) serveSmartcutPlan(w http.ResponseWriter, in directorBuildInput, write func(string)) {
	mode := smartcutMode(in.System)
	text, delay := smartcutReply(in.System, in.User)
	magic := ""
	for _, m := range []string{magicCutParseFail, magicCutSlow} {
		if strings.Contains(in.User, m) {
			magic = m
		}
	}
	s.smartcutProbe.record(smartcutPlanRecord{Mode: mode, Model: in.Model, Images: in.Images,
		DataImages: in.DataImages, HTTPImages: in.HTTPImages, URLs: in.URLs, Magic: magic})
	log.Printf("  收到智能剪辑请求（%s，%d 张图，其中内联 %d 张）%s", mode, in.Images, in.DataImages, magic)
	if delay > 0 {
		time.Sleep(delay)
	}
	write(text)
}

// ---------------------------------------------------------------- 探针

// smartcutPlanRecord 是一次智能剪辑请求的记录（E2E 第 283 步核对看图的拼图都是内联、没有外链，SC-AC-37）。
type smartcutPlanRecord struct {
	Seq        int64    `json:"seq"`
	Mode       string   `json:"mode"`
	Model      string   `json:"model"`
	Images     int      `json:"images"`
	DataImages int      `json:"dataImages"`
	HTTPImages int      `json:"httpImages"`
	URLs       []string `json:"urls"`
	Magic      string   `json:"magic"`
	At         string   `json:"at"`
}

type smartcutPlanProbe struct {
	mu   sync.Mutex
	seq  atomic.Int64
	recs []smartcutPlanRecord
}

func (p *smartcutPlanProbe) record(r smartcutPlanRecord) {
	r.Seq = p.seq.Add(1)
	r.At = time.Now().UTC().Format(time.RFC3339Nano)
	if r.URLs == nil {
		r.URLs = []string{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recs = append(p.recs, r)
	if len(p.recs) > smartcutProbeKeep {
		p.recs = p.recs[len(p.recs)-smartcutProbeKeep:]
	}
}

// list 最近的记录（新的在前）；mode 非空时只要这种模式，limit ≤ 0 不限。
func (p *smartcutPlanProbe) list(mode string, limit int) []smartcutPlanRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []smartcutPlanRecord{}
	for i := len(p.recs) - 1; i >= 0; i-- {
		if mode != "" && p.recs[i].Mode != mode {
			continue
		}
		out = append(out, p.recs[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// probeSmartcut 是 GET /_probe/smartcut?mode=&limit=：最近的智能剪辑请求（只记形态，不记正文）。
func (s *server) probeSmartcut(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, map[string]any{"items": s.smartcutProbe.list(r.URL.Query().Get("mode"), limit)})
}
