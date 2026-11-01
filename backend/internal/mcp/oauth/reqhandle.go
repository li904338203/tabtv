package oauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// 授权请求句柄（req，开发 01 §12 R5）。
//
// /oauth/authorize 校验完客户端参数后，把它们打包成 **HMAC 签名 + 10 分钟过期**的句柄，302 到用户端
// 同意页；同意页再以用户身份把句柄交回 /api/v1/mcp/consent。为什么不落库：用户可能根本不点同意，
// 每个半途放弃的授权都留一行只会把表撑大；签名保证同意页（以及中途的任何人）改不了里面的
// client / redirect_uri / PKCE challenge / resource —— 改一个字节签名就对不上。
//
// ⚠️ 句柄里的内容**不是秘密**（URL 里传，浏览器历史里也有），只是**不可篡改**：没有任何令牌或授权码。

// ConsentRequest 是句柄里打包的授权请求（已经过 /oauth/authorize 的全部校验）。
type ConsentRequest struct {
	ClientID      int64    `json:"c"`
	RedirectURI   string   `json:"r"`
	Scopes        []string `json:"s"`
	State         string   `json:"st,omitempty"`
	CodeChallenge string   `json:"cc"`
	Resource      string   `json:"res"`
	// Issuer 随句柄带走：同意接口经用户端代理调用，它看到的 Host 不是授权服务器自己的。
	Issuer string `json:"iss"`
	Exp    int64  `json:"exp"`
}

var (
	// ErrRequestInvalid：句柄格式不对或签名对不上（被篡改 / 不是本服务签的）。
	ErrRequestInvalid = errors.New("授权请求句柄无效")
	// ErrRequestExpired：句柄已过期。
	ErrRequestExpired = errors.New("授权请求已过期")
)

const reqKeyLabel = "seeaitv/mcp/oauth-consent-request/v1"

// DeriveRequestKey 从主密钥材料（SEEAITV_SECRET_KEY）派生句柄的 HMAC 子密钥。
// 派生而不直接用：同一份主密钥还用于加密落库凭据与 elicitation requestState，各用各的子密钥，
// 一处的签名永远不能拿去另一处冒充。
func DeriveRequestKey(material string) []byte {
	m := hmac.New(sha256.New, []byte(material))
	m.Write([]byte(reqKeyLabel))
	return m.Sum(nil)
}

// SignRequest 签发句柄：base64url(JSON) + "." + base64url(HMAC)。
func SignRequest(key []byte, req ConsentRequest) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	payload := enc.EncodeToString(b)
	return payload + "." + enc.EncodeToString(reqMAC(key, payload)), nil
}

// ParseRequest 校验签名与过期并解出请求。**先验签再解析**：反过来的话攻击者能控制我们解析什么。
func ParseRequest(key []byte, handle string, now time.Time) (ConsentRequest, error) {
	payload, sig, ok := strings.Cut(handle, ".")
	if !ok || payload == "" || sig == "" || len(key) == 0 {
		return ConsentRequest{}, ErrRequestInvalid
	}
	enc := base64.RawURLEncoding
	got, err := enc.DecodeString(sig)
	if err != nil || !hmac.Equal(got, reqMAC(key, payload)) {
		return ConsentRequest{}, ErrRequestInvalid
	}
	raw, err := enc.DecodeString(payload)
	if err != nil {
		return ConsentRequest{}, ErrRequestInvalid
	}
	var req ConsentRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.ClientID == 0 {
		return ConsentRequest{}, ErrRequestInvalid
	}
	if now.Unix() >= req.Exp {
		return ConsentRequest{}, ErrRequestExpired
	}
	return req, nil
}

func reqMAC(key []byte, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}
