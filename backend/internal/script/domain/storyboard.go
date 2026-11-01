// Package domain 是脚本域的纯规则层：分镜解析、镜头表约束、轮次与幂等。
//
// 模型输出的容错 JSON 解析在 `platform/lenientjson`（DRT-18 从本包下沉：导演台的 AI 搭景是第二个调用方）。
package domain

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/platform/lenientjson"
)

// 分镜生成的产出解析与清洗。

const (
	// MaxShots 单脚本镜头数上限（AV-09）。
	MaxShots = 200
	// MinShotSeconds / MaxShotSeconds 单镜时长范围。
	MinShotSeconds = 1
	MaxShotSeconds = 60
	// MaxEntityNameRunes 实体名长度上限。
	//
	// ⚠️ 这不是洁癖：中文**没有词边界**，正则里的 `\p{L}` 会把
	// 「@唐僧牵着马走在山路上」整串吞成一个名字。超长的名字一律丢弃
	// 比留着强 —— 留着的话资产库里会出现一个谁也认不出的"角色"，
	// 而且它会参与同名归并，把别的镜头也带偏。
	MaxEntityNameRunes = 12
)

// AssetType 是资产类型。
type AssetType string

const (
	AssetCharacter AssetType = "character"
	AssetScene     AssetType = "scene"
	AssetProp      AssetType = "prop"
)

func validAssetType(t AssetType) bool {
	return t == AssetCharacter || t == AssetScene || t == AssetProp
}

// Entity 是镜头里出现的一个实体。
type Entity struct {
	Name string    `json:"name"`
	Type AssetType `json:"type"`
}

// AssetProfile 是全片级的实体档案。
//
// ⚠️ 只写「**去掉临时状态后仍然成立**」的稳定事实。当下的服装、持有物、
// 表情属于状态而不是身份 —— 写进档案会让参考图带上一次性信息，
// 下游每一镜都继承那个瞬间的样子。
type AssetProfile struct {
	Name string    `json:"name"`
	Type AssetType `json:"type"`
	// Alias 别称（变身形态写明是谁所化，如「白骨精所化」）。
	Alias      string `json:"alias,omitempty"`
	Appearance string `json:"appearance,omitempty"`
	// Costume 仅角色：逐部位的服装。
	Costume string `json:"costume,omitempty"`
	// Features 角色与道具：辨识细节。
	Features string `json:"features,omitempty"`
	// Layers 仅场景：背景层与前景层各有什么。
	Layers string `json:"layers,omitempty"`
	// Mood 仅场景：氛围基调。
	Mood string `json:"mood,omitempty"`
	// Ambience 是 CD-76 时期契约里的场景氛围字段（那时还没有 mood）。
	// ⚠️ 保留读取：库里已有的档案用的是它，丢掉的话这些场景的氛围凭空消失。
	Ambience string `json:"ambience,omitempty"`
}

// SceneMood 场景的氛围：新档案写在 mood，CD-76 时期的写在 ambience。
func (p AssetProfile) SceneMood() string {
	if strings.TrimSpace(p.Mood) != "" {
		return p.Mood
	}
	return p.Ambience
}

// ParsedShot 是解析出的一个镜头。
type ParsedShot struct {
	ShotNo      int      `json:"shotNo"`
	Description string   `json:"description"`
	DurationSec int      `json:"durationSec"`
	ShotSize    string   `json:"shotSize,omitempty"`
	Lighting    string   `json:"lighting,omitempty"`
	Dialogue    string   `json:"dialogue,omitempty"`
	SoundEffect string   `json:"soundEffect,omitempty"`
	CameraMove  string   `json:"cameraMove,omitempty"`
	Entities    []Entity `json:"entities,omitempty"`
	// StartState / EndState 是**运动边界**：这一镜开始与结束时画面的状态。
	// 下游合成视频提示词时靠它衔接上下镜，缺了会出现"每镜各演各的"。
	StartState string `json:"startState,omitempty"`
	EndState   string `json:"endState,omitempty"`
}

// Storyboard 是一次分镜生成的完整产出。
type Storyboard struct {
	GlobalStyle string         `json:"globalStyle,omitempty"`
	Shots       []ParsedShot   `json:"shots"`
	Assets      []AssetProfile `json:"assets,omitempty"`
}

// ParseStoryboard 解析模型输出并清洗。
//
// ⚠️ **解析必须在结算之前跑**（这是老系统最贵的一个 bug）：
// 分镜 JSON 解析失败仍扣了 60 积分、库里落 0 个镜头，用户看到一个
// 空脚本和一笔说不清的消费。调用方的顺序必须是
// `reserve → 调模型 → 先解析 → 通过才 settle`，解析失败走退款。
func ParseStoryboard(raw string) (Storyboard, error) {
	var sb Storyboard
	if err := lenientjson.Decode(raw, &sb); err != nil {
		// 顶层是数组时按「只有镜头」处理：模型常常直接回一个镜头列表。
		var shots []ParsedShot
		if err2 := lenientjson.Decode(raw, &shots); err2 != nil {
			return Storyboard{}, err
		}
		sb.Shots = shots
	}
	if len(sb.Shots) == 0 {
		// ⚠️ 0 个镜头不是"成功但为空"，是失败：用户拿到一个空脚本
		// 却被扣了费，而且看不出哪里出了问题。
		return Storyboard{}, fmt.Errorf("模型没有返回任何镜头")
	}
	if len(sb.Shots) > MaxShots {
		return Storyboard{}, fmt.Errorf("镜头数 %d 超过上限 %d", len(sb.Shots), MaxShots)
	}

	known := knownNames(sb.Assets)
	for i := range sb.Shots {
		s := &sb.Shots[i]
		// 镜号一律**按顺序重排**，不信模型给的编号：模型跳号、重号都见过，
		// 而镜号是唯一索引的一部分，重号会让整批写入失败。
		s.ShotNo = i + 1
		s.DurationSec = clampDuration(s.DurationSec)
		s.Description = strings.TrimSpace(s.Description)
		s.Entities = normalizeEntities(s.Entities, s.Description, known)
	}
	sb.Assets = normalizeProfiles(sb.Assets)
	sb.GlobalStyle = strings.TrimSpace(sb.GlobalStyle)
	return sb, nil
}

