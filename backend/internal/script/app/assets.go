package app

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 资产 reconcile、编辑、批量生成与跨脚本复用（P07-02）。

// AssetGenerator 由生成域实现：为一个资产发起一次 asset_gen 任务。
//
// ⚠️ 资产图走 **generation 队列**（与同步 LLM 那条链分开）：产出是媒体，
// 要归档、要重试、要按产物计价。两条链在日志、幂等与退款上分别追踪。
type AssetGenerator interface {
	// Generate 受理一次资产图生成。
	Generate(ctx context.Context, in AssetGenRequest) (AssetGenResult, error)
}

type AssetGenRequest struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ProjectID      int64
	// NodeID 用脚本节点 + 资产 ID 拼出来：资产图不占独立画布节点，
	// 但生成记录需要一个稳定的归属位置。
	NodeID         string
	ModelCode      string
	Prompt         string
	IdempotencyKey string
	AssetID        int64
	// Params 画幅 / 画质（已按 AssetParamKeys 过滤）。提示词由代码拼，不在这里。
	Params map[string]any
}

// AssetGenResult 是一次受理的结果。
type AssetGenResult struct {
	GenerationID int64
	// Replayed 幂等键命中了已有的那一次，没有新建任务；Status 是那一次的状态。
	Replayed bool
	Status   string
}

// AssetParamKeys 是批量生成资产图时允许用户调的参数：只有画幅与画质。
//
// ⚠️ 不放开任意键：资产图的版式（四视图 / 2×2 / 2×3）由代码固定（AssetSheetSpec），
// 带上 `preset` 之类的键会让服务端再往提示词后面拼一段配方，把版式改掉。
var AssetParamKeys = []string{"aspect_ratio", "quality"}

// maxAssetKeyChain 是「上一次失败 → 换下一把键」最多走几步。
// 每一步都对应同一描述下一次真实的失败，走到这里说明这段描述 / 这个模型反复失败，该停下来让人看。
const maxAssetKeyChain = 20

// BalanceReader 由账本域实现：批量生成前看一眼余额。
type BalanceReader interface {
	Balance(ctx context.Context, payerID int64) (int, error)
}

func (s *Service) SetAssetGenerator(g AssetGenerator) { s.assets = g }
func (s *Service) SetBalanceReader(b BalanceReader)   { s.balance = b }

// ---------------------------------------------------------------- reconcile

// ReconcileResult 是一次资产对账的结果。
type ReconcileResult struct {
	Assets []repo.Asset
	// Unmatched 镜头里出现但模型没给档案的实体名（退回了模板描述）。
	Unmatched []string
	// Orphan 本次镜头中不再出现的旧资产 —— **只报不删**。
	Orphan []repo.Asset
}

