//go:build integration

// 火山 V4 签名的 Provider（docs/agent/smartcut SCT-32，SCD-38）：后台保存过得了库里的 CHECK、
// 密钥三态照旧、地域与服务从自定义请求头里取出来交给签名，回显里没有任何密钥片段。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	"github.com/li904338203/tabtv/backend/internal/platform/secret"
	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

func volcInput(headers string, key *catalogapp.SecretField, version int) catalogapp.ProviderInput {
	return catalogapp.ProviderInput{
		Code: "volc-music", Name: "火山豆包音乐", AdapterType: "http", BaseURL: "https://open.volcengineapi.com",
		AuthType: runcfg.AuthVolcV4, APIKey: key, Headers: json.RawMessage(headers),
		ConnectTimeoutMS: 5000, RequestTimeoutMS: 60000, IdleTimeoutMS: 30000, MaxRetries: 1,
		Status: "enabled", Version: version,
	}
}

func bizCode(err error) int {
	var be *httpx.BizError
	if errors.As(err, &be) {
		return int(be.Code)
	}
	return 0
}

func TestSaveProvider_火山V4签名(t *testing.T) {
	r, _, next := setup(t)
	svc := catalogapp.New(r, next)
	cipher, err := secret.New(map[int]string{1: "test-key-material"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetSecretCipher(cipher)
	ctx := context.Background()
	headers := `{"X-Volc-Region":"cn-north-1","x-volc-service":"imagination","X-Note":"plain"}`

	// 地域 / 服务缺一样：保存时就拒，不等 Worker 签名时才报错（那时已经扣过费）。
	err = svc.SaveProvider(ctx, 1, volcInput(`{"X-Volc-Region":"cn-north-1"}`,
		&catalogapp.SecretField{Action: "replace", Value: "AKLT1:SK1"}, 0))
	if bizCode(err) != int(errcode.InvalidArgument) || !strings.Contains(err.Error(), runcfg.VolcServiceHeader) {
		t.Fatalf("缺服务应 40001 并点名 %s: %v", runcfg.VolcServiceHeader, err)
	}
	// 密钥不是 AK:SK：同样当场拒。
	err = svc.SaveProvider(ctx, 1, volcInput(headers, &catalogapp.SecretField{Action: "replace", Value: "only-ak"}, 0))
	if bizCode(err) != int(errcode.InvalidArgument) || !strings.Contains(err.Error(), "AccessKey:SecretKey") {
		t.Fatalf("密钥格式不对应 40001: %v", err)
	}

	// 正常保存：库里的 ck_provider_auth 放行 volc_v4（00067）。
	if err := svc.SaveProvider(ctx, 1, volcInput(headers,
		&catalogapp.SecretField{Action: "replace", Value: "AKLT1:SK1"}, 0)); err != nil {
		t.Fatalf("保存 volc_v4 应成功: %v", err)
	}
	list, err := svc.ListProviders(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("列表: %v %+v", err, list)
	}
	view, _ := json.Marshal(list[0])
	if !list[0].HasAPIKey || list[0].AuthType != runcfg.AuthVolcV4 ||
		strings.Contains(string(view), "AKLT1") || strings.Contains(string(view), "SK1") {
		t.Fatalf("回显只说配没配、不带任何密钥片段: %s", view)
	}

	// 跑任务时：地域 / 服务交给签名，不再随请求发出；密钥是解密后的 AK:SK。
	if err := svc.SyncProtocols(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveModel(ctx, 1, catalogapp.ModelInput{
		Code: "volc-bgm", Name: "豆包配乐", ProviderCode: "volc-music", Domain: "audio",
		UpstreamModelCode: "GenBGM", APIProtocol: "generic_sync", SubmitPath: "/?Action=GenBGM",
		EstSeconds: 30, Status: "disabled",
	}); err != nil {
		t.Fatalf("建模型: %v", err)
	}
	cfg, err := svc.ResolveForRun(ctx, "volc-bgm")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthType != runcfg.AuthVolcV4 || cfg.APIKey != "AKLT1:SK1" ||
		cfg.SignRegion != "cn-north-1" || cfg.SignService != "imagination" {
		t.Fatalf("运行配置不对: auth=%s region=%s service=%s", cfg.AuthType, cfg.SignRegion, cfg.SignService)
	}
	if len(cfg.Headers) != 1 || cfg.Headers["X-Note"] != "plain" {
		t.Fatalf("约定键（大小写不敏感）要从发出的请求头里剔掉: %+v", cfg.Headers)
	}
	probe, err := svc.ProviderForProbe(ctx, "volc-music")
	if err != nil || probe.SignRegion != "cn-north-1" || probe.SignService != "imagination" {
		t.Fatalf("连接测试与真实调用同一份解析: %+v %v", probe, err)
	}

	// 「保留」：不带密钥字段改别的，密钥不变。
	if err := svc.SaveProvider(ctx, 1, volcInput(headers, nil, 1)); err != nil {
		t.Fatalf("保留密钥时保存应成功: %v", err)
	}
	if cfg, _ := svc.ResolveForRun(ctx, "volc-bgm"); cfg.APIKey != "AKLT1:SK1" {
		t.Fatalf("「保留」不能改密钥: %q", cfg.APIKey)
	}
	// 「清除」之后启用要被拒（需要认证却没有密钥）。
	err = svc.SaveProvider(ctx, 1, volcInput(headers, &catalogapp.SecretField{Action: "clear"}, 2))
	if bizCode(err) != int(errcode.InvalidArgument) {
		t.Fatalf("清除密钥后仍启用应 40001: %v", err)
	}
}