// clampDuration 把时长夹到合法区间。
//
// ⚠️ 夹住而不是报错：模型偶尔给 0 或 120，为此让整批分镜作废
// （并触发一次退款与重来）不划算 —— 时长是用户随后就能改的字段。
// 真正该报错的是**结构性**问题（没有镜头、镜头太多）。
func clampDuration(d int) int {
	switch {
	case d < MinShotSeconds:
		return 5
	case d > MaxShotSeconds:
		return MaxShotSeconds
	}
	return d
}

func knownNames(profiles []AssetProfile) map[string]AssetType {
	out := map[string]AssetType{}
	for _, p := range profiles {
		name := strings.TrimSpace(p.Name)
		if name != "" && validAssetType(p.Type) {
			out[name] = p.Type
		}
	}
	return out
}

// normalizeEntities 清洗实体列表。
//
// 优先用模型给的结构化实体；模型没给才回落到从描述里扫 `@名字`。
//
// ⚠️ 回落是**兜底不是主路径**：正则的分类表只覆盖得了现代题材
// （天宫、金箍棒会被一股脑判成 character），而结构化产出里模型自己
// 标了类型。老系统反过来以正则为主，结果古装题材的资产类型基本全错。
func normalizeEntities(declared []Entity, description string, known map[string]AssetType) []Entity {
	seen := map[string]bool{}
	var out []Entity

	add := func(name string, typ AssetType) {
		name = strings.TrimSpace(name)
		name = strings.Trim(name, "@＠ 　")
		if name == "" || seen[name] {
			return
		}
		if utf8.RuneCountInString(name) > MaxEntityNameRunes {
			return
		}
		if !validAssetType(typ) {
			if k, ok := known[name]; ok {
				typ = k
			} else {
				typ = AssetCharacter
			}
		}
		seen[name] = true
		out = append(out, Entity{Name: name, Type: typ})
	}

	for _, e := range declared {
		add(e.Name, e.Type)
	}
	if len(out) > 0 {
		return out
	}
	for _, name := range ScanMentions(description, known) {
		add(name, known[name])
	}
	return out
}

// ScanMentions 从描述里扫 `@名字`。
//
// ⚠️ 中文没有词边界，所以**不能**用「@ 后面连续的字母」这种规则 ——
// `@唐僧牵着马` 会整串被吞掉。做法是：先按标点与空白切断，
// 再用**已知名单做最长前缀收敛**；收敛不了且超长就丢弃。
func ScanMentions(description string, known map[string]AssetType) []string {
	var out []string
	seen := map[string]bool{}
	runes := []rune(description)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '@' && runes[i] != '＠' {
			continue
		}
		j := i + 1
		for j < len(runes) && !isBreak(runes[j]) {
			j++
		}
		candidate := string(runes[i+1 : j])
		if candidate == "" {
			continue
		}
		name := narrow(candidate, known)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// narrow 把一段候选收敛成一个名字。
func narrow(candidate string, known map[string]AssetType) string {
	if utf8.RuneCountInString(candidate) <= MaxEntityNameRunes {
		if _, ok := known[candidate]; ok {
			return candidate
		}
	}
	// 用已知名做最长前缀匹配。
	best := ""
	for name := range known {
		if strings.HasPrefix(candidate, name) && len(name) > len(best) {
			best = name
		}
	}
	if best != "" {
		return best
	}
	// ⚠️ 收敛不了就**按长度决定去留**：短的多半是个真名字（`@小明`），
	// 长的多半是把整句吞了进来，留着比丢掉更糟。
	if utf8.RuneCountInString(candidate) <= MaxEntityNameRunes {
		return candidate
	}
	return ""
}

func isBreak(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '，', ',', '。', '.', '、', '；', ';', '：', ':',
		'"', '\u201c', '\u201d', '\'', '(', ')', '（', '）', '!', '！', '?', '？', '@', '＠':
		return true
	}
	return false
}

// normalizeProfiles 清洗档案：去空名、去重、排序（排序让 jsonb 快照稳定）。
func normalizeProfiles(list []AssetProfile) []AssetProfile {
	seen := map[string]bool{}
	out := make([]AssetProfile, 0, len(list))
	for _, p := range list {
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" || seen[p.Name] {
			continue
		}
		if utf8.RuneCountInString(p.Name) > MaxEntityNameRunes {
			continue
		}
		if !validAssetType(p.Type) {
			p.Type = AssetCharacter
		}
		seen[p.Name] = true
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
