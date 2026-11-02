// Command fake-upstream 是联调用的**假上游**。
//
// 它不是 dev_fake 适配器的替代品：dev_fake 在进程内短路，
// 而这个进程真的在 HTTP 上说话 —— 因此它能验到 dev_fake 验不到的东西：
// 请求真的拼对了没有、认证头带没带、产物真的下载得回来没有、
// 请求日志落了没有、跨源下载会不会把密钥漏出去。
//
// ⚠️ 只用于本地联调与 P06-04 的矩阵回归，**不进任何部署产物**。
// 真实凭据的联调见 docs/00 §7 的 PARAM-04。
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// storyboardJSON 是一份形状正确的分镜产出。
//
// ⚠️ 故意**裹在代码块围栏里**并带一个尾随逗号：真实模型经常这么干，
// 而容错解析器就是为此存在的。形状正确但"不干净"才是真实输入。
const storyboardJSON = "```json\n" + `{"globalStyle":"写实电影感",
 "shots":[
  {"shotNo":1,"description":"@林澈 站在天台边缘俯瞰城市","durationSec":6,
   "shotSize":"全景","lighting":"黄昏逆光","cameraMove":"缓慢推近",
   "startState":"人物背对镜头静立","endState":"人物转头看向镜头",
   "entities":[{"name":"林澈","type":"character"},{"name":"天台","type":"scene"}]},
  {"shotNo":2,"description":"手中的@旧怀表 指针停在三点","durationSec":4,
   "shotSize":"特写","lighting":"侧光","cameraMove":"固定",
   "startState":"怀表合着","endState":"怀表被打开",
   "entities":[{"name":"旧怀表","type":"prop"}]},
  {"shotNo":3,"description":"@林澈 转身走入楼梯间","durationSec":5,
   "shotSize":"中景","lighting":"昏暗","cameraMove":"跟随",
   "startState":"人物起步","endState":"人物没入阴影",
   "entities":[{"name":"林澈","type":"character"}],},
 ],
 "assets":[
  {"name":"林澈","type":"character","appearance":"三十岁男性，短发，风衣",
   "costume":"深色风衣与白衬衫","features":"左眉有一道旧疤"},
  {"name":"天台","type":"scene","appearance":"老旧居民楼顶","ambience":"黄昏，风大"},
  {"name":"旧怀表","type":"prop","appearance":"黄铜壳，表面有划痕"}]}` + "\n```"

// onePixelPNG 是一张 1×1 的 PNG（透明像素），base64 来自公开的最小合法 PNG。
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

