package wiring

import (
	"context"
	"errors"
	"log/slog"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	mktdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
)

// 营销商品库 / 角色库的端口适配（MKT-T-33；dev/05 §4.1）。
//
// marketing 不 import 媒体域：图片的归属与格式、删除商品后清理媒体都经这里接到 media/app。

// MarketingWorkspaceMedia 把媒体域接成 marketing/app.WorkspaceMediaLookup 与 MediaDeleter。
//
// ⚠️ 删除走 media/app.Service.Delete（会跑全部引用检查器）——**不要**在这里直接改
// media_object.status，那会绕过会话、时间线等的引用保护。
type MarketingWorkspaceMedia struct{ Media *mediaapp.Service }

// LookupWorkspaceMedia 实现 WorkspaceMediaLookup。
//
// Info 的口径是「本工作区 ∨ 平台素材（workspace_id=0）且未删除」；平台素材由调用方按 WorkspaceID 再拒。
// Info 对「不存在 / 不是你的 / 已删除」统一回 NotFound，这里一律翻成 found=false。
func (m MarketingWorkspaceMedia) LookupWorkspaceMedia(ctx context.Context, workspaceID, mediaID int64) (
	marketingapp.MediaFacts, bool, error) {
	o, err := m.Media.Info(ctx, workspaceID, mediaID)
	if err != nil {
		return marketingapp.MediaFacts{}, false, nil
	}
	return marketingapp.MediaFacts{ID: o.ID, WorkspaceID: o.WorkspaceID, Purpose: o.Purpose,
		ContentType: o.ContentType}, true, nil
}

// DeleteMedia 实现 MediaDeleter。
func (m MarketingWorkspaceMedia) DeleteMedia(ctx context.Context, workspaceID, mediaID int64) error {
	return m.Media.Delete(ctx, workspaceID, mediaID)
}

// CopyToWorkspace 实现 MediaCopier（营销角色 adopt，MKT-T-34）：真复制字节、按来源去重。
//
// ⚠️ 媒体域的 CopyToWorkspace 不校验源归属；营销域只在判完「预置角色启用」之后、对那个预置角色
// 自己的媒体调用它（dev/05 MKD-30）。
func (m MarketingWorkspaceMedia) CopyToWorkspace(ctx context.Context, mediaID, targetWorkspaceID,
	actorUserID int64) (int64, error) {
	return m.Media.CopyToWorkspace(ctx, mediaID, targetWorkspaceID, actorUserID)
}

// MarketingLibraryInput 是商品库服务的依赖。
type MarketingLibraryInput struct {
	Repo     *marketingrepo.Repo
	Media    *mediaapp.Service
	Settings marketingapp.LibrarySettings
	NextID   func() int64
	Logger   *slog.Logger
}

// NewMarketingLibrary 装配商品库与角色库服务（MKT-T-33 / MKT-T-34）。
func NewMarketingLibrary(in MarketingLibraryInput) *marketingapp.Library {
	media := MarketingWorkspaceMedia{Media: in.Media}
	lib := marketingapp.NewLibrary(in.Repo, media, media, in.Settings, in.NextID)
	lib.SetLogger(in.Logger)
	// 角色库与营销角色（MKT-T-34）：工作区角色与预置角色是两张表，同一个仓储实现两个端口。
	lib.SetCharacters(in.Repo, in.Repo, media)
	return lib
}

// AgentSceneLibrary 把营销库接成 agentapp.SceneLibrary（MKT-T-37；dev/05 MKD-24、25）：
// 建营销会话时取商品 / 角色快照。agent 不 import 营销域，营销域也不认识 agent 的类型。
type AgentSceneLibrary struct{ Lib *marketingapp.Library }

// Snapshot 实现 SceneLibrary。取不到的那一项翻成 *agentapp.SceneLibraryMissing（点名槽位）。
func (a AgentSceneLibrary) Snapshot(ctx context.Context, ws, productID int64, characterIDs []int64) (
	*agentapp.SceneProductSnap, []agentapp.SceneCharacterSnap, error) {
	snap, err := a.Lib.SnapshotForSession(ctx, ws, productID, characterIDs)
	var miss *marketingapp.SnapshotMissing
	if errors.As(err, &miss) {
		slot := map[string]string{"product": "product", "character": "character"}[miss.Kind]
		return nil, nil, &agentapp.SceneLibraryMissing{Slot: slot, ID: miss.ID}
	}
	if err != nil {
		return nil, nil, err
	}
	var prod *agentapp.SceneProductSnap
	if p := snap.Product; p != nil {
		prod = &agentapp.SceneProductSnap{ID: p.ID, Version: p.Version, Name: p.Name,
			SellingPoints: p.SellingPoints, Description: p.Description,
			ImageMediaIDs: append([]int64(nil), p.ImageMediaIDs...)}
	}
	chars := make([]agentapp.SceneCharacterSnap, 0, len(snap.Characters))
	for _, c := range snap.Characters {
		chars = append(chars, agentapp.SceneCharacterSnap{ID: c.ID, Name: c.Name,
			MainMediaID: mktdomain.MainImage(c.ImageMediaIDs), Appearance: c.Appearance,
			VoiceText: c.VoiceText, VoiceMediaID: c.VoiceMediaID})
	}
	return prod, chars, nil
}

var _ agentapp.SceneLibrary = AgentSceneLibrary{}
