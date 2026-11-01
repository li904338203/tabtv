package app

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 访问密钥有效期（MCP-PARAM-05：默认 90 天、可选永久 —— **设计建议值**，未经产品批准）。
const (
	DefaultKeyTTLDays = 90
	MaxKeyTTLDays     = 3650
	maxClientNameLen  = 100
)

// 平台审计的动作名（ObjectType 一律 mcp_grant：撤销的单位是授权）。
const (
	AuditKeyCreate   = "mcp.key.create"
	AuditKeyRevoke   = "mcp.key.revoke"
	AuditGrantRevoke = "mcp.grant.revoke"
	auditObjectGrant = "mcp_grant"
)

// Keys 管理访问密钥与授权（/me/mcp/*）。
type Keys struct {
	store   Store
	members Members
	audit   Auditor
	nextID  func() int64
	rand    io.Reader
	now     func() time.Time
}

// NewKeys 构造。audit 可为 nil（不写审计，仅测试用）。
func NewKeys(store Store, members Members, audit Auditor, nextID func() int64) *Keys {
	return &Keys{store: store, members: members, audit: audit, nextID: nextID, rand: rand.Reader, now: time.Now}
}

// CreateKeyInput 是建密钥的入参。
type CreateKeyInput struct {
	WorkspaceID int64
	ClientName  string
	Scopes      []string
	Toolsets    []string
	// ExpiresInDays 为空取默认 90 天；NeverExpires=true 时为永久（二者不能同时给）。
	ExpiresInDays      *int
	NeverExpires       bool
	RequirePageConfirm bool
}

// KeyCreated 是建密钥的结果。⚠️ Plaintext **只在这里出现一次**：库里只有哈希与前缀，
// 之后任何接口都取不回来（MCP-F-03、MCP-N-01）。
type KeyCreated struct {
	Key       repo.AccessKey
	Plaintext string
}

