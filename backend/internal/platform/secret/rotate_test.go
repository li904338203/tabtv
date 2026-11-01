package secret

import "testing"

// 轮换的核心前提：**新 Cipher 必须能解开旧版本的密文**。
//
// ⚠️ 这一条此前在**物理上不成立**：两个 main 都写死
// `secret.New(map[int]string{1: keyMaterial}, 1)`，所以换掉 SEEAITV_SECRET_KEY
// 就等于宣称"v1 是新 key"，库里全部 v1 密文当场解不开
// （Provider 密钥、支付商户私钥、SMTP 凭据一起），而错误看起来像数据损坏。
func TestRotate_新密钥必须解得开旧密文(t *testing.T) {
	old, err := New(map[int]string{1: "old-key-material-old-key-material"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := old.Encrypt("sk-provider-secret")
	if err != nil {
		t.Fatal(err)
	}

	// 轮换后：当前是 v2，但 v1 仍在手上。
	rotated, err := New(map[int]string{
		1: "old-key-material-old-key-material",
		2: "new-key-material-new-key-material",
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rotated.Decrypt(ct)
	if err != nil {
		t.Fatalf("轮换后解不开旧密文 —— 这正是「换个密钥所有凭据全废」的那一刻: %v", err)
	}
	if got != "sk-provider-secret" {
		t.Fatalf("明文不对: %q", got)
	}

	// 只给新 key 的话（也就是修复前的行为）必须解不开 —— 钉住"为什么要带旧 key"。
	onlyNew, err := New(map[int]string{2: "new-key-material-new-key-material"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onlyNew.Decrypt(ct); err == nil {
		t.Fatal("只带新密钥竟然解开了旧密文 —— 那说明版本前缀没起作用")
	}
}

// 重写之后必须是当前版本，且明文不变。
func TestReencrypt_重写成当前版本且明文不变(t *testing.T) {
	old, _ := New(map[int]string{1: "old-key-material-old-key-material"}, 1)
	ct, _ := old.Encrypt("smtp-password")

	c, _ := New(map[int]string{
		1: "old-key-material-old-key-material",
		2: "new-key-material-new-key-material",
	}, 2)

	fresh, changed, err := reencrypt(c, ct)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("v1 的密文应当被重写")
	}
	if ver, _ := parseVersion(fresh); ver != 2 {
		t.Fatalf("重写后应是 v2，实际 v%d", ver)
	}
	plain, err := c.Decrypt(fresh)
	if err != nil || plain != "smtp-password" {
		t.Fatalf("重写后明文变了: %q %v", plain, err)
	}
}

// 幂等：已经是当前版本的不动它。
//
// ⚠️ 这条决定了 `rotate-secrets` 能不能重跑。中断了必须能直接再来一次，
// 否则运维在一次失败之后就只剩"手工挑出哪些已经重写过"这一条路。
func TestReencrypt_已是当前版本则跳过(t *testing.T) {
	c, _ := New(map[int]string{1: "key-material-key-material-32bytes"}, 1)
	ct, _ := c.Encrypt("x")
	fresh, changed, err := reencrypt(c, ct)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("当前版本的密文不该被重写")
	}
	if fresh != ct {
		t.Fatal("跳过时不该改动密文")
	}
}

// 不是密文的普通字符串原样放过（认证配置的 jsonb 里混着 host、port 这类值）。
func TestReencrypt_普通字符串原样放过(t *testing.T) {
	c, _ := New(map[int]string{1: "key-material-key-material-32bytes"}, 1)
	for _, s := range []string{"smtp.example.com", "", "587", "v:notacipher", "vx:1"} {
		got, changed, err := reencrypt(c, s)
		if err != nil || changed || got != s {
			t.Errorf("%q 不该被当成密文: got=%q changed=%v err=%v", s, got, changed, err)
		}
	}
}

// 解不开要说清是**哪个版本**——几乎总是因为对应的旧 key 没配。
func TestReencrypt_解不开时说清缺哪个版本(t *testing.T) {
	old, _ := New(map[int]string{1: "old-key-material-old-key-material"}, 1)
	ct, _ := old.Encrypt("x")

	// 当前是 v3，手上只有 v3 —— v1 解不开。
	c, _ := New(map[int]string{3: "new-key-material-new-key-material"}, 3)
	_, _, err := reencrypt(c, ct)
	if err == nil {
		t.Fatal("缺旧密钥时必须报错，不能静默跳过 —— 跳过等于报告一次假的「轮换完成」")
	}
	if !contains(err.Error(), "SEEAITV_SECRET_KEY_V1") {
		t.Fatalf("错误里要点名缺的是哪个环境变量，实际: %v", err)
	}
}

// jsonb 里的密文要被递归重写，而普通字段不动。
func TestRewriteJSONValues_只重写密文(t *testing.T) {
	old, _ := New(map[int]string{1: "old-key-material-old-key-material"}, 1)
	pwd, _ := old.Encrypt("p@ss")
	c, _ := New(map[int]string{
		1: "old-key-material-old-key-material",
		2: "new-key-material-new-key-material",
	}, 2)

	obj := map[string]any{
		"host":     "smtp.example.com",
		"port":     float64(587),
		"password": pwd,
		"nested":   map[string]any{"token": pwd},
	}
	n, _ := rewriteJSONValues(c, obj)
	if n != 2 {
		t.Fatalf("应重写 2 处（含嵌套），实际 %d", n)
	}
	if obj["host"] != "smtp.example.com" {
		t.Fatal("普通字段被改动了")
	}
	for _, key := range []string{"password"} {
		if ver, _ := parseVersion(obj[key].(string)); ver != 2 {
			t.Fatalf("%s 没有被重写成 v2", key)
		}
	}
	if ver, _ := parseVersion(obj["nested"].(map[string]any)["token"].(string)); ver != 2 {
		t.Fatal("嵌套对象里的密文没有被重写 —— 认证配置的凭据正是嵌在里面的")
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		ver  int
		want bool
	}{
		{"v1:abc", 1, true},
		{"v12:abc", 12, true},
		{"v:abc", 0, false},
		{"abc", 0, false},
		{"v1abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		ver, ok := parseVersion(c.in)
		if ok != c.want || (ok && ver != c.ver) {
			t.Errorf("%q: got (%d,%v) want (%d,%v)", c.in, ver, ok, c.ver, c.want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
