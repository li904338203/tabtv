package domain

import (
	"strings"
	"testing"
)

func caps(list ...Capability) CapabilitySet { return NewCapabilitySet(list) }

// ⚠️ GAP-22 的核心场景：规则层放行的组合，具体型号未必接受。
// 例：通用 ref2video 允许 audio，而 Seedance 拒收 audio。
func TestCapabilitySet_拒绝未声明的角色(t *testing.T) {
	// 模拟 Seedance：ref2video 只收 image，不收 audio。
	cs := caps(Capability{Role: RoleImage, MinCount: 1, MaxCount: 4})

	err := cs.Check(RoleCount{RoleImage: 1, RoleAudio: 1})
	if err == nil {
		t.Fatal("模型未声明 audio 能力，应拒绝")
	}
	// 错误必须说清是哪个角色，"不支持该组合"让用户无从调整。
	if !strings.Contains(err.Error(), "audio") {
		t.Errorf("错误应指明角色，实际 %v", err)
	}
	if err := cs.Check(RoleCount{RoleImage: 1}); err != nil {
		t.Errorf("只给 image 应通过: %v", err)
	}
}

// 例：BBLABU 不支持 first_last_frame —— 表现为该操作下没有 first_frame 能力。
func TestCapabilitySet_数量区间(t *testing.T) {
	cs := caps(
		Capability{Role: RoleFirstFrame, MinCount: 1, MaxCount: 1},
		Capability{Role: RoleLastFrame, MinCount: 1, MaxCount: 1},
	)
	if err := cs.Check(RoleCount{RoleFirstFrame: 1, RoleLastFrame: 1}); err != nil {
		t.Errorf("首尾帧各 1 张应通过: %v", err)
	}
	// 缺尾帧
	err := cs.Check(RoleCount{RoleFirstFrame: 1})
	if err == nil {
		t.Fatal("缺尾帧应被拒绝")
	}
	if !strings.Contains(err.Error(), "至少 1") {
		t.Errorf("错误应说明要求数量，实际 %v", err)
	}
	// 超量
	err = cs.Check(RoleCount{RoleFirstFrame: 2, RoleLastFrame: 1})
	if err == nil || !strings.Contains(err.Error(), "最多") {
		t.Errorf("超量应被拒绝并说明上限，实际 %v", err)
	}
}

func TestCapabilitySet_可选角色(t *testing.T) {
	// reference 可选（min=0），不给也行。
	cs := caps(
		Capability{Role: RoleText, MinCount: 1, MaxCount: 1},
		Capability{Role: RoleReference, MinCount: 0, MaxCount: 3},
	)
	if err := cs.Check(RoleCount{RoleText: 1}); err != nil {
		t.Errorf("可选角色不给应通过: %v", err)
	}
	if err := cs.Check(RoleCount{RoleText: 1, RoleReference: 3}); err != nil {
		t.Errorf("可选角色给满应通过: %v", err)
	}
	if err := cs.Check(RoleCount{RoleText: 1, RoleReference: 4}); err == nil {
		t.Error("超过可选上限应被拒绝")
	}
}

func TestCapabilitySet_MIME(t *testing.T) {
	cs := caps(Capability{Role: RoleImage, MinCount: 1, MaxCount: 1,
		AllowedMIME: []string{"image/png", "image/jpeg"}})

	if err := cs.CheckMIME(RoleImage, "image/PNG"); err != nil {
		t.Errorf("MIME 比较应忽略大小写: %v", err)
	}
	err := cs.CheckMIME(RoleImage, "image/gif")
	if err == nil || !strings.Contains(err.Error(), "image/png") {
		t.Errorf("不支持的格式应被拒绝并列出允许项，实际 %v", err)
	}
	if err := cs.CheckMIME(RoleAudio, "audio/mp3"); err == nil {
		t.Error("未声明的角色应被拒绝")
	}
	// 未声明 allowedMIME 即不限
	open := caps(Capability{Role: RoleVideo, MinCount: 1, MaxCount: 1})
	if err := open.CheckMIME(RoleVideo, "video/anything"); err != nil {
		t.Errorf("未声明 MIME 限制应放行: %v", err)
	}
}

func TestValidateCapability(t *testing.T) {
	ok := Capability{Role: RoleImage, MinCount: 1, MaxCount: 4}
	if err := ValidateCapability(ok); err != nil {
		t.Errorf("正常能力应通过: %v", err)
	}
	for _, bad := range []Capability{
		{Role: "hologram", MinCount: 1, MaxCount: 1},
		{Role: RoleImage, MinCount: -1, MaxCount: 1},
		{Role: RoleImage, MinCount: 3, MaxCount: 1},
		// 全 0 会让运营以为配了能力其实没配。
		{Role: RoleImage, MinCount: 0, MaxCount: 0},
	} {
		if err := ValidateCapability(bad); err == nil {
			t.Errorf("非法能力应被拒绝: %+v", bad)
		}
	}
}

// ⚠️ GAP-09：协议由代码注册，未注册的协议不可配置。
func TestValidateModelProtocol(t *testing.T) {
	// ⚠️ 这里必须用一个**真的没实现**的协议码。写 kling_video 曾经成立，
	// 但 P06-03 把它实现了 —— 测试跟着注册表走，不跟着当时的举例走。
	if err := ValidateModelProtocol("runway_video", "/submit", "/query"); err == nil {
		t.Fatal("未注册的协议应被拒绝")
	}
	if err := ValidateModelProtocol("openai_chat", "", ""); err == nil {
		t.Fatal("缺 submitPath 应被拒绝")
	}
	// 异步协议缺 queryPath：配置能存、模型能启用，
	// 直到 Worker 要轮询才发现没地方查，而那时任务已扣费。
	err := ValidateModelProtocol("generic_async", "/submit", "")
	if err == nil || !strings.Contains(err.Error(), "queryPath") {
		t.Fatalf("异步协议缺 queryPath 应被拒绝，实际 %v", err)
	}
	if err := ValidateModelProtocol("generic_async", "/submit", "/query"); err != nil {
		t.Errorf("异步协议配齐应通过: %v", err)
	}
	if err := ValidateModelProtocol("openai_chat", "/v1/chat", "/query"); err == nil {
		t.Error("同步协议配 queryPath 应被拒绝")
	}
}

