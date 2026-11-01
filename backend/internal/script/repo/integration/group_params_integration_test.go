//go:build integration

// 建组参数与视频时长（CD-77）。
//
// ⚠️ 起因：建组接口此前不收任何参数 —— 建组弹窗里选的比例 / 画质 / 清晰度一个都没发出去；
// 视频子项的时长写在 `durationSec` 键上，而各家视频协议读的是 `duration`，于是一律按默认 5 秒出片，
// 计价却按执行时给的秒数扣，扣的积分和片子的时长对不上。
package integration

import (
	"context"
	"encoding/json"
	"testing"

	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
)

func paramsOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("参数不是合法 JSON: %s", raw)
		}
	}
	return m
}

func TestCreateGroup_分镜图组带上整组参数且只认白名单(t *testing.T) {
	svc, _, _, gg, scriptID := groupSetup(t)
	if _, err := svc.CreateGroup(context.Background(), scriptapp.CreateGroupInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: scriptID,
		CanvasNodeID: "node-sb-params", GroupType: domain.GroupStoryboardImage, Name: "分镜图", ModelCode: "gpt-x",
		Params: map[string]any{"aspect_ratio": "16:9", "quality": "high", "prompt": "别的词", "preset": "storyboard"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(gg.calls) == 0 {
		t.Fatal("分镜图组建组即提交，应有受理")
	}
	for _, c := range gg.calls {
		p := paramsOf(t, c.Params)
		if p["aspect_ratio"] != "16:9" || p["quality"] != "high" {
			t.Fatalf("建组时选的比例与画质要发出去，得到 %v", p)
		}
		// ⚠️ 提示词由脚本域按镜头拼：带进 prompt / preset 会把它顶掉或在后面再拼一段配方
		if _, ok := p["prompt"]; ok {
			t.Fatalf("参数里不许混进提示词: %v", p)
		}
		if _, ok := p["preset"]; ok {
			t.Fatalf("参数里不许混进预设: %v", p)
		}
	}
}

func TestCreateGroup_视频组逐镜时长进快照且计价与出片用同一个数(t *testing.T) {
	svc, r, _, gg, scriptID := groupSetup(t)
	ctx := context.Background()
	shots, err := r.ListShots(ctx, scriptID)
	if err != nil || len(shots) != 2 {
		t.Fatalf("前置：应有 2 镜，%v %d", err, len(shots))
	}
	v, err := svc.CreateGroup(ctx, scriptapp.CreateGroupInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, ScriptID: scriptID,
		CanvasNodeID: "node-video-dur", GroupType: domain.GroupVideo, Name: "视频",
		Params:    map[string]any{"aspect_ratio": "9:16", "resolution": "1080p", "audio": true},
		Durations: map[int64]int{shots[0].ID: 6},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]float64{shots[0].ID: 6, shots[1].ID: float64(shots[1].DurationSec)}
	for _, it := range v.Items {
		p := paramsOf(t, it.ParamsSnapshot)
		// 逐镜给的用逐镜的；没给的用镜头自己的时长（分镜里定好的长度），不是一律 5 秒
		if p["duration"] != want[it.ShotID] {
			t.Fatalf("镜头 %d 的快照时长应为 %v，得到 %v（%v）", it.ShotID, want[it.ShotID], p["duration"], p)
		}
		if p["aspect_ratio"] != "9:16" || p["resolution"] != "1080p" || p["audio"] != true {
			t.Fatalf("整组的比例 / 清晰度 / 音频要进快照，得到 %v", p)
		}
	}

	// 执行时给了另一个时长：每一项仍按自己快照里的时长出片，计价也按它 —— 两个数必须是同一个。
	if _, err := svc.ExecuteGroup(ctx, scriptapp.ExecuteGroupInput{
		WorkspaceID: ws, PayerAccountID: payer, UserID: user, GroupID: v.Group.ID,
		ModelCode: "gpt-x", ExpectedVersion: v.Group.Version, DurationSec: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if len(gg.calls) != 2 {
		t.Fatalf("应提交 2 项，实际 %d", len(gg.calls))
	}
	for _, c := range gg.calls {
		p := paramsOf(t, c.Params)
		if p["duration"] != float64(c.DurationSec) {
			t.Fatalf("出片时长 %v 与计价时长 %d 不一致", p["duration"], c.DurationSec)
		}
		if float64(c.DurationSec) != want[c.ShotID] {
			t.Fatalf("镜头 %d 应按自己的 %v 秒，得到 %d", c.ShotID, want[c.ShotID], c.DurationSec)
		}
	}
}

// 资产组不产生任务、子项建出来就带着资产的状态：组状态要当场算，不能停在 draft
// （画布上显示「4/4 已完成 · draft」，而它其实早就齐了）。
func TestCreateGroup_资产组建出来就按子项算状态(t *testing.T) {
	svc, r, _, _, scriptID := groupSetup(t)
	ctx := context.Background()
	assets, err := r.ListAssetsByScript(ctx, scriptID)
	if err != nil || len(assets) == 0 {
		t.Fatalf("前置：应有资产，%v %d", err, len(assets))
	}
	ready, mediaID := "ready", int64(77)
	for _, a := range assets {
		if _, err := r.UpdateAsset(ctx, repo.UpdateAssetInput{ID: a.ID, WorkspaceID: ws, Status: &ready, MediaObjectID: &mediaID}); err != nil {
			t.Fatal(err)
		}
	}
	v := createGroup(t, svc, scriptID, domain.GroupAsset, "node-asset-status")
	if v.Group.Status != string(domain.GroupSuccess) {
		t.Fatalf("资产全部就绪时资产组应是 success，得到 %s", v.Group.Status)
	}
}
