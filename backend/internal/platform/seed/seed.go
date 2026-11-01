// Package seed 幂等写入基础配置。
//
// ⚠️ seed 只含**非用户业务配置**：节点、控件、连线规则、operation、
// 协议目录、权限、角色、默认提示词、项目模板（10 §1）。
// **不含**任何用户、余额、订单、项目、媒体 —— 新系统从空账本开始。
//
// 幂等靠 `on conflict do update`，不靠"只跑一次"：
// INIT-01 要求 migration 与 seed 都能在空库**重复**验证。
package seed

import (
	"context"
	"database/sql"
	"fmt"
)

// Apply 执行全部 seed。整体一个事务：部分写入的配置比没有配置更难排查。
func Apply(ctx context.Context, db *sql.DB, nextID func() int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 seed 事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	steps := []struct {
		name string
		fn   func(context.Context, *sql.Tx, func() int64) error
	}{
		{"api_protocol", seedProtocols},
		{"permission", seedPermissions},
		{"role", seedRoles},
		{"node_type", seedNodeTypes},
		{"param_def+node_param", seedParams},
		{"operation", seedOperations},
		{"connection_rule", seedConnectionRules},
		{"image_preset", seedImagePresets},
		{"camera_move", seedCameraMoves},
		{"project_template", seedTemplates},
		{"pricing(render)", seedRenderPricing},
		{"pricing(director)", seedDirectorPricing},
		{"pricing(smartcut)", seedSmartcutPricing},
		{"member_catalog", seedMemberCatalog},
		{"agent_skill", seedAgentSkills},
		{"config_revision", seedConfigRevision},
	}
	for _, s := range steps {
		if err := s.fn(ctx, tx, nextID); err != nil {
			return fmt.Errorf("seed %s 失败: %w", s.name, err)
		}
	}
	return tx.Commit()
}

// api_protocol 的 is_registered 由进程启动时按代码里的 adapter registry 覆盖；
// 这里只写目录行，保证管理端下拉有内容（修 GAP-09）。
func seedProtocols(ctx context.Context, tx *sql.Tx, _ func() int64) error {
	rows := []struct {
		code       string
		name       string
		async      bool
		needsQuery bool
	}{
		{"dev_fake", "开发假适配", false, false},
		{"openai_chat", "OpenAI Chat", false, false},
		{"anthropic_messages", "Anthropic Messages", false, false},
		{"openai_images", "OpenAI Images", false, false},
		{"openai_tts", "OpenAI TTS", false, false},
		{"generic_sync", "通用同步 HTTP", false, false},
		{"generic_async", "通用异步 HTTP", true, true},
	}
	for _, r := range rows {
		_, err := tx.ExecContext(ctx, `
			insert into api_protocol (code, name, is_async, requires_query_path)
			values ($1, $2, $3, $4)
			on conflict (code) do update set
				name = excluded.name,
				is_async = excluded.is_async,
				requires_query_path = excluded.requires_query_path,
				updated_at = now()`,
			r.code, r.name, r.async, r.needsQuery)
		if err != nil {
			return err
		}
	}
	return nil
}

