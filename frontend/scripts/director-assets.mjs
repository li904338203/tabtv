#!/usr/bin/env node
/**
 * 3D 导演台的素模与姿势素材构建（DRT-10，DR-F-40~42，DRK-2，DR-N-15）。离线跑，产物提交进仓库。
 *
 * 用法（在 frontend 下）：
 *   node scripts/director-assets.mjs models --adult-male <glb> --adult-female <glb> --youth-male <glb> --youth-female <glb>
 *       输入是上游 wonderunit/shot-generator-models 的 4 个 `*-lod.glb`（本地路径，脚本不下载；
 *       用哪个提交的哪个文件见 public/director/models/SOURCE.md）。产出 public/director/models/{code}.glb，随后自动 check。
 *   node scripts/director-assets.mjs poses     scripts/poses.source.mjs → public/director/poses.json，随后自动 check
 *   node scripts/director-assets.mjs rest      从 4 个素模读 rest 姿势 → utils/directorRestPose.ts（参考图转姿势用，DRT-23）
 *   node scripts/director-assets.mjs mediapipe [--download]
 *       复制 node_modules/@mediapipe/tasks-vision 的模块版 wasm 到 public/director/mediapipe/wasm/（DRT-23）；
 *       `--download` 才从 Google 官方地址取一次姿势模型（核对大小与 MD5 后落盘，随仓库提交；构建与运行都不联网）
 *   node scripts/director-assets.mjs check     核对已提交的产物：用 three 的 GLTFLoader 在 Node 里真加载一遍
 *   node scripts/director-assets.mjs preview --out <dir>   软件光栅化出目视核对图（4 模型 × 3 体型、全部姿势），不提交
 *
 * 处理步骤（models）：
 *   1. 只留一档 LOD（LOD1，约 6000 面）：LOD0 约 13400 面，导演台的群众一阵最多 100 人（DRD-37），
 *      用 LOD0 就是 134 万面；单人特写下两档的轮廓差别肉眼难辨。
 *   2. 统一素模材质（DRK-2）：一个 MeshStandardMaterial，浅蓝灰 MANNEQUIN_COLOR，metalness 0、roughness 0.8；
 *      删掉全部贴图与 UV（没有贴图时 UV 只占体积）。
 *   3. 保留骨骼与三个体型 morph；末端骨改成标准名（见 utils/directorRig.ts，three 会删掉名字里的 `.`）。
 *   4. 坐标归一：单位米、Y 轴向上、面朝 +Z（上游本来如此，只核对不改）；脚底贴到 y = 0（上游鞋底离地 4~6 mm），
 *      并按 rest 重算 inverse bind matrices，使「绑定姿势 = rest」严格成立。
 *   5. 体积：裁掉孤立的数据 + 量化（KHR_mesh_quantization，GLTFLoader 原生支持、不需要解码器）。
 *      ⚠️ 只量化法线（含体型 morph 的法线增量，int8）、权重（uint8）与骨骼索引（uint8），**不量化顶点位置**：
 *      量化位置要把反量化矩阵乘进 inverse bind matrices，于是 three 的 `skeleton.pose()`（「回到 rest」）
 *      会把骨架整体缩放、平移走样，未蒙皮的几何包围盒也变成归一化坐标。位置保持 float 每个模型只多约 50 KB。
 *      ⚠️ 裁剪与量化由本脚本自己做，只用 @gltf-transform/core 与 extensions（MIT）：
 *      `@gltf-transform/functions` 经 ndarray-pixels 带进 sharp 与 LGPL-3.0 的 libvips，不在 03 C-4 的许可白名单里。
 *      ⚠️ 不用 meshopt：运行时要配 MeshoptDecoder，漏配时 GLTFLoader 只打一条 warn、模型静默加载失败。
 */
import { readFileSync, writeFileSync, mkdirSync, existsSync, statSync, readdirSync } from "node:fs";
import { dirname, join, resolve, relative } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { deflateSync } from "node:zlib";

const here = dirname(fileURLToPath(import.meta.url));
const webRoot = resolve(here, "..");
const repoRoot = resolve(webRoot, "../..");
const modelsDir = join(webRoot, "public/director/models");
const posesPath = join(webRoot, "public/director/poses.json");
/** 姿势缩略图（DRT-21）：开发态沙盒的「生成姿势缩略图」工具产出，提交进仓库；poses 命令按文件存在与否回填路径。 */
const thumbnailsDir = join(webRoot, "public/director/poses");
const thumbnailUrl = (code) => `/director/poses/${code}.webp`;
const catalogPath = join(repoRoot, "api/fixtures/director/catalog.json");

/** 素模颜色（sRGB）。glTF 的 baseColorFactor 是线性值，写入前换算，否则显示出来会偏白。 */
const MANNEQUIN_COLOR = "#9AB0CC";
const METALNESS = 0;
const ROUGHNESS = 0.8;
const KEEP_LOD = /[-_]LOD1$/;
const MAX_MODEL_BYTES = 2 * 1024 * 1024;
/** 姿势里单个分量的上限（度），与 Go 校验 `maxBoneRotation` 一致。 */
const MAX_BONE_DEGREES = 360;

const MODEL_FLAGS = {
  adult_male: "--adult-male",
  adult_female: "--adult-female",
  youth_male: "--youth-male",
  youth_female: "--youth-female"
};

function fail(message) {
  console.error(`✖ director-assets：${message}`);
  process.exit(1);
}

function argValue(name) {
  const i = process.argv.indexOf(name);
  return i > 0 ? process.argv[i + 1] : undefined;
}

/**
 * 直接读 utils/directorRig.ts（TS 源码经 typescript 转译后按 data URL 导入）。
 * 骨骼名与体型表只有那一份：脚本另抄一份的话，两份迟早不一致，而两边各自都「没错」。
 */
