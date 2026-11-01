package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
	"github.com/li904338203/tabtv/backend/internal/provider/transport"
)

// snapshot 是任务参数快照里 `_snapshot` 段的一部分。
// 只取这一层需要的字段，其余原样留在 params 里给协议用。
type snapshot struct {
	ModelCode     string `json:"modelCode"`
	OperationCode string `json:"operationCode"`
	ProviderCode  string `json:"providerCode"`
	APIProtocol   string `json:"apiProtocol"`
	ResultType    string `json:"resultType"`
	PromptSuffix  string `json:"promptSuffix"`
}

// resolvedInput 是 inputs_snapshot 里的一条（与 canvas/domain.ResolvedInput 对应）。
//
// ⚠️ 这里**单独定义一份**而不是 import 画布域的类型：
// 产品域之间按接口调用，不直接依赖对方的内部结构（08 §3）。
// 快照的 JSON 形状本身就是这两边之间的契约，契约变化会在测试里炸出来。
type resolvedInput struct {
	Kind      string `json:"kind"`
	Order     int    `json:"order"`
	Text      string `json:"text,omitempty"`
	MediaID   int64  `json:"mediaId,omitempty"`
	MediaKind string `json:"mediaKind,omitempty"`
	VoiceCode string `json:"voiceCode,omitempty"`
	// Role 是输入角色（`mask` = 蒙版）。协议靠它区分蒙版与参考图，不再按「第二张就是蒙版」猜（CD-80）。
	Role string `json:"role,omitempty"`
	// RangeMs 只用素材的哪一段（SCD-34）：发给上游前截出来（cut.go），时长报区间长度。
	RangeMs []int `json:"rangeMs,omitempty"`
}

// buildJob 把生成任务翻成协议工作项。
//
// urlOnly 是协议是否只收地址形态的参考媒体（protocol.RequiresMediaURL）：只有提交要传它 ——
// 查询不发参考图，签不出外链也不该让轮询失败。
func (p *HTTPProvider) buildJob(ctx context.Context, job genapp.Job, cfg RunConfig, urlOnly bool) (
	protocol.Job, error) {
	snap := parseSnapshot(job.Params)
	params := userParams(job.Params)
	// ⚠️ 把已解析的提交路径交给协议：可灵要靠它区分普通端点与 omni 端点
	// （两者的请求体形状不同）。协议层拿不到 Provider 配置 —— 它只收 Job，
	// 这样才能保持纯函数、用 fixture 单独测。
	if cfg.SubmitPath != "" {
		params[protocol.SubmitPathHintKey] = cfg.SubmitPath
	}

	inputs, err := parseInputs(job.Inputs)
	if err != nil {
		return protocol.Job{}, err
	}

	prompt, media, err := p.composeInputs(ctx, cfg, job.GenerationID, params, inputs, snap.PromptSuffix, urlOnly)
	if err != nil {
		return protocol.Job{}, err
	}

	images := make([]string, 0, len(media))
	for _, m := range media {
		if m.Kind == "image" {
			images = append(images, m.Value)
		}
	}

	return protocol.Job{
		OperationCode: job.OperationCode, ModelCode: cfg.ModelCode,
		UpstreamModel: cfg.UpstreamModel, ResultType: job.ResultType,
		IdempotencyKey: job.IdempotencyKey, Prompt: prompt,
		Images: images, Media: media, Params: params, Options: optionsOf(params),
	}, nil
}

// composeInputs 拼提示词并解析参考媒体。
//
// ⚠️ 拼接顺序不可换：**上游文本（按连线顺序）→ 本节点的 prompt 参数 → 素材词**。
// 识别信息必须排在审美词前面 —— 反过来的话模型会先照着风格词画，
// 把用户真正要的内容挤掉（老系统 §2.8 的教训）。
func (p *HTTPProvider) composeInputs(ctx context.Context, cfg RunConfig, generationID int64,
	params map[string]any, inputs []resolvedInput, suffix string, urlOnly bool) (
	string, []protocol.Media, error) {
	sort.SliceStable(inputs, func(i, j int) bool { return inputs[i].Order < inputs[j].Order })

	var parts []string
	var media []protocol.Media
	for _, in := range inputs {
		switch {
		case in.Kind == "text" && in.Text != "":
			parts = append(parts, in.Text)
		case in.MediaID != 0 && hasRange(in):
			// 带区间：截出那一段再发，时长报区间长度（SCD-34）。
			value, mime, err := p.rangeValue(ctx, generationID, in, urlOnly)
			if err != nil {
				return "", nil, err
			}
			media = append(media, protocol.Media{
				Kind: in.MediaKind, Order: in.Order, Value: value, MIME: mime, Role: in.Role,
				DurationMs: in.RangeMs[1] - in.RangeMs[0],
			})
		case in.MediaID != 0:
			value, mime, err := p.mediaValue(ctx, cfg, in.MediaID, urlOnly)
			if err != nil {
				return "", nil, err
			}
			kind := in.MediaKind
			if kind == "" {
				kind = kindFromMIME(mime)
			}
			media = append(media, protocol.Media{
				Kind: kind, Order: in.Order, Value: value, MIME: mime, Role: in.Role,
				DurationMs: p.mediaDuration(ctx, kind, in.MediaID),
			})
		}
	}
	if own := paramText(params, "prompt"); own != "" {
		parts = append(parts, own)
	}
	if suffix != "" {
		parts = append(parts, suffix)
	}
	return strings.Join(parts, "\n"), media, nil
}

