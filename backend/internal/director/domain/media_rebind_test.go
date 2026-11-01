package domain

import (
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestMediaRefs_全景与泼溅(t *testing.T) {
	raw := []byte(`{"schemaVersion":1,
		"background":{"mode":"color","color":"#060608","panorama":{"sourceNodeId":"n1","mediaId":"200","yaw":0,"radius":50}},
		"splat":{"mediaId":"100","scale":1,"visible":true}}`)
	refs, err := MediaRefs(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []MediaRef{{ID: 200, Role: MediaRolePanorama}, {ID: 100, Role: MediaRoleSplat}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("期望 %v，实际 %v（背景是纯色时全景配置仍算引用）", want, refs)
	}
	none, err := MediaRefs(DefaultScene())
	if err != nil || len(none) != 0 {
		t.Fatalf("默认场景不引用媒体，实际 %v / %v", none, err)
	}
	if _, err := MediaRefs([]byte(`{"schemaVersion":1,"splat":{"mediaId":12}}`)); err == nil {
		t.Fatal("数字形态的媒体 ID 应当报错（对外 ID 一律字符串）")
	}
}

func TestMediaIDs_去重排序(t *testing.T) {
	got := MediaIDs([]MediaRef{{ID: 30}, {ID: 4}, {ID: 30}, {ID: 100}, {ID: 0}})
	if want := []string{"4", "30", "100"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("期望 %v，实际 %v", want, got)
	}
	if got := MediaIDs(nil); got == nil || len(got) != 0 {
		t.Fatalf("没有引用时应返回空切片（入库是 '{}' 不是 NULL），实际 %#v", got)
	}
}

func TestMigrate_v1原样返回(t *testing.T) {
	raw := []byte(`{"schemaVersion":1,"future":{"a":1.50}}`)
	out, err := Migrate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(raw) {
		t.Fatalf("v1 应原样返回（字节不变），实际 %s", out)
	}
}

func TestMigrate_结构版本过新拒绝(t *testing.T) {
	if _, err := Migrate([]byte(`{"schemaVersion":2}`)); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("期望 ErrSchemaTooNew，实际 %v", err)
	}
}

func TestRebindSceneIDs_只改导演台节点(t *testing.T) {
	canvas := []byte(`{"nodes":[
		{"id":"n1","type":"director","params":{"sceneId":"11","other":1},"extra":"keep"},
		{"id":"n2","type":"image","params":{"sceneId":"11"}},
		{"id":"n3","type":"document","params":{"docId":"11"}}
	],"edges":[{"id":"e1","source":"n1","target":"n2","order":1}],"futureKey":true}`)
	out, n, err := RebindSceneIDs(canvas, map[int64]int64{11: 22})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("只应改 1 个导演台节点，实际 %d", n)
	}
	var tree struct {
		Nodes []struct {
			Type   string         `json:"type"`
			Params map[string]any `json:"params"`
			Extra  string         `json:"extra"`
		} `json:"nodes"`
		FutureKey bool `json:"futureKey"`
	}
	if err := json.Unmarshal(out, &tree); err != nil {
		t.Fatal(err)
	}
	if tree.Nodes[0].Params["sceneId"] != "22" || tree.Nodes[0].Extra != "keep" {
		t.Fatalf("导演台节点应改成 22 且保留未知字段：%+v", tree.Nodes[0])
	}
	if tree.Nodes[1].Params["sceneId"] != "11" {
		t.Fatal("别的节点里恰好叫 sceneId 的键不该被改")
	}
	if !tree.FutureKey {
		t.Fatal("画布顶层的未知字段丢了")
	}
}

func TestRebindSceneIDs_不在映射里的原样保留(t *testing.T) {
	canvas := []byte(`{"nodes":[{"id":"n1","type":"director","params":{"sceneId":"99"}}]}`)
	out, n, err := RebindSceneIDs(canvas, map[int64]int64{11: 22})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || string(out) != string(canvas) {
		t.Fatalf("映射里没有的 sceneId 应原样保留（字节不变），实际 n=%d %s", n, out)
	}
}

// TestDomain_不依赖数据库与网络：domain 是纯函数（同 agent/domain）。
//
// 唯一允许的内部依赖是 `platform/lenientjson`（DRT-18 从脚本域下沉的容错解析）：它是只依赖标准库的
// 叶子包（它自己的 `TestPackage_只依赖标准库` 钉住），`-deps` 是传递的，它若引入了库或网络这里照样会红。
func TestDomain_不依赖数据库与网络(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list 不可用：%v", err)
	}
	allowed := map[string]bool{
		"github.com/li904338203/tabtv/backend/internal/director/domain":      true,
		"github.com/li904338203/tabtv/backend/internal/platform/lenientjson": true,
	}
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "database/sql", dep == "net/http", strings.Contains(dep, "jackc/pgx"),
			strings.HasPrefix(dep, "github.com/li904338203/tabtv/backend/internal/") && !allowed[dep]:
			t.Errorf("domain 包依赖了 %s", dep)
		}
	}
}
