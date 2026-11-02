package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	marketingdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediadomain "github.com/li904338203/tabtv/backend/internal/media/domain"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	seeddata "github.com/li904338203/tabtv/backend/internal/platform/seed/data"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// 内置营销内容（TT-T-24，dev/02 TTD-27）。
//
// 来源系统里营销数据**不进 seed**，由运营经管理端导入、逐条上传封面、逐条显式启用（MKD-21）：那是线上发布决定。
// 桌面版没有运营 —— 用户装上就要看到模板墙、能用营销 Agent，所以首次启动把内容写进库并启用。
// 走的是进程内的管理端服务（与管理端同一套校验），不是直接写表。
//
// 规则：
//   - 按 code（分类按 kind+code、角色按名称不区分大小写）判断，**已存在就跳过** —— 营销表没有
//     is_user_modified 列，无法区分「用户改过」与「旧内置版本」，所以不更新已有行，只补缺的；
//   - 推荐模型 / 技能绑定的模型只保留本机目录里存在的（来源数据按来源环境的模型 code 写，本机多半没有，
//     管理端校验「不存在即拒」）；
//   - 封面在新建时上传为平台素材（workspace 0、用途 material_cover）；新建失败就软删掉刚上传的封面，
//     免得每次启动都多一份孤儿文件；通用技能（seed 写的）没有封面时补上。
//   - 启用失败（例如校验不过）只记日志，不阻断启动。

type mkOptionsFile struct {
	Categories []struct {
		Kind string `json:"kind"`
		Code string `json:"code"`
		Name string `json:"name"`
		Sort int    `json:"sort"`
	} `json:"categories"`
	Skills  []mkSkill  `json:"skills"`
	Options []mkOption `json:"options"`
}

type mkSkill struct {
	Code          string                    `json:"code"`
	Name          string                    `json:"name"`
	Category      string                    `json:"category"`
	Description   string                    `json:"description"`
	PrefillText   *string                   `json:"prefillText"`
	Tags          []string                  `json:"tags"`
	Body          string                    `json:"body"`
	Steps         []agentdomain.SkillStep   `json:"steps"`
	Tools         []string                  `json:"tools"`
	ModelBindings agentdomain.SkillBindings `json:"modelBindings"`
	IsQuickEntry  bool                      `json:"isQuickEntry"`
	Sort          int                       `json:"sort"`
	Scenes        []string                  `json:"scenes"`
	InputSlots    json.RawMessage           `json:"inputSlots"`
	IsBatch       *bool                     `json:"isBatch"`
}

type mkOption struct {
	Kind              string            `json:"kind"`
	Code              string            `json:"code"`
	Name              string            `json:"name"`
	CategoryCode      string            `json:"categoryCode"`
	Sort              int               `json:"sort"`
	Description       string            `json:"description"`
	Prompt            string            `json:"prompt"`
	OutputTypes       []string          `json:"outputTypes"`
	InputRequirements map[string]string `json:"inputRequirements"`
	IsAudioRequired   bool              `json:"isAudioRequired"`
	RecommendedModels []string          `json:"recommendedModels"`
	RecommendedParams json.RawMessage   `json:"recommendedParams"`
	DefaultSkillCode  string            `json:"defaultSkillCode"`
	Tags              []string          `json:"tags"`
}

type mkSkillsFile struct {
	Skills []mkSkill `json:"skills"`
}

type mkRefsFile struct {
	SkillRefs []struct {
		SkillCodes []string `json:"skillCodes"`
		Name       string   `json:"name"`
		Sort       int      `json:"sort"`
		Body       string   `json:"body"`
	} `json:"skillRefs"`
}

type mkPresetsFile struct {
	PresetCharacters []struct {
		Name       string   `json:"name"`
		Summary    string   `json:"summary"`
		Appearance string   `json:"appearance"`
		VoiceText  string   `json:"voiceText"`
		Images     []string `json:"images"`
	} `json:"presetCharacters"`
}

