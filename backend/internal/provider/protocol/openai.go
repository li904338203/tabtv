package protocol

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

func init() {
	register(openAIChat{})
	register(openAIImages{})
	register(openAITTS{})
	register(anthropicMessages{})
}

// ---------------------------------------------------------------- openai_chat

type openAIChat struct{}

func (openAIChat) Code() string            { return "openai_chat" }
func (openAIChat) Async() bool             { return false }
func (openAIChat) RequiresQueryPath() bool { return false }

func (openAIChat) BuildSubmit(job Job) (Request, error) {
	// ⚠️ 有图片时必须走 content 数组形态。只发文字的话，
	// 图片反推会得到「您的消息中没有附带任何图片」—— 看起来像模型不行，
	// 其实是我们没把画面发过去。
	var content any = job.Prompt
	if len(job.Images) > 0 {
		parts := make([]any, 0, len(job.Images)+1)
		parts = append(parts, map[string]any{"type": "text", "text": job.Prompt})
		for _, img := range job.Images {
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": img},
			})
		}
		content = parts
	}
	messages := make([]any, 0, 2)
	if job.System != "" {
		// OpenAI 的系统提示词是一条 role=system 的**消息**。
		messages = append(messages, map[string]any{"role": "system", "content": job.System})
	}
	messages = append(messages, map[string]any{"role": "user", "content": content})
	body := map[string]any{
		"model":    job.UpstreamModel,
		"messages": messages,
	}
	// options 原样透传：temperature/top_p/max_tokens 等由调用方决定，
	// 协议不替它做选择（也不替它兜底，上游不认的参数应当由上游报错）。
	for k, v := range job.Options {
		body[k] = v
	}
	return Request{Method: "POST", Path: "/chat/completions", Body: mustJSON(body)}, nil
}

func (openAIChat) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Result{}, err
	}
	if len(resp.Choices) == 0 {
		// 先按结构精确取，取不到才回落到兜底扫描（见 ExtractOutputs 注释）。
		if outs := ExtractOutputs(body); len(outs) > 0 {
			return Result{Outputs: outs}, nil
		}
		return Result{}, httpError(status, body)
	}
	var outs []Output
	for _, c := range resp.Choices {
		outs = append(outs, Output{Kind: "text", Value: c.Message.Content})
	}
	return Result{Outputs: outs}, nil
}

func (openAIChat) BuildQuery(Job, string) (Request, bool) { return Request{}, false }
func (openAIChat) ParseQuery(int, []byte) (Result, error) { return Result{}, nil }

// ParseSSE 把 OpenAI 风格的 SSE 流拼成完整文本。
//
// ⚠️ 要能处理**半包**：网络按字节切块，一个 `data:` 行可能跨两次读取到达。
// 调用方持有累积缓冲，每次把新块喂进来。切行逻辑在 sse.go，与 Chat 流式解析共用。
func ParseSSE(buf *strings.Builder, chunk []byte, onDelta func(string)) (done bool) {
	return feedSSELines(buf, chunk, func(line string) bool {
		field, payload, ok := sseField(line)
		if !ok || field != "data" {
			return false
		}
		if payload == "[DONE]" {
			return true
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			// ⚠️ 单条事件解析失败不能中断整个流：上游可能夹带注释行或心跳。
			return false
		}
		for _, c := range ev.Choices {
			if c.Delta.Content != "" {
				onDelta(c.Delta.Content)
			}
		}
		return false
	})
}

// ---------------------------------------------------------------- openai_images

type openAIImages struct{}

func (openAIImages) Code() string            { return "openai_images" }
func (openAIImages) Async() bool             { return false }
func (openAIImages) RequiresQueryPath() bool { return false }

