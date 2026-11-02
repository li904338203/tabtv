package desktop

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql 驱动（seed 与管理员引导用 *sql.DB）

	identitydomain "github.com/li904338203/tabtv/backend/internal/identity/domain"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrapadmin"
	"github.com/li904338203/tabtv/backend/internal/platform/seed"
)

// 本机身份（dev/01 TTD-6/7，修订块 R-8）。
//
// tabtv 没有登录：首次启动建一个本机用户（连同个人工作区与付费主体）和一个本机管理员，
// 每次启动随机生成签名密钥、给两者各签一张长期令牌，由 LocalAuth 中间件补进每个请求的头里。
//
// 这样做而不是「把鉴权中间件换成放行」：来源系统的鉴权、工作区归属、权限位代码一行不改照常生效，
// 校验的就是这个本机用户 —— 越权防线（每条查询都带 workspace_id）原样成立，也不会出现
// 「某条路径没经过身份中间件、userID 为 0」这类只在运行时暴露的问题。

const (
	localNickname = "本机用户"
	localEmail    = "local@tabtv.invalid" // .invalid 是保留域名，不会与任何真实地址冲突
	localAdmin    = "local"
	localWSName   = "我的工作区"

	headerAuth      = "Authorization"
	headerWorkspace = "X-Workspace-Id"
)

// Identity 是本机身份：两张令牌与工作区 ID。
type Identity struct {
	UserID      int64
	AdminID     int64
	WorkspaceID int64
	PayerID     int64
	userToken   string
	adminToken  string
}

// seedAndIdentity 写入结构配置与内置内容（幂等），确保本机用户与管理员存在，并签发令牌。
func seedAndIdentity(ctx context.Context, dsn string, pool *pgxpool.Pool, nextID func() int64) (*Identity, *token.Service, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	if err := seed.Apply(ctx, db, nextID); err != nil {
		return nil, nil, fmt.Errorf("写入内置配置失败：%w", err)
	}
	// 管理员密码随机且不保存：没有任何人需要用它登录，令牌由本进程直接签发。
	if _, err := bootstrapadmin.Apply(ctx, db, nextID, localAdmin, ""); err != nil {
		return nil, nil, fmt.Errorf("建立本机管理员失败：%w", err)
	}

	id := &Identity{}
	repo := identityrepo.New(pool)
	if err := pool.QueryRow(ctx, `select id from admin_user where username = $1`, localAdmin).Scan(&id.AdminID); err != nil {
		return nil, nil, err
	}
	err = pool.QueryRow(ctx, `select id from app_user where email = $1`, localEmail).Scan(&id.UserID)
	if errors.Is(err, sql.ErrNoRows) || isNoRows(err) {
		res, rerr := repo.Register(ctx, identitydomain.User{Email: localEmail, Nickname: localNickname,
			PasswordHash: "!"}, [3]int64{nextID(), nextID(), nextID()}, localWSName)
		if rerr != nil {
			return nil, nil, fmt.Errorf("建立本机用户失败：%w", rerr)
		}
		id.UserID = res.User.ID
	} else if err != nil {
		return nil, nil, err
	}
	if id.WorkspaceID, err = repo.DefaultWorkspace(ctx, id.UserID); err != nil {
		return nil, nil, fmt.Errorf("找不到本机工作区：%w", err)
	}
	if err := pool.QueryRow(ctx, `select payer_account_id from workspace where id = $1`, id.WorkspaceID).
		Scan(&id.PayerID); err != nil {
		return nil, nil, err
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, nil, err
	}
	tokens, err := token.New(token.Config{
		Secret: secret, Issuer: "tabtv",
		// 密钥每次启动重新生成，令牌只活在本进程内：有效期给长，免得长时间开着的窗口中途失效。
		AccessTTL: 10 * 365 * 24 * time.Hour, RefreshTTL: 10 * 365 * 24 * time.Hour,
	})
	if err != nil {
		return nil, nil, err
	}
	var userVer, adminVer int
	if err := pool.QueryRow(ctx, `select token_version from app_user where id = $1`, id.UserID).Scan(&userVer); err != nil {
		return nil, nil, err
	}
	if err := pool.QueryRow(ctx, `select token_version from admin_user where id = $1`, id.AdminID).Scan(&adminVer); err != nil {
		return nil, nil, err
	}
	u, err := tokens.IssueAccess(id.UserID, token.KindUser, userVer, randomJTI(), "")
	if err != nil {
		return nil, nil, err
	}
	a, err := tokens.IssueAccess(id.AdminID, token.KindAdmin, adminVer, randomJTI(), "")
	if err != nil {
		return nil, nil, err
	}
	id.userToken, id.adminToken = u.Token, a.Token
	return id, tokens, nil
}

func isNoRows(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no rows in result set")
}

func randomJTI() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LocalAuth 给每个请求补上本机身份：管理接口（/api/v1/admin/...）用管理员令牌，其余用用户令牌；
// 缺 X-Workspace-Id 时补本机工作区。请求自己带了的不覆盖（测试可以显式指定）。
func (id *Identity) LocalAuth(next http.Handler) http.Handler {
	ws := strconv.FormatInt(id.WorkspaceID, 10)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerAuth) == "" {
			tok := id.userToken
			if strings.HasPrefix(r.URL.Path, "/api/v1/admin/") || r.URL.Path == "/api/v1/admin" {
				tok = id.adminToken
			}
			r.Header.Set(headerAuth, "Bearer "+tok)
		}
		if r.Header.Get(headerWorkspace) == "" {
			r.Header.Set(headerWorkspace, ws)
		}
		next.ServeHTTP(w, r)
	})
}
