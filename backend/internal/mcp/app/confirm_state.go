package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// elicitation 的 requestState（MCPT-10，MCP-F-43）：`base64url(quoteId|exp) + "." + base64url(HMAC-SHA256)`。
//
// 为什么要签名：requestState 由客户端原样回传，规范要求「未认证的服务端必须加密、签名并校验」。
// 不签的话客户端（或被注入的模型）可以把一个从没给用户看过的 quoteId 塞进 state、再伪造一个 accept 回答。
// 签名只证明「这个 quoteId 是我们为本次确认签发的」—— quote 本身属于哪个授权、金额与指纹，
// 仍由同一个 Verify 判定（判定只有一份）。
//
// 密钥从 SEEAITV_SECRET_KEY **派生子密钥**（DeriveStateKey），不新增环境变量：API 多实例共用同一份
// 材料，签发与校验可以落在不同实例上。go-sdk v1.8.0 不签 requestState（它只原样转发，见 mrtr.go），所以自己签。
// 密钥轮换后在途的 state 校验失败 —— 用户重新点一次确认即可（state 寿命 = quote 有效期，默认 10 分钟）。

// stateKeyLabel 是派生子密钥的用途标签：同一份主密钥派生出的其他子密钥（如果将来有）与它互不相同。
const stateKeyLabel = "seeaitv/mcp/elicitation-request-state/v1"

// ErrBadState requestState 被篡改、格式不对或已过期。
var ErrBadState = errors.New("requestState 无效或已过期")

// DeriveStateKey 从主密钥材料派生 requestState 的 HMAC 子密钥：HMAC-SHA256(material, label)。
// 直接拿主密钥当 HMAC 密钥的话，同一份材料就同时用在了「加密落库凭据」与「签发对外可见的 token」两处。
func DeriveStateKey(material string) []byte {
	m := hmac.New(sha256.New, []byte(material))
	m.Write([]byte(stateKeyLabel))
	return m.Sum(nil)
}

// SignState 签发 requestState。
func SignState(key []byte, quoteID int64, exp time.Time) string {
	payload := strconv.FormatInt(quoteID, 10) + "|" + strconv.FormatInt(exp.Unix(), 10)
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(stateMAC(key, payload))
}

// VerifyState 校验签名与过期，返回 quoteId。
func VerifyState(key []byte, state string, now time.Time) (int64, error) {
	if len(key) == 0 {
		return 0, ErrBadState
	}
	body, sig, ok := strings.Cut(state, ".")
	if !ok {
		return 0, ErrBadState
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(body)
	if err != nil {
		return 0, ErrBadState
	}
	mac, err := enc.DecodeString(sig)
	if err != nil || !hmac.Equal(mac, stateMAC(key, string(payload))) {
		return 0, ErrBadState
	}
	idPart, expPart, ok := strings.Cut(string(payload), "|")
	if !ok {
		return 0, ErrBadState
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrBadState
	}
	exp, err := strconv.ParseInt(expPart, 10, 64)
	if err != nil || !now.Before(time.Unix(exp, 0)) {
		return 0, ErrBadState
	}
	return id, nil
}

func stateMAC(key []byte, payload string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}