// BuiltinReport 是一次内置内容安装的结果（日志与测试用）。
type BuiltinReport struct {
	Categories, Skills, Refs, Options, Presets, SkillCovers int
	Failures                                                []string
}

type builtinInstaller struct {
	pool    *pgxpool.Pool
	media   *mediaapp.Service
	catalog *catalogapp.Service
	skills  *agentapp.SkillAdmin
	options *marketingapp.Admin
	presets *marketingapp.PresetAdmin
	adminID int64
	log     *slog.Logger
	models  map[string]bool
	rep     BuiltinReport
}

// installBuiltinContent 安装内置营销内容（幂等）。只在出错到无法继续时返回 error；单条失败记进报告。
func installBuiltinContent(ctx context.Context, pool *pgxpool.Pool, store *objstore.Client, nextID func() int64,
	adminID int64, log *slog.Logger) (BuiltinReport, error) {
	catalog := catalogapp.New(catalogrepo.New(pool), nextID)
	media := mediaapp.New(mediarepo.New(pool), store, nextID)
	skillAdmin := agentapp.NewSkillAdmin(agentrepo.New(pool), skillModels{catalog}, wiring.AgentSkillRenderer{}, nextID)
	skillAdmin.SetMarketingChecks(wiring.AgentSkillCopy{}, wiring.AgentSkillSlots{})
	in := &builtinInstaller{
		pool: pool, media: media, catalog: catalog, skills: skillAdmin, adminID: adminID, log: log,
		options: wiring.NewMarketingAdmin(wiring.MarketingAdminInput{
			Repo: marketingrepo.New(pool), Catalog: catalog, Agent: agentrepo.New(pool), Media: media, NextID: nextID,
		}),
		presets: marketingapp.NewPresetAdmin(marketingrepo.New(pool), wiring.MarketingMedia{Media: media}, nextID),
	}
	ms, err := catalog.ListModels(ctx)
	if err != nil {
		return in.rep, err
	}
	in.models = map[string]bool{}
	for _, m := range ms {
		in.models[m.Code] = true
	}

	var opts mkOptionsFile
	var drafts mkSkillsFile
	var refs mkRefsFile
	var presets mkPresetsFile
	for name, dst := range map[string]any{
		"marketing/options-2026-10-21.json":          &opts,
		"marketing/skills-draft-2026-11-01.json":     &drafts,
		"marketing/skill-refs-draft-2026-11-01.json": &refs,
		"marketing/preset-characters-draft.json":     &presets,
	} {
		b, err := fs.ReadFile(seeddata.FS, name)
		if err != nil {
			return in.rep, err
		}
		if err := json.Unmarshal(b, dst); err != nil {
			return in.rep, fmt.Errorf("内置数据 %s 解析失败：%w", name, err)
		}
	}

	// 顺序与来源导入工具一致：分类 → 技能 → 附属文档 → 选项（选项引用分类与默认技能）→ 角色。
	for _, c := range opts.Categories {
		in.category(ctx, c.Kind, c.Code, c.Name, c.Sort)
	}
	for _, s := range opts.Skills {
		in.skill(ctx, s, true)
	}
	// 7 个建议稿技能（未经运营审定）装上但**不启用**：用户在设置 → Agent 技能里看得到、自己决定开不开
	//（2026-11-02 定，TT-T-24 执行记录）。已经装过的不动 —— 用户可能已经自己开了。
	for _, s := range drafts.Skills {
		in.skill(ctx, s, false)
	}
	for _, r := range refs.SkillRefs {
		for _, code := range r.SkillCodes {
			in.skillRef(ctx, code, r.Name, r.Body, r.Sort)
		}
	}
	in.generalSkillCovers(ctx)
	for _, o := range opts.Options {
		in.option(ctx, o)
	}
	for _, p := range presets.PresetCharacters {
		in.preset(ctx, p.Name, marketingdomain.PresetInput{Name: p.Name, Summary: p.Summary,
			Appearance: p.Appearance, VoiceText: p.VoiceText}, p.Images)
	}
	if len(in.rep.Failures) > 0 {
		log.Warn("部分内置营销内容没有装上（不影响启动）", "failures", in.rep.Failures)
	}
	return in.rep, nil
}

