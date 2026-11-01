package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 用户端营销接口（MKT-T-04；dev/01 §3.1、dev/02 §4.1）：配置、模板墙 / 选项弹层、详情、公开封面。
//
// ⚠️ 全部是**只读、公开**的：未登录也能浏览模板墙（MKT-F-03）。
// ⚠️ 可见性判定只有一处（MKD-13）：列表走 repo 的同义谓词，详情 / 封面 / Resolve 取行后走
//    domain.Visible。各写一份就会出现「列表里没有但封面直链还能打开」。
// ⚠️ 用户端视图（PublicOption）**结构体上没有 prompt 字段**（MKT-AC-06）—— 不是 `json:"-"`：
//    有这个字段，总有一天会有人给它补上 json 标签。

// UserStore 是用户端用到的读（marketing/repo 实现）。
type UserStore interface {
	ListVisibleOptions(ctx context.Context, f repo.VisibleFilter) ([]repo.Option, bool, error)
	VisibleRecommendedModels(ctx context.Context, kind domain.Kind, now time.Time) ([]string, error)
	GetOptionByCode(ctx context.Context, code string) (repo.Option, error)
	GetVersionByCode(ctx context.Context, code string, version int) (repo.OptionVersion, error)
	ListCategories(ctx context.Context, kind domain.Kind) ([]repo.Category, error)
}

// Settings 是用户端服务的配置（来自环境变量，MKD-16）。
type Settings struct {
	// MaxProductImages 每个商品的图片上限（MKT-T-33 起语义是「每个商品」）。
	MaxProductImages int
	MaxVariants      int
	// MaxProducts / MaxCharacters 工作区商品 / 角色配额（MKD-34），只为经 /marketing/config 下发；
	// 真正的判定在商品库 / 角色库的插入事务里。≤ 0 按默认值。
	MaxProducts   int
	MaxCharacters int
	// AgentEnabled 为 false 时营销页能浏览、发不出去（B-5）：/marketing/config 回 available=false。
	AgentEnabled bool
}

// Service 是用户端营销服务。
type Service struct {
	store   UserStore
	catalog ModelCatalog
	// names 给「推荐了但此刻不可用」的模型补显示名；为 nil 时这类模型不列。
	names ModelLookup
	cfg   Settings
	now   Clock
	// prompts 渲染场景约束段（MKT-T-21，见 appendix.go）。
	prompts PromptRenderer
}

// NewService 构造。
func NewService(store UserStore, catalog ModelCatalog, names ModelLookup, cfg Settings) *Service {
	return &Service{store: store, catalog: catalog, names: names, cfg: cfg,
		now: func() time.Time { return time.Now().UTC() }}
}

// SetClock 注入时钟（测试用）。
func (s *Service) SetClock(c Clock) { s.now = c }

// 不可用原因（42215 的 details.reason）。
const (
	UnavailableAgentDisabled = "agent_disabled"
)

// Dimension 是输入框上的一个选择器维度。
type Dimension struct {
	Key   string
	Label string
}

// Dimensions 是营销输入框的四个选择器，顺序即胶囊顺序：技能 → 创意 → Hook → 风格（MKT-F-14）。
var Dimensions = []Dimension{
	{Key: "skill", Label: "技能"},
	{Key: string(domain.KindCreative), Label: domain.KindCreative.Label()},
	{Key: string(domain.KindHook), Label: domain.KindHook.Label()},
	{Key: string(domain.KindStyle), Label: domain.KindStyle.Label()},
}

// ConfigView 是 `GET /marketing/config`。
type ConfigView struct {
	Available bool
	// UnavailableReason 只在 Available=false 时给（目前只有 agent_disabled）。
	UnavailableReason string
	Slots             []domain.SlotDecl
	MaxProductImages  int
	MaxVariants       int
	Dimensions        []Dimension
	// Categories 按 kind 给出**启用的**分类，按 sort。
	Categories map[domain.Kind][]repo.Category
	// CreativeModelFilters 是「按模型」筛选项：在投创意推荐过的 ∩ 当前可用视频模型。
	CreativeModelFilters []ModelRef
	// 商品库 / 角色库的上限与规则（MKT-T-37；dev/05 §3.3、MKD-34~36）：前端预检读这里，不写死 ——
	// 写死的话改配置后「界面允许、提交被拒」。
	Library LibraryConfig
}

