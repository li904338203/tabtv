package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ResourceView 是一份共享资源对受让方的展示形态。
//
// ⚠️ 它是**投影不是实体**：受让方拿到的永远是业务域挑出来给他看的字段，
// 不是仓储结构体。直接把 repo 的行序列化出去的话，加一个内部列就会漏给
// 一个本来不该看到它的人，而这不报错（同导出格式那条规则）。
type ResourceView struct {
	ID          int64
	WorkspaceID int64
	Name        string
	Kind        string
	UpdatedAt   time.Time
	// Detail 只在打开单个资源时填，列表不带 —— 列表要的是一行摘要，
	// 带上整份 canvasData 会让"共享给我的"这个列表变成几 MB 的响应。
	Detail json.RawMessage
}

// ErrResourceGone 表示资源已经不在了（被删、被彻底清理）。
//
// ⚠️ 与"查询出错"必须分开：混成一个的话，一次数据库抖动会让整张
// "共享给我的"列表报错，而正确的表现是那一行显示"来源已删除"、其余照常。
var ErrResourceGone = errors.New("共享的资源已不存在")

// ResourceProvider 由各业务域实现：按资源 ID 取一份共享视图。
//
// ⚠️ 实现方**不得**再带 workspace_id 条件。授权已经由 resource_share 给出，
// 再按调用者的工作区过滤只会永远查不到 —— 受让方本来就不在那个工作区里。
// 正因为这些查询绕过了租户条件，它们只能由本服务在校验完共享之后调用，
// 不许直接暴露成 HTTP 接口。
type ResourceProvider interface {
	ResourceType() domain.ResourceType
	Describe(ctx context.Context, id int64, detail bool) (ResourceView, error)
}

// ResourceImporter 由支持"导入到我的工作区"的业务域实现（目前只有资产）。
//
// 导入是在**受让方自己的工作区里新建一份**，不是跨工作区写 ——
// 后者会让一次导入直接改动别人的数据，而源工作区看不到任何痕迹。
type ResourceImporter interface {
	ImportTo(ctx context.Context, targetWorkspaceID, resourceID, actorUserID int64) (int64, error)
}

// RegisterProvider 注册资源提供方。
func (s *Service) RegisterProvider(p ResourceProvider) {
	if s.providers == nil {
		s.providers = map[domain.ResourceType]ResourceProvider{}
	}
	s.providers[p.ResourceType()] = p
}

// RegisterImporter 注册导入实现。
func (s *Service) RegisterImporter(t domain.ResourceType, im ResourceImporter) {
	if s.importers == nil {
		s.importers = map[domain.ResourceType]ResourceImporter{}
	}
	s.importers[t] = im
}

// GrantInput 是一次共享授予。
type GrantInput struct {
	Resource    domain.ResourceType
	ResourceID  int64
	GranteeType domain.GranteeType
	GranteeID   int64
	Permissions []string
}

