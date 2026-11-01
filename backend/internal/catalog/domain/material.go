package domain

import (
	"fmt"
	"strings"
)

// MaterialType 素材种类。
type MaterialType string

const (
	MaterialStyle  MaterialType = "style"
	MaterialEffect MaterialType = "effect"
	MaterialVoice  MaterialType = "voice"
	// MaterialCamera 运镜（视频节点从「运镜广场」挑一个镜头运动，迁移 00064）。
	MaterialCamera MaterialType = "camera"
)

func (m MaterialType) Valid() bool {
	return m == MaterialStyle || m == MaterialEffect || m == MaterialVoice || m == MaterialCamera
}

// Table 是这类素材的目录表（表名来自受控枚举而非用户输入，拼进 SQL 没有注入面）。
func (m MaterialType) Table() string {
	switch m {
	case MaterialEffect:
		return "effect"
	case MaterialVoice:
		return "voice"
	case MaterialCamera:
		return "camera_move"
	default:
		return "style"
	}
}

// TargetNodeType 返回该素材能修饰哪种节点。
//
// ⚠️ 风格只修饰图片、特效只修饰视频，**不可互换**（连线矩阵已有同样约束）。
// 这里再判一次，是因为素材可以经由"插入画布"进来，不一定走连线。
func (m MaterialType) TargetNodeType() (string, error) {
	switch m {
	case MaterialStyle:
		return "image", nil
	case MaterialEffect, MaterialCamera:
		return "video", nil
	default:
		return "", fmt.Errorf("素材 %s 不作为修饰输入", m)
	}
}

// MaterialPromptSuffix 把素材提示词拼成追加片段。
//
// ⚠️ 拼接发生在**服务端**（图片预设设计同理）：
// 前端拼出来的词会落进 canvas_data，用户改一个字玩法就失效，
// 而且历史节点永远停在旧词，调词就没法只靠改配置完成。
//
// 顺序固定：风格在前、特效在后。识别信息（用户描述）由调用方排在最前，
// 审美词必须排在内容之后，否则模型会先照着风格画，把用户要的内容挤掉。
func MaterialPromptSuffix(stylePrompts, effectPrompts []string) string {
	var parts []string
	for _, p := range stylePrompts {
		if s := strings.TrimSpace(p); s != "" {
			parts = append(parts, s)
		}
	}
	for _, p := range effectPrompts {
		if s := strings.TrimSpace(p); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "，")
}

// Material 是一条素材目录项。
type Material struct {
	Code         string
	Name         string
	Type         MaterialType
	Category     string
	Prompt       string
	Author       string
	IsCommercial bool
	CoverMediaID *int64
	Sort         int
	IsEnabled    bool
}

func ValidateMaterial(m Material) error {
	if !m.Type.Valid() {
		return fmt.Errorf("未知素材类型: %q", m.Type)
	}
	if strings.TrimSpace(m.Code) == "" || strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("code 与 name 必填")
	}
	// 编码会写进画布节点的参数（materialCode）：大小写、空格混进去的话同一个素材有两个名字。
	if err := checkCode("素材", m.Code, 64); err != nil {
		return err
	}
	if err := checkText("素材名称", m.Name, 64, true); err != nil {
		return err
	}
	if err := checkText("分类", m.Category, 32, false); err != nil {
		return err
	}
	if err := checkText("作者", m.Author, 64, false); err != nil {
		return err
	}
	// 风格/特效的价值就在提示词上，没有提示词等于插入一个不起作用的节点。
	if m.Type != MaterialVoice && strings.TrimSpace(m.Prompt) == "" {
		return fmt.Errorf("%s 必须配置提示词，否则插入画布后不产生任何效果", m.Type)
	}
	return nil
}

// Voice 是一条官方音色。
type Voice struct {
	Code            string
	Name            string
	UpstreamVoiceID string
	Language        string
	Gender          string
	IconMediaID     *int64
	PreviewMediaID  *int64
	Sort            int
	IsEnabled       bool
}

func ValidateVoice(v Voice) error {
	if strings.TrimSpace(v.Code) == "" || strings.TrimSpace(v.Name) == "" {
		return fmt.Errorf("code 与 name 必填")
	}
	if err := checkCode("音色", v.Code, 64); err != nil {
		return err
	}
	for _, c := range []struct {
		what string
		v    string
		max  int
	}{{"音色名称", v.Name, 64}, {"上游 voice ID", v.UpstreamVoiceID, 128}, {"语言", v.Language, 16}} {
		if err := checkText(c.what, c.v, c.max, false); err != nil {
			return err
		}
	}
	// ⚠️ 没有上游 voice ID 的音色无法真正合成，
	// 启用它等于让用户选一个必然失败的选项。
	if v.IsEnabled && strings.TrimSpace(v.UpstreamVoiceID) == "" {
		return fmt.Errorf("启用前必须配置上游 voice ID")
	}
	switch v.Gender {
	case "", "male", "female", "neutral":
	default:
		return fmt.Errorf("gender 取值非法: %q", v.Gender)
	}
	return nil
}