async function importTs(path) {
  const ts = (await import("typescript")).default;
  const out = ts.transpileModule(readFileSync(path, "utf8"), {
    compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 }
  });
  return import(`data:text/javascript;base64,${Buffer.from(out.outputText).toString("base64")}`);
}

const loadRig = () => importTs(join(webRoot, "utils/directorRig.ts"));
const loadCatalog = () => JSON.parse(readFileSync(catalogPath, "utf8"));

/**
 * 裁掉孤立的数据：删掉多余的 LOD 之后，它们的访问器、骨骼、材质只剩根节点一个父级。
 * 反复做到没有可删的为止（删掉一个网格之后，它的访问器才变成孤立的）。节点一个都不删。
 */
function pruneOrphans(doc) {
  const root = doc.getRoot();
  for (let removed = true; removed; ) {
    removed = false;
    for (const list of [root.listMeshes(), root.listSkins(), root.listMaterials(), root.listTextures(), root.listAccessors()]) {
      for (const prop of list) {
        if (prop.listParents().every((p) => p === root)) {
          prop.dispose();
          removed = true;
        }
      }
    }
  }
}

/** 浮点数组 → 归一化整数数组：signed 时按 ±(2^(bits-1)−1)，否则按 2^bits−1；越界的夹到 ±1 / [0, 1]。 */
function normalizedInts(src, signed) {
  const out = signed ? new Int8Array(src.length) : new Uint8Array(src.length);
  const max = signed ? 127 : 255;
  let clamped = 0;
  for (let i = 0; i < src.length; i++) {
    const lo = signed ? -1 : 0;
    const v = Math.min(1, Math.max(lo, src[i]));
    if (v !== src[i]) clamped++;
    out[i] = Math.round(v * max);
  }
  return { out, clamped };
}

/**
 * 量化法线、权重与骨骼索引（见文件头第 5 步）。
 *   - 法线与 morph 的法线增量 → int8 归一化（KHR_mesh_quantization 允许两者）；
 *   - 权重 → uint8 归一化，逐个顶点把舍入误差补到最大的那一项上，四项之和恰好是 255（three 不再归一化）；
 *   - 骨骼索引 → uint8（67 根骨骼，放得下）。
 * ⚠️ 共享的访问器只处理一次：处理两次就是把 int8 当成 float 再量化一遍，法线全部变成 0。
 */
function quantizeAttributes(doc) {
  const done = new Set();
  let clamped = 0;
  const once = (accessor, fn) => {
    if (!accessor || done.has(accessor)) return;
    done.add(accessor);
    fn(accessor);
  };
  const normals = (a) => {
    const r = normalizedInts(a.getArray(), true);
    clamped += r.clamped;
    a.setArray(r.out).setNormalized(true);
  };
  for (const mesh of doc.getRoot().listMeshes()) {
    for (const prim of mesh.listPrimitives()) {
      once(prim.getAttribute("NORMAL"), normals);
      for (const target of prim.listTargets()) once(target.getAttribute("NORMAL"), normals);
      for (const semantic of prim.listSemantics()) {
        if (/^WEIGHTS_\d+$/.test(semantic)) {
          once(prim.getAttribute(semantic), (a) => {
            const src = a.getArray();
            const out = new Uint8Array(src.length);
            for (let v = 0; v < src.length; v += 4) {
              let sum = 0;
              let big = v;
              for (let k = v; k < v + 4; k++) {
                out[k] = Math.round(Math.min(1, Math.max(0, src[k])) * 255);
                sum += out[k];
                if (out[k] > out[big]) big = k;
              }
              if (sum > 0) out[big] = Math.min(255, Math.max(0, out[big] + (255 - sum)));
            }
            a.setArray(out).setNormalized(true);
          });
        }
        if (/^JOINTS_\d+$/.test(semantic)) {
          once(prim.getAttribute(semantic), (a) => {
            const src = a.getArray();
            if (src.some((j) => j > 255)) fail(`${mesh.getName()}：骨骼索引超过 255，放不进 uint8`);
            a.setArray(Uint8Array.from(src)).setNormalized(false);
          });
        }
      }
    }
  }
  if (clamped) console.warn(`  ⚠ 有 ${clamped} 个法线分量越过 ±1，已夹取`);
}

function srgbToLinear(hex) {
  const c = hex.replace("#", "");
  return [0, 2, 4].map((i) => {
    const v = parseInt(c.slice(i, i + 2), 16) / 255;
    return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  });
}

// ---------------------------------------------------------------- models

