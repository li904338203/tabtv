# 3D 导演台共享 fixture

> 前后端共用（DRD-29，docs/agent/director/dev/02 §6）。改任何一份，都要让读它的两边一起过。

| 文件 | 谁在读 | 怎么改 |
| --- | --- | --- |
| `catalog.json` | Go：`internal/director/domain` 的 `TestCatalog_与共享fixture一致`（与 `catalog.go` 逐项比对）；前端：`utils/directorCatalog.test.ts`（DRT-11） | 由代码生成：先改 `catalog.go`，再 `DIRECTOR_FIXTURE_UPDATE=1 go test ./internal/director/domain/...` 重写，然后同步前端 `utils/directorCatalog.ts` 与 `public/director/poses.json` 的姿势码（DRT-10） |
| `scenes/valid-default.json` | Go：`TestDefaultScene_与fixture逐字相等`；前端：DRT-11 的默认场景测试 | 由代码生成（`DefaultScene()` 经两空格缩进加换行），重写方式同上 |
| `scenes/valid-unknown-fields.json` | Go：`TestValidate_未知字段原样保留`；前端：DRT-11 | 手写：每一层都带额外字段，校验必须放行且原样保留 |
| `scenes/invalid-*.json` | Go：`TestValidate_非法场景报出字段路径`；前端：DRT-11 的保存前校验 | 手写：`{"scene": …, "path": "…"}`，两边都要报同一个 `path` |
| `fingerprint/*.json` | Go：`TestRenderFingerprint_只看画面内容`；前端：DRT-17 的「哪些改动显示过期」展示逻辑 | 手写：`{"before", "after", "same", "why"}`。⚠️ 指纹只在 Go 算（DRD-13），前端不算第二份 |
| `camera/fov.json` | Go：`TestVerticalFOV_横幅与竖幅`（DRT-18 求解器）；前端：`utils/directorCamera.test.ts`（DRT-11） | 手写：`[{focalMm, sensorMm, aspect, vfovDeg}]`，`aspect` 是画幅码字符串。期望值独立算出（不拿任一侧的实现去算），两边按同一份核对 |
| `build/raw-*.txt`、`build/raw-expect.json` | Go：`TestParseDraft_raw用例全部符合期望`（DRT-18）；DRT-30 的假上游 | 手写：模型的原始输出（围栏、杂文、截断等），期望写在 `raw-expect.json`。读取方与各键的含义见 `build/README.md` |
| `build/case-*.json` | Go：`TestSolve_fixture用例全部通过`（DRT-18）；DRT-30 的假上游从这里取草案原样回包 | 手写：`{"why", "scene", "draft", "expect"}`；`draft` 就是模型按契约写的 JSON。默认场景的两个对象是 `source=ai`（可替换），`case-default-scene*.json` 钉住这一点 |
| `pose/*-landmarks.json` | 前端：`utils/directorPose.test.ts`（DRT-23，关键点 → 关节旋转的真实输入） | 不手写：MediaPipe Pose Landmarker 对证据目录里照片的**真实识别结果**（在浏览器里识别后导出 `landmarks` / `worldLandmarks`，做法见 `docs/agent/director/evidence/DRT-23/README.md`）。每份的 `_source` 写明模型版本、照片与照片的许可（公有领域 / CC0，来源见同一份 README）；换照片要一起重导 |

## 约定

- 单位米、Y 轴向上；角色 `rotation.y = 0` 时面朝 +Z；机位 rotation 全 0 时朝 −Z 看（见 `internal/director/domain/scene.go`）。
- 媒体 ID 一律是字符串。
- 错误路径的格式：点号分段，数组用方括号下标，例如 `objects[3].camera.aspect`。
- 角色默认身高取所选模型的 `heightM`，所以 `catalog.json` 的 `defaults.character` 里没有身高。
- 新建场景的默认机位与默认角色记为 `source=ai`：用户没动过之前，第一次 AI 搭景可以替换它们（DR-AC-27）；用户一动就改写为 `user`（DR-F-105）。
