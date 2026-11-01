package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrSchemaTooNew 表示正文的结构版本比后端认识的新：前端比后端新（先发了前端）。
// 拒绝而不是照存：后端不认识的结构，校验与指纹都可能算错，存进去就再也说不清是哪一版写的。
var ErrSchemaTooNew = errors.New("场景结构版本比服务端新，请等服务端升级后再保存")

// Migrate 把正文升级到 CurrentSchemaVersion。
//
// v1 是第一版，所以目前**原样返回**（不重新序列化：入库的必须是请求里的原始字节，
// 未知字段与数字写法都不能变）。将来出现 v2 时，在这里按版本逐级升级。
// 结构版本缺失或不是整数时原样返回，交给 Validate 报具体路径。
func Migrate(raw json.RawMessage) (json.RawMessage, error) {
	var head struct {
		SchemaVersion *float64 `json:"schemaVersion"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		// 不是对象或不是合法 JSON：交给 Validate 报「$」。
		return raw, nil
	}
	if head.SchemaVersion == nil {
		return raw, nil
	}
	v := *head.SchemaVersion
	if v > CurrentSchemaVersion {
		return nil, fmt.Errorf("%w（正文为 %v，服务端为 %d）", ErrSchemaTooNew, v, CurrentSchemaVersion)
	}
	return raw, nil
}
