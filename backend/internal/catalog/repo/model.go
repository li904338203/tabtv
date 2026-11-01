package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
)

type Provider struct {
	ID               int64
	Code             string
	Name             string
	AdapterType      string
	BaseURL          string
	AuthType         string
	APIKeyCipher     string
	KeyVersion       int
	Headers          json.RawMessage
	TestPath         string
	ConnectTimeoutMS int
	RequestTimeoutMS int
	IdleTimeoutMS    int
	MaxRetries       int
	// MaxConcurrent 0 表示不限。跨 Worker 实例生效（在领取任务时按库里的 running 数判）。
	MaxConcurrent int
	Status        string
	Version       int
}

func (p Provider) IsReady() bool {
	// 就绪 = 已启用 且（无需认证 或 已配密钥）。
	// 只看 status 会让一个没配 key 的 Provider 被当成可用。
	return p.Status == "enabled" && (p.AuthType == "none" || p.APIKeyCipher != "")
}

func (r *Repo) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := r.pool.Query(ctx, `
		select id, code, name, adapter_type, base_url, auth_type,
		       coalesce(api_key_cipher,''), key_version, headers, coalesce(test_path,''),
		       connect_timeout_ms, request_timeout_ms, idle_timeout_ms, max_retries,
		       max_concurrent, status, version
		from provider_config order by code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.ID, &p.Code, &p.Name, &p.AdapterType, &p.BaseURL, &p.AuthType,
			&p.APIKeyCipher, &p.KeyVersion, &p.Headers, &p.TestPath,
			&p.ConnectTimeoutMS, &p.RequestTimeoutMS, &p.IdleTimeoutMS,
			&p.MaxRetries, &p.MaxConcurrent, &p.Status, &p.Version); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) GetProvider(ctx context.Context, code string) (Provider, error) {
	var p Provider
	err := r.pool.QueryRow(ctx, `
		select id, code, name, adapter_type, base_url, auth_type,
		       coalesce(api_key_cipher,''), key_version, headers, coalesce(test_path,''),
		       connect_timeout_ms, request_timeout_ms, idle_timeout_ms, max_retries,
		       max_concurrent, status, version
		from provider_config where code = $1`, code).
		Scan(&p.ID, &p.Code, &p.Name, &p.AdapterType, &p.BaseURL, &p.AuthType,
			&p.APIKeyCipher, &p.KeyVersion, &p.Headers, &p.TestPath,
			&p.ConnectTimeoutMS, &p.RequestTimeoutMS, &p.IdleTimeoutMS,
			&p.MaxRetries, &p.MaxConcurrent, &p.Status, &p.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	return p, err
}

// GetProviderByID 按主键取 Provider（执行任务时用，那时手上只有模型的 provider_id）。
func (r *Repo) GetProviderByID(ctx context.Context, id int64) (Provider, error) {
	var p Provider
	err := r.pool.QueryRow(ctx, `
		select id, code, name, adapter_type, base_url, auth_type,
		       coalesce(api_key_cipher,''), key_version, headers, coalesce(test_path,''),
		       connect_timeout_ms, request_timeout_ms, idle_timeout_ms, max_retries,
		       max_concurrent, status, version
		from provider_config where id = $1`, id).
		Scan(&p.ID, &p.Code, &p.Name, &p.AdapterType, &p.BaseURL, &p.AuthType,
			&p.APIKeyCipher, &p.KeyVersion, &p.Headers, &p.TestPath,
			&p.ConnectTimeoutMS, &p.RequestTimeoutMS, &p.IdleTimeoutMS,
			&p.MaxRetries, &p.MaxConcurrent, &p.Status, &p.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, ErrNotFound
	}
	return p, err
}

// SaveProvider 带版本的 upsert。version=0 表示新建。
func (r *Repo) SaveProvider(ctx context.Context, id int64, p Provider) error {
	if p.Version == 0 {
		_, err := r.pool.Exec(ctx, `
			insert into provider_config
			  (id, code, name, adapter_type, base_url, auth_type, api_key_cipher, key_version,
			   headers, test_path, connect_timeout_ms, request_timeout_ms, idle_timeout_ms,
			   max_retries, max_concurrent, status)
			values ($1,$2,$3,$4,$5,$6,nullif($7,''),$8,$9,nullif($10,''),$11,$12,$13,$14,$15,$16)`,
			id, p.Code, p.Name, p.AdapterType, p.BaseURL, p.AuthType, p.APIKeyCipher,
			p.KeyVersion, p.Headers, p.TestPath, p.ConnectTimeoutMS, p.RequestTimeoutMS,
			p.IdleTimeoutMS, p.MaxRetries, p.MaxConcurrent, p.Status)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		return err
	}
	tag, err := r.pool.Exec(ctx, `
		update provider_config
		set name=$2, adapter_type=$3, base_url=$4, auth_type=$5,
		    api_key_cipher=nullif($6,''), key_version=$7, headers=$8, test_path=nullif($9,''),
		    connect_timeout_ms=$10, request_timeout_ms=$11, idle_timeout_ms=$12,
		    max_retries=$13, max_concurrent=$14, status=$15, version=version+1, updated_at=now()
		where code=$1 and version=$16`,
		p.Code, p.Name, p.AdapterType, p.BaseURL, p.AuthType, p.APIKeyCipher, p.KeyVersion,
		p.Headers, p.TestPath, p.ConnectTimeoutMS, p.RequestTimeoutMS, p.IdleTimeoutMS,
		p.MaxRetries, p.MaxConcurrent, p.Status, p.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrVersionConflict
	}
	return nil
}

// HasEnabledModels 判断 Provider 是否还有启用中的模型。
// ⚠️ 有启用模型时不能停用 Provider（02 §3.2）：停用会让这些模型
// 在用户点生成时才失败，而配置页看起来一切正常。
func (r *Repo) HasEnabledModels(ctx context.Context, providerID int64) (bool, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`select count(*) from model where provider_id = $1 and status = 'enabled'`, providerID).Scan(&n)
	return n > 0, err
}

type Model struct {
	ID                int64
	Code              string
	Name              string
	ProviderID        int64
	ProviderCode      string
	Domain            string
	SubType           string
	UpstreamModelCode string
	APIProtocol       string
	SubmitPath        string
	QueryPath         string
	EstSeconds        int
	MaxInputChars     *int
	MinMemberLevelID  *int64
	Status            string
	Sort              int
	// 视频能力（AGT-05）：只是规划阶段的提示，报价/受理/渲染都不读。
	// MaxDurationSec 为 nil = 未配置。
	MaxDurationSec *int
	IsMultiShot    bool
	HasNativeAudio bool
	// IsAgentCapable 可作推理模型（AGT-04）：保存时目录域校验 domain=text ∧ 协议可聊天。
	IsAgentCapable bool
}

func (r *Repo) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := r.pool.Query(ctx, `
		select m.id, m.code, m.name, m.provider_id, p.code, m.domain,
		       coalesce(m.sub_type,''), m.upstream_model_code, m.api_protocol,
		       coalesce(m.submit_path,''), coalesce(m.query_path,''), m.est_seconds,
		       m.max_input_chars, m.min_member_level_id, m.status, m.sort,
		       m.max_duration_sec, m.is_multi_shot, m.has_native_audio, m.is_agent_capable
		from model m join provider_config p on p.id = m.provider_id
		order by m.sort, m.code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderCode,
			&m.Domain, &m.SubType, &m.UpstreamModelCode, &m.APIProtocol,
			&m.SubmitPath, &m.QueryPath, &m.EstSeconds, &m.MaxInputChars,
			&m.MinMemberLevelID, &m.Status, &m.Sort,
			&m.MaxDurationSec, &m.IsMultiShot, &m.HasNativeAudio, &m.IsAgentCapable); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Repo) GetModel(ctx context.Context, code string) (Model, error) {
	var m Model
	err := r.pool.QueryRow(ctx, `
		select m.id, m.code, m.name, m.provider_id, p.code, m.domain,
		       coalesce(m.sub_type,''), m.upstream_model_code, m.api_protocol,
		       coalesce(m.submit_path,''), coalesce(m.query_path,''), m.est_seconds,
		       m.max_input_chars, m.min_member_level_id, m.status, m.sort,
		       m.max_duration_sec, m.is_multi_shot, m.has_native_audio, m.is_agent_capable
		from model m join provider_config p on p.id = m.provider_id
		where m.code = $1`, code).
		Scan(&m.ID, &m.Code, &m.Name, &m.ProviderID, &m.ProviderCode, &m.Domain,
			&m.SubType, &m.UpstreamModelCode, &m.APIProtocol, &m.SubmitPath,
			&m.QueryPath, &m.EstSeconds, &m.MaxInputChars, &m.MinMemberLevelID,
			&m.Status, &m.Sort, &m.MaxDurationSec, &m.IsMultiShot, &m.HasNativeAudio,
			&m.IsAgentCapable)
	if errors.Is(err, pgx.ErrNoRows) {
		return Model{}, ErrNotFound
	}
	return m, err
}

