package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 管理端技能配置（AGT-60；AG-13，dev/02 §2.3、design/01 §10）。
//
// 运营不发版即可新增 / 调整 / 停用技能。运行时每轮按 code 现读（Runner.loadSkills、
// load_skill / read_skill_ref 都不缓存），所以保存后**下一轮**即用新内容。
//
// ⚠️ 保存时就把正文试渲染一遍（变量用 SkillVars 给全）：渲染失败的技能存进去，
// 运行时 load_skill 会拒绝加载、Runner 会换成一句「渲染失败」—— 而那要等用户发消息才暴露。
// ⚠️ 白名单只能收窄（AllowedTools 取交集）；这里只验「名字是已注册的工具」。
// ⚠️ 不提供删除：历史会话的 skill_codes 要能追溯，停用即可。

// SkillAdminStore 是管理端用到的技能读写（agent/repo 实现）。
type SkillAdminStore interface {
	ListAllSkills(ctx context.Context) ([]repo.AdminSkill, error)
	GetSkillByID(ctx context.Context, id int64) (repo.Skill, error)
	ListSkillReferences(ctx context.Context, skillID int64) ([]repo.SkillReference, error)
	InsertSkill(ctx context.Context, s repo.Skill) (repo.Skill, error)
	UpdateSkill(ctx context.Context, s repo.Skill, expectVersion int) (repo.Skill, error)
	SetSkillEnabled(ctx context.Context, id int64, enabled bool) (repo.Skill, bool, error)
	SaveSkillReference(ctx context.Context, ref repo.SkillReference, isNew bool, expectSkillVersion int) (int, error)
	DeleteSkillReference(ctx context.Context, skillID, refID int64, expectSkillVersion int) (int, error)
	RestoreSkill(ctx context.Context, id int64, c repo.SkillRestore) (repo.Skill, error)
}

// BuiltinSkills 按 code 取随包内置的技能内容（seed 包实现，desktop 装配）。
// ok=false 表示这个 code 不是内置技能 —— 恢复内置版本据此回 404。
type BuiltinSkills interface {
	BuiltinSkill(code string) (repo.SkillRestore, bool)
}

// SkillModelInfo 是绑定模型校验与下拉选项要的模型事实（目录域提供）。
type SkillModelInfo struct {
	Code         string
	Name         string
	Domain       string // text / image / video / audio
	Status       string // enabled / disabled
	AgentCapable bool
}

// SkillModels 列出模型目录（全部，含停用的）。
type SkillModels interface {
	SkillModels(ctx context.Context) ([]SkillModelInfo, error)
}

// Auditor 写业务审计（platform/audit.Logger 实现）。
type Auditor interface {
	Write(ctx context.Context, e audit.Entry)
}

// SkillAdmin 是管理端技能配置服务。
type SkillAdmin struct {
	store  SkillAdminStore
	models SkillModels
	render SkillRenderer
	audit  Auditor
	nextID func() int64
	// 营销域的文案与槽位校验（MKT-T-05，wiring 适配）。copy 为 nil 时不做口播检查；
	// slots 为 nil 时声明了输入槽的技能一律存不进去（缺了等于放行一切槽位声明）。
	copy  CopyChecker
	slots SlotDeclValidator
	// builtins 为 nil 时「恢复内置版本」一律 404。
	builtins BuiltinSkills
}

// NewSkillAdmin 构造。render 必须非 nil：保存前要试渲染（缺了等于放行一切模板）。
func NewSkillAdmin(store SkillAdminStore, models SkillModels, render SkillRenderer,
	nextID func() int64) *SkillAdmin {
	return &SkillAdmin{store: store, models: models, render: render, nextID: nextID}
}

// SetBuiltins 注入内置技能来源（TT-T-19）。
func (a *SkillAdmin) SetBuiltins(b BuiltinSkills) { a.builtins = b }

// SetAudit 注入审计。
func (a *SkillAdmin) SetAudit(au Auditor) { a.audit = au }

// SetMarketingChecks 注入口播文案检查与输入槽声明校验（实现在营销域，经 wiring 适配）。
func (a *SkillAdmin) SetMarketingChecks(copy CopyChecker, slots SlotDeclValidator) {
	a.copy, a.slots = copy, slots
}

// SkillSaved 是新建 / 修改的结果：技能行 + 不拦截的提示（缺口播提示句、合规风险用语）。
type SkillSaved struct {
	repo.Skill
	Warnings []CopyIssue
}

