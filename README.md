<p align="center">
  <img src="frontend/public/brand/logo-mark.png" alt="tabtv" width="96">
</p>

<h1 align="center">tabtv</h1>

<p align="center">
  开源的 AI 视频创作桌面应用 · 节点画布一站式完成剧本 → 分镜 → 角色/场景图 → 视频 → 剪辑成片
</p>

<p align="center">
  <a href="https://github.com/li904338203/tabtv/releases/latest"><img src="https://img.shields.io/github/v/release/li904338203/tabtv?label=%E4%B8%8B%E8%BD%BD&color=6d4aff" alt="最新版本"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="Apache-2.0"></a>
  <img src="https://img.shields.io/badge/platform-macOS%20%7C%20Windows%20%7C%20Linux-lightgrey" alt="macOS | Windows | Linux">
  <a href="https://github.com/li904338203/tabtv/actions/workflows/ci.yml"><img src="https://github.com/li904338203/tabtv/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
</p>

<p align="center">
  <b>中文</b> · <a href="README.en.md">English</a>
</p>

tabtv 在节点画布上把文本、图片、视频、音频串起来，调用你自己配置的模型完成从剧本到成片的整条流程。

<p align="center">
  <img src="assets/screenshots/canvas.jpg" alt="tabtv 节点画布：创意描述、角色与场景图连到全能参考视频节点" width="100%">
</p>

- **单机、离线可用**：一个窗口、一个进程，数据库与素材都在本机。只在调用模型和检查更新时联网。
- **没有账号**：不需要注册登录，也没有积分和计价 —— 模型费用由你和模型厂商直接结算。
- **自带 Key**：在设置里填写各厂商的 API Key，Key 只保存在本机数据库里。

## 功能

- **节点画布**：文本 / 图片 / 视频 / 音频节点自由连线，整图一键运行（自动并行、复用未变化的结果）。
- **Agent**：对话式规划与生成，含营销场景（商品库、角色库、批量变体）。
- **脚本分镜**：剧本 → 分镜 → 角色 / 场景 / 道具资产图 → 分镜提示词 → 批量出图出视频。
- **时间线与成片**：剪辑、转场、字幕、按分镜自动装配，用 ffmpeg 在本机渲染成片。
- **智能剪辑与导演台**：自动粗剪；3D 素模摆姿势、定机位。
- **图片工具**：设定图、打光、高清、多角度、涂抹重绘、抠图、宫格切分等。
- **项目导出导入**：项目打包成 zip，带到另一台电脑恢复（包里不含任何 Key）。
- **自动更新**：Windows 与 Linux（AppImage）应用内下载安装，macOS 提示并打开下载页。

## 截图

| 和 AI 一起创作 | 营销 Agent |
| --- | --- |
| <img src="assets/screenshots/create.jpg" alt="创造页：描述想做的视频，Agent 规划并生成" width="100%"> | <img src="assets/screenshots/marketing-agent.jpg" alt="营销 Agent：选商品、角色、创意与 Hook，批量生成推广视频" width="100%"> |
| **节点画布（浅色）** | **模型与厂商：填 Key 即用** |
| <img src="assets/screenshots/canvas-light.jpg" alt="节点画布浅色主题" width="100%"> | <img src="assets/screenshots/settings-models.jpg" alt="设置：内置 OpenAI、Anthropic、可灵、火山方舟等厂商模板" width="100%"> |

## 下载与安装

