package app

import (
	"context"
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 管理端：渲染水印配置与成片追溯（P10-04）。
//
// ⚠️ **水印图是配置不是代码**：换 logo、调位置、调透明度都不该发版
// （与图片预设、提示词模板同一条）。

// MediaExistence 由媒体域实现：**平台级**素材的存在性校验。
//
// ⚠️ 与 `MediaSource.Lookup` 分开是必须的：水印图是平台配置、
// 不属于任何工作区，按工作区解析一定查不到 —— 混用的话
// 运营无论传什么 ID 都会被判成"图不存在"，而后台看不出为什么。
type MediaExistence interface {
	Exists(ctx context.Context, mediaID int64) error
}

func (s *Service) SetMediaExistence(m MediaExistence) { s.mediaExists = m }

// RenderSettingView 下发给管理端的配置。
//
// ⚠️ **不带预览 URL**：水印图走的是受保护的媒体下载端点，
// 管理端按 `watermarkMediaId` 用 fetch+blob 自己取（与全站一致，
// 裸 `<img src>` 不带 Authorization 头，一律 401）。
type RenderSettingView struct {
	repo.RenderSetting
	// WatermarkMissing 配了 ID 但图已经不在了。
	//
	// ⚠️ 要**显式告诉运营**：不说的话后台显示"已配置"，
	// 而每一次渲染都在悄悄少一个水印。
	WatermarkMissing bool
}

func (s *Service) RenderSetting(ctx context.Context) (RenderSettingView, error) {
	set, err := s.repo.RenderSetting(ctx)
	if err != nil {
		return RenderSettingView{}, httpx.Wrap(errcode.Internal, err)
	}
	out := RenderSettingView{RenderSetting: set}
	if set.WatermarkMediaID != 0 && s.mediaExists != nil {
		// 图被删掉时**不报错**：配置还在，只是标一下"图没了"。
		// 报错的话运营连这条配置都改不掉（表单拉不出来）。
		out.WatermarkMissing = s.mediaExists.Exists(ctx, set.WatermarkMediaID) != nil
	}
	return out, nil
}

// SaveRenderSetting 保存配置。
//
// ⚠️ 校验在这里做一遍**不是**因为库里没有 CHECK，而是因为
// 约束冲突给用户的是一句 23514，说不出哪个字段越界。
func (s *Service) SaveRenderSetting(ctx context.Context, adminID int64,
	in repo.RenderSetting) error {
	if in.Position == "" {
		in.Position = domain.WatermarkBottomRight
	}
	if !domain.ValidWatermarkPosition(in.Position) {
		return httpx.Errf(errcode.InvalidArgument, "水印位置只能是 top_left/top_right/bottom_left/bottom_right")
	}
	if in.Opacity < 1 || in.Opacity > 100 {
		return httpx.Errf(errcode.InvalidArgument,
			fmt.Sprintf("水印不透明度须在 1~100 之间（当前 %d）", in.Opacity))
	}
	// 上限 50%：再大就不是标识而是遮挡了。
	if in.Scale < 1 || in.Scale > 50 {
		return httpx.Errf(errcode.InvalidArgument,
			fmt.Sprintf("水印宽度占比须在 1~50 之间（当前 %d）", in.Scale))
	}
	// ⚠️ 配了 ID 就要**验图存在**：配一个不存在的 ID，后果是
	// 此后每一次渲染都少一个水印而后台显示"已配置"。
	if in.WatermarkMediaID != 0 && s.mediaExists != nil {
		if err := s.mediaExists.Exists(ctx, in.WatermarkMediaID); err != nil {
			return httpx.Errf(errcode.NotFound, "水印图不存在或已被删除")
		}
	}
	if err := s.repo.SaveRenderSetting(ctx, in, adminID); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

// TraceRender 后台追溯一次渲染：任务 + 快照 + 当时用到的素材 + 成片作品。
//
// ⚠️ 给的是**快照**里的东西，不是"现在时间线上有什么"：
// 排查「这个片子怎么不对」时，只看当前时间线会得出完全错误的结论
// —— 用户很可能已经改过了。两份都给，差别才看得出来。
func (s *Service) TraceRender(ctx context.Context, id int64) (repo.RenderTrace, error) {
	tr, err := s.repo.TraceRender(ctx, id)
	if err != nil {
		return repo.RenderTrace{}, translate(err)
	}
	return tr, nil
}
