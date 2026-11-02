package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ParamSupportInput 是一行型号级参数覆盖（TT-T-18）。AllowedValues / Override 为空表示该列置空。
type ParamSupportInput struct {
	ParamCode     string
	AllowedValues json.RawMessage
	DefaultValue  string
	Override      json.RawMessage
}

// ValidateParamSupport 在写库之前把配错的覆盖拦下来，并说清是哪一项。
//
// ⚠️ 与读取用同一个折叠函数（domain.ModelParamOverride）：写得进去的就一定读得出来 ——
// 读取那边遇到解析不了的行只告警跳过，于是一行配坏的覆盖在界面上表现为「改了没生效」。
// ⚠️ 默认值必须在可选项里：读取时会把不在可选项里的默认值悄悄丢掉，那样「保存成功了，默认值却不对」。
func ValidateParamSupport(in ParamSupportInput) error {
	if strings.TrimSpace(in.ParamCode) == "" {
		return errors.New("参数必选")
	}
	allowed := normJSON(in.AllowedValues)
	override := normJSON(in.Override)
	if allowed != nil {
		var values []any
		if err := json.Unmarshal(allowed, &values); err != nil {
			return errors.New("可选值必须是 JSON 数组")
		}
		for _, v := range values {
			switch v.(type) {
			case string, float64, bool:
			default:
				return errors.New("可选值只能是字符串、数字或布尔")
			}
		}
	}
	if override != nil {
		var m map[string]any
		if err := json.Unmarshal(override, &m); err != nil || m == nil {
			return errors.New("控件覆盖必须是 JSON 对象")
		}
	}
	if allowed == nil && override == nil && in.DefaultValue == "" {
		return errors.New("可选值、默认值、控件覆盖至少填一项（全空等于没有覆盖，请直接删除这一行）")
	}
	cfg, err := domain.ModelParamOverride(override, allowed, in.DefaultValue)
	if err != nil {
		return err
	}
	if in.DefaultValue != "" {
		if _, kept := cfg["default"]; !kept {
			return fmt.Errorf("默认值 %q 不在可选项里", in.DefaultValue)
		}
	}
	return nil
}

func normJSON(b json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	return json.RawMessage(s)
}

// ParamSupports 返回一个模型的覆盖行与可选的参数定义。
func (s *Service) ParamSupports(ctx context.Context, modelCode string) ([]repo.AdminParamSupport, []repo.ParamDefBrief, error) {
	rows, err := s.repo.ListParamSupportsAdmin(ctx, modelCode)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, nil, httpx.Errf(errcode.NotFound, "模型不存在: "+modelCode)
	}
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	defs, err := s.repo.ListParamDefsBrief(ctx)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	return rows, defs, nil
}

// SaveParamSupport 写一行覆盖并推进模型配置版本。
func (s *Service) SaveParamSupport(ctx context.Context, operatorID int64, modelCode string, in ParamSupportInput) error {
	if err := ValidateParamSupport(in); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	err := s.repo.UpsertParamSupport(ctx, s.nextID(), modelCode, in.ParamCode,
		normJSON(in.AllowedValues), normJSON(in.Override), in.DefaultValue)
	if errors.Is(err, repo.ErrNotFound) {
		return httpx.Errf(errcode.NotFound, "模型或参数不存在")
	}
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bumpScope(ctx, ScopeModel, operatorID, "model_param.save", map[string]any{
		"model": modelCode, "param": in.ParamCode,
	})
}

// DeleteParamSupport 删掉一行覆盖。
func (s *Service) DeleteParamSupport(ctx context.Context, operatorID int64, modelCode, paramCode string) error {
	err := s.repo.DeleteParamSupport(ctx, modelCode, paramCode)
	if errors.Is(err, repo.ErrNotFound) {
		return httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bumpScope(ctx, ScopeModel, operatorID, "model_param.delete", map[string]any{
		"model": modelCode, "param": paramCode,
	})
}