// permissionSeeds 是全部权限位（包级变量：排除项断言要逐条扫它，MKT-T-32 / MKT-AC-23）。
// permission.code 是后端鉴权与前端路由 meta 的共同契约，迁移后不得随意改名。
var permissionSeeds = []struct{ code, name, module string }{
	{"system:read", "系统查看", "system"},
	{"system:write", "系统配置", "system"},
	// ⚠️ 日志单开权限位，不随 system:read 放开：
	// 日志含脱敏后的请求体，仍属敏感面。
	{"system:log", "请求日志查看", "system"},
	{"user:read", "用户查看", "user"},
	{"user:write", "用户管理", "user"},
	{"member:read", "会员商品查看", "member"},
	{"member:write", "会员商品管理", "member"},
	{"order:read", "订单查看", "order"},
	{"order:write", "订单处理", "order"},
	// ⚠️ 人工调账**单开权限位**，不随 order:write 放开：
	// 它是后台唯一能**直接改用户资产**的操作，处理退款的客服
	// 不该顺手就能给任意账户加积分（同 system:log 单开的考量）。
	{"credits:adjust", "人工积分调账", "order"},
	{"canvas:read", "画布配置查看", "canvas"},
	{"canvas:write", "画布配置管理", "canvas"},
	{"model:read", "模型配置查看", "model"},
	{"model:write", "模型配置管理", "model"},
	{"voice:read", "音色查看", "voice"},
	{"voice:write", "音色管理", "voice"},
	{"content:read", "运营内容查看", "content"},
	{"content:write", "运营内容管理", "content"},
	{"product:read", "产品模块查看", "product"},
	{"product:write", "产品模块管理", "product"},
	{"review:read", "作品审核查看", "review"},
	{"review:write", "作品审核处理", "review"},
	{"auth:read", "认证配置查看", "auth"},
	{"auth:write", "认证配置管理", "auth"},
	{"rbac:read", "管理员权限查看", "rbac"},
	{"rbac:write", "管理员权限管理", "rbac"},
	{"ops:read", "任务运维查看", "ops"},
	{"ops:write", "任务运维操作", "ops"},
	{"prompt:read", "提示词查看", "prompt"},
	{"prompt:write", "提示词管理", "prompt"},
	{"audit:read", "业务审计查看", "audit"},
	// 管理端 Agent 技能配置（docs/agent AGT-60）。
	{"agent:read", "技能查看", "agent"},
	{"agent:write", "技能管理", "agent"},
	// 对外 MCP 的平台级连接管理（docs/mcp MCPT-14）：查看全站连接与统计 / 撤销连接与平台总开关。
	// ⚠️ 单开一对，不随 system:* 放开：撤销的是用户自己授予外部客户端的访问，
	// 总开关一关全站的外部客户端同时断开 —— 配水印的人不该顺手就能做这两件事。
	{"mcp:read", "MCP 连接查看", "mcp"},
	{"mcp:write", "MCP 连接管理", "mcp"},
}

// permission.code 是后端鉴权与前端路由 meta 的共同契约，迁移后不得随意改名。
func seedPermissions(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	for _, p := range permissionSeeds {
		_, err := tx.ExecContext(ctx, `
			insert into permission (id, code, name, module) values ($1, $2, $3, $4)
			on conflict (code) do update set name = excluded.name, module = excluded.module`,
			nextID(), p.code, p.name, p.module)
		if err != nil {
			return err
		}
	}
	return nil
}

func seedRoles(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	roles := []struct{ code, name, desc string }{
		{"super_admin", "超级管理员", "拥有全部权限"},
		{"operator", "运营", "内容运营与审核"},
		{"viewer", "只读", "仅查看，不能执行写操作"},
	}
	for _, r := range roles {
		if _, err := tx.ExecContext(ctx, `
			insert into role (id, code, name, description) values ($1, $2, $3, $4)
			on conflict (code) do update set name = excluded.name, description = excluded.description,
				updated_at = now()`,
			nextID(), r.code, r.name, r.desc); err != nil {
			return err
		}
	}

	// super_admin 拿全部权限。
	// ⚠️ 用真实的关联行而不是代码里的通配判断：通配是不可审计的隐式规则，
	// 「这个管理员到底能做什么」必须能从库里查出来（06 §1）。
	if _, err := tx.ExecContext(ctx, `
		insert into role_permission (id, role_id, permission_id)
		select (random() * 9e18)::bigint, r.id, p.id
		from role r cross join permission p
		where r.code = 'super_admin'
		on conflict (role_id, permission_id) do nothing`); err != nil {
		return err
	}

	// viewer 只拿读权限。
	if _, err := tx.ExecContext(ctx, `
		insert into role_permission (id, role_id, permission_id)
		select (random() * 9e18)::bigint, r.id, p.id
		from role r cross join permission p
		where r.code = 'viewer' and p.code like '%:read'
		on conflict (role_id, permission_id) do nothing`); err != nil {
		return err
	}

	// operator 拿内容与审核。
	if _, err := tx.ExecContext(ctx, `
		insert into role_permission (id, role_id, permission_id)
		select (random() * 9e18)::bigint, r.id, p.id
		from role r cross join permission p
		where r.code = 'operator' and p.module in ('content','review','product')
		on conflict (role_id, permission_id) do nothing`); err != nil {
		return err
	}
	return nil
}

