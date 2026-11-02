package projectpkg

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckEntryName(t *testing.T) {
	ok := []string{"manifest.json", "project.json", "media.json", "data/shot.json", "media/123456789012345678.png"}
	for _, n := range ok {
		if err := CheckEntryName(n); err != nil {
			t.Errorf("%q 应通过：%v", n, err)
		}
	}
	bad := []string{"", "../x", "media/../../etc/passwd", "/etc/passwd", `C:\x`, "C:/x", `media\a.png`,
		"data/a/b.json", "media/a/b.png", "other.txt", "./manifest.json", "data//a.json", "data/../manifest.json"}
	for _, n := range bad {
		if err := CheckEntryName(n); err == nil {
			t.Errorf("%q 应拒绝", n)
		}
	}
}

func TestCheckManifest(t *testing.T) {
	if err := CheckManifest(Manifest{Format: Format, SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := CheckManifest(Manifest{Format: Format, SchemaVersion: 2}); err == nil || !strings.Contains(err.Error(), "升级") {
		t.Fatalf("新大版本应拒绝并说明：%v", err)
	}
	if err := CheckManifest(Manifest{Format: "x", SchemaVersion: 1}); err == nil {
		t.Fatal("别的格式应拒绝")
	}
	if err := CheckManifest(Manifest{Format: Format}); err == nil {
		t.Fatal("缺版本应拒绝")
	}
}

func TestRemap_按值替换且保留未知字段(t *testing.T) {
	raw := []byte(`{"nodes":[{"id":"n1","futureField":{"x":1},"params":{"referenceId":"300000000000000001","count":3},
		"result":{"id":"300000000000000002","status":"success","outputUrl":"/api/v1/generations/300000000000000002/outputs/0"}}],
		"byId":{"300000000000000003":true},"num":300000000000000004,"keep":"999999999999999999","url":"https://x/300000000000000001"}`)
	ids := map[string]string{
		"300000000000000001": "400000000000000001", "300000000000000002": "400000000000000002",
		"300000000000000003": "400000000000000003", "300000000000000004": "400000000000000004",
	}
	out, err := RemapJSON(raw, ids)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"referenceId":"400000000000000001"`, `"id":"400000000000000002"`,
		`"/api/v1/generations/400000000000000002/outputs/0"`, `"400000000000000003":true`, `"num":400000000000000004`,
		`"keep":"999999999999999999"`, `"futureField":{"x":1}`, `"count":3`,
		// 非 /api/ 的地址不改（外部地址里的数字不是本机 ID）
		`"https://x/300000000000000001"`} {
		if !strings.Contains(s, want) {
			t.Errorf("缺 %s：%s", want, s)
		}
	}
	// 往返：反向映射回去与原文语义一致。
	back := map[string]string{}
	for k, v := range ids {
		back[v] = k
	}
	again, _ := RemapJSON(out, back)
	var a, b any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(again, &b)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("往返不一致：\n%s\n%s", ja, jb)
	}
}

func TestCollectIDs(t *testing.T) {
	v, _ := Decode([]byte(`{"a":300000000000000001,"b":"300000000000000002","c":"/api/v1/media/300000000000000003?w=320","d":12,"300000000000000004":1}`))
	got := map[string]struct{}{}
	CollectIDs(v, got)
	for _, id := range []string{"300000000000000001", "300000000000000002", "300000000000000003", "300000000000000004"} {
		if _, ok := got[id]; !ok {
			t.Errorf("缺 %s：%v", id, got)
		}
	}
}

func TestImportName(t *testing.T) {
	taken := map[string]bool{"片子": true, "片子（导入）": true}
	f := func(s string) bool { return taken[s] }
	if got := ImportName("新片", f); got != "新片" {
		t.Error(got)
	}
	if got := ImportName("片子", f); got != "片子（导入 2）" {
		t.Error(got)
	}
	if got := ImportName("  ", f); got != "导入的项目" {
		t.Error(got)
	}
}

func TestFileName(t *testing.T) {
	if got := FileName(`a/b:c`); got != "a_b_c.tabtv.zip" {
		t.Error(got)
	}
	if got := FileName(""); got != "project.tabtv.zip" {
		t.Error(got)
	}
}
