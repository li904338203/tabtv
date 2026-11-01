package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 组节点：资产组 / 分镜图组 / 视频组（AV-21~AV-26，P07-04）。

// GroupGenRequest 是组子项的一次生成受理。
type GroupGenRequest struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ProjectID      int64
	NodeID         string
	OperationCode  string
	ModelCode      string
	Prompt         string
	Params         json.RawMessage
	// DeclaredInputs 只带 ID，不带 URL（CD-24）：服务端自己去解析成
	// 同项目、成功、类型兼容的产出。
	DeclaredInputs json.RawMessage
	DurationSec    int
	IdempotencyKey string
	// GroupItemID 让 Worker 在终态时回写子项。
	//
	// ⚠️ 不传的话子项**永远停在 queued** —— 图其实已经归档了，
	// 但组里那一格一直转圈，用户既看不到结果也不敢重试。
	// P07-02 在资产上踩过一模一样的坑，是联调才发现的。
	GroupItemID int64
	ShotID      int64
}

// GroupGenerator 由生成域实现：受理一次子项生成。
//
// 返回**实际扣的积分**，因为子项要各记各的费用（AV-22：
// 重跑某一项不该重新扣整个组的钱）。
type GroupGenerator interface {
	Generate(ctx context.Context, in GroupGenRequest) (GroupGenResult, error)
}

// GroupGenResult 是一次子项受理的结果。
type GroupGenResult struct {
	GenerationID int64
	Credits      int
	// Replayed 幂等键命中了已有的那一次，没有新建任务；Status 是那一次的状态。
	Replayed bool
	Status   string
}

// GenerationCanceller 由生成域实现：取消一次生成。
type GenerationCanceller interface {
	Cancel(ctx context.Context, workspaceID, generationID int64, reason string) error
}

func (s *Service) SetGroupGenerator(g GroupGenerator) { s.groupGen = g }
func (s *Service) SetCanceller(c GenerationCanceller) { s.canceller = c }

// ---------------------------------------------------------------- 视图

// GroupView 是组的对外视图。
type GroupView struct {
	Group repo.GroupNode
	Items []repo.GroupItem
}

// Group 取一个组及其子项。
func (s *Service) Group(ctx context.Context, workspaceID, groupID int64) (GroupView, error) {
	g, err := s.repo.GetGroup(ctx, workspaceID, groupID)
	if err != nil {
		return GroupView{}, translateGroup(err)
	}
	items, err := s.repo.ListItems(ctx, g.ID)
	if err != nil {
		return GroupView{}, httpx.Wrap(errcode.Internal, err)
	}
	return GroupView{Group: g, Items: items}, nil
}

// GroupByNode 按画布节点取组（用户重新打开节点时走这里）。
func (s *Service) GroupByNode(ctx context.Context, workspaceID, projectID int64,
	nodeID string) (GroupView, error) {
	g, err := s.repo.GetGroupByNode(ctx, workspaceID, projectID, nodeID)
	if err != nil {
		return GroupView{}, translateGroup(err)
	}
	return s.Group(ctx, workspaceID, g.ID)
}

// Groups 列出一个脚本下的全部组。
func (s *Service) Groups(ctx context.Context, workspaceID, scriptID int64) (
	[]repo.GroupNode, error) {
	list, err := s.repo.ListGroups(ctx, workspaceID, scriptID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// ---------------------------------------------------------------- 建组

// CreateGroupInput 是建组请求。
type CreateGroupInput struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ScriptID       int64
	CanvasNodeID   string
	GroupType      domain.GroupType
	Name           string
	// ShotIDs 为空表示全片。
	ShotIDs []int64
	// ModelCode 分镜图组建组即提交，因此必填；视频组可以留到 execute 时给。
	ModelCode string
	// Operation 视频组的默认操作（空＝由 videoPlan 决定）。
	Operation   string
	DurationSec int
	// Params 整组共用的生成参数（画幅、画质、清晰度、音频），只认 GroupParamKeys 里的键。
	Params map[string]any
	// Durations 视频组逐镜的时长（秒）；没给的镜头用 DurationSec，再没有就用镜头自己的时长。
	Durations map[int64]int
}

// GroupParamKeys 是建组时允许带进子项快照的参数（CD-77）。
//
// ⚠️ 此前建组接口不收任何参数：建组弹窗里选的比例 / 画质 / 清晰度一个都没发出去，
// 分镜图按模型默认画幅出（多半是 1:1），选了等于没选。
// ⚠️ 只放开这几个键：提示词由脚本域按镜头拼，带上 `prompt` / `preset` 会把它顶掉或改掉。
var GroupParamKeys = map[domain.GroupType][]string{
	domain.GroupStoryboardImage: {"aspect_ratio", "quality"},
	domain.GroupVideo:           {"aspect_ratio", "resolution", "audio"},
}

// withGroupParams 把整组参数与这一项的时长并进子项快照（快照里规划好的键不被覆盖）。
func withGroupParams(raw json.RawMessage, typ domain.GroupType, extra map[string]any,
	duration int) (json.RawMessage, error) {
	params := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
	}
	for _, k := range GroupParamKeys[typ] {
		v, ok := extra[k]
		if !ok {
			continue
		}
		switch val := v.(type) {
		case string:
			if strings.TrimSpace(val) != "" {
				params[k] = strings.TrimSpace(val)
			}
		case bool:
			params[k] = val
		}
	}
	if duration > 0 {
		// ⚠️ 键是 `duration`（各家视频协议读的就是它）。此前写的是 `durationSec`，协议读不到、一律按默认 5 秒出片，
		// 而计价按执行时给的时长算 —— 扣的积分和片子的时长对不上（CD-77）。
		params["duration"] = duration
	}
	return json.Marshal(params)
}