func TestProtocols_注册表非空且有序(t *testing.T) {
	ps := Protocols()
	if len(ps) == 0 {
		t.Fatal("协议注册表不应为空")
	}
	for i := 1; i < len(ps); i++ {
		if ps[i-1].Code >= ps[i].Code {
			t.Error("协议列表应按 code 排序")
		}
	}
	if _, ok := LookupProtocol("generic_async"); !ok {
		t.Error("generic_async 应已注册")
	}
}

// FE-07：发布页要直接列出阻塞原因，而不是只给"不可发布"。
func TestCheckPublishable(t *testing.T) {
	full := PublishInput{
		ModelEnabled: true, ProviderReady: true,
		Protocol: "openai_images", SubmitPath: "/v1/images",
		EnabledOps:     []string{"text2img"},
		OpsWithPricing: map[string]bool{"text2img": true},
		OpsWithCaps:    map[string]bool{"text2img": true},
		MemberLevelOK:  true,
	}
	if b := CheckPublishable(full); len(b) != 0 {
		t.Fatalf("配齐后应无阻塞，实际 %+v", b)
	}

	codesOf := func(in PublishInput) map[string]bool {
		m := map[string]bool{}
		for _, b := range CheckPublishable(in) {
			m[b.Code] = true
		}
		return m
	}

	noProvider := full
	noProvider.ProviderReady = false
	if !codesOf(noProvider)["provider_not_ready"] {
		t.Error("Provider 未就绪应被列出")
	}

	noOps := full
	noOps.EnabledOps = nil
	if !codesOf(noOps)["no_operation"] {
		t.Error("无启用操作应被列出")
	}

	// 启用的能力缺价 → 报价接口会失败，用户点生成时才看到错误。
	noPrice := full
	noPrice.OpsWithPricing = map[string]bool{}
	if !codesOf(noPrice)["pricing_missing"] {
		t.Error("缺价应被列出")
	}

	// 缺能力矩阵 → 无法在扣费前校验输入（AC-10）。
	noCaps := full
	noCaps.OpsWithCaps = map[string]bool{}
	if !codesOf(noCaps)["capability_missing"] {
		t.Error("缺能力矩阵应被列出")
	}

	badProto := full
	badProto.Protocol = "generic_async"
	badProto.QueryPath = ""
	if !codesOf(badProto)["protocol_invalid"] {
		t.Error("协议配置非法应被列出")
	}

	// 多个问题要**一次性全报**，不能只报第一个 —— 否则运营得改一次试一次。
	allBad := PublishInput{Protocol: "openai_images", SubmitPath: "/x"}
	if got := CheckPublishable(allBad); len(got) < 3 {
		t.Errorf("多个问题应一次性全部列出，实际只有 %d 条", len(got))
	}
}

func TestCheckAgentCapable(t *testing.T) {
	cases := []struct {
		domain, proto string
		ok            bool
	}{
		{"text", "openai_chat", true},
		{"text", "anthropic_messages", true},
		{"text", "generic_sync", false},
		{"image", "openai_chat", false},
		{"text", "no_such_protocol", false},
	}
	for _, c := range cases {
		err := CheckAgentCapable(c.domain, c.proto)
		if (err == nil) != c.ok {
			t.Errorf("%s/%s: 期望 ok=%v，得到 %v", c.domain, c.proto, c.ok, err)
		}
	}
}

// 发布检查与报价共用同一条继承规则：agent_chat 自己没声明能力时沿用 text2text。
func TestInheritCapabilityFlags(t *testing.T) {
	got := InheritCapabilityFlags(map[string]bool{"text2text": true})
	if !got["agent_chat"] || !got["text2text"] {
		t.Fatalf("agent_chat 应继承 text2text 的能力声明：%v", got)
	}
	// 被继承方没声明时不凭空补上。
	if got := InheritCapabilityFlags(map[string]bool{"text2img": true}); got["agent_chat"] {
		t.Fatalf("text2text 没声明时 agent_chat 不该算已声明：%v", got)
	}
	// 自己声明了的不受影响；入参不被改写。
	in := map[string]bool{"agent_chat": true}
	if got := InheritCapabilityFlags(in); !got["agent_chat"] || len(in) != 1 {
		t.Fatalf("自己声明的应保留且不改入参：%v / %v", got, in)
	}
	// 发布检查据此不再给 agent_chat 报 capability_missing。
	blockers := CheckPublishable(PublishInput{
		ProviderReady: true, Protocol: "openai_chat", SubmitPath: "/chat/completions",
		EnabledOps:     []string{"text2text", "agent_chat"},
		OpsWithPricing: map[string]bool{"text2text": true, "agent_chat": true},
		OpsWithCaps:    InheritCapabilityFlags(map[string]bool{"text2text": true}),
		MemberLevelOK:  true,
	})
	for _, b := range blockers {
		if b.Code == "capability_missing" {
			t.Fatalf("继承后不该再有 capability_missing：%+v", blockers)
		}
	}
}
