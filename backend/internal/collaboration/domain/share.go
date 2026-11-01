package domain

import (
	"errors"
	"sort"
	"strings"
)

// ResourceType 是可以被共享的资源类别（与 ck_share_resource 一致）。
type ResourceType string

const (
	ResourceProject     ResourceType = "project"
	ResourceScriptAsset ResourceType = "script_asset"
	ResourceTimeline    ResourceType = "timeline"
)

func (t ResourceType) Valid() bool {
	switch t {
	case ResourceProject, ResourceScriptAsset, ResourceTimeline:
		return true
	}
	return false
}

// GranteeType 是受让方类别。
//
// user 与 workspace 两种**必须都有**：
//   - 只给 user，把一个项目共享给一个十人团队就要点十次，且有人入职后拿不到；
//   - 只给 workspace，就没法把一份资产单独给某一个外部合作者看。
type GranteeType string

const (
	GranteeUser      GranteeType = "user"
	GranteeWorkspace GranteeType = "workspace"
)

func (t GranteeType) Valid() bool {
	return t == GranteeUser || t == GranteeWorkspace
}

// shareable 是**可以被共享出去**的操作，顺序即 PermSet 的序列化顺序。
//
// ⚠️ 不含 manage/transfer：共享是把一份资源给外人看或用，不是把工作区的
// 管理权交出去 —— 授得出 manage 的话，受让方能反过来邀请成员、改别人的角色，
// 甚至把这份资源再共享给第三方，而源工作区的 owner 完全不知情。
var shareable = []Action{ActionView, ActionEdit, ActionGenerate, ActionExport}

// PermSet 是一次共享授权允许的操作集合。
type PermSet map[Action]bool

func (p PermSet) Can(a Action) bool { return p[a] }

func (p PermSet) Empty() bool { return len(p) == 0 }

// List 按 shareable 的固定顺序输出。
//
// ⚠️ 不能用 map 遍历：Go 的 map 顺序每次都不同，入库的字符串会在
// "view,edit" 与 "edit,view" 之间随机跳，而这两个值语义完全相同 ——
// 结果是审计里每次看都像"权限改过了"，diff 全是噪声。
func (p PermSet) List() []Action {
	out := make([]Action, 0, len(p))
	for _, a := range shareable {
		if p[a] {
			out = append(out, a)
		}
	}
	return out
}

// String 是入库形态：逗号分隔。
func (p PermSet) String() string {
	items := p.List()
	parts := make([]string, len(items))
	for i, a := range items {
		parts[i] = string(a)
	}
	return strings.Join(parts, ",")
}

// Union 求并集。
//
// ⚠️ 一个用户可能同时被两条有效授权命中：直接给他本人一条，他所在的工作区
// 又被给了一条。**必须取并集，不能取"第一条"** —— 取第一条的话，权限
// 取决于 SQL 的排序，而排序一改权限就变，且没有任何报错（同 §5.11 里
// superseded/expired 混用的那一类错误）。
func (p PermSet) Union(other PermSet) PermSet {
	out := make(PermSet, len(p)+len(other))
	for a := range p {
		out[a] = true
	}
	for a := range other {
		out[a] = true
	}
	return out
}

// ParsePermSet 解析入库形态。未知词一律丢弃（库里可能有历史值），
// 但**不会**因此报错 —— 一条授权的权限字段有脏值，不该让整个共享列表打不开。
func ParsePermSet(s string) PermSet {
	out := PermSet{}
	for _, raw := range strings.Split(s, ",") {
		a := Action(strings.TrimSpace(raw))
		for _, ok := range shareable {
			if a == ok {
				out[a] = true
			}
		}
	}
	return out
}

