package domain

import (
	"fmt"
	"sort"
)

// 组节点：资产组 / 分镜图组 / 视频组（AV-21~AV-26）。

// GroupType 是组的种类。
type GroupType string

const (
	// GroupAsset 资产组：把脚本的角色/场景/道具图收在一个节点里。
	GroupAsset GroupType = "asset"
	// GroupStoryboardImage 分镜图组：**建组即提交**，每镜一张图。
	GroupStoryboardImage GroupType = "storyboard_image"
	// GroupVideo 视频组：**建组先落 draft**，用户确认输入与参数后再 execute。
	//
	// ⚠️ 两者的差别不是实现偷懒，是钱：分镜图一张几积分、参数几乎没得调；
	// 视频一条几十积分且输入编排（首帧用哪张、参考图给几张）直接决定成片，
	// 建组就提交等于让用户为一批他还没看过的输入付钱。
	GroupVideo GroupType = "video_generator"
)

func ValidGroupType(t GroupType) bool {
	return t == GroupAsset || t == GroupStoryboardImage || t == GroupVideo
}

// GroupStatus 是组的聚合状态。
type GroupStatus string

const (
	GroupDraft     GroupStatus = "draft"
	GroupRunning   GroupStatus = "running"
	GroupSuccess   GroupStatus = "success"
	GroupFailed    GroupStatus = "failed"
	GroupCancelled GroupStatus = "cancelled"
	// GroupPartialSuccess 一部分成了、一部分没成。
	//
	// ⚠️ 它必须是一个**独立状态**（AV-24）：批量操作是多次提交，
	// 不是一个数据库事务。把"10 条里成了 9 条"显示成 success，
	// 用户不会去翻那一条失败的；显示成 failed，他会把 9 条成功的也重跑一遍
	// —— 两种都在花他的钱。
	GroupPartialSuccess GroupStatus = "partial_success"
	// GroupUngrouped 已解组：子项已经 materialize 成独立画布节点。
	GroupUngrouped GroupStatus = "ungrouped"
)

// ItemStatus 是子项状态。
type ItemStatus string

const (
	ItemDraft     ItemStatus = "draft"
	ItemQueued    ItemStatus = "queued"
	ItemRunning   ItemStatus = "running"
	ItemSuccess   ItemStatus = "success"
	ItemFailed    ItemStatus = "failed"
	ItemCancelled ItemStatus = "cancelled"
	// ItemStale 子项依赖的镜头在建组之后改过了。
	//
	// ⚠️ 不能默默拿旧 prompt 去生成（FLOW-05）：用户改了镜头描述，
	// 而这一项还带着建组时的那份快照 —— 跑出来的画面是他已经改掉的版本，
	// 钱花了、结果还得重来。
	ItemStale ItemStatus = "stale"
)

// ItemTerminal 判断子项是否已到终态。
func ItemTerminal(s ItemStatus) bool {
	return s == ItemSuccess || s == ItemFailed || s == ItemCancelled
}

// ItemActive 判断子项是否还在跑（占着并发、可取消）。
func ItemActive(s ItemStatus) bool {
	return s == ItemQueued || s == ItemRunning
}

// ---------------------------------------------------------------- 聚合状态

// ItemView 是聚合状态计算需要的最小信息。
type ItemView struct {
	ItemNo int
	Status ItemStatus
}