func (openAIImages) BuildSubmit(job Job) (Request, error) {
	n := paramInt(job.Params, "count", 1)
	size := sizeFromAspect(paramString(job.Params, "aspect_ratio", "1:1"))
	quality := imageQuality(paramString(job.Params, "quality", ""))

	// ⚠️ 有参考图时走**编辑接口**，而且是 multipart 不是 JSON：
	// `/images/generations` 根本不接受图片，把图塞进它的 JSON body 里
	// 上游会直接忽略 —— 出图成功、但和参考图毫无关系，用户只会觉得"模型不听话"。
	if len(job.Images) > 0 {
		refs, mask := editImages(job)
		req := Request{
			Method: "POST", Path: "/images/edits",
			Fields: map[string]string{
				"model": job.UpstreamModel, "prompt": job.Prompt,
				"n": itoa(n), "size": size,
			},
		}
		if quality != "" {
			req.Fields["quality"] = quality
		}
		// ⚠️ 多张参考图**每张一个 `image` 字段**（CD-80）：分镜图要同时参照这一镜的角色、场景、道具。
		// 此前只发第一张、第二张当成蒙版 —— 带着五六张资产图的分镜图会把第二个角色的设定图当成「要重绘的区域」。
		// 老系统对同一个上游（gpt-image-2 @ relay.example.com）就是这样发的，实测 6~7 张图返回 200。
		// ⚠️ 官方多图字段名是 `image[]`（单张是 `image`）—— tabtv 核实官方文档后改（docs/research/厂商官方端点核实.md §1）。
		field := "image"
		if len(refs) > 1 {
			field = "image[]"
		}
		for i, ref := range refs {
			req.Files = append(req.Files,
				FilePart{Field: field, Filename: "reference-" + itoa(i+1) + ".png", Ref: ref})
		}
		if mask != "" {
			// 编辑接口用 mask 指定要重绘的区域（涂抹重绘）。
			req.Files = append(req.Files, FilePart{Field: "mask", Filename: "mask.png", Ref: mask})
		} else if paramString(job.Params, "input_fidelity", "") == "" && supportsInputFidelity(job.UpstreamModel) {
			// 参考图式编辑：让上游尽量保住参考图里的身份与细节（老系统同款默认）。
			// ⚠️ 只有 gpt-image-1 系列认这个参数；官方写明 gpt-image-2 及之后的型号不要传（传了会被拒）。
			req.Fields["input_fidelity"] = "high"
		}
		return req, nil
	}

	body := map[string]any{
		"model": job.UpstreamModel, "prompt": job.Prompt, "n": n, "size": size,
	}
	if quality != "" {
		body["quality"] = quality
	}
	for k, v := range job.Options {
		body[k] = v
	}
	return Request{Method: "POST", Path: "/images/generations", Body: mustJSON(body)}, nil
}

func (openAIImages) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	var resp struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Result{}, err
	}
	var outs []Output
	for _, d := range resp.Data {
		switch {
		case d.URL != "":
			outs = append(outs, Output{Kind: "url", Value: d.URL, MIME: mimeFromURL(d.URL)})
		case d.B64JSON != "":
			outs = append(outs, Output{Kind: "base64", Value: d.B64JSON, MIME: "image/png"})
		}
	}
	if len(outs) == 0 {
		outs = ExtractOutputs(body)
	}
	return Result{Outputs: outs}, nil
}

func (openAIImages) BuildQuery(Job, string) (Request, bool) { return Request{}, false }
func (openAIImages) ParseQuery(int, []byte) (Result, error) { return Result{}, nil }