// LibraryConfig 是 `/marketing/config` 里商品库 / 角色库那一部分。
type LibraryConfig struct {
	MaxProductsPerSession int
	MaxCharacterRefs      int
	MaxCharacterImages    int
	MaxCharacterGenCount  int
	QuotaProducts         int
	QuotaCharacters       int
	// CharacterNameForbidden 角色名禁用字符（逐字符一项），唯一来源 domain.CharacterNameForbidden。
	CharacterNameForbidden []string
	AllowedImageMimes      []string
	// 文本长度上限（码点）。
	MaxProductName        int
	MaxSellingPoints      int
	MaxProductDescription int
	MaxCharacterName      int
	MaxAppearance         int
	MaxVoiceText          int
}

// libraryConfig 组装库配置（全部来自 domain 常量与配置值，只有这一处）。
func (s *Service) libraryConfig() LibraryConfig {
	forbidden := []string{}
	for _, r := range domain.CharacterNameForbidden {
		forbidden = append(forbidden, string(r))
	}
	quotaP, quotaC := s.cfg.MaxProducts, s.cfg.MaxCharacters
	if quotaP <= 0 {
		quotaP = domain.DefaultMaxProducts
	}
	if quotaC <= 0 {
		quotaC = domain.DefaultMaxCharacters
	}
	return LibraryConfig{
		MaxProductsPerSession: domain.MaxProductsPerSession, MaxCharacterRefs: domain.MaxCharacterRefs,
		MaxCharacterImages: domain.MaxCharacterImages, MaxCharacterGenCount: domain.MaxCharacterGenCount,
		QuotaProducts: quotaP, QuotaCharacters: quotaC, CharacterNameForbidden: forbidden,
		AllowedImageMimes: append([]string(nil), domain.AllowedImageMimes...),
		MaxProductName:    domain.MaxProductName, MaxSellingPoints: domain.MaxSellingPoints,
		MaxProductDescription: domain.MaxProductDescription, MaxCharacterName: domain.MaxCharacterName,
		MaxAppearance: domain.MaxAppearance, MaxVoiceText: domain.MaxVoiceText,
	}
}

// Config 给出营销页的输入框配置与分类字典（MKT-D-03：前端不写死营销页有哪些槽）。
func (s *Service) Config(ctx context.Context) (ConfigView, error) {
	now := s.now()
	v := ConfigView{
		Available: true, Slots: domain.DefaultSlots(),
		MaxProductImages: s.cfg.MaxProductImages, MaxVariants: s.cfg.MaxVariants, Dimensions: Dimensions,
		Categories: map[domain.Kind][]repo.Category{}, Library: s.libraryConfig(),
	}
	if v.MaxProductImages <= 0 {
		v.MaxProductImages = domain.DefaultMaxProductImages
	}
	// ⚠️ B-5：Agent 关着时营销页仍可浏览（模板墙是门面），但发送必然失败 ——
	// 告诉前端「不可用」，不放一个点了没反应的按钮。
	if !s.cfg.AgentEnabled {
		v.Available, v.UnavailableReason = false, UnavailableAgentDisabled
	}
	cats, err := s.store.ListCategories(ctx, "")
	if err != nil {
		return ConfigView{}, httpx.Wrap(errcode.Internal, err)
	}
	for _, k := range domain.Kinds {
		v.Categories[k] = []repo.Category{}
	}
	for _, c := range cats { // 已按 kind、sort、id 排好
		if c.IsEnabled && c.Kind.Valid() {
			v.Categories[c.Kind] = append(v.Categories[c.Kind], c)
		}
	}
	filters, err := s.creativeModelFilters(ctx, now)
	if err != nil {
		return ConfigView{}, err
	}
	v.CreativeModelFilters = filters
	return v, nil
}

// creativeModelFilters 派生「按模型」筛选项（MKT-F-31、MKT-AC-13）。
//
// 顺序跟随模型目录（与用户端模型下拉一致），不按推荐次数排：推荐次数是运营录入的副产物，
// 不是用户关心的东西。
func (s *Service) creativeModelFilters(ctx context.Context, now time.Time) ([]ModelRef, error) {
	recommended, err := s.store.VisibleRecommendedModels(ctx, domain.KindCreative, now)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := []ModelRef{}
	if len(recommended) == 0 {
		return out, nil
	}
	usable, err := s.catalog.UsableModels(ctx, domain.OutputVideo)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	want := make(map[string]bool, len(recommended))
	for _, c := range recommended {
		want[c] = true
	}
	for _, m := range usable {
		if want[m.Code] {
			out = append(out, m)
		}
	}
	return out, nil
}