// CreateGroup 建组。
//
// 两种组的提交时机**故意不同**（AV-23）：
//
//   - 分镜图组：**建组即提交**。一张图几积分、参数几乎没得调，
//     多一步确认只是多一次点击。
//   - 视频组：**先落 draft**。一条视频几十积分，且输入编排（首帧用哪张、
//     参考图给几张）直接决定成片 —— 建组就提交等于让用户
//     为一批他还没看过的输入付钱。
//
// 资产组只是把已有资产聚合展示，不产生新任务。
func (s *Service) CreateGroup(ctx context.Context, in CreateGroupInput) (GroupView, error) {
	if !domain.ValidGroupType(in.GroupType) {
		return GroupView{}, httpx.Errf(errcode.InvalidArgument,
			"组类型只能是 asset / storyboard_image / video_generator")
	}
	if in.CanvasNodeID == "" {
		return GroupView{}, httpx.Errf(errcode.InvalidArgument, "缺少画布节点 ID")
	}
	sc, err := s.repo.Get(ctx, in.WorkspaceID, in.ScriptID)
	if err != nil {
		return GroupView{}, translate(err)
	}

	// 同一个画布节点重复建组 → 返回已有的那个（同 AV-05 的"按节点恢复"）。
	// 报 409 的话前端得再发一次查询，而中间那一刻用户看到的是一个失败提示。
	if existing, err := s.repo.GetGroupByNode(ctx, in.WorkspaceID, sc.ProjectID,
		in.CanvasNodeID); err == nil {
		return s.Group(ctx, in.WorkspaceID, existing.ID)
	} else if !errors.Is(err, repo.ErrGroupNotFound) {
		return GroupView{}, httpx.Wrap(errcode.Internal, err)
	}

	group := repo.GroupNode{
		ID: s.nextID(), WorkspaceID: in.WorkspaceID, ProjectID: sc.ProjectID,
		CanvasNodeID: in.CanvasNodeID, GroupType: string(in.GroupType),
		ScriptID: in.ScriptID, Name: in.Name, Status: string(domain.GroupDraft),
	}

	var items []repo.GroupItem
	switch in.GroupType {
	case domain.GroupAsset:
		items, err = s.assetItems(ctx, group.ID, in.ScriptID)
	default:
		items, err = s.shotItems(ctx, group.ID, in)
	}
	if err != nil {
		return GroupView{}, err
	}
	if len(items) == 0 {
		return GroupView{}, httpx.Errf(errcode.InvalidArgument,
			"没有可加入的子项：请先合成提示词或生成资产")
	}

	if err := s.repo.InsertGroup(ctx, group, items); err != nil {
		if errors.Is(err, repo.ErrGroupDuplicate) {
			return s.GroupByNode(ctx, in.WorkspaceID, sc.ProjectID, in.CanvasNodeID)
		}
		return GroupView{}, httpx.Wrap(errcode.Internal, err)
	}

	// 资产组不产生任务、子项建出来就带着资产的状态：当场按子项算一次组状态。
	// 不算的话组一直停在 draft —— 画布上显示「4/4 已完成 · draft」，而它其实早就齐了。
	if in.GroupType == domain.GroupAsset {
		s.refreshGroup(ctx, group.ID)
	}
	// 分镜图组建组即提交。
	if in.GroupType == domain.GroupStoryboardImage {
		return s.ExecuteGroup(ctx, ExecuteGroupInput{
			WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID,
			UserID: in.UserID, GroupID: group.ID, ModelCode: in.ModelCode,
			DurationSec: in.DurationSec,
		})
	}
	return s.Group(ctx, in.WorkspaceID, group.ID)
}

// assetItems 把脚本的资产收成子项。
func (s *Service) assetItems(ctx context.Context, groupID, scriptID int64) (
	[]repo.GroupItem, error) {
	assets, err := s.repo.ListAssetsByScript(ctx, scriptID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]repo.GroupItem, 0, len(assets))
	for i, a := range assets {
		status := domain.ItemDraft
		if a.Status == string(domain.AssetReady) {
			status = domain.ItemSuccess
		} else if a.Status == string(domain.AssetFailed) {
			status = domain.ItemFailed
		}
		out = append(out, repo.GroupItem{
			ID: s.nextID(), GroupNodeID: groupID, ItemNo: i + 1,
			ScriptAssetID: a.ID, Status: string(status),
		})
	}
	return out, nil
}

