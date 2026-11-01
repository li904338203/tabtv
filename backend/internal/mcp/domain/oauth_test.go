package domain

import (
	"crypto/rand"
	"testing"
)

// 三种 OAuth 凭据与访问密钥形态互斥：拿错一种（刷新令牌当访问令牌、授权码当 Bearer）在形态上就被挡下。
func TestOAuthToken_形态互斥(t *testing.T) {
	at, _ := GenerateAccessToken(rand.Reader)
	rt, _ := GenerateRefreshToken(rand.Reader)
	ac, _ := GenerateAuthCode(rand.Reader)
	sk, _ := GenerateKey(rand.Reader)
	if !IsAccessToken(at) || IsAccessToken(rt) || IsAccessToken(ac) || IsAccessToken(sk) {
		t.Fatal("访问令牌判定不对")
	}
	if !IsRefreshToken(rt) || IsRefreshToken(at) || !IsAuthCode(ac) || IsAuthCode(rt) {
		t.Fatal("刷新令牌 / 授权码判定不对")
	}
	if _, err := ParseKey(at); err == nil {
		t.Fatal("访问令牌不是访问密钥")
	}
	if IsAccessToken(at[:len(at)-1]) || IsAccessToken(at+"0") || IsAccessToken(at[:10]+"!"+at[11:]) {
		t.Fatal("长度或字符集不对的应拒")
	}
}