// AggregateStatus 按子项算组的聚合状态（AV-24）。
//
// 优先级：**还在跑 > 有成有败 > 全成 > 全败 > 全取消 > 还没开始**。
//
// ⚠️ "有成有败就是 partial_success"这一条不能省：见 GroupPartialSuccess 的注释。
// ⚠️ `stale` 不参与"跑完了没有"的判断，但它**会拉低**结论 ——
// 一组里只要还有过期项，就不能说这组成功了。
func AggregateStatus(items []ItemView) GroupStatus {
	if len(items) == 0 {
		return GroupDraft
	}
	var running, success, failed, cancelled, draft, stale int
	for _, it := range items {
		switch it.Status {
		case ItemQueued, ItemRunning:
			running++
		case ItemSuccess:
			success++
		case ItemFailed:
			failed++
		case ItemCancelled:
			cancelled++
		case ItemStale:
			stale++
		default:
			draft++
		}
	}
	if running > 0 {
		return GroupRunning
	}
	// 有成功也有没成功的 —— 不管另一半是失败、取消还是过期。
	if success > 0 && success < len(items) {
		return GroupPartialSuccess
	}
	if success == len(items) {
		return GroupSuccess
	}
	if cancelled == len(items) {
		return GroupCancelled
	}
	if failed > 0 {
		return GroupFailed
	}
	// 只剩 draft / stale：还没真正跑过。
	return GroupDraft
}

// CanExecuteGroup 判断组现在能不能执行。
func CanExecuteGroup(t GroupType, s GroupStatus) error {
	if s == GroupUngrouped {
		return fmt.Errorf("组已解组，子项现在是独立节点，请直接操作那些节点")
	}
	if s == GroupRunning {
		return fmt.Errorf("组内还有任务在跑，等它跑完再执行")
	}
	if t == GroupAsset {
		// 资产组的重跑入口在资产本身（AV-23）：一个资产可能被多个组引用，
		// 从组里重跑会让"这张图属于谁"变得说不清。
		return fmt.Errorf("资产组不支持整组执行，请在资产列表里重新生成")
	}
	return nil
}

// CanUngroup 判断能不能解组（AV-26）。
//
// ⚠️ 还在跑的时候不许解组：子项一旦变成独立节点，回写就找不到落点了 ——
// 任务照样跑完、照样扣钱，而用户在画布上看不到任何结果。
func CanUngroup(s GroupStatus) error {
	if s == GroupUngrouped {
		return fmt.Errorf("组已解组")
	}
	if s == GroupRunning {
		return fmt.Errorf("组内还有任务在跑，取消或等待完成后再解组")
	}
	return nil
}

// ---------------------------------------------------------------- 视频输入编排

// PlanRole 是视频输入里某张素材的角色。
type PlanRole string

const (
	RoleText  PlanRole = "text"
	RoleImage PlanRole = "image"
	RoleFirst PlanRole = "first_frame"
	RoleLast  PlanRole = "last_frame"
)

// PlanImage 是一张可用作输入的图。
type PlanImage struct {
	// GenerationID 非 0 表示它来自一次成功的生成（分镜图）。
	GenerationID int64
	// MediaID 非 0 表示它是一个已归档的媒体（资产图）。
	MediaID int64
	// Kind 仅用于排序与解释，不参与上游请求。
	Kind string // storyboard / asset
}

// PlanInput 是一次视频输入规划的输入。
type PlanInput struct {
	// Operation 为空表示由规划器决定（text2video / ref2video）。
	Operation string
	Prompt    string
	// StoryboardImages 该镜**最新一次成功**的分镜图（通常 0 或 1 张）。
	StoryboardImages []PlanImage
	// AssetImages 该镜引用的资产图，**按引用顺序**。
	AssetImages []PlanImage
}

// PlanRef 是规划出来的一条输入。
type PlanRef struct {
	Role  PlanRole
	Order int
	Image PlanImage
	Text  string
}

// PlanResult 是规划结果。
type PlanResult struct {
	Operation string
	Refs      []PlanRef
}

// 分镜图操作码（目录里真实存在的 operation，别自造）。
const (
	OpText2Img = "text2img"
	OpImg2Img  = "img2img"
)

// 视频操作码。
const (
	OpText2Video     = "text2video"
	OpImg2Video      = "img2video"
	OpFirstLastFrame = "first_last_frame"
	OpRef2Video      = "ref2video"
)