// SkillInput 是新建 / 修改技能的内容。Code 只在新建时生效（创建后不能改：会话的 skill_codes 引用它）。
type SkillInput struct {
	Code         string
	Name         string
	Category     string
	Description  string
	Tags         []string
	CoverMediaID int64
	Body         string
	Steps        []domain.SkillStep
	Tools        []string
	Bindings     domain.SkillBindings
	IsQuickEntry bool
	Sort         int
	// 场景扩展（MKT-T-05）。⚠️ 修改时 nil = **保留原值**：管理端是整份写入，
	// 而 MKT-T-07 之前的表单不认识这三个字段 —— 不保留的话，运营在旧表单上改个错字
	// 就把配好的输入槽清空了，而保存是「成功」的。要清空请显式传 [] / ""。
	Scenes      []string
	InputSlots  json.RawMessage
	PrefillText *string
	// IsBatch 批量标识（MKT-T-32）。nil = 修改时保留原值（与上面三个字段同一条理由）。
	IsBatch *bool
}

// SkillRefInput 是附属文档的内容。
type SkillRefInput struct {
	Name string
	Body string
	Sort int
}

// SkillDetail 是一个技能连同附属文档。
type SkillDetail struct {
	Skill repo.Skill
	Refs  []repo.SkillReference
}

// SkillToolOption 是白名单可选的一个工具（含义说明给管理端看）。
type SkillToolOption struct {
	Name      string
	Paid      bool
	Writes    bool
	Action    string
	Planning  bool // 规划态会下发
	Executing bool // 执行态会下发
}

// SkillOptions 是编辑表单的可选项：工具、渲染变量、模型。
type SkillOptions struct {
	Tools  []SkillToolOption
	Vars   []string
	Models []SkillModelInfo
}

// Options 给出编辑表单的可选项。工具来自规格表（不在前端写死），模型来自目录。
func (a *SkillAdmin) Options(ctx context.Context) (SkillOptions, error) {
	var out SkillOptions
	for _, s := range domain.Tools() {
		out.Tools = append(out.Tools, SkillToolOption{
			Name: string(s.Name), Paid: s.Paid, Writes: s.CanvasWrite, Action: s.Action,
			// 按营销场景判：营销会话的工具集合是通用集合的超集（MKT-T-18），场景专属工具
			// （list_marketing_options）按通用场景判会显示成「两个阶段都不下发」。
			// ⚠️ 按「这个工具在不在」判，不按结果条数：场景专属的只读工具不受白名单收窄（P-51），
			// 白名单只写一个工具时结果里也会带上它们。
			Planning: domain.IsToolAllowedIn(domain.SceneMarketing, domain.StatePlanning,
				[]domain.ToolName{s.Name}, s.Name),
			Executing: domain.IsToolAllowedIn(domain.SceneMarketing, domain.StateExecuting,
				[]domain.ToolName{s.Name}, s.Name),
		})
	}
	out.Vars = skillVarNames()
	if a.models != nil {
		ms, err := a.models.SkillModels(ctx)
		if err != nil {
			return SkillOptions{}, httpx.Wrap(errcode.Internal, err)
		}
		out.Models = ms
	}
	return out, nil
}

