//go:build integration

// 支付渠道配置与回调留痕的真库测试（P11-02）。
//
// ⚠️ 这里要证明的两件事**只能在真库里成立**：回调幂等靠
// `uk_callback_event(channel, channel_event_id)` 的唯一冲突（不是"先查再写"），
// 密钥三态靠 `coalesce($n, 旧值)` 的 upsert。内存假实现看不到这两点。
package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/payment/domain"
	"github.com/li904338203/tabtv/backend/internal/payment/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool, gen.Next), pool
}

func str(s string) *string { return &s }

// ⚠️ 密钥三态：nil = 保留、空串 = 清除、有值 = 替换。
// 只用"有值就写"的话，管理端回显的掩码会被当成新密钥存回去 ——
// 而那一刻起这个渠道就再也用不了了，症状是"什么都没改却开始验签失败"。
func TestIntegration_密钥三态(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()

	base := repo.SaveChannelInput{
		Code: domain.ChannelWechat, Name: "微信", MerchantID: "M1",
		BaseURL: "https://api.example.com", NotifyURL: "https://x/cb",
		Status: "enabled", KeyVersion: 1,
		PrivateKey: str("cipher-priv-1"), PlatformKey: str("cipher-plat-1"),
		APIv3Key: str("cipher-v3-1"),
	}
	if err := r.SaveChannel(ctx, base); err != nil {
		t.Fatal(err)
	}

	// 保留：三个都传 nil，密文应原样不动。
	keep := base
	keep.Name = "微信支付"
	keep.PrivateKey, keep.PlatformKey, keep.APIv3Key = nil, nil, nil
	if err := r.SaveChannel(ctx, keep); err != nil {
		t.Fatal(err)
	}
	got, err := r.Channel(ctx, domain.ChannelWechat)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "微信支付" {
		t.Fatalf("名字应更新：%+v", got)
	}
	if got.PrivateKeyCipher != "cipher-priv-1" || got.APIv3KeyCipher != "cipher-v3-1" {
		t.Fatalf("传 nil 应保留原密钥：%+v", got)
	}

	// 替换：只换私钥，另外两个保留。
	rep := keep
	rep.PrivateKey = str("cipher-priv-2")
	if err := r.SaveChannel(ctx, rep); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Channel(ctx, domain.ChannelWechat)
	if got.PrivateKeyCipher != "cipher-priv-2" {
		t.Fatalf("私钥应被替换：%+v", got)
	}
	if got.PlatformKeyCipher != "cipher-plat-1" {
		t.Fatalf("没传的那个应保留：%+v", got)
	}

	// 清除：传空串。
	clr := keep
	clr.APIv3Key = str("")
	if err := r.SaveChannel(ctx, clr); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Channel(ctx, domain.ChannelWechat)
	if got.APIv3KeyCipher != "" {
		t.Fatalf("空串应清除：%+v", got)
	}
}