// shotItems 把镜头收成子项（分镜图组 / 视频组）。
//
// ⚠️ **只收提示词已就绪的镜头**：没有词的镜头提交上去就是一张
// 与剧情无关的图，而钱照扣。跳过的镜头要在错误里说清楚是哪几镜。
func (s *Service) shotItems(ctx context.Context, groupID int64, in CreateGroupInput) (
	[]repo.GroupItem, error) {
	kind := domain.PromptImage
	if in.GroupType == domain.GroupVideo {
		kind = domain.PromptVideo
	}
	views, err := s.Prompts(ctx, in.WorkspaceID, in.ScriptID)
	if err != nil {
		return nil, err
	}
	// ⚠️ 用的是 Prompts 算出来的**有效状态**，不是库里存的那一列 ——
	// 上游改过之后那条词已经是 stale，拿它出图出来的是旧画面。
	ready := map[int64]bool{}
	for _, v := range views {
		if v.Kind == kind && v.Status == domain.PromptReady {
			ready[v.ShotID] = true
		}
	}

	shots, err := s.repo.ListShots(ctx, in.ScriptID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	want := map[int64]bool{}
	for _, id := range in.ShotIDs {
		want[id] = true
	}

	// 跳过的镜头**连同原因**记下来：全被跳过时一句「提示词还没就绪」会把「资产还没有设定图」也说成它（CD-82）
	var skipped []string
	out := make([]repo.GroupItem, 0, len(shots))
	no := 0
	for _, sh := range shots {
		if len(want) > 0 && !want[sh.ID] {
			continue
		}
		if _, ok := ready[sh.ID]; !ok {
			skipped = append(skipped, fmt.Sprintf("第 %d 镜提示词还没就绪", sh.ShotNo))
			continue
		}
		no++
		item := repo.GroupItem{
			ID: s.nextID(), GroupNodeID: groupID, ItemNo: no,
			ShotID: sh.ID, ShotVersion: sh.Version, Status: string(domain.ItemDraft),
		}
		// ⚠️ 建组时就把输入规划好并**存进快照**：用户要能在 draft 期间
		// 看到"这一条会用哪几张图"，而预览与真跑必须是同一份数据 ——
		// 否则"预览说 3 张、实际发了 1 张"，而他是照着预览掏的钱。
		var refs, params json.RawMessage
		duration := 0
		if in.GroupType == domain.GroupVideo {
			duration = itemDurationFor(in, sh)
			refs, params, err = s.planItem(ctx, in.WorkspaceID, sh.ID, in.Operation, duration)
		} else {
			refs, params, err = s.planImageItem(ctx, sh.ID)
		}
		if err == nil {
			params, err = withGroupParams(params, in.GroupType, in.Params, duration)
		}
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("第 %d 镜%s", sh.ShotNo, err.Error()))
			no--
			continue
		}
		item.InputRefs, item.ParamsSnapshot = refs, params
		out = append(out, item)
	}
	if len(out) == 0 && len(skipped) > 0 {
		return nil, httpx.Errf(errcode.InvalidArgument, "没有能加入的镜头："+strings.Join(skipped, "；"))
	}
	return out, nil
}

// planItem 为一个镜头规划视频输入（AV-21）。
//
// ⚠️ 三处共用这一个函数：建组预览、整组 execute、单项 generate。
// 各实现一遍的直接后果见 domain.PlanVideoInputs 的注释。
func (s *Service) planItem(ctx context.Context, workspaceID, shotID int64,
	operation string, durationSec int) (json.RawMessage, json.RawMessage, error) {
	boards, err := s.repo.LatestShotImages(ctx, shotID)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	assets, err := s.repo.ShotAssetMedia(ctx, shotID)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	plan, err := domain.PlanVideoInputs(domain.PlanInput{
		Operation:        operation,
		StoryboardImages: toPlanImages(boards),
		AssetImages:      toPlanImages(assets),
	})
	if err != nil {
		return nil, nil, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	refs, err := json.Marshal(declaredFromPlan(plan))
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	values := map[string]any{"operation": plan.Operation}
	if durationSec > 0 {
		// ⚠️ `duration` 才是协议读的键（见 withGroupParams）
		values["duration"] = durationSec
	}
	params, err := json.Marshal(values)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	return refs, params, nil
}

// planImageItem 为一个镜头规划分镜图的输入。
//
// ⚠️ **必须把该镜引用的资产图带上**：合成好的图片提示词里明写着
// 「参考图对应关系：图1 = 林澈（角色）…」。不带图的话模型收到一份
// 指向空气的编号表 —— 出来的角色长什么样全凭它自己发挥，
// 而资产设定图的全部价值就是让每一镜的角色长得一样。
//
// ⚠️ 顺序必须与提示词里的编号一致（都取 `shot_asset_ref.sort`）：
// 错位的话模型会把 A 的特征画到 B 身上，而画面看上去挺正常。
func (s *Service) planImageItem(ctx context.Context, shotID int64) (
	json.RawMessage, json.RawMessage, error) {
	// ⚠️ 引用的资产缺图就**不规划**（不扣费），而不是丢掉它照跑：提示词按全部引用编号，
	// 少一张图后面的「图N」全错一位，画面里的角色张冠李戴而任务是成功的（CD-82）。
	missing, err := s.repo.ShotAssetsWithoutImage(ctx, shotID)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	if len(missing) > 0 {
		return nil, nil, httpx.Errf(errcode.InvalidArgument,
			"引用的资产还没有设定图："+strings.Join(missing, "、"))
	}
	assets, err := s.repo.ShotAssetMedia(ctx, shotID)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	// 没有资产图就是纯文生图；有就是图生图（参考已有外观）。
	op := domain.OpText2Img
	refs := make([]declaredInput, 0, len(assets))
	for i, a := range assets {
		op = domain.OpImg2Img
		refs = append(refs, declaredInput{
			Kind: "reference", Order: i + 1,
			ReferenceID: strconv.FormatInt(a.MediaID, 10),
		})
	}
	rawRefs, err := json.Marshal(refs)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	rawParams, err := json.Marshal(map[string]any{"operation": op})
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	return rawRefs, rawParams, nil
}

// declaredInput 是发给受理侧的声明输入（只有 ID，没有 URL）。
type declaredInput struct {
	Kind         string `json:"kind"`
	Order        int    `json:"order"`
	GenerationID string `json:"generationId,omitempty"`
	ReferenceID  string `json:"referenceId,omitempty"`
	Text         string `json:"text,omitempty"`
}

func declaredFromPlan(p domain.PlanResult) []declaredInput {
	out := make([]declaredInput, 0, len(p.Refs))
	for _, r := range p.Refs {
		d := declaredInput{Order: r.Order}
		switch {
		case r.Role == domain.RoleText && r.Text == "":
			// ⚠️ 空文本引用不发：子项的提示词走 params.prompt（CD-81），规划时本就不知道它。
			// 发出去的话 `omitempty` 把它序列化成 {"kind":"text","order":1}，受理侧判
			// 「上游文本节点还没有内容」—— 文生视频的组**每一项都受理失败**（MCPT-11E 第 215 步）。
			continue
		case r.Role == domain.RoleText:
			d.Kind, d.Text = "text", r.Text
		case r.Image.GenerationID != 0:
			d.Kind = "generation"
			d.GenerationID = strconv.FormatInt(r.Image.GenerationID, 10)
		default:
			// 资产图是工作区里的媒体对象，按 reference 解析（归属由解析侧校验）。
			d.Kind = "reference"
			d.ReferenceID = strconv.FormatInt(r.Image.MediaID, 10)
		}
		out = append(out, d)
	}
	return out
}

// withoutEmptyText 剔掉声明输入里没有内容的文本项；解不开或没有可剔的就原样返回。
// 剔空后返回 nil（文生视频没有任何外部输入是合法的，`[]` 与缺省对受理侧等价）。
func withoutEmptyText(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw
	}
	kept := items[:0]
	for _, it := range items {
		if it["kind"] == "text" {
			if t, _ := it["text"].(string); t == "" {
				continue
			}
		}
		kept = append(kept, it)
	}
	if len(kept) == len(items) {
		return raw
	}
	if len(kept) == 0 {
		return nil
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return raw
	}
	return out
}

