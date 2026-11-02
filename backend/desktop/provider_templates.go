package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	seeddata "github.com/li904338203/tabtv/backend/internal/platform/seed/data"
)

// 厂商模板（TT-T-23）。
//
// 来源系统的模型目录由运营脚本（configure-models / enable-models）在每个环境里配一遍，地址是中转。
// 桌面版没有运营，用户装上就该在设置页看到「OpenAI / 可灵 / 火山方舟 …」，填一个 Key 就能启用。
// 所以首次启动把 providers.json 里的厂商、模型、操作绑定、能力矩阵、参数支持写进库：
//   - 只放 docs/research/厂商官方端点核实.md §9 判定「可进」的协议 × 操作，地址一律官方；
//   - Key 永远为空、厂商与模型全部停用 —— 启用要用户自己填 Key，启用时照常过发布检查；
//   - 走进程内的目录服务（与设置页同一套校验），不直接写表；参数支持没有服务方法，直接 SQL。
//
// 规则：按 code **不存在才建**，已存在的一律不改（用户可能已填 Key、改过超时或停用了某个操作）。
// 新版本新增的厂商、模型、绑定会被补上；能力矩阵只跟着**本次新建的绑定**写，
// 参数支持只跟着**本次新建的模型**写 —— 用户删掉的能力行 / 参数覆盖不会在下次启动时复活。
//
// 必须在 freePricing 之后（发布检查要价格）、installBuiltinContent 之前（营销推荐模型按 code 引用这里的模型）。

type tplFile struct {
	CapabilityRules     map[string][]tplCap `json:"capabilityRules"`
	ProtocolSubmitPaths map[string]string   `json:"protocolSubmitPaths"`
	OperationGroups     map[string][]string `json:"operationGroups"`
	Providers           []tplProvider       `json:"providers"`
	Models              []tplModel          `json:"models"`
}

type tplCap struct {
	Role string `json:"role"`
	Min  int    `json:"min"`
	Max  int    `json:"max"`
}

type tplProvider struct {
	Code             string `json:"code"`
	Name             string `json:"name"`
	BaseURL          string `json:"baseUrl"`
	AuthType         string `json:"authType"`
	HeaderName       string `json:"headerName"`
	TestPath         string `json:"testPath"`
	ConnectTimeoutMS int    `json:"connectTimeoutMs"`
	RequestTimeoutMS int    `json:"requestTimeoutMs"`
	IdleTimeoutMS    int    `json:"idleTimeoutMs"`
	MaxRetries       int    `json:"maxRetries"`
}

type tplModel struct {
	Code           string     `json:"code"`
	Name           string     `json:"name"`
	Provider       string     `json:"provider"`
	Domain         string     `json:"domain"`
	Upstream       string     `json:"upstream"`
	Protocol       string     `json:"protocol"`
	SubmitPath     string     `json:"submitPath"`
	QueryPath      string     `json:"queryPath"`
	EstSeconds     int        `json:"estSeconds"`
	Sort           int        `json:"sort"`
	MaxDurationSec *int       `json:"maxDurationSec"`
	NativeAudio    bool       `json:"nativeAudio"`
	AgentCapable   bool       `json:"agentCapable"`
	OpsGroup       string     `json:"opsGroup"`
	Operations     []string   `json:"operations"`
	Params         []tplParam `json:"params"`
}

type tplParam struct {
	Code    string   `json:"code"`
	Allowed []string `json:"allowed"`
	Default string   `json:"default"`
}

// ProviderTemplateResult 是一次安装的统计（启动日志与测试用）。
type ProviderTemplateResult struct {
	Providers, Models, Bindings, Capabilities, Params int
}

func loadProviderTemplates() (tplFile, error) {
	var f tplFile
	raw, err := seeddata.FS.ReadFile("providers.json")
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("providers.json 解析失败：%w", err)
	}
	return f, nil
}

// operations 返回模型绑定的操作：显式列表优先，否则取操作组。
func (f tplFile) operations(m tplModel) []string {
	if len(m.Operations) > 0 {
		return m.Operations
	}
	return f.OperationGroups[m.OpsGroup]
}

func (f tplFile) submitPath(m tplModel) string {
	if m.SubmitPath != "" {
		return m.SubmitPath
	}
	return f.ProtocolSubmitPaths[m.Protocol]
}