// RecommendedModel 是创意上推荐的一个模型。Available=false 表示它此刻不在用户端模型下拉里
// （停用 / Provider 未就绪）：照样列出，由前端说明，而不是悄悄少一个（MKT-F-19 同一条）。
type RecommendedModel struct {
	Code      string
	Name      string
	Available bool
}

// PublicOption 是用户端看到的一条选项。
//
// ⚠️ **没有 Prompt 字段**（MKT-AC-06），也没有媒体 ID：封面与示例视频只按 code + 下标取
// （`/marketing/options/{code}/covers/{index}`），媒体 ID 拿去走受保护端点一律 401。
type PublicOption struct {
	Kind              domain.Kind
	Code              string
	Name              string
	CategoryCode      string
	Description       string
	Version           int
	Lifecycle         domain.LifecycleState
	CoverCount        int
	HasPreview        bool
	IsAudioRequired   bool
	OutputTypes       []string
	InputRequirements map[string]string
	Tags              []string
	// 以下三项只给创意（其余类型为空值）。
	RecommendedModels []RecommendedModel
	RecommendedParams json.RawMessage
	DefaultSkillCode  string
}

// OptionsPage 是列表的一页。
type OptionsPage struct {
	Items      []PublicOption
	NextCursor string
}

// OptionsQuery 是列表筛选（transport 解析好）。
type OptionsQuery struct {
	Kind     domain.Kind
	Category string
	Model    string
	Q        string
	Cursor   string
	Limit    int
}

// ListOptions 是模板墙与选项弹层**同一个**接口（MKD-7）：只返回此刻在投的，服务端筛选、游标分页。
func (s *Service) ListOptions(ctx context.Context, q OptionsQuery) (OptionsPage, error) {
	if !q.Kind.Valid() {
		return OptionsPage{}, httpx.Errf(errcode.InvalidArgument, "kind 只能是 creative / hook / style")
	}
	f := repo.VisibleFilter{Kind: q.Kind, Category: strings.TrimSpace(q.Category),
		Model: strings.TrimSpace(q.Model), Q: q.Q, Now: s.now(), Limit: q.Limit}
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 24
	}
	if q.Cursor != "" {
		sortV, id, ok := parseCursor(q.Cursor)
		if !ok {
			return OptionsPage{}, httpx.Errf(errcode.InvalidArgument, "cursor 非法")
		}
		f.CursorSort, f.CursorID = sortV, id
	}
	rows, more, err := s.store.ListVisibleOptions(ctx, f)
	if err != nil {
		return OptionsPage{}, httpx.Wrap(errcode.Internal, err)
	}
	opts := make([]domain.Option, 0, len(rows))
	for _, r := range rows {
		opts = append(opts, r.Option)
	}
	models, err := s.modelIndex(ctx, opts)
	if err != nil {
		return OptionsPage{}, err
	}
	page := OptionsPage{Items: make([]PublicOption, 0, len(rows))}
	for _, r := range rows {
		page.Items = append(page.Items, publicOption(r.Option, f.Now, models))
	}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor = fmt.Sprintf("%d_%d", last.Sort, last.ID)
	}
	return page, nil
}

// GetOption 取一条在投选项的详情。
//
// ⚠️ 不可见与不存在回**同一个 404**：区分开等于告诉对方「曾经有过这个活动」。
func (s *Service) GetOption(ctx context.Context, code string) (PublicOption, error) {
	now := s.now()
	o, err := s.visible(ctx, code, now)
	if err != nil {
		return PublicOption{}, err
	}
	models, err := s.modelIndex(ctx, []domain.Option{o})
	if err != nil {
		return PublicOption{}, err
	}
	return publicOption(o, now, models), nil
}

