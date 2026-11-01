package app

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 游标编码成不透明字符串。
//
// ⚠️ 刻意不是明文的「时间|id」：游标一旦看起来可读，调用方就会去解析、拼接、
// 甚至手工构造，排序键换一个字段就全线崩。base64 表达的是「这是服务端的内部状态，
// 原样回传即可」。这里不做签名 —— 游标不承载权限，改坏了最多是翻页位置不对，
// 而每条查询本身仍带 workspace_id 过滤。
func encodeCursor(t time.Time, id int64) string {
	raw := strconv.FormatInt(t.UTC().UnixMicro(), 10) + "." + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, err
	}
	parts := strings.SplitN(string(b), ".", 2)
	if len(parts) != 2 {
		return time.Time{}, 0, fmt.Errorf("游标格式非法")
	}
	us, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, 0, err
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, 0, err
	}
	// ⚠️ 必须用微秒：PostgreSQL 的 timestamptz 精度就是微秒，
	// 用毫秒会把同一毫秒内的多条记录压成同一个游标值，翻页时整批漏掉。
	return time.UnixMicro(us).UTC(), id, nil
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