// nodeTypeRows 是 node_type 的 seed 行。提到包级是为了让画布技能的附属文档
// 「节点与连线」（agent_skill.go）从同一份数据生成，不另编一份（AGT-30）。
var nodeTypeRows = []struct {
	code, name, kind string
	sort             int
}{
	{"text", "文本", "generator", 1},
	{"image", "图片", "generator", 2},
	{"video", "视频", "generator", 3},
	{"audio", "音频", "generator", 4},
	{"script", "脚本", "composite", 5},
	{"group", "组", "composite", 6},
	// 素材节点无独立生成能力，只作为目标节点生成时的修饰输入。
	{"style", "风格", "material", 7},
	{"effect", "特效", "material", 8},
	// 文档节点（AGT-09）：只存 params.docId，正文在 project_document。
	// kind=material 是借用「没有独立生成能力」这层语义；**不配任何 connection_rule** ——
	// 它不能作为可执行输入，整图解析会把它和触及它的连线一起剔除。
	{"document", "文档", "material", 9},
	// 3D 导演台（docs/agent/director，DRT-05）：节点只存 params.sceneId，场景在 director_scene。
	// kind=composite：它不是生成节点（没有模型、不扣费），也不是素材（素材节点不接收输入，而导演台要收全景 / 参考图）。
	// ⚠️ 不放进整图的 nonExecutableTypes（DRT-06）：那会连同它的出边一起剔除，下游就收不到截图了。
	{"director", "3D 导演台", "composite", 10},
	// 智能剪辑（docs/agent/smartcut SCT-08）：节点只存 params.timelineId 与用户输入，时间线在 timeline。
	// kind=composite：本身不调模型（AI 剪辑免费、导出不扣费），也不是素材（它要收上游的视频 / 图片 / 音频 / 文字）。
	// ⚠️ 不放进整图的 nonExecutableTypes：整图把它当静态源（SCT-09），剔除的话下游就收不到成片了。
	{"smartcut", "智能剪辑", "composite", 11},
}

func seedNodeTypes(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	for _, n := range nodeTypeRows {
		if _, err := tx.ExecContext(ctx, `
			insert into node_type (id, code, name, kind, sort) values ($1, $2, $3, $4, $5)
			on conflict (code) do update set name = excluded.name, kind = excluded.kind,
				sort = excluded.sort, updated_at = now()`,
			nextID(), n.code, n.name, n.kind, n.sort); err != nil {
			return err
		}
	}
	return nil
}

