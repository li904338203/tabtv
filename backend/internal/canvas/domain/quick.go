package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 创造页「直接生成」（AGT-42，AG-02）：一次请求建项目 + 建一个节点 + 受理生成。
//
// 本文件只放**纯函数**：操作码怎么选、节点长什么样、项目叫什么。
// 编排（报价 → 建项目 → 受理 → 标失败）在 app/quick.go。

// QuickMode 是创造页输入框的直接生成模式。
type QuickMode string

const (
	QuickVideo QuickMode = "video"
	QuickImage QuickMode = "image"
	QuickAudio QuickMode = "audio"
)

// ErrQuickMode 模式或操作与输入不符。
var ErrQuickMode = errors.New("直接生成的模式与输入不符")

// ParseQuickMode 校验模式。Agent 模式不走这里（它建的是会话，不是节点，见 AGT-43）。
func ParseQuickMode(s string) (QuickMode, error) {
	switch m := QuickMode(s); m {
	case QuickVideo, QuickImage, QuickAudio:
		return m, nil
	}
	return "", fmt.Errorf("%w：不支持的模式 %q", ErrQuickMode, s)
}

// NodeType 模式对应的画布节点类型。三种模式恰好与节点类型同名，
// 但仍走一个函数：将来模式与节点类型不再一一对应时只改这里。
func (m QuickMode) NodeType() string { return string(m) }

// Title 节点默认标题的前半段（前端 addNode 的 `${title}节点 ${index}`）。
func (m QuickMode) Title() string {
	switch m {
	case QuickVideo:
		return "视频"
	case QuickImage:
		return "图片"
	case QuickAudio:
		return "音频"
	}
	return string(m)
}

// OperationFor 按模式、参考图张数与用户显式选的操作，定出受理用的操作码。
//
// ⚠️ 操作码以这里为准，不信客户端：客户端说「图生视频」却一张图都没带，
// 发出去上游会忽略图片、出一段无关的视频**而任务是成功的**（§5.5「提交地址可以随操作变」同一类）。
// 所以显式选择只在「与输入对得上」时采纳，对不上**明确报错**，不悄悄改成另一个操作 ——
// 悄悄改的话用户选的是首尾帧、扣的是图生视频的钱，他看不出为什么。
//
// 规则（卡片 AGT-42 §实施步骤 2）：
//
//	视频：无图 text2video · 1 图 img2video · 2 图 first_last_frame（默认推荐）· 选了全参考 ref2video
//	图片：无图 text2img · 有图 img2img
//	音频：text2audio（不收图）
func OperationFor(mode QuickMode, imageCount int, explicit string) (string, error) {
	if imageCount < 0 {
		imageCount = 0
	}
	explicit = strings.TrimSpace(explicit)
	switch mode {
	case QuickVideo:
		switch explicit {
		case "":
			switch {
			case imageCount == 0:
				return "text2video", nil
			case imageCount == 1:
				return "img2video", nil
			case imageCount == 2:
				return "first_last_frame", nil
			default:
				return "ref2video", nil
			}
		case "text2video":
			if imageCount != 0 {
				return "", fmt.Errorf("%w：文生视频不收参考图，请去掉参考图或改选图生视频", ErrQuickMode)
			}
		case "img2video":
			if imageCount != 1 {
				return "", fmt.Errorf("%w：图生视频需要恰好 1 张参考图，当前 %d 张", ErrQuickMode, imageCount)
			}
		case "first_last_frame":
			if imageCount != 2 {
				return "", fmt.Errorf("%w：首尾帧需要恰好 2 张参考图（第 1 张首帧、第 2 张尾帧），当前 %d 张",
					ErrQuickMode, imageCount)
			}
		case "ref2video":
			if imageCount == 0 {
				return "", fmt.Errorf("%w：全能参考至少需要 1 张参考图", ErrQuickMode)
			}
		default:
			return "", fmt.Errorf("%w：视频不支持操作 %q", ErrQuickMode, explicit)
		}
		return explicit, nil

	case QuickImage:
		want := "text2img"
		if imageCount > 0 {
			want = "img2img"
		}
		if explicit != "" && explicit != want {
			return "", fmt.Errorf("%w：%d 张参考图对应的操作是 %s，不是 %s", ErrQuickMode, imageCount, want, explicit)
		}
		return want, nil

	case QuickAudio:
		if imageCount > 0 {
			return "", fmt.Errorf("%w：音频生成不收参考图", ErrQuickMode)
		}
		if explicit != "" && explicit != "text2audio" {
			return "", fmt.Errorf("%w：音频不支持操作 %q", ErrQuickMode, explicit)
		}
		return "text2audio", nil
	}
	return "", fmt.Errorf("%w：不支持的模式 %q", ErrQuickMode, mode)
}

