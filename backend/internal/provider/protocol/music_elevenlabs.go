package protocol

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

func init() { register(elevenLabsMusic{}) }

// ElevenLabs Music（docs/agent/smartcut SCT-39，dev/01 §7）：
//
//	POST /v1/music?output_format=mp3_44100_128   {"prompt":"…","music_length_ms":30000,"force_instrumental":true}
//	→ 200，正文直接是音频字节
//
// 2026-10-28 核对过的官方文档（https://elevenlabs.io/docs/api-reference/music/compose 、
// https://elevenlabs.io/docs/api-reference/authentication）：
//   - 认证头 `xi-api-key`（Provider 配 `header` 认证，自定义头 `X-Auth-Header-Name: xi-api-key`）；
//   - `music_length_ms` 3000~600000；`force_instrumental` 缺省 false；`model_id` 取 music_v1 / music_v2 / music_v2_5；
//   - `output_format` 在查询串，缺省 `auto`（格式由上游定）；校验错误回 422 `{detail:[{loc,msg,type}]}`。
//
// ⚠️ `output_format` **一律显式给 mp3**：缺省的 auto 由上游决定格式，而我们把字节按 audio/mpeg 归档 ——
// 回的是 pcm 的话文件后缀与内容对不上，播放器打不开，而任务是「成功」的。
// ⚠️ 时长按剪辑台的上限取 3~300 秒（契约 `durationSec` 最多 300）：上游虽然收到 10 分钟，一段配乐五分钟够了，
// 也免得一次误操作花掉十分钟的钱。
// ⚠️ 只有付费套餐可以商用，而且影视 / 广播 / 游戏除外：写进后台的模型说明（dev/01 R-20）。
type elevenLabsMusic struct{}

const (
	// ElevenLabsMusicMinSeconds / ElevenLabsMusicMaxSeconds 单次时长（上游 3~600 秒，剪辑台取到 300）。
	ElevenLabsMusicMinSeconds = 3
	ElevenLabsMusicMaxSeconds = 300
	elevenLabsMusicPath       = "/v1/music"
	elevenLabsOutputFormat    = "mp3_44100_128"
)

func (elevenLabsMusic) Code() string            { return "elevenlabs_music" }
func (elevenLabsMusic) Async() bool             { return false }
func (elevenLabsMusic) RequiresQueryPath() bool { return false }

// SupportedOperations 实现 CapabilityDeclaring。
func (elevenLabsMusic) SupportedOperations() map[string]OperationSupport {
	return map[string]OperationSupport{"text2music": {Roles: []InputRole{RoleText}}}
}

// DurationRange 实现 DurationDeclaring。
func (elevenLabsMusic) DurationRange(operation string) (int, int, bool) {
	if operation != "text2music" {
		return 0, 0, false
	}
	return ElevenLabsMusicMinSeconds, ElevenLabsMusicMaxSeconds, true
}

// CheckParams 实现 ParamChecking：时长范围（带不带人声都收）。
func (elevenLabsMusic) CheckParams(operation string, durationSec int, _ map[string]any) error {
	if operation != "text2music" {
		return nil
	}
	return checkSeconds("ElevenLabs 配乐", durationSec, ElevenLabsMusicMinSeconds, ElevenLabsMusicMaxSeconds)
}

func (p elevenLabsMusic) BuildSubmit(job Job) (Request, error) {
	if job.OperationCode != "text2music" {
		return Request{}, fmt.Errorf("ElevenLabs Music 不支持 %s", job.OperationCode)
	}
	prompt := strings.TrimSpace(job.Prompt)
	if prompt == "" {
		return Request{}, fmt.Errorf("配乐描述不能为空")
	}
	sec := paramInt(job.Params, "duration", 30)
	if err := p.CheckParams(job.OperationCode, sec, job.Params); err != nil {
		return Request{}, err
	}
	body := map[string]any{
		"prompt": prompt, "music_length_ms": sec * 1000,
		// 配乐缺省纯音乐（契约 instrumental 缺省 true）；上游的缺省是 false，所以一律显式发。
		"force_instrumental": paramBool(job.Params, true, "instrumental"),
	}
	if m := strings.TrimSpace(job.UpstreamModel); strings.HasPrefix(m, "music_") {
		body["model_id"] = m
	}
	return Request{Method: "POST", Path: elevenLabsMusicPath + "?output_format=" + elevenLabsOutputFormat,
		Body: mustJSON(body)}, nil
}

// SubmitPath 实现 PathRouter：配置的提交地址上补 `output_format=mp3_*`（已经配了 mp3 的照旧）。
// 运营配的路径会盖掉 BuildSubmit 给的那一条，不在这里补的话查询串就丢了，回来的就是 auto 格式。
func (elevenLabsMusic) SubmitPath(_ Job, configured string) (string, error) {
	if configured == "" {
		return elevenLabsMusicPath + "?output_format=" + elevenLabsOutputFormat, nil
	}
	path, rawQuery, _ := strings.Cut(configured, "?")
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", fmt.Errorf("ElevenLabs 的提交地址查询串不合法: %q", configured)
	}
	if !strings.HasPrefix(q.Get("output_format"), "mp3_") {
		q.Set("output_format", elevenLabsOutputFormat)
	}
	return path + "?" + q.Encode(), nil
}

// ParseSubmit 正文是**裸音频字节**（同 openai_tts）：按 JSON 解析会失败，而那条错误指不到真正的原因。
func (elevenLabsMusic) ParseSubmit(status int, body []byte) (Result, error) {
	if status >= 400 {
		return Result{}, httpError(status, body)
	}
	if len(body) == 0 {
		return Result{}, fmt.Errorf("ElevenLabs 返回了空的音频")
	}
	if b := strings.TrimSpace(string(body[:min(len(body), 16)])); strings.HasPrefix(b, "{") {
		// 200 却是 JSON：多半是网关或上游换了返回形态。不把它当音频存。
		return Result{}, fmt.Errorf("ElevenLabs 应返回音频字节，实际是 JSON: %s", bodySnippet(body))
	}
	return Result{Outputs: []Output{{Kind: "base64", Value: base64.StdEncoding.EncodeToString(body), MIME: "audio/mpeg"}}}, nil
}

func (elevenLabsMusic) BuildQuery(Job, string) (Request, bool) { return Request{}, false }
func (elevenLabsMusic) ParseQuery(int, []byte) (Result, error) { return Result{}, nil }
