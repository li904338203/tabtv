<script setup lang="ts">
/**
 * 3D 导演台开发用沙盒（DRT-12，dev/03 §14）：只在开发态经 `nuxt.config.ts` 的 `pages:extend` 注册为 `/__director-sandbox`，
 * 生产构建里没有这个路由。画布接入（DRT-16）之前，DRT-12 / 13 / 15 / 21 / 22 的浏览器检查都在这里做。
 *
 * - 场景取 `fetch("/__director-sandbox/scene.json?name=…")`：检查脚本拦截并返回 fixture，开发服务器上并没有这个文件；
 * - 全景走与编辑器相同的 `createDirectorMediaFetcher`（受保护的 `/api/v1/media/{id}`，检查脚本拦截）；
 * - `window.__sandbox` 给检查脚本：取引擎、重挂、换场景、事件记录；页面上显示最近一次 `transform-end` 的载荷（DRT-13）；
 * - 「生成姿势缩略图」（DRT-21，dev/03 §9.2）：每个姿势用成年男素模离屏渲染一张 256×256 的透明 WebP。
 *   浏览器支持目录选择时直接写进选中的目录（选 `frontend/public/director/poses/`），否则逐个下载；
 *   写好后跑 `node scripts/director-assets.mjs poses` 回填 `poses.json` 的 `thumbnail`。
 *   `window.__sandbox.poseThumbnails()` 给脚本调用（返回 data URL），产物作为静态文件提交。
 *
 * ⚠️ 不碰用户的真实画布：这里没有任何项目、节点或场景接口。
 */
import { DirectorEngine } from "~/director/engine/DirectorEngine";
import type { DirectorEngineEvents } from "~/director/engine/events";
import { createDirectorEngineHandle } from "~/director/engine/handle";
import { directorLoadPoses } from "~/director/assets";
import { createDirectorMediaFetcher } from "~/director/media";
import type { DirectorScene } from "~/utils/directorScene";
import { directorValidateScene } from "~/utils/directorScene";

type LoggedEvent = { type: keyof DirectorEngineEvents; payload: unknown; at: number };

const mounted = ref(true);
const viewportKey = ref(0);
const canvasEl = ref<HTMLCanvasElement | null>(null);
const labelsEl = ref<HTMLElement | null>(null);
const status = ref("加载中");
const lastTransformEnd = ref("");
const scene = shallowRef<DirectorScene | null>(null);
const events: LoggedEvent[] = [];
const handle = createDirectorEngineHandle(() => new DirectorEngine());
const fetchMediaBlob = createDirectorMediaFetcher();
const EVENT_TYPES: Array<keyof DirectorEngineEvents> = ["ready", "select", "camera-moved", "transform-end", "mode-rejected", "context-lost"];

function attach() {
  if (!canvasEl.value) return null;
  const engine = handle.attach(canvasEl.value, labelsEl.value, { fetchMediaBlob });
  for (const type of EVENT_TYPES) {
    engine.on(type, (payload) => {
      events.push({ type, payload, at: performance.now() });
      if (type === "transform-end") lastTransformEnd.value = JSON.stringify(payload);
      if (type === "ready") status.value = "就绪";
      if (type === "context-lost") status.value = "图形环境已重置";
      // 代替 store（DRT-14）：拖动 / 走位结束时把变换写回场景再回灌，与编辑器里「改数据 → setScene」的闭环一致。
      if (type === "transform-end" || type === "camera-moved") commit(payload as DirectorEngineEvents["transform-end"]);
    });
  }
  if (scene.value) engine.setScene(scene.value);
  return engine;
}

function commit({ id, transform }: DirectorEngineEvents["transform-end"]) {
  const current = scene.value;
  if (!current) return;
  setScene({
    ...current,
    objects: (current.objects ?? []).map((o) => (o.id === id ? { ...o, transform: { ...o.transform, ...transform } } : o))
  });
}

function setScene(next: DirectorScene) {
  const result = directorValidateScene(next);
  if (!result.ok) {
    status.value = `场景不合法：${result.path} ${result.message}`;
    return false;
  }
  scene.value = next;
  handle.current()?.setScene(next);
  return true;
}

// ---------------------------------------------------------------- 姿势缩略图（DRT-21）

const THUMB_ID = "pose_thumb";
const thumbBusy = ref(false);

/** 只有一个成年男素模的场景：背景透明由引擎处理，地面隐藏（不然缩略图里有一块地板），没有机位与时间轴。 */
function thumbnailScene(base: DirectorScene, code: string): DirectorScene {
  return {
    ...base,
    outputCameraId: null,
    ground: { ...(base.ground ?? {}), visible: false },
    timeline: { ...(base.timeline ?? { fps: 30, durationFrames: 150, loop: false }), tracks: [] },
    objects: [{
      id: THUMB_ID, name: "姿势", kind: "character", source: "user", visible: true, locked: false,
      transform: { position: { x: 0, y: 0, z: 0 }, rotation: { x: 0, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 } },
      character: { model: "adult_male", body: "standard", height: 1.8, posePreset: code }
    }]
  } as DirectorScene;
}

function blobToDataUrl(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(blob);
  });
}

