package main

// 3D 导演台的 AI 搭景（DRT-30）。
//
// 搭景请求的系统提示词里一定有输出契约的标题 `## 输出格式（导演台搭景）`（代码固定追加，
// `director/domain.ContractHeading`）。看到它就回 `api/fixtures/director/build/fake-reply.json`：
// 那是一份按输出契约写的方案，在默认场景上求解出 3 个角色、1 个机位（与 case-default-scene-standoff 同一份 draft）。
//
// ⚠️ 必须**先于**「分镜」启发式判断：那条启发式只要系统提示词里出现「分镜」二字就回分镜 JSON，
//    运营哪天在搭景模板里写进「分镜」，每次搭景都会变成 42216 —— 而看上去像解析器坏了。
// ⚠️ 回包读不到就报错，**不回兜底**：随手编一份方案的话，求解结果对不上 E2E 的断言，
//    红的是断言而查起来像产品。
//
// 魔法词（写在 user 文本里）：
//   - `#搭景解析失败`：回一段不是 JSON 的文字，验 42216；
//   - `#搭景慢`：先等 3 秒再回，验同时进行的次数限制（42902）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	directorBuildMarker = "## 输出格式（导演台搭景）"
	magicBuildParseFail = "#搭景解析失败"
	magicBuildSlow      = "#搭景慢"
	directorReplyRel    = "api/fixtures/director/build/fake-reply.json"
	directorProbeKeep   = 50
)

// directorSlowDelay 是 `#搭景慢` 的等待时长（测试里调短）。
var directorSlowDelay = 3 * time.Second

// directorBuildInput 是从一次搭景请求里取出的、与协议无关的部分。
type directorBuildInput struct {
	Protocol string
	Model    string
	System   string
	User     string
	// Images：图片块总数；DataImages / HTTPImages：其中以 data: 内联、以 http(s) 外链发出的张数。
	Images     int
	DataImages int
	HTTPImages int
	URLs       []string
}

// isDirectorBuild 判断是不是搭景请求（系统提示词里有搭景输出契约的标题）。
func isDirectorBuild(system string) bool { return strings.Contains(system, directorBuildMarker) }

// directorBuildReply 决定回什么：回复文本与要先等多久。没有回包时报错（不回兜底）。
func directorBuildReply(in directorBuildInput, fixture []byte) (string, time.Duration, error) {
	if strings.Contains(in.User, magicBuildParseFail) {
		return "这一次没想好怎么摆：大概是两个人面对面站着，机位放在侧面。", 0, nil
	}
	var delay time.Duration
	if strings.Contains(in.User, magicBuildSlow) {
		delay = directorSlowDelay
	}
	if len(fixture) == 0 {
		return "", 0, errors.New("假上游没有导演台搭景回包：" + directorReplyRel + " 不存在（用 -director-reply 指定）")
	}
	return string(fixture), delay, nil
}