// operationRows 是 operation 的 seed 行。提到包级是为了让「每条连线规则都能通过后台保存校验」的测试
// 直接拿同一份数据（3D 导演台 DRT-05：director 要进默认操作的 in_node_types，否则运营一点保存就报对不上）。
var operationRows = []struct {
	code, name, domain, inTypes, outType string
}{
	{"text2text", "文本生成", "text", "text", "text"},
	{"text2img", "文生图", "image", "text,style", "image"},
	{"img2img", "图生图", "image", "image,style,director", "image"},
	{"img2prompt", "图片反推", "text", "image", "text"},
	// 涂抹重绘与抠图（NEW-13）：都吃图，且重绘要额外吃一张蒙版。
	// ⚠️ 蒙版走 image 角色 + `role:"mask"` 标记，不另立节点类型 ——
	// 它本来就是一张图，只是说的是"改哪一块"。
	{"inpaint", "涂抹重绘", "image", "image,style", "image"},
	{"remove_bg", "抠图", "image", "image", "image"},
	{"text2video", "文生视频", "video", "text,effect", "video"},
	{"img2video", "图生视频", "video", "image,effect,director", "video"},
	{"first_last_frame", "首尾帧", "video", "image,effect,director", "video"},
	{"ref2video", "全能参考", "video", "text,image,audio,video,effect,director", "video"},
	// smartcut：智能剪辑节点导出的成片可以接视频 / 文本节点（SCT-08，连线规则见下）。
	{"video2video", "视频生视频", "video", "video,effect,smartcut", "video"},
	{"video2prompt", "视频反推", "text", "video,smartcut", "text"},
	{"text2audio", "文本转语音", "audio", "text", "audio"},
	{"asset_gen", "资产图生成", "image", "text", "image"},
	{"storyboard_gen", "分镜生成", "text", "text", "text"},
	{"prompt_compose", "提示词合成", "text", "text", "text"},
	// 时间线渲染（NEW-10）。
	//
	// ⚠️ 它**不调用任何上游模型**，但仍然走定价目录：渲染真实消耗
	// CPU 与存储，而"渲染多少钱"必须只有一个答案 ——
	// 另写一份价格的话运营只会改其中一个。
	//
	// ⚠️ `in_node_types` 为空：它的输入不是画布节点，是一条时间线。
	{"timeline_render", "时间线渲染", "video", "", "video"},
	// 画布 Agent 的一次对话调用（AGT-04）。推理模型绑定它并配价，Agent 每轮按它报价
	// （复用会员等级与可用性合取式）。
	//
	// ⚠️ 它**不是画布节点的操作**：不进 connection_rule，用户端 `/models` 不带
	// agentCapable 时也不把它算作可用操作 —— 否则只绑了它的推理模型会出现在
	// 文本节点的下拉里，选了又报不出 text2text 的价。
	{"agent_chat", "Agent 对话", "text", "text", "text"},
	// 3D 导演台的 AI 搭景（docs/agent/director，DRT-19）：同步调一次看图的文本模型，产出搭景方案（不是媒体）。
	//
	// ⚠️ 同 agent_chat：**不是画布节点的操作**，不进 connection_rule、没有产出节点类型；用户端 `/models`
	// 也不把它算作可用操作。输入能力沿用同一模型的 text2text（catalog/domain.CapabilityInheritance）。
	// ⚠️ 免费靠 seedDirectorPricing 的 0 积分兜底价成立，不是代码特例（DRD-22）。
	{"scene_build", "AI 搭景", "text", "text,image", ""},
	// 智能剪辑（docs/agent/smartcut SCT-08）。
	//
	// AI 剪辑的一次规划调用：同 scene_build，**不是画布节点的操作**（不进 connection_rule、没有产出节点类型），
	// 用户端 `/models` 不把它算作可用操作；输入能力沿用同一模型的 text2text；免费靠 0 积分兜底价（SCK-2）。
	{"smartcut_plan", "AI 剪辑", "text", "", ""},
	// 以下四个只在剪辑台里用（片段级生成、语音识别，SCK-5），按模型定价、不配兜底价（SCK-4）。
	// ⚠️ 同样不是画布连线的操作：没有对应的 connection_rule；`/models` 也排除它们，只绑了它们的模型
	// 不该出现在画布节点的模型下拉里。
	{"speech2text", "语音识别", "text", "audio", "text"},
	{"video_desubtitle", "去字幕", "video", "video", "video"},
	{"text2music", "生成配乐", "audio", "text", "audio"},
	{"video2audio", "视频配音效", "audio", "video", "audio"},
}

