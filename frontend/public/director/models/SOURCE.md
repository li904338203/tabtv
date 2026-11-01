# 导演台素模的来源与处理

四个人体素模来自 [wonderunit/shot-generator-models](https://github.com/wonderunit/shot-generator-models)，MIT 许可，Copyright (c) 2019 Wonder Unit（许可全文见同目录 `LICENSE`）。人物由 Wonder Unit 自己建模，骨骼用 Mixamo 的自动绑定生成（上游 README「Armature」一节），仓库里没有其他第三方素材的限制说明。

## 用的是哪个文件

全部取自提交 `a6c43cd14c08e93149d6a5248e916c0b40a37b0d`（2019-08-13，「lots of changes」）：

| 素模码 | 上游文件 | blob |
| --- | --- | --- |
| `adult_male` | `characters/essential/adult-male/male-adult-lod.glb` | `dcb2690e57d7be8b55ac8151d6f7ca3a00c829af` |
| `adult_female` | `characters/essential/adult-female/adult-female-lod.glb` | `ea15d9675d70a3c6647841deb8751531eae7f478` |
| `youth_male` | `characters/essential/teen-male/teen-male-lod.glb` | `8f0a8218f8a95cc04cd609e30fba8f2d50c96c49` |
| `youth_female` | `characters/essential/teen-female/teen-female-lod.glb` | `75740513ae84245bafa2159bd1ed55edc8d4f63d` |

⚠️ 成年男**没有用 master 上的版本**：提交 `3f7489f`（2019-08-17）把它改名为 `adult-male-lod.glb` 重新导出，三个体型 morph（Skinny / Muscular / Obese）在那次导出里丢了。两个版本的顶点、索引、蒙皮权重、骨骼与 inverse bind matrices 逐项相同，只有法线略有差别。另外三个文件在 master（`8195e38`）上与 `a6c43cd` 完全相同（blob 一致）。

## 处理步骤（`frontend/scripts/director-assets.mjs models`）

1. 每个模型只留 LOD1（约 6000 面）。LOD0 约 13400 面；群众一阵最多 100 人，用 LOD0 就是 134 万面，而单人特写下两档轮廓肉眼难辨。
2. 四个模型统一换成一个 `MeshStandardMaterial`：颜色 `#9AB0CC`（写入 glTF 时换算成线性值），metalness 0，roughness 0.8。删掉全部贴图与 UV。
3. 保留 67 根骨骼与三个体型 morph。13 根末端骨从 `leaf` / `leaf.001`… 改成标准名（`HeadTop_End`、`LeftHandIndex4`、`LeftToe_End` 等，对照表在 `frontend/utils/directorRig.ts`），实际改名记录写进 GLB 的 `extras.directorAsset.boneRenames`。
4. 坐标：单位米、Y 轴向上、面朝 +Z，上游本来如此，只核对不改。鞋底在上游离地 4~6 mm，整体下移贴到 y = 0，并按 rest 重算 inverse bind matrices，使「绑定姿势 = rest」严格成立。
5. 体积：裁掉删掉 LOD 之后孤立的数据，再量化：法线（含体型 morph 的法线增量）量化成 int8、权重量化成 uint8、骨骼索引存 uint8（KHR_mesh_quantization，three 的 GLTFLoader 原生支持、不需要解码器）。**顶点位置保持 float**：量化位置会把反量化矩阵乘进 inverse bind matrices，`skeleton.pose()` 之后骨架就会走样。不用 meshopt / Draco，它们要在运行时配解码器。
   裁剪与量化由脚本自己实现，只依赖 `@gltf-transform/core` 与 `@gltf-transform/extensions`（MIT）。没有用 `@gltf-transform/functions`：它经 ndarray-pixels 带进 sharp 与 LGPL-3.0 的 libvips，不在许可白名单（MIT / Apache-2.0 / MPL-2.0）里。体型 morph 的法线增量有极少数分量越过 ±1（每个模型 3~27 个），量化时夹取到 ±1。

处理之后：

| 素模码 | 体积 | 面数 | 身高（偏瘦 / 标准 / 偏壮） | 贴地下移 |
| --- | --- | --- | --- | --- |
| `adult_male` | 302 KB | 5992 | 1.799 / 1.800 / 1.799 m | 6.2 mm |
| `adult_female` | 303 KB | 6026 | 1.624 / 1.625 / 1.625 m | 3.9 mm |
| `youth_male` | 302 KB | 6010 | 1.599 / 1.600 / 1.599 m | 5.5 mm |
| `youth_female` | 302 KB | 6012 | 1.600 / 1.600 / 1.601 m | 3.8 mm |

## 重新生成

脚本不下载上游文件。先取到本地，再在 `frontend` 下运行（两次运行的产物逐字节相同）：

```bash
SHA=a6c43cd14c08e93149d6a5248e916c0b40a37b0d
RAW=https://raw.githubusercontent.com/wonderunit/shot-generator-models/$SHA/characters/essential
curl -sSfLo /tmp/am.glb $RAW/adult-male/male-adult-lod.glb
curl -sSfLo /tmp/af.glb $RAW/adult-female/adult-female-lod.glb
curl -sSfLo /tmp/ym.glb $RAW/teen-male/teen-male-lod.glb
curl -sSfLo /tmp/yf.glb $RAW/teen-female/teen-female-lod.glb
node scripts/director-assets.mjs models --adult-male /tmp/am.glb --adult-female /tmp/af.glb --youth-male /tmp/ym.glb --youth-female /tmp/yf.glb
```

`models` 之后会自动跑 `check`：用 three 的 GLTFLoader 在 Node 里加载一遍，核对材质、无贴图、骨骼名、morph、脚底高度、身高、朝向，以及 `skeleton.pose()` 之后骨骼不走样。