// Grant 授予共享。
func (s *Service) Grant(ctx context.Context, workspaceID int64, actor domain.Role,
	actorID int64, in GrantInput) (GrantedItem, error) {
	perms, err := domain.NormalizePermissions(in.Permissions)
	if err != nil {
		return GrantedItem{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	ask := domain.GrantAsk{
		OwnerWorkspaceID: workspaceID, Resource: in.Resource, ResourceID: in.ResourceID,
		GranteeType: in.GranteeType, GranteeID: in.GranteeID, Permissions: perms,
	}
	if err := domain.CheckGrant(actor, ask); err != nil {
		return GrantedItem{}, mapShareErr(err)
	}

	// ⚠️ 资源必须**属于本工作区**才能被共享出去。少了这一条，任何工作区的
	// 管理员只要猜到一个项目 ID 就能把别人的项目共享给自己 —— 而共享行上
	// 记的 workspace_id 是他自己的，源工作区的 owner 在"我授出的共享"里
	// 一条都看不到，等于一个完全隐身的越权读取通道（AC-02）。
	p, ok := s.providers[in.Resource]
	if !ok {
		return GrantedItem{}, httpx.Errf(errcode.InvalidArgument, "暂不支持共享该类资源")
	}
	view, err := p.Describe(ctx, in.ResourceID, false)
	if errors.Is(err, ErrResourceGone) {
		return GrantedItem{}, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return GrantedItem{}, httpx.Wrap(errcode.Internal, err)
	}
	if view.WorkspaceID != workspaceID {
		// 不区分"不存在"与"不是你的"：区分开等于提供一个按 id 探测的接口。
		return GrantedItem{}, httpx.Err(errcode.NotFound)
	}

	// 受让方必须真的存在，否则共享列表里会挂着一条永远无人认领的授权。
	granteeName, err := s.checkGrantee(ctx, in.GranteeType, in.GranteeID)
	if err != nil {
		return GrantedItem{}, err
	}

	sh, err := s.repo.Grant(ctx, s.nextID(), repo.Share{
		WorkspaceID: workspaceID, Resource: in.Resource, ResourceID: in.ResourceID,
		GranteeType: in.GranteeType, GranteeID: in.GranteeID,
		Permissions: perms, GrantedBy: actorID,
	})
	if err != nil {
		return GrantedItem{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorUser, ActorID: actorID, WorkspaceID: workspaceID,
		Action: "share.grant", ObjectType: string(in.Resource), ObjectID: in.ResourceID,
		After: map[string]any{
			"granteeType": string(in.GranteeType),
			"granteeId":   fmt.Sprint(in.GranteeID),
			"permissions": perms.String(),
		},
	})
	// ⚠️ 这两个名字必须分开赋：GranteeName 是**受让方**的名字，
	// view.Name 是**资源**的名字。把后者塞进前者的话，界面上会显示
	// 「项目 A → 项目 A」，管理员完全看不出这份资源到底共享给了谁。
	sh.GranteeName = granteeName
	return GrantedItem{Share: sh, ResourceName: view.Name}, nil
}

// checkGrantee 校验受让方存在并回带它的名字。
func (s *Service) checkGrantee(ctx context.Context, t domain.GranteeType, id int64) (string, error) {
	var name string
	var ok bool
	var err error
	switch t {
	case domain.GranteeUser:
		name, ok, err = s.repo.UserName(ctx, id)
	case domain.GranteeWorkspace:
		name, ok, err = s.repo.WorkspaceName(ctx, id)
	}
	if err != nil {
		return "", httpx.Wrap(errcode.Internal, err)
	}
	if !ok {
		return "", httpx.Errf(errcode.NotFound, "受让方不存在")
	}
	return name, nil
}

// Revoke 撤销共享。
//
// ⚠️ 撤销**立刻生效**，不靠任何缓存过期：受让方的每次读取都实时查
// resource_share。这正是 NEW-18「撤销同步作用于 API」的落地方式 ——
// 把有效权限缓存进会话的话，撤销之后对方还能用到会话过期为止，
// 而这段时间有多长完全取决于他什么时候关浏览器。
func (s *Service) Revoke(ctx context.Context, workspaceID, shareID int64,
	actor domain.Role, actorID int64) error {
	if !actor.Can(domain.ActionManage) {
		return httpx.Err(errcode.InsufficientRole)
	}
	sh, err := s.repo.Revoke(ctx, workspaceID, shareID, actorID)
	if errors.Is(err, repo.ErrNotFound) {
		// 已经撤销过的再撤一次报 404 而不是静默成功：
		// 静默成功会让"撤销了但对方还看得到"这类问题无从分辨。
		return httpx.Errf(errcode.NotFound, "共享不存在或已撤销")
	}
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorUser, ActorID: actorID, WorkspaceID: workspaceID,
		Action: "share.revoke", ObjectType: string(sh.Resource), ObjectID: sh.ResourceID,
		Before: map[string]any{
			"granteeType": string(sh.GranteeType),
			"granteeId":   fmt.Sprint(sh.GranteeID),
			"permissions": sh.Permissions.String(),
		},
	})
	return nil
}

// ListGranted 列出本工作区授出的共享，并补上资源名。
func (s *Service) ListGranted(ctx context.Context, workspaceID int64,
	actor domain.Role) ([]GrantedItem, error) {
	if !actor.Can(domain.ActionManage) {
		return nil, httpx.Err(errcode.InsufficientRole)
	}
	rows, err := s.repo.ListGranted(ctx, workspaceID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]GrantedItem, 0, len(rows))
	for _, r := range rows {
		it := GrantedItem{Share: r, ResourceName: s.resourceName(ctx, r.Resource, r.ResourceID)}
		out = append(out, it)
	}
	return out, nil
}

// GrantedItem 是授予方视角的一行。
type GrantedItem struct {
	repo.Share
	// ResourceName 资源已被删除时是空串，前端显示"资源已删除"并仍给撤销入口 ——
	// 留着一条指向空气的授权比看不到它更糟。
	ResourceName string
}

// ReceivedItem 是受让方视角的一行。
type ReceivedItem struct {
	repo.Received
	ResourceName string
	// Gone 表示源资源已不存在。单独给一个布尔而不是把整行藏掉：
	// 藏掉的话用户会以为"共享被撤销了"，而实际上是对方把东西删了。
	Gone bool
	// Importable 是"能不能导入到我的工作区"，由资源类别与 export 权限共同决定。
	Importable bool
}

// ListReceived 列出共享给我的资源。
func (s *Service) ListReceived(ctx context.Context, userID int64) ([]ReceivedItem, error) {
	rows, err := s.repo.ListReceived(ctx, userID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]ReceivedItem, 0, len(rows))
	for _, r := range rows {
		it := ReceivedItem{Received: r}
		p, ok := s.providers[r.Resource]
		if !ok {
			it.Gone = true
		} else if v, err := p.Describe(ctx, r.ResourceID, false); err == nil {
			it.ResourceName = v.Name
		} else {
			// 单个资源取不到不中断整张列表（与整图预览里"一个节点报不出价
			// 不影响其余节点"是同一条）。
			it.Gone = true
		}
		it.Importable = domain.Importable(r.Resource) && r.Permissions.Can(domain.ActionExport)
		out = append(out, it)
	}
	return out, nil
}

