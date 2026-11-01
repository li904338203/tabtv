package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/secret"
	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
	providertransport "github.com/li904338203/tabtv/backend/internal/provider/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ScopeModel 是模型配置的版本作用域，与画布配置分开。
const ScopeModel = "model"

func (s *Service) SetSecretCipher(c *secret.Cipher) { s.cipher = c }

// SyncProtocols 启动时把代码注册的协议同步进表（修 GAP-09）。
//
// ⚠️ 这一步让「代码里有的协议」与「管理端能选的协议」永远一致：
// 旧系统靠人同步四处，结果下拉漏了三个已实现的协议。
func (s *Service) SyncProtocols(ctx context.Context) error {
	return s.repo.UpsertProtocols(ctx, domain.Protocols())
}

func (s *Service) Protocols() []domain.Protocol { return domain.Protocols() }

// ProviderView 是对外的 Provider 视图。**绝不回显密钥**。
type ProviderView struct {
	Code             string          `json:"code"`
	Name             string          `json:"name"`
	AdapterType      string          `json:"adapterType"`
	BaseURL          string          `json:"baseUrl"`
	AuthType         string          `json:"authType"`
	HasAPIKey        bool            `json:"hasApiKey"`
	Headers          json.RawMessage `json:"headers"`
	TestPath         string          `json:"testPath,omitempty"`
	ConnectTimeoutMS int             `json:"connectTimeoutMs"`
	RequestTimeoutMS int             `json:"requestTimeoutMs"`
	IdleTimeoutMS    int             `json:"idleTimeoutMs"`
	MaxRetries       int             `json:"maxRetries"`
	MaxConcurrent    int             `json:"maxConcurrent"`
	Status           string          `json:"status"`
	IsReady          bool            `json:"isReady"`
	Version          int             `json:"version"`
}

func toProviderView(p repo.Provider) ProviderView {
	return ProviderView{
		Code: p.Code, Name: p.Name, AdapterType: p.AdapterType, BaseURL: p.BaseURL,
		AuthType: p.AuthType, HasAPIKey: p.APIKeyCipher != "", Headers: p.Headers,
		TestPath: p.TestPath, ConnectTimeoutMS: p.ConnectTimeoutMS,
		RequestTimeoutMS: p.RequestTimeoutMS, IdleTimeoutMS: p.IdleTimeoutMS,
		MaxRetries: p.MaxRetries, MaxConcurrent: p.MaxConcurrent,
		Status: p.Status, IsReady: p.IsReady(), Version: p.Version,
	}
}

func (s *Service) ListProviders(ctx context.Context) ([]ProviderView, error) {
	list, err := s.repo.ListProviders(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]ProviderView, 0, len(list))
	for _, p := range list {
		out = append(out, toProviderView(p))
	}
	return out, nil
}

// SecretField 与身份域的认证配置同构：保留 / 替换 / 清除（CD-17）。
type SecretField struct {
	Action string `json:"action"`
	Value  string `json:"value,omitempty"`
}

type ProviderInput struct {
	Code             string
	Name             string
	AdapterType      string
	BaseURL          string
	AuthType         string
	APIKey           *SecretField
	Headers          json.RawMessage
	TestPath         string
	ConnectTimeoutMS int
	RequestTimeoutMS int
	IdleTimeoutMS    int
	MaxRetries       int
	MaxConcurrent    int
	Status           string
	Version          int
}

