package transport

import (
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
)

// 官方算例（https://www.volcengine.com/docs/6369/67269「签名示例」，2026-10-28 核对）：
// GET billing.volcengineapi.com/?Action=QueryBalanceAcct&Version=2022-01-01，只签 host 与 x-date。
// AK / SK 是文档里公开的示例值，不是真密钥。
var volcExample = struct {
	cred                     VolcCredential
	url, xDate               string
	canonicalHash            string
	kDate, kRegion, kService string
	kSigning, signature      string
}{
	cred: VolcCredential{
		AccessKey: "AKLTYWViMTVmZGYzM2E0NDI5Mzk2MDZjNjFmMjc2MjRjMzg", // scan-internal:allow 官方文档公开示例值
		SecretKey: "WkRZeE1EQmxPVGhsWWpWak5HVmtNbUUxTXpZeU9UVXlOMlE1TmpZeVlqTQ==",
		Region:    "cn-beijing", Service: "billing",
	},
	url:           "https://billing.volcengineapi.com/?Action=QueryBalanceAcct&Version=2022-01-01",
	xDate:         "20250329T180937Z",
	canonicalHash: "43171c1658c64b5db55c58d54988a4598d2d09a5613136beaa5eef40eae6e2c1",
	kDate:         "069b1da2ba9c0ecbd8e8aaf2a5742696ebc22f3fe95a649983d31b433ba94ff3",
	kRegion:       "2f41e8c797f1f0200484c9b0986f89cb371bf44d51e37ca7ad0e12dcbbd83cfd",
	kService:      "2d9caf568d4a1bd052daf42378d281e7f3233c7110dd6849988bd17fd5423777",
	kSigning:      "b491ed164936de3bb06c1eb23326aa9587b5aaa6a4e02144b9d523bbebb7ca9f",
	signature:     "1eda9e7e6b1728151a8e8791fdaf67cfbd28bd5c80d0fce2eb208746cf483105",
}

func TestSignVolcV4_与官方算例逐步一致(t *testing.T) {
	ex := volcExample
	u, err := url.Parse(ex.url)
	if err != nil {
		t.Fatal(err)
	}
	signed := map[string]string{"host": u.Host, "x-date": ex.xDate}
	canonical, names := volcCanonicalRequest("GET", u, signed, sha256Hex(nil))
	wantCanonical := "GET\n/\nAction=QueryBalanceAcct&Version=2022-01-01\nhost:billing.volcengineapi.com\n" +
		"x-date:20250329T180937Z\n\nhost;x-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if canonical != wantCanonical || names != "host;x-date" {
		t.Fatalf("规范化请求不对:\n%s", canonical)
	}
	if got := sha256Hex([]byte(canonical)); got != ex.canonicalHash {
		t.Fatalf("规范化请求的哈希 = %s，官方 %s", got, ex.canonicalHash)
	}

	// 派生链逐级核对：错在哪一级一眼看得出来。
	k := []byte(ex.cred.SecretKey)
	for _, step := range []struct{ name, data, want string }{
		{"kDate", "20250329", ex.kDate},
		{"kRegion", ex.cred.Region, ex.kRegion},
		{"kService", ex.cred.Service, ex.kService},
		{"kSigning", "request", ex.kSigning},
	} {
		k = hmacSHA256(k, step.data)
		if got := hex.EncodeToString(k); got != step.want {
			t.Fatalf("%s = %s，官方 %s", step.name, got, step.want)
		}
	}
	if hex.EncodeToString(volcSigningKey(ex.cred.SecretKey, "20250329", ex.cred.Region, ex.cred.Service)) != ex.kSigning {
		t.Fatal("volcSigningKey 与逐级派生不一致")
	}

	auth := volcAuthorization("GET", u, signed, sha256Hex(nil), ex.cred, ex.xDate)
	want := "HMAC-SHA256 Credential=" + ex.cred.AccessKey + "/20250329/cn-beijing/billing/request" +
		", SignedHeaders=host;x-date, Signature=" + ex.signature
	if auth != want {
		t.Fatalf("Authorization 不对:\n got %s\nwant %s", auth, want)
	}
}