// HasGenerations 判断模型是否已有历史任务。
// ⚠️ 有历史任务后，provider/协议/domain 等关键字段被锁定（02 §3.1）：
// 改掉它们会让历史 generation 的快照无法被正确解释。
func (r *Repo) HasGenerations(ctx context.Context, modelID int64) (bool, error) {
	// ⚠️ 用 exists 不用 count：原先的 `count(*) ... limit 1` 里 limit 限的是**结果行**，
	// 不是扫描行 —— 每保存一次模型都要把它的全部历史任务数一遍。
	var has bool
	err := r.pool.QueryRow(ctx,
		`select exists (select 1 from generation where model_id = $1)`, modelID).Scan(&has)
	return has, err
}

func (r *Repo) SaveModel(ctx context.Context, id int64, m Model, isNew bool) error {
	if isNew {
		_, err := r.pool.Exec(ctx, `
			insert into model (id, code, name, provider_id, domain, sub_type,
			  upstream_model_code, api_protocol, submit_path, query_path,
			  est_seconds, max_input_chars, min_member_level_id, status, sort,
			  max_duration_sec, is_multi_shot, has_native_audio, is_agent_capable)
			values ($1,$2,$3,$4,$5,nullif($6,''),$7,$8,nullif($9,''),nullif($10,''),
			        $11,$12,$13,$14,$15,$16,$17,$18,$19)`,
			id, m.Code, m.Name, m.ProviderID, m.Domain, m.SubType, m.UpstreamModelCode,
			m.APIProtocol, m.SubmitPath, m.QueryPath, m.EstSeconds, m.MaxInputChars,
			m.MinMemberLevelID, m.Status, m.Sort,
			m.MaxDurationSec, m.IsMultiShot, m.HasNativeAudio, m.IsAgentCapable)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		return err
	}
	tag, err := r.pool.Exec(ctx, `
		update model set name=$2, provider_id=$3, domain=$4, sub_type=nullif($5,''),
		    upstream_model_code=$6, api_protocol=$7, submit_path=nullif($8,''),
		    query_path=nullif($9,''), est_seconds=$10, max_input_chars=$11,
		    min_member_level_id=$12, status=$13, sort=$14,
		    max_duration_sec=$15, is_multi_shot=$16, has_native_audio=$17,
		    is_agent_capable=$18, updated_at=now()
		where code=$1`,
		m.Code, m.Name, m.ProviderID, m.Domain, m.SubType, m.UpstreamModelCode,
		m.APIProtocol, m.SubmitPath, m.QueryPath, m.EstSeconds, m.MaxInputChars,
		m.MinMemberLevelID, m.Status, m.Sort,
		m.MaxDurationSec, m.IsMultiShot, m.HasNativeAudio, m.IsAgentCapable)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type ModelOperation struct {
	OperationCode string
	IsEnabled     bool
	EstSeconds    *int
}

func (r *Repo) ListModelOperations(ctx context.Context, modelID int64) ([]ModelOperation, error) {
	rows, err := r.pool.Query(ctx, `
		select o.code, mo.is_enabled, mo.est_seconds
		from model_operation mo join operation o on o.id = mo.operation_id
		where mo.model_id = $1 order by o.code`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelOperation
	for rows.Next() {
		var m ModelOperation
		if err := rows.Scan(&m.OperationCode, &m.IsEnabled, &m.EstSeconds); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AllModelOperations 一次取回全部模型的操作绑定，按模型 code 归组。
//
// 后台列表与「谁在靠这条价格报价」都要看全部绑定：逐个模型查的话是 N+1，
// 模型一多，打开计价页就是几十次往返。
func (r *Repo) AllModelOperations(ctx context.Context) (map[string][]ModelOperation, error) {
	rows, err := r.pool.Query(ctx, `
		select m.code, o.code, mo.is_enabled, mo.est_seconds
		from model_operation mo
		join model m on m.id = mo.model_id
		join operation o on o.id = mo.operation_id
		order by m.code, o.code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]ModelOperation{}
	for rows.Next() {
		var model string
		var m ModelOperation
		if err := rows.Scan(&model, &m.OperationCode, &m.IsEnabled, &m.EstSeconds); err != nil {
			return nil, err
		}
		out[model] = append(out[model], m)
	}
	return out, rows.Err()
}

// Operation 是操作目录的一行（后台下拉用）。
type Operation struct {
	Code        string
	Name        string
	Domain      string
	InNodeTypes string
	OutNodeType string
	Status      string
	Sort        int
}

// ListOperations 操作目录。
func (r *Repo) ListOperations(ctx context.Context) ([]Operation, error) {
	rows, err := r.pool.Query(ctx, `
		select code, name, domain, coalesce(in_node_types,''), coalesce(out_node_type,''), status, sort
		from operation order by sort, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		var o Operation
		if err := rows.Scan(&o.Code, &o.Name, &o.Domain, &o.InNodeTypes, &o.OutNodeType,
			&o.Status, &o.Sort); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (r *Repo) SaveModelOperation(ctx context.Context, id, modelID int64, opCode string, enabled bool, est *int) error {
	tag, err := r.pool.Exec(ctx, `
		insert into model_operation (id, model_id, operation_id, is_enabled, est_seconds)
		select $1, $2, o.id, $4, $5 from operation o where o.code = $3
		on conflict (model_id, operation_id) do update
		set is_enabled = excluded.is_enabled, est_seconds = excluded.est_seconds, updated_at = now()`,
		id, modelID, opCode, enabled, est)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListCapabilities 取某模型某操作的能力矩阵。opCode 为空则取全部。
func (r *Repo) ListCapabilities(ctx context.Context, modelID int64, opCode string) (map[string][]domain.Capability, error) {
	rows, err := r.pool.Query(ctx, `
		select o.code, c.input_role, c.min_count, c.max_count, coalesce(c.allowed_mime,'')
		from model_input_capability c
		join operation o on o.id = c.operation_id
		where c.model_id = $1 and ($2 = '' or o.code = $2)
		order by o.code, c.input_role`, modelID, opCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]domain.Capability{}
	for rows.Next() {
		var op, role, mime string
		var minC, maxC int
		if err := rows.Scan(&op, &role, &minC, &maxC, &mime); err != nil {
			return nil, err
		}
		c := domain.Capability{Role: domain.InputRole(role), MinCount: minC, MaxCount: maxC}
		if mime != "" {
			c.AllowedMIME = splitCSV(mime)
		}
		out[op] = append(out[op], c)
	}
	return out, rows.Err()
}

func (r *Repo) SaveCapability(ctx context.Context, id, modelID int64, opCode string, c domain.Capability) error {
	var mime any
	if len(c.AllowedMIME) > 0 {
		mime = joinCSV(c.AllowedMIME)
	}
	tag, err := r.pool.Exec(ctx, `
		insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count, allowed_mime)
		select $1, $2, o.id, $4, $5, $6, $7 from operation o where o.code = $3
		on conflict (model_id, operation_id, input_role) do update
		set min_count = excluded.min_count, max_count = excluded.max_count,
		    allowed_mime = excluded.allowed_mime`,
		id, modelID, opCode, string(c.Role), c.MinCount, c.MaxCount, mime)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) DeleteCapability(ctx context.Context, modelID int64, opCode, role string) error {
	tag, err := r.pool.Exec(ctx, `
		delete from model_input_capability c using operation o
		where c.operation_id = o.id and c.model_id = $1 and o.code = $2 and c.input_role = $3`,
		modelID, opCode, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// OpsWithPricing 返回该模型哪些操作有可用价格（精确价或兜底价）。
func (r *Repo) OpsWithPricing(ctx context.Context, modelID int64) (map[string]bool, error) {
	rows, err := r.pool.Query(ctx, `
		select distinct o.code
		from model_operation mo
		join operation o on o.id = mo.operation_id
		join pricing pr on pr.operation_id = o.id
		     and (pr.model_id = $1 or pr.model_id is null)
		     and pr.status = 'enabled'
		where mo.model_id = $1 and mo.is_enabled`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out[c] = true
	}
	return out, rows.Err()
}

// UpsertProtocols 把代码里注册的协议同步进表（修 GAP-09）。
// 未注册的协议标记为 is_registered=false，管理端据此不展示。
func (r *Repo) UpsertProtocols(ctx context.Context, ps []domain.Protocol) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `update api_protocol set is_registered = false`); err != nil {
		return err
	}
	for _, p := range ps {
		if _, err := tx.Exec(ctx, `
			insert into api_protocol (code, name, is_async, requires_query_path, is_registered)
			values ($1,$2,$3,$4,true)
			on conflict (code) do update
			set name = excluded.name, is_async = excluded.is_async,
			    requires_query_path = excluded.requires_query_path,
			    is_registered = true, updated_at = now()`,
			p.Code, p.Name, p.IsAsync, p.RequiresQueryPath); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func splitCSV(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func joinCSV(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

// UsableModel 是给**用户端**看的模型，只含展示所需的字段。
//
// ⚠️ 刻意不含 provider code / 提交地址 / 协议：那些是运维信息，
// 下发给浏览器等于把上游拓扑公开出去（08 §8 的同一条）。
type UsableModel struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	Domain     string `json:"domain"`
	ResultType string `json:"resultType"`
	// MinMemberLevelID 为 0 表示不限。前端据此显示"需升级会员"。
	MinMemberLevelID int64 `json:"minMemberLevelId,omitempty"`
	// AgentCapable 可作画布 Agent 的推理模型（AGT-04）。
	AgentCapable bool `json:"agentCapable"`
	// HasNativeAudio 原生出声（AGT-05 的视频能力字段，MKT-T-01 起下发用户端）。
	// 只对视频模型有意义，非视频模型恒 false —— 列上的值可能是改域前残留的，
	// 所以在 SQL 里按 domain 收一道，不信任列值本身。
	// Agent / 营销据此在前端置灰依赖出声的选项；服务端判定仍是兜底（MKT-T-20）。
	HasNativeAudio bool `json:"hasNativeAudio"`
}

// OpAgentChat 是画布 Agent 的对话操作码（AGT-04）。
//
// ⚠️ 它不是画布节点的操作：`/models` 不带 agentCapable 时不把它算作可用操作。
const OpAgentChat = "agent_chat"

// OpSceneBuild 是 3D 导演台 AI 搭景的操作码（DRT-19）。同 agent_chat：它不是画布节点的操作，
// `/models` 不把它算作可用操作 —— 否则只为搭景绑的看图模型会出现在文本节点的下拉里，选了却报不出 text2text 的价。
const OpSceneBuild = "scene_build"

// nonCanvasOperations 不是画布节点操作的操作码：只绑了它们的模型不出现在画布节点的模型下拉里（`/models` 不带
// agentOnly 时）。智能剪辑的五个（docs/agent/smartcut SCT-08）只在剪辑台里用 —— 只为它们绑的模型列进图片 /
// 视频 / 音频节点的下拉，选了却报不出那个节点操作的价。
var nonCanvasOperations = []string{OpAgentChat, OpSceneBuild,
	"smartcut_plan", "speech2text", "video_desubtitle", "text2music", "video2audio"}

// ListUsableModels 列出用户当前**可能**调得动的模型：
// 模型启用 ∧ Provider 就绪 ∧ 至少绑定并启用了一个操作。
//
// ⚠️ 这里只做到"可能"：完整的合取式还包含有价、能力矩阵、会员等级，
// 那几条与**具体操作**绑定，只有报价那条路径知道。所以选出模型之后
// 仍要问一次 `/image-tools` —— 这不是重复，是两个粒度
// （"这个模型值得出现在下拉里吗" vs "这个模型 + 这个操作现在能不能点"）。
//
// agentOnly（AGT-04，AG-14）：只列可作推理模型的 —— `is_agent_capable` 且绑定并启用了
// `agent_chat`（没绑它的话 Agent 每轮报不出价，列出来选了也跑不动）。
// ⚠️ 不带 agentOnly 时 **`agent_chat` 不算可用操作**：只绑了它的推理模型会被列进
// 画布文本节点的下拉，选了又报不出 text2text 的价，用户看到的是「这个模型不能用」。
func (r *Repo) ListUsableModels(ctx context.Context, resultType string, agentOnly bool) ([]UsableModel, error) {
	// ⚠️ 产出类型取 `operation.domain`，**不要自己按操作码猜**：
	// 猜的话新增一个操作就要回来改这里，而漏改的表现是模型在下拉里消失。
	// 启用状态在这两张表上都是 `status`/`is_enabled` 各一套 ——
	// operation 用的是 `status='enabled'`（它有 disabled 这个终态），
	// model_operation 用的是布尔 `is_enabled`。写错哪一个都是 42703，
	// 而 42703 只在真跑起来时才报，编译期看不出来。
	// ⚠️ 产出类型报 `model.domain`（模型本来就是按产出分域的），
	// 筛选用 `operation.domain`。两者分开的理由是**去重**：
	// 一个模型可能绑着好几个操作，按操作的 domain 分组的话，
	// 不带筛选时同一个模型会在下拉里出现好几次 —— 用户会以为配重了。
	//
	// ⚠️ 两张表的启用字段名**不一样**：operation 是 `status='enabled'`
	// （它有 disabled 这个终态），model_operation 是布尔 `is_enabled`。
	// 写错哪一个都是 42703，而 42703 只在真跑起来时才报，编译期看不出来。
	rows, err := r.pool.Query(ctx, `
		select m.code, m.name, m.domain, m.domain,
		       coalesce(m.min_member_level_id, 0), m.is_agent_capable,
		       (m.domain = 'video' and m.has_native_audio)
		from model m
		join provider_config p on p.id = m.provider_id
		join model_operation mo on mo.model_id = m.id and mo.is_enabled
		join operation o on o.id = mo.operation_id and o.status = 'enabled'
		where m.status = 'enabled' and p.status = 'enabled'
		  and ($1 = '' or o.domain = $1)
		  and (case when $2 then m.is_agent_capable and o.code = $3
		            else o.code <> all($4::text[]) end)
		group by m.code, m.name, m.domain, m.min_member_level_id, m.sort, m.is_agent_capable,
		         m.has_native_audio
		order by m.sort, m.code`, resultType, agentOnly, OpAgentChat, nonCanvasOperations)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsableModel
	for rows.Next() {
		var m UsableModel
		if err := rows.Scan(&m.Code, &m.Name, &m.Domain, &m.ResultType,
			&m.MinMemberLevelID, &m.AgentCapable, &m.HasNativeAudio); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ModelParamSupport 是 model_param_support 的一行：一个模型对一个参数的型号级覆盖
// （参数合并的第三层，P15-01）。
type ModelParamSupport struct {
	ParamCode     string
	Override      []byte
	AllowedValues []byte
	DefaultValue  string
}

// ListModelParamSupports 取一个模型的型号级参数覆盖。模型不存在时 ErrNotFound；
// 模型存在但一行都没配时返回空切片（前端只用前两层）。
//
// ⚠️ 停用的参数定义不下发（`param_def.is_enabled`）：节点上已经不渲染它，覆盖它没有意义。
// 用 left join 是为了在一次查询里分清「模型不存在」与「模型没配覆盖」。
func (r *Repo) ListModelParamSupports(ctx context.Context, modelCode string) ([]ModelParamSupport, error) {
	rows, err := r.pool.Query(ctx, `
		select pd.code, mps.override, mps.allowed_values, coalesce(mps.default_value, '')
		from model m
		left join model_param_support mps on mps.model_id = m.id
		left join param_def pd on pd.id = mps.param_def_id and pd.is_enabled
		where m.code = $1
		order by pd.code nulls last`, modelCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := false
	out := []ModelParamSupport{}
	for rows.Next() {
		found = true
		var code *string
		var s ModelParamSupport
		if err := rows.Scan(&code, &s.Override, &s.AllowedValues, &s.DefaultValue); err != nil {
			return nil, err
		}
		if code == nil {
			continue
		}
		s.ParamCode = *code
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return out, nil
}

// OperationInputs 是「这个操作接受哪些节点类型作为输入」。
//
// ⚠️ 它与能力矩阵**不是一回事**：能力矩阵说的是"这个模型这次能收几张图"，
// 这里说的是"这个操作本身收不收图"。`text2img` 的 in_node_types 是
// `text,style` —— 连了一张图给它，受理时一定被拒，而能力矩阵可能是放行的
// （管理员完全可能给 text2img 配一条 image 角色）。
// 两条都要判，少判这一条的症状就是**工具栏说能用、点下去 422**。
func (r *Repo) OperationInputs(ctx context.Context) (map[string][]string, error) {
	rows, err := r.pool.Query(ctx,
		`select code, coalesce(in_node_types,'') from operation where status = 'enabled'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var code, types string
		if err := rows.Scan(&code, &types); err != nil {
			return nil, err
		}
		var list []string
		for _, t := range strings.Split(types, ",") {
			if t = strings.TrimSpace(t); t != "" {
				list = append(list, t)
			}
		}
		out[code] = list
	}
	return out, rows.Err()
}

// ListModelsForOperation 列出绑定了某个操作、此刻可能调得动的模型（模型启用 ∧ Provider 就绪 ∧ 绑定启用 ∧
// 操作启用），按排序。给不属于画布节点的操作选模型用（AI 搭景按排序逐个报价，取第一个通过的，DRT-19）。
// 有价、能力矩阵、会员等级仍由报价判断。
func (r *Repo) ListModelsForOperation(ctx context.Context, operationCode string) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		select m.code from model m
		join provider_config p on p.id = m.provider_id
		join model_operation mo on mo.model_id = m.id and mo.is_enabled
		join operation o on o.id = mo.operation_id and o.status = 'enabled'
		where m.status = 'enabled' and p.status = 'enabled' and o.code = $1
		order by m.sort, m.code`, operationCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		out = append(out, code)
	}
	return out, rows.Err()
}
