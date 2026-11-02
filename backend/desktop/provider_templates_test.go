package desktop

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/resources"
)

// 官方域名白名单（docs/research/厂商官方端点核实.md §9）。模板里出现别的域名（尤其是中转）即失败。
var officialHosts = map[string]bool{
	"api.openai.com": true, "api.anthropic.com": true, "api-beijing.klingai.com": true,
	"ark.cn-beijing.volces.com": true, "openspeech.bytedance.com": true,
	"mediakit.cn-beijing.volces.com": true, "api.x.ai": true, "api.elevenlabs.io": true,
}

// 不连库就能核对的部分：域名、协议路径、协议能做的操作与角色、操作组与能力规则齐全。
func TestProviderTemplates_数据文件自洽(t *testing.T) {
	f, err := loadProviderTemplates()
	if err != nil {
		t.Fatal(err)
	}
	providers := map[string]bool{}
	for _, p := range f.Providers {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Scheme != "https" || !officialHosts[u.Host] {
			t.Errorf("厂商 %s 的地址不是官方 https 域名：%s", p.Code, p.BaseURL)
		}
		providers[p.Code] = true
	}
	seen := map[string]bool{}
	for _, m := range f.Models {
		if seen[m.Code] {
			t.Errorf("模型 code 重复：%s", m.Code)
		}
		seen[m.Code] = true
		if !providers[m.Provider] {
			t.Errorf("模型 %s 的厂商 %s 不在模板里", m.Code, m.Provider)
		}
		if err := catalogdomain.ValidateModelProtocol(m.Protocol, f.submitPath(m), m.QueryPath); err != nil {
			t.Errorf("模型 %s：%v", m.Code, err)
		}
		ops := f.operations(m)
		if len(ops) == 0 {
			t.Errorf("模型 %s 没有绑定操作", m.Code)
		}
		for _, op := range ops {
			if err := catalogdomain.CheckProtocolOperation(m.Protocol, op); err != nil {
				t.Errorf("模型 %s：%v", m.Code, err)
			}
			rules := f.CapabilityRules[op]
			if len(rules) == 0 && !inheritsCaps(op) {
				t.Errorf("操作 %s 没有能力规则", op)
			}
			for _, c := range rules {
				if err := catalogdomain.CheckProtocolRole(m.Protocol, op, catalogdomain.InputRole(c.Role)); err != nil {
					t.Errorf("模型 %s：%v", m.Code, err)
				}
			}
		}
	}
}

// inheritsCaps 是从 text2text 继承能力矩阵的操作（catalog/domain.InheritCapabilityFlags）。
func inheritsCaps(op string) bool {
	return op == "agent_chat" || op == "scene_build" || op == "smartcut_plan"
}

// 真库：全新安装后全部厂商与模型已建、全部停用、无 Key；每个模型的发布阻塞只剩「厂商未就绪」
// （即绑定、能力矩阵、价格都齐，填 Key 启用即可）；再启动一次不重复建、不改已有配置。
func TestProviderTemplates_首次启动写入且幂等(t *testing.T) {
	if _, err := resources.Postgres(); err != nil {
		t.Skipf("没有 PG 二进制：%v", err)
	}
	t.Setenv("TABTV_DATA_DIR", t.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	f, err := loadProviderTemplates()
	if err != nil {
		t.Fatal(err)
	}

	rt, err := Boot(ctx, log)
	if err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	r := catalogrepo.New(rt.pool.Pool)
	svc := catalogapp.New(r, func() int64 { return 1 })

	for _, p := range f.Providers {
		got, err := r.GetProvider(ctx, p.Code)
		if err != nil {
			t.Fatalf("厂商 %s 未建：%v", p.Code, err)
		}
		if got.Status != "disabled" || got.APIKeyCipher != "" {
			t.Errorf("厂商 %s 应停用且无 Key：status=%s hasKey=%v", p.Code, got.Status, got.APIKeyCipher != "")
		}
	}
	for _, m := range f.Models {
		got, err := r.GetModel(ctx, m.Code)
		if err != nil {
			t.Fatalf("模型 %s 未建：%v", m.Code, err)
		}
		if got.Status != "disabled" {
			t.Errorf("模型 %s 应停用：%s", m.Code, got.Status)
		}
		if got.IsAgentCapable != m.AgentCapable {
			t.Errorf("模型 %s 推理模型标记 %v，期望 %v", m.Code, got.IsAgentCapable, m.AgentCapable)
		}
		blockers, err := svc.Publishable(ctx, m.Code)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range blockers {
			if b.Code != "provider_not_ready" {
				t.Errorf("模型 %s 有缺 Key 之外的发布阻塞：%s %s", m.Code, b.Code, b.Detail)
			}
		}
	}
	countRows := func() (n int) {
		t.Helper()
		if err := rt.pool.Pool.QueryRow(ctx, `select
			(select count(*) from provider_config) + (select count(*) from model)
			+ (select count(*) from model_operation) + (select count(*) from model_input_capability)
			+ (select count(*) from model_param_support)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countRows()
	// 用户改过的配置第二次启动不能被改回去：停用一个绑定。
	if err := svc.SaveModelOperation(ctx, 1, "kling_3_0", "first_last_frame", false, nil); err != nil {
		t.Fatal(err)
	}
	rt.Close(ctx)

	rt, err = Boot(ctx, log)
	if err != nil {
		t.Fatalf("第二次启动失败：%v", err)
	}
	defer rt.Close(context.Background())
	if after := countRows(); after != before {
		t.Errorf("第二次启动不应新增行：%d → %d", before, after)
	}
	m, _ := catalogrepo.New(rt.pool.Pool).GetModel(ctx, "kling_3_0")
	ops, _ := catalogrepo.New(rt.pool.Pool).ListModelOperations(ctx, m.ID)
	for _, o := range ops {
		if o.OperationCode == "first_last_frame" && o.IsEnabled {
			t.Error("用户停用的绑定被第二次启动改回了启用")
		}
	}
}
