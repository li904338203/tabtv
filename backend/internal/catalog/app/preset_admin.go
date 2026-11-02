package app

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 图片预设的写接口（TT-T-20，TT-F-31）。来源系统的预设只能由 seed 写、后台只读；
// tabtv 的使用者要能自己调配方、加玩法、停用不用的。
//
// ⚠️ 停用不删行：历史节点 params 里存着 presetCode，删了它们的配方就查不到了。
// ⚠️ 改完 bump 画布配置版本：前端按 revision 缓存配置，不 bump 的话选择器里看不到新预设。

// BuiltinPresets 提供内置预设内容（由组装层用 seed 包实现，目录域不依赖 seed）。
type BuiltinPresets interface {
	BuiltinPreset(code string) (repo.AdminImagePreset, string, bool)
}

// SetBuiltinPresets 接上内置预设来源（恢复内置版本用）。
func (s *Service) SetBuiltinPresets(b BuiltinPresets) { s.builtinPresets = b }

var presetCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

var (
	presetGroups = map[string]bool{"sheet": true, "tuning": true, "storyboard": true, "space": true, "tool": true}
	presetSpecs  = map[string]bool{"sheet": true, "sequence": true, "variation": true, "retouch": true, "panorama": true}
	presetInputs = map[string]bool{"image_required": true, "text_required": true, "either": true}
)

// validatePreset 与表上的 CHECK 约束同一套取值，在写库之前说清哪一项不对（约束报错只有一串英文名）。
func validatePreset(p repo.AdminImagePreset) error {
	switch {
	case !presetCodeRe.MatchString(p.Code):
		return errors.New("编码只能是小写字母开头的小写字母、数字、下划线，2~64 个字符")
	case strings.TrimSpace(p.Name) == "" || len([]rune(p.Name)) > 64:
		return errors.New("名称必填，最多 64 个字")
	case !presetGroups[p.PresetGroup]:
		return errors.New("分组只能是 sheet / tuning / storyboard / space / tool")
	case !presetSpecs[p.Spec]:
		return errors.New("产出类型只能是 sheet / sequence / variation / retouch / panorama")
	case !presetInputs[p.InputMode]:
		return errors.New("输入要求只能是 image_required / text_required / either")
	case strings.TrimSpace(p.Prompt) == "":
		return errors.New("提示词正文必填：预设就是一段配方，没有正文等于选了没效果")
	case len([]rune(p.Description)) > 300:
		return errors.New("占位说明最多 300 个字")
	}
	if len(p.Params) > 0 && string(p.Params) != "null" {
		var m map[string]any
		if err := json.Unmarshal(p.Params, &m); err != nil {
			return errors.New("参数默认值必须是 JSON 对象")
		}
	}
	if len(p.Dimensions) > 0 && string(p.Dimensions) != "null" {
		var raw []json.RawMessage
		if err := json.Unmarshal(p.Dimensions, &raw); err != nil {
			return errors.New("调节维度必须是 JSON 数组")
		}
		dims := domain.ParseDimensions(p.Dimensions)
		if len(dims) != len(raw) {
			return errors.New("调节维度格式不对")
		}
		for _, d := range dims {
			if d.Code == "" || len(d.Options) == 0 {
				return errors.New("每个调节维度都要有编码和至少一个选项")
			}
		}
	}
	return nil
}

// AdminListPresets 列出全部预设（含已停用）。
func (s *Service) AdminListPresets(ctx context.Context) ([]repo.AdminImagePreset, error) {
	list, err := s.repo.AdminListImagePresets(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	if list == nil {
		list = []repo.AdminImagePreset{}
	}
	return list, nil
}

// SavePreset 新建或更新。
func (s *Service) SavePreset(ctx context.Context, operatorID int64, p repo.AdminImagePreset, create bool) error {
	if err := validatePreset(p); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	// 操作码必须是目录里有的：写错的话预设能存进去，而工具栏按它报价时一律「没有这个操作」。
	if op := strings.TrimSpace(p.Operation); op != "" {
		ops, err := s.repo.ListOperations(ctx)
		if err != nil {
			return httpx.Wrap(errcode.Internal, err)
		}
		known := false
		for _, o := range ops {
			if o.Code == op {
				known = true
				break
			}
		}
		if !known {
			return httpx.Errf(errcode.InvalidArgument, "操作「"+op+"」不存在")
		}
	}
	err := s.repo.SaveImagePreset(ctx, s.nextID(), p, create)
	switch {
	case errors.Is(err, repo.ErrDuplicate):
		return httpx.Errf(errcode.DuplicateName, "预设编码已存在")
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Err(errcode.NotFound)
	case err != nil:
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bump(ctx, operatorID, "image_preset.save", map[string]any{"code": p.Code, "create": create})
}

// SetPresetEnabled 启用 / 停用。
func (s *Service) SetPresetEnabled(ctx context.Context, operatorID int64, code string, enabled bool) error {
	if err := s.repo.SetImagePresetEnabled(ctx, code, enabled); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bump(ctx, operatorID, "image_preset.enable", map[string]any{"code": code, "enabled": enabled})
}

// RestorePreset 恢复内置版本：用内置内容覆盖并清掉「已修改」标记。不是内置的预设回 404。
func (s *Service) RestorePreset(ctx context.Context, operatorID int64, code string) error {
	if s.builtinPresets == nil {
		return httpx.Errf(errcode.NotFound, "没有内置版本")
	}
	p, hash, ok := s.builtinPresets.BuiltinPreset(code)
	if !ok {
		return httpx.Errf(errcode.NotFound, "这个预设不是内置的，没有可恢复的版本")
	}
	if err := s.repo.RestoreImagePreset(ctx, p, hash); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bump(ctx, operatorID, "image_preset.restore", map[string]any{"code": code})
}

// AdminGetPreset 取一条（含提示词）。
func (s *Service) AdminGetPreset(ctx context.Context, code string) (repo.AdminImagePreset, error) {
	p, err := s.repo.GetAdminImagePreset(ctx, code)
	if errors.Is(err, repo.ErrNotFound) {
		return p, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return p, httpx.Wrap(errcode.Internal, err)
	}
	return p, nil
}

// PreviewPreset 按服务端同一份规则拼出配方（设置页的预览，TT-T-20）：
// 没选的维度按 default 出词 —— 这条规则只在服务端，前端复刻一份迟早会与实际出图对不上。
func (s *Service) PreviewPreset(prompt string, dims json.RawMessage, selected map[string]string) string {
	return domain.ComposePreset(domain.Preset{Prompt: prompt, Dimensions: domain.ParseDimensions(dims)}, selected)
}