func installProviderTemplates(ctx context.Context, pool *pgxpool.Pool, nextID func() int64, operatorID int64, log *slog.Logger) (ProviderTemplateResult, error) {
	var res ProviderTemplateResult
	f, err := loadProviderTemplates()
	if err != nil {
		return res, err
	}
	r := catalogrepo.New(pool)
	svc := catalogapp.New(r, nextID)
	// 协议表有外键约束：seed 只写了一部分协议，模型保存前先把代码注册的全部同步进去。
	if err := svc.SyncProtocols(ctx); err != nil {
		return res, fmt.Errorf("同步协议失败：%w", err)
	}

	for _, p := range f.Providers {
		if _, err := r.GetProvider(ctx, p.Code); err == nil {
			continue
		} else if !errors.Is(err, catalogrepo.ErrNotFound) {
			return res, err
		}
		headers := map[string]string{}
		if p.HeaderName != "" {
			headers["X-Auth-Header-Name"] = p.HeaderName
		}
		hb, _ := json.Marshal(headers)
		if err := svc.SaveProvider(ctx, operatorID, catalogapp.ProviderInput{
			Code: p.Code, Name: p.Name, AdapterType: "configurable_http", BaseURL: p.BaseURL,
			AuthType: p.AuthType, Headers: hb, TestPath: p.TestPath,
			ConnectTimeoutMS: p.ConnectTimeoutMS, RequestTimeoutMS: p.RequestTimeoutMS,
			IdleTimeoutMS: p.IdleTimeoutMS, MaxRetries: p.MaxRetries, Status: "disabled",
		}); err != nil {
			return res, fmt.Errorf("厂商 %s：%w", p.Code, err)
		}
		res.Providers++
	}

	for _, m := range f.Models {
		created := false
		if _, err := r.GetModel(ctx, m.Code); errors.Is(err, catalogrepo.ErrNotFound) {
			agent, audio, multi := m.AgentCapable, m.NativeAudio, false
			if err := svc.SaveModel(ctx, operatorID, catalogapp.ModelInput{
				Code: m.Code, Name: m.Name, ProviderCode: m.Provider, Domain: m.Domain,
				UpstreamModelCode: m.Upstream, APIProtocol: m.Protocol,
				SubmitPath: f.submitPath(m), QueryPath: m.QueryPath,
				EstSeconds: m.EstSeconds, Status: "disabled", Sort: m.Sort,
				MaxDurationSec: m.MaxDurationSec, IsMultiShot: &multi, HasNativeAudio: &audio,
				IsAgentCapable: &agent,
			}); err != nil {
				return res, fmt.Errorf("模型 %s：%w", m.Code, err)
			}
			created = true
			res.Models++
		} else if err != nil {
			return res, err
		}
		model, err := r.GetModel(ctx, m.Code)
		if err != nil {
			return res, err
		}
		bound, err := r.ListModelOperations(ctx, model.ID)
		if err != nil {
			return res, err
		}
		has := map[string]bool{}
		for _, b := range bound {
			has[b.OperationCode] = true
		}
		for _, op := range f.operations(m) {
			if has[op] {
				continue
			}
			if err := svc.SaveModelOperation(ctx, operatorID, m.Code, op, true, nil); err != nil {
				return res, fmt.Errorf("模型 %s 绑定 %s：%w", m.Code, op, err)
			}
			res.Bindings++
			for _, c := range f.CapabilityRules[op] {
				if err := svc.SaveCapability(ctx, operatorID, m.Code, op, catalogdomain.Capability{
					Role: catalogdomain.InputRole(c.Role), MinCount: c.Min, MaxCount: c.Max,
				}); err != nil {
					return res, fmt.Errorf("模型 %s 能力 %s/%s：%w", m.Code, op, c.Role, err)
				}
				res.Capabilities++
			}
		}
		if created {
			n, err := saveParamSupports(ctx, pool, nextID, model.ID, m)
			if err != nil {
				return res, fmt.Errorf("模型 %s 参数支持：%w", m.Code, err)
			}
			res.Params += n
		}
	}
	if res.Providers+res.Models+res.Bindings > 0 {
		log.Info("厂商模板已写入", "providers", res.Providers, "models", res.Models,
			"bindings", res.Bindings, "capabilities", res.Capabilities, "params", res.Params)
	}
	return res, nil
}

// saveParamSupports 写型号级参数覆盖：选项带上与 param_def 同款的文案（「5 秒」「1080P」），
// 只给值的话前端显示的是裸值，与没选模型时的控件不一致。
func saveParamSupports(ctx context.Context, pool *pgxpool.Pool, nextID func() int64, modelID int64, m tplModel) (int, error) {
	n := 0
	for _, p := range m.Params {
		allowed, _ := json.Marshal(p.Allowed)
		options := make([]map[string]string, 0, len(p.Allowed))
		for _, v := range p.Allowed {
			options = append(options, map[string]string{"value": v, "label": paramLabel(p.Code, v)})
		}
		override, _ := json.Marshal(map[string]any{"options": options})
		tag, err := pool.Exec(ctx, `
			insert into model_param_support (id, model_id, param_def_id, allowed_values, default_value, override)
			select $1, $2, pd.id, $4::jsonb, nullif($5, ''), $6::jsonb from param_def pd where pd.code = $3
			on conflict (model_id, param_def_id) do nothing`,
			nextID(), modelID, p.Code, string(allowed), p.Default, string(override))
		if err != nil {
			return n, err
		}
		if tag.RowsAffected() == 0 {
			return n, fmt.Errorf("参数定义 %s 不存在或已有覆盖", p.Code)
		}
		n++
	}
	return n, nil
}

func paramLabel(code, v string) string {
	switch code {
	case "duration":
		return v + " 秒"
	case "resolution":
		return strings.ToUpper(v)
	}
	return v
}