// ReconcileAssets 按镜头实体与档案对账资产（AV-11）。
//
// ⚠️ 归并范围是**本脚本**，不是工作区全部资产。老系统按用户级同名匹配，
// 新节点里的 `@唐僧` 直接命中数周前的记录，把别的节点的成品图
// **和 failed 状态一起**继承过来 —— 用户看到凭空出现的"生成失败"
// 和一张不认识的占位图。跨脚本复用是用户**手动挑选**的动作（AV-20），
// 不是自动识别的一部分。
func (s *Service) ReconcileAssets(ctx context.Context, workspaceID, scriptID int64) (
	ReconcileResult, error) {
	sc, err := s.repo.Get(ctx, workspaceID, scriptID)
	if err != nil {
		return ReconcileResult{}, translate(err)
	}
	shots, err := s.repo.ListShots(ctx, scriptID)
	if err != nil {
		return ReconcileResult{}, httpx.Wrap(errcode.Internal, err)
	}
	existing, err := s.repo.ListAssetsByScript(ctx, scriptID)
	if err != nil {
		return ReconcileResult{}, httpx.Wrap(errcode.Internal, err)
	}

	plan := domain.Reconcile(domain.ReconcileInput{
		Entities:    entitiesOf(shots),
		Profiles:    profilesOf(sc.AssetProfiles),
		Existing:    toDomainAssets(existing),
		GlobalStyle: sc.GlobalStyle,
	})

	for _, a := range plan.Create {
		id := s.nextID()
		if err := s.repo.InsertAsset(ctx, repo.Asset{
			ID: id, WorkspaceID: workspaceID, Type: string(a.Type), Name: a.Name,
			Description: a.Description, Profile: rawJSON(a.Profile),
			Source: string(a.Source), Status: string(a.Status),
		}); err != nil {
			return ReconcileResult{}, httpx.Wrap(errcode.Internal, err)
		}
	}
	for _, a := range plan.Update {
		desc, typ := a.Description, string(a.Type)
		if _, err := s.repo.UpdateAsset(ctx, repo.UpdateAssetInput{
			ID: a.ID, WorkspaceID: workspaceID, Type: &typ, Description: &desc,
			Profile: rawJSON(a.Profile),
		}); err != nil {
			return ReconcileResult{}, httpx.Wrap(errcode.Internal, err)
		}
	}

	// ⚠️ 缺档案**汇总成一条告警**，不逐个刷日志：漏 20 个实体时逐条打
	// 会把真正要看的那一行淹掉，而运营要的恰恰是"这次漏了哪些"这张名单。
	if len(plan.Unmatched) > 0 {
		s.warn(ctx, "分镜产出缺少部分实体的档案，已退回模板描述",
			"scriptId", scriptID, "count", len(plan.Unmatched),
			"names", strings.Join(plan.Unmatched, "、"))
	}

	// 资产建好之后重写镜头引用：顺序即参考图的锚定顺序。
	if err := s.rebindShotRefs(ctx, workspaceID, scriptID, shots); err != nil {
		return ReconcileResult{}, err
	}

	assets, err := s.repo.ListAssetsByScript(ctx, scriptID)
	if err != nil {
		return ReconcileResult{}, httpx.Wrap(errcode.Internal, err)
	}
	return ReconcileResult{
		Assets: assets, Unmatched: plan.Unmatched, Orphan: toRepoAssets(plan.Orphan),
	}, nil
}