type server struct {
	addr string
	// asyncTasks 记录异步任务被查询了几次，用来模拟"先排队后成功"。
	asyncTasks sync.Map
	seq        atomic.Int64
	// lastAuth 记下最近一次下载请求带没带认证头，供脚本断言。
	lastDownloadAuth atomic.Value
	// gated 门控异步视频（AGT-31）：任务 ID → 是否已放行，见 agent_script.go。
	gated sync.Map
	// agentProbe 记录 Agent 请求里解析出的工具名（AGT-31），见 agent_script.go。
	agentProbe agentProbe
	// directorReply 是导演台搭景的固定回包、directorProbe 记录搭景请求（DRT-30），见 director.go。
	directorReply []byte
	directorProbe directorProbe
	// asr 智能剪辑的语音识别（SCT-33）：请求记录、下一次的结果、标准版的任务，见 smartcut_asr.go。
	asr asrState
	// clipGen 智能剪辑的片段级生成（SCT-38~39）：去字幕的任务与请求记录，见 smartcut_media.go。
	clipGen clipGenState
	// smartcutProbe 记录智能剪辑的 AI 剪辑 / 追加指令 / 翻译请求（SCT-42），见 smartcut.go。
	smartcutProbe smartcutPlanProbe
	// imageEdits 记录图片编辑接口的请求（IMGT-19），见 imageedit.go。
	imageEdits imageEditProbe
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8195", "监听地址")
	replyPath := flag.String("director-reply", "", "导演台搭景回包（默认从当前目录往上找 "+directorReplyRel+"）")
	flag.Parse()

	s := &server{addr: "http://" + *addr}
	s.lastDownloadAuth.Store("")
	if *replyPath == "" {
		*replyPath = findDirectorReply()
	}
	if b, err := loadDirectorReply(*replyPath); err != nil {
		// 不退出：别的段落照常能跑；搭景请求会回 500 并说清缺什么（不回兜底）。
		log.Printf("⚠️ 导演台搭景回包不可用：%v", err)
	} else {
		s.directorReply = b
		log.Printf("导演台搭景回包：%s", *replyPath)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.chat)
	// 推理模型（AGT-04）：Anthropic 形状的流式 + tool_use。OpenAI 形状复用上面那个入口
	// （请求带 stream 或 tools 时由 openAIAgent 接管，见 agent.go）。
	mux.HandleFunc("/v1/messages", s.anthropicMessages)
	mux.HandleFunc("/v1/images/generations", s.images)
	mux.HandleFunc("/v1/images/edits", s.imageEdit)
	// 图片节点工具条（IMGT-19）：同一个编辑接口，回与收到的图同尺寸的纯红 PNG（按 n 回多张），见 imageedit.go。
	mux.HandleFunc("/v1/imagetools/images/edits", s.imageEdit)
	mux.HandleFunc("/_probe/image-edits", s.imageEdits.handle)
	mux.HandleFunc("/v1/audio/speech", s.tts)
	mux.HandleFunc("/v1/async/submit", s.asyncSubmit)
	// 四家视频协议：形状互不相同，假上游按各自的形状校验一遍再回。
	// ⚠️ 只校验**形状**不校验内容：这里要证明的是"我们拼对了"，
	// 不是"上游会生成什么"。
	mux.HandleFunc("/v1/videos/text-to-video/generations", s.klingSubmit)
	mux.HandleFunc("/v1/videos/image-to-video/generations", s.klingSubmit)
	mux.HandleFunc("/v1/videos/omni-video/generations", s.klingSubmit)
	mux.HandleFunc("/v1/kling/query/", s.klingQuery)
	mux.HandleFunc("/v1/seedance/submit", s.seedanceSubmit)
	mux.HandleFunc("/v1/seedance/query/", s.seedanceQuery)
	mux.HandleFunc("/v1/async/query/", s.asyncQuery)
	// 门控异步视频（AGT-31）：一直「处理中」直到 /_probe/gated/release 放行。
	mux.HandleFunc("/v1/gated/submit", s.gatedSubmit)
	mux.HandleFunc("/v1/gated/query/", s.gatedQuery)
	mux.HandleFunc("/_probe/gated/release", s.gatedRelease)
	// 画布 Agent 请求里解析出的工具名（AGT-31）：断言「规划态没有付费 / 画布写工具」只能靠它，
	// Provider 请求日志是截断过的（见 agent_script.go 文件头）。
	mux.HandleFunc("/_probe/agent-tools", s.agentProbe.handle)
	// 导演台搭景请求（DRT-30）：模型名、图片块数、内联与外链各几张、有没有搭景标记。
	mux.HandleFunc("/_probe/director-build", s.directorProbe.handle)
	// 智能剪辑的语音识别（SCT-33）：火山极速版 / 标准版、OpenAI 转写，见 smartcut_asr.go。
	mux.HandleFunc("/api/v3/auc/bigmodel/recognize/flash", s.volcFlash)
	mux.HandleFunc("/api/v3/auc/bigmodel/submit", s.volcSubmit)
	mux.HandleFunc("/api/v3/auc/bigmodel/query", s.volcQuery)
	mux.HandleFunc("/v1/audio/transcriptions", s.openAITranscribe)
	mux.HandleFunc("/_probe/asr", s.asr.handleProbe)
	mux.HandleFunc("/_probe/asr/next", s.asr.handleNext)
	// 智能剪辑的片段级生成（SCT-38~39）：火山 MediaKit 去字幕，见 smartcut_media.go。
	mux.HandleFunc("/api/v1/tools/erase-video-subtitle-pro", s.mediaKitErase)
	mux.HandleFunc("/api/v1/tasks/", s.mediaKitTask)
	// 配乐与音效（SCT-39）：豆包音乐挂在 /volc/ 下（Action 风格），ElevenLabs、可灵视频配音效按各自的路径。
	mux.HandleFunc("/volc/", s.volcMusic)
	mux.HandleFunc("/v1/music", s.elevenLabsMusic)
	mux.HandleFunc("/v1/audio/video-to-audio", s.klingV2A)
	mux.HandleFunc("/v1/audio/video-to-audio/", s.klingV2A)
	mux.HandleFunc("/_probe/smartcut", s.probeSmartcut)
	mux.HandleFunc("/_probe/smartcut/media", s.clipGen.handleProbe)
	mux.HandleFunc("/_probe/smartcut/media/next", s.clipGen.handleNext)
	mux.HandleFunc("/files/", s.download)
	// 故障注入：真实联调矩阵要证明**失败路径也走对了**，而不是只证明成功路径。
	// ⚠️ 故障必须可复现、可指名 —— 随机失败会让一次红色结果无法定位。
	mux.HandleFunc("/v1/fail/permanent", s.failPermanent)
	mux.HandleFunc("/v1/fail/ratelimit", s.failRateLimit)
	mux.HandleFunc("/v1/fail/empty", s.failEmpty)
	mux.HandleFunc("/v1/fail/hang", s.failHang)
	mux.HandleFunc("/v1/fail/task", s.failTaskSubmit)
	mux.HandleFunc("/v1/fail/task/query/", s.failTaskQuery)
	// 假支付渠道（P11-02）。⚠️ 密钥都是**进程启动时现生成的**，只发给本机；
	// 真实商户凭据是 PARAM-03，永远不进仓库。
	pay := newPayState()
	mux.HandleFunc("/payment/keys", pay.payKeys)
	mux.HandleFunc("/payment/wechat/v3/pay/transactions/native", pay.wechatNative)
	mux.HandleFunc("/payment/wechat/v3/pay/transactions/out-trade-no/", pay.wechatQuery)
	mux.HandleFunc("/payment/wechat/v3/refund/domestic/refunds", pay.wechatRefund)
	mux.HandleFunc("/payment/alipay/gateway", pay.alipayGateway)
	// 故障注入：让某一单的退款失败（验"渠道拒绝时权益已回收"那条路）。
	mux.HandleFunc("/payment/fail-refund", pay.payFailRefund)
	mux.HandleFunc("/payment/refund-count", pay.payRefundCount)
	// 由脚本触发一条**真实形状**的回调（签名 + 对微信还要 AES-GCM 加密）。
	mux.HandleFunc("/payment/fire/wechat", pay.fireWechatCallback)
	mux.HandleFunc("/payment/fire/alipay", pay.fireAlipayCallback)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	// 供脚本核对"下载时到底带没带认证头"。
	mux.HandleFunc("/_probe/last-download-auth", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"auth":%q}`, s.lastDownloadAuth.Load())
	})

	log.Printf("假上游已启动 %s", s.addr)
	srv := &http.Server{Addr: *addr, Handler: logging(mux)}
	if err := srv.ListenAndServe(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s auth=%q", r.Method, r.URL.Path, mask(r.Header.Get("Authorization")))
		next.ServeHTTP(w, r)
	})
}

// mask 只留前 8 位：日志里出现完整密钥本身就是泄漏。
func mask(s string) string {
	if len(s) > 8 {
		return s[:8] + "***"
	}
	return s
}

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)

	// 导演台搭景（DRT-30）：系统提示词里有搭景输出契约的标题就回固定方案。
	// ⚠️ 排在最前：既先于推理模型的判断，也先于下面的「分镜」启发式（见 director.go 文件头）。
	if in := openAIDirectorInput(req.Model, req.Messages, body); isDirectorBuild(in.System) {
		s.serveDirectorBuild(w, in, func(text string) {
			writeJSON(w, map[string]any{"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}}})
		})
		return
	} else if isSmartcutPlan(in.System) {
		// 智能剪辑的 AI 剪辑 / 追加指令 / 翻译（SCT-42）：同样排在推理模型与「分镜」启发式之前（见 smartcut.go 文件头）。
		s.serveSmartcutPlan(w, in, func(text string) {
			writeJSON(w, map[string]any{"choices": []any{map[string]any{
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}}})
		})
		return
	}

	// 流式或带工具的请求（推理模型，AGT-04）交给 openAIAgent；
	// 其余（脚本线的单轮同步调用）行为不变。
	if s.openAIAgent(w, body) {
		return
	}

	// 系统提示词里带「分镜」二字时回一份合法的分镜 JSON，
	// 好让脚本链路能端到端跑通。⚠️ 这不是在模拟模型的创作能力，
	// 只是给解析器一个**形状正确**的输入 —— 解析、扣费、落库那几步才是要验的。
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		if s, ok := m.Content.(string); ok && strings.Contains(s, "分镜") {
			log.Printf("  收到分镜请求，回一份 3 镜的分镜 JSON")
			writeJSON(w, map[string]any{"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": storyboardJSON},
			}}})
			return
		}
	}

	text := fmt.Sprintf("假上游回应（model=%s，收到 %d 条消息，%d 字节）",
		req.Model, len(req.Messages), len(body))
	writeJSON(w, map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
	})
}

func (s *server) images(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		N int `json:"n"`
	}
	_ = json.Unmarshal(body, &req)
	if req.N <= 0 {
		req.N = 1
	}
	data := make([]any, 0, req.N)
	for i := 0; i < req.N; i++ {
		id := s.seq.Add(1)
		// ⚠️ 故意回 URL 而不是 base64：URL 形态才会走下载与同源判定这条路。
		data = append(data, map[string]any{"url": fmt.Sprintf("%s/files/img-%d.png", s.addr, id)})
	}
	writeJSON(w, map[string]any{"data": data})
}

func (s *server) tts(w http.ResponseWriter, _ *http.Request) {
	// ⚠️ 裸字节，不是 JSON —— 按 JSON 解析会失败，且失败信息指不到真正的原因。
	w.Header().Set("Content-Type", "audio/mpeg")
	_, _ = w.Write([]byte("ID3\x03\x00\x00\x00假音频字节"))
}

func (s *server) asyncSubmit(w http.ResponseWriter, _ *http.Request) {
	id := fmt.Sprintf("task-%d", s.seq.Add(1))
	s.asyncTasks.Store(id, new(atomic.Int64))
	writeJSON(w, map[string]any{"task_id": id})
}

func (s *server) asyncQuery(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/v1/async/query/"):]
	v, ok := s.asyncTasks.Load(id)
	if !ok {
		http.Error(w, "任务不存在", http.StatusNotFound)
		return
	}
	n := v.(*atomic.Int64).Add(1)
	if n < 2 {
		writeJSON(w, map[string]any{"status": "processing", "progress": 30})
		return
	}
	// ⚠️ 产物挂在 content.video_url —— 老系统的路径表里没有 content，
	// 一个产物都扫不到，落到"任务完成但未返回产物"。
	writeJSON(w, map[string]any{
		"status":  "succeeded",
		"content": map[string]any{"video_url": s.addr + "/files/out.mp4"},
	})
}

// klingSubmit 校验三段式请求体。
func (s *server) klingSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Contents []map[string]any `json:"contents"`
		Prompt   string           `json:"prompt"`
		Settings map[string]any   `json:"settings"`
		Options  map[string]any   `json:"options"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "请求体不是 JSON", http.StatusBadRequest)
		return
	}
	if req.Settings == nil {
		// 可灵的规格参数在 settings 里；缺了说明拼错了形状。
		writeJSON(w, map[string]any{"code": 1001, "message": "缺少 settings"})
		return
	}
	if len(req.Contents) == 0 && req.Prompt == "" {
		writeJSON(w, map[string]any{"code": 1002, "message": "既没有 contents 也没有 prompt"})
		return
	}
	roles := make([]string, 0, len(req.Contents))
	for _, c := range req.Contents {
		roles = append(roles, fmt.Sprint(c["type"]))
	}
	log.Printf("  可灵收到 %s：contents=%v settings=%v", r.URL.Path, roles, req.Settings)
	id := fmt.Sprintf("kling-%d", s.seq.Add(1))
	s.asyncTasks.Store(id, new(atomic.Int64))
	// ⚠️ 业务码在 HTTP 200 里：只看 http_status 判不出失败。
	writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
		"id": id, "status": "submitted",
	}})
}

