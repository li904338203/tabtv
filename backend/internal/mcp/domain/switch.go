package domain

// SwitchVerdict 是两级开关对一次 MCP 请求的结论（MCPT-14，dev/01 §12 R7、dev/02 §5）。
type SwitchVerdict int

const (
	// SwitchAllow：放行。
	SwitchAllow SwitchVerdict = iota
	// SwitchNotFound：环境变量或平台总开关关着 —— 回 404（关掉就是不存在）。
	SwitchNotFound
	// SwitchWorkspaceDisabled：工作区管理员关掉了本工作区的 MCP —— 回 403 MCP_DISABLED。
	SwitchWorkspaceDisabled
)

// SwitchState 是做判定所需的三个事实。
type SwitchState struct {
	// EnvEnabled 是 SEEAITV_MCP_ENABLED（启动时决定是否挂载路由）。
	EnvEnabled bool
	// PlatformEnabled 是 mcp_setting.enabled（管理端运行时开关）。
	PlatformEnabled bool
	// WorkspaceEnabled 是 ¬workspace.mcp_disabled。平台层判定时（还不知道工作区）传 true。
	WorkspaceEnabled bool
}

// SwitchDecision 是两级开关的唯一判定。顺序有意义：环境变量 > 平台 > 工作区 ——
// 平台关着时不该泄露「这个工作区开没开」，所以平台层先回 404。
//
// ⚠️ 关闭**不撤销**任何 grant：开关是「暂停」，撤销是「收回」，两件事的恢复路径完全不同
// （前者重新打开即恢复，后者要用户重新授权）。混成一件的话，管理员为了排查临时关一下，
// 全体用户就得重新连一遍客户端。
func SwitchDecision(s SwitchState) SwitchVerdict {
	switch {
	case !s.EnvEnabled, !s.PlatformEnabled:
		return SwitchNotFound
	case !s.WorkspaceEnabled:
		return SwitchWorkspaceDisabled
	}
	return SwitchAllow
}