// rebindShotRefs 按每个镜头的实体顺序重写引用。
//
// ⚠️ 按**镜头实体的出现顺序**排，不是按资产 ID 或名字：同一个资产在不同
// 镜头里的位次不同，排序必须每个镜头各算一次。顺序错了，模型会把
// A 的特征画到 B 身上，而画面看上去"挺正常"—— 这种错最难被发现。
func (s *Service) rebindShotRefs(ctx context.Context, workspaceID, scriptID int64,
	shots []repo.Shot) error {
	assets, err := s.repo.ListAssetsByScriptOrWorkspace(ctx, workspaceID, scriptID)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	byName := map[string]int64{}
	for _, a := range assets {
		byName[a.Name] = a.ID
	}
	for _, sh := range shots {
		refs := domain.OrderRefs(entitiesOfShot(sh), byName)
		rows := make([]repo.ShotRef, 0, len(refs))
		for _, r := range refs {
			rows = append(rows, repo.ShotRef{AssetID: r.AssetID, Sort: r.Sort})
		}
		if err := s.repo.ReplaceShotRefs(ctx, sh.ID, s.nextID, rows); err != nil {
			return httpx.Wrap(errcode.Internal, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- 编辑

// SaveAssetInput 是一次资产编辑（AV-12）。
type SaveAssetInput struct {
	WorkspaceID int64
	AssetID     int64
	Type        *string
	Name        *string
	Description *string
	Profile     json.RawMessage
}

// SaveAsset 编辑资产。
func (s *Service) SaveAsset(ctx context.Context, in SaveAssetInput) (repo.Asset, []domain.Lint, error) {
	if in.Name != nil {
		if err := domain.ValidateAssetName(*in.Name); err != nil {
			return repo.Asset{}, nil, httpx.Errf(errcode.InvalidArgument, err.Error())
		}
	}
	if in.Type != nil && !validType(*in.Type) {
		return repo.Asset{}, nil, httpx.Errf(errcode.InvalidArgument,
			"资产类型只能是 character / scene / prop")
	}
	// ⚠️ 用户改了描述之后，来源标成 upload 之外的"手改"没有单独状态位；
	// 靠 reconcile 侧的规则保护：已 ready 或 upload 的不覆盖。
	updated, err := s.repo.UpdateAsset(ctx, repo.UpdateAssetInput{
		ID: in.AssetID, WorkspaceID: in.WorkspaceID, Type: in.Type, Name: in.Name,
		Description: in.Description, Profile: in.Profile,
	})
	if err != nil {
		return repo.Asset{}, nil, translateAsset(err)
	}
	// ⚠️ 资产描述进了下游提示词：改了它，引用这个资产的镜头提示词都该变灰。
	// 不标的话，用户改完角色外观、提示词却还显示 ready —— 他拿着一段过时的词
	// 去出图，画面里那个角色还是旧的样子，而界面上一切正常。
	if in.Description != nil || in.Name != nil || in.Type != nil {
		if err := s.repo.MarkPromptsStaleByAsset(ctx, in.AssetID); err != nil {
			s.warn(ctx, "标记提示词过期失败", "assetId", in.AssetID, "error", err)
		}
	}
	return updated, s.lint(ctx, in.WorkspaceID, updated), nil
}

// AttachMedia 把上传好的图绑成资产图（AV-12 的"上传替换"）。
//
// ⚠️ 这里只做**绑定**：文件本身由媒体域上传（它按实际文件头识别类型、
// 校验大小、登记归属）。资产域再信一次客户端声明的 MIME 就是白做。
func (s *Service) AttachMedia(ctx context.Context, workspaceID, assetID, mediaID int64,
	mime string, size int64) (repo.Asset, error) {
	if err := domain.ValidateAssetMedia(mime, size); err != nil {
		return repo.Asset{}, httpx.Errf(errcode.UnsupportedMediaType, err.Error())
	}
	source := string(domain.SourceUpload)
	status := string(domain.AssetReady)
	updated, err := s.repo.UpdateAsset(ctx, repo.UpdateAssetInput{
		ID: assetID, WorkspaceID: workspaceID, MediaObjectID: &mediaID,
		Source: &source, Status: &status,
	})
	if err != nil {
		return repo.Asset{}, translateAsset(err)
	}
	return updated, nil
}

// Assets 列出本脚本的资产（带质量提示）。
func (s *Service) Assets(ctx context.Context, workspaceID, scriptID int64) (
	[]repo.Asset, map[int64][]domain.Lint, error) {
	if _, err := s.repo.Get(ctx, workspaceID, scriptID); err != nil {
		return nil, nil, translate(err)
	}
	list, err := s.repo.ListAssetsByScript(ctx, scriptID)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	names := make([]string, 0, len(list))
	for _, a := range list {
		names = append(names, a.Name)
	}
	lints := map[int64][]domain.Lint{}
	for _, a := range list {
		if l := domain.LintAsset(toDomainAsset(a), names); len(l) > 0 {
			lints[a.ID] = l
		}
	}
	return list, lints, nil
}

// Library 工作区级资产库（AV-20 跨脚本复用的挑选入口）。
func (s *Service) Library(ctx context.Context, workspaceID int64,
	assetType, keyword string, limit int) ([]repo.Asset, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	list, err := s.repo.ListAssetsByWorkspace(ctx, workspaceID, assetType, keyword, limit)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// AttachExisting 把资产库里已有的资产挂到某个镜头上（手动复用）。
//
// ⚠️ 这是跨脚本复用的**唯一**入口：它是用户的显式动作。
// 自动识别永远只在本脚本内归并 —— 两者混在一起就是老系统那个
// "凭空出现的生成失败"。
func (s *Service) AttachExisting(ctx context.Context, workspaceID, scriptID, shotID,
	assetID int64) (Detail, error) {
	if _, err := s.repo.Get(ctx, workspaceID, scriptID); err != nil {
		return Detail{}, translate(err)
	}
	if _, err := s.repo.GetAsset(ctx, workspaceID, assetID); err != nil {
		return Detail{}, translateAsset(err)
	}
	shot, err := s.repo.GetShot(ctx, scriptID, shotID)
	if err != nil {
		return Detail{}, translateShot(err)
	}
	refs, err := s.repo.ListShotRefs(ctx, scriptID)
	if err != nil {
		return Detail{}, httpx.Wrap(errcode.Internal, err)
	}
	current := refs[shot.ID]
	for _, r := range current {
		if r.AssetID == assetID {
			// 已经挂上了：原样返回而不是报错（重复点击不该失败）。
			return s.detailOf(ctx, workspaceID, scriptID)
		}
	}
	// 追加到末尾：锚定顺序按"先有的在前"，与用户看到的列表一致。
	current = append(current, repo.ShotRef{AssetID: assetID, Sort: len(current) + 1})
	if err := s.repo.ReplaceShotRefs(ctx, shot.ID, s.nextID, current); err != nil {
		return Detail{}, httpx.Wrap(errcode.Internal, err)
	}
	return s.detailOf(ctx, workspaceID, scriptID)
}

// ---------------------------------------------------------------- 批量生成

// BatchAssetInput 是一次批量资产生成（AV-13）。
type BatchAssetInput struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ScriptID       int64
	ModelCode      string
	// AssetIDs 为空表示"把所有未就绪的补上"。
	AssetIDs []int64
	UnitCost int
	// Params 画幅 / 画质，只认 AssetParamKeys 里的键（其余丢弃）。
	Params map[string]any
}

// BatchAssetItem 是批量生成里的一项结果。
type BatchAssetItem struct {
	AssetID      int64
	Name         string
	GenerationID int64
	Skipped      bool
	Reason       string
}

// GenerateAssets 批量生成资产图。
//
// ⚠️ **每项独立任务、独立扣费**（AV-13）：一项失败不影响别的项，
// 用户可以只重试失败的那几个。整批一次扣费的话，重试就得整批重来。
//
// ⚠️ 余额不足**停掉后续项**而不是逐项失败：逐项失败会给用户十条
// 一模一样的"积分不足"，每条还占一次任务记录。停下来并说清"还差多少"
// 才是有用的。前面已经提交的照常跑 —— 这是**允许部分成功**的操作。
func (s *Service) GenerateAssets(ctx context.Context, in BatchAssetInput) (
	[]BatchAssetItem, error) {
	if s.assets == nil {
		return nil, httpx.Errf(errcode.Internal, "资产生成链路未接入")
	}
	sc, err := s.repo.Get(ctx, in.WorkspaceID, in.ScriptID)
	if err != nil {
		return nil, translate(err)
	}
	all, err := s.repo.ListAssetsByScript(ctx, in.ScriptID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	picked := filterAssets(all, in.AssetIDs)
	if len(picked) == 0 {
		return nil, httpx.Errf(errcode.InvalidArgument, "没有可生成的资产")
	}

	balance := 0
	if s.balance != nil {
		if balance, err = s.balance.Balance(ctx, in.PayerAccountID); err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
	}
	decisions := domain.PlanBatch(toDomainAssets(picked), in.UnitCost, balance)
	params := pickAssetParams(in.Params)

	out := make([]BatchAssetItem, 0, len(decisions))
	stopped := ""
	for _, d := range decisions {
		item := BatchAssetItem{AssetID: d.Asset.ID, Name: d.Asset.Name}
		if d.Skip {
			item.Skipped = true
			item.Reason = d.Reason
			out = append(out, item)
			continue
		}
		if stopped != "" {
			item.Skipped, item.Reason = true, stopped
			out = append(out, item)
			continue
		}
		res, err := s.generateAsset(ctx, in, sc, d.Asset, params)
		if err != nil {
			item.Skipped = true
			item.Reason = err.Error()
			// ⚠️ 积分不足**停掉后续项**（按错误码判，不按文案）：后面每一项都会得到同一句「积分不足」，
			// 每条还占一次受理。单价没报进来时（UnitCost=0）前面那道预判挡不住，靠的就是这里。
			var biz *httpx.BizError
			if errors.As(err, &biz) && biz.Code == errcode.InsufficientCredits {
				stopped = "积分不足，已停止后续生成"
			}
			// 其他错误**不中断整批**：后面的资产与这一个无关，但把原因带回去，用户才知道该重试哪个。
			out = append(out, item)
			continue
		}
		item.GenerationID = res.GenerationID
		if res.Replayed && res.Status == "success" {
			// 这段描述上次已经出过图：回写早就发生过了，再标 generating 它就会永远转圈。
			item.Skipped, item.Reason = true, "这段描述已经生成过资产图"
			out = append(out, item)
			continue
		}
		generating := string(domain.AssetGenerating)
		if _, err := s.repo.UpdateAsset(ctx, repo.UpdateAssetInput{
			ID: d.Asset.ID, WorkspaceID: in.WorkspaceID, Status: &generating,
		}); err != nil {
			s.warn(ctx, "资产状态未能置为 generating", "assetId", d.Asset.ID, "error", err)
		}
		out = append(out, item)
	}
	return out, nil
}

// generateAsset 为一个资产受理一次出图。
//
// ⚠️ 幂等键带上资产 ID 与**当前描述的指纹**：改了描述再点生成应当是一次新任务，
// 而不是命中上一次的结果（双击仍然收敛到同一次）。
// ⚠️ 但同一段描述**上一次失败了**的话，同一把键永远指向那次失败：受理把它原样返回，
// 资产被标成 generating 而再也没有任务在跑 —— 一直转圈、再点还是它（CD-77，与 CD-34 的脚本轮次同一个坑）。
// 所以命中失败 / 取消的那一次时，换一把**由那次的 ID 推出来**的键：双击仍收敛到同一次，
// 而每一次真实的失败都会让下一次重试拿到新键。
func (s *Service) generateAsset(ctx context.Context, in BatchAssetInput, sc repo.Script,
	a domain.Asset, params map[string]any) (AssetGenResult, error) {
	base := "asset:" + strconv.FormatInt(a.ID, 10) + ":" + domain.FingerprintOf(a.Description)
	key := base
	for range maxAssetKeyChain {
		res, err := s.assets.Generate(ctx, AssetGenRequest{
			WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID,
			UserID: in.UserID, ProjectID: sc.ProjectID,
			NodeID:    sc.CanvasNodeID + ":asset:" + strconv.FormatInt(a.ID, 10),
			ModelCode: in.ModelCode, Prompt: domain.AssetPrompt(a),
			IdempotencyKey: key, AssetID: a.ID, Params: params,
		})
		if err != nil {
			return AssetGenResult{}, err
		}
		if res.Replayed && (res.Status == "failed" || res.Status == "cancelled") {
			key = base + ":r" + strconv.FormatInt(res.GenerationID, 10)
			continue
		}
		return res, nil
	}
	return AssetGenResult{}, httpx.Errf(errcode.InvalidArgument,
		"这段描述已连续失败多次，请修改描述或换一个模型再试")
}

// pickAssetParams 只留 AssetParamKeys 里的键，且只收字符串。
func pickAssetParams(in map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range AssetParamKeys {
		if v, ok := in[k].(string); ok && strings.TrimSpace(v) != "" {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// BindAsset 回写资产图的生成结果（由生成域在任务终态后调用）。
//
// ⚠️ 它跨域被调用，但**只做一件事**：写状态。不在这里顺带做别的
// （比如自动推进步骤、发通知）—— 那些都会让一次状态同步变成
// 一串有副作用的操作，而它跑在别人的 Worker 里。
func (s *Service) BindAsset(ctx context.Context, assetID, mediaID int64, status string) error {
	return s.repo.MarkAssetResult(ctx, assetID, mediaID, status)
}

// AssetsReady 本脚本的资产是否全部就绪（供步骤推进判断）。
func (s *Service) AssetsReady(ctx context.Context, scriptID int64) (bool, error) {
	list, err := s.repo.ListAssetsByScript(ctx, scriptID)
	if err != nil {
		return false, err
	}
	for _, a := range list {
		if !toDomainAsset(a).IsReady() {
			return false, nil
		}
	}
	return true, nil
}

// ---------------------------------------------------------------- 辅助

func (s *Service) lint(ctx context.Context, workspaceID int64, a repo.Asset) []domain.Lint {
	siblings, err := s.repo.ListAssetsByWorkspace(ctx, workspaceID, "", "", 200)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(siblings))
	for _, x := range siblings {
		names = append(names, x.Name)
	}
	return domain.LintAsset(toDomainAsset(a), names)
}

func (s *Service) detailOf(ctx context.Context, workspaceID, scriptID int64) (Detail, error) {
	sc, err := s.repo.Get(ctx, workspaceID, scriptID)
	if err != nil {
		return Detail{}, translate(err)
	}
	return s.detail(ctx, sc)
}

func filterAssets(all []repo.Asset, ids []int64) []repo.Asset {
	if len(ids) == 0 {
		return all
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := make([]repo.Asset, 0, len(ids))
	for _, a := range all {
		if want[a.ID] {
			out = append(out, a)
		}
	}
	return out
}

func entitiesOf(shots []repo.Shot) []domain.Entity {
	var out []domain.Entity
	for _, sh := range shots {
		out = append(out, entitiesOfShot(sh)...)
	}
	return out
}

func entitiesOfShot(sh repo.Shot) []domain.Entity {
	var out []domain.Entity
	if len(sh.Entities) > 0 {
		_ = json.Unmarshal(sh.Entities, &out)
	}
	return out
}

func profilesOf(raw json.RawMessage) []domain.AssetProfile {
	var out []domain.AssetProfile
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func toDomainAssets(list []repo.Asset) []domain.Asset {
	out := make([]domain.Asset, 0, len(list))
	for _, a := range list {
		out = append(out, toDomainAsset(a))
	}
	return out
}

func toDomainAsset(a repo.Asset) domain.Asset {
	var p domain.AssetProfile
	if len(a.Profile) > 0 {
		_ = json.Unmarshal(a.Profile, &p)
	}
	return domain.Asset{
		ID: a.ID, Type: domain.AssetType(a.Type), Name: a.Name,
		Description: a.Description, Profile: p, MediaObjectID: a.MediaObjectID,
		Source: domain.AssetSource(a.Source), Status: domain.AssetStatus(a.Status),
	}
}

func toRepoAssets(list []domain.Asset) []repo.Asset {
	out := make([]repo.Asset, 0, len(list))
	for _, a := range list {
		out = append(out, repo.Asset{
			ID: a.ID, Type: string(a.Type), Name: a.Name, Description: a.Description,
			MediaObjectID: a.MediaObjectID, Source: string(a.Source),
			Status: string(a.Status),
		})
	}
	return out
}

func validType(t string) bool {
	return t == "character" || t == "scene" || t == "prop"
}

func translateAsset(err error) error {
	if err == nil {
		return nil
	}
	if errorsIsNotFound(err) {
		return httpx.Errf(errcode.NotFound, "资产不存在")
	}
	return httpx.Wrap(errcode.Internal, err)
}