// OpenShared 打开一份共享给我的资源。
//
// ⚠️ 这条路径**不经过工作区中间件**。受让方不是源工作区的成员，
// 带不出 X-Workspace-Id，也不该被要求带 —— 让他先"切换到别人的工作区"
// 才能看共享给他的东西，等于把共享退化成了临时成员资格。
// 授权的唯一来源是 resource_share，租户铁律（CD-04）一个字不改。
func (s *Service) OpenShared(ctx context.Context, userID int64,
	resource domain.ResourceType, resourceID int64) (ResourceView, domain.PermSet, error) {
	if !resource.Valid() {
		return ResourceView{}, nil, httpx.Errf(errcode.InvalidArgument, "资源类别非法")
	}
	perms, err := s.repo.EffectivePermissions(ctx, resource, resourceID, userID)
	if err != nil {
		return ResourceView{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	if !perms.Can(domain.ActionView) {
		// 已撤销与从未共享给出同一个错误：区分开就等于告诉对方
		// "这个资源确实存在，而且曾经共享给过你"。
		return ResourceView{}, nil, httpx.Err(errcode.ShareRevoked)
	}
	p, ok := s.providers[resource]
	if !ok {
		return ResourceView{}, nil, httpx.Errf(errcode.InvalidArgument, "暂不支持打开该类资源")
	}
	view, err := p.Describe(ctx, resourceID, true)
	if errors.Is(err, ErrResourceGone) {
		return ResourceView{}, nil, httpx.Errf(errcode.NotFound, "共享的资源已被删除")
	}
	if err != nil {
		return ResourceView{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	return view, perms, nil
}

// ImportShared 把一份共享给我的资源复制进我当前的工作区。
//
// 两道权限缺一不可：
//   - 对**源资源**要有 export（把内容取出去就是导出，不是查看）；
//   - 对**目标工作区**要有 edit（导入是在我的工作区里新建一份）。
//
// 少了后者，一个只读成员就能往团队工作区里塞东西；少了前者，
// 任何拿到只读共享的人都能把内容整份搬走，而"只读"这个词就不成立了。
func (s *Service) ImportShared(ctx context.Context, userID, targetWorkspaceID int64,
	target domain.Role, resource domain.ResourceType, resourceID int64) (int64, error) {
	if !target.Can(domain.ActionEdit) {
		return 0, httpx.Errf(errcode.InsufficientRole, "在当前工作区没有编辑权限，无法导入")
	}
	if !domain.Importable(resource) {
		return 0, httpx.Errf(errcode.InvalidArgument, domain.ErrShareNotImportable.Error())
	}
	perms, err := s.repo.EffectivePermissions(ctx, resource, resourceID, userID)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	if !perms.Can(domain.ActionView) {
		return 0, httpx.Err(errcode.ShareRevoked)
	}
	if !perms.Can(domain.ActionExport) {
		return 0, httpx.Errf(errcode.InsufficientRole, "这份共享是只读的，不能导入")
	}
	im, ok := s.importers[resource]
	if !ok {
		return 0, httpx.Errf(errcode.InvalidArgument, domain.ErrShareNotImportable.Error())
	}
	newID, err := im.ImportTo(ctx, targetWorkspaceID, resourceID, userID)
	if errors.Is(err, ErrResourceGone) {
		return 0, httpx.Errf(errcode.NotFound, "共享的资源已被删除")
	}
	if err != nil {
		// 业务域已经给出明确业务码的（例如媒体复制被配额挡下），原样透出 ——
		// 一律包成 Internal 会把一句用户能照着处理的提示变成"服务器错误"。
		var biz *httpx.BizError
		if errors.As(err, &biz) {
			return 0, err
		}
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, audit.Entry{
		ActorType: audit.ActorUser, ActorID: userID, WorkspaceID: targetWorkspaceID,
		Action: "share.import", ObjectType: string(resource), ObjectID: resourceID,
		After: map[string]any{"newResourceId": fmt.Sprint(newID)},
	})
	return newID, nil
}

func (s *Service) resourceName(ctx context.Context, t domain.ResourceType, id int64) string {
	p, ok := s.providers[t]
	if !ok {
		return ""
	}
	v, err := p.Describe(ctx, id, false)
	if err != nil {
		return ""
	}
	return v.Name
}

// SharedPermissions 供其他领域判定"这个用户凭共享对这个资源有什么权限"。
//
// 它是 NEW-18 在跨域侧的唯一入口：各业务域不自己查 resource_share，
// 各查一份的结果是撤销之后总有一处漏掉，而漏掉的那一处不会报错。
func (s *Service) SharedPermissions(ctx context.Context, resource string,
	resourceID, userID int64) (domain.PermSet, error) {
	return s.repo.EffectivePermissions(ctx, domain.ResourceType(resource), resourceID, userID)
}

func mapShareErr(err error) error {
	switch {
	case errors.Is(err, domain.ErrNoShareRight):
		return httpx.Errf(errcode.InsufficientRole, err.Error())
	default:
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
}
