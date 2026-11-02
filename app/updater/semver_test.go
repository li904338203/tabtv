package updater

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"0.1.0", "0.1.1", true},
		{"v0.1.1", "v0.1.0", false},
		{"0.1.0", "0.1.0", false},
		{"0.2.0-rc1", "0.2.0", true},
		{"0.2.0", "0.2.0-rc1", false},
		{"0.2.0-rc2", "0.2.0-rc10", true},
		{"0.2.0-rc.2", "0.2.0-rc.10", true},
		{"0.9.9", "1.0.0", true},
		{"1.0.0+abc", "1.0.0", false},
		// 开发构建与本地脏构建不参与比较：不能把开发者手上的程序「升级」掉。
		{"dev", "9.9.9", false},
		{"0.1.0-3-gabc-dirty", "0.2.0", false},
		{"0.1.0", "garbage", false},
		{"0.1", "0.2.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.cur, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v，应为 %v", c.cur, c.latest, got, c.want)
		}
	}
}

func TestParseSums(t *testing.T) {
	const a = "aa0f4c1b2d3e4f5061728394a5b6c7d8e9f00112233445566778899aabbccdde"
	const b = "0000000000000000000000000000000000000000000000000000000000000001"
	got, err := ParseSums(a + "  tabtv-0.2.0-linux-amd64.AppImage\n" + b + " *tabtv-0.2.0-windows-amd64-setup.exe\n\n")
	if err != nil || got["tabtv-0.2.0-linux-amd64.AppImage"] != a || got["tabtv-0.2.0-windows-amd64-setup.exe"] != b {
		t.Fatalf("解析：%v %v", got, err)
	}
	for _, bad := range []string{"", "nothex  file\n", a + "\n", a[:60] + "  f\n"} {
		if _, err := ParseSums(bad); err == nil {
			t.Errorf("应拒绝：%q", bad)
		}
	}
}

func TestAssetName(t *testing.T) {
	if got := AssetName("v0.2.0", "windows", "amd64"); got != "tabtv-0.2.0-windows-amd64-setup.exe" {
		t.Error(got)
	}
	if got := AssetName("0.2.0-rc1", "linux", "arm64"); got != "tabtv-0.2.0-rc1-linux-arm64.AppImage" {
		t.Error(got)
	}
	if AssetName("0.2.0", "darwin", "arm64") != "" {
		t.Error("macOS 不自动替换")
	}
}

func TestDetect(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if d := Detect("darwin", env(nil)); d.Mode != ModeManual {
		t.Error("macOS 只提示")
	}
	if d := Detect("linux", env(nil)); d.Mode != ModeManual {
		t.Error("非 AppImage 的 Linux（.deb）只提示")
	}
	d := Detect("linux", env(map[string]string{"APPIMAGE": "/home/u/Apps/tabtv.AppImage"}))
	if d.Mode != ModeAuto || d.Dir != "/home/u/Apps" || d.AppImage != "/home/u/Apps/tabtv.AppImage" {
		t.Errorf("AppImage 应下载到自身目录：%+v", d)
	}
	if d := Detect("windows", env(nil)); d.Mode != ModeAuto {
		t.Error("Windows 自动安装")
	}
}
