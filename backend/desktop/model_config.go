package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/buildinfo"
	"github.com/li904338203/tabtv/backend/internal/platform/settings"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 模型配置的导出 / 导入（换电脑、重装时把「厂商 + 模型 + 操作绑定 + 能力矩阵 + 参数覆盖 + 公网中转存储」整体搬过去）。
//
// ⚠️ 文件里的 API Key 与 S3 SecretKey 是**明文**（用户要求随文件同步；库里本来也是明文，TTD-13）。
//    界面上导出前要说清这一点。
// ⚠️ 不放在前端逐条调保存接口：要读出明文 Key（管理端点从不回显），而且写入有先后 ——
//    厂商（含 Key）→ 模型（先停用）→ 绑定 / 能力 / 参数 → 再启用（启用要过发布检查，检查要看绑定）。
// 规则：以文件为准。同 code 的覆盖、没有的新建；本机有而文件里没有的厂商 / 模型**不动**（只在预览里列出来）。
//    同一模型下，文件里没有的操作绑定改为停用、能力行与参数覆盖删除 —— 否则「同步」之后两台机器的行为仍不一样。
//    每一步都走目录服务（与设置页同一套校验）。某一项失败只记在那一项上，其余照常导入，不整体回滚
//    （与模板启用同一条取舍：说清原因，修好后再导一次即可，导入是幂等的）。

const (
	modelConfigFormat  = "tabtv.settings"
	modelConfigKind    = "model-config"
	modelConfigVersion = 1
)

type modelConfigFile struct {
	Format     string              `json:"format"`
	Kind       string              `json:"kind"`
	Version    int                 `json:"version"`
	ExportedAt string              `json:"exportedAt"`
	AppVersion string              `json:"appVersion,omitempty"`
	Providers  []modelConfigProv   `json:"providers"`
	Models     []modelConfigModel  `json:"models"`
	Storage    *modelConfigStorage `json:"storage,omitempty"`
}

type modelConfigProv struct {
	Code             string          `json:"code"`
	Name             string          `json:"name"`
	AdapterType      string          `json:"adapterType"`
	BaseURL          string          `json:"baseUrl"`
	AuthType         string          `json:"authType"`
	APIKey           string          `json:"apiKey"`
	Headers          json.RawMessage `json:"headers,omitempty"`
	TestPath         string          `json:"testPath,omitempty"`
	ConnectTimeoutMS int             `json:"connectTimeoutMs"`
	RequestTimeoutMS int             `json:"requestTimeoutMs"`
	IdleTimeoutMS    int             `json:"idleTimeoutMs"`
	MaxRetries       int             `json:"maxRetries"`
	MaxConcurrent    int             `json:"maxConcurrent"`
	Status           string          `json:"status"`
}

type modelConfigModel struct {
	Code              string                      `json:"code"`
	Name              string                      `json:"name"`
	ProviderCode      string                      `json:"providerCode"`
	Domain            string                      `json:"domain"`
	SubType           string                      `json:"subType,omitempty"`
	UpstreamModelCode string                      `json:"upstreamModelCode"`
	APIProtocol       string                      `json:"apiProtocol"`
	SubmitPath        string                      `json:"submitPath,omitempty"`
	QueryPath         string                      `json:"queryPath,omitempty"`
	EstSeconds        int                         `json:"estSeconds"`
	MaxInputChars     *int                        `json:"maxInputChars,omitempty"`
	Sort              int                         `json:"sort"`
	MaxDurationSec    *int                        `json:"maxDurationSec,omitempty"`
	IsMultiShot       bool                        `json:"isMultiShot"`
	HasNativeAudio    bool                        `json:"hasNativeAudio"`
	IsAgentCapable    bool                        `json:"isAgentCapable"`
	Status            string                      `json:"status"`
	Operations        []modelConfigOp             `json:"operations"`
	Capabilities      map[string][]modelConfigCap `json:"capabilities"`
	Params            []modelConfigParam          `json:"params"`
}

type modelConfigOp struct {
	Operation  string `json:"operation"`
	Enabled    bool   `json:"enabled"`
	EstSeconds *int   `json:"estSeconds,omitempty"`
}

type modelConfigCap struct {
	Role        string   `json:"role"`
	Min         int      `json:"min"`
	Max         int      `json:"max"`
	AllowedMIME []string `json:"allowedMime,omitempty"`
}

