package migrate

import (
	"testing"
	"testing/fstest"
)

// 期望版本从 embed 的迁移文件**推导**，不是写死的常量。
//
// ⚠️ 写死常量的话，加一个迁移而忘了改常量就会让整个闸门静默失效 ——
// 而那种错误没有编译失败，症状是"漏跑迁移也照样启动"，回到修复前的状态。
func TestExpectedVersion_从迁移文件推导(t *testing.T) {
	fsys := fstest.MapFS{
		"00001_identity.sql":  {},
		"00029_log_index.sql": {},
		"00007_project.sql":   {},
		"README.md":           {}, // 非 .sql 不算
	}
	v, err := ExpectedVersion(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	if v != 29 {
		t.Fatalf("应取最大版本 29，实际 %d", v)
	}
}

func TestExpectedVersion_没有迁移文件要报错(t *testing.T) {
	if _, err := ExpectedVersion(fstest.MapFS{"a.txt": {}}, "."); err == nil {
		t.Fatal("一个迁移都没有时必须报错 —— 静默返回 0 会让闸门永远放行")
	}
}

// ⚠️ 两个方向的处置**必须不同**，这是整条规则的重点。
func TestSchemaState_两个方向处置不同(t *testing.T) {
	behind := SchemaState{Expected: 29, Applied: 28}
	if !behind.Behind() || behind.Ahead() {
		t.Fatal("库落后应判 Behind")
	}
	ahead := SchemaState{Expected: 28, Applied: 29}
	if !ahead.Ahead() || ahead.Behind() {
		t.Fatal("库超前应判 Ahead")
	}
	same := SchemaState{Expected: 29, Applied: 29}
	if same.Behind() || same.Ahead() {
		t.Fatal("一致时两个都该是 false")
	}
}