async function buildModels() {
  const inputs = Object.fromEntries(Object.entries(MODEL_FLAGS).map(([code, flag]) => [code, argValue(flag)]));
  const missing = Object.entries(inputs).filter(([, p]) => !p || !existsSync(p));
  if (missing.length) fail(`缺少上游模型路径：${missing.map(([code]) => MODEL_FLAGS[code]).join("、")}`);

  const { Logger, NodeIO } = await import("@gltf-transform/core");
  const { KHRMeshQuantization } = await import("@gltf-transform/extensions");
  const { Matrix4 } = await import("three");
  const rig = await loadRig();
  // 扩展要在 io 上登记才会写出去（只在 document 上创建的话，写出时被静默丢掉）。
  const io = new NodeIO().registerExtensions([KHRMeshQuantization]);
  mkdirSync(modelsDir, { recursive: true });

  for (const [code, input] of Object.entries(inputs)) {
    const doc = await io.read(input);
    doc.setLogger(new Logger(Logger.Verbosity.WARN));
    const root = doc.getRoot();

    // 1. 只留 LOD1。
    const meshNodes = root.listNodes().filter((n) => n.getMesh());
    const keep = meshNodes.filter((n) => KEEP_LOD.test(n.getMesh().getName()));
    if (keep.length !== 1) fail(`${code}：应恰好有一个 LOD1 网格，实际 ${keep.length} 个`);
    const node = keep[0];
    for (const n of meshNodes) {
      if (n === node) continue;
      const mesh = n.getMesh();
      // ⚠️ 图元与 morph 目标要一起删：只删网格的话它们还挂着访问器，访问器就不算孤立，整份 LOD 数据照样写进 GLB。
      for (const prim of mesh.listPrimitives()) {
        for (const target of prim.listTargets()) target.dispose();
        prim.dispose();
      }
      n.dispose();
      mesh.dispose();
    }
    const mesh = node.getMesh();
    node.setName(code);
    mesh.setName(code);
    const skin = node.getSkin();
    if (!skin) fail(`${code}：LOD1 没有骨骼`);

    // 2. 统一材质、删贴图与 UV。
    const material = doc
      .createMaterial("director_mannequin")
      .setBaseColorFactor([...srgbToLinear(MANNEQUIN_COLOR), 1])
      .setMetallicFactor(METALNESS)
      .setRoughnessFactor(ROUGHNESS);
    for (const prim of mesh.listPrimitives()) {
      prim.setMaterial(material);
      for (const semantic of prim.listSemantics()) {
        // 没有贴图，UV、顶点色与切线都用不上（切线只给法线贴图用）。
        if (semantic.startsWith("TEXCOORD_") || semantic.startsWith("COLOR_") || semantic === "TANGENT") {
          prim.setAttribute(semantic, null);
        }
      }
    }
    for (const m of root.listMaterials()) if (m !== material) m.dispose();
    for (const t of root.listTextures()) t.dispose();

    // 3. 末端骨改成标准名，并核对骨骼集合与层级。
    const renames = {};
    for (const joint of skin.listJoints()) {
      const raw = joint.getName();
      const std = rig.directorStandardBoneName(code, raw);
      if (!std) fail(`${code}：骨骼 ${raw} 不在标准骨骼表里（utils/directorRig.ts）`);
      if (std !== raw) renames[raw] = std;
      joint.setName(std);
    }
    const jointNames = skin.listJoints().map((j) => j.getName());
    const expected = [...rig.DIRECTOR_STANDARD_BONES].sort();
    if (JSON.stringify([...jointNames].sort()) !== JSON.stringify(expected)) {
      fail(`${code}：改名后的骨骼集合与标准表不一致`);
    }
    for (const joint of skin.listJoints()) {
      const parent = joint.getParentNode();
      const parentName = parent && jointNames.includes(parent.getName()) ? parent.getName() : null;
      if (rig.DIRECTOR_BONE_PARENT[joint.getName()] !== parentName) {
        fail(`${code}：${joint.getName()} 的父骨骼是 ${parentName}，标准表写的是 ${rig.DIRECTOR_BONE_PARENT[joint.getName()]}`);
      }
    }
    if (JSON.stringify(renames) !== JSON.stringify(rig.DIRECTOR_UPSTREAM_BONE_RENAMES[code])) {
      fail(`${code}：实际改名与 DIRECTOR_UPSTREAM_BONE_RENAMES 不一致：${JSON.stringify(renames)}`);
    }

    // 4. 脚底贴到 y = 0：移顶点、移根骨骼，再按 rest 重算 inverse bind matrices。
    let minY = Infinity;
    const v = [];
    for (const prim of mesh.listPrimitives()) {
      const pos = prim.getAttribute("POSITION");
      for (let i = 0; i < pos.getCount(); i++) minY = Math.min(minY, pos.getElement(i, v)[1]);
    }
    const groundOffset = -minY;
    for (const prim of mesh.listPrimitives()) {
      const pos = prim.getAttribute("POSITION");
      for (let i = 0; i < pos.getCount(); i++) {
        pos.getElement(i, v);
        v[1] += groundOffset;
        pos.setElement(i, v);
      }
    }
    const hips = skin.listJoints().find((j) => j.getName() === "Hips");
    const t = hips.getTranslation();
    hips.setTranslation([t[0], t[1] + groundOffset, t[2]]);
    const ibm = new Float32Array(skin.listJoints().length * 16);
    skin.listJoints().forEach((joint, i) => {
      ibm.set(new Matrix4().fromArray(joint.getWorldMatrix()).invert().toArray(), i * 16);
    });
    skin.setInverseBindMatrices(
      doc.createAccessor(`${code}_inverse_bind`).setType("MAT4").setArray(ibm).setBuffer(root.listBuffers()[0])
    );

    // 来源与改名写进 GLB：许可页与测试都从产物本身核对，不靠记忆。
    root.getAsset().generator = "seeAitv scripts/director-assets.mjs (glTF-Transform core 4.5.1)";
    root.getAsset().copyright = "Copyright (c) 2019 Wonder Unit — wonderunit/shot-generator-models (MIT)";
    root.setExtras({ directorAsset: { model: code, lod: "LOD1", groundOffsetM: Number(groundOffset.toFixed(6)), boneRenames: renames } });

    // 5. 体积：裁掉孤立的数据，再量化。int8 法线在核心 glTF 里是非法的，必须声明 KHR_mesh_quantization。
    pruneOrphans(doc);
    quantizeAttributes(doc);
    doc.createExtension(KHRMeshQuantization).setRequired(true);

    const out = join(modelsDir, `${code}.glb`);
    writeFileSync(out, await io.writeBinary(doc));
    console.log(`✔ ${code}：${relative(webRoot, out)}（${(statSync(out).size / 1024).toFixed(0)} KB，整体下移 ${(-groundOffset * 1000).toFixed(1)} mm 贴地）`);
  }
  await check();
}

// ---------------------------------------------------------------- poses