func (s *Service) SaveProvider(ctx context.Context, operatorID int64, in ProviderInput) error {
	if in.Code == "" || in.Name == "" || in.BaseURL == "" {
		return httpx.Errf(errcode.InvalidArgument, "code、name、baseUrl 必填")
	}
	if err := domain.ValidateProviderFields(domain.ProviderFields{
		Code: in.Code, Name: in.Name, AdapterType: in.AdapterType, BaseURL: in.BaseURL,
		TestPath: in.TestPath, Headers: in.Headers,
	}); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	switch in.AuthType {
	case "none", "bearer", "header", runcfg.AuthVolcV4:
	default:
		return httpx.Errf(errcode.InvalidArgument, "authType 取值非法")
	}
	if in.AuthType == runcfg.AuthVolcV4 {
		if err := validateVolcProvider(in); err != nil {
			return err
		}
	}
	if in.Status != "enabled" && in.Status != "disabled" {
		return httpx.Errf(errcode.InvalidArgument, "status 取值非法")
	}
	// 超时三件套必须都是正数。0 会让请求永不超时，一个卡住的上游能把 Worker 拖死。
	if in.ConnectTimeoutMS <= 0 || in.RequestTimeoutMS <= 0 || in.IdleTimeoutMS <= 0 {
		return httpx.Errf(errcode.InvalidArgument, "连接/请求/空闲超时都必须大于 0")
	}
	// ⚠️ 空闲超时与整体超时是**两件事**：长文本可以合法地跑十分钟，
	// 但 60 秒不吐字就是死了。空闲超时不该大于整体超时。
	if in.IdleTimeoutMS > in.RequestTimeoutMS {
		return httpx.Errf(errcode.InvalidArgument, "空闲超时不应大于整体请求超时")
	}
	// ⚠️ 供应商级并发上限：0 表示不限。负数会让「达到上限」永远成立，
	// 于是这家 Provider 的任务全部卡在 queued 而没有任何错误信息。
	if in.MaxConcurrent < 0 {
		return httpx.Errf(errcode.InvalidArgument, "供应商并发上限不能为负（0 表示不限）")
	}

	existing := repo.Provider{}
	if in.Version > 0 {
		var err error
		existing, err = s.repo.GetProvider(ctx, in.Code)
		if err != nil {
			return httpx.Err(errcode.NotFound)
		}
		// 有启用模型时不能停用 Provider：停用会让这些模型在用户点生成时
		// 才失败，而配置页看起来一切正常（02 §3.2）。
		if in.Status == "disabled" && existing.Status == "enabled" {
			has, err := s.repo.HasEnabledModels(ctx, existing.ID)
			if err != nil {
				return httpx.Wrap(errcode.Internal, err)
			}
			if has {
				return httpx.Errf(errcode.ResourceInUse,
					"该 Provider 下仍有启用中的模型，请先停用这些模型")
			}
		}
	}

	cipherText := existing.APIKeyCipher
	if in.APIKey != nil {
		if s.cipher == nil {
			return httpx.Errf(errcode.Internal, "密钥加密未配置")
		}
		v, err := resolveSecret(s.cipher, *in.APIKey, existing.APIKeyCipher)
		if err != nil {
			return httpx.Errf(errcode.InvalidArgument, "API Key: "+err.Error())
		}
		cipherText = v
	}
	// 需要认证却没有密钥 → 直接拒绝启用，而不是等调用上游时 401。
	if in.Status == "enabled" && in.AuthType != "none" && cipherText == "" {
		return httpx.Errf(errcode.InvalidArgument, "启用前必须配置 API Key")
	}

	headers := in.Headers
	if len(headers) == 0 {
		headers = []byte("{}")
	}
	keyVersion := existing.KeyVersion
	if keyVersion == 0 {
		keyVersion = 1
	}
	if s.cipher != nil && cipherText != "" {
		keyVersion = s.cipher.CurrentVersion()
	}

	err := s.repo.SaveProvider(ctx, s.nextID(), repo.Provider{
		Code: in.Code, Name: in.Name, AdapterType: in.AdapterType, BaseURL: in.BaseURL,
		AuthType: in.AuthType, APIKeyCipher: cipherText, KeyVersion: keyVersion,
		Headers: headers, TestPath: in.TestPath,
		ConnectTimeoutMS: in.ConnectTimeoutMS, RequestTimeoutMS: in.RequestTimeoutMS,
		IdleTimeoutMS: in.IdleTimeoutMS, MaxRetries: in.MaxRetries,
		MaxConcurrent: in.MaxConcurrent,
		Status:        in.Status, Version: in.Version,
	})
	switch {
	case errors.Is(err, repo.ErrDuplicate):
		return httpx.Errf(errcode.DuplicateName, "Provider code 已存在")
	case errors.Is(err, repo.ErrVersionConflict):
		return httpx.Errf(errcode.VersionConflict, "配置已被他人修改，请重新加载")
	case err != nil:
		return httpx.Wrap(errcode.Internal, err)
	}
	// 审计不记密钥内容，只记是否变更过。
	return s.bumpScope(ctx, ScopeModel, operatorID, "provider.save", map[string]any{
		"code": in.Code, "status": in.Status, "apiKeyChanged": in.APIKey != nil,
	})
}