func TestSignVolcV4_签正文与ContentType且正文一变签名就变(t *testing.T) {
	cred := VolcCredential{AccessKey: "AK", SecretKey: "SK", Region: "cn-north-1", Service: "imagination"}
	now := time.Date(2026, 10, 28, 9, 30, 5, 0, time.FixedZone("CST", 8*3600))
	hdr := map[string]string{"Content-Type": "application/json"}
	body := []byte(`{"Text":"轻快的钢琴"}`)
	out, err := SignVolcV4("post", "https://open.volcengineapi.com/?Version=2024-08-12&Action=GenBGM", hdr, body, cred, now)
	if err != nil {
		t.Fatal(err)
	}
	if out["X-Date"] != "20261028T013005Z" {
		t.Fatalf("X-Date 要按 UTC: %q", out["X-Date"])
	}
	if out["X-Content-Sha256"] != sha256Hex(body) {
		t.Fatalf("X-Content-Sha256 要是正文的哈希: %q", out["X-Content-Sha256"])
	}
	if !strings.Contains(out["Authorization"], "SignedHeaders=content-type;host;x-content-sha256;x-date,") ||
		!strings.Contains(out["Authorization"], "Credential=AK/20261028/cn-north-1/imagination/request") {
		t.Fatalf("Authorization 不对: %s", out["Authorization"])
	}
	if strings.Contains(out["Authorization"], "SK") {
		t.Fatal("签名结果里不能有 SK")
	}

	again, _ := SignVolcV4("POST", "https://open.volcengineapi.com/?Action=GenBGM&Version=2024-08-12", hdr, body, cred, now)
	if again["Authorization"] != out["Authorization"] {
		t.Fatal("查询参数顺序不同、方法大小写不同，签名应当一样（规范化之后是同一个请求）")
	}
	changed, _ := SignVolcV4("POST", "https://open.volcengineapi.com/?Action=GenBGM&Version=2024-08-12", hdr,
		[]byte(`{"Text":"轻快的吉他"}`), cred, now)
	if changed["Authorization"] == out["Authorization"] {
		t.Fatal("正文变了签名必须变：签名要覆盖最终发出的正文")
	}
	otherType, _ := SignVolcV4("POST", "https://open.volcengineapi.com/?Action=GenBGM&Version=2024-08-12",
		map[string]string{"content-type": "text/plain"}, body, cred, now)
	if otherType["Authorization"] == out["Authorization"] {
		t.Fatal("Content-Type 也在签名里（大小写不敏感地取）")
	}
}

func TestSignVolcV4_编码按RFC3986(t *testing.T) {
	u, _ := url.Parse("https://x.example.com/a b/c~d?b=2&a=x%20y&a=%2F&c=*")
	if got := volcCanonicalPath(u); got != "/a%20b/c~d" {
		t.Fatalf("路径: %q", got)
	}
	// 键排序、同键的值排序；空格是 %20 不是 +；/ 与 * 都要编码。
	if got := volcCanonicalQuery(u); got != "a=%2F&a=x%20y&b=2&c=%2A" {
		t.Fatalf("查询串: %q", got)
	}
	if got := volcCanonicalPath(&url.URL{Host: "x"}); got != "/" {
		t.Fatalf("空路径记 /: %q", got)
	}
}

func TestSignVolcV4_缺配置时说清楚缺什么(t *testing.T) {
	_, err := SignVolcV4("GET", "https://x.example.com/", nil, nil, VolcCredential{AccessKey: "a", SecretKey: "b"}, time.Now())
	if err == nil || !strings.Contains(err.Error(), runcfg.VolcRegionHeader) ||
		!strings.Contains(err.Error(), runcfg.VolcServiceHeader) {
		t.Fatalf("缺地域 / 服务时要点名请求头: %v", err)
	}
	if _, err := SignVolcV4("GET", "https://x.example.com/", nil, nil, VolcCredential{Region: "r", Service: "s"}, time.Now()); err == nil {
		t.Fatal("缺 AK / SK 要报错")
	}
}

func TestParseVolcKey(t *testing.T) {
	cases := []struct {
		in, ak, sk string
		ok         bool
	}{
		{"AKLT123:c2Vj:cmV0", "AKLT123", "c2Vj:cmV0", true}, // SK 里有冒号：按第一个切
		{" AK : SK ", "AK", "SK", true},
		{"AKonly", "", "", false},
		{":SK", "", "", false},
		{"AK:", "", "", false},
	}
	for _, c := range cases {
		ak, sk, err := ParseVolcKey(c.in)
		if (err == nil) != c.ok || ak != c.ak || sk != c.sk {
			t.Errorf("ParseVolcKey(%q) = %q, %q, %v", c.in, ak, sk, err)
		}
	}
}