async function buildPoses() {
  const source = await import(pathToFileURL(join(here, "poses.source.mjs")).href);
  const compiled = source.compilePoses();
  const rig = await loadRig();
  const catalog = loadCatalog();
  const standard = new Set(rig.DIRECTOR_STANDARD_BONES);
  const codes = catalog.poses.map((p) => p.code);
  const extra = Object.keys(compiled).filter((code) => !codes.includes(code));
  const missing = codes.filter((code) => !compiled[code]);
  if (extra.length || missing.length) fail(`姿势码与 catalog.json 不一致：多出 ${extra.join("、") || "无"}；缺少 ${missing.join("、") || "无"}`);

  // name / category 取自 catalog.json：两边各写一份的话迟早对不上（DRD-29）。
  const poses = catalog.poses.map((p) => {
    const src = compiled[p.code];
    const bones = {};
    for (const [name, value] of Object.entries(src.bones)) {
      if (!standard.has(name)) fail(`${p.code}：${name} 不是标准骨骼名`);
      if (!Array.isArray(value) || value.length !== 3 || !value.every((n) => Number.isFinite(n) && Math.abs(n) <= MAX_BONE_DEGREES)) {
        fail(`${p.code}.${name}：角度必须是三个绝对值不超过 ${MAX_BONE_DEGREES} 的数`);
      }
      bones[name] = { x: value[0], y: value[1], z: value[2] };
    }
    if (Object.keys(bones).length > catalog.limits.maxBoneOverrides) fail(`${p.code}：骨骼数超过 ${catalog.limits.maxBoneOverrides}`);
    const entry = { code: p.code, name: p.name, category: p.category, bones };
    if (src.lift !== undefined) {
      if (!(src.lift > 0 && src.lift < 1)) fail(`${p.code}：lift 是离地高度占身高的比例，必须在 0~1 之间`);
      entry.lift = src.lift;
    }
    // 缩略图要用引擎离屏渲染（DRT-21 的沙盒工具），这里只按文件存在与否回填路径
    entry.thumbnail = existsSync(join(thumbnailsDir, `${p.code}.webp`)) ? thumbnailUrl(p.code) : "";
    return entry;
  });
  writeFileSync(posesPath, `${JSON.stringify(poses, null, 2)}\n`);
  console.log(`✔ ${relative(webRoot, posesPath)}：${poses.length} 个姿势`);
  await check();
}

// ---------------------------------------------------------------- check

/** 读 GLB 的 JSON 块（不依赖任何库，测试里同样的读法见 utils/directorRig.test.ts）。 */
function glbJson(bytes) {
  if (bytes.readUInt32LE(0) !== 0x46546c67) throw new Error("不是 GLB 文件");
  const length = bytes.readUInt32LE(12);
  if (bytes.readUInt32LE(16) !== 0x4e4f534a) throw new Error("GLB 的第一个块不是 JSON");
  return JSON.parse(bytes.subarray(20, 20 + length).toString("utf8"));
}

async function loadThreeModel(path) {
  const THREE = await import("three");
  const { GLTFLoader } = await import("three/examples/jsm/loaders/GLTFLoader.js");
  const bytes = readFileSync(path);
  const buffer = bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength);
  const gltf = await new GLTFLoader().parseAsync(buffer, "");
  let skinned = null;
  gltf.scene.traverse((o) => {
    if (o.isSkinnedMesh) skinned = o;
  });
  gltf.scene.updateMatrixWorld(true);
  return { THREE, gltf, skinned, bytes };
}

/** 当前骨骼与 morph 下，蒙皮后全部顶点的世界坐标。 */
function skinnedPositions(THREE, mesh) {
  mesh.updateMatrixWorld(true);
  const count = mesh.geometry.attributes.position.count;
  const out = new Float32Array(count * 3);
  const p = new THREE.Vector3();
  for (let i = 0; i < count; i++) {
    mesh.getVertexPosition(i, p).applyMatrix4(mesh.matrixWorld);
    out[i * 3] = p.x;
    out[i * 3 + 1] = p.y;
    out[i * 3 + 2] = p.z;
  }
  return out;
}

function bounds(positions) {
  const min = [Infinity, Infinity, Infinity];
  const max = [-Infinity, -Infinity, -Infinity];
  for (let i = 0; i < positions.length; i += 3) {
    for (let k = 0; k < 3; k++) {
      min[k] = Math.min(min[k], positions[i + k]);
      max[k] = Math.max(max[k], positions[i + k]);
    }
  }
  return { min, max };
}