func (s *server) klingQuery(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/v1/kling/query/"):]
	v, ok := s.asyncTasks.Load(id)
	if !ok {
		writeJSON(w, map[string]any{"code": 1404, "message": "任务不存在"})
		return
	}
	if v.(*atomic.Int64).Add(1) < 2 {
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
			"id": id, "status": "processing", "progress": 40,
		}})
		return
	}
	// ⚠️ 可灵的成功状态是 `succeed`（没有 ed）。词表少一个词的后果不是报错，
	// 而是任务永远停在 pending 直到撞死线。
	writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
		"id": id, "status": "succeed",
		"video": map[string]any{"url": s.addr + "/files/kling.mp4"},
	}})
}

// seedanceSubmit 校验「content 数组 + 顶层规格」。
func (s *server) seedanceSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model      string           `json:"model"`
		Content    []map[string]any `json:"content"`
		Resolution string           `json:"resolution"`
		Duration   int              `json:"duration"`
		Ratio      string           `json:"ratio"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "请求体不是 JSON", http.StatusBadRequest)
		return
	}
	// ⚠️ 是 content 不是 contents —— 抄错一个字母上游就整包读不到输入。
	if len(req.Content) == 0 {
		writeJSON(w, map[string]any{"error": map[string]any{
			"code": "InvalidParameter", "message": "缺少 content 数组",
		}})
		return
	}
	if req.Resolution == "" || req.Duration == 0 {
		writeJSON(w, map[string]any{"error": map[string]any{
			"code": "InvalidParameter", "message": "规格参数必须在顶层",
		}})
		return
	}
	// 营销 E2E（MKT-T-30）：画面描述里带 `#上游失败` 的那一条提交即 400（永久失败），
	// 用来让批量里的某一个变体失败、验「退款恰好一次 + 部分成功」。
	for _, c := range req.Content {
		if t, _ := c["text"].(string); strings.Contains(t, magicUpstreamFail) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": "InvalidParameter", "message": "假上游按 " + magicUpstreamFail + " 拒绝"}})
			return
		}
	}
	roles := make([]string, 0, len(req.Content))
	for _, c := range req.Content {
		roles = append(roles, fmt.Sprintf("%v/%v", c["type"], c["role"]))
	}
	log.Printf("  Seedance 收到：content=%v resolution=%s duration=%d ratio=%q",
		roles, req.Resolution, req.Duration, req.Ratio)
	id := fmt.Sprintf("seedance-%d", s.seq.Add(1))
	s.asyncTasks.Store(id, new(atomic.Int64))
	writeJSON(w, map[string]any{"id": id, "status": "queued"})
}