type modelConfigParam struct {
	ParamCode     string          `json:"paramCode"`
	AllowedValues json.RawMessage `json:"allowedValues,omitempty"`
	DefaultValue  string          `json:"defaultValue,omitempty"`
	Override      json.RawMessage `json:"override,omitempty"`
}

type modelConfigStorage struct {
	Endpoint     string `json:"endpoint"`
	Region       string `json:"region"`
	Bucket       string `json:"bucket"`
	AccessKey    string `json:"accessKey"`
	SecretKey    string `json:"secretKey"`
	PathStyle    bool   `json:"pathStyle"`
	Prefix       string `json:"prefix"`
	SignTTLHours int    `json:"signTtlHours"`
}

type modelConfig struct {
	svc  *catalogapp.Service
	repo *catalogrepo.Repo
	st   *settings.Store
	// decrypt 把库里的 Key 还原成明文（tabtv 是直通，见 secret.Plain）。
	decrypt func(string) (string, error)
	who     func(*http.Request) int64
}

func mountModelConfig(r chi.Router, mc modelConfig, requireAdmin func(http.Handler) http.Handler,
	requirePerm func(string) func(http.Handler) http.Handler) {
	r.Route("/admin/model-config", func(r chi.Router) {
		r.Use(requireAdmin)
		r.With(requirePerm("model:read")).Get("/export", mc.exportHandler)
		r.With(requirePerm("model:write")).Post("/import", mc.importHandler)
	})
}

func (mc modelConfig) exportHandler(w http.ResponseWriter, r *http.Request) {
	f, err := mc.export(r.Context(), time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, f)
}

func (mc modelConfig) export(ctx context.Context, now time.Time) (modelConfigFile, error) {
	f := modelConfigFile{Format: modelConfigFormat, Kind: modelConfigKind, Version: modelConfigVersion,
		ExportedAt: now.UTC().Format(time.RFC3339), AppVersion: buildinfo.Version,
		Providers: []modelConfigProv{}, Models: []modelConfigModel{}}
	provs, err := mc.repo.ListProviders(ctx)
	if err != nil {
		return f, httpx.Wrap(errcode.Internal, err)
	}
	for _, p := range provs {
		key := ""
		if p.APIKeyCipher != "" {
			if key, err = mc.decrypt(p.APIKeyCipher); err != nil {
				return f, httpx.Errf(errcode.Internal, fmt.Sprintf("厂商 %s 的 Key 读不出来：%v", p.Code, err))
			}
		}
		f.Providers = append(f.Providers, modelConfigProv{
			Code: p.Code, Name: p.Name, AdapterType: p.AdapterType, BaseURL: p.BaseURL, AuthType: p.AuthType,
			APIKey: key, Headers: p.Headers, TestPath: p.TestPath,
			ConnectTimeoutMS: p.ConnectTimeoutMS, RequestTimeoutMS: p.RequestTimeoutMS, IdleTimeoutMS: p.IdleTimeoutMS,
			MaxRetries: p.MaxRetries, MaxConcurrent: p.MaxConcurrent, Status: p.Status,
		})
	}
	models, err := mc.repo.ListModels(ctx)
	if err != nil {
		return f, httpx.Wrap(errcode.Internal, err)
	}
	for _, m := range models {
		out := modelConfigModel{
			Code: m.Code, Name: m.Name, ProviderCode: m.ProviderCode, Domain: m.Domain, SubType: m.SubType,
			UpstreamModelCode: m.UpstreamModelCode, APIProtocol: m.APIProtocol, SubmitPath: m.SubmitPath,
			QueryPath: m.QueryPath, EstSeconds: m.EstSeconds, MaxInputChars: m.MaxInputChars, Sort: m.Sort,
			MaxDurationSec: m.MaxDurationSec, IsMultiShot: m.IsMultiShot, HasNativeAudio: m.HasNativeAudio,
			IsAgentCapable: m.IsAgentCapable, Status: m.Status,
			Operations: []modelConfigOp{}, Capabilities: map[string][]modelConfigCap{}, Params: []modelConfigParam{},
		}
		ops, err := mc.repo.ListModelOperations(ctx, m.ID)
		if err != nil {
			return f, httpx.Wrap(errcode.Internal, err)
		}
		for _, o := range ops {
			out.Operations = append(out.Operations, modelConfigOp{Operation: o.OperationCode, Enabled: o.IsEnabled, EstSeconds: o.EstSeconds})
		}
		caps, err := mc.repo.ListCapabilities(ctx, m.ID, "")
		if err != nil {
			return f, httpx.Wrap(errcode.Internal, err)
		}
		for op, list := range caps {
			for _, c := range list {
				out.Capabilities[op] = append(out.Capabilities[op], modelConfigCap{
					Role: string(c.Role), Min: c.MinCount, Max: c.MaxCount, AllowedMIME: c.AllowedMIME})
			}
		}
		params, err := mc.repo.ListParamSupportsAdmin(ctx, m.Code)
		if err != nil {
			return f, httpx.Wrap(errcode.Internal, err)
		}
		for _, p := range params {
			out.Params = append(out.Params, modelConfigParam{ParamCode: p.ParamCode,
				AllowedValues: nullJSON(p.AllowedValues), DefaultValue: p.DefaultValue, Override: nullJSON(p.Override)})
		}
		f.Models = append(f.Models, out)
	}
	if ep := mc.st.String(settings.S3Endpoint); ep != "" {
		f.Storage = &modelConfigStorage{
			Endpoint: ep, Region: mc.st.String(settings.S3Region), Bucket: mc.st.String(settings.S3Bucket),
			AccessKey: mc.st.String(settings.S3AccessKey), SecretKey: mc.st.String(settings.S3SecretKey),
			PathStyle: mc.st.Bool(settings.S3PathStyle), Prefix: mc.st.String(settings.S3Prefix),
			SignTTLHours: mc.st.Int(settings.S3SignTTLHours),
		}
	}
	return f, nil
}