async function check() {
  const rig = await loadRig();
  const catalog = loadCatalog();
  const problems = [];
  const expect = (ok, message) => {
    if (!ok) problems.push(message);
  };

  for (const model of catalog.models) {
    const path = join(modelsDir, `${model.code}.glb`);
    if (!existsSync(path)) {
      problems.push(`${model.code}：缺少 ${relative(webRoot, path)}`);
      continue;
    }
    const { THREE, skinned, bytes } = await loadThreeModel(path);
    const json = glbJson(bytes);
    expect(bytes.length <= MAX_MODEL_BYTES, `${model.code}：${bytes.length} 字节，超过 2 MB`);
    expect(
      (json.extensionsUsed ?? []).includes("KHR_mesh_quantization") && (json.extensionsRequired ?? []).includes("KHR_mesh_quantization"),
      `${model.code}：没有声明 KHR_mesh_quantization（int8 法线在核心 glTF 里非法）`
    );
    for (const ext of ["EXT_meshopt_compression", "KHR_draco_mesh_compression"]) {
      expect(!(json.extensionsUsed ?? []).includes(ext), `${model.code}：用了需要解码器的 ${ext}`);
    }
    expect(!json.images && !json.textures && !json.samplers, `${model.code}：还带着贴图`);
    expect(json.materials?.length === 1 && json.meshes?.length === 1 && json.skins?.length === 1, `${model.code}：材质 / 网格 / 骨骼应各一份`);
    expect(
      JSON.stringify(json.extras?.directorAsset?.boneRenames) === JSON.stringify(rig.DIRECTOR_UPSTREAM_BONE_RENAMES[model.code]),
      `${model.code}：GLB 里记录的改名与 DIRECTOR_UPSTREAM_BONE_RENAMES 不一致`
    );
    if (!skinned) {
      problems.push(`${model.code}：GLTFLoader 没有加载出蒙皮网格`);
      continue;
    }
    const mat = skinned.material;
    expect(mat.isMeshStandardMaterial && mat.color.getHexString() === MANNEQUIN_COLOR.slice(1).toLowerCase(), `${model.code}：材质颜色 ${mat.color?.getHexString()}`);
    expect(mat.metalness === METALNESS && Math.abs(mat.roughness - ROUGHNESS) < 1e-6 && !mat.map, `${model.code}：材质参数不对或还有贴图`);
    const dict = skinned.morphTargetDictionary ?? {};
    expect(JSON.stringify(Object.keys(dict).sort()) === JSON.stringify([...rig.DIRECTOR_MORPH_TARGETS].sort()), `${model.code}：体型 morph ${Object.keys(dict)}`);
    const boneNames = skinned.skeleton.bones.map((b) => b.name).sort();
    expect(JSON.stringify(boneNames) === JSON.stringify([...rig.DIRECTOR_STANDARD_BONES].sort()), `${model.code}：加载后的骨骼名与标准表不一致`);

    // 几何：rest 下脚底在 y = 0、身高与目录一致、面朝 +Z、左手在 +X。
    const restBounds = bounds(skinnedPositions(THREE, skinned));
    const height = restBounds.max[1] - restBounds.min[1];
    expect(Math.abs(restBounds.min[1]) < 0.0005, `${model.code}：rest 下最低点 ${restBounds.min[1].toFixed(4)} m，应在 0`);
    expect(Math.abs(height - model.heightM) < 0.005, `${model.code}：身高 ${height.toFixed(4)} m，目录写 ${model.heightM}`);
    const bone = (name) => skinned.skeleton.getBoneByName(name).getWorldPosition(new THREE.Vector3());
    expect(bone("LeftToeBase").z > bone("LeftFoot").z, `${model.code}：脚尖没有朝 +Z`);
    expect(bone("LeftHand").x > 0 && bone("RightHand").x < 0, `${model.code}：左手不在 +X 侧`);
    // 绑定姿势 = rest：skeleton.pose() 之后骨骼位置不变（DRT-21 的「回到 rest」依赖这一条）。
    const before = skinned.skeleton.bones.map((b) => b.getWorldPosition(new THREE.Vector3()));
    skinned.skeleton.pose();
    skinned.parent.updateMatrixWorld(true);
    const drift = Math.max(...skinned.skeleton.bones.map((b, i) => b.getWorldPosition(new THREE.Vector3()).distanceTo(before[i])));
    expect(drift < 1e-4, `${model.code}：skeleton.pose() 后骨骼偏移 ${drift.toFixed(5)} m（inverse bind matrices 与 rest 不一致）`);
    // 三档体型都不把脚底压到地面以下。
    const heights = [];
    for (const body of catalog.bodies.map((b) => b.code)) {
      const w = rig.directorBodyMorphWeights(model.code, body);
      for (const [name, value] of Object.entries(w)) skinned.morphTargetInfluences[dict[name]] = value;
      const b = bounds(skinnedPositions(THREE, skinned));
      expect(b.min[1] > -0.002, `${model.code}/${body}：最低点 ${b.min[1].toFixed(4)} m 低于地面`);
      heights.push(`${body} ${(b.max[1] - b.min[1]).toFixed(3)}`);
    }
    skinned.morphTargetInfluences.fill(0);
    console.log(`  ${model.code}：${(bytes.length / 1024).toFixed(0)} KB，${skinned.geometry.index.count / 3} 面，身高 ${heights.join(" / ")} m`);
  }

  if (existsSync(posesPath)) {
    const poses = JSON.parse(readFileSync(posesPath, "utf8"));
    const standard = new Set(rig.DIRECTOR_STANDARD_BONES);
    expect(JSON.stringify(poses.map((p) => p.code)) === JSON.stringify(catalog.poses.map((p) => p.code)), "poses.json 的姿势码与 catalog.json 不一致（含顺序）");
    for (const p of poses) {
      const c = catalog.poses.find((x) => x.code === p.code);
      expect(c && c.name === p.name && c.category === p.category, `${p.code}：名称或分类与 catalog.json 不一致`);
      expect(Object.keys(p.bones).every((b) => standard.has(b)), `${p.code}：用了非标准骨骼名`);
      // DR-F-42：每个姿势都有缩略图，且文件真在（路径写了而文件没提交的话面板上是裂图）
      expect(p.thumbnail === thumbnailUrl(p.code), `${p.code}：thumbnail 应为 ${thumbnailUrl(p.code)}（先用沙盒工具生成缩略图，再跑 poses）`);
      expect(existsSync(join(thumbnailsDir, `${p.code}.webp`)), `${p.code}：缺少缩略图文件 public/director/poses/${p.code}.webp`);
    }
    console.log(`  poses.json：${poses.length} 个姿势`);
  } else {
    problems.push(`缺少 ${relative(webRoot, posesPath)}`);
  }

  if (problems.length) fail(`核对未通过：\n  - ${problems.join("\n  - ")}`);
  console.log("✔ 素材核对通过");
}

// ---------------------------------------------------------------- preview（目视核对，不提交）

/**
 * 软件光栅化：正交投影 + z 缓冲 + 面法线 Lambert。只为在没有 GPU 的地方看一眼体型与姿势对不对，
 * 不是缩略图（缩略图由 DRT-21 用引擎离屏渲染）。
 */