// findDirectorReply 从当前目录与可执行文件所在目录往上找仓库里的回包；找不到返回空串。
func findDirectorReply() string {
	starts := []string{}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, start := range starts {
		for dir := start; ; dir = filepath.Dir(dir) {
			p := filepath.Join(dir, directorReplyRel)
			if _, err := os.Stat(p); err == nil {
				return p
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return ""
}

// loadDirectorReply 读回包并确认是 JSON（读进来的是坏文件的话，在启动时就说清楚）。
func loadDirectorReply(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("找不到 " + directorReplyRel)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("%s 不是合法 JSON", path)
	}
	return b, nil
}

// openAIDirectorInput 从 OpenAI Chat 的消息里取搭景要用的部分（content 可能是字符串，也可能是分块数组）。
func openAIDirectorInput(model string, messages []struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}, body []byte) directorBuildInput {
	in := directorBuildInput{Protocol: "openai_chat", Model: model, URLs: bodyURLs(body)}
	var system, user []string
	for _, m := range messages {
		texts, urls := contentParts(m.Content)
		for _, u := range urls {
			in.countImage(u)
		}
		switch m.Role {
		case "system":
			system = append(system, texts...)
		case "user":
			user = append(user, texts...)
		}
	}
	in.System, in.User = strings.Join(system, "\n"), strings.Join(user, "\n")
	return in
}

func (in *directorBuildInput) countImage(url string) {
	in.Images++
	switch {
	case strings.HasPrefix(url, "data:"):
		in.DataImages++
	case strings.HasPrefix(url, "http://"), strings.HasPrefix(url, "https://"):
		in.HTTPImages++
	}
}

// contentParts 取一条消息里的文字与图片地址：OpenAI 的 `image_url.url`、Anthropic 的 `source`（base64 记成 data:）。
func contentParts(content any) (texts, images []string) {
	switch c := content.(type) {
	case string:
		return []string{c}, nil
	case []any:
		for _, p := range c {
			part, _ := p.(map[string]any)
			switch part["type"] {
			case "text":
				if t, ok := part["text"].(string); ok {
					texts = append(texts, t)
				}
			case "image_url":
				if iu, ok := part["image_url"].(map[string]any); ok {
					u, _ := iu["url"].(string)
					images = append(images, u)
				}
			case "image":
				src, _ := part["source"].(map[string]any)
				switch src["type"] {
				case "base64":
					mt, _ := src["media_type"].(string)
					images = append(images, "data:"+mt+";base64,")
				case "url":
					u, _ := src["url"].(string)
					images = append(images, u)
				}
			}
		}
	}
	return texts, images
}

// ---------------------------------------------------------------- 探针

// directorBuildRecord 是一次搭景请求的记录（E2E 第 265 步核对参考图都是内联、没有外链，DR-AC-33）。
type directorBuildRecord struct {
	Seq        int64    `json:"seq"`
	Protocol   string   `json:"protocol"`
	Model      string   `json:"model"`
	Images     int      `json:"images"`
	DataImages int      `json:"dataImages"`
	HTTPImages int      `json:"httpImages"`
	URLs       []string `json:"urls"`
	Marker     bool     `json:"marker"`
	Magic      string   `json:"magic"`
	At         string   `json:"at"`
}

type directorProbe struct {
	mu   sync.Mutex
	seq  atomic.Int64
	recs []directorBuildRecord
}

func (p *directorProbe) record(in directorBuildInput) {
	magic := ""
	switch {
	case strings.Contains(in.User, magicBuildParseFail):
		magic = "parse_fail"
	case strings.Contains(in.User, magicBuildSlow):
		magic = "slow"
	}
	urls := append([]string{}, in.URLs...)
	rec := directorBuildRecord{Seq: p.seq.Add(1), Protocol: in.Protocol, Model: in.Model, Images: in.Images,
		DataImages: in.DataImages, HTTPImages: in.HTTPImages, URLs: urls, Marker: isDirectorBuild(in.System),
		Magic: magic, At: time.Now().UTC().Format(time.RFC3339Nano)}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recs = append(p.recs, rec)
	if len(p.recs) > directorProbeKeep {
		p.recs = append([]directorBuildRecord(nil), p.recs[len(p.recs)-directorProbeKeep:]...)
	}
}

// list 返回最近 limit 条（旧在前）；model 非空时只看这个模型的。
func (p *directorProbe) list(model string, limit int) []directorBuildRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []directorBuildRecord{}
	for _, r := range p.recs {
		if model == "" || r.Model == model {
			out = append(out, r)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (p *directorProbe) handle(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, map[string]any{"items": p.list(r.URL.Query().Get("model"), limit)})
}

// ---------------------------------------------------------------- 回复

// serveDirectorBuild 记探针、按魔法词决定回复，再按协议的形状写出去。
// 读不到回包时回 500 并说清缺什么（不回兜底）。
func (s *server) serveDirectorBuild(w http.ResponseWriter, in directorBuildInput, write func(text string)) {
	s.directorProbe.record(in)
	text, delay, err := directorBuildReply(in, s.directorReply)
	if err != nil {
		log.Printf("  导演台搭景：%v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("  导演台搭景：model=%s 图 %d 张（内联 %d、外链 %d），等 %v", in.Model, in.Images, in.DataImages, in.HTTPImages, delay)
	if delay > 0 {
		time.Sleep(delay)
	}
	write(text)
}