// sizeFromAspect 把画幅比翻成上游要的像素尺寸。
//
// 规则**照老系统**（`ConfigurableHttpProviderAdapter.openAiImageSize`，在生产上用这批
// gpt-image 模型验证过）：上游只有横 / 竖 / 方三种尺寸，按宽高比折算 ——
// ≥ 1.2 取横版、≤ 0.84 取竖版、其余取方形；「自适应」与解析不了的一律交给上游 `auto`。
//
// ⚠️ P15-02 之前这里按 **DALL·E 3** 的尺寸写（16:9 → 1792x1024），而实际配置的是
//
//	gpt-image 系列（1024x1024 / 1536x1024 / 1024x1536 / auto）。比例选项也只认 5 个，
//	其余一律落成方形 —— 用户选了 21:9 出来一张正方形，而任务是"成功"的。
//
// ⚠️ 具体型号支持哪些尺寸由 `model_param_support` 配置决定，这里只做**形态转换**，
// 不做能力判断 —— 把型号能力写进代码，上游放开一档就得改代码重新发版。
func sizeFromAspect(ratio string) string {
	ratio = strings.TrimSpace(ratio)
	if ratio == "" || strings.EqualFold(ratio, "adaptive") || strings.EqualFold(ratio, "auto") {
		return "auto"
	}
	parts := strings.SplitN(ratio, ":", 2)
	if len(parts) != 2 {
		return "auto"
	}
	w, errW := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	h, errH := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return "auto"
	}
	switch v := w / h; {
	case v >= 1.2:
		return "1536x1024"
	case v <= 0.84:
		return "1024x1536"
	}
	return "1024x1024"
}

// imageQuality 把画质翻成上游的取值（照老系统）：standard → medium，low/high 原样；
// 没选就不发，让上游用它自己的默认值。
//
// ⚠️ 老系统里画质**真的发给上游**；同一个弹窗里的「清晰度」对图片从没发出去过，
// 那个没有搬过来（搬了就是一个选了等于没选的控件）。
func imageQuality(q string) string {
	switch strings.ToLower(strings.TrimSpace(q)) {
	case "":
		return ""
	case "standard", "medium":
		return "medium"
	case "low":
		return "low"
	case "high":
		return "high"
	}
	// 认不出的值不往上游发：发一个上游不认的枚举，整次生成会被拒掉。
	return ""
}

// ---------------------------------------------------------------- openai_tts

type openAITTS struct{}

func (openAITTS) Code() string            { return "openai_tts" }
func (openAITTS) Async() bool             { return false }
func (openAITTS) RequiresQueryPath() bool { return false }

func (openAITTS) BuildSubmit(job Job) (Request, error) {
	body := map[string]any{
		"model": job.UpstreamModel,
		"input": job.Prompt,
		"voice": paramString(job.Params, "voice", "alloy"),
	}
	if speed, ok := job.Params["speed"]; ok {
		body["speed"] = speed
	}
	return Request{Method: "POST", Path: "/audio/speech", Body: mustJSON(body)}, nil
}

// ParseSubmit 处理**裸字节**响应。
//
// ⚠️ TTS 回的是音频字节而不是 JSON：按 JSON 解析会失败，
// 而失败信息（"invalid character"）完全指不到真正的原因。
func (openAITTS) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	return Result{Outputs: []Output{{
		Kind:  "base64",
		Value: base64.StdEncoding.EncodeToString(body),
		MIME:  "audio/mpeg",
	}}}, nil
}

func (openAITTS) BuildQuery(Job, string) (Request, bool) { return Request{}, false }
func (openAITTS) ParseQuery(int, []byte) (Result, error) { return Result{}, nil }

// ---------------------------------------------------------------- anthropic_messages

type anthropicMessages struct{}

func (anthropicMessages) Code() string            { return "anthropic_messages" }
func (anthropicMessages) Async() bool             { return false }
func (anthropicMessages) RequiresQueryPath() bool { return false }

func (anthropicMessages) BuildSubmit(job Job) (Request, error) {
	content := []any{}
	// ⚠️ Anthropic 的图片要放在文本**之前**，且用 source 结构而不是 url ——
	// 与 OpenAI 的形态完全不同，这正是"换一家厂商基本都要加协议"的原因。
	for _, img := range job.Images {
		if src, ok := anthropicImageSource(img); ok {
			content = append(content, src)
		}
	}
	content = append(content, map[string]any{"type": "text", "text": job.Prompt})

	body := map[string]any{
		"model":    job.UpstreamModel,
		"messages": []any{map[string]any{"role": "user", "content": content}},
		// max_tokens 在 Anthropic 是**必填**，缺了直接 400。
		"max_tokens": paramInt(job.Options, "max_tokens", 4096),
	}
	// ⚠️ Anthropic 的系统提示词是**顶层字段**，不是一条 role=system 的消息 ——
	// 发成消息会直接 400。这是与 OpenAI 的又一处形状差异。
	if job.System != "" {
		body["system"] = job.System
	}
	for k, v := range job.Options {
		if k == "max_tokens" {
			continue
		}
		body[k] = v
	}
	return Request{
		Method: "POST", Path: "/messages", Body: mustJSON(body),
		// 版本头也是必填，漏了回 400。
		Header: map[string]string{"anthropic-version": "2023-06-01"},
	}, nil
}

