//go:build integration

// 模型保存的会员门槛三态（P15-05 补编辑表单时发现）。
//
// ⚠️ 此前后台接口里根本没有这个字段，而保存是整条替换 —— **任何一次保存模型都会把
// `min_member_level_id` 清成 NULL**：高等级专属的模型从此对所有人开放，而没有任何报错。
// 只能用真库证明：门槛是逻辑外键（member_level），要看的是那一列在一次次保存之后还剩什么。
package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

func TestSaveModel_会员门槛不传则保留(t *testing.T) {
	r, pool, next := setup(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	if err := svc.SyncProtocols(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveProvider(ctx, next(), repo.Provider{
		Code: "prov-gate", Name: "测试上游", AdapterType: "http",
		BaseURL: "https://upstream.example.com/v1", AuthType: "bearer",
		APIKeyCipher: "v1:fake", Headers: []byte(`{}`),
		ConnectTimeoutMS: 5000, RequestTimeoutMS: 30000, IdleTimeoutMS: 10000, Status: "enabled",
	}); err != nil {
		t.Fatal(err)
	}
	levelID := next()
	if _, err := pool.Exec(ctx, `insert into member_level (id, code, name, sort, is_enabled)
		values ($1, 'studio', '工作室', 30, true)`, levelID); err != nil {
		t.Fatal(err)
	}
	gate := func() *int64 {
		t.Helper()
		m, err := r.GetModel(ctx, "m-gate")
		if err != nil {
			t.Fatal(err)
		}
		return m.MinMemberLevelID
	}
	save := func(level *string) error {
		in := modelInput("m-gate", "prov-gate", "disabled")
		in.MinMemberLevel = level
		return svc.SaveModel(ctx, 0, in)
	}
	str := func(s string) *string { return &s }

	if err := save(nil); err != nil {
		t.Fatal(err)
	}
	if gate() != nil {
		t.Fatal("新建时没给门槛，应为不限")
	}
	if err := save(str(strconv.FormatInt(levelID, 10))); err != nil {
		t.Fatal(err)
	}
	if g := gate(); g == nil || *g != levelID {
		t.Fatalf("门槛应设为工作室，实际 %v", g)
	}
	// ⚠️ 核心断言：不带这个字段的保存（配置脚本、联调工具、只改了名字的表单）必须保留门槛
	if err := save(nil); err != nil {
		t.Fatal(err)
	}
	if g := gate(); g == nil || *g != levelID {
		t.Fatal("不传门槛的保存把它清掉了 —— 高等级专属模型会对所有人开放")
	}
	// 显式传空串才清除
	if err := save(str("")); err != nil {
		t.Fatal(err)
	}
	if gate() != nil {
		t.Fatal("传空串应清除门槛")
	}
	// 指向不存在的等级：拒
	if err := save(str("987654321")); codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("不存在的等级应被拒，得到 %v", err)
	}
}

// ---- 视频能力三项（AGT-05）----

func seedVideoProvider(t *testing.T, r *repo.Repo, next func() int64, svc *catalogapp.Service) {
	t.Helper()
	ctx := context.Background()
	if err := svc.SyncProtocols(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveProvider(ctx, next(), repo.Provider{
		Code: "prov-video", Name: "测试上游", AdapterType: "http",
		BaseURL: "https://upstream.example.com/v1", AuthType: "bearer",
		APIKeyCipher: "v1:fake", Headers: []byte(`{}`),
		ConnectTimeoutMS: 5000, RequestTimeoutMS: 30000, IdleTimeoutMS: 10000, Status: "enabled",
	}); err != nil {
		t.Fatal(err)
	}
}

func videoModelInput(code string) catalogapp.ModelInput {
	return catalogapp.ModelInput{
		Code: code, Name: "测试视频模型", ProviderCode: "prov-video", Domain: "video",
		UpstreamModelCode: "video-v1", APIProtocol: "generic_async",
		SubmitPath: "/videos", QueryPath: "/videos/{taskId}", EstSeconds: 120, Status: "disabled",
	}
}

func TestSaveModel_视频能力字段不传则保留(t *testing.T) {
	r, _, next := setup(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	seedVideoProvider(t, r, next, svc)
	intp := func(v int) *int { return &v }
	boolp := func(v bool) *bool { return &v }
	get := func() repo.Model {
		t.Helper()
		m, err := r.GetModel(ctx, "m-video")
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	// 新建不带三项：未配置 / false / false
	if err := svc.SaveModel(ctx, 0, videoModelInput("m-video")); err != nil {
		t.Fatal(err)
	}
	if m := get(); m.MaxDurationSec != nil || m.IsMultiShot || m.HasNativeAudio {
		t.Fatalf("新建不带视频能力应取默认，得到 %v/%v/%v", m.MaxDurationSec, m.IsMultiShot, m.HasNativeAudio)
	}

	in := videoModelInput("m-video")
	in.MaxDurationSec, in.IsMultiShot, in.HasNativeAudio = intp(10), boolp(true), boolp(true)
	if err := svc.SaveModel(ctx, 0, in); err != nil {
		t.Fatal(err)
	}
	if m := get(); m.MaxDurationSec == nil || *m.MaxDurationSec != 10 || !m.IsMultiShot || !m.HasNativeAudio {
		t.Fatalf("保存后回读不一致：%v/%v/%v", m.MaxDurationSec, m.IsMultiShot, m.HasNativeAudio)
	}
	// 列表回读同一份
	list, err := svc.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].MaxDurationSec == nil || *list[0].MaxDurationSec != 10 || !list[0].IsMultiShot || !list[0].HasNativeAudio {
		t.Fatalf("列表回读不一致：%+v", list)
	}

	// ⚠️ 核心断言：不带三项的保存（配置脚本、联调工具、只改了名字）必须保留
	in = videoModelInput("m-video")
	in.Name = "改了名字"
	if err := svc.SaveModel(ctx, 0, in); err != nil {
		t.Fatal(err)
	}
	if m := get(); m.MaxDurationSec == nil || *m.MaxDurationSec != 10 || !m.IsMultiShot || !m.HasNativeAudio || m.Name != "改了名字" {
		t.Fatalf("不传视频能力的保存把它们改掉了：%v/%v/%v", m.MaxDurationSec, m.IsMultiShot, m.HasNativeAudio)
	}

	// 显式清除时长，布尔显式改 false
	in = videoModelInput("m-video")
	in.ClearMaxDurationSec, in.IsMultiShot = true, boolp(false)
	if err := svc.SaveModel(ctx, 0, in); err != nil {
		t.Fatal(err)
	}
	if m := get(); m.MaxDurationSec != nil || m.IsMultiShot || !m.HasNativeAudio {
		t.Fatalf("清除时长 / 关多镜头后回读不对：%v/%v/%v", m.MaxDurationSec, m.IsMultiShot, m.HasNativeAudio)
	}

	// 时长 0 被领域校验拒
	in = videoModelInput("m-video")
	in.MaxDurationSec = intp(0)
	if err := svc.SaveModel(ctx, 0, in); codeOf(err) != errcode.InvalidArgument {
		t.Fatalf("时长 0 应被拒，得到 %v", err)
	}
}

func TestSaveModel_非视频模型不能设视频能力(t *testing.T) {
	r, pool, next := setup(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	seedVideoProvider(t, r, next, svc)
	intp := func(v int) *int { return &v }
	boolp := func(v bool) *bool { return &v }

	in := modelInput("m-image", "prov-video", "disabled")
	in.MaxDurationSec = intp(10)
	err := svc.SaveModel(ctx, 0, in)
	if codeOf(err) != errcode.InvalidArgument || !strings.Contains(err.Error(), "maxDurationSec") {
		t.Fatalf("图片模型设时长应 40001 且点名字段，得到 %v", err)
	}
	in = modelInput("m-image", "prov-video", "disabled")
	in.HasNativeAudio = boolp(true)
	if err := svc.SaveModel(ctx, 0, in); codeOf(err) != errcode.InvalidArgument || !strings.Contains(err.Error(), "hasNativeAudio") {
		t.Fatalf("图片模型设音轨应 40001 且点名字段，得到 %v", err)
	}
	// 传默认值放行（管理端切换类型时会带上 false）
	in = modelInput("m-image", "prov-video", "disabled")
	in.IsMultiShot, in.HasNativeAudio = boolp(false), boolp(false)
	if err := svc.SaveModel(ctx, 0, in); err != nil {
		t.Fatalf("图片模型传默认值应放行：%v", err)
	}

	// 视频模型改成图片模型（无历史任务时 domain 可改）：旧的视频能力不能留在库里
	v := videoModelInput("m-switch")
	v.MaxDurationSec, v.HasNativeAudio = intp(15), boolp(true)
	if err := svc.SaveModel(ctx, 0, v); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveModel(ctx, 0, modelInput("m-switch", "prov-video", "disabled")); err != nil {
		t.Fatal(err)
	}
	m, err := r.GetModel(ctx, "m-switch")
	if err != nil {
		t.Fatal(err)
	}
	if m.MaxDurationSec != nil || m.HasNativeAudio || m.IsMultiShot {
		t.Fatalf("改成图片模型后视频能力应清掉，得到 %v/%v/%v", m.MaxDurationSec, m.IsMultiShot, m.HasNativeAudio)
	}

	// 库约束兜底：绕过领域校验直接写 0 也进不去
	if _, err := pool.Exec(ctx, `update model set max_duration_sec = 0 where code = 'm-switch'`); err == nil {
		t.Fatal("max_duration_sec = 0 应被 ck_model_max_duration 拒绝")
	}
}

func TestVideoTraits_未配置时返回空值(t *testing.T) {
	r, _, next := setup(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	seedVideoProvider(t, r, next, svc)

	if err := svc.SaveModel(ctx, 0, videoModelInput("m-video")); err != nil {
		t.Fatal(err)
	}
	tr, err := svc.VideoTraits(ctx, "m-video")
	if err != nil {
		t.Fatal(err)
	}
	if !tr.OK || tr.MaxDurationSec != nil || tr.IsMultiShot || tr.HasNativeAudio {
		t.Fatalf("未配置的视频模型应 OK=true 且三项为空值，得到 %+v", tr)
	}

	in := videoModelInput("m-video")
	ten, yes := 10, true
	in.MaxDurationSec, in.IsMultiShot = &ten, &yes
	if err := svc.SaveModel(ctx, 0, in); err != nil {
		t.Fatal(err)
	}
	if tr, err = svc.VideoTraits(ctx, "m-video"); err != nil || tr.MaxDurationSec == nil || *tr.MaxDurationSec != 10 || !tr.IsMultiShot {
		t.Fatalf("配置后应读到 10 秒 + 多镜头，得到 %+v / %v", tr, err)
	}

	if err := svc.SaveModel(ctx, 0, modelInput("m-image", "prov-video", "disabled")); err != nil {
		t.Fatal(err)
	}
	if tr, err = svc.VideoTraits(ctx, "m-image"); err != nil || tr.OK {
		t.Fatalf("非视频模型应 OK=false 且不报错，得到 %+v / %v", tr, err)
	}
	if _, err = svc.VideoTraits(ctx, "m-nope"); codeOf(err) != errcode.NotFound {
		t.Fatalf("不存在的模型应 NotFound，得到 %v", err)
	}
}

// ---- 支持 Agent（AGT-04）----

func textModelInput(code, proto string) catalogapp.ModelInput {
	return catalogapp.ModelInput{
		Code: code, Name: "测试文本模型", ProviderCode: "prov-video", Domain: "text",
		UpstreamModelCode: "chat-v1", APIProtocol: proto, SubmitPath: "/chat",
		EstSeconds: 30, Status: "disabled",
	}
}

func TestSaveModel_支持Agent要求文本域且协议可聊天(t *testing.T) {
	r, _, next := setup(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	seedVideoProvider(t, r, next, svc)
	yes := true

	// 图片域勾选：拒（42203）
	img := modelInput("m-img", "prov-video", "disabled")
	img.IsAgentCapable = &yes
	if err := svc.SaveModel(ctx, 0, img); codeOf(err) != errcode.CapabilityUnsupported {
		t.Fatalf("图片模型勾支持 Agent 应 42203，得到 %v", err)
	}
	// generic_sync 文本模型勾选：拒，且说清是协议的问题
	gs := textModelInput("m-gs", "generic_sync")
	gs.IsAgentCapable = &yes
	err := svc.SaveModel(ctx, 0, gs)
	if codeOf(err) != errcode.CapabilityUnsupported || !strings.Contains(err.Error(), "generic_sync") {
		t.Fatalf("generic_sync 勾支持 Agent 应 42203 并点名协议，得到 %v", err)
	}
	// openai_chat / anthropic_messages：通过
	for _, proto := range []string{"openai_chat", "anthropic_messages"} {
		in := textModelInput("m-"+proto, proto)
		in.IsAgentCapable = &yes
		if err := svc.SaveModel(ctx, 0, in); err != nil {
			t.Fatalf("%s 应能勾支持 Agent：%v", proto, err)
		}
		if m, err := r.GetModel(ctx, "m-"+proto); err != nil || !m.IsAgentCapable {
			t.Fatalf("%s 保存后回读应为 true：%v / %v", proto, m.IsAgentCapable, err)
		}
	}
	// ⚠️ 按合并后的值校验：不传该字段、只把推理模型的协议改成 generic_sync，同样要拦
	in := textModelInput("m-openai_chat", "generic_sync")
	if err := svc.SaveModel(ctx, 0, in); codeOf(err) != errcode.CapabilityUnsupported {
		t.Fatalf("推理模型改成不可聊天的协议应 42203，得到 %v", err)
	}
}

func TestSaveModel_支持Agent不传则保留(t *testing.T) {
	r, _, next := setup(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	seedVideoProvider(t, r, next, svc)
	get := func() bool {
		t.Helper()
		m, err := r.GetModel(ctx, "m-agent")
		if err != nil {
			t.Fatal(err)
		}
		return m.IsAgentCapable
	}
	in := textModelInput("m-agent", "openai_chat")
	if err := svc.SaveModel(ctx, 0, in); err != nil {
		t.Fatal(err)
	}
	if get() {
		t.Fatal("新建不带该字段应为 false")
	}
	yes, no := true, false
	in.IsAgentCapable = &yes
	if err := svc.SaveModel(ctx, 0, in); err != nil {
		t.Fatal(err)
	}
	// ⚠️ 核心断言：老调用方（配置脚本、联调工具）不带这个字段保存，标记必须留着
	in.IsAgentCapable = nil
	in.Name = "改了名字"
	if err := svc.SaveModel(ctx, 0, in); err != nil {
		t.Fatal(err)
	}
	if !get() {
		t.Fatal("不传支持 Agent 的保存把标记清掉了")
	}
	in.IsAgentCapable = &no
	if err := svc.SaveModel(ctx, 0, in); err != nil {
		t.Fatal(err)
	}
	if get() {
		t.Fatal("显式传 false 应取消标记")
	}
}

// agentCapable=true 只列推理模型；不带参数时 agent_chat **不算**可用操作。
func TestListUsableModels_agentCapable过滤(t *testing.T) {
	r, pool, next := setupCatalog(t)
	ctx := context.Background()

	// 普通文本模型：绑 text2text
	seedModel(t, pool, next, modelSeed{code: "plain-text", providerStatus: "enabled",
		modelStatus: "enabled", domain: "text", op: "text2text", opEnabled: true})
	// 推理模型：只绑 agent_chat 且标记支持 Agent
	seedModel(t, pool, next, modelSeed{code: "agent-only", providerStatus: "enabled",
		modelStatus: "enabled", domain: "text", op: "agent_chat", opEnabled: true})
	// 两样都绑、标记支持 Agent
	seedModel(t, pool, next, modelSeed{code: "both", providerStatus: "enabled",
		modelStatus: "enabled", domain: "text", op: "text2text", opEnabled: true})
	bindOperation(t, pool, next, "both", "agent_chat")
	// 绑了 agent_chat 但没勾支持 Agent：两边都不列
	seedModel(t, pool, next, modelSeed{code: "unflagged", providerStatus: "enabled",
		modelStatus: "enabled", domain: "text", op: "agent_chat", opEnabled: true})
	if _, err := pool.Exec(ctx,
		`update model set is_agent_capable = true where code in ('agent-only','both')`); err != nil {
		t.Fatal(err)
	}

	codesOf := func(agentOnly bool) map[string]repo.UsableModel {
		t.Helper()
		list, err := r.ListUsableModels(ctx, "text", agentOnly)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]repo.UsableModel{}
		for _, m := range list {
			out[m.Code] = m
		}
		return out
	}

	agents := codesOf(true)
	if len(agents) != 2 || !agents["agent-only"].AgentCapable || !agents["both"].AgentCapable {
		t.Fatalf("agentCapable=true 应只列 agent-only 与 both，得到 %+v", agents)
	}
	plain := codesOf(false)
	if _, ok := plain["agent-only"]; ok {
		t.Fatal("只绑了 agent_chat 的推理模型不该出现在普通下拉里（选了报不出 text2text 的价）")
	}
	if _, ok := plain["unflagged"]; ok {
		t.Fatal("agent_chat 不该算作普通可用操作")
	}
	if _, ok := plain["plain-text"]; !ok || !plain["both"].AgentCapable || plain["plain-text"].AgentCapable {
		t.Fatalf("不带参数时行为应不变、并带上 agentCapable：%+v", plain)
	}
}
