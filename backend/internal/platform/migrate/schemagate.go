package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema 版本闸（P14-04）。
//
// ⚠️ 起因：`api` 与 `worker` **启动时完全不看数据库 schema 是第几版**。
// 于是发布时漏跑一次 `migrate up`，进程照常起来、`/healthz` 照样 200，
// 而**每一个碰到新列的请求都 500**（`column ... does not exist`）——
// 健康检查全绿、编排系统认为发布成功，只有用户在报错。
//
// 这是发布环节最容易犯也最难当场看出的一个错：发布流水线里少一步、
// 或者迁移那一步失败了而没人看退出码，表现完全一样。

// ExpectedVersion 返回**这个二进制知道的**最高迁移版本，
// 从它自己 embed 的迁移文件推导 —— 不是写死的常量。
//
// ⚠️ 写死常量的话，加一个迁移而忘了改常量就会让整个闸门静默失效，
// 而那种错误不会有编译失败。从 embed.FS 推导则是"加了文件就自动算进去"。
func ExpectedVersion(fsys fs.FS, dir string) (int64, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return 0, err
	}
	var max int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		// goose 的文件名形如 `00029_log_index_order.sql`，版本号是第一段。
		name := path.Base(e.Name())
		i := strings.IndexByte(name, '_')
		if i <= 0 {
			continue
		}
		v, err := strconv.ParseInt(name[:i], 10, 64)
		if err != nil {
			continue
		}
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return 0, fmt.Errorf("没有从 %q 里找到任何迁移文件", dir)
	}
	return max, nil
}

// AppliedVersion 读数据库里已应用的最高迁移版本。
//
// ⚠️ 直接查 `schema_migration` 而不引入 goose：这段要在 `api`/`worker` 的
// 启动路径上跑，而它们不该为了读一个数字把整个迁移框架链进来。
func AppliedVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var v *int64
	err := pool.QueryRow(ctx,
		`select max(version_id) from schema_migration where is_applied`).Scan(&v)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, nil
	}
	return *v, nil
}

// SchemaState 是一次版本比对的结果。
type SchemaState struct {
	// Expected 二进制知道的最高版本。
	Expected int64
	// Applied 数据库已应用的最高版本。
	Applied int64
}

// Behind 数据库落后于二进制：**必然出问题**，代码要用的列还不存在。
func (s SchemaState) Behind() bool { return s.Applied < s.Expected }

// Ahead 数据库超前于二进制：多半没事，但必须说出来。
func (s SchemaState) Ahead() bool { return s.Applied > s.Expected }

// CheckSchema 比对版本并给出该不该拦。
//
// ⚠️ **两个方向的处置必须不同**，这是整条规则的重点：
//
//   - **数据库落后**（漏跑迁移）→ **拦住，启动失败**。
//     代码要读的列还不存在，放它起来只会让每个请求 500，
//     而健康检查全绿 —— 那是最糟的一种"成功发布"。
//
//   - **数据库超前**（回退了二进制但没回退库）→ **放行，但告警**。
//     这恰恰是**版本回退窗口里的正常状态**（INIT-08）：先回退应用、
//     观察一阵、必要时再回退库。在这里拦住的话，回退本身就做不了 ——
//     而回退是出事时唯一的退路，不能被一条检查挡死。
//
// 所以它返回 (要不要拦, 说明)，由调用方决定怎么用。
func CheckSchema(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir string) (
	state SchemaState, fatal bool, msg string, err error) {
	exp, err := ExpectedVersion(fsys, dir)
	if err != nil {
		return SchemaState{}, false, "", err
	}
	app, err := AppliedVersion(ctx, pool)
	if err != nil {
		return SchemaState{}, false, "", fmt.Errorf("读取已应用迁移版本失败: %w", err)
	}
	state = SchemaState{Expected: exp, Applied: app}
	switch {
	case state.Behind():
		return state, true, fmt.Sprintf(
			"数据库 schema 是第 %d 版，而这个二进制需要第 %d 版 —— "+
				"发布时漏跑了 `migrate up`。现在启动的话进程会是「健康」的，"+
				"而每一个碰到新列的请求都会 500。", app, exp), nil
	case state.Ahead():
		return state, false, fmt.Sprintf(
			"数据库 schema 是第 %d 版，比这个二进制知道的第 %d 版**新**。"+
				"这在版本回退窗口里是正常的（先回退应用、库暂不回退），"+
				"但若不是在回退中，说明部署的是旧制品。", app, exp), nil
	default:
		return state, false, "", nil
	}
}