/** 逐个姿势渲染缩略图；渲染完把场景换回原来那份。 */
async function renderPoseThumbnails(size = 256): Promise<Array<{ code: string; blob: Blob }>> {
  const engine = handle.current() as DirectorEngine | null;
  const base = scene.value;
  if (!engine || !base) throw new Error("沙盒还没准备好");
  const poses = await directorLoadPoses();
  const out: Array<{ code: string; blob: Blob }> = [];
  try {
    for (const code of poses.keys()) {
      if (!setScene(thumbnailScene(base, code))) throw new Error(`姿势 ${code} 的场景不合法`);
      out.push({ code, blob: await engine.renderThumbnail(THUMB_ID, { size }) });
      status.value = `缩略图 ${out.length} / ${poses.size}`;
    }
  } finally {
    setScene(base);
  }
  return out;
}

async function poseThumbnails(size = 256): Promise<Array<{ code: string; dataUrl: string }>> {
  const list = await renderPoseThumbnails(size);
  return Promise.all(list.map(async item => ({ code: item.code, dataUrl: await blobToDataUrl(item.blob) })));
}

type DirectoryPicker = () => Promise<{ getFileHandle(name: string, o: { create: boolean }): Promise<{ createWritable(): Promise<{ write(b: Blob): Promise<void>; close(): Promise<void> }> }> }>;

async function generateThumbnails() {
  if (thumbBusy.value) return;
  thumbBusy.value = true;
  try {
    const list = await renderPoseThumbnails();
    const pick = (window as unknown as { showDirectoryPicker?: DirectoryPicker }).showDirectoryPicker;
    if (pick) {
      const dir = await pick();
      for (const item of list) {
        const file = await (await dir.getFileHandle(`${item.code}.webp`, { create: true })).createWritable();
        await file.write(item.blob);
        await file.close();
      }
    } else {
      for (const item of list) {
        const a = document.createElement("a");
        a.href = URL.createObjectURL(item.blob);
        a.download = `${item.code}.webp`;
        a.click();
        setTimeout(() => URL.revokeObjectURL(a.href), 1000);
      }
    }
    status.value = `已生成 ${list.length} 张姿势缩略图；接着跑 node scripts/director-assets.mjs poses`;
  } catch (cause) {
    status.value = `生成缩略图失败：${cause instanceof Error ? cause.message : String(cause)}`;
  } finally {
    thumbBusy.value = false;
  }
}

async function loadScene(name = "default") {
  const response = await fetch(`/__director-sandbox/scene.json?name=${encodeURIComponent(name)}`);
  if (!response.ok) {
    status.value = `场景加载失败（HTTP ${response.status}）`;
    return false;
  }
  return setScene((await response.json()) as DirectorScene);
}

async function unmount() {
  handle.detach();
  mounted.value = false;
  await nextTick();
}

async function mount() {
  handle.detach();
  mounted.value = true;
  viewportKey.value++;
  await nextTick();
  return attach() !== null;
}

/** 重挂一次：释放旧引擎、换一个新的 canvas、挂新引擎（上下文丢失后同一个 canvas 不能再用）。 */
async function remount() {
  await unmount();
  return mount();
}

onMounted(async () => {
  (window as unknown as { __sandbox: unknown }).__sandbox = {
    engine: () => handle.current(),
    mount,
    unmount,
    remount,
    loadScene,
    setScene,
    scene: () => scene.value,
    events,
    poseThumbnails
  };
  const query = new URLSearchParams(window.location.search);
  attach();
  await loadScene(query.get("scene") ?? "default");
});

onBeforeUnmount(() => {
  handle.detach();
});
</script>

<template>
  <div class="director-sandbox">
    <header class="director-sandbox-bar">
      <strong>导演台沙盒</strong>
      <span class="director-sandbox-status" data-testid="status">{{ status }}</span>
      <code class="director-sandbox-payload" data-testid="transform-end">{{ lastTransformEnd || "（还没有 transform-end）" }}</code>
      <button type="button" class="director-sandbox-tool" :disabled="thumbBusy" @click="generateThumbnails">生成姿势缩略图</button>
    </header>
    <main v-if="mounted" :key="viewportKey" class="director-sandbox-viewport">
      <canvas ref="canvasEl" class="director-sandbox-canvas" />
      <div ref="labelsEl" class="director-sandbox-labels" />
    </main>
  </div>
</template>

<style scoped>
.director-sandbox {
  position: fixed;
  inset: 0;
  display: grid;
  grid-template-rows: 40px 1fr;
  background: var(--portal-bg);
  color: var(--portal-text);
}
.director-sandbox-bar {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 0 12px;
  border-bottom: 1px solid var(--portal-line);
  font-size: 13px;
  overflow: hidden;
}
.director-sandbox-status {
  color: var(--portal-muted);
}
.director-sandbox-payload {
  flex: 1;
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
  color: var(--portal-text-soft);
}
.director-sandbox-tool {
  flex: 0 0 auto;
  padding: 4px 10px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 6px;
  color: var(--portal-text);
  background: var(--portal-panel);
  font-size: 12px;
  cursor: pointer;
}
.director-sandbox-viewport {
  position: relative;
  min-height: 0;
  overflow: hidden;
}
.director-sandbox-canvas {
  display: block;
  width: 100%;
  height: 100%;
}
.director-sandbox-labels {
  position: absolute;
  left: 0;
  top: 0;
  pointer-events: none;
}
</style>