// PlanVideoInputs 规划视频组子项的输入（AV-21）。
//
// ⚠️ 这是**纯函数**，三处共用一套：建组预览、整组 execute、单项 generate。
// 各实现一遍的直接后果是"预览说用 3 张图，真跑的时候只发了 1 张" ——
// 而用户是照着预览掏的钱。
//
// 规则（06 §4.2）：
//
//	没有任何图          → text2video，只发一条 text
//	有分镜图            → 分镜图排第一（首帧候选）
//	有资产图            → 追加在分镜图之后（参考锚定）
//	显式 img2video      → 只留第一张
//	显式 first_last_frame → 只留前两张，顺序即首帧/尾帧
//	有图但没指定操作    → ref2video
//
// ⚠️ 顺序就是语义：分镜图必须排在资产图前面。反过来的话首帧会取到一张
// 角色设定图（多视图拼版），出来的视频第一帧是一张"设定图"。
func PlanVideoInputs(in PlanInput) (PlanResult, error) {
	images := make([]PlanImage, 0, len(in.StoryboardImages)+len(in.AssetImages))
	images = append(images, in.StoryboardImages...)
	images = append(images, in.AssetImages...)

	op := in.Operation
	if op == "" {
		if len(images) == 0 {
			op = OpText2Video
		} else {
			op = OpRef2Video
		}
	}

	switch op {
	case OpText2Video:
		// ⚠️ 即便手上有图也不带：用户显式选了文生视频。
		return PlanResult{Operation: op, Refs: []PlanRef{
			{Role: RoleText, Order: 1, Text: in.Prompt},
		}}, nil

	case OpImg2Video:
		if len(images) < 1 {
			return PlanResult{}, fmt.Errorf("图生视频需要 1 张图，当前一张都没有")
		}
		return PlanResult{Operation: op, Refs: []PlanRef{
			{Role: RoleImage, Order: 1, Image: images[0]},
		}}, nil

	case OpFirstLastFrame:
		if len(images) < 2 {
			return PlanResult{}, fmt.Errorf("首尾帧需要 2 张图，当前只有 %d 张", len(images))
		}
		return PlanResult{Operation: op, Refs: []PlanRef{
			{Role: RoleFirst, Order: 1, Image: images[0]},
			{Role: RoleLast, Order: 2, Image: images[1]},
		}}, nil

	case OpRef2Video:
		if len(images) == 0 {
			return PlanResult{}, fmt.Errorf("参考生视频至少需要 1 张图")
		}
		refs := make([]PlanRef, 0, len(images))
		for i, img := range images {
			refs = append(refs, PlanRef{Role: RoleImage, Order: i + 1, Image: img})
		}
		return PlanResult{Operation: op, Refs: refs}, nil
	}
	return PlanResult{}, fmt.Errorf("不支持的视频操作：%s", op)
}

// ---------------------------------------------------------------- 批量规划

// BatchItem 是一次整组执行里的一项。
type BatchItem struct {
	ItemID int64
	ItemNo int
	Status ItemStatus
	Skip   bool
	Reason string
}

// PlanGroupRun 规划一次整组执行：挑出真正要提交的子项。
//
// ⚠️ 已成功的**跳过而不是重做**：重做要再花一次钱，而用户点"执行"
// 的意思是"把没跑的跑了"。想重跑某一项有单项 retry。
// ⚠️ `stale` 的**不跳过**：它恰恰是需要用新快照重跑的那些。
func PlanGroupRun(items []BatchItem, force bool) []BatchItem {
	out := make([]BatchItem, 0, len(items))
	for _, it := range items {
		switch {
		case ItemActive(it.Status):
			it.Skip, it.Reason = true, "已在队列中"
		case it.Status == ItemSuccess && !force:
			it.Skip, it.Reason = true, "已成功，跳过（重跑请用单项重试）"
		}
		out = append(out, it)
	}
	return out
}

// SortItems 让子项按 itemNo 稳定排序。
func SortItems[T any](items []T, no func(T) int) {
	sort.SliceStable(items, func(i, j int) bool { return no(items[i]) < no(items[j]) })
}