func (in *builtinInstaller) fail(what string, err error) {
	in.rep.Failures = append(in.rep.Failures, fmt.Sprintf("%s：%v", what, err))
}

func (in *builtinInstaller) exists(ctx context.Context, q string, args ...any) (int64, bool) {
	var id int64
	err := in.pool.QueryRow(ctx, q, args...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		in.fail("查询", err)
		return 0, true // 查不清就当存在：宁可少装，不重复插
	}
	return id, true
}

// upload 把一张内置图片上传为平台封面素材，返回媒体 ID；文件不存在返回 0。
func (in *builtinInstaller) upload(ctx context.Context, file string) int64 {
	b, err := fs.ReadFile(seeddata.FS, file)
	if err != nil {
		return 0
	}
	up, err := in.media.Upload(ctx, mediaapp.UploadInput{
		WorkspaceID: 0, UserID: in.adminID, Purpose: "material_cover", DeclaredMIME: "image/jpeg",
		FileName: path.Base(file), Size: int64(len(b)), Body: bytes.NewReader(b), ExpectKind: mediadomain.KindImage,
	})
	if err != nil {
		in.fail("上传封面 "+file, err)
		return 0
	}
	return up.ID
}

func (in *builtinInstaller) discard(ctx context.Context, ids ...int64) {
	for _, id := range ids {
		if id != 0 {
			_ = in.media.Delete(ctx, 0, id)
		}
	}
}

func (in *builtinInstaller) category(ctx context.Context, kind, code, name string, sort int) {
	if _, ok := in.exists(ctx, `select id from marketing_category where kind = $1 and code = $2`, kind, code); ok {
		return
	}
	enabled := true
	if _, err := in.options.CreateCategory(ctx, in.adminID, marketingapp.CategoryInput{
		Kind: marketingdomain.Kind(kind), Code: code, Name: name, Sort: sort, IsEnabled: &enabled}); err != nil {
		in.fail("分类 "+code, err)
		return
	}
	in.rep.Categories++
}

func (in *builtinInstaller) keepModel(code string) string {
	if in.models[code] {
		return code
	}
	return ""
}

func (in *builtinInstaller) skill(ctx context.Context, s mkSkill, enable bool) {
	if _, ok := in.exists(ctx, `select id from agent_skill where code = $1`, s.Code); ok {
		return
	}
	cover := in.upload(ctx, "images/skills/"+s.Code+".jpg")
	if cover == 0 {
		cover = in.upload(ctx, "images/options/"+s.Code+".jpg")
	}
	saved, err := in.skills.Create(ctx, in.adminID, agentapp.SkillInput{
		Code: s.Code, Name: s.Name, Category: s.Category, Description: s.Description, Tags: s.Tags,
		CoverMediaID: cover, Body: s.Body, Steps: s.Steps, Tools: s.Tools,
		Bindings: agentdomain.SkillBindings{Video: in.keepModel(s.ModelBindings.Video),
			Image: in.keepModel(s.ModelBindings.Image), Reasoning: in.keepModel(s.ModelBindings.Reasoning)},
		IsQuickEntry: s.IsQuickEntry, Sort: s.Sort, Scenes: s.Scenes, InputSlots: s.InputSlots,
		PrefillText: s.PrefillText, IsBatch: s.IsBatch,
	})
	if err != nil {
		in.discard(ctx, cover)
		in.fail("技能 "+s.Code, err)
		return
	}
	in.rep.Skills++
	if !enable {
		return
	}
	if _, err := in.skills.SetEnabled(ctx, in.adminID, saved.ID, true); err != nil {
		in.fail("启用技能 "+s.Code, err)
	}
}