// CreateKey 建一把访问密钥（及其授权）。
func (s *Keys) CreateKey(ctx context.Context, userID int64, in CreateKeyInput) (KeyCreated, error) {
	name := strings.TrimSpace(in.ClientName)
	if name == "" {
		return KeyCreated{}, httpx.Errf(errcode.InvalidArgument,
			"clientName 必填：它会显示在「已连接的客户端」与画布节点的来源角标上，用来认出是哪个客户端在操作")
	}
	if utf8.RuneCountInString(name) > maxClientNameLen {
		return KeyCreated{}, httpx.Errf(errcode.InvalidArgument, "clientName 不能超过 100 个字符")
	}
	scopes, err := domain.ParseScopes(in.Scopes)
	if err != nil {
		return KeyCreated{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	toolsets, err := domain.ParseToolsets(in.Toolsets)
	if err != nil {
		return KeyCreated{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	var expiresAt *time.Time
	switch {
	case in.NeverExpires && in.ExpiresInDays != nil:
		return KeyCreated{}, httpx.Errf(errcode.InvalidArgument, "expiresInDays 与 neverExpires 只能给一个")
	case in.NeverExpires:
	default:
		days := DefaultKeyTTLDays
		if in.ExpiresInDays != nil {
			days = *in.ExpiresInDays
		}
		if days < 1 || days > MaxKeyTTLDays {
			return KeyCreated{}, httpx.Errf(errcode.InvalidArgument, "expiresInDays 取值 1~3650（要永久有效请用 neverExpires）")
		}
		t := s.now().Add(time.Duration(days) * 24 * time.Hour)
		expiresAt = &t
	}
	if in.WorkspaceID <= 0 {
		return KeyCreated{}, httpx.Errf(errcode.InvalidArgument, "workspaceId 必填")
	}
	// ⚠️ 只能给自己是 active 成员的工作区建：不校验的话谁都能为任意工作区建一把密钥，
	// 虽然每次调用还会再查一次成员资格，但「已连接的客户端」里会出现一个他根本进不去的工作区。
	if _, err := s.members.Resolve(ctx, userID, in.WorkspaceID); err != nil {
		return KeyCreated{}, err
	}
	plain, err := domain.GenerateKey(s.rand)
	if err != nil {
		return KeyCreated{}, httpx.Wrap(errcode.Internal, err)
	}
	k, err := s.store.CreateAccessKey(ctx, repo.NewAccessKey{
		Grant: repo.Grant{ID: s.nextID(), UserID: userID, WorkspaceID: in.WorkspaceID, ClientName: name,
			Scopes: scopes, Toolsets: toolsets, RequirePageConfirm: in.RequirePageConfirm},
		KeyID: s.nextID(), Prefix: domain.DisplayPrefix(plain), KeyHash: domain.HashKey(plain), ExpiresAt: expiresAt,
	})
	if err != nil {
		return KeyCreated{}, httpx.Wrap(errcode.Internal, err)
	}
	// ⚠️ 审计**不含明文与哈希**：审计表不自动删、看的人多，任何一份能还原密钥的东西都不该在里面。
	s.write(ctx, userID, k.Grant, AuditKeyCreate, nil, keyAudit(k))
	return KeyCreated{Key: k, Plaintext: plain}, nil
}

// ListKeys 列出用户的全部访问密钥（不含明文，含已撤销 / 已过期）。
func (s *Keys) ListKeys(ctx context.Context, userID int64) ([]repo.AccessKey, error) {
	ks, err := s.store.ListKeys(ctx, userID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return ks, nil
}

// RevokeKey 撤销一把访问密钥（= 撤销它的授权：访问密钥与授权一一对应）。
func (s *Keys) RevokeKey(ctx context.Context, userID, keyID int64) error {
	k, err := s.store.GetKey(ctx, userID, keyID)
	if err != nil {
		return mapRepoErr(err, "访问密钥")
	}
	before, err := s.store.RevokeGrant(ctx, userID, k.GrantID)
	if err != nil {
		return mapRepoErr(err, "访问密钥")
	}
	s.write(ctx, userID, before, AuditKeyRevoke, keyAudit(k), map[string]any{"status": repo.GrantRevoked})
	return nil
}

// ListGrants 列出用户的全部授权（访问密钥与 OAuth）。
func (s *Keys) ListGrants(ctx context.Context, userID int64) ([]repo.Grant, error) {
	gs, err := s.store.ListGrants(ctx, userID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return gs, nil
}

// RevokeGrant 撤销一条授权（它下面的访问密钥 / 令牌随之失效，下一次调用 401）。
func (s *Keys) RevokeGrant(ctx context.Context, userID, grantID int64) error {
	before, err := s.store.RevokeGrant(ctx, userID, grantID)
	if err != nil {
		return mapRepoErr(err, "授权")
	}
	s.write(ctx, userID, before, AuditGrantRevoke, grantAudit(before), map[string]any{"status": repo.GrantRevoked})
	return nil
}

// KeyStatus 是对外的密钥状态：revoked > expired > active。过期是**算出来的**
// （`now >= expires_at`），不靠作业标记（同导出过期、Banner 排期）。
func KeyStatus(k repo.AccessKey, now time.Time) string {
	if k.Grant.Status != repo.GrantActive {
		return "revoked"
	}
	if k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) {
		return "expired"
	}
	return "active"
}

// Now 是服务的当前时间（transport 算状态用，与过期判定同一个时钟）。
func (s *Keys) Now() time.Time { return s.now() }

func mapRepoErr(err error, what string) error {
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Errf(errcode.NotFound, what+"不存在")
	case errors.Is(err, repo.ErrAlreadyRevoked):
		return httpx.Errf(errcode.InvalidStateTransition, what+"已经撤销过了")
	}
	return httpx.Wrap(errcode.Internal, err)
}

func (s *Keys) write(ctx context.Context, userID int64, g repo.Grant, action string, before, after any) {
	if s.audit == nil {
		return
	}
	s.audit.Write(ctx, audit.Entry{ActorType: audit.ActorUser, ActorID: userID, WorkspaceID: g.WorkspaceID,
		Action: action, ObjectType: auditObjectGrant, ObjectID: g.ID, Before: before, After: after})
}

// keyAudit 是审计里记的密钥信息：只有前缀（界面上本来就显示，用来认出是哪一把），**没有明文与哈希**。
func keyAudit(k repo.AccessKey) map[string]any {
	m := grantAudit(k.Grant)
	m["keyId"] = k.ID
	m["prefix"] = k.Prefix
	if k.ExpiresAt != nil {
		m["expiresAt"] = k.ExpiresAt.UTC().Format(time.RFC3339)
	} else {
		m["expiresAt"] = nil
	}
	return m
}

func grantAudit(g repo.Grant) map[string]any {
	return map[string]any{
		"grantId": g.ID, "clientName": g.ClientName, "source": g.Source, "scopes": g.Scopes,
		"toolsets": g.Toolsets, "requirePageConfirm": g.RequirePageConfirm, "status": g.Status,
	}
}
