package domain

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 聚合状态

// ⚠️ 核心：**部分失败不能被摊平成 success**（AV-24）。
// 显示成 success，用户不会去翻那一条失败的；显示成 failed，
// 他会把已经成功的也重跑一遍 —— 两种都在花他的钱。
func TestAggregateStatus(t *testing.T) {
	item := func(s ItemStatus) ItemView { return ItemView{Status: s} }
	cases := []struct {
		name  string
		items []ItemView
		want  GroupStatus
	}{
		{"空组算草稿", nil, GroupDraft},
		{"全部成功", []ItemView{item(ItemSuccess), item(ItemSuccess)}, GroupSuccess},
		{"全部失败", []ItemView{item(ItemFailed), item(ItemFailed)}, GroupFailed},
		{"一成一败是部分成功", []ItemView{item(ItemSuccess), item(ItemFailed)},
			GroupPartialSuccess},
		{"九成一败仍是部分成功", []ItemView{
			item(ItemSuccess), item(ItemSuccess), item(ItemSuccess), item(ItemSuccess),
			item(ItemSuccess), item(ItemSuccess), item(ItemSuccess), item(ItemSuccess),
			item(ItemSuccess), item(ItemFailed)}, GroupPartialSuccess},
		{"还有在跑的就是 running", []ItemView{item(ItemSuccess), item(ItemRunning)},
			GroupRunning},
		{"排队中也算 running", []ItemView{item(ItemQueued), item(ItemDraft)}, GroupRunning},
		{"全部取消", []ItemView{item(ItemCancelled), item(ItemCancelled)}, GroupCancelled},
		{"有成功有取消也是部分成功", []ItemView{item(ItemSuccess), item(ItemCancelled)},
			GroupPartialSuccess},
		{"失败混取消算失败", []ItemView{item(ItemFailed), item(ItemCancelled)}, GroupFailed},
		{"全是草稿", []ItemView{item(ItemDraft), item(ItemDraft)}, GroupDraft},
		// ⚠️ 只要还有过期项，就不能说这组成功了 —— 那一项跑出来的画面
		// 是用户已经改掉的版本。
		{"有成功有过期是部分成功", []ItemView{item(ItemSuccess), item(ItemStale)},
			GroupPartialSuccess},
		{"只剩过期算草稿（还没真正跑过）", []ItemView{item(ItemStale)}, GroupDraft},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AggregateStatus(c.items); got != c.want {
				t.Fatalf("want %s got %s", c.want, got)
			}
		})
	}
}

// ---------------------------------------------------------------- 执行与解组门槛

func TestCanExecuteGroup(t *testing.T) {
	if err := CanExecuteGroup(GroupVideo, GroupDraft); err != nil {
		t.Fatalf("草稿视频组应可执行: %v", err)
	}
	if err := CanExecuteGroup(GroupVideo, GroupPartialSuccess); err != nil {
		t.Fatalf("部分成功应可补跑: %v", err)
	}
	if CanExecuteGroup(GroupVideo, GroupRunning) == nil {
		t.Fatal("还在跑时不该允许再次执行（会重复扣费）")
	}
	if CanExecuteGroup(GroupVideo, GroupUngrouped) == nil {
		t.Fatal("已解组的不该还能整组执行")
	}
	// AV-23：资产组的重试入口在资产本身。
	err := CanExecuteGroup(GroupAsset, GroupDraft)
	if err == nil || !strings.Contains(err.Error(), "资产列表") {
		t.Fatalf("资产组应引导到资产列表: %v", err)
	}
}

// ⚠️ 还在跑的时候解组，回写就找不到落点了 ——
// 任务照样跑完、照样扣钱，而用户在画布上看不到任何结果。
func TestCanUngroup(t *testing.T) {
	if err := CanUngroup(GroupSuccess); err != nil {
		t.Fatalf("成功的组应可解组: %v", err)
	}
	if err := CanUngroup(GroupPartialSuccess); err != nil {
		t.Fatalf("部分成功也应可解组: %v", err)
	}
	if CanUngroup(GroupRunning) == nil {
		t.Fatal("运行中不该允许解组")
	}
	if CanUngroup(GroupUngrouped) == nil {
		t.Fatal("重复解组应被拒绝")
	}
}

// ---------------------------------------------------------------- 视频输入编排

func board(id int64) PlanImage { return PlanImage{GenerationID: id, Kind: "storyboard"} }
func asset(id int64) PlanImage { return PlanImage{MediaID: id, Kind: "asset"} }