function rasterize(positions, index, view, width, height, frame) {
  const rgba = Buffer.alloc(width * height * 4);
  for (let i = 0; i < width * height; i++) rgba.set([24, 26, 32, 255], i * 4);
  const depth = new Float32Array(width * height).fill(-Infinity);
  const [r0, g0, b0] = [1, 3, 5].map((i) => parseInt(MANNEQUIN_COLOR.slice(i, i + 2), 16));
  const light = [0.35, 0.55, 0.76];
  const project = (i) => {
    const x = positions[i * 3];
    const y = positions[i * 3 + 1];
    const z = positions[i * 3 + 2];
    // 屏幕 u 向右、v 向上、d 越大越近。
    const [u, vv, d] = view === "front" ? [x, y, z] : view === "side" ? [-z, y, x] : [x, -z, y];
    return [((u - frame.u0) / frame.size) * width, height - ((vv - frame.v0) / frame.size) * height, d];
  };
  // 地面线（y = 0）。
  if (view !== "top") {
    const gy = Math.round(height - ((0 - frame.v0) / frame.size) * height);
    for (let x = 0; x < width && gy >= 0 && gy < height; x++) rgba.set([70, 74, 86, 255], (gy * width + x) * 4);
  }
  for (let t = 0; t < index.length; t += 3) {
    const a = project(index[t]);
    const b = project(index[t + 1]);
    const c = project(index[t + 2]);
    const ia = index[t] * 3;
    const ib = index[t + 1] * 3;
    const ic = index[t + 2] * 3;
    const e1 = [positions[ib] - positions[ia], positions[ib + 1] - positions[ia + 1], positions[ib + 2] - positions[ia + 2]];
    const e2 = [positions[ic] - positions[ia], positions[ic + 1] - positions[ia + 1], positions[ic + 2] - positions[ia + 2]];
    const n = [e1[1] * e2[2] - e1[2] * e2[1], e1[2] * e2[0] - e1[0] * e2[2], e1[0] * e2[1] - e1[1] * e2[0]];
    const len = Math.hypot(n[0], n[1], n[2]) || 1;
    const lv = view === "front" ? light : view === "side" ? [light[2], light[1], -light[0]] : [light[0], light[2], -light[1]];
    const shade = 0.25 + 0.75 * Math.abs((n[0] * lv[0] + n[1] * lv[1] + n[2] * lv[2]) / len);
    const minX = Math.max(0, Math.floor(Math.min(a[0], b[0], c[0])));
    const maxX = Math.min(width - 1, Math.ceil(Math.max(a[0], b[0], c[0])));
    const minY = Math.max(0, Math.floor(Math.min(a[1], b[1], c[1])));
    const maxY = Math.min(height - 1, Math.ceil(Math.max(a[1], b[1], c[1])));
    const area = (b[0] - a[0]) * (c[1] - a[1]) - (b[1] - a[1]) * (c[0] - a[0]);
    if (Math.abs(area) < 1e-9) continue;
    for (let y = minY; y <= maxY; y++) {
      for (let x = minX; x <= maxX; x++) {
        const px = x + 0.5;
        const py = y + 0.5;
        const w0 = ((b[0] - px) * (c[1] - py) - (b[1] - py) * (c[0] - px)) / area;
        const w1 = ((c[0] - px) * (a[1] - py) - (c[1] - py) * (a[0] - px)) / area;
        const w2 = 1 - w0 - w1;
        if (w0 < 0 || w1 < 0 || w2 < 0) continue;
        const d = w0 * a[2] + w1 * b[2] + w2 * c[2];
        const k = y * width + x;
        if (d <= depth[k]) continue;
        depth[k] = d;
        rgba.set([Math.round(r0 * shade), Math.round(g0 * shade), Math.round(b0 * shade), 255], k * 4);
      }
    }
  }
  return rgba;
}

const CRC_TABLE = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});

function crc32(buf) {
  let c = 0xffffffff;
  for (const byte of buf) c = CRC_TABLE[(c ^ byte) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function encodePng(width, height, rgba) {
  const raw = Buffer.alloc((width * 4 + 1) * height);
  for (let y = 0; y < height; y++) rgba.copy(raw, y * (width * 4 + 1) + 1, y * width * 4, (y + 1) * width * 4);
  const chunk = (type, data) => {
    const len = Buffer.alloc(4);
    len.writeUInt32BE(data.length);
    const body = Buffer.concat([Buffer.from(type, "ascii"), data]);
    const crc = Buffer.alloc(4);
    crc.writeUInt32BE(crc32(body));
    return Buffer.concat([len, body, crc]);
  };
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8;
  ihdr[9] = 6;
  return Buffer.concat([
    Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]),
    chunk("IHDR", ihdr),
    chunk("IDAT", deflateSync(raw)),
    chunk("IEND", Buffer.alloc(0))
  ]);
}

/** 把若干张同尺寸的图横向拼成一行、多行竖向拼接。 */
function sheet(rows, cellW, cellH) {
  const cols = Math.max(...rows.map((r) => r.length));
  const width = cols * cellW;
  const height = rows.length * cellH;
  const out = Buffer.alloc(width * height * 4);
  rows.forEach((row, ri) => {
    row.forEach((cell, ci) => {
      for (let y = 0; y < cellH; y++) {
        cell.copy(out, ((ri * cellH + y) * width + ci * cellW) * 4, y * cellW * 4, (y + 1) * cellW * 4);
      }
    });
  });
  return { width, height, rgba: out };
}

/** 套姿势：所有骨骼回到 rest，再右乘每根骨骼的局部欧拉角（口径见 utils/directorRig.ts）。 */
function applyPose(THREE, skinned, rest, bones) {
  for (const bone of skinned.skeleton.bones) bone.quaternion.copy(rest.get(bone.name));
  const euler = new THREE.Euler();
  const q = new THREE.Quaternion();
  for (const [name, r] of Object.entries(bones ?? {})) {
    const bone = skinned.skeleton.getBoneByName(name);
    if (!bone) continue;
    euler.set(THREE.MathUtils.degToRad(r.x), THREE.MathUtils.degToRad(r.y), THREE.MathUtils.degToRad(r.z), "XYZ");
    bone.quaternion.multiply(q.setFromEuler(euler));
  }
  skinned.parent.updateMatrixWorld(true);
}

