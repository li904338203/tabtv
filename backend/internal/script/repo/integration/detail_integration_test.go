//go:build integration

// 脚本详情给出的提示词状态与资产引用（CD-77）。
//
// ⚠️ 起因：脚本工作台与画布节点只读详情，而详情此前回的是库里那一列「改动时的即时标记」——
// 「改了又改回原样」之后它还停在 stale，按指纹算却已经是 ready；`GET /prompts` 说 ready、详情说 stale，
// 用户会为一段没过期的词再花一次钱。引用关系此前详情里根本没有，前端只能照名字去猜锚定顺序。
package integration

import (
	"context"
	"testing"

	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/script/domain"
)

func TestDetail_提示词状态与提示词列表是同一个答案(t *testing.T) {
	svc, r, _, scriptID, shotIDs := prepared(t)
	compose(t, svc, scriptID, scriptapp.ComposeInput{Source: domain.SourcePromptTemplate})
	ctx := context.Background()

	// 改了又改回原样：库里那一列停在 stale（即时标记），按指纹算已经是 ready。
	shot := shotOf(t, r, scriptID, shotIDs[0])
	original, changed := shot.Description, "临时改一下"
	saved, err := svc.SaveShot(ctx, scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shotIDs[0],
		ExpectedVersion: shot.Version, Description: &changed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveShot(ctx, scriptapp.SaveShotInput{
		WorkspaceID: ws, ScriptID: scriptID, ShotID: shotIDs[0],
		ExpectedVersion: saved.Version, Description: &original,
	}); err != nil {
		t.Fatal(err)
	}
	if stored := shotOf(t, r, scriptID, shotIDs[0]); stored.ImagePromptStatus != string(domain.PromptStale) {
		t.Fatalf("前置：库里那一列应还停在 stale，得到 %q", stored.ImagePromptStatus)
	}

	d, err := svc.Get(ctx, ws, scriptID)
	if err != nil {
		t.Fatal(err)
	}
	views := prompts(t, svc, scriptID)
	for _, sh := range d.Shots {
		img, vid := views[key(sh.ShotNo, domain.PromptImage)], views[key(sh.ShotNo, domain.PromptVideo)]
		if sh.ImagePromptStatus != string(img.Status) || sh.VideoPromptStatus != string(vid.Status) {
			t.Fatalf("第 %d 镜：详情 %s/%s，提示词列表 %s/%s —— 两处必须同一个答案",
				sh.ShotNo, sh.ImagePromptStatus, sh.VideoPromptStatus, img.Status, vid.Status)
		}
	}
	if d.Shots[0].ImagePromptStatus != string(domain.PromptReady) {
		t.Fatalf("改回原样的那一镜，详情里应是 ready（此前回库里那一列 stale），得到 %q", d.Shots[0].ImagePromptStatus)
	}
}

// 第 1 镜先出现孙悟空、第 2 镜先出现唐僧：资产按出现顺序建，第 2 镜的锚定顺序与资产 ID 顺序相反 ——
// 按 ID 或按名字排的实现在这里都会错。
const twoRolesReply = `{"globalStyle":"水墨","shots":[
 {"description":"孙悟空与唐僧在山路上","durationSec":5,
  "entities":[{"name":"孙悟空","type":"character"},{"name":"唐僧","type":"character"}]},
 {"description":"唐僧回头看孙悟空","durationSec":5,
  "entities":[{"name":"唐僧","type":"character"},{"name":"孙悟空","type":"character"}]},
 {"description":"空山","durationSec":3}],
 "assets":[{"name":"孙悟空","type":"character","appearance":"猴脸"},{"name":"唐僧","type":"character","appearance":"僧袍"}]}`

func TestDetail_给出每一镜的资产引用且按锚定顺序(t *testing.T) {
	svc, _, bill, _ := setup(t, &fakeModel{reply: twoRolesReply}, 6)
	fund(t, bill, 100)
	sc := newScript(t, svc, "三打白骨精")
	if _, err := storyboard(svc, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.ReconcileAssets(ctx, ws, sc.Script.ID); err != nil {
		t.Fatal(err)
	}
	assets, _, err := svc.Assets(ctx, ws, sc.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]int64{}
	for _, a := range assets {
		byName[a.Name] = a.ID
	}

	d, err := svc.Get(ctx, ws, sc.Script.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]int64{
		{byName["孙悟空"], byName["唐僧"]},
		{byName["唐僧"], byName["孙悟空"]},
		{},
	}
	for i, sh := range d.Shots {
		got, ok := d.AssetIDs[sh.ID]
		if !ok {
			t.Fatalf("第 %d 镜在详情里没有引用列表（没有引用也要给空列表）", sh.ShotNo)
		}
		if len(got) != len(want[i]) {
			t.Fatalf("第 %d 镜引用 %v，应为 %v", sh.ShotNo, got, want[i])
		}
		for j := range got {
			if got[j] != want[i][j] {
				t.Fatalf("第 %d 镜引用顺序 %v，应为 %v（锚定顺序按镜头里的出现顺序）", sh.ShotNo, got, want[i])
			}
		}
	}
}
