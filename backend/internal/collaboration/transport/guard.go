package transport

import (
	"net/http"

	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 工作区内的统一权限闸（P12-03；NEW-18、AC-02、AC-24、FULL-06）。
//
// ⚠️ **起因是一个真实漏洞**：到 P12-02 为止，角色只在 `canvas/app` 里被检查过，
// 而 media / generation / script / workflow / timeline 五个域**一次都没查过**。
// 也就是说一个 `viewer`（只读成员）可以上传素材、**发起生成花掉团队的积分**、
// 改脚本、跑整张图、渲成片 —— 而后台看不出任何异常，那是一次完全正常的成功任务。
//
// ⚠️ 修法不是"在每个 app 方法里补一行 requireRole"。那种做法**漏掉任何一处
// 都不会有编译错误**，而漏掉的那一处就是一个隐身的越权口子（与提示词只在改动点
// 标 stale、与 NotificationSender 各域自己查表是同一类错误）。
// 闸门收在**路由层**，并且：
//
//  1. `GuardByMethod` 挂在整组路由上，**按 HTTP 方法给一个安全的默认**
//     （GET/HEAD → view，其余 → edit）。新增一条写路由时忘了标注，
//     它自动落在 edit 上而不是无人看管 —— **默认安全**。
//  2. 少数例外用 `RequireAction` 显式覆盖：会**扣费**的入口标 generate / export，
//     只读的 POST（报价、运行前预览）放宽回 view。
//
// 第 2 条的价值不只是精确：标出来之后，**"哪些接口会花用户的钱"在代码里一眼可见**，
// 而且共享授权（P12-01 的 view/edit/generate/export）将来能精确表达到这一层。

// GuardByMethod 按 HTTP 方法给出默认动作要求。
//
// ⚠️ 必须挂在 `RequireWorkspace` **之后**：它读的是那个中间件放进 context 的角色。
// 顺序反了的话拿不到上下文，而它会**放行**（见下面的注释）——
// 一个静默失效的安全闸比没有闸更糟，所以这里选择拿不到就 403。
func GuardByMethod(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := domain.ActionEdit
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			action = domain.ActionView
		}
		if !allow(w, r, action) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAction 显式要求某个动作，覆盖按方法推断的默认值。
//
// 用法：`r.With(collabhttp.RequireAction(domain.ActionGenerate)).Post(...)`。
func RequireAction(action domain.Action) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allow(w, r, action) {
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func allow(w http.ResponseWriter, r *http.Request, action domain.Action) bool {
	wc, ok := WorkspaceFrom(r.Context())
	if !ok {
		// ⚠️ 拿不到工作区上下文**一律拒绝**，不是放行。
		// 放行的话，任何一次中间件顺序写反都会让整组路由的权限闸静默消失 ——
		// 而那不会有任何报错，测试也照样绿。
		httpx.Fail(w, r, httpx.Err(errcode.NotWorkspaceMember))
		return false
	}
	if !wc.Role.Can(action) {
		httpx.Fail(w, r, httpx.Errf(errcode.InsufficientRole, denyMessage(wc.Role, action)))
		return false
	}
	return true
}

// denyMessage 说清**缺的是哪一项**，而不是一句"权限不足"。
//
// 用户看到"权限不足"时不知道下一步该做什么；说出"需要编辑权限，你当前是只读"
// 他就知道该去找管理员要什么。这与图片工具栏的阻塞原因分类是同一条：
// **说得出缺什么，用户才有下一步**。
func denyMessage(role domain.Role, action domain.Action) string {
	return "需要" + actionLabel(action) + "权限，你在该工作区的角色是" + roleLabel(role)
}

func actionLabel(a domain.Action) string {
	switch a {
	case domain.ActionView:
		return "查看"
	case domain.ActionEdit:
		return "编辑"
	case domain.ActionGenerate:
		return "生成"
	case domain.ActionExport:
		return "导出"
	case domain.ActionManage:
		return "管理"
	case domain.ActionTransfer:
		return "所有者"
	}
	return string(a)
}

func roleLabel(r domain.Role) string {
	switch r {
	case domain.RoleOwner:
		return "所有者"
	case domain.RoleAdmin:
		return "管理员"
	case domain.RoleEditor:
		return "编辑"
	case domain.RoleViewer:
		return "只读"
	}
	return string(r)
}

// Allows 供**处理器内部**判定动作，用于中间件看不到的情形。
//
// ⚠️ 只有一种情形该用它：**同一个端点里免费与收费两种语义并存**，
// 而区分它们的字段在**请求体**里（中间件只看得到方法与路径）。
// 目前唯一的例子是 `POST /timelines/{id}/renders`：`dryRun=true` 是免费预检
// （而且它报的"第几段缺素材"是诊断信息，只读成员帮忙看问题时需要），
// `dryRun=false` 才是那条链上最贵的一步。
//
// ⚠️ **不要**拿它去代替路由层的闸。那等于回到"每个方法手写一行"，
// 而漏掉任何一处都不会有编译错误 —— 那正是 P12-03 要修掉的东西。
func Allows(r *http.Request, action domain.Action) bool {
	wc, ok := WorkspaceFrom(r.Context())
	return ok && wc.Role.Can(action)
}

// DenyAction 构造一个说得出缺什么的 403，供 Allows 的调用方使用。
func DenyAction(r *http.Request, action domain.Action) error {
	wc, _ := WorkspaceFrom(r.Context())
	return httpx.Errf(errcode.InsufficientRole, denyMessage(wc.Role, action))
}