async function preview() {
  const outDir = argValue("--out");
  if (!outDir) fail("preview 需要 --out <目录>");
  mkdirSync(outDir, { recursive: true });
  const rig = await loadRig();
  const catalog = loadCatalog();
  const poses = existsSync(posesPath) ? JSON.parse(readFileSync(posesPath, "utf8")) : [];
  const only = argValue("--only")?.split(",");
  const cell = Number(argValue("--cell") ?? 220);
  /** 每个视图按包围盒的最大边取景，三个视图同一比例尺（看得出姿势占地多大）。 */
  const frames = (positions, views) => {
    const b = bounds(positions);
    const span = Math.max(b.max[0] - b.min[0], b.max[1] - b.min[1], b.max[2] - b.min[2], 1.2) * 1.12;
    const mid = [(b.min[0] + b.max[0]) / 2, (b.min[1] + b.max[1]) / 2, (b.min[2] + b.max[2]) / 2];
    return views.map((view) => {
      const [cu, cv] = view === "front" ? [mid[0], mid[1]] : view === "side" ? [-mid[2], mid[1]] : [mid[0], -mid[2]];
      return { view, frame: { u0: cu - span / 2, v0: cv - span / 2, size: span } };
    });
  };
  const render = (positions, index, views) =>
    frames(positions, views).map(({ view, frame }) => rasterize(positions, index, view, cell, cell, frame));

  // 4 模型 × 3 体型（正面 + 侧面）。
  const bodyRows = [];
  const loaded = {};
  for (const model of catalog.models) {
    const m = await loadThreeModel(join(modelsDir, `${model.code}.glb`));
    loaded[model.code] = m;
    const row = [];
    for (const body of catalog.bodies.map((b) => b.code)) {
      const w = rig.directorBodyMorphWeights(model.code, body);
      for (const [name, value] of Object.entries(w)) m.skinned.morphTargetInfluences[m.skinned.morphTargetDictionary[name]] = value;
      row.push(...render(skinnedPositions(m.THREE, m.skinned), m.skinned.geometry.index.array, ["front", "side"]));
    }
    m.skinned.morphTargetInfluences.fill(0);
    bodyRows.push(row);
  }
  const bodiesSheet = sheet(bodyRows, cell, cell);
  writeFileSync(join(outDir, "bodies.png"), encodePng(bodiesSheet.width, bodiesSheet.height, bodiesSheet.rgba));

  // 姿势：默认成年男标准体型（--model 可换），按「最低点落地 + lift」放好，正面 / 侧面 / 顶视。
  const poseModel = argValue("--model") ?? "adult_male";
  const { THREE, skinned } = loaded[poseModel] ?? fail(`不认识的素模 ${poseModel}`);
  const rest = new Map(skinned.skeleton.bones.map((b) => [b.name, b.quaternion.clone()]));
  const height = catalog.models.find((m) => m.code === poseModel).heightM;
  for (const category of catalog.poseCategories.map((c) => c.code)) {
    const rows = [];
    for (const pose of poses.filter((p) => p.category === category && (!only || only.includes(p.code)))) {
      applyPose(THREE, skinned, rest, pose.bones);
      const positions = skinnedPositions(THREE, skinned);
      const lowest = bounds(positions).min[1];
      const shift = -lowest + (pose.lift ?? 0) * height;
      for (let i = 1; i < positions.length; i += 3) positions[i] += shift;
      rows.push(render(positions, skinned.geometry.index.array, ["front", "side", "top"]));
    }
    if (!rows.length) continue;
    const s = sheet(rows, cell, cell);
    writeFileSync(join(outDir, `poses-${category}.png`), encodePng(s.width, s.height, s.rgba));
  }
  applyPose(THREE, skinned, rest, {});
  console.log(`✔ 目视核对图写到 ${outDir}：${readdirSync(outDir).filter((f) => f.endsWith(".png")).join("、")}`);
}

// ---------------------------------------------------------------- rest（DRT-23）

/**
 * 参考图转姿势要用的骨骼（躯干、头、四肢，加上求方向用的末端：头顶、中指根、脚趾根）。
 * ⚠️ `utils/` 不能 import three（DRD-33），换算又离不开每根骨骼的 rest 局部旋转与位置 —— 所以离线导出成数据，
 *    由 `utils/directorRestPose.test.ts` 直接读 GLB 的 JSON 逐项核对（重建素模而忘了重跑本命令的话那条测试会红）。
 */
const REST_BONES = [
  "Hips", "Spine", "Spine1", "Spine2", "Neck", "Head", "HeadTop_End",
  ...["Left", "Right"].flatMap((side) => [
    `${side}Shoulder`, `${side}Arm`, `${side}ForeArm`, `${side}Hand`, `${side}HandMiddle1`,
    `${side}UpLeg`, `${side}Leg`, `${side}Foot`, `${side}ToeBase`
  ])
];
const restPosePath = join(webRoot, "utils/directorRestPose.ts");
const round6 = (v) => Math.round(v * 1e6) / 1e6 || 0;

