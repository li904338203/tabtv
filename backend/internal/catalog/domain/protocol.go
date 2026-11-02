package domain

import (
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
)

// Protocol 是一个已实现的上游协议。
//
// ⚠️ 修 GAP-09：旧系统 api_protocol 是**硬编码枚举**，改一处要同步四处，
// 结果管理端下拉漏掉了后端已支持的 kling/seedance，
// 运营根本无法维护这些已有适配。
//
// 新设计：协议由**适配器注册表**（`provider/protocol`）决定，
// 启动时 upsert 进 api_protocol 表并置 is_registered=true。
// 管理端下拉直接读这张表 —— 代码没实现的不可能出现在下拉里，实现了的也不可能漏。
type Protocol struct {
	Code string
	Name string
	// IsAsync 异步协议：submit 返回 task id，需轮询 query。
	IsAsync bool
	// RequiresQueryPath 异步协议必须配 query_path，否则跑起来才发现没法轮询。
	RequiresQueryPath bool
	// ChatCapable 能做多轮 + 工具调用（可作 Agent 推理模型，AGT-04）。
	// 由 `protocol.IsChatCapable` 派生，管理端据此决定「支持 Agent」能不能勾 —— 前端不另抄协议名单。
	ChatCapable bool
	// DefaultSubmitPath / DefaultQueryPath 是该协议在**官方接口**上的提交 / 查询路径（相对厂商地址，
	// 与内置厂商模板的地址约定一致，例如 OpenAI 的地址写到 /v1 为止）。空 = 没有通用值：
	// 通用信封由中转自定，可灵按型号与模式区分路径。设置页换协议时据此带出缺省值。
	DefaultSubmitPath string
	DefaultQueryPath  string
}

// displayNames 是协议的中文名。
//
// ⚠️ 这里只管**怎么叫**，不管**有哪些** —— 名单来自适配器注册表。
// 在这里漏写一个名字只会让下拉显示成协议码（难看但不影响功能），
// 而如果名单也维护在这里，漏写一行就是「后端支持但运营选不到」，
// 那正是 GAP-09。
var displayNames = map[string]string{
	"dev_fake":           "开发假适配",
	"openai_chat":        "OpenAI Chat",
	"anthropic_messages": "Anthropic Messages",
	"openai_images":      "OpenAI Images",
	"openai_tts":         "OpenAI TTS",
	"generic_sync":       "通用同步 HTTP",
	"generic_async":      "通用异步 HTTP",
	"grok_video":         "Grok 视频",
	"kling_video":        "可灵视频",
	"seedance_video":     "Seedance 视频（火山方舟）",
	// 语音识别（docs/agent/smartcut SCT-33）。
	"volc_asr":          "火山录音文件识别（极速版）",
	"volc_asr_standard": "火山录音文件识别（标准版）",
	"openai_transcribe": "OpenAI 转写",
	// 片段级生成（docs/agent/smartcut SCT-38~39）。
	"volc_mediakit_erase": "火山 MediaKit 视频去字幕",
	"volc_music":          "火山豆包音乐（纯音乐）",
	"elevenlabs_music":    "ElevenLabs Music",
	"kling_v2a":           "可灵视频配音效",
}

// defaultPaths 是协议的缺省路径（提交、查询）。只写官方文档里固定的那一段；
// 内置厂商模板（seed/data/providers.json）用到的路径必须与这里一致（desktop 包有测试比对）。
var defaultPaths = map[string][2]string{
	"openai_chat":        {"/chat/completions", ""},
	"anthropic_messages": {"/messages", ""},
	"openai_images":      {"/images/generations", ""},
	"openai_tts":         {"/audio/speech", ""},
	"openai_transcribe":  {"/audio/transcriptions", ""},
	"grok_video":         {"/v1/videos/generations", "/v1/videos/{taskId}"},
	"seedance_video":     {"/contents/generations/tasks", "/contents/generations/tasks/{taskId}"},
	"volc_asr":           {"/api/v3/auc/bigmodel/recognize/flash", ""},
	// 标准版的查询路径由协议自己从提交路径推出（submit → query），不用配。
	"volc_asr_standard": {"/api/v3/auc/bigmodel/submit", ""},
	// 可灵的提交路径按型号与模式区分（/text-to-video/<型号>、/omni-video/<型号>…），没有通用值；查询路径是统一的。
	"kling_video":         {"", "/tasks?task_ids={taskId}"},
	"volc_mediakit_erase": {"/api/v1/tools/erase-video-subtitle-pro", "/api/v1/tasks/{taskId}"},
	"elevenlabs_music":    {"/v1/music", ""},
	"kling_v2a":           {"/v1/audio/video-to-audio", ""},
}

