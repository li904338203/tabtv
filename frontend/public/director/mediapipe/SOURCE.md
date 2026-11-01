# MediaPipe 姿势识别文件的来源（DRT-23，DRD-41）

自托管，构建与运行都不访问 Google CDN。重新生成：`node scripts/director-assets.mjs mediapipe`（`--download` 才联网取模型）。

| 文件 | 来源 | 许可 | 核对 |
| --- | --- | --- | --- |
| `wasm/vision_wasm_module_internal.js`、`wasm/vision_wasm_module_internal.wasm` | npm 包 `@mediapipe/tasks-vision@1.1.1` 的 `wasm/` 目录，原样复制 | Apache-2.0 | `utils/directorLicenses.test.ts` 逐字节比对 node_modules 里锁定版本的同名文件 |
| `pose_landmarker_full.task` | Google 官方发布：<https://storage.googleapis.com/mediapipe-models/pose_landmarker/pose_landmarker_full/float16/latest/pose_landmarker_full.task> | Apache-2.0（官方 Model Card「BlazePose GHUM 3D」：LICENSED UNDER Apache License, Version 2.0，<https://storage.googleapis.com/mediapipe-assets/Model%20Card%20BlazePose%20GHUM%203D.pdf>） | 9,398,198 字节，MD5（base64）`WprYiRmyIx0Csf30pUCQ7A==`，与官方存储的 `x-goog-hash` 一致 |
| `LICENSE` | <https://github.com/google-ai-edge/mediapipe/blob/master/LICENSE> | — | Apache License 2.0 全文 |

- 只复制模块版的 wasm：识别跑在 module worker 里，MediaPipe 在那里只能用模块版加载器；经典版与无 SIMD 版（共 26 MB）用不到。
- 升级 `@mediapipe/tasks-vision` 后必须重跑 `mediapipe` 子命令：JS 与 wasm 版本不配套时识别会在运行时失败，而构建照样通过（那条逐字节比对的测试会先红）。
