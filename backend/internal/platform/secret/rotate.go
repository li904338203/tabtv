package secret

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// 密钥轮换：把存量密文重写成当前版本（P14-03）。
//
// ⚠️ 没有这一步的话「带版本的密文」只解决了一半问题：新写入用新 key 了，
// 但**旧密文永远停在旧版本**，于是旧 key 永远摘不掉 —— 而摘不掉的旧 key
// 只会一直留在配置里，直到某天有人以为它没用了把它删掉。
//
// ⚠️ 轮换**必须能反复跑**：中途失败（连接断了、被 Ctrl-C）不能留下半套。
// 每一行是独立的一次「读旧密文 → 解 → 用当前 key 加 → 写回」，
// 已经是当前版本的行**直接跳过**，所以重跑是幂等的。
//
// ⚠️ 逐行事务，不是整批一个事务：凭据表行数很少，但整批事务会在失败时
// 把已经重写好的行一起回滚 —— 而重写是幂等的，回滚只是白做。

// cipherColumn 描述一处 text 型密文列。
type cipherColumn struct {
	table  string
	idCol  string
	column string
}

// 所有 text 型密文列。新增密文列时**必须加进这张表**。
//
// ⚠️ 漏掉一列的表现是：轮换报告"全部完成"，而那一列还停在旧版本；
// 等旧 key 被摘掉之后，那一处凭据就永久解不开了 —— 而它多半是
// 支付私钥或某个 Provider 的 key，也就是"平时不动、出事才发现"的那一类。
// `RotateReport.Remaining` 因此**按库里实际的 `v<n>:` 前缀统计**，
// 不按这张表统计：这样漏掉一列会表现为"还有残留"，而不是被一起漏掉。
var cipherColumns = []cipherColumn{
	{"provider", "id", "api_key_cipher"},
	{"payment_channel", "id", "private_key_cipher"},
	{"payment_channel", "id", "platform_key_cipher"},
	{"payment_channel", "id", "api_v3_key_cipher"},
}

// jsonCipherColumn 描述一处 jsonb 里嵌着密文的列（认证配置的 SMTP / 短信凭据）。
type jsonCipherColumn struct {
	table  string
	idCol  string
	column string
}

var jsonCipherColumns = []jsonCipherColumn{
	{"auth_setting", "id", "smtp_config"},
	{"auth_setting", "id", "sms_config"},
}

// RotateReport 是一次轮换的结果。
type RotateReport struct {
	// Rewritten 重写了多少处密文。
	Rewritten int
	// Skipped 已经是当前版本、无需重写的处数。
	Skipped int
	// Remaining 重写后仍不是当前版本的处数。
	//
	// ⚠️ **只有它是 0，才可以把旧 key 从配置里摘掉**。不是 0 就说明还有
	// 某处密文用着旧版本 —— 此刻摘掉旧 key，那处凭据就永久解不开了。
	Remaining int
	// Versions 库里出现过的密文版本分布，用于人工核对。
	Versions map[int]int
}

// Rotate 把全部存量密文重写成 `c` 的当前版本。
//
// ⚠️ `c` 必须**同时持有新旧密钥**（`SEEAITV_SECRET_KEY` + `SEEAITV_SECRET_KEY_V<n>`），
// 否则旧密文解不开 —— 那时这个函数会报错退出而**不会**写坏任何一行。
func Rotate(ctx context.Context, db *sql.DB, c *Cipher) (RotateReport, error) {
	rep := RotateReport{Versions: map[int]int{}}

	for _, col := range cipherColumns {
		n, s, err := rotateTextColumn(ctx, db, c, col)
		if err != nil {
			return rep, fmt.Errorf("%s.%s: %w", col.table, col.column, err)
		}
		rep.Rewritten += n
		rep.Skipped += s
	}
	for _, col := range jsonCipherColumns {
		n, s, err := rotateJSONColumn(ctx, db, c, col)
		if err != nil {
			return rep, fmt.Errorf("%s.%s: %w", col.table, col.column, err)
		}
		rep.Rewritten += n
		rep.Skipped += s
	}

	remaining, versions, err := scanVersions(ctx, db, c.current)
	if err != nil {
		return rep, err
	}
	rep.Remaining = remaining
	rep.Versions = versions
	return rep, nil
}

