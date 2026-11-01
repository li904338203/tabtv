package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// touchInterval 是 last_used_at 的写入节流：同一授权一分钟内最多写一次。
// 每次调用都写的话，一个跑脚本的客户端每秒几十次 update 同一行，而这一列只是给人看
// 「最近什么时候用过」，精确到分钟足够。
const touchInterval = time.Minute

// Resolver 把凭据解析成 Principal（开发 01 §4、MT-4）。每个 /mcp 请求调一次，**不缓存**。
type Resolver struct {
	store   Store
	users   Users
	members Members
	levels  MemberLevels
	log     *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	touched map[int64]time.Time // 进程内的节流（SQL 里还有一道，多实例也成立）
	wg      sync.WaitGroup
}

// NewResolver 构造凭据解析器。levels 可为 nil（等级恒为 0）。
func NewResolver(store Store, users Users, members Members, levels MemberLevels, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{store: store, users: users, members: members, levels: levels, log: log,
		now: time.Now, touched: map[int64]time.Time{}}
}

// Resolve 把 Bearer 值解析成 Principal。错误都是 *httpx.BizError，transport 原样回给客户端：
//
//	不是 sk_mcp_ 形态（含用户端 / 管理端 JWT）→ 401 UNAUTHENTICATED（MCP-F-06）
//	哈希查不到                              → 401 UNAUTHENTICATED
//	授权已撤销                              → 401 TOKEN_REVOKED
//	已过期                                  → 401 TOKEN_EXPIRED
//	账号已封禁                              → 403 ACCOUNT_BANNED
//	已不是该工作区 active 成员               → 403 NOT_WORKSPACE_MEMBER
//
// ⚠️ 账号状态与成员角色**每次都实时查**：成员被移出、降级、账号被封，下一次调用即生效（MCP-AC-03）。
func (r *Resolver) Resolve(ctx context.Context, bearer string) (Principal, error) {
	if strings.HasPrefix(bearer, domain.AccessTokenPrefix) {
		return r.resolveOAuth(ctx, bearer)
	}
	key, err := domain.ParseKey(bearer)
	if err != nil {
		return Principal{}, httpx.Errf(errcode.Unauthenticated,
			"MCP 凭据无效：只接受 MCP 访问密钥（sk_mcp_ 开头）或 MCP OAuth 令牌，网页登录令牌不能用于 MCP")
	}
	k, err := r.store.FindByHash(ctx, domain.HashKey(key))
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return Principal{}, httpx.Errf(errcode.Unauthenticated, "MCP 访问密钥不存在或已被删除")
		}
		return Principal{}, httpx.Wrap(errcode.Internal, err)
	}
	g := k.Grant
	if g.Status != repo.GrantActive {
		return Principal{}, httpx.Errf(errcode.TokenRevoked, "该 MCP 访问密钥已被撤销，请新建一把")
	}
	if k.ExpiresAt != nil && !r.now().Before(*k.ExpiresAt) {
		return Principal{}, httpx.Errf(errcode.TokenExpired, "该 MCP 访问密钥已于 "+
			k.ExpiresAt.UTC().Format(time.RFC3339)+" 过期，请新建一把")
	}
	return r.principalFor(ctx, g)
}