func seedOperations(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	for _, o := range operationRows {
		if _, err := tx.ExecContext(ctx, `
			insert into operation (id, code, name, domain, in_node_types, out_node_type)
			values ($1, $2, $3, $4, $5, $6)
			on conflict (code) do update set name = excluded.name, domain = excluded.domain,
				in_node_types = excluded.in_node_types, out_node_type = excluded.out_node_type,
				updated_at = now()`,
			nextID(), o.code, o.name, o.domain, o.inTypes, o.outType); err != nil {
			return err
		}
	}
	return nil
}

// 连线矩阵（03 §2.6 / 数据模型）：
// 文→图/视/音/文 · 图→文/图/视 · 视→文/视 · 音→视 · 风格→仅图片 · 特效→仅视频。
//
// 提到包级同 nodeTypeRows：画布技能的「节点与连线」由它生成（AGT-30）。
var connectionRuleRows = []struct{ src, dst, defaultOp string }{
	{"text", "text", "text2text"},
	{"text", "image", "text2img"},
	{"text", "video", "text2video"},
	{"text", "audio", "text2audio"},
	{"image", "text", "img2prompt"},
	{"image", "image", "img2img"},
	{"image", "video", "img2video"},
	{"video", "text", "video2prompt"},
	{"video", "video", "video2video"},
	{"audio", "video", "ref2video"},
	// 脚本节点（2026-10-30 用户反馈：文本连不上脚本）：文本是剧本原文 / 参考文本，图片是角色参考（「角色」入口）。
	// 脚本不是按连线生成的节点（分镜与提示词在脚本工作台里同步生成），default_operation 为空。
	// ⚠️ 此前缺这两条，不只是拖线连不上：脚本节点自己的「剧本 / 角色」入口建上游节点前也走同一个校验，一样被拒。
	{"text", "script", ""},
	{"image", "script", ""},
	// 素材只修饰，不产生独立操作 —— default_operation 为空。
	{"style", "image", ""},
	{"effect", "video", ""},
	// 3D 导演台（DRT-05，DRD-20）：上游图片作全景背景或搭景参考；输出截图交给图片 / 视频节点。
	// ⚠️ 默认操作要在 operation.in_node_types 里含 director，否则运营在后台保存这条规则时校验不过（rule_admin.go）。
	// 连视频节点默认全能参考：截图是构图参考，不是首帧（DRK-5）；实际以视频节点所选模式为准。
	{"image", "director", ""},
	{"director", "image", "img2img"},
	{"director", "video", "ref2video"},
	// 智能剪辑（SCT-08，SC-F-04 / SC-F-05）：上游素材连进来即入轨（不带默认操作 —— 剪辑节点不是生成节点）；
	// 导出的成片交给视频节点（视频生视频）或文本节点（视频反推）。
	// ⚠️ 默认操作要在 operation.in_node_types 里含 smartcut，否则运营在后台保存这条规则时校验不过。
	{"video", "smartcut", ""},
	{"image", "smartcut", ""},
	{"audio", "smartcut", ""},
	{"text", "smartcut", ""},
	{"script", "smartcut", ""},
	{"group", "smartcut", ""},
	{"smartcut", "video", "video2video"},
	{"smartcut", "text", "video2prompt"},
}

func seedConnectionRules(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	for _, r := range connectionRuleRows {
		var op any
		if r.defaultOp != "" {
			op = r.defaultOp
		}
		if _, err := tx.ExecContext(ctx, `
			insert into connection_rule (id, source_type, target_type, is_allowed, default_operation)
			values ($1, $2, $3, true, $4)
			on conflict (source_type, target_type) do update set
				is_allowed = excluded.is_allowed,
				default_operation = excluded.default_operation,
				updated_at = now()`,
			nextID(), r.src, r.dst, op); err != nil {
			return err
		}
	}
	return nil
}

