// Package domain 是营销域（创意 / Hook / 风格）的规则层。
//
// ⚠️ 全部是纯函数：不碰库、不发网络、不取当前时间（`now` 由调用方传进来）。
// 这一域的每条规则都直接决定「用户看到什么、发给模型的是什么」，散在 service
// 里的话只能靠真库测试去验，而边界根本盖不全。
//
// ⚠️ 本包**不 import** agent / generation / catalog / canvas 的任何包（MKD-1）：
// 谁要用营销的规则，谁定义端口、在 wiring 里适配（dev/01 §2.2）。
// `TestDomain_不依赖其他业务域` 钉住这一条。
package domain

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Kind 是选项类型。
type Kind string

const (
	KindCreative Kind = "creative"
	KindHook     Kind = "hook"
	KindStyle    Kind = "style"
)

// Kinds 按**拼接顺序**列出全部类型：创意 → Hook → 风格（MKT-F-44）。
// 拼接、摘要、管理端分页签都按这个顺序，不按字典序。
var Kinds = []Kind{KindCreative, KindHook, KindStyle}

// Valid 判断类型是否合法。
func (k Kind) Valid() bool {
	return k == KindCreative || k == KindHook || k == KindStyle
}

// order 是类型在拼接顺序里的位置，未知类型排最后。
func (k Kind) order() int {
	for i, x := range Kinds {
		if x == k {
			return i
		}
	}
	return len(Kinds)
}

// Label 是类型的中文名（错误信息与摘要用）。
func (k Kind) Label() string {
	switch k {
	case KindCreative:
		return "创意"
	case KindHook:
		return "Hook"
	case KindStyle:
		return "风格"
	}
	return string(k)
}

// codePattern 是选项 code 的格式（MKT-D-21）：英文小写下划线、字母开头、3~64 位。
// ⚠️ code 建后不可改：历史会话、生成快照、版本历史都靠它追溯。
var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)

// ValidCode 判断 code 格式是否合法。
func ValidCode(code string) bool { return codePattern.MatchString(code) }

// 产出类型（MKT-D-28）。
const (
	OutputVideo = "video"
	OutputImage = "image"
)

// 输入要求的取值（MKT-D-29）。
const (
	RequirementRequired = "required"
	RequirementOptional = "optional"
)

// Option 是一条营销选项（当前版本）。字段与 `marketing_option` 一一对应（dev/02 §2.1）。
type Option struct {
	Kind         Kind
	Code         string
	Name         string
	CategoryCode string
	Description  string

	CoverMediaIDs  []int64
	PreviewMediaID *int64

	// Prompt 是服务端片段。⚠️ 用户端 DTO 上**不存在**这个字段（MKT-AC-06）。
	Prompt string

	OutputTypes       []string
	InputRequirements map[string]string // 槽位 key → required / optional
	IsAudioRequired   bool

	// 以下三项只给创意（MKT-D-31~33）。
	RecommendedModels []string
	RecommendedParams json.RawMessage
	DefaultSkillCode  string

	Tags []string

	StartAt *time.Time
	EndAt   *time.Time

	Sort      int
	IsEnabled bool
	Version   int
}

// Category 是分类字典的一项。
type Category struct {
	Kind      Kind
	Code      string
	Name      string
	Sort      int
	IsEnabled bool
}

// Pick 是一次「钉住」：会话与生成参数里只存 (kind, code, version)，
// 原文从只增不改的版本历史表取（MKD-5）。⚠️ 不复制片段原文 —— 复制的话片段会落进
// 节点 params，也就是 canvasData（MK-8 禁止）。
type Pick struct {
	Kind    Kind   `json:"kind"`
	Code    string `json:"code"`
	Version int    `json:"version"`
}

// Snapshot 是某个 (code, version) 的原文，来自 `marketing_option_version.snapshot`。
type Snapshot struct {
	Pick
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	Prompt            string            `json:"prompt"`
	Tags              []string          `json:"tags"`
	IsAudioRequired   bool              `json:"isAudioRequired"`
	OutputTypes       []string          `json:"outputTypes"`
	InputRequirements map[string]string `json:"inputRequirements"`
}

// UserVisibleFields 是选项上**面向用户**的文案，口播校验（CheckTalkCopy）只看这些。
//
// ⚠️ 片段（prompt）不在里面：它不下发前端，用户看不到；把它也拿去查「缺提示句」
// 会逼运营往给模型的片段里塞一句给人看的免责声明。
func UserVisibleFields(o Option) map[string]string {
	return map[string]string{
		"name":        o.Name,
		"description": o.Description,
	}
}

// HasTag 判断是否带某个标签（去掉两端空白后逐字比较）。
func HasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if strings.TrimSpace(t) == tag {
			return true
		}
	}
	return false
}