// List 列出全部技能（含停用的）。
func (a *SkillAdmin) List(ctx context.Context) ([]repo.AdminSkill, error) {
	rows, err := a.store.ListAllSkills(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	for i := range rows {
		rows[i].InputSlots = a.normalizeSlots(rows[i].InputSlots)
	}
	return rows, nil
}

// normalizeSlots 读路径上把输入槽声明换成新形状（MKD-26）：库里可能还是旧形状（MKT-T-09 录入的技能），
// 管理端编辑器读到旧形状再原样保存的话会一直带着它。校验器同时实现 SlotDeclNormalizer（wiring 里是同一个）。
func (a *SkillAdmin) normalizeSlots(raw json.RawMessage) json.RawMessage {
	if n, ok := a.slots.(SlotDeclNormalizer); ok && hasSlots(raw) {
		return n.NormalizeSlotDecl(raw)
	}
	return raw
}

// Get 取一个技能与它的附属文档。
func (a *SkillAdmin) Get(ctx context.Context, id int64) (SkillDetail, error) {
	sk, err := a.store.GetSkillByID(ctx, id)
	if err != nil {
		return SkillDetail{}, mapSkillErr(err)
	}
	refs, err := a.store.ListSkillReferences(ctx, id)
	if err != nil {
		return SkillDetail{}, httpx.Wrap(errcode.Internal, err)
	}
	sk.InputSlots = a.normalizeSlots(sk.InputSlots)
	return SkillDetail{Skill: sk, Refs: refs}, nil
}

// Create 新建技能。⚠️ 一律落**停用**：新技能确认无误后再单独启用，
// 一次误保存不该等于对全部用户上线一个半成品技能。
func (a *SkillAdmin) Create(ctx context.Context, adminID int64, in SkillInput) (SkillSaved, error) {
	in = normalizeSkillInput(in)
	in, warns, err := a.validate(ctx, in)
	if err != nil {
		return SkillSaved{}, err
	}
	row, err := toSkillRow(in)
	if err != nil {
		return SkillSaved{}, err
	}
	row.ID = a.nextID()
	row.Code = in.Code
	row.IsEnabled = false
	out, err := a.store.InsertSkill(ctx, row)
	if errors.Is(err, repo.ErrDuplicate) {
		return SkillSaved{}, httpx.Errf(errcode.DuplicateName, fmt.Sprintf("技能 code「%s」已存在", in.Code))
	}
	if err != nil {
		return SkillSaved{}, httpx.Wrap(errcode.Internal, err)
	}
	a.write(ctx, adminID, "agent_skill.create", out.ID, nil, skillSnapshot(out))
	return SkillSaved{Skill: out, Warnings: warns}, nil
}

// Update 按 version 乐观锁改技能内容（不改 code、不改启用状态）。
func (a *SkillAdmin) Update(ctx context.Context, adminID, id int64, version int, in SkillInput) (SkillSaved, error) {
	before, err := a.store.GetSkillByID(ctx, id)
	if err != nil {
		return SkillSaved{}, mapSkillErr(err)
	}
	in.Code = before.Code
	in = keepSceneFields(in, before)
	in = normalizeSkillInput(in)
	in, warns, err := a.validate(ctx, in)
	if err != nil {
		return SkillSaved{}, err
	}
	// 已启用的营销技能改完仍要有封面（04 文档 P-54②）：封面闸原先只在「启用」那一步，
	// 启用之后去掉封面、或给已启用的技能加上营销场景都拦不住 —— 营销页的技能弹层上就是一块空白。
	if before.IsEnabled && domain.SkillInScene(in.Scenes, domain.SkillSceneMarketing) && in.CoverMediaID == 0 {
		return SkillSaved{}, problemsErr("技能配置有问题", []string{"营销场景的技能必须配封面（已启用的技能改完也要有）"})
	}
	row, err := toSkillRow(in)
	if err != nil {
		return SkillSaved{}, err
	}
	row.ID = id
	out, err := a.store.UpdateSkill(ctx, row, version)
	if err != nil {
		return SkillSaved{}, a.conflict(ctx, id, err)
	}
	a.write(ctx, adminID, "agent_skill.update", id, skillSnapshot(before), skillSnapshot(out))
	return SkillSaved{Skill: out, Warnings: warns}, nil
}

// keepSceneFields 把请求里没带的场景字段补成库里的原值（见 SkillInput 的说明）。
func keepSceneFields(in SkillInput, before repo.Skill) SkillInput {
	if in.Scenes == nil {
		in.Scenes = before.SceneList()
		if in.Scenes == nil {
			in.Scenes = []string{}
		}
	}
	if in.InputSlots == nil {
		in.InputSlots = before.InputSlots
	}
	if in.PrefillText == nil {
		p := before.PrefillText
		in.PrefillText = &p
	}
	if in.IsBatch == nil {
		b := before.IsBatch
		in.IsBatch = &b
	}
	return in
}

// SetEnabled 启用 / 停用。启用前把主说明与附属文档**重新**试渲染一遍：
// 渲染变量（SkillVars）可能在技能保存之后变过，停用期间存下的内容不保证还能渲染。
// 已是目标状态时不报错（重复点击、两个运营同时点都会发生），不写审计。
func (a *SkillAdmin) SetEnabled(ctx context.Context, adminID, id int64, enabled bool) (repo.Skill, error) {
	before, err := a.store.GetSkillByID(ctx, id)
	if err != nil {
		return repo.Skill{}, mapSkillErr(err)
	}
	if enabled && !before.IsEnabled {
		refs, err := a.store.ListSkillReferences(ctx, id)
		if err != nil {
			return repo.Skill{}, httpx.Wrap(errcode.Internal, err)
		}
		var probs []string
		// MKT-D-14：营销技能必须配封面 —— 营销页的技能弹层与预览卡按封面展示，
		// 没封面的技能上线后是一块空白。草稿可以先存，启用时才强制（与营销选项的封面同一取舍）。
		if domain.SkillInScene(before.SceneList(), domain.SkillSceneMarketing) && before.CoverMediaID == 0 {
			probs = append(probs, "营销场景的技能必须配封面")
		}
		probs = append(probs, a.renderProblems("主说明", before.Body)...)
		for _, f := range refs {
			probs = append(probs, a.renderProblems("附属文档「"+f.Name+"」", f.Body)...)
		}
		if len(probs) > 0 {
			return repo.Skill{}, problemsErr("技能不能启用", probs)
		}
	}
	out, changed, err := a.store.SetSkillEnabled(ctx, id, enabled)
	if err != nil {
		return repo.Skill{}, mapSkillErr(err)
	}
	if changed {
		action := "agent_skill.disable"
		if enabled {
			action = "agent_skill.enable"
		}
		a.write(ctx, adminID, action, id,
			map[string]any{"isEnabled": before.IsEnabled, "version": before.Version},
			map[string]any{"isEnabled": out.IsEnabled, "version": out.Version})
	}
	return out, nil
}

// RestoreBuiltin 用随包内置内容覆盖技能（TT-T-19）：清「已修改」、写入内置哈希、version +1，
// 附属文档同步成内置那一套。不是内置技能（seed 里没有这个 code）返回 404。
// ⚠️ 不做乐观锁：恢复的语义就是「不管现在是什么，回到内置那份」，带 version 只会让它在
// 别人刚保存过时失败，而用户要的结果并不因此改变。启用状态不动。
func (a *SkillAdmin) RestoreBuiltin(ctx context.Context, adminID, id int64) (repo.Skill, error) {
	before, err := a.store.GetSkillByID(ctx, id)
	if err != nil {
		return repo.Skill{}, mapSkillErr(err)
	}
	if a.builtins == nil {
		return repo.Skill{}, httpx.Errf(errcode.NotFound, "这个技能没有内置版本")
	}
	c, ok := a.builtins.BuiltinSkill(before.Code)
	if !ok {
		return repo.Skill{}, httpx.Errf(errcode.NotFound, "这个技能没有内置版本")
	}
	for i := range c.Refs {
		if c.Refs[i].ID == 0 {
			c.Refs[i].ID = a.nextID()
		}
	}
	out, err := a.store.RestoreSkill(ctx, id, c)
	if err != nil {
		return repo.Skill{}, mapSkillErr(err)
	}
	a.write(ctx, adminID, "agent_skill.restore_builtin", id,
		map[string]any{"version": before.Version, "isUserModified": before.IsUserModified},
		map[string]any{"version": out.Version, "isUserModified": out.IsUserModified})
	return out, nil
}

// SaveRef 新建（refID=0）或修改附属文档；skillVersion 是技能的乐观锁版本。返回技能的新版本。
func (a *SkillAdmin) SaveRef(ctx context.Context, adminID, skillID, refID int64, skillVersion int,
	in SkillRefInput) (int, error) {
	in.Name = strings.TrimSpace(in.Name)
	probs := domain.ValidateSkillRef(in.Name, in.Body)
	if in.Sort < 0 || in.Sort > domain.SkillSortMax {
		probs = append(probs, fmt.Sprintf("排序 %d 超出范围 0~%d", in.Sort, domain.SkillSortMax))
	}
	probs = append(probs, a.renderProblems("附属文档", in.Body)...)
	if len(probs) > 0 {
		return 0, problemsErr("附属文档配置有问题", probs)
	}
	var before *repo.SkillReference
	isNew := refID == 0
	if !isNew {
		refs, err := a.store.ListSkillReferences(ctx, skillID)
		if err != nil {
			return 0, httpx.Wrap(errcode.Internal, err)
		}
		for i := range refs {
			if refs[i].ID == refID {
				before = &refs[i]
			}
		}
		if before == nil {
			return 0, httpx.Errf(errcode.NotFound, "附属文档不存在")
		}
	} else {
		refID = a.nextID()
	}
	ref := repo.SkillReference{ID: refID, SkillID: skillID, Name: in.Name, Body: in.Body, Sort: in.Sort}
	v, err := a.store.SaveSkillReference(ctx, ref, isNew, skillVersion)
	if errors.Is(err, repo.ErrDuplicate) {
		return 0, httpx.Errf(errcode.DuplicateName, fmt.Sprintf("这个技能已有名为「%s」的附属文档", in.Name))
	}
	if err != nil {
		return 0, a.conflict(ctx, skillID, err)
	}
	action, prev := "agent_skill_ref.create", any(nil)
	if !isNew {
		action, prev = "agent_skill_ref.update", refSnapshot(*before)
	}
	a.write(ctx, adminID, action, skillID, prev, map[string]any{"ref": refSnapshot(ref), "skillVersion": v})
	return v, nil
}

// DeleteRef 删除附属文档。返回技能的新版本。
func (a *SkillAdmin) DeleteRef(ctx context.Context, adminID, skillID, refID int64, skillVersion int) (int, error) {
	refs, err := a.store.ListSkillReferences(ctx, skillID)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	var before *repo.SkillReference
	for i := range refs {
		if refs[i].ID == refID {
			before = &refs[i]
		}
	}
	if before == nil {
		return 0, httpx.Errf(errcode.NotFound, "附属文档不存在")
	}
	v, err := a.store.DeleteSkillReference(ctx, skillID, refID, skillVersion)
	if err != nil {
		return 0, a.conflict(ctx, skillID, err)
	}
	a.write(ctx, adminID, "agent_skill_ref.delete", skillID, refSnapshot(*before),
		map[string]any{"skillVersion": v})
	return v, nil
}

// ---------------------------------------------------------------- 校验

// validate 一次报完全部问题；通过时返回（输入槽已规范化的）输入与不拦截的提示（warnings）。
func (a *SkillAdmin) validate(ctx context.Context, in SkillInput) (SkillInput, []CopyIssue, error) {
	prefill := ""
	if in.PrefillText != nil {
		prefill = *in.PrefillText
	}
	probs := domain.ValidateSkill(domain.SkillDraft{
		Code: in.Code, Name: in.Name, Category: in.Category, Description: in.Description,
		Tags: in.Tags, Body: in.Body, Steps: in.Steps, Tools: in.Tools, Sort: in.Sort,
		Scenes: in.Scenes, PrefillText: prefill,
	}, domain.ToolNames())
	if strings.TrimSpace(in.Body) != "" {
		probs = append(probs, a.renderProblems("主说明", in.Body)...)
	}
	bp, err := a.bindingProblems(ctx, in.Bindings)
	if err != nil {
		return in, nil, err
	}
	probs = append(probs, bp...)
	var issues, warns []CopyIssue
	if hasSlots(in.InputSlots) {
		if a.slots == nil {
			probs = append(probs, "服务端未接入输入槽校验，无法保存输入槽声明")
		} else {
			norm, is := a.slots.ValidateSlotDecl(in.InputSlots)
			for _, i := range is {
				// 旧形状已换成新形状（MKD-26）：照常保存，提醒运营「已按新形状保存」。
				if i.Code == SlotDeclNormalizedCode {
					warns = append(warns, i)
				} else {
					issues = append(issues, i)
				}
			}
			in.InputSlots = norm
		}
	}
	if a.copy != nil {
		errs, w := a.copy.CheckCopy(skillCopyFields(in, prefill), in.Tags)
		issues = append(issues, errs...)
		warns = append(warns, w...)
	}
	for _, i := range issues {
		probs = append(probs, i.Message)
	}
	if len(probs) > 0 {
		if len(issues) == 0 {
			return in, nil, problemsErr("技能配置有问题", probs)
		}
		// 字段级问题另给结构化的一份：管理端据此把错误挂到对应表单项上（点名字段）。
		return in, nil, httpx.Errf(errcode.InvalidArgument,
			fmt.Sprintf("技能配置有问题（%d 处）：%s", len(probs), strings.Join(probs, "；"))).
			WithDetails(map[string]any{"problems": probs, "issues": issueMaps(issues)})
	}
	return in, warns, nil
}

// skillCopyFields 是技能**用户看得见**的文案（MKT-F-76）：名称、说明、预填描述、步骤名。
// 主说明与附属文档是给模型的，不算（它们本来就可能要写「不得出现数字人」这类约束）。
func skillCopyFields(in SkillInput, prefill string) map[string]string {
	f := map[string]string{"name": in.Name, "description": in.Description, "prefillText": prefill}
	for i, st := range in.Steps {
		f[fmt.Sprintf("steps[%d].name", i)] = st.Name
	}
	return f
}

// hasSlots 判断输入槽声明是否非空（null / [] / 空白都算没有）。
func hasSlots(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t != "" && t != "null" && t != "[]"
}

func issueMaps(is []CopyIssue) []map[string]any {
	out := make([]map[string]any, 0, len(is))
	for _, i := range is {
		out = append(out, map[string]any{"field": i.Field, "code": i.Code, "message": i.Message})
	}
	return out
}

// renderProblems 用运行时同一份变量（SkillVars，全部给齐）试渲染。
// 变量取值用样例：这里只判「能不能渲染」，不关心渲染出什么。
func (a *SkillAdmin) renderProblems(label, body string) []string {
	if a.render == nil {
		return []string{"服务端未配置技能渲染器，无法校验正文"}
	}
	if _, err := a.render.RenderSkill(body, SkillVars(domain.StatePlanning, "sample_model")); err != nil {
		return []string{fmt.Sprintf("%s渲染失败：%v（可用变量：%s）", label, err, strings.Join(skillVarNames(), "、"))}
	}
	return nil
}

// bindingProblems 绑定模型：推理模型必须是支持 Agent 的文本模型；视频 / 图片模型存在且 domain 对得上。
// ⚠️ 不要求「已启用」：停用的模型照样能绑（运营可以先配好再上线模型），
// 运行时的可用性仍按能力合取式判，不会因为绑了就绕过去。
func (a *SkillAdmin) bindingProblems(ctx context.Context, b domain.SkillBindings) ([]string, error) {
	if b.Video == "" && b.Image == "" && b.Reasoning == "" {
		return nil, nil
	}
	if a.models == nil {
		return []string{"服务端未接入模型目录，无法校验绑定模型"}, nil
	}
	list, err := a.models.SkillModels(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	byCode := make(map[string]SkillModelInfo, len(list))
	for _, m := range list {
		byCode[m.Code] = m
	}
	var out []string
	check := func(label, code, wantDomain string) (SkillModelInfo, bool) {
		if code == "" {
			return SkillModelInfo{}, false
		}
		m, ok := byCode[code]
		if !ok {
			out = append(out, fmt.Sprintf("%s「%s」在模型目录里不存在", label, code))
			return m, false
		}
		if m.Domain != wantDomain {
			out = append(out, fmt.Sprintf("%s「%s」是%s模型，不是%s模型", label, code,
				domainLabel(m.Domain), domainLabel(wantDomain)))
			return m, false
		}
		return m, true
	}
	check("视频模型", b.Video, "video")
	check("图片模型", b.Image, "image")
	if m, ok := check("推理模型", b.Reasoning, "text"); ok && !m.AgentCapable {
		out = append(out, fmt.Sprintf("推理模型「%s」不支持 Agent（在「模型与供应商」里开启「可作推理模型」后才能绑定）", b.Reasoning))
	}
	return out, nil
}

func domainLabel(d string) string {
	switch d {
	case "text":
		return "文本"
	case "image":
		return "图片"
	case "video":
		return "视频"
	case "audio":
		return "音频"
	}
	return d
}

func problemsErr(head string, probs []string) error {
	return httpx.Errf(errcode.InvalidArgument,
		fmt.Sprintf("%s（%d 处）：%s", head, len(probs), strings.Join(probs, "；"))).
		WithDetails(map[string]any{"problems": probs})
}

// ---------------------------------------------------------------- 转换

func normalizeSkillInput(in SkillInput) SkillInput {
	in.Code = strings.TrimSpace(in.Code)
	in.Name = strings.TrimSpace(in.Name)
	in.Category = strings.TrimSpace(in.Category)
	in.Description = strings.TrimSpace(in.Description)
	for i := range in.Tags {
		in.Tags[i] = strings.TrimSpace(in.Tags[i])
	}
	for i := range in.Steps {
		in.Steps[i].Name = strings.TrimSpace(in.Steps[i].Name)
	}
	for i := range in.Tools {
		in.Tools[i] = strings.TrimSpace(in.Tools[i])
	}
	in.Bindings.Video = strings.TrimSpace(in.Bindings.Video)
	in.Bindings.Image = strings.TrimSpace(in.Bindings.Image)
	in.Bindings.Reasoning = strings.TrimSpace(in.Bindings.Reasoning)
	for i := range in.Scenes {
		in.Scenes[i] = strings.TrimSpace(in.Scenes[i])
	}
	if in.PrefillText != nil {
		p := strings.TrimSpace(*in.PrefillText)
		in.PrefillText = &p
	}
	return in
}

func toSkillRow(in SkillInput) (repo.Skill, error) {
	enc := func(v any, def string) (json.RawMessage, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		if string(b) == "null" {
			return json.RawMessage(def), nil
		}
		return b, nil
	}
	tags, err := enc(in.Tags, "[]")
	if err != nil {
		return repo.Skill{}, err
	}
	steps, err := enc(in.Steps, "[]")
	if err != nil {
		return repo.Skill{}, err
	}
	tools, err := enc(in.Tools, "[]")
	if err != nil {
		return repo.Skill{}, err
	}
	bindings, err := enc(in.Bindings, "{}")
	if err != nil {
		return repo.Skill{}, err
	}
	scenes, err := enc(in.Scenes, "[]")
	if err != nil {
		return repo.Skill{}, err
	}
	slots := json.RawMessage("[]")
	if hasSlots(in.InputSlots) {
		slots = in.InputSlots // 已由 SlotDeclValidator 校验并规范化（键名与契约一致）
	}
	prefill := ""
	if in.PrefillText != nil {
		prefill = *in.PrefillText
	}
	return repo.Skill{
		Name: in.Name, Category: in.Category, Description: in.Description, Tags: tags,
		CoverMediaID: in.CoverMediaID, Body: in.Body, Steps: steps, Tools: tools,
		ModelBindings: bindings, IsQuickEntry: in.IsQuickEntry, Sort: in.Sort,
		Scenes: scenes, InputSlots: slots, PrefillText: prefill, IsBatch: in.IsBatch != nil && *in.IsBatch,
	}, nil
}

// conflict 把仓储错误翻成业务错误；版本冲突时带上当前版本，前端据此提示「重新加载」。
func (a *SkillAdmin) conflict(ctx context.Context, id int64, err error) error {
	if errors.Is(err, repo.ErrStale) {
		be := httpx.Errf(errcode.VersionConflict, "技能已被其他人修改，请重新加载后再保存（你的修改不会被静默覆盖）")
		if cur, gerr := a.store.GetSkillByID(ctx, id); gerr == nil {
			be = be.WithDetails(map[string]any{"currentVersion": cur.Version})
		}
		return be
	}
	return mapSkillErr(err)
}

func mapSkillErr(err error) error {
	if errors.Is(err, repo.ErrNotFound) {
		return httpx.Errf(errcode.NotFound, "技能或附属文档不存在")
	}
	return httpx.Wrap(errcode.Internal, err)
}

func (a *SkillAdmin) write(ctx context.Context, adminID int64, action string, id int64, before, after any) {
	if a.audit == nil {
		return
	}
	a.audit.Write(ctx, audit.Entry{ActorType: audit.ActorAdmin, ActorID: adminID,
		Action: action, ObjectType: "agent_skill", ObjectID: id, Before: before, After: after})
}

func skillSnapshot(s repo.Skill) map[string]any {
	return map[string]any{
		"code": s.Code, "name": s.Name, "category": s.Category, "description": s.Description,
		"tags": s.Tags, "coverMediaId": s.CoverMediaID, "body": s.Body, "steps": s.Steps,
		"tools": s.Tools, "modelBindings": s.ModelBindings, "isQuickEntry": s.IsQuickEntry,
		"sort": s.Sort, "isEnabled": s.IsEnabled, "version": s.Version,
		"scenes": s.Scenes, "inputSlots": s.InputSlots, "prefillText": s.PrefillText,
		"isBatch": s.IsBatch,
	}
}

func refSnapshot(f repo.SkillReference) map[string]any {
	return map[string]any{"id": f.ID, "name": f.Name, "body": f.Body, "sort": f.Sort}
}

func skillVarNames() []string {
	var out []string
	for k := range SkillVars(domain.StatePlanning, "") {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