func rotateTextColumn(ctx context.Context, db *sql.DB, c *Cipher,
	col cipherColumn) (rewritten, skipped int, err error) {
	q := fmt.Sprintf(
		`select %s, %s from %s where %s is not null and %s <> ''`,
		col.idCol, col.column, col.table, col.column, col.column)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return 0, 0, err
	}
	type item struct {
		id     int64
		cipher string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.cipher); err != nil {
			rows.Close()
			return 0, 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	upd := fmt.Sprintf(`update %s set %s = $1 where %s = $2`, col.table, col.column, col.idCol)
	for _, it := range items {
		fresh, changed, err := reencrypt(c, it.cipher)
		if err != nil {
			return rewritten, skipped, fmt.Errorf("id=%d: %w", it.id, err)
		}
		if !changed {
			skipped++
			continue
		}
		if _, err := db.ExecContext(ctx, upd, fresh, it.id); err != nil {
			return rewritten, skipped, err
		}
		rewritten++
	}
	return rewritten, skipped, nil
}

// rotateJSONColumn 重写 jsonb 里所有**看起来是密文**的字符串值。
//
// ⚠️ 判据是 `IsCiphertext`（`v<数字>:` 前缀 + 能解开），不是字段名白名单：
// 认证配置的 JSON 结构由上层决定，在这里维护一份字段名清单等于第二处定义 ——
// 而它会在上层加一个字段时静默失配。能解开就说明它确实是我们加的密。
func rotateJSONColumn(ctx context.Context, db *sql.DB, c *Cipher,
	col jsonCipherColumn) (rewritten, skipped int, err error) {
	q := fmt.Sprintf(`select %s, %s from %s`, col.idCol, col.column, col.table)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return 0, 0, err
	}
	type item struct {
		id   int64
		body []byte
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.body); err != nil {
			rows.Close()
			return 0, 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	upd := fmt.Sprintf(`update %s set %s = $1 where %s = $2`, col.table, col.column, col.idCol)
	for _, it := range items {
		var obj map[string]any
		if err := json.Unmarshal(it.body, &obj); err != nil {
			// 结构不认识就不动它：轮换不该成为一次数据清洗。
			continue
		}
		n, s := rewriteJSONValues(c, obj)
		rewritten += n
		skipped += s
		if n == 0 {
			continue
		}
		out, err := json.Marshal(obj)
		if err != nil {
			return rewritten, skipped, err
		}
		if _, err := db.ExecContext(ctx, upd, out, it.id); err != nil {
			return rewritten, skipped, err
		}
	}
	return rewritten, skipped, nil
}

// rewriteJSONValues 递归重写对象里的密文字符串。
func rewriteJSONValues(c *Cipher, obj map[string]any) (rewritten, skipped int) {
	for k, v := range obj {
		switch t := v.(type) {
		case string:
			ver, ok := parseVersion(t)
			if !ok {
				// 不是密文格式（普通配置项，如 smtp host）—— 跳过，不计数。
				continue
			}
			if ver == c.current {
				skipped++
				continue
			}
			fresh, changed, err := reencrypt(c, t)
			if err != nil || !changed {
				// ⚠️ 这里**不能**把错误吞掉当成"跳过"：解不开意味着对应版本的
				// 旧 key 没配，而那正是必须让运维看见的事。但 JSON 里也可能
				// 恰好有一个形如 `v2:xxx` 的普通字符串 —— 所以只跳过、
				// 由 scanVersions 在最后统一报"还有残留"。
				continue
			}
			obj[k] = fresh
			rewritten++
		case map[string]any:
			n, s := rewriteJSONValues(c, t)
			rewritten += n
			skipped += s
		}
	}
	return rewritten, skipped
}

// reencrypt 解开再用当前版本加密。changed=false 表示它已经是当前版本。
func reencrypt(c *Cipher, ct string) (string, bool, error) {
	ver, ok := parseVersion(ct)
	if !ok {
		// 不是我们的密文格式（普通字符串），原样放过。
		return ct, false, nil
	}
	if ver == c.current {
		return ct, false, nil
	}
	plain, err := c.Decrypt(ct)
	if err != nil {
		// ⚠️ 这里报错要说清是**哪个版本**解不开：几乎总是因为
		// 对应的 SEEAITV_SECRET_KEY_V<n> 没有配。
		return "", false, fmt.Errorf(
			"版本 v%d 的密文解不开（多半是没有配 SEEAITV_SECRET_KEY_V%d）: %w", ver, ver, err)
	}
	fresh, err := c.Encrypt(plain)
	if err != nil {
		return "", false, err
	}
	return fresh, true, nil
}