最新版本：**[v0.1.0](https://github.com/li904338203/tabtv/releases/tag/v0.1.0)**（全部版本见 [Releases](https://github.com/li904338203/tabtv/releases)）

| 平台 | 下载 |
| --- | --- |
| macOS · Apple 芯片 | [tabtv-0.1.0-darwin-arm64.dmg](https://github.com/li904338203/tabtv/releases/download/v0.1.0/tabtv-0.1.0-darwin-arm64.dmg) |
| macOS · Intel | [tabtv-0.1.0-darwin-amd64.dmg](https://github.com/li904338203/tabtv/releases/download/v0.1.0/tabtv-0.1.0-darwin-amd64.dmg) |
| Windows 10/11 · 64 位 | [tabtv-0.1.0-windows-amd64-setup.exe](https://github.com/li904338203/tabtv/releases/download/v0.1.0/tabtv-0.1.0-windows-amd64-setup.exe) |
| Linux · x86_64 | [AppImage](https://github.com/li904338203/tabtv/releases/download/v0.1.0/tabtv-0.1.0-linux-amd64.AppImage) · [deb](https://github.com/li904338203/tabtv/releases/download/v0.1.0/tabtv_0.1.0_amd64.deb) |

校验文件：[SHA256SUMS](https://github.com/li904338203/tabtv/releases/download/v0.1.0/SHA256SUMS)。安装包约 110–135 MB，已包含数据库、ffmpeg 与字体，不需要另装任何东西。

安装包**没有做代码签名**，第一次打开时系统会拦一下，按下面的方法放行即可。

### macOS（12 及以上）

把 `tabtv.app` 拖进「应用程序」，然后**右键点击 → 打开 → 打开**。

如果提示「已损坏，无法打开」，在终端执行：

```bash
xattr -dr com.apple.quarantine /Applications/tabtv.app
```

### Windows（10 / 11，64 位）

运行安装程序时如果出现「Windows 已保护你的电脑」（SmartScreen），点 **更多信息 → 仍要运行**。

应用依赖 WebView2：Windows 11 已自带，Windows 10 缺失时安装程序会引导安装。

### Linux（x86_64，glibc 2.31+）

```bash
chmod +x tabtv-*.AppImage
./tabtv-*.AppImage
```

界面依赖系统的 WebKitGTK 4.0。**播放生成的 H.264 视频还需要 GStreamer 的 `gst-libav` 插件**，缺少时视频区域是黑的（产物本身没有问题）：

| 发行版 | 命令 |
| --- | --- |
| Debian / Ubuntu | `sudo apt install gstreamer1.0-libav gstreamer1.0-plugins-good` |
| Fedora | `sudo dnf install gstreamer1-libav`（需先启用 RPM Fusion） |
| Arch | `sudo pacman -S gst-libav` |

## 第一次使用：配置模型

1. 打开左侧栏的 **设置 → 模型与厂商**。
2. 在「模板」里选一个厂商，填入你的 API Key → **测试连接** → **启用**。模板里的地址都是厂商官方地址，默认全部停用。
3. 回到画布即可生成。

内置模板：OpenAI（GPT、GPT Image）、Anthropic（Claude）、可灵、火山方舟（Seedance）、火山引擎语音识别与 MediaKit、xAI（Grok 视频）、ElevenLabs（音乐）。

**接入自己的中转网关**：在「厂商」里新建厂商（填网关地址与 Key），在「模型」里新建模型并选对应的 API 协议，提交路径会按协议自动填好。新建时直接打开「启用」，应用会按内置模板自动绑好常用操作与输入规则；已有模型可在「操作与参数」里点 **一键推荐配置**。

| API 协议 | 用途 |
| --- | --- |
| OpenAI Chat / Anthropic Messages | 文本、Agent 推理、分镜与提示词 |
| OpenAI Images | 文生图、图生图、资产图 |
| OpenAI Video（`/v1/videos`） | 兼容 OpenAI 视频接口的服务：文生 / 图生 / 首尾帧 / 全能参考 / 视频生视频 |
| Grok 视频、可灵视频、Seedance 视频 | 各家视频接口 |
| OpenAI TTS / 转写、火山语音识别、ElevenLabs 音乐等 | 音频 |
| 通用同步 / 异步 | 兼容本应用请求信封的中转 |

**只收公网地址的上游**（如 OpenAI Video 协议的参考图、参考视频）：在 **设置 → 存储** 里配一个 S3 兼容存储（MinIO、阿里云 OSS、腾讯云 COS、火山 TOS、R2、S3 均可）。参考媒体会先传到那里，再把带签名的临时地址发给上游；本机素材仍保存在本地。

设置页里还有：Agent 技能、提示词、图片预设、营销选项与角色、并发与日志、调用日志、环境自检（数据库、ffmpeg、H.264 编码器、字体、Linux 视频解码插件）。

## 数据保存在哪里

| 平台 | 默认位置 |
| --- | --- |
| macOS | `~/Library/Application Support/tabtv` |
| Windows | `%APPDATA%\tabtv` |
| Linux | `$XDG_DATA_HOME/tabtv`（未设置时为 `~/.local/share/tabtv`） |

目录里有 `pg/`（数据库）、`media/`（素材与产物）、`logs/`（日志）、`backups/`（升级迁移前的数据库备份）。设置 → 常规里可以直接打开这个目录；也可以用环境变量 `TABTV_DATA_DIR` 指定别的位置。

⚠️ 数据库里**明文保存**你填写的 API Key。把数据目录发给别人之前，请先在设置里清掉 Key。

## 从源码构建

环境要求：Go 1.26（`go.mod` 指定；本机 Go 较旧时会自动下载对应工具链）· Node 20.20.0（见 `.nvmrc`）· pnpm 10.32.1 · [Wails v2 CLI](https://wails.io/docs/gettingstarted/installation)。Linux 另需 `libgtk-3-dev libwebkit2gtk-4.0-dev`。

```bash
pnpm install --frozen-lockfile
make pg       # 下载内嵌 PostgreSQL（按 build/third_party.lock 校验）
make fonts    # 下载排版字体（按 SHA-256 校验）

# 构建桌面应用，产物在 build/bin/
wails build                       # macOS / Windows
wails build -tags webkit2_40      # Linux：必须带这个标签，否则请求体传不到后端

# 或者一步打出安装包（含数据库、ffmpeg、字体），产物在 build/dist/
make package
```

发布：推送 `v*` 标签后，GitHub Actions 会在四个平台打包并上传为 Release 草稿。

开发与提交前的门禁：

```bash
make check     # 内部信息扫描 + 格式 + lint + 契约一致 + 类型检查 + 测试 + 构建
make test-it   # 数据库集成测试（用内嵌 PostgreSQL，不需要 docker）
make help      # 全部命令
```

参与开发请先读 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 许可证

tabtv 以 [Apache License 2.0](LICENSE) 发布。

随包分发的第三方组件（PostgreSQL、FFmpeg、字体、导演台 3D 素模与 MediaPipe 等）及其许可证见 [NOTICE](NOTICE)；依赖库的许可清单由 `build/gen-licenses.sh` 生成（出现 GPL / AGPL 依赖即失败）。