// ---------------------------------------------------------------- 执行

// ExecuteGroupInput 是整组执行请求。
type ExecuteGroupInput struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	GroupID        int64
	ModelCode      string
	DurationSec    int
	// ExpectedVersion 组的乐观锁版本（0 表示不校验，仅内部调用用）。
	ExpectedVersion int
	// Operation 视频组的操作码覆盖（空＝用子项快照里的那个）。
	Operation string
	// Force 为真时连已成功的也重跑。
	Force bool
	// ItemIDs 为空表示整组。
	ItemIDs []int64
}

// ExecuteGroup 执行整组（或指定子项）。
//
// ⚠️ **每项独立受理、独立扣费**：一项失败不影响别的项，用户可以只重试
// 失败的那几个（AV-22）。整批一次扣费的话，重试就得整批重来。
//
// ⚠️ 余额不足**停掉后续项**而不是逐项失败（同 AV-13 的批量资产）：
// 逐项失败会给用户十条一模一样的"积分不足"。前面已提交的照常跑。
func (s *Service) ExecuteGroup(ctx context.Context, in ExecuteGroupInput) (GroupView, error) {
	if s.groupGen == nil {
		return GroupView{}, httpx.Errf(errcode.Internal, "生成链路未接入")
	}
	g, err := s.repo.GetGroup(ctx, in.WorkspaceID, in.GroupID)
	if err != nil {
		return GroupView{}, translateGroup(err)
	}
	if err := domain.CanExecuteGroup(domain.GroupType(g.GroupType),
		domain.GroupStatus(g.Status)); err != nil {
		return GroupView{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if in.ModelCode == "" {
		return GroupView{}, httpx.Errf(errcode.InvalidArgument, "缺少模型")
	}
	// 乐观锁：两个人同时点"执行"，没有它就是两批任务、两份钱（FLOW §5.2）。
	if _, err := s.repo.UpdateGroupStatus(ctx, g.ID, string(domain.GroupRunning),
		in.ExpectedVersion); err != nil {
		return GroupView{}, translateGroup(err)
	}

	items, err := s.repo.ListItems(ctx, g.ID)
	if err != nil {
		return GroupView{}, httpx.Wrap(errcode.Internal, err)
	}
	picked := filterItems(items, in.ItemIDs)
	planned := domain.PlanGroupRun(toBatchItems(picked), in.Force)

	byID := map[int64]repo.GroupItem{}
	for _, it := range picked {
		byID[it.ID] = it
	}
	for _, p := range planned {
		if p.Skip {
			continue
		}
		if err := s.runItem(ctx, g, byID[p.ItemID], in); err != nil {
			// ⚠️ 余额不足要停掉后续，其他错误只影响这一项。
			if errors.Is(err, errInsufficient) {
				break
			}
		}
	}
	s.refreshGroup(ctx, g.ID)
	return s.Group(ctx, in.WorkspaceID, g.ID)
}

var errInsufficient = errors.New("积分不足")

// runItem 提交一个子项。
func (s *Service) runItem(ctx context.Context, g repo.GroupNode, it repo.GroupItem,
	in ExecuteGroupInput) error {
	req, err := s.itemGenRequest(ctx, g, it, in)
	if err != nil {
		_ = s.repo.MarkItemFailed(ctx, it.ID, err.Error())
		return err
	}
	base := req.IdempotencyKey
	var res GroupGenResult
	for attempt := 0; ; attempt++ {
		if attempt == maxAssetKeyChain {
			err = fmt.Errorf("这一项已连续失败多次，请检查提示词或换一个模型再试")
			break
		}
		if res, err = s.groupGen.Generate(ctx, req); err != nil {
			break
		}
		if res.Replayed && (res.Status == "failed" || res.Status == "cancelled") {
			req.IdempotencyKey = base + ":r" + strconv.FormatInt(res.GenerationID, 10)
			continue
		}
		break
	}
	if err != nil {
		_ = s.repo.MarkItemFailed(ctx, it.ID, err.Error())
		if isInsufficient(err) {
			return errInsufficient
		}
		return err
	}
	genID, credits := res.GenerationID, res.Credits
	if ok, err := s.repo.MarkItemQueued(ctx, it.ID, genID, credits); err != nil {
		s.warn(ctx, "子项状态未能置为 queued", "itemId", it.ID, "error", err)
	} else if !ok {
		// 并发点了两次：第二次的 0 行更新就是"别人已经提交过了"。
		s.warn(ctx, "子项已被并发提交，跳过状态写入", "itemId", it.ID)
	}
	return nil
}

// itemGenRequest 组装一个子项**真正会发出去的**受理请求（幂等键为基础键）。
//
// ⚠️ 提交（runItem）与报价（PlanGroupExecution，AGT-50 的只读报价）共用这一个函数：
// 各拼一份的话，报的价与扣的钱会因为时长 / 操作码 / 输入的一点差异对不上。
func (s *Service) itemGenRequest(ctx context.Context, g repo.GroupNode, it repo.GroupItem,
	in ExecuteGroupInput) (GroupGenRequest, error) {
	prompt, operation, err := s.itemRequest(ctx, g, it)
	if err != nil {
		return GroupGenRequest{}, err
	}

	// ⚠️ 视频子项每次提交前**按当前快照重新规划**：用户可能在 draft 期间
	// 改过输入。用建组时那份的话，他改的东西不生效而界面显示已改。
	refs := it.InputRefs
	if g.GroupType == string(domain.GroupVideo) && len(refs) == 0 {
		if refs, _, err = s.planItem(ctx, g.WorkspaceID, it.ShotID, in.Operation,
			in.DurationSec); err != nil {
			return GroupGenRequest{}, err
		}
	}
	// 修复前建的子项库里还存着空文本引用，提交前同样剔掉（见 declaredFromPlan）。
	refs = withoutEmptyText(refs)

	// ⚠️ 幂等键带子项 ID 与**输入指纹**：输入没变的双击收敛到同一次。
	// ⚠️ 但同样的输入**上一次失败了**的话，同一把键永远指向那次失败：受理把它原样返回，
	// 子项被标成 queued 而再也没有任务在跑 —— 一直转圈、点重试还是它（CD-77，与 CD-34 同一个坑）。
	// 所以命中失败 / 取消的那一次时，换一把由那次的 ID 推出来的键（双击仍收敛，见 runItem）。
	params, duration := it.ParamsSnapshot, in.DurationSec
	if g.GroupType == string(domain.GroupVideo) {
		duration, params = itemDuration(it.ParamsSnapshot, in.DurationSec)
	}
	base := fmt.Sprintf("group:%d:item:%d:%s", g.ID, it.ID,
		domain.FingerprintOf(string(refs)+prompt+in.ModelCode+string(params)))
	return GroupGenRequest{
		WorkspaceID: g.WorkspaceID, PayerAccountID: in.PayerAccountID,
		UserID: in.UserID, ProjectID: g.ProjectID,
		NodeID:         g.CanvasNodeID + ":item:" + strconv.FormatInt(it.ID, 10),
		OperationCode:  operation,
		ModelCode:      in.ModelCode,
		Prompt:         prompt,
		Params:         params,
		DeclaredInputs: refs,
		DurationSec:    duration,
		IdempotencyKey: base,
		GroupItemID:    it.ID,
		ShotID:         it.ShotID,
	}, nil
}

// itemRequest 取子项的提示词与操作码。
func (s *Service) itemRequest(ctx context.Context, g repo.GroupNode, it repo.GroupItem) (
	prompt, operation string, err error) {
	if it.ShotID == 0 {
		return "", "", fmt.Errorf("子项没有关联镜头")
	}
	shot, err := s.repo.GetShot(ctx, g.ScriptID, it.ShotID)
	if err != nil {
		return "", "", err
	}
	// ⚠️ 镜头在建组之后改过就**不能拿旧快照去跑**（FLOW-05）：
	// 跑出来的画面是用户已经改掉的版本，钱花了还得重来。
	if it.ShotVersion != 0 && shot.Version != it.ShotVersion {
		return "", "", fmt.Errorf("镜头已修改（建组时版本 %d，当前 %d），请先刷新这一项",
			it.ShotVersion, shot.Version)
	}
	if g.GroupType == string(domain.GroupVideo) {
		if shot.VideoPrompt == "" {
			return "", "", fmt.Errorf("这一镜还没有视频提示词")
		}
		op := domain.OpRef2Video
		if v := paramString(it.ParamsSnapshot, "operation"); v != "" {
			op = v
		}
		return shot.VideoPrompt, op, nil
	}
	if shot.ImagePrompt == "" {
		return "", "", fmt.Errorf("这一镜还没有图片提示词")
	}
	// ⚠️ 操作码来自快照而不是写死：有资产图就是 img2img、没有就是 text2img。
	// 写死一个"分镜图"操作码的话，目录里根本没有这个 operation，
	// 绑操作、配价、能力矩阵全都落不上（联调时是一个 404）。
	op := domain.OpText2Img
	if v := paramString(it.ParamsSnapshot, "operation"); v != "" {
		op = v
	}
	return shot.ImagePrompt, op, nil
}

// ---------------------------------------------------------------- 单项操作

// RetryItem 重试单个子项。
//
// ⚠️ 只重跑这一项、只扣这一项的钱（AV-22）。整组重跑会把已经成功的
// 也重做一遍 —— 那是在花用户的钱做他没要的事。
func (s *Service) RetryItem(ctx context.Context, in ExecuteGroupInput, itemID int64) (
	GroupView, error) {
	in.ItemIDs = []int64{itemID}
	in.Force = true
	return s.ExecuteGroup(ctx, in)
}

// SaveGroupItemInput 是子项编辑请求。
type SaveGroupItemInput struct {
	WorkspaceID int64
	GroupID     int64
	ItemID      int64
	Params      json.RawMessage
	InputRefs   json.RawMessage
}

// SaveGroupItem 编辑 / 刷新 draft 子项。
//
// ⚠️ 它同时是 `stale` 的**唯一出路**：把快照对齐到镜头的当前版本，
// 并按当前上游重新规划输入。少了这一步，用户改一次镜头这一项就
// 永远跑不了了 —— 状态回到 draft、提交时却被 shotVersion 比对拦下，
// 而界面上看不出为什么。
//
// ⚠️ 调用方没显式给输入时**按当前上游重算**，不是沿用建组那份：
// 沿用的话刷新等于什么都没做，那张新出的分镜图永远进不来。
func (s *Service) SaveGroupItem(ctx context.Context, in SaveGroupItemInput) (
	repo.GroupItem, error) {
	g, err := s.repo.GetGroup(ctx, in.WorkspaceID, in.GroupID)
	if err != nil {
		return repo.GroupItem{}, translateGroup(err)
	}
	cur, err := s.repo.GetItem(ctx, in.GroupID, in.ItemID)
	if err != nil {
		return repo.GroupItem{}, translateGroup(err)
	}

	shotVersion := 0
	params, inputs := in.Params, in.InputRefs
	if cur.ShotID != 0 {
		shot, err := s.repo.GetShot(ctx, g.ScriptID, cur.ShotID)
		if err != nil {
			return repo.GroupItem{}, translateShot(err)
		}
		shotVersion = shot.Version
		if len(inputs) == 0 {
			var planned json.RawMessage
			if g.GroupType == string(domain.GroupVideo) {
				planned, _, err = s.planItem(ctx, in.WorkspaceID, cur.ShotID,
					paramString(cur.ParamsSnapshot, "operation"), 0)
			} else {
				planned, _, err = s.planImageItem(ctx, cur.ShotID)
			}
			if err != nil {
				return repo.GroupItem{}, err
			}
			inputs = planned
		}
	}

	it, err := s.repo.SaveItem(ctx, repo.SaveItemInput{
		GroupID: in.GroupID, ItemID: in.ItemID,
		ParamsSnapshot: params, InputRefs: inputs, ShotVersion: shotVersion,
	})
	if err != nil {
		if errors.Is(err, repo.ErrGroupItemNotFound) {
			return repo.GroupItem{}, httpx.Errf(errcode.NotFound, "组子项不存在")
		}
		return repo.GroupItem{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return it, nil
}

// CancelGroup 取消组内还在跑的子项。
//
// ⚠️ 逐项取消而不是"把组标成 cancelled 了事"：组状态是**算出来的**，
// 真正占着上游资源、真正要退款的是那些 generation。
func (s *Service) CancelGroup(ctx context.Context, workspaceID, groupID int64,
	itemIDs []int64) (GroupView, error) {
	g, err := s.repo.GetGroup(ctx, workspaceID, groupID)
	if err != nil {
		return GroupView{}, translateGroup(err)
	}
	items, err := s.repo.CancellableItems(ctx, g.ID)
	if err != nil {
		return GroupView{}, httpx.Wrap(errcode.Internal, err)
	}
	for _, it := range filterItems(items, itemIDs) {
		if s.canceller != nil && it.GenerationID != 0 {
			if err := s.canceller.Cancel(ctx, workspaceID, it.GenerationID, "用户取消组任务"); err != nil {
				// ⚠️ 取消失败不中断其余项：它可能刚好跑完了，
				// 那就让回写去决定终态。
				s.warn(ctx, "取消子项任务失败", "itemId", it.ID, "error", err)
				continue
			}
		}
		if err := s.repo.MarkItemCancelled(ctx, it.ID); err != nil {
			s.warn(ctx, "子项状态未能置为 cancelled", "itemId", it.ID, "error", err)
		}
	}
	s.refreshGroup(ctx, g.ID)
	return s.Group(ctx, workspaceID, g.ID)
}

// ---------------------------------------------------------------- 解组

// UngroupResult 是解组结果。
type UngroupResult struct {
	Group repo.GroupNode
	// Nodes 是要 materialize 成独立画布节点的描述。
	Nodes []UngroupNode
}

// UngroupNode 是一个待 materialize 的节点。
type UngroupNode struct {
	ItemID       int64
	ItemNo       int
	NodeID       string
	ShotID       int64
	GenerationID int64
	Params       json.RawMessage
	InputRefs    json.RawMessage
	Status       string
}

// Ungroup 解组（AV-26）。
//
// ⚠️ 服务端**不直接改 `project.canvas_data`**，只把组标成 `ungrouped`
// 并返回节点描述，由前端放进画布后走正常的乐观锁保存。
//
// 理由是画布数据归客户端所有、靠版本号保护（GAP/FLOW-09）：
// 服务端在客户端不知情时改 canvas_data，会和它的自动保存对撞 ——
// 结果是用户刚画的连线**静默消失**，而这正是乐观锁要防的那件事。
// 节点坐标也只有前端知道该放哪。
//
// ⚠️ 组行与子项行**保留不删**：前端存盘前崩了的话，
// 这些描述还能再取一次；删掉就成了既没有组也没有节点的孤儿产物。
func (s *Service) Ungroup(ctx context.Context, workspaceID, groupID int64,
	expectedVersion int) (UngroupResult, error) {
	g, err := s.repo.GetGroup(ctx, workspaceID, groupID)
	if err != nil {
		return UngroupResult{}, translateGroup(err)
	}
	if err := domain.CanUngroup(domain.GroupStatus(g.Status)); err != nil {
		return UngroupResult{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	items, err := s.repo.ListItems(ctx, g.ID)
	if err != nil {
		return UngroupResult{}, httpx.Wrap(errcode.Internal, err)
	}
	updated, err := s.repo.UpdateGroupStatus(ctx, g.ID, string(domain.GroupUngrouped),
		expectedVersion)
	if err != nil {
		return UngroupResult{}, translateGroup(err)
	}
	nodes := make([]UngroupNode, 0, len(items))
	for _, it := range items {
		nodes = append(nodes, UngroupNode{
			ItemID: it.ID, ItemNo: it.ItemNo,
			// 节点 ID 用组节点 + 子项拼出来：与子项提交时用的 nodeID 一致，
			// 于是已有的 generation 记录仍然指向这个节点（"保留其引用"）。
			NodeID:       g.CanvasNodeID + ":item:" + strconv.FormatInt(it.ID, 10),
			ShotID:       it.ShotID,
			GenerationID: it.GenerationID,
			Params:       it.ParamsSnapshot,
			InputRefs:    it.InputRefs,
			Status:       it.Status,
		})
	}
	return UngroupResult{Group: updated, Nodes: nodes}, nil
}

// ---------------------------------------------------------------- 回写

// BindGroupItem 回写子项终态（由生成域在任务终态后调用）。
//
// ⚠️ 与 BindAsset 一样，它跨域被调用，**只做两件事**：写子项状态、
// 重算组的聚合状态。不在这里发通知、不推进步骤 —— 那些都会让
// 一次状态同步变成一串有副作用的操作，而它跑在别人的 Worker 里。
func (s *Service) BindGroupItem(ctx context.Context, generationID int64,
	status, errMsg string) error {
	groupID, err := s.repo.BindItemOutcome(ctx, generationID, status, errMsg)
	if err != nil {
		return err
	}
	if groupID == 0 {
		return nil // 不是组任务，或子项已是终态。
	}
	s.refreshGroup(ctx, groupID)
	return nil
}

// refreshGroup 按子项重算组的聚合状态。
func (s *Service) refreshGroup(ctx context.Context, groupID int64) {
	items, err := s.repo.ListItems(ctx, groupID)
	if err != nil {
		s.warn(ctx, "重算组状态失败", "groupId", groupID, "error", err)
		return
	}
	views := make([]domain.ItemView, 0, len(items))
	for _, it := range items {
		views = append(views, domain.ItemView{
			ItemNo: it.ItemNo, Status: domain.ItemStatus(it.Status),
		})
	}
	status := domain.AggregateStatus(views)
	if err := s.repo.RefreshGroupStatus(ctx, groupID, string(status)); err != nil {
		s.warn(ctx, "写入组状态失败", "groupId", groupID, "error", err)
	}
}

// ---------------------------------------------------------------- 工具

func filterItems(items []repo.GroupItem, ids []int64) []repo.GroupItem {
	if len(ids) == 0 {
		return items
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := make([]repo.GroupItem, 0, len(ids))
	for _, it := range items {
		if want[it.ID] {
			out = append(out, it)
		}
	}
	return out
}

// toPlanImages 把仓储的素材行转成领域类型（repo 不反向依赖 domain）。
func toPlanImages(in []repo.PlanImage) []domain.PlanImage {
	out := make([]domain.PlanImage, 0, len(in))
	for _, x := range in {
		out = append(out, domain.PlanImage{
			GenerationID: x.GenerationID, MediaID: x.MediaID, Kind: x.Kind,
		})
	}
	return out
}

func toBatchItems(items []repo.GroupItem) []domain.BatchItem {
	out := make([]domain.BatchItem, 0, len(items))
	for _, it := range items {
		out = append(out, domain.BatchItem{
			ItemID: it.ID, ItemNo: it.ItemNo, Status: domain.ItemStatus(it.Status),
		})
	}
	return out
}

// itemDurationFor 视频组这一镜的时长：逐镜给的 > 整组给的 > 镜头自己的（分镜里定好的长度）。
func itemDurationFor(in CreateGroupInput, sh repo.Shot) int {
	if d := in.Durations[sh.ID]; d > 0 {
		return d
	}
	if in.DurationSec > 0 {
		return in.DurationSec
	}
	return sh.DurationSec
}

// itemDuration 这一项真正会发给上游的时长，以及据此要发出去的参数。
//
// ⚠️ 计价与出片必须用**同一个数**：快照里有 `duration` 就以它为准（上游读的就是它）；
// 没有而执行时给了，就把执行时的写进参数 —— 否则上游按默认 5 秒出片、计价却按执行时的秒数扣。
func itemDuration(params json.RawMessage, fallback int) (int, json.RawMessage) {
	m := map[string]any{}
	if len(params) > 0 && json.Unmarshal(params, &m) != nil {
		return fallback, params
	}
	if v, ok := m["duration"].(float64); ok && v > 0 {
		return int(v), params
	}
	if fallback <= 0 {
		return 0, params
	}
	m["duration"] = fallback
	out, err := json.Marshal(m)
	if err != nil {
		return fallback, params
	}
	return fallback, out
}

func paramString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// isInsufficient 判断是不是"积分不足"。
//
// ⚠️ 按**错误码**判而不是按文案：文案改一个字这里就失效，
// 而失效的表现是余额不足时继续提交十次、每次都失败。
func isInsufficient(err error) bool {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return be.Code == errcode.InsufficientCredits
	}
	return false
}

func translateGroup(err error) error {
	switch {
	case errors.Is(err, repo.ErrGroupNotFound):
		return httpx.Errf(errcode.NotFound, "组节点不存在")
	case errors.Is(err, repo.ErrGroupItemNotFound):
		return httpx.Errf(errcode.NotFound, "组子项不存在")
	case errors.Is(err, repo.ErrGroupConflict):
		return httpx.Errf(errcode.VersionConflict, "组已被他人修改，请刷新后重试")
	case errors.Is(err, repo.ErrGroupDuplicate):
		return httpx.Errf(errcode.Conflict, "该画布节点已经有组了")
	}
	return httpx.Wrap(errcode.Internal, err)
}

// ShotVideoView 是一镜的成片素材（供时间线按分镜装配，NEW-07）。
type ShotVideoView struct {
	ShotID  int64
	ShotNo  int
	MediaID int64
	// Reason 在 MediaID 为 0 时说明为什么没有。
	//
	// ⚠️ 要**说人话**：把 `status=failed` 原样丢过去，用户在时间线那边
	// 看到的是一个状态码，而他并不知道那是哪一步的状态。
	Reason string
}

// ShotVideos 给时间线域用：按镜号顺序回每一镜的视频产出与缺口原因。
func (s *Service) ShotVideos(ctx context.Context, workspaceID, scriptID int64) (
	[]ShotVideoView, error) {
	rows, err := s.repo.ShotVideos(ctx, workspaceID, scriptID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]ShotVideoView, 0, len(rows))
	for _, v := range rows {
		view := ShotVideoView{ShotID: v.ShotID, ShotNo: v.ShotNo, MediaID: v.MediaID}
		if v.MediaID == 0 {
			view.Reason = shotGapReason(v.Status, v.ErrorMessage)
		}
		out = append(out, view)
	}
	return out, nil
}

func shotGapReason(status, errMsg string) string {
	switch status {
	case "":
		return "还没有加入视频组"
	case "draft", "queued":
		return "已排队，还没开始生成"
	case "running":
		return "正在生成中"
	case "failed":
		if errMsg != "" {
			return "生成失败：" + errMsg
		}
		return "生成失败"
	case "cancelled":
		return "生成已取消"
	case "success":
		// 状态成功却没有产物：这本身就是一个需要有人看的问题，
		// 别把它伪装成"还没生成"。
		return "任务成功但没有产物，请重试这一镜"
	}
	return "暂无可用视频（" + status + "）"
}

// BoardShotView 是分镜成片要的一镜（docs/agent/smartcut SCT-28）：没有可用视频时 Reason 说明为什么（同按分镜自动装配的缺口）。
type BoardShotView struct {
	ShotNo      int
	DurationSec int
	Description string
	Dialogue    string
	MediaID     int64
	Reason      string
}

// BoardShots 按镜号给出每一镜的视频产出、台词与画面描述。groupID > 0 时只取那个视频组的子项。
func (s *Service) BoardShots(ctx context.Context, workspaceID, scriptID, groupID int64) ([]BoardShotView, error) {
	rows, err := s.repo.BoardShots(ctx, workspaceID, scriptID, groupID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]BoardShotView, 0, len(rows))
	for _, v := range rows {
		view := BoardShotView{ShotNo: v.ShotNo, DurationSec: v.DurationSec, Description: v.Description,
			Dialogue: v.Dialogue, MediaID: v.MediaID}
		if v.MediaID == 0 {
			view.Reason = shotGapReason(v.Status, v.ErrorMessage)
		}
		out = append(out, view)
	}
	return out, nil
}