// mediaValue 把媒体 ID 变成上游能用的形态。
//
// 回落规则：**签名外链优先，上游够不着时内联 base64**。
//
// ⚠️ 判"够不着"用的是固定的公网规则（见 upstreamReachable）：
// 我们无从知道上游的网络，但**内网地址上游一定拉不到**
// （本地 MinIO 签出来的就是 127.0.0.1）。
// 老系统在这里栽过：本地存储下参考图一律内联成 base64，于是
// 要求 URL 形态的上游直接拒收（`reference asset URL is too long`），
// 而一张图 base64 后好几 MB，每次生成还要重传一遍。
//
// urlOnly（协议只收地址形态，protocol.MediaURLRequiring）时不回落内联：签不出上游够得着的地址就报错，
// 由提交方判永久失败（不发上游、退款）。
func (p *HTTPProvider) mediaValue(ctx context.Context, cfg RunConfig, mediaID int64, urlOnly bool) (
	string, string, error) {
	if p.media == nil {
		return "", "", fmt.Errorf("媒体解析器未接入，无法取参考媒体 %d", mediaID)
	}
	url, mime, err := p.media.SignedURL(ctx, mediaID)
	if err == nil && url != "" && upstreamReachable(url) {
		return url, mime, nil
	}
	if urlOnly {
		why := "签出的地址上游访问不到（本地 MinIO 这类存储签出的是内网地址）"
		if err != nil {
			why = "签发地址失败：" + err.Error()
		}
		// 配了公网中转（R-15）：转存一份过去，发那边的地址。
		if p.relay != nil {
			ru, rmime, rerr := p.relay.URL(ctx, mediaID)
			switch {
			case rerr != nil:
				why += "；转存到公网中转存储也失败了：" + rerr.Error()
			case !upstreamReachable(ru):
				why += "；公网中转存储签出的地址同样访问不到（检查 SEEAITV_RELAY_STORAGE_ENDPOINT）"
			default:
				if rmime == "" {
					rmime = mime
				}
				return ru, rmime, nil
			}
		}
		// ⚠️ 指名道姓地报原因：这家上游收到内联图时回的是「提示词过长」之类与原因无关的话（R-13），
		// 不在这里说清，排查会一路怀疑提示词和参数。
		return "", "", fmt.Errorf("该模型的上游只收公网可访问的参考媒体地址、不收内联图片，而参考媒体 %d %s；"+
			"请配置公网可访问的对象存储（SEEAITV_STORAGE_*）或公网中转存储（SEEAITV_RELAY_STORAGE_*），"+
			"或换一个收内联图片的模型", mediaID, why)
	}
	if err != nil {
		p.logf("签发参考媒体外链失败，回落内联", "mediaId", mediaID, "error", err)
	} else {
		p.logf("参考媒体外链上游够不着，回落内联", "mediaId", mediaID)
	}

	data, mime2, err := p.media.Inline(ctx, mediaID)
	if err != nil {
		return "", "", fmt.Errorf("取参考媒体 %d 失败: %w", mediaID, err)
	}
	if mime2 != "" {
		mime = mime2
	}
	if int64(len(data)) > p.maxInlineBytes() {
		// ⚠️ 指名道姓地报：内联超限的真实原因是"部署没配可外链的对象存储"，
		// 不说清楚的话排查时会一路怀疑模型、怀疑参数。
		return "", "", fmt.Errorf(
			"参考媒体 %d 有 %d 字节，超过内联上限 %d；请配置可对外签发地址的对象存储",
			mediaID, len(data), p.maxInlineBytes())
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), mime, nil
}