func nullJSON(b json.RawMessage) json.RawMessage {
	if s := strings.TrimSpace(string(b)); s == "" || s == "null" {
		return nil
	}
	return b
}

// ---------------------------------------------------------------- 导入

type importItem struct {
	Code     string   `json:"code"`
	Name     string   `json:"name"`
	Action   string   `json:"action"` // create / update
	Status   string   `json:"status"` // 文件里的状态
	HasKey   bool     `json:"hasKey,omitempty"`
	OK       bool     `json:"ok"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

type importReport struct {
	DryRun          bool         `json:"dryRun"`
	Problems        []string     `json:"problems"`
	Providers       []importItem `json:"providers"`
	Models          []importItem `json:"models"`
	Storage         string       `json:"storage"` // none / set
	StorageEndpoint string       `json:"storageEndpoint,omitempty"`
	LocalOnly       struct {
		Providers []string `json:"providers"`
		Models    []string `json:"models"`
	} `json:"localOnly"`
	RestartRequired bool `json:"restartRequired"`
}

func (mc modelConfig) importHandler(w http.ResponseWriter, r *http.Request) {
	var f modelConfigFile
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 20<<20))
	if err := dec.Decode(&f); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "文件不是合法的 JSON"))
		return
	}
	dry := r.URL.Query().Get("dryRun") == "true"
	rep, err := mc.importFile(r.Context(), mc.who(r), f, dry)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
}

// validate 返回挡住整次导入的问题（文件形状不对、同一 code 出现两次、模型指向不存在的厂商）。
func (mc modelConfig) validate(ctx context.Context, f modelConfigFile) ([]string, error) {
	var problems []string
	if f.Format != modelConfigFormat || f.Kind != modelConfigKind {
		return []string{"这不是 tabtv 导出的模型配置文件（format / kind 对不上）"}, nil
	}
	if f.Version != modelConfigVersion {
		return []string{fmt.Sprintf("文件版本 %d 不受支持（当前只认 %d），请升级 tabtv 后再导入", f.Version, modelConfigVersion)}, nil
	}
	provInFile := map[string]bool{}
	for _, p := range f.Providers {
		if strings.TrimSpace(p.Code) == "" {
			problems = append(problems, "有一个厂商没有 code")
			continue
		}
		if provInFile[p.Code] {
			problems = append(problems, "厂商 "+p.Code+" 在文件里出现了两次")
		}
		provInFile[p.Code] = true
	}
	seen := map[string]bool{}
	for _, m := range f.Models {
		if strings.TrimSpace(m.Code) == "" {
			problems = append(problems, "有一个模型没有 code")
			continue
		}
		if seen[m.Code] {
			problems = append(problems, "模型 "+m.Code+" 在文件里出现了两次")
		}
		seen[m.Code] = true
		if !provInFile[m.ProviderCode] {
			if _, err := mc.repo.GetProvider(ctx, m.ProviderCode); errors.Is(err, catalogrepo.ErrNotFound) {
				problems = append(problems, fmt.Sprintf("模型 %s 的厂商 %s 既不在文件里、本机也没有", m.Code, m.ProviderCode))
			} else if err != nil {
				return nil, httpx.Wrap(errcode.Internal, err)
			}
		}
	}
	return problems, nil
}

func (mc modelConfig) importFile(ctx context.Context, operatorID int64, f modelConfigFile, dry bool) (importReport, error) {
	rep := importReport{DryRun: dry, Problems: []string{}, Providers: []importItem{}, Models: []importItem{}, Storage: "none"}
	rep.LocalOnly.Providers, rep.LocalOnly.Models = []string{}, []string{}
	problems, err := mc.validate(ctx, f)
	if err != nil {
		return rep, err
	}
	rep.Problems = append(rep.Problems, problems...)

	// 本机已有的，用来判「新建 / 覆盖」与列出「本机多出来的」。
	localProv := map[string]catalogrepo.Provider{}
	provs, err := mc.repo.ListProviders(ctx)
	if err != nil {
		return rep, httpx.Wrap(errcode.Internal, err)
	}
	for _, p := range provs {
		localProv[p.Code] = p
	}
	localModel := map[string]catalogrepo.Model{}
	models, err := mc.repo.ListModels(ctx)
	if err != nil {
		return rep, httpx.Wrap(errcode.Internal, err)
	}
	for _, m := range models {
		localModel[m.Code] = m
	}
	fileProv, fileModel := map[string]bool{}, map[string]bool{}
	for _, p := range f.Providers {
		fileProv[p.Code] = true
		_, exists := localProv[p.Code]
		rep.Providers = append(rep.Providers, importItem{Code: p.Code, Name: p.Name, Action: actionOf(exists),
			Status: p.Status, HasKey: p.APIKey != ""})
	}
	for _, m := range f.Models {
		fileModel[m.Code] = true
		_, exists := localModel[m.Code]
		rep.Models = append(rep.Models, importItem{Code: m.Code, Name: m.Name, Action: actionOf(exists), Status: m.Status})
	}
	for _, p := range provs {
		if !fileProv[p.Code] {
			rep.LocalOnly.Providers = append(rep.LocalOnly.Providers, p.Code)
		}
	}
	for _, m := range models {
		if !fileModel[m.Code] {
			rep.LocalOnly.Models = append(rep.LocalOnly.Models, m.Code)
		}
	}
	if f.Storage != nil && f.Storage.Endpoint != "" {
		rep.Storage, rep.StorageEndpoint = "set", f.Storage.Endpoint
	}
	if dry || len(rep.Problems) > 0 {
		return rep, nil
	}

	if err := mc.svc.SyncProtocols(ctx); err != nil {
		return rep, httpx.Wrap(errcode.Internal, err)
	}

	// 1) 厂商。要停用的厂商先保持启用：本机还挂着启用中的模型时不能停用（目录服务会拒），放到模型之后再停。
	provOK := map[string]bool{}
	var disableLater []modelConfigProv
	for i, p := range f.Providers {
		item := &rep.Providers[i]
		existing, exists := localProv[p.Code]
		status := p.Status
		if status != "enabled" && exists && existing.Status == "enabled" {
			status = "enabled"
			disableLater = append(disableLater, p)
		}
		in := catalogapp.ProviderInput{
			Code: p.Code, Name: p.Name, AdapterType: p.AdapterType, BaseURL: p.BaseURL, AuthType: p.AuthType,
			Headers: p.Headers, TestPath: p.TestPath,
			ConnectTimeoutMS: p.ConnectTimeoutMS, RequestTimeoutMS: p.RequestTimeoutMS, IdleTimeoutMS: p.IdleTimeoutMS,
			MaxRetries: p.MaxRetries, MaxConcurrent: p.MaxConcurrent, Status: status,
		}
		if in.AdapterType == "" {
			in.AdapterType = "configurable_http"
		}
		if exists {
			in.Version = existing.Version
		}
		// 文件里没 Key = 保留本机已有的（不把空值当成「清除」）。
		if p.APIKey != "" {
			in.APIKey = &catalogapp.SecretField{Action: "replace", Value: p.APIKey}
		}
		if err := mc.svc.SaveProvider(ctx, operatorID, in); err != nil {
			item.Error = errText(err)
			continue
		}
		item.OK, provOK[p.Code] = true, true
	}

	// 2) 模型。
	for i, m := range f.Models {
		item := &rep.Models[i]
		if _, inFile := fileProv[m.ProviderCode]; inFile && !provOK[m.ProviderCode] {
			item.Error = "所属厂商 " + m.ProviderCode + " 没有导入成功"
			continue
		}
		mc.importModel(ctx, operatorID, m, localModel, item)
	}

	// 3) 文件里要停用的厂商。
	for _, p := range disableLater {
		existing, err := mc.repo.GetProvider(ctx, p.Code)
		if err != nil {
			continue
		}
		in := catalogapp.ProviderInput{
			Code: existing.Code, Name: existing.Name, AdapterType: existing.AdapterType, BaseURL: existing.BaseURL,
			AuthType: existing.AuthType, Headers: existing.Headers, TestPath: existing.TestPath,
			ConnectTimeoutMS: existing.ConnectTimeoutMS, RequestTimeoutMS: existing.RequestTimeoutMS,
			IdleTimeoutMS: existing.IdleTimeoutMS, MaxRetries: existing.MaxRetries, MaxConcurrent: existing.MaxConcurrent,
			Status: "disabled", Version: existing.Version,
		}
		if err := mc.svc.SaveProvider(ctx, operatorID, in); err != nil {
			for i := range rep.Providers {
				if rep.Providers[i].Code == p.Code {
					rep.Providers[i].Warnings = append(rep.Providers[i].Warnings, "没有停用："+errText(err))
				}
			}
		}
	}

	// 4) 公网中转存储（改了要重启才生效）。
	if rep.Storage == "set" {
		s := f.Storage
		vals := map[string]any{
			settings.S3Endpoint: s.Endpoint, settings.S3Region: s.Region, settings.S3Bucket: s.Bucket,
			settings.S3AccessKey: s.AccessKey, settings.S3PathStyle: s.PathStyle, settings.S3Prefix: s.Prefix,
		}
		if s.SecretKey != "" {
			vals[settings.S3SecretKey] = s.SecretKey
		}
		if s.SignTTLHours > 0 {
			vals[settings.S3SignTTLHours] = s.SignTTLHours
		}
		raw := map[string]json.RawMessage{}
		for k, v := range vals {
			b, _ := json.Marshal(v)
			raw[k] = b
		}
		restart, err := mc.st.Set(ctx, raw)
		if err != nil {
			rep.Problems = append(rep.Problems, "公网中转存储没有写入："+err.Error())
		} else {
			rep.RestartRequired = len(restart) > 0
		}
	}
	return rep, nil
}

// importModel：先按文件字段保存（不是本来就启用的一律先停用），再同步绑定 / 能力 / 参数，最后按文件状态启用。
func (mc modelConfig) importModel(ctx context.Context, operatorID int64, m modelConfigModel,
	local map[string]catalogrepo.Model, item *importItem) {
	existing, exists := local[m.Code]
	in := catalogapp.ModelInput{
		Code: m.Code, Name: m.Name, ProviderCode: m.ProviderCode, Domain: m.Domain, SubType: m.SubType,
		UpstreamModelCode: m.UpstreamModelCode, APIProtocol: m.APIProtocol,
		SubmitPath: m.SubmitPath, QueryPath: m.QueryPath, EstSeconds: m.EstSeconds,
		MaxInputChars: m.MaxInputChars, Sort: m.Sort,
		MaxDurationSec: m.MaxDurationSec, ClearMaxDurationSec: m.MaxDurationSec == nil,
		IsMultiShot: &m.IsMultiShot, HasNativeAudio: &m.HasNativeAudio, IsAgentCapable: &m.IsAgentCapable,
		Status: "disabled",
	}
	// 本来就启用、文件里也启用的先保持启用（导入中途失败也不会把正在用的模型停掉）。
	if exists && existing.Status == "enabled" && m.Status == "enabled" {
		in.Status = "enabled"
	}
	if err := mc.svc.SaveModel(ctx, operatorID, in); err != nil {
		item.Error = errText(err)
		return
	}
	warn := func(format string, a ...any) { item.Warnings = append(item.Warnings, fmt.Sprintf(format, a...)) }

	row, err := mc.repo.GetModel(ctx, m.Code)
	if err != nil {
		item.Error = errText(err)
		return
	}
	// 操作绑定：文件里的照写，本机多出来的改为停用。
	bound, err := mc.repo.ListModelOperations(ctx, row.ID)
	if err != nil {
		item.Error = errText(err)
		return
	}
	inFile := map[string]bool{}
	for _, o := range m.Operations {
		inFile[o.Operation] = true
		if err := mc.svc.SaveModelOperation(ctx, operatorID, m.Code, o.Operation, o.Enabled, o.EstSeconds); err != nil {
			warn("操作 %s 没有绑定：%s", o.Operation, errText(err))
		}
	}
	for _, b := range bound {
		if !inFile[b.OperationCode] && b.IsEnabled {
			if err := mc.svc.SaveModelOperation(ctx, operatorID, m.Code, b.OperationCode, false, b.EstSeconds); err != nil {
				warn("操作 %s 没有停用：%s", b.OperationCode, errText(err))
			}
		}
	}
	// 能力矩阵：照写文件里的，删掉文件里没有的行。
	caps, err := mc.repo.ListCapabilities(ctx, row.ID, "")
	if err != nil {
		item.Error = errText(err)
		return
	}
	want := map[string]bool{}
	for _, op := range sortedKeys(m.Capabilities) {
		for _, c := range m.Capabilities[op] {
			want[op+"/"+c.Role] = true
			if err := mc.svc.SaveCapability(ctx, operatorID, m.Code, op, catalogdomain.Capability{
				Role: catalogdomain.InputRole(c.Role), MinCount: c.Min, MaxCount: c.Max, AllowedMIME: c.AllowedMIME,
			}); err != nil {
				warn("输入规则 %s/%s 没有写入：%s", op, c.Role, errText(err))
			}
		}
	}
	for op, list := range caps {
		for _, c := range list {
			if !want[op+"/"+string(c.Role)] {
				if err := mc.svc.DeleteCapability(ctx, operatorID, m.Code, op, string(c.Role)); err != nil {
					warn("输入规则 %s/%s 没有删除：%s", op, c.Role, errText(err))
				}
			}
		}
	}
	// 参数覆盖：同上。
	params, err := mc.repo.ListParamSupportsAdmin(ctx, m.Code)
	if err != nil {
		item.Error = errText(err)
		return
	}
	wantParam := map[string]bool{}
	for _, p := range m.Params {
		wantParam[p.ParamCode] = true
		if err := mc.svc.SaveParamSupport(ctx, operatorID, m.Code, catalogapp.ParamSupportInput{
			ParamCode: p.ParamCode, AllowedValues: p.AllowedValues, DefaultValue: p.DefaultValue, Override: p.Override,
		}); err != nil {
			warn("参数 %s 没有写入：%s", p.ParamCode, errText(err))
		}
	}
	for _, p := range params {
		if !wantParam[p.ParamCode] {
			if err := mc.svc.DeleteParamSupport(ctx, operatorID, m.Code, p.ParamCode); err != nil {
				warn("参数 %s 没有删除：%s", p.ParamCode, errText(err))
			}
		}
	}
	// 最后按文件状态启用（过发布检查）。
	if m.Status == "enabled" {
		in.Status = "enabled"
		if err := mc.svc.SaveModel(ctx, operatorID, in); err != nil {
			item.Error = "已导入但没有启用：" + errText(err)
			return
		}
	}
	item.OK = true
}

func actionOf(exists bool) string {
	if exists {
		return "update"
	}
	return "create"
}

func errText(err error) string {
	var be *httpx.BizError
	if errors.As(err, &be) && be.Message != "" {
		return be.Message
	}
	return err.Error()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