// CoverMedia 返回第 index 张封面（从 0 起）的媒体 ID —— **先判可见**，再由调用方交给公开取图。
//
// ⚠️ 只能按 code + 下标取，绝不接受客户端给的媒体 ID：公开取图不做归属校验，
// 接受 ID 等于开了一条「拿任何 ID 取任何人的图」的路。下标越界与不可见同一个 404。
func (s *Service) CoverMedia(ctx context.Context, code string, index int) (int64, error) {
	o, err := s.visible(ctx, code, s.now())
	if err != nil {
		return 0, err
	}
	if index < 0 || index >= len(o.CoverMediaIDs) {
		return 0, errOptionNotFound()
	}
	return o.CoverMediaIDs[index], nil
}

// PreviewMedia 返回示例视频的媒体 ID（先判可见；没有示例视频同一个 404）。
func (s *Service) PreviewMedia(ctx context.Context, code string) (int64, error) {
	o, err := s.visible(ctx, code, s.now())
	if err != nil {
		return 0, err
	}
	if o.PreviewMediaID == nil || *o.PreviewMediaID <= 0 {
		return 0, errOptionNotFound()
	}
	return *o.PreviewMediaID, nil
}

// visible 取一条**此刻在投**的选项；不存在、格式不对、不可见一律同一个 404。
func (s *Service) visible(ctx context.Context, code string, now time.Time) (domain.Option, error) {
	if !domain.ValidCode(code) {
		return domain.Option{}, errOptionNotFound()
	}
	row, err := s.store.GetOptionByCode(ctx, code)
	if errors.Is(err, repo.ErrNotFound) {
		return domain.Option{}, errOptionNotFound()
	}
	if err != nil {
		return domain.Option{}, httpx.Wrap(errcode.Internal, err)
	}
	if !domain.Visible(row.Option, now) {
		return domain.Option{}, errOptionNotFound()
	}
	return row.Option, nil
}

func errOptionNotFound() error { return httpx.Errf(errcode.NotFound, "选项不存在或已下线") }

// modelIndex 给一批选项上推荐的模型配上名称与「此刻可用」。
// 不存在的模型（已被删除）直接不列：保存时已拒绝不存在的，这里只会是事后删掉的。
func (s *Service) modelIndex(ctx context.Context, opts []domain.Option) (map[string]RecommendedModel, error) {
	out := map[string]RecommendedModel{}
	has := false
	for _, o := range opts {
		if o.Kind == domain.KindCreative && len(o.RecommendedModels) > 0 {
			has = true
			break
		}
	}
	if !has {
		return out, nil
	}
	usable, err := s.catalog.UsableModels(ctx, domain.OutputVideo)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	for _, m := range usable {
		out[m.Code] = RecommendedModel{Code: m.Code, Name: m.Name, Available: true}
	}
	var missing []string
	for _, o := range opts {
		for _, c := range o.RecommendedModels {
			if _, ok := out[c]; !ok && !containsStr(missing, c) {
				missing = append(missing, c)
			}
		}
	}
	if len(missing) > 0 && s.names != nil {
		facts, err := s.names.LookupModels(ctx, missing)
		if err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		for c, f := range facts {
			out[c] = RecommendedModel{Code: c, Name: f.Name, Available: false}
		}
	}
	return out, nil
}

func publicOption(o domain.Option, now time.Time, models map[string]RecommendedModel) PublicOption {
	p := PublicOption{
		Kind: o.Kind, Code: o.Code, Name: o.Name, CategoryCode: o.CategoryCode, Description: o.Description,
		Version: o.Version, Lifecycle: domain.Lifecycle(o, now), CoverCount: len(o.CoverMediaIDs),
		HasPreview: o.PreviewMediaID != nil && *o.PreviewMediaID > 0, IsAudioRequired: o.IsAudioRequired,
		OutputTypes: nonNil(o.OutputTypes), InputRequirements: o.InputRequirements, Tags: nonNil(o.Tags),
		RecommendedModels: []RecommendedModel{}, RecommendedParams: json.RawMessage("{}"),
	}
	if p.InputRequirements == nil {
		p.InputRequirements = map[string]string{}
	}
	if o.Kind == domain.KindCreative {
		for _, c := range o.RecommendedModels {
			if m, ok := models[c]; ok {
				p.RecommendedModels = append(p.RecommendedModels, m)
			}
		}
		if len(o.RecommendedParams) > 0 && strings.TrimSpace(string(o.RecommendedParams)) != "null" {
			p.RecommendedParams = o.RecommendedParams
		}
		p.DefaultSkillCode = o.DefaultSkillCode
	}
	return p
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
