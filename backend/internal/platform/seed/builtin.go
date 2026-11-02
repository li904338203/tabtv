package seed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// builtinHash 是一条内置内容的指纹（TT-T-24）：内置内容改了它就变，seed 据此判断要不要更新没被用户改过的行。
// 用 JSON 序列化各字段再哈希：字段顺序固定，任一字段变化都会反映出来。
func builtinHash(fields ...any) string {
	b, _ := json.Marshal(fields)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