// seedConfigRevision 建立并**推进**配置版本。
//
// ⚠️ 这一步刻意不是「已存在就跳过」：seed 刚刚改过 node_type / param_def /
// node_param / connection_rule，而配置接口的缓存键含 revision（CD-14）。
// 不 bump 的话，跑完 `make seed` 的 API 会继续发旧快照 ——
// P04-02 实测踩到：参数刚 seed 进库，`GET /canvas/config` 里的 params 仍是空的，
// 看起来像 seed 没生效，其实是缓存没换键。
//
// 它也是 seed 里**唯一按次递增**的东西。这不破坏幂等的承诺：
// 幂等指的是「重复执行不会多出业务数据」，而 revision 是缓存失效计数器，
// 重跑 seed 本来就应该让缓存失效。
func seedConfigRevision(ctx context.Context, tx *sql.Tx, _ func() int64) error {
	for _, scope := range []string{"canvas", "model", "material"} {
		if _, err := tx.ExecContext(ctx, `
			insert into config_revision (scope, revision) values ($1, 1)
			on conflict (scope) do update set
				revision = config_revision.revision + 1, updated_at = now()`, scope); err != nil {
			return err
		}
	}
	return nil
}

// seedRenderPricing 给时间线渲染配一条**兜底价**：0 积分（导出免费，按用户限流，docs/agent/smartcut SCK-3，
// 2026-10-27 已拍板）。原先的 1 积分/秒是 PARAM-07 的设计建议值，已有环境由迁移 00067 改成 0。
//
// ⚠️ 0 积分时渲染不碰账本（StartRender 里只有价格 > 0 才扣），成本靠限流兜（SCT-15）。
// ⚠️ 用**兜底价**（model_id 为空）而不是精确价：渲染不绑模型，精确价那一档在这里没有意义。
// ⚠️ `on conflict do nothing`：运营改过就不覆盖。
func seedRenderPricing(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	var opID int64
	if err := tx.QueryRowContext(ctx,
		`select id from operation where code = 'timeline_render'`).Scan(&opID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		insert into pricing (id, operation_id, model_id, credits, unit, status)
		values ($1, $2, null, 0, 'per_second', 'enabled')
		on conflict (operation_id, model_id) do nothing`, nextID(), opID)
	return err
}

// seedSmartcutPricing 给 AI 剪辑的规划调用配一条 **0 积分兜底价**（docs/agent/smartcut SCK-2「AI 剪辑免费」）。
//
// 同 seedDirectorPricing：免费靠这条配置成立，不靠代码特例 —— 规划调用照常走能力合取式，没有价格就是
// 「缺少可用价格」。成本靠调用记录上的限流兜（SCT-27）。`on conflict do nothing`：运营改过就不覆盖。
func seedSmartcutPricing(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	var opID int64
	if err := tx.QueryRowContext(ctx,
		`select id from operation where code = 'smartcut_plan'`).Scan(&opID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		insert into pricing (id, operation_id, model_id, credits, unit, status)
		values ($1, $2, null, 0, 'per_time', 'enabled')
		on conflict (operation_id, model_id) do nothing`, nextID(), opID)
	return err
}

