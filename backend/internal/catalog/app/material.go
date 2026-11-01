package app

import (
	"context"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ScopeMaterial 是素材目录的版本作用域。
const ScopeMaterial = "material"

type MaterialFilter struct {
	Keyword        string
	Category       string
	CommercialOnly bool
	FavoritedOnly  bool
}

func (s *Service) ListMaterials(ctx context.Context, mt domain.MaterialType,
	userID int64, f MaterialFilter, enabledOnly bool) ([]repo.MaterialRow, error) {
	if !mt.Valid() || mt == domain.MaterialVoice {
		return nil, httpx.Errf(errcode.InvalidArgument, "素材类型非法")
	}
	list, err := s.repo.ListMaterials(ctx, mt, userID, f.Keyword, f.Category,
		f.CommercialOnly, f.FavoritedOnly, enabledOnly)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

func (s *Service) SaveMaterial(ctx context.Context, operatorID int64, m domain.Material) error {
	if err := domain.ValidateMaterial(m); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	// 音色另有一张表、另有保存入口（SaveVoice），列也不一样：走这里会写坏。
	if m.Type == domain.MaterialVoice {
		return httpx.Errf(errcode.InvalidArgument, "音色请在音色管理里保存")
	}
	if err := s.repo.SaveMaterial(ctx, s.nextID(), m); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bumpScope(ctx, ScopeMaterial, operatorID, "material.save", map[string]any{
		"type": string(m.Type), "code": m.Code, "enabled": m.IsEnabled,
		"commercial": m.IsCommercial,
	})
}

func (s *Service) ToggleFavorite(ctx context.Context, userID int64,
	mt domain.MaterialType, code string, favorite bool) (bool, error) {
	if !mt.Valid() {
		return false, httpx.Errf(errcode.InvalidArgument, "素材类型非法")
	}
	got, err := s.repo.ToggleFavorite(ctx, s.nextID(), userID, mt, code, favorite)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return false, httpx.Err(errcode.NotFound)
		}
		return false, httpx.Wrap(errcode.Internal, err)
	}
	return got, nil
}

// MaterialPromptSuffix 按选中的素材算出要追加的提示词。
//
// ⚠️ 拼接在**服务端**完成（CV-15）。前端拼出来的词会落进 canvas_data，
// 用户改一个字玩法就失效，且历史节点永远停在旧词。
// MaterialPromptSuffixChecked 在拼接提示词的同时回报**查不到的素材码**。
//
// 调用方（输入解析）据此报错而不是静默忽略：选了一个已下线或拼错的码，
// 静默丢掉就是"选了等于没选"—— 界面上它是选中的，生成出来却没有那个效果。
func (s *Service) MaterialPromptSuffixChecked(ctx context.Context,
	styleCodes, effectCodes []string) (string, []string, error) {
	styles, err := s.repo.MaterialPromptsByCode(ctx, domain.MaterialStyle, styleCodes)
	if err != nil {
		return "", nil, httpx.Wrap(errcode.Internal, err)
	}
	effects, err := s.repo.MaterialPromptsByCode(ctx, domain.MaterialEffect, effectCodes)
	if err != nil {
		return "", nil, httpx.Wrap(errcode.Internal, err)
	}
	var missing []string
	var stylePrompts, effectPrompts []string
	for _, c := range styleCodes {
		p, ok := styles[c]
		if !ok {
			missing = append(missing, c)
			continue
		}
		stylePrompts = append(stylePrompts, p)
	}
	for _, c := range effectCodes {
		p, ok := effects[c]
		if !ok {
			missing = append(missing, c)
			continue
		}
		effectPrompts = append(effectPrompts, p)
	}
	return domain.MaterialPromptSuffix(stylePrompts, effectPrompts), missing, nil
}

func (s *Service) MaterialPromptSuffix(ctx context.Context, styleCodes, effectCodes []string) (string, error) {
	stylePrompts, err := s.repo.MaterialPrompts(ctx, domain.MaterialStyle, styleCodes)
	if err != nil {
		return "", httpx.Wrap(errcode.Internal, err)
	}
	effectPrompts, err := s.repo.MaterialPrompts(ctx, domain.MaterialEffect, effectCodes)
	if err != nil {
		return "", httpx.Wrap(errcode.Internal, err)
	}
	return domain.MaterialPromptSuffix(stylePrompts, effectPrompts), nil
}

type VoiceFilter struct {
	Keyword       string
	Language      string
	Gender        string
	FavoritedOnly bool
}

func (s *Service) ListVoices(ctx context.Context, userID int64, f VoiceFilter, enabledOnly bool) ([]repo.VoiceRow, error) {
	list, err := s.repo.ListVoices(ctx, userID, f.Keyword, f.Language, f.Gender, f.FavoritedOnly, enabledOnly)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

func (s *Service) SaveVoice(ctx context.Context, operatorID int64, v domain.Voice) error {
	if err := domain.ValidateVoice(v); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := s.repo.SaveVoice(ctx, s.nextID(), v); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bumpScope(ctx, ScopeMaterial, operatorID, "voice.save", map[string]any{
		"code": v.Code, "enabled": v.IsEnabled, "upstreamConfigured": v.UpstreamVoiceID != "",
	})
}

// VoicePreviewMediaID 取试听文件的媒体 ID。
func (s *Service) VoicePreviewMediaID(ctx context.Context, code string) (int64, error) {
	id, err := s.repo.GetVoicePreview(ctx, code)
	if err != nil {
		return 0, httpx.Errf(errcode.NotFound, "该音色没有可用的试听文件")
	}
	return id, nil
}