type ModelInput struct {
	Code              string
	Name              string
	ProviderCode      string
	Domain            string
	SubType           string
	UpstreamModelCode string
	APIProtocol       string
	SubmitPath        string
	QueryPath         string
	EstSeconds        int
	MaxInputChars     *int
	Status            string
	Sort              int
	// MinMemberLevel 会员门槛，**三态**：nil = 不改（保留库里的值）· "" = 清除（不限）· 等级 ID = 设置。
	//
	// ⚠️ 此前请求里根本没有这个字段，而保存是整条替换 —— **任何一次保存模型都会把会员门槛清掉**，
	// 高等级专属的模型从此对所有人开放，而没有任何报错。不传就保留，是为了不带这个字段的
	// 老调用方（配置脚本、联调工具）不会顺手把门槛抹掉。
	MinMemberLevel *string
	// 视频能力（AGT-05）：nil = 不改（保留库里的值，新建时取默认：未配置 / false / false）。
	// 与 MinMemberLevel 同一个理由 —— 保存是整条替换，不带这几个字段的老调用方
	// （配置脚本、联调工具）不能顺手把已配好的值抹掉。
	MaxDurationSec *int
	// ClearMaxDurationSec 显式清除时长（回到「未配置」）。*int 表达不了「传了 null」，
	// 所以清除单独一个开关；与 MaxDurationSec 同时给时以清除为准。
	ClearMaxDurationSec bool
	IsMultiShot         *bool
	HasNativeAudio      *bool
	// IsAgentCapable 可作推理模型（AGT-04）：nil = 不改（保留库里的值，新建时为 false）。
	// 同样是三态 —— 不带这个字段的老调用方保存一次就把标记清掉的话，推理模型会从
	// Agent 的模型下拉里悄悄消失。
	IsAgentCapable *bool
}

func (s *Service) SaveModel(ctx context.Context, operatorID int64, in ModelInput) error {
	if in.Code == "" || in.Name == "" || in.UpstreamModelCode == "" {
		return httpx.Errf(errcode.InvalidArgument, "code、name、upstreamModelCode 必填")
	}
	if err := domain.ValidateModelFields(domain.ModelFields{
		Code: in.Code, Name: in.Name, Domain: in.Domain, SubType: in.SubType,
		UpstreamModelCode: in.UpstreamModelCode, SubmitPath: in.SubmitPath, QueryPath: in.QueryPath,
		Status: in.Status, MaxInputChars: in.MaxInputChars, EstSeconds: in.EstSeconds,
		MaxDurationSec: in.MaxDurationSec, IsMultiShot: in.IsMultiShot, HasNativeAudio: in.HasNativeAudio,
	}); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if in.EstSeconds <= 0 {
		// est 决定超时墙 max(30, est×3)，为 0 会让所有任务立刻判超时。
		return httpx.Errf(errcode.InvalidArgument, "预计耗时必须大于 0")
	}
	if err := domain.ValidateModelProtocol(in.APIProtocol, in.SubmitPath, in.QueryPath); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	provider, err := s.repo.GetProvider(ctx, in.ProviderCode)
	if err != nil {
		return httpx.Errf(errcode.NotFound, "Provider 不存在: "+in.ProviderCode)
	}

	existing, getErr := s.repo.GetModel(ctx, in.Code)
	isNew := errors.Is(getErr, repo.ErrNotFound)
	if getErr != nil && !isNew {
		return httpx.Wrap(errcode.Internal, getErr)
	}

	if !isNew {
		// ⚠️ 有历史任务后关键字段锁定（02 §3.1）：
		// 改掉 provider/协议/domain 会让历史 generation 的快照无法被正确解释
		// ——那些任务记录的是"当时用哪个上游、什么协议"。
		hasGen, err := s.repo.HasGenerations(ctx, existing.ID)
		if err != nil {
			return httpx.Wrap(errcode.Internal, err)
		}
		if hasGen {
			var locked []string
			if existing.ProviderID != provider.ID {
				locked = append(locked, "provider")
			}
			if existing.APIProtocol != in.APIProtocol {
				locked = append(locked, "apiProtocol")
			}
			if existing.Domain != in.Domain {
				locked = append(locked, "domain")
			}
			if existing.UpstreamModelCode != in.UpstreamModelCode {
				locked = append(locked, "upstreamModelCode")
			}
			if len(locked) > 0 {
				return httpx.Errf(errcode.Conflict,
					"该模型已有历史任务，以下字段不可修改："+joinComma(locked)+
						"（改动会让历史任务的快照无法被正确解释）")
			}
		}
	}

	m := repo.Model{
		Code: in.Code, Name: in.Name, ProviderID: provider.ID, Domain: in.Domain,
		SubType: in.SubType, UpstreamModelCode: in.UpstreamModelCode,
		APIProtocol: in.APIProtocol, SubmitPath: in.SubmitPath, QueryPath: in.QueryPath,
		EstSeconds: in.EstSeconds, MaxInputChars: in.MaxInputChars,
		Status: in.Status, Sort: in.Sort,
	}
	// 会员门槛：不传保留、传空清除、传 ID 设置（见 ModelInput.MinMemberLevel）。
	if !isNew {
		m.MinMemberLevelID = existing.MinMemberLevelID
	}
	if in.MinMemberLevel != nil {
		m.MinMemberLevelID = nil
		if raw := strings.TrimSpace(*in.MinMemberLevel); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				return httpx.Errf(errcode.InvalidArgument, "会员门槛的等级 ID 非法")
			}
			ok, err := s.validateMemberLevel(ctx, &id)
			if err != nil {
				return httpx.Wrap(errcode.Internal, err)
			}
			if !ok {
				return httpx.Errf(errcode.InvalidArgument, "会员门槛指向的等级不存在")
			}
			m.MinMemberLevelID = &id
		}
	}
	applyVideoTraits(&m, in, existing, isNew)
	if !isNew {
		m.IsAgentCapable = existing.IsAgentCapable
	}
	if in.IsAgentCapable != nil {
		m.IsAgentCapable = *in.IsAgentCapable
	}
	if m.IsAgentCapable {
		// ⚠️ 按**合并后**的值校验：不传该字段、只把一个推理模型的协议改成 generic_sync，
		// 也要在这里拦下 —— 否则库里留着一个声称支持 Agent、实际第一轮就报错的模型。
		if err := domain.CheckAgentCapable(m.Domain, m.APIProtocol); err != nil {
			return httpx.Errf(errcode.CapabilityUnsupported, err.Error())
		}
	}
	if !isNew {
		// ⚠️ 必须把已有 ID 带上：发布检查要按 model_id 去查操作绑定、
		// 价格与能力矩阵。少了这一行，ID 是 0，三张表都查不到任何行，
		// 于是**任何已存在的模型都永远启用不了** —— 报的还是
		// "至少需要绑定并启用一个操作"，而操作明明已经绑好了，
		// 运营会一直去改那个根本没问题的地方。
		m.ID = existing.ID
	}
	if in.Status == "enabled" {
		// 启用前必须过发布检查，逐条给出阻塞原因（FE-07）。
		blockers, err := s.publishBlockers(ctx, m, provider)
		if err != nil {
			return err
		}
		if len(blockers) > 0 {
			return httpx.Errf(errcode.InvalidArgument,
				"无法启用："+blockers[0].Detail).WithDetails(map[string]any{"blockers": blockers})
		}
	}

	if err := s.repo.SaveModel(ctx, s.nextID(), m, isNew); err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			return httpx.Errf(errcode.DuplicateName, "模型 code 已存在")
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bumpScope(ctx, ScopeModel, operatorID, "model.save", map[string]any{
		"code": in.Code, "status": in.Status, "protocol": in.APIProtocol,
	})
}