// resolveOAuth 解析 OAuth 访问令牌（MCPT-12）：
//
//	形态不对 / 哈希查不到 / 不是访问令牌 → 401 UNAUTHENTICATED
//	令牌或授权已撤销                     → 401 TOKEN_REVOKED
//	已过期                               → 401 TOKEN_EXPIRED（客户端据此用刷新令牌换新的）
//	受众不是本次请求的端点（RFC 8707）     → 401 UNAUTHENTICATED
//
// 之后与访问密钥**同一段**：账号状态、成员角色、付费主体、会员等级（principalFor）。
//
// ⚠️ 受众按端点规范 URI **精确比对**：/mcp 的令牌不能拿去打 /mcp/x/script（反之亦然）。
// 这是规范要求的「不透传」—— 客户端按它配置的 URL 申请令牌，令牌只对那个资源有效。
// ctx 里没有受众（调用方没经过 MCP 端点）时一律拒：宁可拒错，不可放过。
func (r *Resolver) resolveOAuth(ctx context.Context, bearer string) (Principal, error) {
	if !domain.IsAccessToken(bearer) {
		return Principal{}, httpx.Errf(errcode.Unauthenticated, "MCP OAuth 访问令牌格式不正确")
	}
	t, err := r.store.FindToken(ctx, domain.HashKey(bearer))
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return Principal{}, httpx.Errf(errcode.Unauthenticated, "MCP OAuth 访问令牌不存在")
		}
		return Principal{}, httpx.Wrap(errcode.Internal, err)
	}
	if t.Kind != repo.TokenAccess {
		return Principal{}, httpx.Errf(errcode.Unauthenticated, "这不是访问令牌：刷新令牌只能用于令牌端点换取新的访问令牌")
	}
	if t.RevokedAt != nil || t.Grant.Status != repo.GrantActive {
		return Principal{}, httpx.Errf(errcode.TokenRevoked, "该 MCP 授权已被撤销，请在客户端重新连接 seeAitv")
	}
	if !r.now().Before(t.ExpiresAt) {
		return Principal{}, httpx.Errf(errcode.TokenExpired, "MCP OAuth 访问令牌已过期，请用刷新令牌换取新的")
	}
	aud, ok := AudienceFrom(ctx)
	if !ok || aud == "" || aud != t.Audience {
		return Principal{}, httpx.Errf(errcode.Unauthenticated, "该访问令牌签发给 "+t.Audience+
			"，不能用于本端点（"+aud+"）：请让客户端以本端点地址作为 resource 重新授权").
			WithDetails(map[string]any{"tokenAudience": t.Audience, "endpoint": aud})
	}
	return r.principalFor(ctx, t.Grant)
}

// principalFor 是两种凭据共用的后半段：账号状态、成员角色、付费主体、会员等级，每次都实时查。
func (r *Resolver) principalFor(ctx context.Context, g repo.Grant) (Principal, error) {
	active, err := r.users.Active(ctx, g.UserID)
	if err != nil {
		return Principal{}, err
	}
	if !active {
		return Principal{}, httpx.Err(errcode.AccountBanned)
	}
	m, err := r.members.Resolve(ctx, g.UserID, g.WorkspaceID)
	if err != nil {
		return Principal{}, err
	}
	var level int64
	if r.levels != nil {
		level = r.levels.MemberLevelID(ctx, m.PayerAccountID)
	}
	r.touch(g.ID)
	return Principal{
		UserID: g.UserID, WorkspaceID: g.WorkspaceID, Role: m.Role, PayerAccountID: m.PayerAccountID,
		MemberLevelID: level, GrantID: g.ID, ClientName: g.ClientName,
		Scopes: g.Scopes, Toolsets: g.Toolsets, RequirePageConfirm: g.RequirePageConfirm,
	}, nil
}

// touch 异步写 last_used_at：**独立 context**（请求的 ctx 在响应后就取消了，拿它写库
// 每一次都会 context canceled，CD-64 同一个坑）、失败只 Warn（这一列写不进去不该让调用失败）。
func (r *Resolver) touch(grantID int64) {
	now := r.now()
	r.mu.Lock()
	if last, ok := r.touched[grantID]; ok && now.Sub(last) < touchInterval {
		r.mu.Unlock()
		return
	}
	r.touched[grantID] = now
	if len(r.touched) > 10000 { // 只是节流用的，过期项丢掉不影响正确性（SQL 里还有一道）
		for id, t := range r.touched {
			if now.Sub(t) >= touchInterval {
				delete(r.touched, id)
			}
		}
	}
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := r.store.TouchLastUsed(ctx, grantID); err != nil {
			r.log.Warn("写 MCP 授权最近使用时间失败", "grantId", grantID, "error", err)
		}
	}()
}

// Flush 等待已发起的 last_used_at 写入完成（测试与优雅退出用）。
func (r *Resolver) Flush() { r.wg.Wait() }

// SetClock 替换时钟（测试用）。
func (r *Resolver) SetClock(now func() time.Time) { r.now = now }