/** 直接读 GLB 的节点表（不经 three）：局部旋转默认 (0,0,0,1)、位置默认 (0,0,0)。 */
function restOfGlb(bytes) {
  const json = glbJson(bytes);
  const nodes = json.nodes ?? [];
  const parentOf = new Map();
  nodes.forEach((node, index) => (node.children ?? []).forEach((child) => parentOf.set(child, index)));
  const indexOf = new Map(nodes.map((node, index) => [node.name, index]));
  const bones = {};
  for (const name of REST_BONES) {
    const node = nodes[indexOf.get(name)];
    if (!node) throw new Error(`缺少骨骼 ${name}`);
    bones[name] = { q: (node.rotation ?? [0, 0, 0, 1]).map(round6), p: (node.translation ?? [0, 0, 0]).map(round6) };
  }
  // Hips 以上的节点必须是单位变换：换算直接在模型坐标里做（Y 向上、面朝 +Z、单位米）
  for (let i = parentOf.get(indexOf.get("Hips")); i !== undefined; i = parentOf.get(i)) {
    const node = nodes[i];
    const identity = !node.matrix && (node.rotation ?? [0, 0, 0, 1]).every((v, k) => Math.abs(v - [0, 0, 0, 1][k]) < 1e-6)
      && (node.scale ?? [1, 1, 1]).every((v) => Math.abs(v - 1) < 1e-6) && (node.translation ?? [0, 0, 0]).every((v) => Math.abs(v) < 1e-6);
    if (!identity) throw new Error(`Hips 的上级节点 ${node.name ?? i} 不是单位变换`);
  }
  return bones;
}

function restSource(table) {
  return `/**
 * 素模的 rest 姿势（DRT-23）：每根骨骼的局部旋转 \`q\`（四元数 x, y, z, w）与局部位置 \`p\`（米）。
 * 参考图转姿势按它把关键点方向换算成相对 rest 的局部欧拉角（\`directorPoseFromLandmarks\`）。
 *
 * ⚠️ 生成文件，不要手改：\`node scripts/director-assets.mjs rest\`。\`directorRestPose.test.ts\` 直接读 GLB 逐项核对。
 */
import type { DirectorModelCode } from "./directorCatalog";

export type DirectorRestBone = { q: readonly [number, number, number, number]; p: readonly [number, number, number] };

export const DIRECTOR_REST_BONES = [${REST_BONES.map((n) => `"${n}"`).join(", ")}] as const;

export const DIRECTOR_REST_POSE: Readonly<Record<DirectorModelCode, Readonly<Record<string, DirectorRestBone>>>> = {
${Object.entries(table).map(([code, bones]) => `  ${code}: {
${Object.entries(bones).map(([name, b]) => `    ${name}: { q: [${b.q.join(", ")}], p: [${b.p.join(", ")}] }`).join(",\n")}
  }`).join(",\n")}
};
`;
}

async function buildRest() {
  const rig = await loadRig();
  const table = {};
  for (const code of rig.DIRECTOR_RIG_MODELS) table[code] = restOfGlb(readFileSync(join(modelsDir, `${code}.glb`)));
  writeFileSync(restPosePath, restSource(table));
  console.log(`✔ ${relative(webRoot, restPosePath)}：${Object.keys(table).length} 个素模 × ${REST_BONES.length} 根骨骼`);
}

// ---------------------------------------------------------------- mediapipe（DRT-23）

/**
 * 自托管 MediaPipe（DRD-41）：识别跑在 **module worker** 里，MediaPipe 在那里只能用模块版的加载器
 * （`importScripts` 在 module worker 里抛 TypeError，它退回 `import()`，而经典版 js 的顶层 `var` 在模块里不是全局变量），
 * 所以只复制模块版的两个文件；经典版与无 SIMD 版（共 26 MB）用不到。
 * 模型文件不在 npm 包里：`--download` 从官方地址取一次，核对大小与 MD5 后落盘，随仓库提交。
 */
const mediapipeDir = join(webRoot, "public/director/mediapipe");
const MEDIAPIPE_WASM = ["vision_wasm_module_internal.js", "vision_wasm_module_internal.wasm"];
const POSE_MODEL = {
  file: "pose_landmarker_full.task",
  url: "https://storage.googleapis.com/mediapipe-models/pose_landmarker/pose_landmarker_full/float16/latest/pose_landmarker_full.task",
  bytes: 9398198,
  md5Base64: "WprYiRmyIx0Csf30pUCQ7A=="
};

async function mediapipe() {
  const { createHash } = await import("node:crypto");
  const { copyFileSync } = await import("node:fs");
  const source = join(webRoot, "node_modules/@mediapipe/tasks-vision/wasm");
  mkdirSync(join(mediapipeDir, "wasm"), { recursive: true });
  for (const name of MEDIAPIPE_WASM) {
    if (!existsSync(join(source, name))) fail(`node_modules 里没有 ${name}：先 pnpm install`);
    copyFileSync(join(source, name), join(mediapipeDir, "wasm", name));
  }
  const modelPath = join(mediapipeDir, POSE_MODEL.file);
  if (process.argv.includes("--download")) {
    const response = await fetch(POSE_MODEL.url);
    if (!response.ok) fail(`下载模型失败：HTTP ${response.status}`);
    writeFileSync(modelPath, Buffer.from(await response.arrayBuffer()));
  }
  if (!existsSync(modelPath)) fail(`缺少 ${relative(webRoot, modelPath)}：加 --download 从官方地址取一次`);
  const model = readFileSync(modelPath);
  const md5 = createHash("md5").update(model).digest("base64");
  if (model.length !== POSE_MODEL.bytes || md5 !== POSE_MODEL.md5Base64) fail(`模型文件与官方发布的不一致（${model.length} 字节，MD5 ${md5}）`);
  console.log(`✔ ${relative(webRoot, mediapipeDir)}：wasm ${MEDIAPIPE_WASM.join("、")}；模型 ${POSE_MODEL.file}（${model.length} 字节，MD5 核对通过）`);
}

// ---------------------------------------------------------------- main

const commands = { models: buildModels, poses: buildPoses, rest: buildRest, mediapipe, check, preview };
const command = commands[process.argv[2]];
if (!command) fail(`用法：node scripts/director-assets.mjs <${Object.keys(commands).join(" | ")}> …`);
await command();
