// Package app 是营销域的编排层（管理端 / 用户端服务）。
//
// ⚠️ 本包不 import catalog / agent / media 的任何包（MKD-1）：推荐模型要问目录、默认技能要问
// Agent、封面要问媒体 —— **谁要用谁定义端口**，由 internal/wiring 适配（dev/01 §2.2）。
package app

import (
	"context"
	"time"

	"github.com/li904338203/tabtv/backend/internal/platform/audit"
)

// ModelFacts 是推荐模型校验要的事实（目录域提供）。
type ModelFacts struct {
	Code   string
	Name   string
	Domain string // text / image / video / audio
	// Enabled 是模型当前可用（model.status=enabled）。
	Enabled bool
	// OutputTypes 是该模型**启用的**操作绑定能产出的类型（operation.out_node_type 去重），
	// 用来判「是否支持对应操作」：创意产出 video 而模型只绑了图片操作时，推荐它没有意义。
	OutputTypes []string
}

// ModelLookup 按 code 批量查模型。返回的 map 里**没有**的 code 即「不存在」。
//
// 规则（dev/01 §12 #5 按建议）：不存在 → 拒绝保存（40001 点名字段）；
// 存在但停用 / 不支持对应产出 → 只告警（运营可以先配好、再上线模型）。
type ModelLookup interface {
	LookupModels(ctx context.Context, codes []string) (map[string]ModelFacts, error)
}

// SkillFacts 是默认技能校验要的事实（Agent 域提供）。
type SkillFacts struct {
	Code    string
	Enabled bool
	// ScenesKnown 为 false 表示提供方回答不了「属于哪些场景」，
	// 这时不判场景、只判存在。
	ScenesKnown bool
	Scenes      []string
}

// SkillLookup 按 code 批量查技能。map 里没有的 code 即「不存在」。
//
// ⚠️ dev/01 §2.1 的 ports.go 只列了 ModelLookup（B-17）：管理端「默认技能存在性」也要问 Agent，
// 本端口是补上的那一个。不存在 → 拒绝；停用 / 不属营销场景 → 告警。
type SkillLookup interface {
	LookupSkills(ctx context.Context, codes []string) (map[string]SkillFacts, error)
}

// MediaFacts 是封面 / 示例视频校验要的事实（媒体域提供）。
type MediaFacts struct {
	ID          int64
	WorkspaceID int64
	Purpose     string
	ContentType string
}

// MediaLookup 按 ID 查**仍有效**的媒体；不存在（或已删除）返回 found=false。
type MediaLookup interface {
	LookupMedia(ctx context.Context, id int64) (MediaFacts, bool, error)
}

// Auditor 写业务审计（platform/audit.Logger 实现）。
type Auditor interface {
	Write(ctx context.Context, e audit.Entry)
}

// Clock 给出当前时间（测试注入）。
type Clock func() time.Time

// ModelRef 是一个模型的 code 与显示名。
type ModelRef struct {
	Code string
	Name string
}

// ModelCatalog 列出**当前可用**的某产出模型（用户端营销接口用）。
//
// ⚠️ 口径与用户端 `/models?resultType=` 的下拉**同一个**（catalog.UsableModels：启用 ∧ Provider 就绪 ∧
// 至少绑了一个能用的操作），不在营销域另写一份判断 —— 否则「按模型」筛选项里有、而模型下拉里没有
// （或反过来），两边各自看都"没错"（MKT-AC-13）。
type ModelCatalog interface {
	UsableModels(ctx context.Context, outputType string) ([]ModelRef, error)
}

// PromptRenderer 渲染提示词模板（目录域 catalog.Service.RenderPrompt 实现，MKT-T-21）。
//
// 场景约束段 `agent.scene.marketing.constraint` 由它取：DB 行优先、代码兜底在目录域。
// ⚠️ 约束段**不钉版本**（dev/01 §12 #4 按建议）：运营改约束段后，钉住旧选项版本的会话
// 拿到的是新约束段 —— 约束是平台规则不是用户确认过的配方。
type PromptRenderer interface {
	RenderPrompt(ctx context.Context, code, modelCode string, vars map[string]string) (string, error)
}

// WorkspaceMediaLookup 按**工作区**查仍有效的媒体（商品库 / 角色库保存时校验图片，MKT-T-33）。
//
// 与 MediaLookup 分开：那一个只查平台素材（workspace_id = 0，选项封面），这一个只认用户工作区的文件。
// 合成一个方法加参数的话，总有一处会把 0 当成「任意工作区」传进去。
// found=false 表示不存在 / 已删除 / 不属于该工作区（**不区分**，区分等于提供按 ID 探测别人文件的口子）。
// ⚠️ 实现可能把平台素材（workspace_id=0）也查出来（媒体域 GetScoped 的口径）：调用方必须再比一次
// MediaFacts.WorkspaceID —— 商品图不能引用平台素材。
//
// （dev/05 §4.1 写的是「MediaLookup 加方法」；拆成独立端口是为了不让已有的假实现全部编译不过。）
type WorkspaceMediaLookup interface {
	LookupWorkspaceMedia(ctx context.Context, workspaceID, mediaID int64) (MediaFacts, bool, error)
}

// MediaDeleter 删除本工作区的一份媒体（media/app.Service.Delete 实现，MKD-31）。
//
// ⚠️ 必须走媒体域的 Delete（会跑全部引用检查器），**不要**直接改 media_object.status ——
// 那会绕过会话、时间线等的引用保护。被引用时它返回 RESOURCE_IN_USE，调用方保留文件即可。
type MediaDeleter interface {
	DeleteMedia(ctx context.Context, workspaceID, mediaID int64) error
}

// MediaCopier 把一份媒体**复制字节**进目标工作区，返回新媒体 ID（media/app.Service.CopyToWorkspace 实现，MKD-30）。
//
// ⚠️ 实现**不校验源归属**（它信任调用方已判定可见）：只能在判完「预置角色启用」之后、对预置角色
// 自己的媒体调用，**绝不**暴露成按媒体 ID 复制的接口。按 (目标工作区, 来源) 去重。
type MediaCopier interface {
	CopyToWorkspace(ctx context.Context, mediaID, targetWorkspaceID, actorUserID int64) (int64, error)
}