var (
	ErrNoPermissions   = errors.New("至少要授予一项权限")
	ErrNotShareable    = errors.New("该权限不能通过共享授予")
	ErrInvalidResource = errors.New("资源类别非法")
	ErrInvalidGrantee  = errors.New("受让方类别非法")
	ErrSelfShare       = errors.New("不能把资源共享给它自己所在的工作区")
	ErrNoShareRight    = errors.New("只有工作区管理者能对外共享资源")
	// ErrShareNotImportable 与 ErrNotShareable 分开：前者是"这类资源不支持导入"，
	// 后者是"这项权限不能共享"。合成一个会让用户以为自己权限不够。
	ErrShareNotImportable = errors.New("该类资源暂不支持导入到其他工作区")
)

// NormalizePermissions 校验并规范化请求里的权限列表。
//
// 两条规则：
//  1. 只接受 shareable 里的操作，其余明确报错而不是静默丢弃 ——
//     静默丢弃的话，管理员勾了"管理"却得到一条只读授权，界面上看不出区别。
//  2. **自动补上 view**：没有 view 的 edit 是自相矛盾的（看不见怎么编辑），
//     而真让它落库，受让方会在列表里看到这条共享却永远打不开。
func NormalizePermissions(raw []string) (PermSet, error) {
	if len(raw) == 0 {
		return nil, ErrNoPermissions
	}
	out := PermSet{}
	for _, s := range raw {
		a := Action(strings.TrimSpace(s))
		ok := false
		for _, s := range shareable {
			if a == s {
				ok = true
				break
			}
		}
		if !ok {
			return nil, ErrNotShareable
		}
		out[a] = true
	}
	if out.Empty() {
		return nil, ErrNoPermissions
	}
	out[ActionView] = true
	return out, nil
}

// GrantAsk 是一次共享授予请求（已从 HTTP 解析出来的形态）。
type GrantAsk struct {
	OwnerWorkspaceID int64
	Resource         ResourceType
	ResourceID       int64
	GranteeType      GranteeType
	GranteeID        int64
	Permissions      PermSet
}

// CheckGrant 判定一次授予是否合法。
//
// ⚠️ 授予权限要的是 **manage**，不是 edit。共享等于扩大这份资源的可见面 ——
// 那是一次对外发布决定（与"加精"同构），不是日常编辑动作。editor 能改画布
// 不等于他能决定谁看得见；反过来，若按 edit 放行，任何被拉进来的临时编辑
// 都能把整个项目共享出去，而 owner 在成员列表里看不到任何异常。
//
// 授予者「不能授出超过自己拥有的权限」这条不需要单独写：有 manage 的角色
// （owner/admin）本来就拥有 shareable 里的全部四项。
func CheckGrant(actor Role, ask GrantAsk) error {
	if !actor.Can(ActionManage) {
		return ErrNoShareRight
	}
	if !ask.Resource.Valid() {
		return ErrInvalidResource
	}
	if !ask.GranteeType.Valid() {
		return ErrInvalidGrantee
	}
	if ask.ResourceID <= 0 || ask.GranteeID <= 0 {
		return ErrInvalidGrantee
	}
	if ask.Permissions.Empty() {
		return ErrNoPermissions
	}
	// 共享给自己所在的工作区没有意义，而且会让"有效权限"出现两个来源：
	// 成员资格给一份、共享再给一份，两者冲突时按哪个算说不清楚。
	if ask.GranteeType == GranteeWorkspace && ask.GranteeID == ask.OwnerWorkspaceID {
		return ErrSelfShare
	}
	return nil
}

// Importable 判定某类资源能否被"导入到我的工作区"。
//
// 目前只有资产可导入。项目**明确不支持**而不是悄悄失败：项目的 canvasData
// 里存着一串产出 ID 与素材 ID，整份搬到另一个工作区后那些引用全部指向
// 别人的资源 —— 画布能打开，但每个节点的产出都是裂的，而这不会报错。
// 真要支持得先把引用逐条重绑（同 P04 的复制项目），那是独立的一件事。
func Importable(t ResourceType) bool { return t == ResourceScriptAsset }

// SortActions 供测试与展示使用的稳定排序。
func SortActions(in []Action) []Action {
	out := append([]Action(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
