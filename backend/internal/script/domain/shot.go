package domain

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var (
	ErrShotNotFound  = errors.New("镜头不存在")
	ErrTooManyShots  = errors.New("镜头数超过上限")
	ErrBadDuration   = errors.New("镜头时长超出范围")
	ErrEmptyMove     = errors.New("移动方向非法")
	ErrInputTooLong  = errors.New("输入超过模型上限")
	ErrEmptyInput    = errors.New("剧情输入不能为空")
	ErrBadSourceType = errors.New("脚本来源非法")
)

// SourceType 脚本来源（AV-05）。
type SourceType string

const (
	SourceScript    SourceType = "script"    // 剧本原文
	SourceCharacter SourceType = "character" // 角色参考
	SourceManual    SourceType = "manual"    // 自己写镜头
)

func ValidateSourceType(s SourceType) error {
	switch s {
	case SourceScript, SourceCharacter, SourceManual:
		return nil
	}
	return fmt.Errorf("%w: %q", ErrBadSourceType, s)
}

// ValidateRawInput 校验剧情输入。
//
// ⚠️ 长度上限**按字符不按 token**，且来自模型配置（`model.max_input_chars`），
// 不写死在 DTO 上：调用方手上只有字符串，token 数依分词器而异；
// 而不同模型的上限差一个数量级。写死在 DTO 上的后果是换个长上下文模型
// 也用不了 —— 那正是老系统里要修的 bug。
func ValidateRawInput(raw string, maxChars int) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ErrEmptyInput
	}
	if maxChars > 0 {
		if n := utf8.RuneCountInString(raw); n > maxChars {
			return fmt.Errorf("%w：当前 %d 字，上限 %d 字", ErrInputTooLong, n, maxChars)
		}
	}
	return nil
}

// Shot 是镜头表里的一行（领域视角，与仓储行一一对应）。
type Shot struct {
	ID          int64
	ShotNo      int
	DurationSec int
	Description string
	ShotSize    string
	Lighting    string
	Dialogue    string
	SoundEffect string
	CameraMove  string
	StartState  string
	EndState    string
	Entities    []Entity
	Version     int
}

// ValidateShot 校验单个镜头。
func ValidateShot(s Shot) error {
	if s.DurationSec < MinShotSeconds || s.DurationSec > MaxShotSeconds {
		return fmt.Errorf("%w：%d 秒不在 %d~%d 之间",
			ErrBadDuration, s.DurationSec, MinShotSeconds, MaxShotSeconds)
	}
	return nil
}

// Move 是镜头的移动方向。
type Move string

const (
	MoveUp   Move = "up"
	MoveDown Move = "down"
)

// Reorder 把 shots 里第 index 个镜头上移/下移一位，并重排镜号。
//
// ⚠️ 返回**新的切片**、不原地改：调用方通常还持有旧顺序用于比对
// （哪些镜头的 shotNo 变了 → 哪些下游 prompt 要标 stale）。
func Reorder(shots []Shot, index int, dir Move) ([]Shot, error) {
	if index < 0 || index >= len(shots) {
		return nil, ErrShotNotFound
	}
	target := index
	switch dir {
	case MoveUp:
		if index == 0 {
			// 已经在最上面：**原样返回而不是报错**。用户连点两下上移
			// 不该收到一个错误弹窗。
			return append([]Shot(nil), shots...), nil
		}
		target = index - 1
	case MoveDown:
		if index == len(shots)-1 {
			return append([]Shot(nil), shots...), nil
		}
		target = index + 1
	default:
		return nil, ErrEmptyMove
	}
	out := append([]Shot(nil), shots...)
	out[index], out[target] = out[target], out[index]
	return Renumber(out), nil
}

// Renumber 按切片顺序重排镜号，从 1 起连续。
//
// ⚠️ 镜号必须**连续且唯一**：它进唯一索引（script_id, shot_no），
// 也是用户口头交流的编号（"第 7 镜再来一版"）。留空号会让两边都乱。
func Renumber(shots []Shot) []Shot {
	for i := range shots {
		shots[i].ShotNo = i + 1
	}
	return shots
}

// InsertAfter 在第 index 个镜头之后插入一个新镜头并重排。
// index 为 -1 表示插到最前面。
func InsertAfter(shots []Shot, index int, newShot Shot) ([]Shot, error) {
	if len(shots) >= MaxShots {
		return nil, fmt.Errorf("%w：已有 %d 个镜头，上限 %d", ErrTooManyShots, len(shots), MaxShots)
	}
	if index < -1 || index >= len(shots) {
		return nil, ErrShotNotFound
	}
	if err := ValidateShot(newShot); err != nil {
		return nil, err
	}
	out := make([]Shot, 0, len(shots)+1)
	out = append(out, shots[:index+1]...)
	out = append(out, newShot)
	out = append(out, shots[index+1:]...)
	return Renumber(out), nil
}

// Duplicate 复制第 index 个镜头到它的后面。
//
// ⚠️ 复制出来的镜头**不带提示词状态**：提示词是按镜头内容算出来的，
// 原样继承会让副本一上来就是 ready，而它其实从没被合成过。
// 用户拿它去出图，出的是上一个镜头的画面。
func Duplicate(shots []Shot, index int) ([]Shot, error) {
	if index < 0 || index >= len(shots) {
		return nil, ErrShotNotFound
	}
	copyOf := shots[index]
	copyOf.ID = 0
	copyOf.Version = 1
	return InsertAfter(shots, index, copyOf)
}

// Remove 删除第 index 个镜头并重排。
func Remove(shots []Shot, index int) ([]Shot, error) {
	if index < 0 || index >= len(shots) {
		return nil, ErrShotNotFound
	}
	out := make([]Shot, 0, len(shots)-1)
	out = append(out, shots[:index]...)
	out = append(out, shots[index+1:]...)
	return Renumber(out), nil
}

// IndexOfShot 按 ID 找下标。
func IndexOfShot(shots []Shot, id int64) int {
	for i, s := range shots {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// Step 是脚本工作台的步骤（AV-18）。
type Step int

const (
	StepShots   Step = 1 // 镜头表
	StepAssets  Step = 2 // 资产
	StepPrompts Step = 3 // 提示词
)

// CanAdvanceTo 判断能否进到某一步。
//
// ⚠️ 前端已经会挡一道，服务端**仍然要独立校验**（AV-18 明确要求）：
// 前端的检查是体验，服务端的检查才是约束 —— 直接打 API 绕过前端
// 是最容易发生的事。
func CanAdvanceTo(target Step, shotCount int, assetsReady bool) error {
	switch target {
	case StepShots:
		return nil
	case StepAssets:
		if shotCount == 0 {
			return fmt.Errorf("还没有镜头，先生成或手动添加分镜")
		}
		return nil
	case StepPrompts:
		if shotCount == 0 {
			return fmt.Errorf("还没有镜头，先生成或手动添加分镜")
		}
		if !assetsReady {
			return fmt.Errorf("还有资产没有就绪，先完成资产生成或上传")
		}
		return nil
	}
	return fmt.Errorf("未知步骤 %d", target)
}