func (s *server) seedanceQuery(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/v1/seedance/query/"):]
	v, ok := s.asyncTasks.Load(id)
	if !ok {
		http.Error(w, "任务不存在", http.StatusNotFound)
		return
	}
	if v.(*atomic.Int64).Add(1) < 2 {
		writeJSON(w, map[string]any{"id": id, "status": "running"})
		return
	}
	writeJSON(w, map[string]any{"id": id, "status": "succeeded",
		"content": map[string]any{"video_url": s.addr + "/files/seedance.mp4"}})
}

// failPermanent 上游明确拒绝（400）：永久失败，不该重试。
func (s *server) failPermanent(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusBadRequest)
	writeJSON(w, map[string]any{"error": map[string]any{
		"code": "InvalidPrompt", "message": "提示词包含不被允许的内容",
	}})
}

// failRateLimit 限流（429）：可重试失败。
// ⚠️ 状态码要原样带回 —— 判成 400 就永远不重试了。
func (s *server) failRateLimit(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Retry-After", "2")
	w.WriteHeader(http.StatusTooManyRequests)
	writeJSON(w, map[string]any{"error": map[string]any{
		"code": "RateLimited", "message": "请求过于频繁",
	}})
}

// failEmpty HTTP 200 但没有产物。
// ⚠️ 这不能算成功：用户会看到一个空结果还被扣了费。
func (s *server) failEmpty(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"status": "ok", "note": "完成但没有产物"})
}