// seedDirectorPricing 给 3D 导演台的 AI 搭景配一条 **0 积分兜底价**（DRK-4「导演台免费」，DRD-22）。
//
// ⚠️ 免费靠这条配置成立，不靠代码特例：搭景照常走能力合取式（模型、Provider、绑定、有价、能力矩阵、会员等级），
// 没有价格就是「缺少可用价格」，与别的能力一样。运营把它改成非 0 时，搭景**显式拒绝**并说明「按约定免费」
// （catalog/app.SceneBuildChoice）—— 真要收费是改决定与代码，不是改一个数。
// ⚠️ `on conflict do nothing`：运营改过就不覆盖（同渲染兜底价）。
func seedDirectorPricing(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	var opID int64
	if err := tx.QueryRowContext(ctx,
		`select id from operation where code = 'scene_build'`).Scan(&opID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		insert into pricing (id, operation_id, model_id, credits, unit, status)
		values ($1, $2, null, 0, 'per_time', 'enabled')
		on conflict (operation_id, model_id) do nothing`, nextID(), opID)
	return err
}

// seedMemberCatalog 发布一份基础商品目录：3 个等级 × 3 个周期 + 3 个积分包。
//
// ⚠️ 这些价格与赠分是**设计建议值，不是已批准的商业政策**（PARAM-03）。
// 存在的理由只有一个：不发目录的话「会员权益」整条链路
// （下单 → 履约 → 等级解析 → 模型权益）在本地与 CI 里一次都验不到，
// 而那正是 P11-01 要交付的东西。正式定价由运营在管理端改。
//
// ⚠️ 幂等：`code` 与 `(level_id, period)` 是业务键，重复跑只更新不新增。
// **但不覆盖 is_enabled** —— 运营下架了某个套餐，再跑一次 seed
// 就把它又上架了，而没有任何人做过这个决定。
func seedMemberCatalog(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	levels := []struct {
		code, name string
		sort       int
	}{
		{"basic", "基础会员", 10},
		{"pro", "专业会员", 20},
		{"studio", "工作室", 30},
	}
	ids := map[string]int64{}
	for _, l := range levels {
		var id int64
		err := tx.QueryRowContext(ctx, `
			insert into member_level (id, code, name, sort) values ($1,$2,$3,$4)
			on conflict (code) do update set name = excluded.name,
				sort = excluded.sort, updated_at = now()
			returning id`, nextID(), l.code, l.name, l.sort).Scan(&id)
		if err != nil {
			return err
		}
		ids[l.code] = id
	}

	// 月付基准价，季/年按 2.7 / 10 倍折算（少付一点，鼓励长周期）。
	plans := []struct {
		level       string
		monthCents  int64
		monthCredit int
	}{
		{"basic", 2900, 300},
		{"pro", 9900, 1200},
		{"studio", 29900, 4000},
	}
	periods := []struct {
		code   string
		factor float64
		sort   int
	}{
		{"month", 1, 10}, {"quarter", 2.7, 20}, {"year", 10, 30},
	}
	for _, p := range plans {
		for _, pe := range periods {
			price := int64(float64(p.monthCents) * pe.factor)
			// ⚠️ 赠分按**月数**给足（季 3 倍、年 12 倍）而不是按价格折算：
			// 按价格折算的话年付用户少拿两个月的分，而他付的是十个月的钱 ——
			// "买得多反而单价的分更少"没法解释。
			months := 1
			switch pe.code {
			case "quarter":
				months = 3
			case "year":
				months = 12
			}
			_, err := tx.ExecContext(ctx, `
				insert into member_plan (id, level_id, period, price_cents,
					gift_credits, sort)
				values ($1,$2,$3,$4,$5,$6)
				on conflict (level_id, period) do update set
					price_cents = excluded.price_cents,
					gift_credits = excluded.gift_credits,
					sort = excluded.sort, updated_at = now()`,
				nextID(), ids[p.level], pe.code, price,
				p.monthCredit*months, pe.sort)
			if err != nil {
				return err
			}
		}
	}

	packages := []struct {
		code, name string
		credits    int
		cents      int64
		sort       int
	}{
		{"pack_1000", "1000 积分", 1000, 1000, 10},
		{"pack_5000", "5000 积分", 5000, 4500, 20},
		{"pack_20000", "20000 积分", 20000, 16000, 30},
	}
	for _, p := range packages {
		_, err := tx.ExecContext(ctx, `
			insert into credit_package (id, code, name, credits, price_cents, sort)
			values ($1,$2,$3,$4,$5,$6)
			on conflict (code) do update set name = excluded.name,
				credits = excluded.credits, price_cents = excluded.price_cents,
				sort = excluded.sort, updated_at = now()`,
			nextID(), p.code, p.name, p.credits, p.cents, p.sort)
		if err != nil {
			return err
		}
	}
	return nil
}