func anthropicImageSource(img string) (map[string]any, bool) {
	if strings.HasPrefix(img, "data:") {
		i := strings.Index(img, ";base64,")
		if i < 0 {
			return nil, false
		}
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "base64", "media_type": img[5:i], "data": img[i+len(";base64,"):],
			},
		}, true
	}
	// 新版支持 url 形态。
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": img},
	}, true
}

func (anthropicMessages) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Result{}, err
	}
	var sb strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 {
		if outs := ExtractOutputs(body); len(outs) > 0 {
			return Result{Outputs: outs}, nil
		}
	}
	return Result{Outputs: []Output{{Kind: "text", Value: sb.String()}}}, nil
}

func (anthropicMessages) BuildQuery(Job, string) (Request, bool) { return Request{}, false }
func (anthropicMessages) ParseQuery(int, []byte) (Result, error) { return Result{}, nil }

// SubmitPath 有参考图时路由到编辑接口。
//
// ⚠️ 这一条是联调矩阵抓到的真 bug：`BuildSubmit` 里把 `Path` 设成了
// `/images/edits`，但 app 层**优先用配置里的 submitPath**（而协议校验又要求
// submitPath 必填），于是 multipart 请求被发到了 `/images/generations` ——
// 上游解不出文件，出一张与参考图毫无关系的图，**而任务是成功的**。
// 与可灵的文生/图生两个端点是同一类问题，解法也一样：让协议改写路径。
func (openAIImages) SubmitPath(job Job, configured string) (string, error) {
	if len(job.Images) == 0 {
		return configured, nil
	}
	if strings.Contains(configured, "/images/edits") {
		return configured, nil
	}
	if strings.Contains(configured, "/images/generations") {
		return strings.Replace(configured, "/images/generations", "/images/edits", 1), nil
	}
	// 配置指向了别的地址（自建中转）：按它来，不替运营做决定。
	return configured, nil
}

// editImages 把编辑接口的图片分成「参考图」与「蒙版」。
//
// ⚠️ 蒙版认**显式角色**（`role=mask`），不按位置猜：参考图可以有好几张，「第二张就是蒙版」在多参考图时是错的。
// 没有角色信息的旧调用只在涂抹重绘（inpaint）且恰好两张时才把第二张当蒙版 —— 那是此前唯一的用法。
// 有蒙版时只发第一张参考图：蒙版说的是「这张图的哪一块要改」，对着好几张图没有意义。
func editImages(job Job) (refs []string, mask string) {
	if len(job.Media) > 0 {
		for _, m := range job.Media {
			if m.Kind != "image" {
				continue
			}
			if m.Role == "mask" {
				if mask == "" {
					mask = m.Value
				}
				continue
			}
			refs = append(refs, m.Value)
		}
	}
	if len(refs) == 0 && mask == "" {
		refs = append(refs, job.Images...)
	}
	if mask == "" && job.OperationCode == "inpaint" && len(refs) == 2 {
		refs, mask = refs[:1], refs[1]
	}
	if mask != "" && len(refs) > 1 {
		refs = refs[:1]
	}
	return refs, mask
}

// supportsInputFidelity：只有 gpt-image-1 系列支持 input_fidelity（官方图片编辑文档）。
func supportsInputFidelity(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "gpt-image-1")
}