// 节点默认尺寸 —— **抄自前端** `frontend/stores/canvas.ts` 的 `nodeDefaultWidth`
// （video/script 390、其余 330）与 `document()` 里未测量时的高度 180。
// ⚠️ 两边对不上不会报错，只是节点一打开就被前端按自己的最小宽度撑开、
// 首次自动保存把 size 改掉 —— 无害，但「服务端建的节点与前端建的一样」就不成立了。
const (
	quickNodeWidthVideo = 390
	quickNodeWidth      = 330
	quickNodeHeight     = 180
)

// 节点位置 —— 前端 `newNodePosition(1)`（第一个节点）。
const (
	quickNodeX = 180
	quickNodeY = 100
)

// QuickNodeID 节点 ID，形状与前端 `addNode` 相同（`n_<base36 毫秒>_<序号>`）。
// 序号恒为 1：直接生成只建一个节点。
func QuickNodeID(unixMilli int64) string {
	return "n_" + strconv.FormatInt(unixMilli, 36) + "_1"
}

// BuildQuickNode 产出与前端 `document()` **逐字段一致**的存储节点：
// `{id, type:<nodeType>, title, position, size:{w,h}, params}`。
//
// ⚠️ 是**持久化形状**（`type` 是节点类型、`params` 在顶层），不是 Vue Flow 运行时形状
// （`type:"aitv"`、`data:{nodeType,...}`）。写成后者的话，画布打开时整个节点被当成未知类型。
//
// ⚠️ 不写 `result`：受理在建项目之后，结果由 app 层按受理结果补上（成功 → 跟踪中，失败 → 原话）。
// 建项目时先写一个 queued 的话，进程恰好在受理前崩掉，节点会永远显示「排队中」而没有任务可查。
func BuildQuickNode(nodeID string, mode QuickMode, title string, params json.RawMessage) (json.RawMessage, error) {
	w := quickNodeWidth
	if mode == QuickVideo {
		w = quickNodeWidthVideo
	}
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(params, &probe); err != nil || probe == nil {
		return nil, fmt.Errorf("params 必须是 JSON 对象")
	}
	node := struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Title    string `json:"title"`
		Position struct {
			X int `json:"x"`
			Y int `json:"y"`
		} `json:"position"`
		Size struct {
			W int `json:"w"`
			H int `json:"h"`
		} `json:"size"`
		Params json.RawMessage `json:"params"`
	}{ID: nodeID, Type: mode.NodeType(), Title: title, Params: params}
	node.Position.X, node.Position.Y = quickNodeX, quickNodeY
	node.Size.W, node.Size.H = w, quickNodeHeight
	return json.Marshal(node)
}

// QuickCanvas 把单个节点包成一份画布文档（`schemaVersion:1`，与前端 `blankDocument()` 同形）。
func QuickCanvas(node json.RawMessage) json.RawMessage {
	out, _ := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"viewport":      map[string]any{"x": 0, "y": 0, "zoom": 1},
		"nodes":         []json.RawMessage{node},
		"edges":         []any{},
	})
	return out
}

// QuickNodeTitle 节点标题（前端 addNode：`${title}节点 1`）。
func QuickNodeTitle(mode QuickMode) string { return mode.Title() + "节点 1" }

// QuickProjectNameLen 项目名取正文前多少个字（设计 02 §7）。
const QuickProjectNameLen = 20

// QuickProjectName 项目名 = 正文前 20 字；空则「未命名创作」。
//
// 换行与连续空白折成一个空格：正文是多行的，原样截出来的名字在卡片上会断成两行。
func QuickProjectName(text string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.TrimSpace(text) {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		space = false
		b.WriteRune(r)
	}
	name := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(name) > QuickProjectNameLen {
		name = strings.TrimSpace(string([]rune(name)[:QuickProjectNameLen]))
	}
	if name == "" {
		return "未命名创作"
	}
	return name
}

// QuickQuantities 从参数里取计价用的数量与时长 —— 与前端 `generationQuantities` 同一条规则：
// count 默认 1；时长只对视频有意义（`params.duration`），没有就不传（报价按单位默认 5 秒）。
//
// ⚠️ 选项值一律是字符串（`"4"`、`"10"`）。只认数字的话「4 张」按 1 张扣费、上游出 4 张。
func QuickQuantities(mode QuickMode, params json.RawMessage) (count, durationSec int) {
	var m map[string]any
	_ = json.Unmarshal(params, &m)
	count = positiveInt(m["count"])
	if count == 0 {
		count = 1
	}
	if mode == QuickVideo {
		durationSec = positiveInt(m["duration"])
	}
	return count, durationSec
}

func positiveInt(v any) int {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		p, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0
		}
		f = p
	default:
		return 0
	}
	if f < 1 || f > 1<<30 {
		return 0
	}
	return int(f)
}