// mediaDuration 取音视频的时长（毫秒）；取不到返回 0（未知）。
//
// 只有部分协议用得上（UniArt 要自报参考视频总时长），所以是**可选端口**：
// 媒体解析器没实现、或查询失败，都按未知处理 —— 由用得上的协议自己决定拒不拒，
// 不在这里把用不上时长的协议一起拖垮。
func (p *HTTPProvider) mediaDuration(ctx context.Context, kind string, mediaID int64) int {
	if kind != "video" && kind != "audio" {
		return 0
	}
	d, ok := p.media.(MediaDurationResolver)
	if !ok {
		return 0
	}
	ms, err := d.DurationMs(ctx, mediaID)
	if err != nil {
		p.logf("取参考媒体时长失败，按未知处理", "mediaId", mediaID, "error", err)
		return 0
	}
	return ms
}

func (p *HTTPProvider) maxInlineBytes() int64 {
	if p.MaxInlineBytes > 0 {
		return p.MaxInlineBytes
	}
	return DefaultMaxInlineBytes
}

// DefaultMaxInlineBytes 内联 base64 的上限。
//
// 8MB：base64 会再涨三分之一，而很多上游对请求体有 10MB 级的硬限制。
const DefaultMaxInlineBytes int64 = 8 << 20

func parseSnapshot(params json.RawMessage) snapshot {
	var tree struct {
		Snapshot snapshot `json:"_snapshot"`
	}
	_ = json.Unmarshal(params, &tree)
	return tree.Snapshot
}

// userParams 取用户参数（剔掉 `_snapshot`）。
//
// ⚠️ 未知字段**原样保留**（GO-35）：协议可能要用到我们还没建模的参数，
// 按 struct 解一遍再塞回去会把它们悄悄丢掉。
func userParams(params json.RawMessage) map[string]any {
	var tree map[string]any
	if err := json.Unmarshal(params, &tree); err != nil {
		return map[string]any{}
	}
	delete(tree, "_snapshot")
	return tree
}

// optionsOf 取模型调用参数。
func optionsOf(params map[string]any) map[string]any {
	if v, ok := params["options"].(map[string]any); ok {
		return v
	}
	out := map[string]any{}
	// 常见的几个直接放在参数层，不必强制用户套一层 options。
	for _, k := range []string{"temperature", "top_p", "max_tokens"} {
		if v, ok := params[k]; ok {
			out[k] = v
		}
	}
	return out
}

func parseInputs(raw json.RawMessage) ([]resolvedInput, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []resolvedInput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("输入快照无法解析: %w", err)
	}
	return out, nil
}

func paramText(params map[string]any, key string) string {
	if v, ok := params[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func kindFromMIME(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	}
	return ""
}

// publicOnly 是判断"上游够不够得着"用的固定规则。
//
// ⚠️ 不能用本进程的出网策略（p.policy）来判：
// 「**我们**能不能访问这个地址」（SSRF 防护，dev 下会放开内网）与
// 「**上游**能不能访问这个地址」是两个问题。用出网策略判的话，
// dev 下参考图会以 127.0.0.1 的地址发给真实上游，上游一个字节也拉不到，
// 而错误信息只会是一句含糊的「参考图无效」。
var publicOnly = transport.URLPolicy{AllowPrivate: false}

func upstreamReachable(u string) bool { return publicOnly.CheckURL(u) == nil }

// credentialsOf 取一次调用的认证配置（密钥已解密，只在进程内流转）。
func credentialsOf(cfg RunConfig) transport.Credentials {
	return transport.Credentials{Type: cfg.AuthType, Key: cfg.APIKey, HeaderName: cfg.HeaderName,
		Region: cfg.SignRegion, Service: cfg.SignService}
}

// authHeaders 组装认证头与 Provider 自定义头。
//
// 签名类认证（volc_v4）不在这里出头：签名要覆盖请求本身，由 buildTransportRequest 在正文定下后签。
// 下载产物时（同源才带）也就只带自定义头 —— 火山的产物地址是对象存储的签名链接，不需要再签。
func authHeaders(cfg RunConfig) map[string]string {
	h := map[string]string{}
	for k, v := range cfg.Headers {
		h[k] = v
	}
	for k, v := range transport.AuthHeader(cfg.AuthType, cfg.APIKey, cfg.HeaderName) {
		h[k] = v
	}
	return h
}