// failHang 挂起不回：提交结果**不确定**。
// ⚠️ 这一路绝不能重发 —— 请求可能已经到了上游、可能已在那边计费。
func (s *server) failHang(w http.ResponseWriter, r *http.Request) {
	select {
	case <-time.After(60 * time.Second):
	case <-r.Context().Done():
	}
	_ = w
}

// failTaskSubmit / failTaskQuery 异步任务在上游侧失败。
func (s *server) failTaskSubmit(w http.ResponseWriter, _ *http.Request) {
	id := fmt.Sprintf("failtask-%d", s.seq.Add(1))
	writeJSON(w, map[string]any{"task_id": id})
}

func (s *server) failTaskQuery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"status": "failed", "message": "上游渲染失败：显存不足"})
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	s.lastDownloadAuth.Store(r.Header.Get("Authorization"))
	switch {
	case len(r.URL.Path) > 4 && r.URL.Path[len(r.URL.Path)-4:] == ".mp4":
		w.Header().Set("Content-Type", "video/mp4")
		// ⚠️ 回**真的** MP4：假字节串 ffprobe 读不出时长，
		// 于是「脚本 → 视频组 → 时间线装配」这条链在联调里永远走不到成功那一步
		// （P10-01 实测：6 个镜头全被判成"素材时长未知"）。
		_, _ = w.Write(decodeB64(oneSecondMP4))
	case len(r.URL.Path) > 4 && r.URL.Path[len(r.URL.Path)-4:] == ".mp3":
		// 配乐、音效的结果（SCT-39）：同样是真的音频，量得出时长。
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(decodeB64(twoSecondMP3))
	default:
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(decodeB64(onePixelPNG))
	}
}

func decodeB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return []byte("bad-png")
	}
	return b
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