// ⚠️ 单行业务键：`code` 冲突要**更新同一行**，不能保存一次多一行
// （与渲染水印那张单行配置表同一个教训）。
func TestIntegration_渠道保存是幂等的(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()
	in := repo.SaveChannelInput{
		Code: domain.ChannelAlipay, Name: "支付宝", AppID: "2021",
		BaseURL: "https://openapi.example.com", NotifyURL: "https://x/cb",
		Status: "enabled", KeyVersion: 1,
	}
	for i := 0; i < 3; i++ {
		if err := r.SaveChannel(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx,
		`select count(*) from payment_channel where code=$1`, in.Code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("同一个 code 只该有一行，实际 %d 行", n)
	}
}

// ⚠️ 回调幂等靠**唯一冲突**不是"先查再写"：并发的两个回调
// 各自查一遍都会说"没处理过"，于是权益发两次。
func TestIntegration_回调幂等靠唯一索引(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	rec := domain.CallbackRecord{
		Channel: domain.ChannelWechat, EventID: "evt-1",
		RawPayload: []byte(`{"id":"evt-1"}`), SignatureOK: true,
		Stage: "received", OutTradeNo: "ORD-1", AmountCents: 2900,
	}
	dup, err := r.Record(ctx, rec)
	if err != nil || dup {
		t.Fatalf("首次应成功且不算重复：%v %v", dup, err)
	}
	dup, err = r.Record(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Fatal("同一事件再来一次应报重复")
	}
}

// ⚠️ 并发回调只有一个能真正落库 —— 顺序调用永远先撞上"已存在"，
// 那种测试把唯一索引删掉照样绿。
func TestIntegration_并发回调只有一条落库(t *testing.T) {
	r, pool := setup(t)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	fresh := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			dup, err := r.Record(ctx, domain.CallbackRecord{
				Channel: domain.ChannelAlipay, EventID: "evt-concurrent",
				RawPayload: []byte(`{}`), SignatureOK: true, Stage: "received",
			})
			if err == nil && !dup {
				mu.Lock()
				fresh++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if fresh != 1 {
		t.Fatalf("只应有一次算作新事件，实际 %d 次", fresh)
	}
	var rows int
	_ = pool.QueryRow(ctx, `select count(*) from payment_callback
		where channel_event_id='evt-concurrent'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("库里只该有一行，实际 %d 行", rows)
	}
}

// ⚠️ 验签失败**也要落库留痕**：那正是排查攻击与配置错误的唯一线索。
// 这条要能真的落下去 —— 阶段值必须在 CHECK 约束的取值范围内。
func TestIntegration_各阶段都能落库(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	stages := []string{
		"received", "signature_failed", "order_not_found", "amount_mismatch",
		"not_succeeded", "duplicated", "fulfilled", "fulfil_failed",
	}
	for i, st := range stages {
		if _, err := r.Record(ctx, domain.CallbackRecord{
			Channel: domain.ChannelWechat, EventID: "evt-stage-" + st,
			RawPayload: []byte(`{}`), SignatureOK: st != "signature_failed",
			Stage: st, ErrorMessage: "第 " + string(rune('0'+i)) + " 个",
		}); err != nil {
			t.Fatalf("阶段 %q 应能落库（CHECK 约束与代码常量要一致）：%v", st, err)
		}
	}
	rows, err := r.ListCallbacks(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(stages) {
		t.Fatalf("应有 %d 条，实际 %d 条", len(stages), len(rows))
	}
}

// ⚠️ 回调体可能是 form 表单（支付宝）——`raw_payload` 是 jsonb，
// 直接塞的话整条记录落不下，于是**连留痕都没有了**。
func TestIntegration_非JSON回调体也要能落库(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	// 这里存的是 app 层 safeJSON 包过的形状。
	if _, err := r.Record(ctx, domain.CallbackRecord{
		Channel: domain.ChannelAlipay, EventID: "evt-form",
		RawPayload:  []byte(`{"raw":"out_trade_no=ORD-1&sign=abc"}`),
		SignatureOK: true, Stage: "received", OutTradeNo: "ORD-1",
	}); err != nil {
		t.Fatalf("包过的表单体应能落库：%v", err)
	}
}

// 阶段更新要能按订单号查到 —— 那是排障时的第一个动作。
func TestIntegration_按订单号查回调(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	if _, err := r.Record(ctx, domain.CallbackRecord{
		Channel: domain.ChannelWechat, EventID: "evt-x",
		RawPayload: []byte(`{}`), SignatureOK: true, Stage: "received",
		OutTradeNo: "ORD-9", AmountCents: 2900,
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateStage(ctx, domain.ChannelWechat, "evt-x",
		"amount_mismatch", "订单 2900 分，实收 1 分", 0); err != nil {
		t.Fatal(err)
	}
	rows, err := r.ListCallbacks(ctx, "ORD-9", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("应查到 1 条：%v %d", err, len(rows))
	}
	if rows[0].Stage != "amount_mismatch" {
		t.Fatalf("阶段应更新：%+v", rows[0])
	}
	if rows[0].ErrorMessage == "" {
		t.Fatal("错误原因要留着 —— 对账时要能看出差多少")
	}
}

// 只列启用的渠道（装配适配器时用）。
func TestIntegration_只装配启用的渠道(t *testing.T) {
	r, _ := setup(t)
	ctx := context.Background()
	if err := r.SaveChannel(ctx, repo.SaveChannelInput{
		Code: domain.ChannelWechat, Name: "微信", Status: "enabled", KeyVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveChannel(ctx, repo.SaveChannelInput{
		Code: domain.ChannelAlipay, Name: "支付宝", Status: "disabled", KeyVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	all, _ := r.ListChannels(ctx)
	if len(all) != 2 {
		t.Fatalf("管理端应看到两个，实际 %d", len(all))
	}
	enabled, _ := r.EnabledChannels(ctx)
	if len(enabled) != 1 || enabled[0].Code != domain.ChannelWechat {
		t.Fatalf("只该装配启用的：%+v", enabled)
	}
}