func TestPlanVideoInputs(t *testing.T) {
	cases := []struct {
		name     string
		in       PlanInput
		wantOp   string
		wantRefs []PlanRole
		wantErr  string
	}{
		{
			name:   "一张图都没有就是文生视频",
			in:     PlanInput{Prompt: "一个人走过天台"},
			wantOp: OpText2Video, wantRefs: []PlanRole{RoleText},
		},
		{
			name: "有图未指定操作默认参考生视频",
			in: PlanInput{Prompt: "p", StoryboardImages: []PlanImage{board(1)},
				AssetImages: []PlanImage{asset(2)}},
			wantOp: OpRef2Video, wantRefs: []PlanRole{RoleImage, RoleImage},
		},
		{
			name:   "只有资产图也走参考生视频",
			in:     PlanInput{Prompt: "p", AssetImages: []PlanImage{asset(2)}},
			wantOp: OpRef2Video, wantRefs: []PlanRole{RoleImage},
		},
		{
			name: "显式文生视频时有图也不带",
			in: PlanInput{Operation: OpText2Video, Prompt: "p",
				StoryboardImages: []PlanImage{board(1)}},
			wantOp: OpText2Video, wantRefs: []PlanRole{RoleText},
		},
		{
			name: "图生视频只留第一张",
			in: PlanInput{Operation: OpImg2Video, StoryboardImages: []PlanImage{board(1)},
				AssetImages: []PlanImage{asset(2), asset(3)}},
			wantOp: OpImg2Video, wantRefs: []PlanRole{RoleImage},
		},
		{
			name: "首尾帧取前两张且分首尾",
			in: PlanInput{Operation: OpFirstLastFrame,
				StoryboardImages: []PlanImage{board(1)},
				AssetImages:      []PlanImage{asset(2), asset(3)}},
			wantOp: OpFirstLastFrame, wantRefs: []PlanRole{RoleFirst, RoleLast},
		},
		{
			name:    "图生视频没有图要报错而不是静默降级",
			in:      PlanInput{Operation: OpImg2Video},
			wantErr: "需要 1 张图",
		},
		{
			name: "首尾帧只有一张图要报错",
			in: PlanInput{Operation: OpFirstLastFrame,
				StoryboardImages: []PlanImage{board(1)}},
			wantErr: "需要 2 张图",
		},
		{
			name:    "参考生视频没有图要报错",
			in:      PlanInput{Operation: OpRef2Video},
			wantErr: "至少需要 1 张图",
		},
		{
			name:    "不认识的操作要拒绝",
			in:      PlanInput{Operation: "video2video"},
			wantErr: "不支持的视频操作",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := PlanVideoInputs(c.in)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("want error %q got %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Operation != c.wantOp {
				t.Fatalf("操作码 want %s got %s", c.wantOp, got.Operation)
			}
			if len(got.Refs) != len(c.wantRefs) {
				t.Fatalf("输入条数 want %d got %d", len(c.wantRefs), len(got.Refs))
			}
			for i, role := range c.wantRefs {
				if got.Refs[i].Role != role {
					t.Fatalf("第 %d 条角色 want %s got %s", i+1, role, got.Refs[i].Role)
				}
				if got.Refs[i].Order != i+1 {
					t.Fatalf("第 %d 条 order 应为 %d，实际 %d", i+1, i+1, got.Refs[i].Order)
				}
			}
		})
	}
}

// ⚠️ 顺序就是语义：分镜图必须排在资产图前面。反过来的话首帧会取到
// 一张角色设定图（多视图拼版），出来的视频第一帧是一张"设定图"。
func TestPlanVideoInputs_分镜图永远排在资产图之前(t *testing.T) {
	got, err := PlanVideoInputs(PlanInput{
		AssetImages:      []PlanImage{asset(100), asset(101)},
		StoryboardImages: []PlanImage{board(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Refs[0].Image.GenerationID != 1 {
		t.Fatalf("第一条应是分镜图，实际 %+v", got.Refs[0].Image)
	}
	if got.Refs[1].Image.MediaID != 100 || got.Refs[2].Image.MediaID != 101 {
		t.Fatal("资产图应保持引用顺序排在分镜图之后")
	}

	// 首尾帧同理：首帧必须是分镜图。
	ff, err := PlanVideoInputs(PlanInput{Operation: OpFirstLastFrame,
		AssetImages: []PlanImage{asset(100)}, StoryboardImages: []PlanImage{board(1)}})
	if err != nil {
		t.Fatal(err)
	}
	if ff.Refs[0].Image.GenerationID != 1 {
		t.Fatal("首帧应取分镜图，不是资产设定图")
	}
}

// ---------------------------------------------------------------- 整组执行规划

func TestPlanGroupRun(t *testing.T) {
	items := []BatchItem{
		{ItemNo: 1, Status: ItemSuccess},
		{ItemNo: 2, Status: ItemFailed},
		{ItemNo: 3, Status: ItemDraft},
		{ItemNo: 4, Status: ItemRunning},
		{ItemNo: 5, Status: ItemStale},
	}
	got := PlanGroupRun(items, false)
	want := map[int]bool{1: true, 2: false, 3: false, 4: true, 5: false}
	for _, it := range got {
		if it.Skip != want[it.ItemNo] {
			t.Errorf("第 %d 项 skip want %v got %v（%s）", it.ItemNo, want[it.ItemNo],
				it.Skip, it.Reason)
		}
	}
	// ⚠️ stale 不跳过：它恰恰是需要用新快照重跑的那些。
	if got[4].Skip {
		t.Fatal("过期项必须重跑，否则用户永远拿不到改后的画面")
	}

	// force 时连成功的也重跑 —— 但运行中的仍然跳过（重复提交会重复扣费）。
	forced := PlanGroupRun(items, true)
	if forced[0].Skip {
		t.Fatal("force 时已成功的也该重跑")
	}
	if !forced[3].Skip {
		t.Fatal("运行中的任何时候都不该重复提交")
	}
}