func (in *builtinInstaller) skillRef(ctx context.Context, code, name, body string, sort int) {
	var skillID int64
	var version int
	err := in.pool.QueryRow(ctx, `select id, version from agent_skill where code = $1`, code).Scan(&skillID, &version)
	if err != nil {
		in.fail("附属文档 "+name+" 的技能 "+code, err)
		return
	}
	if _, ok := in.exists(ctx, `select id from agent_skill_reference where skill_id = $1 and name = $2`, skillID, name); ok {
		return
	}
	if _, err := in.skills.SaveRef(ctx, in.adminID, skillID, 0, version,
		agentapp.SkillRefInput{Name: name, Body: body, Sort: sort}); err != nil {
		in.fail("附属文档 "+code+"/"+name, err)
		return
	}
	in.rep.Refs++
}

// generalSkillCovers 给 seed 写的通用技能补封面（只在还没有封面时；直接写列，不算用户修改）。
func (in *builtinInstaller) generalSkillCovers(ctx context.Context) {
	entries, err := fs.ReadDir(seeddata.FS, "images/skills")
	if err != nil {
		return
	}
	for _, e := range entries {
		code := strings.TrimSuffix(e.Name(), ".jpg")
		var id int64
		err := in.pool.QueryRow(ctx, `select id from agent_skill where code = $1 and cover_media_id is null`, code).Scan(&id)
		if err != nil {
			continue
		}
		cover := in.upload(ctx, "images/skills/"+e.Name())
		if cover == 0 {
			continue
		}
		if _, err := in.pool.Exec(ctx, `update agent_skill set cover_media_id = $2, updated_at = now()
			where id = $1 and cover_media_id is null`, id, cover); err != nil {
			in.discard(ctx, cover)
			in.fail("技能封面 "+code, err)
			continue
		}
		in.rep.SkillCovers++
	}
}

func (in *builtinInstaller) option(ctx context.Context, o mkOption) {
	if _, ok := in.exists(ctx, `select id from marketing_option where code = $1`, o.Code); ok {
		return
	}
	var models []string
	for _, m := range o.RecommendedModels {
		if in.models[m] {
			models = append(models, m)
		}
	}
	cover := in.upload(ctx, "images/options/"+o.Code+".jpg")
	var covers []int64
	if cover != 0 {
		covers = []int64{cover}
	}
	v, err := in.options.Create(ctx, in.adminID, marketingapp.OptionInput{
		Kind: marketingdomain.Kind(o.Kind), Code: o.Code, Name: o.Name, CategoryCode: o.CategoryCode,
		Description: o.Description, CoverMediaIDs: covers, Prompt: o.Prompt, OutputTypes: o.OutputTypes,
		InputRequirements: o.InputRequirements, IsAudioRequired: o.IsAudioRequired, RecommendedModels: models,
		RecommendedParams: o.RecommendedParams, DefaultSkillCode: o.DefaultSkillCode, Tags: o.Tags, Sort: o.Sort,
	})
	if err != nil {
		in.discard(ctx, cover)
		in.fail("选项 "+o.Code, err)
		return
	}
	in.rep.Options++
	if _, err := in.options.Enable(ctx, in.adminID, v.ID); err != nil {
		in.fail("启用选项 "+o.Code, err)
	}
}

func (in *builtinInstaller) preset(ctx context.Context, name string, p marketingdomain.PresetInput, images []string) {
	if _, ok := in.exists(ctx, `select id from marketing_preset_character where lower(name) = lower($1)`, name); ok {
		return
	}
	var ids []int64
	for _, img := range images {
		file := "images/characters/" + strings.TrimSuffix(img, path.Ext(img)) + ".jpg"
		if id := in.upload(ctx, file); id != 0 {
			ids = append(ids, id)
		}
	}
	p.ImageMediaIDs = ids
	v, err := in.presets.Create(ctx, in.adminID, p)
	if err != nil {
		in.discard(ctx, ids...)
		in.fail("营销角色 "+name, err)
		return
	}
	in.rep.Presets++
	if _, err := in.presets.Enable(ctx, in.adminID, v.ID); err != nil {
		in.fail("启用营销角色 "+name, err)
	}
}
