//go:build integration

// 会话续期与退出（P15-05 之后的会话修复）。
//
// ⚠️ 这些缺陷都只能在真库里证明（看的是 auth_session 那几行最后是什么状态）：
//   - 退出登录撤销的是 access 自己的 jti，而会话表只登记 refresh —— 什么都没撤销，
//     退出之后同一对里的 refresh 还能续期整整 30 天；
//   - 后台**没有续期接口**：登录时发了 refresh 也没处用，15 分钟一到就被踢；
//   - 后台没有退出接口：前端清掉本地存储之后，服务端那条会话还活着；
//   - 严格轮换下「服务端已轮换、响应没回到浏览器」（续期途中换页、断网、两个标签页同时续）
//     会把人踢出去 —— 浏览器只剩那张已作废的旧 refresh。
package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	"github.com/li904338203/tabtv/backend/internal/identity/repo"
	"github.com/li904338203/tabtv/backend/internal/identity/token"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

func sessionSvc(t *testing.T) (*identityapp.Service, *token.Service) {
	svc, tokens, _ := sessionSvcPool(t)
	return svc, tokens
}

func sessionSvcPool(t *testing.T) (*identityapp.Service, *token.Service, *pgxpool.Pool) {
	t.Helper()
	pool := testsupport.Fresh(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("Secret123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`insert into app_user (id, email, password_hash, nickname) values (501, 'u@example.com', $1, '用户')`,
		`insert into admin_user (id, username, password_hash, status) values (601, 'ops', $1, 'active')`,
	} {
		if _, err := pool.Exec(context.Background(), q, string(hash)); err != nil {
			t.Fatal(err)
		}
	}
	tokens, err := token.New(token.DefaultConfig([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := id.New(0)
	return identityapp.New(repo.New(pool), tokens, nil, nil, gen.Next), tokens, pool
}

func errCode(err error) errcode.Code {
	if e, ok := err.(*httpx.BizError); ok {
		return e.Code
	}
	return 0
}

// sessionOf 取一张 refresh 所属的会话（它的 jti）。
func sessionOf(t *testing.T, tokens *token.Service, kind token.Kind, refresh string) string {
	t.Helper()
	c, err := tokens.Parse(refresh, kind, token.TypeRefresh)
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// activeSessions 某主体当前有效的会话数。
func activeSessions(t *testing.T, pool *pgxpool.Pool, kind string, actorID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from auth_session where actor_kind = $1 and actor_id = $2 and revoked_at is null`,
		kind, actorID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ageRotation 把一张已轮换的 refresh 的轮换时间往前拨（模拟宽限期已过）。
func ageRotation(t *testing.T, pool *pgxpool.Pool, jti, ago string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`update auth_session set revoked_at = now() - $2::interval where jti = $1`, jti, ago); err != nil {
		t.Fatal(err)
	}
}

func userLogin(t *testing.T, svc *identityapp.Service) identityapp.TokenPair {
	t.Helper()
	p, err := svc.Login(context.Background(), identityapp.LoginInput{Email: "u@example.com", Password: "Secret123"}, "127.0.0.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRefresh_响应丢了也接得上_且不新开会话(t *testing.T) {
	svc, tokens, pool := sessionSvcPool(t)
	ctx := context.Background()
	p1 := userLogin(t, svc)
	// 第一次续期：服务端轮换了，但这个响应「没回到浏览器」
	lost, err := svc.Refresh(ctx, p1.RefreshToken, "127.0.0.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	// 浏览器拿着旧的再续：宽限期内接得上，而且接的是同一个后继会话
	again, err := svc.Refresh(ctx, p1.RefreshToken, "127.0.0.1", "t")
	if err != nil {
		t.Fatalf("宽限期内旧 refresh 应能接上，得到 %v", err)
	}
	if got, want := sessionOf(t, tokens, token.KindUser, again.RefreshToken), sessionOf(t, tokens, token.KindUser, lost.RefreshToken); got != want {
		t.Fatalf("宽限期内应接上同一个后继会话：得到 %s，应为 %s", got, want)
	}
	if n := activeSessions(t, pool, "user", 501); n != 1 {
		t.Fatalf("宽限期补签不该新开会话：有效会话 %d 个", n)
	}
	// 新 access 指向的就是那个会话（退出登录撤销的也是它）
	c, err := tokens.Parse(again.AccessToken, token.KindUser, token.TypeAccess)
	if err != nil || c.Session() != sessionOf(t, tokens, token.KindUser, lost.RefreshToken) {
		t.Fatalf("补签的 access 应指向后继会话：%v %+v", err, c)
	}
	// 补签出来的 refresh 照常能续（正常轮换）
	if _, err := svc.Refresh(ctx, again.RefreshToken, "127.0.0.1", "t"); err != nil {
		t.Fatalf("补签的 refresh 应能正常续期：%v", err)
	}
}

// 轮换的 CAS 本身：同一张只能被轮换一次（第二次什么都不写）。
// 绕过服务层的状态判断直接打仓储，才验得到「两个请求都读到有效、同时去轮换」时后一个会被挡下。
func TestRotateSession_同一张只能轮换一次(t *testing.T) {
	svc, tokens, pool := sessionSvcPool(t)
	ctx := context.Background()
	old := sessionOf(t, tokens, token.KindUser, userLogin(t, svc).RefreshToken)
	r := repo.New(pool)
	next := func(jti string) repo.Session {
		return repo.Session{JTI: jti, ActorKind: "user", ActorID: 501, ExpiresAt: time.Now().Add(time.Hour)}
	}
	if ok, err := r.RotateSession(ctx, old, 9001, next("r-next-1"), "", ""); err != nil || !ok {
		t.Fatalf("第一次轮换应成功：%v %v", ok, err)
	}
	if ok, err := r.RotateSession(ctx, old, 9002, next("r-next-2"), "", ""); err != nil || ok {
		t.Fatalf("同一张第二次轮换应被挡下（CAS），得到 ok=%v err=%v", ok, err)
	}
	if n := activeSessions(t, pool, "user", 501); n != 1 {
		t.Fatalf("被挡下的那次不该留下后继会话：有效会话 %d 个", n)
	}
	head, ok, err := r.ChainHead(ctx, old)
	if err != nil || !ok || head.JTI != "r-next-1" {
		t.Fatalf("链上当前那一张应是第一次轮换出来的：%+v %v %v", head, ok, err)
	}
}

func TestRefresh_宽限期过了旧refresh就不认(t *testing.T) {
	svc, tokens, pool := sessionSvcPool(t)
	ctx := context.Background()
	p1 := userLogin(t, svc)
	if _, err := svc.Refresh(ctx, p1.RefreshToken, "127.0.0.1", "t"); err != nil {
		t.Fatal(err)
	}
	ageRotation(t, pool, sessionOf(t, tokens, token.KindUser, p1.RefreshToken), "31 seconds")
	if _, err := svc.Refresh(ctx, p1.RefreshToken, "127.0.0.1", "t"); errCode(err) != errcode.TokenRevoked {
		t.Fatalf("宽限期已过，旧 refresh 应作废（40103），得到 %v", err)
	}
}

func TestRefresh_退出了的会话没有宽限期(t *testing.T) {
	svc, tokens, _ := sessionSvcPool(t)
	ctx := context.Background()
	p1 := userLogin(t, svc)
	p2, err := svc.Refresh(ctx, p1.RefreshToken, "127.0.0.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := tokens.Parse(p2.AccessToken, token.KindUser, token.TypeAccess)
	if err := svc.Logout(ctx, c2.Session()); err != nil {
		t.Fatal(err)
	}
	// 旧的那张还在宽限期里，但它接到的是一张已退出的会话：不给接
	if _, err := svc.Refresh(ctx, p1.RefreshToken, "127.0.0.1", "t"); errCode(err) != errcode.TokenRevoked {
		t.Fatalf("会话已退出，宽限期也不该接上（40103），得到 %v", err)
	}
	if _, err := svc.Refresh(ctx, p2.RefreshToken, "127.0.0.1", "t"); errCode(err) != errcode.TokenRevoked {
		t.Fatalf("退出之后 refresh 应作废，得到 %v", err)
	}
}

func TestLogout_拿着旧access也能结束整条链(t *testing.T) {
	svc, tokens, pool := sessionSvcPool(t)
	ctx := context.Background()
	p1 := userLogin(t, svc)
	p2, err := svc.Refresh(ctx, p1.RefreshToken, "127.0.0.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	// 手里的还是续期之前那张 access（续期响应丢了 / 别的标签页刚续过）
	c1, _ := tokens.Parse(p1.AccessToken, token.KindUser, token.TypeAccess)
	if err := svc.Logout(ctx, c1.Session()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, p2.RefreshToken, "127.0.0.1", "t"); errCode(err) != errcode.TokenRevoked {
		t.Fatalf("用旧 access 退出也应作废这条链当前那一张，得到 %v", err)
	}
	if n := activeSessions(t, pool, "user", 501); n != 0 {
		t.Fatalf("退出之后不该还有有效会话：%d 个", n)
	}
}

// ⚠️ 顺序调用测不出来：只有真并发才会两个请求同时读到「有效」再各自轮换。
func TestRefresh_同一张refresh并发续期_全部成功且只留一个会话(t *testing.T) {
	svc, tokens, pool := sessionSvcPool(t)
	ctx := context.Background()
	p1 := userLogin(t, svc)

	// 起跑线：全部就位后同时放行，否则 goroutine 实际上是一个接一个跑的，后到的只会看到「已轮换」走宽限期，
	// 把 CAS 整条删掉也照样绿（实测确认）。
	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	pairs := make([]identityapp.TokenPair, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			pairs[i], errs[i] = svc.Refresh(ctx, p1.RefreshToken, "127.0.0.1", "t")
		}()
	}
	close(start)
	wg.Wait()
	sessions := map[string]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("第 %d 个并发续期失败（两个标签页同时续，输的那个不该被踢）：%v", i, errs[i])
		}
		sessions[sessionOf(t, tokens, token.KindUser, pairs[i].RefreshToken)] = true
	}
	if len(sessions) != 1 {
		t.Fatalf("并发续期应落到同一个会话，得到 %d 个：不管谁的响应最后写进 cookie 都得是有效的那一个", len(sessions))
	}
	if got := activeSessions(t, pool, "user", 501); got != 1 {
		t.Fatalf("应只剩 1 个有效会话，得到 %d", got)
	}
}

func TestUserLogout_同一对的refresh随之作废(t *testing.T) {
	svc, tokens := sessionSvc(t)
	ctx := context.Background()
	pair, err := svc.Login(ctx, identityapp.LoginInput{Email: "u@example.com", Password: "Secret123"}, "127.0.0.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := tokens.Parse(pair.AccessToken, token.KindUser, token.TypeAccess)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, claims.Session()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, pair.RefreshToken, "127.0.0.1", "t"); errCode(err) != errcode.TokenRevoked {
		t.Fatalf("退出之后同一对的 refresh 应已作废（40103），得到 %v", err)
	}
}

func TestAdminRefresh_轮换且停用后续不了(t *testing.T) {
	svc, tokens, pool := sessionSvcPool(t)
	ctx := context.Background()
	p1, err := svc.AdminLogin(ctx, identityapp.AdminLoginInput{Username: "ops", Password: "Secret123"}, "127.0.0.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := svc.AdminRefresh(ctx, p1.RefreshToken, "127.0.0.1", "t")
	if err != nil {
		t.Fatalf("后台续期应成功：%v", err)
	}
	if p2.RefreshToken == p1.RefreshToken || p2.AccessToken == "" {
		t.Fatal("续期应返回一对新的（refresh 要轮换）")
	}
	// 新 access 能过后台鉴权（类型对、带着所属会话）
	c2, err := tokens.Parse(p2.AccessToken, token.KindAdmin, token.TypeAccess)
	if err != nil || c2.SessionID == "" {
		t.Fatalf("新 access 应是后台类型且带所属会话：%v %+v", err, c2)
	}
	// 旧 refresh：宽限期内只接得上同一个会话（响应丢了的那一次），过了宽限期就作废
	again, err := svc.AdminRefresh(ctx, p1.RefreshToken, "127.0.0.1", "t")
	if err != nil {
		t.Fatalf("宽限期内旧 refresh 应能接上：%v", err)
	}
	if sessionOf(t, tokens, token.KindAdmin, again.RefreshToken) != sessionOf(t, tokens, token.KindAdmin, p2.RefreshToken) {
		t.Fatal("宽限期内应接上同一个后继会话，不新开会话")
	}
	ageRotation(t, pool, sessionOf(t, tokens, token.KindAdmin, p1.RefreshToken), "31 seconds")
	if _, err := svc.AdminRefresh(ctx, p1.RefreshToken, "127.0.0.1", "t"); errCode(err) != errcode.TokenRevoked {
		t.Fatalf("宽限期过后旧 refresh 应已作废，得到 %v", err)
	}
	// 用户端的 refresh 不能拿来续后台（反之亦然由类型隔离保证）
	up, err := svc.Login(ctx, identityapp.LoginInput{Email: "u@example.com", Password: "Secret123"}, "127.0.0.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdminRefresh(ctx, up.RefreshToken, "127.0.0.1", "t"); errCode(err) != errcode.WrongTokenKind {
		t.Fatalf("用户的 refresh 续不了后台，得到 %v", err)
	}
	// 后台退出：同一对的 refresh 随之作废
	if err := svc.Logout(ctx, c2.Session()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdminRefresh(ctx, p2.RefreshToken, "127.0.0.1", "t"); errCode(err) != errcode.TokenRevoked {
		t.Fatalf("后台退出之后 refresh 应已作废，得到 %v", err)
	}
	// 停用的管理员：哪怕会话还在，也续不了
	p3, err := svc.AdminLogin(ctx, identityapp.AdminLoginInput{Username: "ops", Password: "Secret123"}, "127.0.0.1", "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetAdminStatus(ctx, 0, 601, "disabled", "测试"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdminRefresh(ctx, p3.RefreshToken, "127.0.0.1", "t"); err == nil {
		t.Fatal("停用的管理员不该还能续期")
	}
}