// parseVersion 从密文前缀取版本号。
func parseVersion(ct string) (int, bool) {
	if !strings.HasPrefix(ct, prefix) {
		return 0, false
	}
	i := strings.IndexByte(ct, ':')
	if i <= len(prefix) {
		return 0, false
	}
	var ver int
	if _, err := fmt.Sscanf(ct[len(prefix):i], "%d", &ver); err != nil {
		return 0, false
	}
	return ver, true
}

// scanVersions 统计库里各版本密文的数量，以及不是当前版本的总数。
//
// ⚠️ 直接扫库而不是复用上面那两张列表：漏登记一列时，这一步仍会把它数出来 ——
// 于是"漏了一列"表现为**还有残留**（旧 key 不能摘），而不是悄悄漏掉。
func scanVersions(ctx context.Context, db *sql.DB, current int) (int, map[int]int, error) {
	versions := map[int]int{}
	remaining := 0

	// 扫所有 text/varchar 列里以 `v<数字>:` 开头的值。
	// information_schema 在这里是**特性不是取巧**：它让"有没有漏一列"
	// 由数据库回答，而不是由这份源码里的一张清单回答。
	const q = `
		select table_name, column_name
		from information_schema.columns
		where table_schema = 'public'
		  and data_type in ('text','character varying')
		  and (column_name like '%_cipher' or column_name = 'api_key_cipher')`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return 0, nil, err
	}
	type col struct{ table, column string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.column); err != nil {
			rows.Close()
			return 0, nil, err
		}
		cols = append(cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	for _, c := range cols {
		q := fmt.Sprintf(`
			select substring(%s from 2 for position(':' in %s) - 2) as ver, count(*)
			from %s
			where %s is not null and %s ~ '^v[0-9]+:'
			group by 1`, c.column, c.column, c.table, c.column, c.column)
		vr, err := db.QueryContext(ctx, q)
		if err != nil {
			return 0, nil, err
		}
		for vr.Next() {
			var verStr string
			var n int
			if err := vr.Scan(&verStr, &n); err != nil {
				vr.Close()
				return 0, nil, err
			}
			var ver int
			if _, err := fmt.Sscanf(verStr, "%d", &ver); err != nil {
				continue
			}
			versions[ver] += n
			if ver != current {
				remaining += n
			}
		}
		vr.Close()
		if err := vr.Err(); err != nil {
			return 0, nil, err
		}
	}

	// jsonb 里嵌着的密文，information_schema 认不出来 —— 只能按登记的列去扫。
	//
	// ⚠️ 所以**新增一处 jsonb 内嵌密文时必须登记进 `jsonCipherColumns`**。
	// 漏登记的表现与 text 列不同：text 列漏登记仍会被上面那段数出来
	// （于是"还有残留"、旧 key 摘不掉），而 jsonb 漏登记是**彻底看不见** ——
	// 轮换报告干干净净，旧 key 被摘掉，那处凭据从此解不开。
	for _, jc := range jsonCipherColumns {
		q := fmt.Sprintf(`select %s from %s`, jc.column, jc.table)
		jr, err := db.QueryContext(ctx, q)
		if err != nil {
			return 0, nil, err
		}
		var bodies [][]byte
		for jr.Next() {
			var body []byte
			if err := jr.Scan(&body); err != nil {
				jr.Close()
				return 0, nil, err
			}
			bodies = append(bodies, body)
		}
		jr.Close()
		if err := jr.Err(); err != nil {
			return 0, nil, err
		}
		for _, body := range bodies {
			var obj map[string]any
			if err := json.Unmarshal(body, &obj); err != nil {
				continue
			}
			countJSONVersions(obj, versions, current, &remaining)
		}
	}
	return remaining, versions, nil
}

// countJSONVersions 递归数 jsonb 里各版本的密文。
func countJSONVersions(obj map[string]any, versions map[int]int, current int, remaining *int) {
	for _, v := range obj {
		switch t := v.(type) {
		case string:
			if ver, ok := parseVersion(t); ok {
				versions[ver]++
				if ver != current {
					*remaining++
				}
			}
		case map[string]any:
			countJSONVersions(t, versions, current, remaining)
		}
	}
}