// Publishable 返回模型的发布阻塞原因列表。空列表即可发布。
func (s *Service) Publishable(ctx context.Context, code string) ([]domain.PublishBlocker, error) {
	m, err := s.repo.GetModel(ctx, code)
	if err != nil {
		return nil, httpx.Err(errcode.NotFound)
	}
	p, err := s.repo.GetProvider(ctx, m.ProviderCode)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return s.publishBlockers(ctx, m, p)
}

func (s *Service) publishBlockers(ctx context.Context, m repo.Model, p repo.Provider) ([]domain.PublishBlocker, error) {
	ops, err := s.repo.ListModelOperations(ctx, m.ID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	var enabled []string
	for _, o := range ops {
		if o.IsEnabled {
			enabled = append(enabled, o.OperationCode)
		}
	}
	pricing, err := s.repo.OpsWithPricing(ctx, m.ID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	capsByOp, err := s.repo.ListCapabilities(ctx, m.ID, "")
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	withCaps := map[string]bool{}
	for op, list := range capsByOp {
		withCaps[op] = len(list) > 0
	}
	withCaps = domain.InheritCapabilityFlags(withCaps)

	levelOK, err := s.validateMemberLevel(ctx, m.MinMemberLevelID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}

	return domain.CheckPublishable(domain.PublishInput{
		ModelEnabled: m.Status == "enabled", ProviderReady: p.IsReady(),
		Protocol: m.APIProtocol, SubmitPath: m.SubmitPath, QueryPath: m.QueryPath,
		EnabledOps: enabled, OpsWithPricing: pricing, OpsWithCaps: withCaps,
		MemberLevelOK: levelOK,
	}), nil
}

func (s *Service) SaveModelOperation(ctx context.Context, operatorID int64, modelCode, opCode string, enabled bool, est *int) error {
	if est != nil && *est <= 0 {
		return httpx.Errf(errcode.InvalidArgument, "操作级预计耗时必须大于 0")
	}
	m, err := s.repo.GetModel(ctx, modelCode)
	if err != nil {
		return httpx.Err(errcode.NotFound)
	}
	// 启用一个协议做不了的操作直接拒；停用（enabled=false）不拦 ——
	// 要让运营能把配错的绑定关掉，而不是被自己的历史配置锁死。
	if enabled {
		if err := domain.CheckProtocolOperation(m.APIProtocol, opCode); err != nil {
			return httpx.Errf(errcode.CapabilityUnsupported, err.Error())
		}
	}
	if err := s.repo.SaveModelOperation(ctx, s.nextID(), m.ID, opCode, enabled, est); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Errf(errcode.NotFound, "操作不存在: "+opCode)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bumpScope(ctx, ScopeModel, operatorID, "model_operation.save", map[string]any{
		"model": modelCode, "operation": opCode, "enabled": enabled,
	})
}

func (s *Service) Capabilities(ctx context.Context, modelCode string) (map[string][]domain.Capability, error) {
	m, err := s.repo.GetModel(ctx, modelCode)
	if err != nil {
		return nil, httpx.Err(errcode.NotFound)
	}
	c, err := s.repo.ListCapabilities(ctx, m.ID, "")
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return c, nil
}

func (s *Service) SaveCapability(ctx context.Context, operatorID int64, modelCode, opCode string, c domain.Capability) error {
	if err := domain.ValidateCapability(c); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	m, err := s.repo.GetModel(ctx, modelCode)
	if err != nil {
		return httpx.Err(errcode.NotFound)
	}
	// ⚠️ 协议收不了的角色，**配置时就拒**（AC-10）。放进去的话，这条能力矩阵
	// 会让报价与受理都放行，直到 Worker 构造请求才发现那套接口没有这个位置 ——
	// 而那时积分已经扣了、任务已经排了队。
	if err := domain.CheckProtocolRole(m.APIProtocol, opCode, c.Role); err != nil {
		return httpx.Errf(errcode.CapabilityUnsupported, err.Error())
	}
	if err := s.repo.SaveCapability(ctx, s.nextID(), m.ID, opCode, c); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Errf(errcode.NotFound, "操作不存在: "+opCode)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bumpScope(ctx, ScopeModel, operatorID, "capability.save", map[string]any{
		"model": modelCode, "operation": opCode, "role": string(c.Role),
		"min": c.MinCount, "max": c.MaxCount,
	})
}

func (s *Service) DeleteCapability(ctx context.Context, operatorID int64, modelCode, opCode, role string) error {
	m, err := s.repo.GetModel(ctx, modelCode)
	if err != nil {
		return httpx.Err(errcode.NotFound)
	}
	if err := s.repo.DeleteCapability(ctx, m.ID, opCode, role); err != nil {
		return httpx.Err(errcode.NotFound)
	}
	return s.bumpScope(ctx, ScopeModel, operatorID, "capability.delete", map[string]any{
		"model": modelCode, "operation": opCode, "role": role,
	})
}

func (s *Service) ListModels(ctx context.Context) ([]repo.Model, error) {
	m, err := s.repo.ListModels(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return m, nil
}

// ModelBindings 全部模型的操作绑定（按模型 code 归组），后台列表与计价表单用。
func (s *Service) ModelBindings(ctx context.Context) (map[string][]repo.ModelOperation, error) {
	b, err := s.repo.AllModelOperations(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return b, nil
}

// ListOperations 操作目录（后台下拉用）。
func (s *Service) ListOperations(ctx context.Context) ([]repo.Operation, error) {
	o, err := s.repo.ListOperations(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return o, nil
}

func (s *Service) bumpScope(ctx context.Context, scope string, operatorID int64, action string, after any) error {
	if err := s.repo.BumpRevision(ctx, scope, operatorID); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if s.audit != nil {
		s.audit.Write(ctx, audit.Entry{
			ActorType: audit.ActorAdmin, ActorID: operatorID,
			Action: action, ObjectType: objectTypeOf(action), After: after,
		})
	}
	return nil
}

// objectTypeOf 从 action 取对象类型。
//
// 本包的 action 一律是 `<对象>.<动作>`（provider.save / prompt.publish …），
// 对象名就写在里面，于是不必在每个调用点再传一遍、也不会两处写得不一致。
//
// ⚠️ 此前这里硬编码成 "model_config"：提示词、素材、音色、计价的审计
// 全被记成模型配置，「这条提示词被谁改过」按对象类型根本查不出来。
func objectTypeOf(action string) string {
	if i := strings.IndexByte(action, '.'); i > 0 {
		return action[:i]
	}
	return action
}

func resolveSecret(c *secret.Cipher, f SecretField, existing string) (string, error) {
	switch f.Action {
	case "", "keep":
		return existing, nil
	case "clear":
		return "", nil
	case "replace":
		if f.Value == "" {
			return "", errors.New("replace 必须提供 value")
		}
		// 掩码回传保护：管理端显示掩码，未改动直接提交会把真实密钥覆盖掉。
		if looksMaskedValue(f.Value) {
			return "", errors.New("提交的值看起来是掩码，请重新输入真实密钥")
		}
		return c.Encrypt(f.Value)
	default:
		return "", errors.New("action 取值非法")
	}
}

func looksMaskedValue(s string) bool {
	run := 0
	for _, r := range s {
		if r == '*' {
			run++
			if run >= 4 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

func joinComma(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += "、"
		}
		out += s
	}
	return out
}

// ProbeConfig 是连接测试所需的配置（含**解密后的**密钥）。
//
// ⚠️ 这个结构只在进程内流转，**绝不下发给前端**：
// 管理端看到的密钥永远是掩码（CD-17）。
type ProbeConfig struct {
	BaseURL          string
	TestPath         string
	AuthType         string
	APIKey           string
	HeaderName       string
	SignRegion       string
	SignService      string
	ConnectTimeoutMS int
	RequestTimeoutMS int
}

// ProviderForProbe 取连接测试用的配置。
func (s *Service) ProviderForProbe(ctx context.Context, code string) (ProbeConfig, error) {
	p, err := s.repo.GetProvider(ctx, code)
	if err != nil {
		return ProbeConfig{}, httpx.Errf(errcode.NotFound, "Provider 不存在: "+code)
	}
	rh := parseRunHeaders(p.Headers)
	out := ProbeConfig{
		BaseURL: p.BaseURL, TestPath: p.TestPath, AuthType: p.AuthType,
		ConnectTimeoutMS: p.ConnectTimeoutMS, RequestTimeoutMS: p.RequestTimeoutMS,
		// ⚠️ 与 ResolveForRun 同一份解析：此前探活不读 X-Auth-Header-Name，换过头名的 Provider
		// 探活一律 401（「鉴权被拒，请检查 API Key」），而真实调用是好的。
		HeaderName: rh.headerName, SignRegion: rh.region, SignService: rh.service,
	}
	if p.APIKeyCipher != "" {
		if s.cipher == nil {
			return ProbeConfig{}, httpx.Errf(errcode.Internal, "密钥解密未配置")
		}
		key, err := s.cipher.Decrypt(p.APIKeyCipher)
		if err != nil {
			// ⚠️ 不回显底层错误：区分"key 版本不对"与"密文被篡改"对攻击者有价值。
			return ProbeConfig{}, httpx.Errf(errcode.Internal, "密钥解密失败，请重新录入")
		}
		out.APIKey = key
	}
	return out, nil
}

// ⚠️ 返回的是 `provider/runcfg.RunConfig`（中立定义），目录域由此**直接实现**
// `runcfg.Resolver` —— 两个 main 里就不用各写一份转换适配器了。
// 密钥只在进程内流转，**绝不下发给前端**（CD-17）。

// ResolveForRun 取执行一次任务所需的连接配置。
//
// ⚠️ 这里**不校验模型是否启用**：任务受理时已经查过可调用性
// （model.enabled ∧ provider.ready ∧ …），而任务可能在队列里待到
// 管理员刚好停用这个模型。那时该让它跑完，不是把一个已扣费的任务判死。
// 真正拦住新任务的是受理侧。
func (s *Service) ResolveForRun(ctx context.Context, modelCode string) (runcfg.RunConfig, error) {
	m, err := s.repo.GetModel(ctx, modelCode)
	if err != nil {
		return runcfg.RunConfig{}, httpx.Errf(errcode.NotFound, "模型不存在: "+modelCode)
	}
	p, err := s.repo.GetProviderByID(ctx, m.ProviderID)
	if err != nil {
		return runcfg.RunConfig{}, httpx.Errf(errcode.NotFound, "模型绑定的 Provider 不存在")
	}
	out := runcfg.RunConfig{
		ModelCode: m.Code, UpstreamModel: m.UpstreamModelCode, ProtocolCode: m.APIProtocol,
		ProviderCode: p.Code, BaseURL: p.BaseURL, SubmitPath: m.SubmitPath,
		QueryPath: m.QueryPath, AuthType: p.AuthType,
		ConnectTimeout: time.Duration(p.ConnectTimeoutMS) * time.Millisecond,
		RequestTimeout: time.Duration(p.RequestTimeoutMS) * time.Millisecond,
		IdleTimeout:    time.Duration(p.IdleTimeoutMS) * time.Millisecond,
		MaxRetries:     p.MaxRetries,
	}
	rh := parseRunHeaders(p.Headers)
	out.Headers, out.HeaderName, out.SignRegion, out.SignService = rh.headers, rh.headerName, rh.region, rh.service
	if p.APIKeyCipher != "" {
		if s.cipher == nil {
			return runcfg.RunConfig{}, httpx.Errf(errcode.Internal, "密钥解密未配置")
		}
		key, err := s.cipher.Decrypt(p.APIKeyCipher)
		if err != nil {
			// ⚠️ 不回显底层错误：区分"key 版本不对"与"密文被篡改"对攻击者有价值。
			return runcfg.RunConfig{}, httpx.Errf(errcode.Internal,
				"Provider "+p.Code+" 的密钥解密失败，请重新录入")
		}
		out.APIKey = key
	}
	return out, nil
}

// runHeaders 是 Provider 自定义请求头里拆出来的几样：真要发出去的头，与几个约定键。
type runHeaders struct {
	headers         map[string]string
	headerName      string
	region, service string
}

// parseRunHeaders 拆 Provider 的自定义请求头。约定键（密钥放哪个头、V4 签名的地域与服务）是配置不是请求头：
// 取出来之后从要发出的头里删掉，大小写不敏感。解不开时按没有自定义头处理（保存时已校验过形状）。
func parseRunHeaders(raw json.RawMessage) runHeaders {
	var out runHeaders
	if len(raw) == 0 {
		return out
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		return out
	}
	out.headers = map[string]string{}
	for k, v := range headers {
		switch {
		case strings.EqualFold(k, "X-Auth-Header-Name"):
			// 自定义头里可以指定密钥放哪个头（auth_type=header 时用）。
			out.headerName = v
		case strings.EqualFold(k, runcfg.VolcRegionHeader):
			out.region = strings.TrimSpace(v)
		case strings.EqualFold(k, runcfg.VolcServiceHeader):
			out.service = strings.TrimSpace(v)
		default:
			out.headers[k] = v
		}
	}
	return out
}

// validateVolcProvider 校验火山 V4 签名的配置（SCD-38）：地域与服务配在自定义请求头里、换密钥时是 `AK:SK`。
//
// ⚠️ 在保存时拦：缺地域的配置存得进去的话，要等到 Worker 签名时才报错 —— 那时任务已经扣过费了。
func validateVolcProvider(in ProviderInput) error {
	rh := parseRunHeaders(in.Headers)
	if rh.region == "" || rh.service == "" {
		return httpx.Errf(errcode.InvalidArgument, "火山 V4 签名要在自定义请求头里配置 "+
			runcfg.VolcRegionHeader+"（地域）与 "+runcfg.VolcServiceHeader+"（服务）")
	}
	// 与签名时同一个拆法（providertransport.ParseVolcKey）：存进去的格式签名时一定拆得开。
	if in.APIKey != nil && in.APIKey.Action == "replace" {
		if _, _, err := providertransport.ParseVolcKey(in.APIKey.Value); err != nil {
			return httpx.Errf(errcode.InvalidArgument, "API Key: "+err.Error())
		}
	}
	return nil
}

// applyVideoTraits 按「先拷 existing → 入参非 nil 再覆盖」合并视频能力三项（AGT-05）。
//
// 非视频模型一律落默认值：领域校验已拒绝给非视频模型设非默认值，这里处理的是
// 「把一个配过时长的视频模型改成别的类型」—— 留着旧值的话，库里会有一个声称能出 10 秒的图片模型。
func applyVideoTraits(m *repo.Model, in ModelInput, existing repo.Model, isNew bool) {
	if !isNew {
		m.MaxDurationSec = existing.MaxDurationSec
		m.IsMultiShot = existing.IsMultiShot
		m.HasNativeAudio = existing.HasNativeAudio
	}
	if in.MaxDurationSec != nil {
		v := *in.MaxDurationSec
		m.MaxDurationSec = &v
	}
	if in.ClearMaxDurationSec {
		m.MaxDurationSec = nil
	}
	if in.IsMultiShot != nil {
		m.IsMultiShot = *in.IsMultiShot
	}
	if in.HasNativeAudio != nil {
		m.HasNativeAudio = *in.HasNativeAudio
	}
	if m.Domain != "video" {
		m.MaxDurationSec, m.IsMultiShot, m.HasNativeAudio = nil, false, false
	}
}

// VideoTraits 是视频模型在目录里声明的能力（AGT-05，供 AGT-50 单段 / 多段分流与 Agent 规划）。
//
// ⚠️ 这是**声明不是事实**：渲染补静音仍以 ffprobe 探到的真实音轨为准（§5.10），
// 不要拿 HasNativeAudio 去决定要不要补静音 —— 标记配错时会音画错位而任务"成功"。
type VideoTraits struct {
	// OK 为 false 表示这不是视频模型（其余字段为零值）。
	OK bool
	// MaxDurationSec 为 nil = 后台未配置；调用方按保守策略处理，不要猜一个数。
	MaxDurationSec *int
	IsMultiShot    bool
	HasNativeAudio bool
}

// VideoTraits 只读取数：模型不存在回 NotFound，非视频模型回零值 + OK=false。
func (s *Service) VideoTraits(ctx context.Context, modelCode string) (VideoTraits, error) {
	m, err := s.repo.GetModel(ctx, modelCode)
	if errors.Is(err, repo.ErrNotFound) {
		return VideoTraits{}, httpx.Errf(errcode.NotFound, "模型不存在: "+modelCode)
	}
	if err != nil {
		return VideoTraits{}, httpx.Wrap(errcode.Internal, err)
	}
	if m.Domain != "video" {
		return VideoTraits{}, nil
	}
	return VideoTraits{OK: true, MaxDurationSec: m.MaxDurationSec,
		IsMultiShot: m.IsMultiShot, HasNativeAudio: m.HasNativeAudio}, nil
}

// OperationFit 是模型的一个可用生成方式：产出类型、各输入角色的数量范围、视频参数范围说明。
//
// Agent 据此给计划项挑生成方式、写系统提示词（04 文档：带了参考图却走文生视频、模型不知道
// 所选视频模型能做哪几种）。能力矩阵与报价用的是同一份（含继承），不另写一份判断。
type OperationFit struct {
	Code string
	// Output 操作的产出类型（operation.domain）：video / image / audio / text。
	Output string
	Caps   []domain.Capability
	// Limits 视频操作的协议参数范围说明（时长 / 清晰度 / 画幅）；其余为空。
	Limits string
}

// ModelOperationFits 列出模型已启用的生成方式（模型 × 操作绑定启用 ∧ 操作启用）。
// 模型不存在回 NotFound；某个操作没配能力矩阵时 Caps 为空（报价会拦，这里照列）。
func (s *Service) ModelOperationFits(ctx context.Context, modelCode string) ([]OperationFit, error) {
	m, err := s.repo.GetModel(ctx, modelCode)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, httpx.Errf(errcode.NotFound, "模型不存在: "+modelCode)
	}
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	bindings, err := s.repo.ListModelOperations(ctx, m.ID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	ops, err := s.repo.ListOperations(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	byCode := make(map[string]repo.Operation, len(ops))
	for _, o := range ops {
		byCode[o.Code] = o
	}
	caps, err := s.repo.ListCapabilities(ctx, m.ID, "")
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	var out []OperationFit
	for _, b := range bindings {
		o, ok := byCode[b.OperationCode]
		if !ok || !b.IsEnabled || o.Status != "enabled" {
			continue
		}
		c := caps[o.Code]
		if len(c) == 0 {
			if base, ok := domain.CapabilityInheritance[o.Code]; ok {
				c = caps[base]
			}
		}
		f := OperationFit{Code: o.Code, Output: o.Domain, Caps: c}
		if o.Domain == "video" {
			f.Limits = domain.DescribeVideoLimits(m.APIProtocol, o.Code)
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return byCode[out[i].Code].Sort < byCode[out[j].Code].Sort
	})
	return out, nil
}
