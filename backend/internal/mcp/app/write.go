package app

import (
	"context"
	"encoding/json"
	"errors"
)

// 写工具（MCPT-11，开发 01 §4）的端口：任务取消、参考文件上传、写画布、写文档、建项目。
// 实现在 internal/wiring/mcp_write.go，全部调各域现有服务，不复制业务判断。
//
// ⚠️ 与读端口（read.go）分开放、各自一个接口，而不是往 Tasks / Media / Canvas / Documents / Projects
// 里加方法：只读端口的实现与假实现都不需要知道写操作的存在，读写也就不会在一个接口里互相牵连。
// ⚠️ 收的仍是 Principal：租户一律 Principal.WorkspaceID、角色一律 Principal.Role、操作者一律
// Principal.UserID（画布与文档上的「代谁写的」），授权 Principal.GrantID 决定节点归属。

// ErrNodeNotOwned 是写画布时改了不是本授权创建、或用户之后改过的节点（MCP-F-62）。
// 工具层翻成 MCP_NODE_NOT_OWNED（40912）。
var ErrNodeNotOwned = errors.New("节点不是本授权创建或已被用户修改")

// CancelResult 是一次取消的结果。Cancelled=false 表示任务已经结束（没有改任何东西，不是错误）。
type CancelResult struct {
	Cancelled bool
	// Task 是取消之后（或发现已结束时）任务的当前详情。
	Task TaskDetail
}

// TaskControl 取消任务（MCP-F-55）：按类分派到各域既有的取消入口，退款由原域完成、只退一次 ——
// 端口里**没有**任何与金额有关的参数（同 ops 的 Refunder，CD-63）。
// 导出任务没有取消入口（media 域只有 Create / Get / List / Run），返回 INVALID_ARGUMENT 并说明。
type TaskControl interface {
	Cancel(ctx context.Context, p Principal, kind string, id int64, reason string) (CancelResult, error)
}

// UploadSource 是参考文件的来源：URL 或已解码的字节（二选一，工具层已校验）。
type UploadSource struct {
	URL string
	// Data 是 base64 解码后的字节（≤5 MB，只收图片）。
	Data     []byte
	FileName string
	// DeclaredMIME 是客户端声明的类型，只作交叉校验（media 域按实际文件头判，GO-27）。
	DeclaredMIME string
	// ImageOnly 限定只收图片（base64 来源）。
	ImageOnly bool
}

// Reference 是上传好的参考文件（可作 generate 的 inputs[].referenceId）。
type Reference struct {
	ID         int64
	Kind       string // image | video | audio
	MIME       string
	SizeBytes  int64
	Width      int
	Height     int
	DurationMs int
}

// MediaUploader 上传参考文件（media 域 Upload，Purpose=upload_reference）。URL 来源走 Provider 的出网策略下载
// （生产禁内网、禁重定向、落临时文件），不进内存。
type MediaUploader interface {
	UploadReference(ctx context.Context, p Principal, src UploadSource) (Reference, error)
}

// CanvasApplyInput 是一批画布操作（存储形状，canvas/domain.Op 的 JSON）。
type CanvasApplyInput struct {
	ProjectID int64
	BatchID   string
	Ops       json.RawMessage
}

// CanvasApplyResult 是写画布的结果。
type CanvasApplyResult struct {
	NodeIDs     []string
	FromVersion int
	ToVersion   int
	Replayed    bool
}

// CanvasWriter 写画布：走画布域的 ApplyOps（canvas_op_log + canvas.ops_applied 事件，MCP-F-60），
// Actor{Kind:mcp, UserID, GrantID, ClientName}。所有权不符返回 ErrNodeNotOwned。
type CanvasWriter interface {
	Apply(ctx context.Context, p Principal, in CanvasApplyInput) (CanvasApplyResult, error)
}

// DocumentWriter 写项目文档：version=0 新建，否则乐观锁更新（冲突原样返回 40902 与「是谁改的」）；
// 来源记 mcp（00053）。
type DocumentWriter interface {
	Put(ctx context.Context, p Principal, projectID int64, path, content string, version int) (Document, bool, error)
}

// ProjectCreator 按幂等键建项目（canvas CreateWithIdemKey）：同键返回已有那个（replayed=true）。
// key 已由工具层按授权加前缀并哈希（domain.ProjectIdemKey）。
type ProjectCreator interface {
	Create(ctx context.Context, p Principal, name, key string) (ProjectSummary, string, bool, error)
}
