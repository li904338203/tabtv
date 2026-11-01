package transport

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
)

// 火山引擎 OpenAPI 的 V4 签名（docs/agent/smartcut SCD-38：豆包音乐要 AK/SK 签名，不收 API key）。
//
// 规范见 https://www.volcengine.com/docs/6369/67269（2026-10-28 核对，文中的完整算例是本文件单测的向量）：
//
//	CanonicalRequest = 方法 \n 路径 \n 查询串 \n 规范化请求头 \n 签名头列表 \n hex(sha256(正文))
//	StringToSign     = "HMAC-SHA256" \n X-Date \n 日期/地域/服务/request \n hex(sha256(CanonicalRequest))
//	kSigning         = HMAC(HMAC(HMAC(HMAC(SK, 日期), 地域), 服务), "request")
//
// 与 AWS SigV4 同构，差在算法名（HMAC-SHA256）、密钥不加前缀、终止串是 "request"。
//
// ⚠️ 签名覆盖**最终发出的正文**：先签再改正文（比如 multipart 再编码一次）会被上游拒，
// 所以调用方必须在请求体完全定下之后再签（provider/app 的 buildTransportRequest）。
// ⚠️ SK 只在进程内流转：签名结果里只有 AK 与签名值，不含 SK；请求日志本来就不记请求头。

// 地域与服务不是密钥，放在 Provider 的自定义请求头里（明文、后台可见，键名见 runcfg），
// 目录域取出来放进 RunConfig，不再随请求发出。

// VolcCredential 是一次签名要的凭据与作用域。
type VolcCredential struct {
	AccessKey string
	SecretKey string
	Region    string
	Service   string
}

// ParseVolcKey 拆开加密字段里存的 `AK:SK`。AK 里没有冒号，SK 可能有，所以按第一个冒号切。
func ParseVolcKey(key string) (ak, sk string, err error) {
	ak, sk, ok := strings.Cut(strings.TrimSpace(key), ":")
	ak, sk = strings.TrimSpace(ak), strings.TrimSpace(sk)
	if !ok || ak == "" || sk == "" {
		return "", "", errors.New("火山 V4 签名的密钥应为 AccessKey:SecretKey")
	}
	return ak, sk, nil
}

// SignVolcV4 给一次请求签名，返回要加上的请求头：X-Date、X-Content-Sha256、Authorization。
//
// header 是已经定下的请求头；签名覆盖其中的 Content-Type（有的话），以及 Host、X-Date、X-Content-Sha256。
// body 是最终发出的字节（没有正文时传 nil）。
func SignVolcV4(method, rawURL string, header map[string]string, body []byte, cred VolcCredential,
	now time.Time) (map[string]string, error) {
	if cred.AccessKey == "" || cred.SecretKey == "" {
		return nil, errors.New("火山 V4 签名缺少 AccessKey 或 SecretKey")
	}
	if cred.Region == "" || cred.Service == "" {
		return nil, fmt.Errorf("火山 V4 签名缺少地域或服务：在 Provider 的自定义请求头里配置 %s 与 %s",
			runcfg.VolcRegionHeader, runcfg.VolcServiceHeader)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("火山 V4 签名的地址非法: %q", rawURL)
	}
	xDate := now.UTC().Format("20060102T150405Z")
	payload := sha256Hex(body)
	signed := map[string]string{"host": u.Host, "x-date": xDate, "x-content-sha256": payload}
	for k, v := range header {
		if strings.EqualFold(k, "Content-Type") {
			signed["content-type"] = v
		}
	}
	auth := volcAuthorization(strings.ToUpper(method), u, signed, payload, cred, xDate)
	return map[string]string{"X-Date": xDate, "X-Content-Sha256": payload, "Authorization": auth}, nil
}

// volcAuthorization 按给定的签名头集合算出 Authorization 头。signed 的键必须是小写。
//
// 单测用官方算例（只签 host 与 x-date）直接调它：算例与线上请求只差签名头集合。
func volcAuthorization(method string, u *url.URL, signed map[string]string, payloadHash string,
	cred VolcCredential, xDate string) string {
	canonical, signedNames := volcCanonicalRequest(method, u, signed, payloadHash)
	date := xDate[:8]
	scope := date + "/" + cred.Region + "/" + cred.Service + "/request"
	toSign := strings.Join([]string{"HMAC-SHA256", xDate, scope, sha256Hex([]byte(canonical))}, "\n")
	key := volcSigningKey(cred.SecretKey, date, cred.Region, cred.Service)
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	return "HMAC-SHA256 Credential=" + cred.AccessKey + "/" + scope +
		", SignedHeaders=" + signedNames + ", Signature=" + sig
}

// volcCanonicalRequest 拼规范化请求，并给出签名头列表（小写、按字典序、分号连接）。
func volcCanonicalRequest(method string, u *url.URL, signed map[string]string, payloadHash string) (string, string) {
	names := make([]string, 0, len(signed))
	for k := range signed {
		names = append(names, k)
	}
	sort.Strings(names)
	var headers strings.Builder
	for _, k := range names {
		headers.WriteString(k)
		headers.WriteByte(':')
		headers.WriteString(strings.TrimSpace(signed[k]))
		headers.WriteByte('\n')
	}
	signedNames := strings.Join(names, ";")
	return strings.Join([]string{
		method, volcCanonicalPath(u), volcCanonicalQuery(u), headers.String(), signedNames, payloadHash,
	}, "\n"), signedNames
}

func volcSigningKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte(secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "request")
}

// volcCanonicalPath 是按 RFC 3986 编码、保留 `/` 的路径；空路径记 `/`。
func volcCanonicalPath(u *url.URL) string {
	p := u.Path
	if p == "" {
		return "/"
	}
	return uriEncode(p, false)
}

// volcCanonicalQuery 是按键（再按值）排序、键与值各自编码后用 & 连起来的查询串。
func volcCanonicalQuery(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode 是 RFC 3986 的百分号编码：只保留非保留字符（A-Z a-z 0-9 - _ . ~），十六进制用大写。
// ⚠️ 不能用 url.QueryEscape（空格编成 +）也不能用 EscapedPath（保留 `!$&'()*+,;=:@`），两者都会让签名对不上。
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