// DefaultPaths 返回协议的缺省提交 / 查询路径（没有通用值时为空串）。
func DefaultPaths(code string) (submit, query string) {
	p := defaultPaths[code]
	return p[0], p[1]
}

func nameOf(code string) string {
	if n := displayNames[code]; n != "" {
		return n
	}
	return code
}

// Protocols 返回全部已注册协议（按码排序，保证下拉顺序稳定）。
func Protocols() []Protocol {
	descs := protocol.All()
	out := make([]Protocol, 0, len(descs))
	for _, d := range descs {
		submit, query := DefaultPaths(d.Code)
		out = append(out, Protocol{
			Code: d.Code, Name: nameOf(d.Code),
			IsAsync: d.Async, RequiresQueryPath: d.RequiresQueryPath,
			ChatCapable:       protocol.IsChatCapable(d.Code),
			DefaultSubmitPath: submit, DefaultQueryPath: query,
		})
	}
	return out
}

func LookupProtocol(code string) (Protocol, bool) {
	d, ok := protocol.Lookup(code)
	if !ok {
		return Protocol{}, false
	}
	return Protocol{Code: d.Code, Name: nameOf(d.Code),
		IsAsync: d.Async, RequiresQueryPath: d.RequiresQueryPath,
		ChatCapable: protocol.IsChatCapable(d.Code)}, true
}

// ValidateModelProtocol 校验模型的协议配置。
func ValidateModelProtocol(protocolCode, submitPath, queryPath string) error {
	p, ok := LookupProtocol(protocolCode)
	if !ok {
		return fmt.Errorf("协议 %q 未注册（代码里没有对应的 adapter 实现）", protocolCode)
	}
	if submitPath == "" {
		return fmt.Errorf("协议 %s 必须配置 submitPath", protocolCode)
	}
	// ⚠️ 异步协议缺 queryPath 时，配置能存进去、模型能启用，
	// 直到 Worker 拿到 task id 要轮询才发现没地方查 —— 那时任务已经扣过费了。
	if p.RequiresQueryPath && queryPath == "" {
		return fmt.Errorf("异步协议 %s 必须配置 queryPath，否则任务提交后无法轮询结果", protocolCode)
	}
	if !p.IsAsync && queryPath != "" {
		return fmt.Errorf("同步协议 %s 不应配置 queryPath", protocolCode)
	}
	return nil
}

// CheckVideoParams 按协议声明的范围校验一次视频生成的参数（04 文档 P-61）：时长、清晰度、画幅。
//
// ⚠️ 报价与受理在扣费之前调它。原先这几项只在 Worker 构造上游请求时才检查，
// 那时积分已扣、任务已排队，失败了再退款。params 为空时只查时长。
// 协议没声明范围（中转协议、非视频操作）时一律放行：不知道就别拦。
func CheckVideoParams(apiProtocol, operation string, durationSec int, params map[string]any) error {
	lim, ok := protocol.VideoLimitsOf(apiProtocol, operation)
	if !ok {
		return nil
	}
	if err := lim.CheckDuration(durationSec); err != nil {
		return err
	}
	if len(params) == 0 {
		return nil
	}
	return lim.CheckParams(params)
}

// DescribeVideoLimits 一句话说明协议在某操作下的参数范围；没声明时为空串（写进 Agent 的提示词）。
func DescribeVideoLimits(apiProtocol, operation string) string {
	lim, ok := protocol.VideoLimitsOf(apiProtocol, operation)
	if !ok {
		return ""
	}
	return lim.Describe()
}
