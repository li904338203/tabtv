package protocol

import "sort"

// InputRole 是一个输入在请求里承担的角色。
//
// ⚠️ 与「媒体类型」不是一回事：两张 image 在 `first_last_frame` 里分别是首帧与尾帧，
// 在 `ref2video` 里都是参考图。角色是**协议能不能表达**这件事的最小单位 ——
// bblabu 的参考输入只有两个平铺数组，根本没有首帧/尾帧的位置可放。
type InputRole string

const (
	RoleText  InputRole = "text"
	RoleImage InputRole = "image"
	RoleAudio InputRole = "audio"
	RoleVideo InputRole = "video"
)

// OperationSupport 是协议对一个操作的支持声明。
type OperationSupport struct {
	// Roles 是该操作下这个协议**收得了**的输入角色。
	Roles []InputRole
	// MaxImages 为 0 表示不限（由能力矩阵按型号收紧）。
	MaxImages int
	MaxVideos int
}

// CapabilityDeclaring 由能说清自己支持范围的协议实现。
//
// ⚠️ 这是 GAP-22 / AC-10 的根子上的修法：老系统这份知识**只存在于 adapter 的
// Java 分支里**，管理端配不出来、报价路径看不见，于是「bblabu 首尾帧」这种组合
// 一路走到上游返回 400 才暴露 —— 那时积分已经扣了、任务已经排了队。
//
// 现在协议自己把支持范围说出来，目录域据此：
//   - 拒绝配置协议表达不了的能力矩阵行（后台当场报错，而不是运行时）；
//   - 发布检查把「绑了协议不支持的操作」列为阻塞原因；
//   - 报价与受理在扣费前就能拒。
//
// ⚠️ 声明的是**协议层的上限**，不是型号能力。哪个型号支持到 1080p、支持几张参考图，
// 仍然由 `model_param_support` / `model_input_capability` 按型号下发 ——
// 把型号能力写进代码，上游放开一档就得改代码重新发版。
type CapabilityDeclaring interface {
	// SupportedOperations 返回操作码 → 支持声明。
	SupportedOperations() map[string]OperationSupport
}

// Capabilities 取一个协议的支持声明；没有声明的协议返回 nil（表示不限制）。
//
// ⚠️ 返回 nil 与返回空 map 是**两件事**：nil = 这个协议没有表达能力边界的义务
// （通用/中转协议本来就不知道下游挂的是什么），空 map = 它明确说自己什么都不支持。
func Capabilities(code string) map[string]OperationSupport {
	p, ok := registry[code]
	if !ok {
		return nil
	}
	d, ok := p.(CapabilityDeclaring)
	if !ok {
		return nil
	}
	return d.SupportedOperations()
}

// SupportsOperation 判断协议支不支持某个操作。
// 未声明能力的协议一律返回 true —— 不知道就别拦，拦错的代价是把能用的组合挡在门外。
func SupportsOperation(code, operation string) bool {
	caps := Capabilities(code)
	if caps == nil {
		return true
	}
	_, ok := caps[operation]
	return ok
}

// SupportsRole 判断协议在某操作下收不收这个输入角色。
func SupportsRole(code, operation string, role InputRole) bool {
	caps := Capabilities(code)
	if caps == nil {
		return true
	}
	spec, ok := caps[operation]
	if !ok {
		return false
	}
	for _, r := range spec.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// OperationsOf 返回协议支持的操作码（排序，供管理端展示）。
func OperationsOf(code string) []string {
	caps := Capabilities(code)
	if caps == nil {
		return nil
	}
	out := make([]string, 0, len(caps))
	for op := range caps {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

// RolesOf 返回协议在某操作下支持的角色（排序）。
func RolesOf(code, operation string) []InputRole {
	caps := Capabilities(code)
	if caps == nil {
		return nil
	}
	spec, ok := caps[operation]
	if !ok {
		return nil
	}
	out := append([]InputRole(nil), spec.Roles...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
