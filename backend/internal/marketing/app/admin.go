package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 管理端营销选项与分类（MKT-F-70~77；dev/01 §2.1、dev/02 §4.2）。
//
// ⚠️ 新建一律落**停用**：上线是一次显式操作（同公告「新建落草稿」），一次误保存不该等于全站上线。
// ⚠️ 管理端列表**不按时段过滤**（看「配了什么」），每行带服务端算好的 lifecycle 与告警；
//    用户端「只返回在投」是另一套查询（MKT-T-04），别共用一个查询加布尔参数。
// ⚠️ 不提供删除：历史快照按 code 引用，只停用。
// ⚠️ 审计 diff **不含片段全文**，只记长度与哈希：片段可能很长，审计表不是存档 ——
//    原文在只增的版本历史表里，按 (code, version) 永远取得回来。

// 校验问题的补充分类（domain 之外、要查库才知道的）。
const (
	IssueNotFound    domain.IssueCode = "not_found"
	IssueUnavailable domain.IssueCode = "unavailable"
	IssueBadSource   domain.IssueCode = "bad_source"
	IssueImmutable   domain.IssueCode = "immutable"
)

// 管理端上传的封面用途（media/transport 的 adminPurposes）。
const purposeMaterialCover = "material_cover"

// MaxCategoryName 是分类名上限（marketing_category.name varchar(24)）。
const MaxCategoryName = 24

// maxSortIDs 是一次排序最多带多少个 ID（防误传整表）。
const maxSortIDs = 500

var categoryCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,47}$`)

// AdminStore 是管理端用到的读写（marketing/repo 实现）。
type AdminStore interface {
	InsertOption(ctx context.Context, id, versionID, adminID int64, o domain.Option, snap json.RawMessage) (repo.Option, error)
	UpdateOption(ctx context.Context, id int64, expect int, versionID, adminID int64, o domain.Option,
		snap json.RawMessage) (repo.Option, error)
	GetOption(ctx context.Context, id int64) (repo.Option, error)
	SetOptionEnabled(ctx context.Context, id int64, enabled bool, adminID int64) (repo.Option, bool, error)
	ListOptions(ctx context.Context, f repo.OptionFilter) ([]repo.Option, bool, error)
	SortOptions(ctx context.Context, kind domain.Kind, category string, ids []int64) error
	ListVersions(ctx context.Context, optionID int64) ([]repo.OptionVersion, error)
	ListCategories(ctx context.Context, kind domain.Kind) ([]repo.Category, error)
	GetCategory(ctx context.Context, id int64) (repo.Category, error)
	InsertCategory(ctx context.Context, c repo.Category) (repo.Category, error)
	UpdateCategory(ctx context.Context, id int64, name string, enabled bool) (repo.Category, error)
	SortCategories(ctx context.Context, kind domain.Kind, ids []int64) error
}

// Admin 是管理端服务。
type Admin struct {
	store  AdminStore
	models ModelLookup
	skills SkillLookup
	media  MediaLookup
	audit  Auditor
	nextID func() int64
	now    Clock
}

// NewAdmin 构造。三个查询端口都必须给：缺了等于放行一切引用（存进去一个不存在的模型，
// 前台筛选里就多出一个点进去为空的项）。
func NewAdmin(store AdminStore, models ModelLookup, skills SkillLookup, media MediaLookup,
	nextID func() int64) *Admin {
	return &Admin{store: store, models: models, skills: skills, media: media, nextID: nextID,
		now: func() time.Time { return time.Now().UTC() }}
}

// SetAudit 注入审计。
func (a *Admin) SetAudit(au Auditor) { a.audit = au }

// SetClock 注入时钟（测试用）。
func (a *Admin) SetClock(c Clock) { a.now = c }

// OptionInput 是新建 / 整份更新的内容。Kind 与 Code 只在新建时生效；更新时若带了且与原值不同即拒。
type OptionInput struct {
	Kind              domain.Kind
	Code              string
	Name              string
	CategoryCode      string
	Description       string
	CoverMediaIDs     []int64
	PreviewMediaID    *int64
	Prompt            string
	OutputTypes       []string
	InputRequirements map[string]string
	IsAudioRequired   bool
	RecommendedModels []string
	RecommendedParams json.RawMessage
	DefaultSkillCode  string
	Tags              []string
	StartAt           *time.Time
	EndAt             *time.Time
	Sort              int
}

// OptionView 是管理端看到的一条选项：当前行 + 服务端算好的投放状态与告警。
type OptionView struct {
	repo.Option
	Lifecycle domain.LifecycleState
	Warnings  []domain.Issue
}

// OptionPage 是列表的一页。
type OptionPage struct {
	Items      []OptionView
	NextCursor string
}

// ListFilter 是列表筛选（transport 解析好）。
type ListFilter struct {
	Kind      domain.Kind
	Category  string
	Enabled   *bool
	Lifecycle domain.LifecycleState
	Q         string
	Cursor    string
	Limit     int
}

// ---------------------------------------------------------------- 选项

// List 管理端列表：不按时段过滤，每行带 lifecycle 与告警。
func (a *Admin) List(ctx context.Context, f ListFilter) (OptionPage, error) {
	rf := repo.OptionFilter{Kind: f.Kind, Category: strings.TrimSpace(f.Category), Enabled: f.Enabled,
		Lifecycle: f.Lifecycle, Q: f.Q, Now: a.now(), Limit: f.Limit}
	if f.Kind != "" && !f.Kind.Valid() {
		return OptionPage{}, httpx.Errf(errcode.InvalidArgument, "kind 只能是 creative / hook / style")
	}
	switch f.Lifecycle {
	case "", domain.LifecycleNotStarted, domain.LifecycleLive, domain.LifecycleEnded, domain.LifecycleDisabled:
	default:
		return OptionPage{}, httpx.Errf(errcode.InvalidArgument, "lifecycle 只能是 not_started / live / ended / disabled")
	}
	if rf.Limit <= 0 || rf.Limit > 200 {
		rf.Limit = 50
	}
	if f.Cursor != "" {
		s, id, ok := parseCursor(f.Cursor)
		if !ok {
			return OptionPage{}, httpx.Errf(errcode.InvalidArgument, "cursor 非法")
		}
		rf.CursorSort, rf.CursorID = s, id
	}
	rows, more, err := a.store.ListOptions(ctx, rf)
	if err != nil {
		return OptionPage{}, httpx.Wrap(errcode.Internal, err)
	}
	lk, err := a.preload(ctx, rows)
	if err != nil {
		return OptionPage{}, err
	}
	now := a.now()
	page := OptionPage{Items: make([]OptionView, 0, len(rows))}
	for _, o := range rows {
		v, err := a.evaluate(ctx, o.Option, lk)
		if err != nil {
			return OptionPage{}, err
		}
		page.Items = append(page.Items, OptionView{Option: o, Lifecycle: domain.Lifecycle(o.Option, now),
			Warnings: v.listWarnings()})
	}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor = fmt.Sprintf("%d_%d", last.Sort, last.ID)
	}
	return page, nil
}

// Get 取一条选项（含 prompt）。
func (a *Admin) Get(ctx context.Context, id int64) (OptionView, error) {
	o, err := a.store.GetOption(ctx, id)
	if err != nil {
		return OptionView{}, mapErr(err, "选项不存在")
	}
	return a.view(ctx, o)
}

func (a *Admin) view(ctx context.Context, o repo.Option) (OptionView, error) {
	v, err := a.evaluate(ctx, o.Option, newLookups())
	if err != nil {
		return OptionView{}, err
	}
	return OptionView{Option: o, Lifecycle: domain.Lifecycle(o.Option, a.now()), Warnings: v.listWarnings()}, nil
}

// Create 新建选项：默认停用，写第 1 版历史，返回告警（无封面、口播缺提示句等）。
func (a *Admin) Create(ctx context.Context, adminID int64, in OptionInput) (OptionView, error) {
	o := toOption(normalize(in))
	o.IsEnabled = false
	o.Version = 1
	v, err := a.evaluate(ctx, o, newLookups())
	if err != nil {
		return OptionView{}, err
	}
	if err := v.saveError(false); err != nil {
		return OptionView{}, err
	}
	snap, err := snapshotJSON(o)
	if err != nil {
		return OptionView{}, httpx.Wrap(errcode.Internal, err)
	}
	row, err := a.store.InsertOption(ctx, a.nextID(), a.nextID(), adminID, o, snap)
	if errors.Is(err, repo.ErrDuplicate) {
		return OptionView{}, httpx.Errf(errcode.DuplicateName, fmt.Sprintf("选项 code「%s」已存在（code 跨类型全局唯一）", o.Code)).
			WithDetails(map[string]any{"issues": []domain.Issue{{Field: "code", Code: domain.IssueDuplicate,
				Message: "code 已存在", Level: domain.LevelError}}})
	}
	if err != nil {
		return OptionView{}, httpx.Wrap(errcode.Internal, err)
	}
	a.write(ctx, adminID, "marketing_option.create", row.ID, nil, auditSnapshot(row.Option))
	return OptionView{Option: row, Lifecycle: domain.Lifecycle(row.Option, a.now()), Warnings: v.saveWarnings(false)}, nil
}

// Update 按 version 乐观锁整份更新：code / kind 不可改，version+1 并写一行版本历史（同一事务）。
//
// ⚠️ 已启用的选项按**启用标准**校验：在投的选项被改成没有封面、或口播缺提示句，等于绕过了启用闸。
func (a *Admin) Update(ctx context.Context, adminID, id int64, version int, in OptionInput) (OptionView, error) {
	before, err := a.store.GetOption(ctx, id)
	if err != nil {
		return OptionView{}, mapErr(err, "选项不存在")
	}
	in = normalize(in)
	var imm []domain.Issue
	if in.Code != "" && in.Code != before.Code {
		imm = append(imm, domain.Issue{Field: "code", Code: IssueImmutable, Level: domain.LevelError,
			Message: "code 建后不可改（历史会话与生成快照靠它追溯）"})
	}
	if in.Kind != "" && in.Kind != before.Kind {
		imm = append(imm, domain.Issue{Field: "kind", Code: IssueImmutable, Level: domain.LevelError,
			Message: "类型建后不可改"})
	}
	in.Code, in.Kind = before.Code, before.Kind
	o := toOption(in)
	o.IsEnabled = before.IsEnabled
	o.Version = version + 1
	v, err := a.evaluate(ctx, o, newLookups())
	if err != nil {
		return OptionView{}, err
	}
	v.hard = append(imm, v.hard...)
	if err := v.saveError(o.IsEnabled); err != nil {
		return OptionView{}, err
	}
	snap, err := snapshotJSON(o)
	if err != nil {
		return OptionView{}, httpx.Wrap(errcode.Internal, err)
	}
	row, err := a.store.UpdateOption(ctx, id, version, a.nextID(), adminID, o, snap)
	if err != nil {
		return OptionView{}, a.conflict(ctx, id, err)
	}
	a.write(ctx, adminID, "marketing_option.update", id, auditSnapshot(before.Option), auditSnapshot(row.Option))
	return OptionView{Option: row, Lifecycle: domain.Lifecycle(row.Option, a.now()), Warnings: v.saveWarnings(o.IsEnabled)}, nil
}

// Enable 启用：完整校验（含封面、口播提示句），不过回 40905 + details.issues[]。
// 已启用时幂等返回（重复点击、两个运营同时点都会发生），不写审计。
func (a *Admin) Enable(ctx context.Context, adminID, id int64) (OptionView, error) {
	before, err := a.store.GetOption(ctx, id)
	if err != nil {
		return OptionView{}, mapErr(err, "选项不存在")
	}
	if before.IsEnabled {
		return a.view(ctx, before)
	}
	probe := before.Option
	probe.IsEnabled = true
	v, err := a.evaluate(ctx, probe, newLookups())
	if err != nil {
		return OptionView{}, err
	}
	if block := append(append([]domain.Issue{}, v.hard...), v.escalated()...); len(block) > 0 {
		return OptionView{}, issuesErr(errcode.InvalidStateTransition, "选项不能启用", block, v.warn)
	}
	return a.setEnabled(ctx, adminID, before, true)
}

// Disable 停用（不删行，MKT-F-75）。已停用时幂等。
func (a *Admin) Disable(ctx context.Context, adminID, id int64) (OptionView, error) {
	before, err := a.store.GetOption(ctx, id)
	if err != nil {
		return OptionView{}, mapErr(err, "选项不存在")
	}
	return a.setEnabled(ctx, adminID, before, false)
}

func (a *Admin) setEnabled(ctx context.Context, adminID int64, before repo.Option, enabled bool) (OptionView, error) {
	row, changed, err := a.store.SetOptionEnabled(ctx, before.ID, enabled, adminID)
	if err != nil {
		return OptionView{}, mapErr(err, "选项不存在")
	}
	if changed {
		action := "marketing_option.disable"
		if enabled {
			action = "marketing_option.enable"
		}
		a.write(ctx, adminID, action, row.ID,
			map[string]any{"isEnabled": before.IsEnabled, "version": before.Version},
			map[string]any{"isEnabled": row.IsEnabled, "version": row.Version})
	}
	return a.view(ctx, row)
}

// Sort 把同一类型 + 分类下的选项按 ids 顺序排成 1..N。
func (a *Admin) Sort(ctx context.Context, adminID int64, kind domain.Kind, category string, ids []int64) error {
	category = strings.TrimSpace(category)
	if !kind.Valid() {
		return httpx.Errf(errcode.InvalidArgument, "kind 只能是 creative / hook / style")
	}
	if category == "" {
		return httpx.Errf(errcode.InvalidArgument, "缺少 category")
	}
	if err := checkSortIDs(ids); err != nil {
		return err
	}
	if err := a.store.SortOptions(ctx, kind, category, ids); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Errf(errcode.InvalidArgument, "ids 里有不存在、或不属于该类型与分类的选项")
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	a.write(ctx, adminID, "marketing_option.sort", 0, nil,
		map[string]any{"kind": kind, "category": category, "ids": idStrings(ids)})
	return nil
}

// Versions 列出版本历史（新版本在前）。
func (a *Admin) Versions(ctx context.Context, id int64) ([]repo.OptionVersion, error) {
	if _, err := a.store.GetOption(ctx, id); err != nil {
		return nil, mapErr(err, "选项不存在")
	}
	vs, err := a.store.ListVersions(ctx, id)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return vs, nil
}

// ---------------------------------------------------------------- 分类

// CategoryInput 是分类的内容。Kind 与 Code 只在新建时生效。
type CategoryInput struct {
	Kind      domain.Kind
	Code      string
	Name      string
	Sort      int
	IsEnabled *bool
}

// Categories 列出分类（kind 为空 = 全部，含停用的）。
func (a *Admin) Categories(ctx context.Context, kind domain.Kind) ([]repo.Category, error) {
	if kind != "" && !kind.Valid() {
		return nil, httpx.Errf(errcode.InvalidArgument, "kind 只能是 creative / hook / style")
	}
	cs, err := a.store.ListCategories(ctx, kind)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return cs, nil
}

// CreateCategory 新建分类（默认启用：分类本身不对外投放，只有其下启用的选项才可见）。
func (a *Admin) CreateCategory(ctx context.Context, adminID int64, in CategoryInput) (repo.Category, error) {
	in.Code = strings.TrimSpace(in.Code)
	in.Name = strings.TrimSpace(in.Name)
	var issues []domain.Issue
	if !in.Kind.Valid() {
		issues = append(issues, errIssue("kind", domain.IssueBadValue, "类型只能是 creative / hook / style"))
	}
	if !categoryCodePattern.MatchString(in.Code) {
		issues = append(issues, errIssue("code", domain.IssueBadFormat, "code 须为小写字母开头的小写字母、数字、下划线，共 2~48 位"))
	}
	issues = append(issues, categoryNameIssues(in.Name)...)
	if len(issues) > 0 {
		return repo.Category{}, issuesErr(errcode.InvalidArgument, "分类配置有问题", issues, nil)
	}
	enabled := true
	if in.IsEnabled != nil {
		enabled = *in.IsEnabled
	}
	c, err := a.store.InsertCategory(ctx, repo.Category{ID: a.nextID(), Category: domain.Category{
		Kind: in.Kind, Code: in.Code, Name: in.Name, Sort: in.Sort, IsEnabled: enabled}})
	if errors.Is(err, repo.ErrDuplicate) {
		return repo.Category{}, httpx.Errf(errcode.DuplicateName, fmt.Sprintf("分类 code「%s」在该类型下已存在", in.Code))
	}
	if err != nil {
		return repo.Category{}, httpx.Wrap(errcode.Internal, err)
	}
	a.writeCategory(ctx, adminID, "marketing_category.create", c.ID, nil, categorySnapshot(c))
	return c, nil
}

// UpdateCategory 改名 / 启停。code 与 kind 不可改（选项按 (kind, code) 引用分类）。
func (a *Admin) UpdateCategory(ctx context.Context, adminID, id int64, in CategoryInput) (repo.Category, error) {
	before, err := a.store.GetCategory(ctx, id)
	if err != nil {
		return repo.Category{}, mapErr(err, "分类不存在")
	}
	in.Code = strings.TrimSpace(in.Code)
	in.Name = strings.TrimSpace(in.Name)
	var issues []domain.Issue
	if in.Code != "" && in.Code != before.Code {
		issues = append(issues, errIssue("code", IssueImmutable, "分类 code 建后不可改（选项按它引用分类）"))
	}
	if in.Kind != "" && in.Kind != before.Kind {
		issues = append(issues, errIssue("kind", IssueImmutable, "分类类型建后不可改"))
	}
	issues = append(issues, categoryNameIssues(in.Name)...)
	if len(issues) > 0 {
		return repo.Category{}, issuesErr(errcode.InvalidArgument, "分类配置有问题", issues, nil)
	}
	enabled := before.IsEnabled
	if in.IsEnabled != nil {
		enabled = *in.IsEnabled
	}
	c, err := a.store.UpdateCategory(ctx, id, in.Name, enabled)
	if err != nil {
		return repo.Category{}, mapErr(err, "分类不存在")
	}
	a.writeCategory(ctx, adminID, "marketing_category.update", id, categorySnapshot(before), categorySnapshot(c))
	return c, nil
}

// SortCategories 把同一类型下的分类按 ids 顺序排成 1..N。
func (a *Admin) SortCategories(ctx context.Context, adminID int64, kind domain.Kind, ids []int64) error {
	if !kind.Valid() {
		return httpx.Errf(errcode.InvalidArgument, "kind 只能是 creative / hook / style")
	}
	if err := checkSortIDs(ids); err != nil {
		return err
	}
	if err := a.store.SortCategories(ctx, kind, ids); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Errf(errcode.InvalidArgument, "ids 里有不存在、或不属于该类型的分类")
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	a.writeCategory(ctx, adminID, "marketing_category.sort", 0, nil,
		map[string]any{"kind": kind, "ids": idStrings(ids)})
	return nil
}

func categoryNameIssues(name string) []domain.Issue {
	switch n := domain.RuneLen(name); {
	case n == 0:
		return []domain.Issue{errIssue("name", domain.IssueRequired, "分类名不能为空")}
	case n > MaxCategoryName:
		return []domain.Issue{errIssue("name", domain.IssueTooLong,
			fmt.Sprintf("分类名最多 %d 字，当前 %d 字", MaxCategoryName, n))}
	}
	return nil
}

func checkSortIDs(ids []int64) error {
	if len(ids) == 0 {
		return httpx.Errf(errcode.InvalidArgument, "ids 不能为空")
	}
	if len(ids) > maxSortIDs {
		return httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("一次最多排序 %d 项", maxSortIDs))
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return httpx.Errf(errcode.InvalidArgument, "ids 里有非法或重复的 ID")
		}
		seen[id] = true
	}
	return nil
}

// ---------------------------------------------------------------- 校验

// verdict 是一条选项的全部问题，按「什么时候拦」分三组：
//   - hard：任何时候都拦（格式、长度、禁用词、引用不存在、封面来源不对）；
//   - enableOnly：草稿可存、启用时才拦（无封面、空片段、口播缺提示句）—— 草稿保存时降为告警；
//   - warn：只告警（推荐模型停用 / 不支持、技能停用、合规风险用语、分类停用）。
type verdict struct {
	hard, enableOnly, warn []domain.Issue
}

// escalated 是 enableOnly 按拦截级给出的副本。
func (v verdict) escalated() []domain.Issue {
	out := make([]domain.Issue, 0, len(v.enableOnly))
	for _, i := range v.enableOnly {
		i.Level = domain.LevelError
		out = append(out, i)
	}
	return out
}

func (v verdict) downgraded() []domain.Issue {
	out := make([]domain.Issue, 0, len(v.enableOnly))
	for _, i := range v.enableOnly {
		i.Level = domain.LevelWarn
		out = append(out, i)
	}
	return out
}

// saveError：草稿只拦 hard；在投的选项连 enableOnly 一起拦。一次报完。
func (v verdict) saveError(enabled bool) error {
	block := append([]domain.Issue{}, v.hard...)
	if enabled {
		block = append(block, v.escalated()...)
	}
	if len(block) == 0 {
		return nil
	}
	return issuesErr(errcode.InvalidArgument, "选项配置有问题", block, v.saveWarnings(enabled))
}

func (v verdict) saveWarnings(enabled bool) []domain.Issue {
	out := append([]domain.Issue{}, v.warn...)
	if !enabled {
		out = append(out, v.downgraded()...)
	}
	if out == nil {
		out = []domain.Issue{}
	}
	return out
}

// listWarnings 是列表 / 详情上展示的全部问题。hard 也列出来（可能是保存之后外部变了：
// 模型被删、封面媒体被清），保留各自的级别，前端据此区分「能不能启用」。
func (v verdict) listWarnings() []domain.Issue {
	out := append([]domain.Issue{}, v.hard...)
	out = append(out, v.escalated()...)
	out = append(out, v.warn...)
	return out
}

// lookups 缓存一次请求里的查询结果（列表一页可能几十行，同一个模型 / 分类只问一次）。
type lookups struct {
	categories map[string]*repo.Category // kind:code → 分类；nil 值 = 查过不存在
	catLoaded  bool
	models     map[string]ModelFacts
	modelAsked map[string]bool
	skills     map[string]SkillFacts
	skillAsked map[string]bool
	media      map[int64]*MediaFacts
}

func newLookups() *lookups {
	return &lookups{categories: map[string]*repo.Category{}, models: map[string]ModelFacts{},
		modelAsked: map[string]bool{}, skills: map[string]SkillFacts{}, skillAsked: map[string]bool{},
		media: map[int64]*MediaFacts{}}
}

// preload 为一页列表批量取分类、模型、技能。
func (a *Admin) preload(ctx context.Context, rows []repo.Option) (*lookups, error) {
	lk := newLookups()
	var models, skills []string
	for _, o := range rows {
		models = append(models, o.RecommendedModels...)
		if o.DefaultSkillCode != "" {
			skills = append(skills, o.DefaultSkillCode)
		}
	}
	if err := a.askModels(ctx, lk, models); err != nil {
		return nil, err
	}
	if err := a.askSkills(ctx, lk, skills); err != nil {
		return nil, err
	}
	return lk, nil
}

func (a *Admin) askModels(ctx context.Context, lk *lookups, codes []string) error {
	var need []string
	for _, c := range codes {
		if c != "" && !lk.modelAsked[c] {
			lk.modelAsked[c] = true
			need = append(need, c)
		}
	}
	if len(need) == 0 {
		return nil
	}
	if a.models == nil {
		return httpx.Errf(errcode.Internal, "服务端未接入模型目录，无法校验推荐模型")
	}
	m, err := a.models.LookupModels(ctx, need)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	for k, v := range m {
		lk.models[k] = v
	}
	return nil
}

func (a *Admin) askSkills(ctx context.Context, lk *lookups, codes []string) error {
	var need []string
	for _, c := range codes {
		if c != "" && !lk.skillAsked[c] {
			lk.skillAsked[c] = true
			need = append(need, c)
		}
	}
	if len(need) == 0 {
		return nil
	}
	if a.skills == nil {
		return httpx.Errf(errcode.Internal, "服务端未接入技能目录，无法校验默认技能")
	}
	m, err := a.skills.LookupSkills(ctx, need)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	for k, v := range m {
		lk.skills[k] = v
	}
	return nil
}

func (a *Admin) category(ctx context.Context, lk *lookups, kind domain.Kind, code string) (*repo.Category, error) {
	if !lk.catLoaded {
		cs, err := a.store.ListCategories(ctx, "")
		if err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		for i := range cs {
			c := cs[i]
			lk.categories[string(c.Kind)+":"+c.Code] = &c
		}
		lk.catLoaded = true
	}
	return lk.categories[string(kind)+":"+code], nil
}

func (a *Admin) mediaFacts(ctx context.Context, lk *lookups, id int64) (*MediaFacts, error) {
	if m, ok := lk.media[id]; ok {
		return m, nil
	}
	if a.media == nil {
		return nil, httpx.Errf(errcode.Internal, "服务端未接入媒体域，无法校验封面")
	}
	f, found, err := a.media.LookupMedia(ctx, id)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	var out *MediaFacts
	if found {
		out = &f
	}
	lk.media[id] = out
	return out, nil
}

// evaluate 一次报完一条选项的全部问题（domain 规则 + 要查库的引用）。
func (a *Admin) evaluate(ctx context.Context, o domain.Option, lk *lookups) (verdict, error) {
	var v verdict
	draft := o
	draft.IsEnabled = false
	hardKeys := map[string]bool{}
	for _, i := range domain.ValidateOption(draft) {
		if i.Level == domain.LevelError {
			v.hard = append(v.hard, i)
			hardKeys[i.Field+"|"+string(i.Code)] = true
		} else {
			v.warn = append(v.warn, i)
		}
	}
	live := o
	live.IsEnabled = true
	for _, i := range domain.ValidateOption(live) {
		if i.Level == domain.LevelError && !hardKeys[i.Field+"|"+string(i.Code)] {
			v.enableOnly = append(v.enableOnly, i)
		}
	}

	// 口播（MKD-12：与技能保存共用一个实现）。禁用词任何时候都拦；缺提示句草稿告警、启用时拦。
	errs, warns := domain.CheckTalkCopy(domain.UserVisibleFields(o), o.Tags)
	v.hard = append(v.hard, errs...)
	for _, w := range warns {
		if w.Code == domain.IssueMissingNote {
			v.enableOnly = append(v.enableOnly, w)
		} else {
			v.warn = append(v.warn, w)
		}
	}

	// 分类：不存在拦（选项会挂空、前台成列时找不到归属），停用告警。
	if o.Kind.Valid() && strings.TrimSpace(o.CategoryCode) != "" {
		c, err := a.category(ctx, lk, o.Kind, o.CategoryCode)
		if err != nil {
			return verdict{}, err
		}
		switch {
		case c == nil:
			v.hard = append(v.hard, errIssue("categoryCode", IssueNotFound,
				fmt.Sprintf("%s分类「%s」不存在，请先在分类里建好", o.Kind.Label(), o.CategoryCode)))
		case !c.IsEnabled:
			v.warn = append(v.warn, warnIssue("categoryCode", IssueUnavailable,
				fmt.Sprintf("分类「%s」已停用，前台不会展示这一列", c.Name)))
		}
	}

	// 封面与示例视频：只收 mediaId，且必须是管理端上传的封面素材（不从外部 URL 拉取）。
	seen := map[int64]bool{}
	for i, id := range o.CoverMediaIDs {
		field := fmt.Sprintf("coverMediaIds[%d]", i)
		if seen[id] {
			v.hard = append(v.hard, errIssue(field, domain.IssueDuplicate, "封面重复"))
			continue
		}
		seen[id] = true
		is, err := a.checkMedia(ctx, lk, field, "封面", id, "image/")
		if err != nil {
			return verdict{}, err
		}
		v.hard = append(v.hard, is...)
	}
	if o.PreviewMediaID != nil {
		is, err := a.checkMedia(ctx, lk, "previewMediaId", "示例视频", *o.PreviewMediaID, "video/")
		if err != nil {
			return verdict{}, err
		}
		v.hard = append(v.hard, is...)
	}

	// 推荐模型（只给创意；非创意已由 domain 报 kind_only）：不存在拒，停用 / 不支持只告警（§12 #5）。
	if o.Kind == domain.KindCreative && len(o.RecommendedModels) > 0 {
		if err := a.askModels(ctx, lk, o.RecommendedModels); err != nil {
			return verdict{}, err
		}
		seenM := map[string]bool{}
		for i, code := range o.RecommendedModels {
			field := fmt.Sprintf("recommendedModels[%d]", i)
			if code == "" {
				v.hard = append(v.hard, errIssue(field, domain.IssueRequired, "推荐模型 code 不能为空"))
				continue
			}
			if seenM[code] {
				v.hard = append(v.hard, errIssue(field, domain.IssueDuplicate, fmt.Sprintf("推荐模型「%s」重复", code)))
				continue
			}
			seenM[code] = true
			m, ok := lk.models[code]
			if !ok {
				v.hard = append(v.hard, errIssue(field, IssueNotFound, fmt.Sprintf("推荐模型「%s」在模型目录里不存在", code)))
				continue
			}
			if !m.Enabled {
				v.warn = append(v.warn, warnIssue(field, IssueUnavailable,
					fmt.Sprintf("推荐模型「%s」当前未启用，用户暂时用不了它", code)))
			}
			if !overlaps(m.OutputTypes, o.OutputTypes) {
				v.warn = append(v.warn, warnIssue(field, IssueUnavailable,
					fmt.Sprintf("推荐模型「%s」没有启用的%s类操作，与这条创意的适用产出对不上", code,
						strings.Join(o.OutputTypes, " / "))))
			}
		}
	}

	// 默认技能（只给创意）：不存在拒；停用 / 不属营销场景告警。
	if o.Kind == domain.KindCreative && o.DefaultSkillCode != "" {
		if err := a.askSkills(ctx, lk, []string{o.DefaultSkillCode}); err != nil {
			return verdict{}, err
		}
		s, ok := lk.skills[o.DefaultSkillCode]
		switch {
		case !ok:
			v.hard = append(v.hard, errIssue("defaultSkillCode", IssueNotFound,
				fmt.Sprintf("默认技能「%s」不存在", o.DefaultSkillCode)))
		default:
			if !s.Enabled {
				v.warn = append(v.warn, warnIssue("defaultSkillCode", IssueUnavailable,
					fmt.Sprintf("默认技能「%s」当前已停用", o.DefaultSkillCode)))
			}
			if s.ScenesKnown && !containsStr(s.Scenes, "marketing") {
				v.warn = append(v.warn, warnIssue("defaultSkillCode", IssueUnavailable,
					fmt.Sprintf("默认技能「%s」不属于营销场景，营销页的技能列表里不会出现它", o.DefaultSkillCode)))
			}
		}
	}
	return v, nil
}

func (a *Admin) checkMedia(ctx context.Context, lk *lookups, field, label string, id int64,
	wantPrefix string) ([]domain.Issue, error) {
	if id <= 0 {
		return []domain.Issue{errIssue(field, domain.IssueBadValue, label+" ID 非法")}, nil
	}
	m, err := a.mediaFacts(ctx, lk, id)
	if err != nil {
		return nil, err
	}
	switch {
	case m == nil:
		return []domain.Issue{errIssue(field, IssueNotFound, label+"媒体不存在或已删除")}, nil
	case m.WorkspaceID != 0 || m.Purpose != purposeMaterialCover:
		return []domain.Issue{errIssue(field, IssueBadSource,
			label+"须为管理端上传的封面素材（用途 material_cover），不能引用用户工作区的文件")}, nil
	case !strings.HasPrefix(m.ContentType, wantPrefix):
		return []domain.Issue{errIssue(field, domain.IssueBadValue,
			fmt.Sprintf("%s须为%s文件，当前是 %s", label, strings.TrimSuffix(wantPrefix, "/"), m.ContentType))}, nil
	}
	return nil, nil
}

// ---------------------------------------------------------------- 转换与工具

func normalize(in OptionInput) OptionInput {
	in.Code = strings.TrimSpace(in.Code)
	in.Name = strings.TrimSpace(in.Name)
	in.CategoryCode = strings.TrimSpace(in.CategoryCode)
	in.Description = strings.TrimSpace(in.Description)
	in.DefaultSkillCode = strings.TrimSpace(in.DefaultSkillCode)
	tags := make([]string, 0, len(in.Tags))
	for _, t := range in.Tags {
		if t = strings.TrimSpace(t); t != "" && !containsStr(tags, t) {
			tags = append(tags, t)
		}
	}
	in.Tags = tags
	for i := range in.RecommendedModels {
		in.RecommendedModels[i] = strings.TrimSpace(in.RecommendedModels[i])
	}
	if len(in.OutputTypes) == 0 {
		in.OutputTypes = []string{domain.OutputVideo}
	}
	if in.InputRequirements == nil {
		in.InputRequirements = map[string]string{}
	}
	if in.StartAt != nil {
		t := in.StartAt.UTC()
		in.StartAt = &t
	}
	if in.EndAt != nil {
		t := in.EndAt.UTC()
		in.EndAt = &t
	}
	if in.PreviewMediaID != nil && *in.PreviewMediaID == 0 {
		in.PreviewMediaID = nil
	}
	return in
}

func toOption(in OptionInput) domain.Option {
	covers := in.CoverMediaIDs
	if covers == nil {
		covers = []int64{}
	}
	models := in.RecommendedModels
	if models == nil {
		models = []string{}
	}
	return domain.Option{Kind: in.Kind, Code: in.Code, Name: in.Name, CategoryCode: in.CategoryCode,
		Description: in.Description, CoverMediaIDs: covers, PreviewMediaID: in.PreviewMediaID,
		Prompt: in.Prompt, OutputTypes: in.OutputTypes, InputRequirements: in.InputRequirements,
		IsAudioRequired: in.IsAudioRequired, RecommendedModels: models,
		RecommendedParams: in.RecommendedParams, DefaultSkillCode: in.DefaultSkillCode, Tags: in.Tags,
		StartAt: in.StartAt, EndAt: in.EndAt, Sort: in.Sort}
}

// snapshotJSON 是写进版本历史的那一版原文（形状即 domain.Snapshot，用户端钉版本时原样解回）。
func snapshotJSON(o domain.Option) (json.RawMessage, error) {
	return json.Marshal(domain.Snapshot{
		Pick: domain.Pick{Kind: o.Kind, Code: o.Code, Version: o.Version},
		Name: o.Name, Description: o.Description, Prompt: o.Prompt, Tags: o.Tags,
		IsAudioRequired: o.IsAudioRequired, OutputTypes: o.OutputTypes, InputRequirements: o.InputRequirements,
	})
}

// auditSnapshot 是审计里的选项内容：**片段只记长度与哈希**，不记原文。
func auditSnapshot(o domain.Option) map[string]any {
	sum := sha256.Sum256([]byte(o.Prompt))
	var preview any
	if o.PreviewMediaID != nil {
		preview = strconv.FormatInt(*o.PreviewMediaID, 10)
	}
	return map[string]any{
		"kind": o.Kind, "code": o.Code, "name": o.Name, "categoryCode": o.CategoryCode,
		"description": o.Description, "coverMediaIds": idStrings(o.CoverMediaIDs), "previewMediaId": preview,
		"promptLength": domain.RuneLen(o.Prompt), "promptSha256": hex.EncodeToString(sum[:]),
		"outputTypes": o.OutputTypes, "inputRequirements": o.InputRequirements,
		"isAudioRequired": o.IsAudioRequired, "recommendedModels": o.RecommendedModels,
		"recommendedParams": o.RecommendedParams, "defaultSkillCode": o.DefaultSkillCode, "tags": o.Tags,
		"startAt": o.StartAt, "endAt": o.EndAt, "sort": o.Sort, "isEnabled": o.IsEnabled, "version": o.Version,
	}
}

func categorySnapshot(c repo.Category) map[string]any {
	return map[string]any{"kind": c.Kind, "code": c.Code, "name": c.Name, "sort": c.Sort, "isEnabled": c.IsEnabled}
}

func (a *Admin) write(ctx context.Context, adminID int64, action string, id int64, before, after any) {
	a.writeObj(ctx, adminID, action, "marketing_option", id, before, after)
}

func (a *Admin) writeCategory(ctx context.Context, adminID int64, action string, id int64, before, after any) {
	a.writeObj(ctx, adminID, action, "marketing_category", id, before, after)
}

func (a *Admin) writeObj(ctx context.Context, adminID int64, action, objType string, id int64, before, after any) {
	if a.audit == nil {
		return
	}
	a.audit.Write(ctx, audit.Entry{ActorType: audit.ActorAdmin, ActorID: adminID, Action: action,
		ObjectType: objType, ObjectID: id, Before: before, After: after})
}

// conflict 把仓储错误翻成业务错误；版本冲突时带上当前版本，前端据此提示「重新加载」。
func (a *Admin) conflict(ctx context.Context, id int64, err error) error {
	if errors.Is(err, repo.ErrStale) {
		be := httpx.Errf(errcode.VersionConflict, "选项已被其他人修改，请重新加载后再保存（你的修改不会被静默覆盖）")
		if cur, gerr := a.store.GetOption(ctx, id); gerr == nil {
			be = be.WithDetails(map[string]any{"currentVersion": cur.Version})
		}
		return be
	}
	return mapErr(err, "选项不存在")
}

func mapErr(err error, notFound string) error {
	if errors.Is(err, repo.ErrNotFound) {
		return httpx.Errf(errcode.NotFound, notFound)
	}
	return httpx.Wrap(errcode.Internal, err)
}

// issuesErr 把问题一次报完：message 列出全部问题，details.issues 给前端挂到表单项上。
func issuesErr(code errcode.Code, head string, issues, warnings []domain.Issue) error {
	msgs := make([]string, 0, len(issues))
	for _, i := range issues {
		msgs = append(msgs, i.Message)
	}
	if warnings == nil {
		warnings = []domain.Issue{}
	}
	return httpx.Errf(code, fmt.Sprintf("%s（%d 处）：%s", head, len(issues), strings.Join(msgs, "；"))).
		WithDetails(map[string]any{"issues": issues, "warnings": warnings})
}

func errIssue(field string, code domain.IssueCode, msg string) domain.Issue {
	return domain.Issue{Field: field, Code: code, Message: msg, Level: domain.LevelError}
}

func warnIssue(field string, code domain.IssueCode, msg string) domain.Issue {
	return domain.Issue{Field: field, Code: code, Message: msg, Level: domain.LevelWarn}
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if containsStr(b, x) {
			return true
		}
	}
	return false
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func idStrings(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}

func parseCursor(s string) (int, int64, bool) {
	a, b, ok := strings.Cut(s, "_")
	if !ok {
		return 0, 0, false
	}
	sortV, err1 := strconv.Atoi(a)
	id, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || id <= 0 {
		return 0, 0, false
	}
	return sortV, id, true
}
