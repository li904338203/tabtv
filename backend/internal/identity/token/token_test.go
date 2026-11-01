package token

import (
	"strings"
	"testing"
	"time"
)

func svc(t *testing.T) *Service {
	t.Helper()
	s, err := New(DefaultConfig([]byte("test-secret-at-least-32-bytes-long!!")))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIssueParse_往返(t *testing.T) {
	s := svc(t)
	iss, err := s.Issue(123456789, KindUser, TypeAccess, 7, "jti-1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Parse(iss.Token, KindUser, TypeAccess)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := c.SubjectID()
	if id != 123456789 {
		t.Errorf("subject = %d", id)
	}
	if c.TokenVersion != 7 {
		t.Errorf("tokenVersion = %d，期望 7", c.TokenVersion)
	}
	if c.ID != "jti-1" {
		t.Errorf("jti = %q", c.ID)
	}
}

// ⚠️ GO-02：用户 token 绝不能调用管理接口。
func TestParse_用户token不能当管理员用(t *testing.T) {
	s := svc(t)
	iss, _ := s.Issue(1, KindUser, TypeAccess, 0, "j")
	if _, err := s.Parse(iss.Token, KindAdmin, TypeAccess); err != ErrWrongKind {
		t.Fatalf("期望 ErrWrongKind，实际 %v", err)
	}
}

// refresh token 不能当 access 用，否则长期凭证就成了日常凭证。
func TestParse_refresh不能当access用(t *testing.T) {
	s := svc(t)
	iss, _ := s.Issue(1, KindUser, TypeRefresh, 0, "j")
	if _, err := s.Parse(iss.Token, KindUser, TypeAccess); err != ErrWrongKind {
		t.Fatalf("期望 ErrWrongKind，实际 %v", err)
	}
}

func TestParse_过期(t *testing.T) {
	s := svc(t)
	s.nowFn = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	iss, _ := s.Issue(1, KindUser, TypeAccess, 0, "j")
	s.nowFn = time.Now
	if _, err := s.Parse(iss.Token, KindUser, TypeAccess); err != ErrExpired {
		t.Fatalf("期望 ErrExpired，实际 %v", err)
	}
}

func TestParse_签名被篡改(t *testing.T) {
	s := svc(t)
	iss, _ := s.Issue(1, KindUser, TypeAccess, 0, "j")
	parts := strings.Split(iss.Token, ".")
	tampered := parts[0] + "." + parts[1] + ".AAAAdeadbeef"
	if _, err := s.Parse(tampered, KindUser, TypeAccess); err != ErrInvalid {
		t.Fatalf("篡改签名应被拒，实际 %v", err)
	}
}

// ⚠️ 不限定签名算法会让攻击者用 alg=none 伪造 token。
func TestParse_拒绝none算法(t *testing.T) {
	s := svc(t)
	// {"alg":"none","typ":"JWT"} . {"sub":"1","knd":"user","typ":"access"} . 空签名
	forged := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." +
		"eyJzdWIiOiIxIiwia25kIjoidXNlciIsInR5cCI6ImFjY2VzcyJ9."
	if _, err := s.Parse(forged, KindUser, TypeAccess); err == nil {
		t.Fatal("alg=none 的伪造 token 必须被拒绝")
	}
}

func TestParse_换密钥后失效(t *testing.T) {
	a := svc(t)
	iss, _ := a.Issue(1, KindUser, TypeAccess, 0, "j")
	b, _ := New(DefaultConfig([]byte("another-secret-at-least-32-bytes!!!!")))
	if _, err := b.Parse(iss.Token, KindUser, TypeAccess); err != ErrInvalid {
		t.Fatalf("用别的密钥签发的 token 应被拒，实际 %v", err)
	}
}

// 弱密钥必须在启动时就被拒绝，而不是等到线上被爆破。
func TestNew_拒绝过短密钥(t *testing.T) {
	if _, err := New(DefaultConfig([]byte("short"))); err == nil {
		t.Fatal("过短的 JWT 密钥应被拒绝")
	}
}

// access 必须明显短于 refresh：泄漏窗口要小。
func TestConfig_默认有效期(t *testing.T) {
	c := DefaultConfig([]byte("test-secret-at-least-32-bytes-long!!"))
	if c.AccessTTL >= c.RefreshTTL {
		t.Fatal("access 有效期应明显短于 refresh")
	}
	if c.AccessTTL > time.Hour {
		t.Errorf("access 有效期 %v 过长", c.AccessTTL)
	}
}

// access 要带上所属会话（同一对 refresh 的 jti）：会话表只登记 refresh，
// 此前退出登录撤销的是 access 自己的 jti —— 表里没有那一行，什么都没撤销。
func TestIssueAccess_带上所属会话(t *testing.T) {
	s := svc(t)
	iss, err := s.IssueAccess(42, KindUser, 1, "a-1", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Parse(iss.Token, KindUser, TypeAccess)
	if err != nil {
		t.Fatal(err)
	}
	if c.SessionID != "r-1" || c.Session() != "r-1" {
		t.Fatalf("所属会话 = %q / %q，期望 r-1", c.SessionID, c.Session())
	}
	// 老 token（没有 sid）回落到 jti：行为与修之前一致
	old, _ := s.Issue(42, KindUser, TypeAccess, 1, "a-2")
	c2, _ := s.Parse(old.Token, KindUser, TypeAccess)
	if c2.Session() != "a-2" {
		t.Fatalf("没有 sid 时应回落到 jti，得到 %q", c2.Session())
	}
}
