<script setup lang="ts">
/**
 * 场景设置（DRT-15，右侧「场景」页）：场景整体变换（DR-F-60）、背景（DR-F-61）、地面（DR-F-62）、
 * 灯光（DR-F-63）、视图（角色标签 DR-F-44、网格 / 角度 / 地面吸附 DR-F-31 / 32）。
 *
 * - 每一项失焦 / 回车 / 松手时提交一次（一步撤销）；
 * - 全景从上游图片里选（DR-F-02，列表由画布经 props 传入）；没有可用产出的上游置灰并说明；
 * - 选了全景而地面不透明时提示「地面会挡住全景的下半部分」并给「隐藏地面」—— 不自动改用户的地面（dev/03 R-5 #13）；
 * - 「在视口里调整」打开时，外壳在视口上拖动改水平旋转、滚轮改半径，松手提交一次（DR-F-61）。
 * - 3D 背景（泼溅，DRT-28）：导入 .spz / .ply（前端先按扩展名与 100 MB 粗筛，内容由服务端按文件头判断），
 *   上传成功后写进场景；替换保留原来的变换；位置 / 旋转 / 缩放 / 显示即时生效。没有「生成 3D 世界」的入口（DRK-14）。
 */
import type { components } from "@tabtv/api-client";
import { DIRECTOR_OBJECT_DEFAULTS } from "~/utils/directorCatalog";
import { directorPatchSplat, directorRemoveSplat, directorSetSplat, directorSplatFileCheck, directorSplatOf, directorSplatUploadErrorText } from "~/utils/directorSplat";
import { directorPatchLight, directorPatchSettings, type DirectorSettingsKey, type DirectorVec3 } from "~/utils/directorScene";
import type { DirectorUpstreamImage } from "~/utils/directorCanvas";
import NumberField from "./props/NumberField.vue";

const props = defineProps<{ upstreamImages: DirectorUpstreamImage[]; readonly: boolean; adjustPanorama: boolean }>();
const emit = defineEmits<{ "update:adjustPanorama": [on: boolean] }>();
const store = useDirectorStore();

const D = DIRECTOR_OBJECT_DEFAULTS.scene;
const AXES = ["x", "y", "z"] as const;
type Json = Record<string, unknown>;
const section = (key: string, defaults: unknown) => computed(() => ({ ...(defaults as Json), ...((store.scene?.[key] ?? {}) as Json) }));
const stage = section("stage", D.stage);
const background = section("background", D.background);
const ground = section("ground", D.ground);
const view = section("view", D.view);
const lights = computed(() => {
  const value = (store.scene?.lights ?? {}) as Record<string, Json>;
  return { key: { ...D.lights.key, ...(value.key ?? {}) }, fill: { ...D.lights.fill, ...(value.fill ?? {}) } };
});
const panorama = computed(() => (background.value.panorama ?? null) as { sourceNodeId?: string; mediaId?: string; yaw?: number; radius?: number } | null);
const panoramaMode = computed(() => background.value.mode === "panorama");
const groundBlocksPanorama = computed(() => panoramaMode.value && ground.value.visible !== false && Number(ground.value.opacity ?? 1) >= 1);
const gridSnap = computed(() => ({ enabled: false, step: 0.5, ...((view.value.gridSnap ?? {}) as Json) }));
const angleSnap = computed(() => ({ enabled: false, step: 15, ...((view.value.angleSnap ?? {}) as Json) }));

function patch(key: DirectorSettingsKey, value: Json) {
  store.commit(scene => directorPatchSettings(scene, key, value));
}

function patchVec(key: "offset" | "rotation", axis: (typeof AXES)[number], value: number) {
  const current = { x: 0, y: 0, z: 0, ...((stage.value[key] ?? {}) as Partial<DirectorVec3>) };
  patch("stage", { [key]: { ...current, [axis]: value } });
}

function light(which: "key" | "fill", value: Json) {
  store.commit(scene => directorPatchLight(scene, which, value));
}

function choosePanorama(sourceNodeId: string) {
  const upstream = props.upstreamImages.find(item => item.sourceNodeId === sourceNodeId);
  if (!upstream?.mediaId) return;
  const current = panorama.value;
  patch("background", {
    mode: "panorama",
    panorama: { sourceNodeId, mediaId: upstream.mediaId, yaw: current?.yaw ?? 0, radius: current?.radius ?? 50 }
  });
}

function setMode(mode: "color" | "panorama") {
  if (mode === "color") {
    emit("update:adjustPanorama", false);
    patch("background", { mode: "color" });
    return;
  }
  const first = props.upstreamImages.find(item => item.mediaId);
  if (panorama.value?.mediaId) patch("background", { mode: "panorama" });
  else if (first) choosePanorama(first.sourceNodeId);
}

function patchPanorama(value: Json) {
  if (!panorama.value) return;
  patch("background", { panorama: { ...panorama.value, ...value } });
}

// ---------------------------------------------------------------- 3D 背景（泼溅，DRT-28）

const splat = computed(() => directorSplatOf(store.scene));
const splatInput = ref<HTMLInputElement | null>(null);
const splatUploading = ref(false);
const splatMessage = ref("");

function pickSplat() {
  if (props.readonly || splatUploading.value) return;
  splatMessage.value = "";
  splatInput.value?.click();
}

async function onSplatFile(event: Event) {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  input.value = "";
  if (!file) return;
  const check = directorSplatFileCheck(file);
  if (!check.ok) {
    splatMessage.value = check.text;
    return;
  }
  const form = new FormData();
  form.append("file", file, file.name);
  form.append("kind", "splat");
  splatUploading.value = true;
  try {
    const saved = await useApi()<components["schemas"]["DirectorMediaRecord"]>(
      `/projects/${store.projectId}/director-scenes/${store.sceneId}/assets`,
      { method: "POST", body: form }
    );
    store.commit(scene => directorSetSplat(scene, String(saved.mediaId)));
  } catch (cause) {
    splatMessage.value = directorSplatUploadErrorText(cause);
  } finally {
    splatUploading.value = false;
  }
}

function patchSplat(value: Parameters<typeof directorPatchSplat>[1]) {
  store.commit(scene => directorPatchSplat(scene, value));
}

function patchSplatVec(key: "position" | "rotation", axis: (typeof AXES)[number], value: number) {
  const current = splat.value;
  if (current) patchSplat({ [key]: { ...current[key], [axis]: value } });
}
</script>

<template>
  <div class="dr-panel" aria-label="场景设置">
    <section class="dr-section">
      <h3>场景整体</h3>
      <div class="dr-field">
        <span>缩放</span>
        <div class="dr-range">
          <NumberField :model-value="Number(stage.scale ?? 1) * 100" :min="10" :max="1000" :digits="0" :disabled="readonly" label="场景整体缩放（百分比）" @commit="patch('stage', { scale: $event / 100 })" />
          <span class="dr-unit">%</span>
        </div>
      </div>
      <h3>平移 <span class="dr-unit">米</span></h3>
      <div class="dr-vec">
        <label v-for="axis in AXES" :key="`o-${axis}`">{{ axis.toUpperCase() }}
          <NumberField :model-value="Number(((stage.offset ?? {}) as Partial<DirectorVec3>)[axis] ?? 0)" :disabled="readonly" :label="`场景平移 ${axis.toUpperCase()}（米）`" @commit="patchVec('offset', axis, $event)" />
        </label>
      </div>
      <h3>旋转 <span class="dr-unit">度</span></h3>
      <div class="dr-vec">
        <label v-for="axis in AXES" :key="`r-${axis}`">{{ axis.toUpperCase() }}
          <NumberField :model-value="Number(((stage.rotation ?? {}) as Partial<DirectorVec3>)[axis] ?? 0)" :digits="1" :disabled="readonly" :label="`场景旋转 ${axis.toUpperCase()}（度）`" @commit="patchVec('rotation', axis, $event)" />
        </label>
      </div>
    </section>

    <section class="dr-section">
      <h3>背景</h3>
      <div class="dr-seg" role="radiogroup" aria-label="背景">
        <button type="button" role="radio" :aria-checked="!panoramaMode" :class="{ active: !panoramaMode }" :disabled="readonly" @click="setMode('color')">纯色</button>
        <button
          type="button"
          role="radio"
          :aria-checked="panoramaMode"
          :class="{ active: panoramaMode }"
          :disabled="readonly || !upstreamImages.some(item => item.mediaId)"
          :title="upstreamImages.some(item => item.mediaId) ? '用连进来的图片作全景背景' : '先把一张图片节点连到导演台的左侧输入口'"
          @click="setMode('panorama')"
        >全景图</button>
      </div>
      <div v-if="!panoramaMode" class="dr-field">
        <span>颜色</span>
        <input class="dr-color" type="color" :value="String(background.color ?? '#060608')" :disabled="readonly" aria-label="背景颜色" @change="patch('background', { color: ($event.target as HTMLInputElement).value })">
      </div>
      <template v-else>
        <div class="dr-field">
          <span>来源</span>
          <select class="dr-select" :value="panorama?.sourceNodeId ?? ''" :disabled="readonly" aria-label="全景来源" @change="choosePanorama(($event.target as HTMLSelectElement).value)">
            <option v-for="item in upstreamImages" :key="item.sourceNodeId" :value="item.sourceNodeId" :disabled="!item.mediaId">
              {{ item.order }}. {{ item.title }}{{ item.mediaId ? "" : "（无可用产出）" }}
            </option>
          </select>
        </div>
        <div class="dr-field">
          <span>水平旋转</span>
          <div class="dr-range">
            <NumberField :model-value="Number(panorama?.yaw ?? 0)" :min="0" :max="360" :digits="0" :disabled="readonly" label="全景水平旋转（度）" @commit="patchPanorama({ yaw: $event })" />
            <span class="dr-unit">度</span>
          </div>
        </div>
        <div class="dr-field">
          <span>球半径</span>
          <div class="dr-range">
            <NumberField :model-value="Number(panorama?.radius ?? 50)" :min="5" :max="200" :digits="0" :disabled="readonly" label="全景球半径（米）" @commit="patchPanorama({ radius: $event })" />
            <span class="dr-unit">米</span>
          </div>
        </div>
        <label class="dr-check">
          <input type="checkbox" :checked="adjustPanorama" :disabled="readonly" @change="emit('update:adjustPanorama', ($event.target as HTMLInputElement).checked)">
          在视口里调整（拖动转、滚轮改半径）
        </label>
        <p v-if="groundBlocksPanorama" class="dr-note">
          地面不透明，会挡住全景的下半部分。
          <button type="button" class="dr-btn" :disabled="readonly" @click="patch('ground', { visible: false })">隐藏地面</button>
        </p>
      </template>
    </section>

    <section class="dr-section" aria-label="3D 背景（泼溅）" data-testid="splat-settings">
      <h3>3D 背景（泼溅）</h3>
      <input ref="splatInput" type="file" accept=".spz,.ply" hidden data-testid="splat-file" @change="onSplatFile">
      <template v-if="!splat">
        <button type="button" class="dr-btn" :disabled="readonly || splatUploading" data-testid="splat-import" @click="pickSplat">
          {{ splatUploading ? "正在上传…" : "导入泼溅文件" }}
        </button>
        <p class="dr-hint">支持 .spz / .ply，最大 100 MB（PLY 建议先转成 SPZ）。可以与纯色或全景背景同时使用。</p>
      </template>
      <template v-else>
        <label class="dr-check"><input type="checkbox" :checked="splat.visible" :disabled="readonly" data-testid="splat-visible" @change="patchSplat({ visible: ($event.target as HTMLInputElement).checked })">显示泼溅</label>
        <h3>位置 <span class="dr-unit">米</span></h3>
        <div class="dr-vec">
          <label v-for="axis in AXES" :key="`sp-${axis}`">{{ axis.toUpperCase() }}
            <NumberField :model-value="splat.position[axis]" :disabled="readonly" :label="`泼溅位置 ${axis.toUpperCase()}（米）`" @commit="patchSplatVec('position', axis, $event)" />
          </label>
        </div>
        <h3>旋转 <span class="dr-unit">度</span></h3>
        <div class="dr-vec">
          <label v-for="axis in AXES" :key="`sr-${axis}`">{{ axis.toUpperCase() }}
            <NumberField :model-value="splat.rotation[axis]" :digits="1" :disabled="readonly" :label="`泼溅旋转 ${axis.toUpperCase()}（度）`" @commit="patchSplatVec('rotation', axis, $event)" />
          </label>
        </div>
        <div class="dr-field">
          <span>缩放</span>
          <NumberField :model-value="splat.scale" :min="0.01" :max="100" :digits="2" :disabled="readonly" label="泼溅缩放（倍）" @commit="patchSplat({ scale: $event })" />
        </div>
        <div class="dr-seg">
          <button type="button" :disabled="readonly || splatUploading" data-testid="splat-replace" @click="pickSplat">{{ splatUploading ? "正在上传…" : "替换文件" }}</button>
          <button type="button" :disabled="readonly" data-testid="splat-remove" @click="store.commit(scene => directorRemoveSplat(scene))">移除</button>
        </div>
        <p class="dr-hint">泼溅只当背景参考：不能在视口里点选拖动；人物与泼溅的前后遮挡在某些角度可能不准。地面吸附会把人物贴到脚下的泼溅表面。</p>
      </template>
      <p v-if="splatMessage" class="dr-note" role="alert" data-testid="splat-message">{{ splatMessage }}</p>
    </section>

    <section class="dr-section">
      <h3>地面</h3>
      <label class="dr-check"><input type="checkbox" :checked="ground.visible !== false" :disabled="readonly" @change="patch('ground', { visible: ($event.target as HTMLInputElement).checked })">显示地面</label>
      <div class="dr-field">
        <span>透明度</span>
        <NumberField :model-value="Number(ground.opacity ?? 1)" :min="0" :max="1" :digits="2" :disabled="readonly" label="地面透明度（0~1）" @commit="patch('ground', { opacity: $event })" />
      </div>
      <div class="dr-field">
        <span>高度</span>
        <div class="dr-range">
          <NumberField :model-value="Number(ground.height ?? 0)" :min="-100" :max="100" :disabled="readonly" label="地面高度（米）" @commit="patch('ground', { height: $event })" />
          <span class="dr-unit">米</span>
        </div>
      </div>
    </section>

    <section v-for="which in (['key', 'fill'] as const)" :key="which" class="dr-section">
      <h3>{{ which === 'key' ? '主光' : '补光' }}</h3>
      <label class="dr-check"><input type="checkbox" :checked="lights[which].visible !== false" :disabled="readonly" @change="light(which, { visible: ($event.target as HTMLInputElement).checked })">开启</label>
      <div class="dr-field">
        <span>强度</span>
        <NumberField :model-value="Number(lights[which].intensity)" :min="0" :max="10" :digits="2" :disabled="readonly" :label="`${which === 'key' ? '主光' : '补光'}强度`" @commit="light(which, { intensity: $event })" />
      </div>
      <div class="dr-field">
        <span>颜色</span>
        <input class="dr-color" type="color" :value="String(lights[which].color)" :disabled="readonly" :aria-label="`${which === 'key' ? '主光' : '补光'}颜色`" @change="light(which, { color: ($event.target as HTMLInputElement).value })">
      </div>
      <div class="dr-field">
        <span>水平角</span>
        <div class="dr-range">
          <NumberField :model-value="Number(lights[which].azimuth)" :min="0" :max="360" :digits="0" :disabled="readonly" :label="`${which === 'key' ? '主光' : '补光'}水平角（度）`" @commit="light(which, { azimuth: $event })" />
          <span class="dr-unit">度</span>
        </div>
      </div>
      <div class="dr-field">
        <span>俯仰角</span>
        <div class="dr-range">
          <NumberField :model-value="Number(lights[which].elevation)" :min="-90" :max="90" :digits="0" :disabled="readonly" :label="`${which === 'key' ? '主光' : '补光'}俯仰角（度）`" @commit="light(which, { elevation: $event })" />
          <span class="dr-unit">度</span>
        </div>
      </div>
    </section>

    <section class="dr-section">
      <h3>视图</h3>
      <label class="dr-check"><input type="checkbox" :checked="view.showLabels !== false" :disabled="readonly" @change="patch('view', { showLabels: ($event.target as HTMLInputElement).checked })">显示角色名称标签</label>
      <label class="dr-check"><input type="checkbox" :checked="view.groundSnap !== false" :disabled="readonly" @change="patch('view', { groundSnap: ($event.target as HTMLInputElement).checked })">地面吸附（脚底、道具底面贴到下方表面）</label>
      <div class="dr-field">
        <label class="dr-check"><input type="checkbox" :checked="gridSnap.enabled === true" :disabled="readonly" @change="patch('view', { gridSnap: { ...gridSnap, enabled: ($event.target as HTMLInputElement).checked } })">网格吸附</label>
        <div class="dr-seg" aria-label="网格步长">
          <button v-for="step in [0.1, 0.25, 0.5, 1]" :key="step" type="button" :class="{ active: gridSnap.step === step }" :disabled="readonly" :title="`${step} 米`" @click="patch('view', { gridSnap: { ...gridSnap, step } })">{{ step }}</button>
        </div>
      </div>
      <div class="dr-field">
        <label class="dr-check"><input type="checkbox" :checked="angleSnap.enabled === true" :disabled="readonly" @change="patch('view', { angleSnap: { ...angleSnap, enabled: ($event.target as HTMLInputElement).checked } })">角度吸附</label>
        <div class="dr-seg" aria-label="角度步长">
          <button v-for="step in [5, 15, 45]" :key="step" type="button" :class="{ active: angleSnap.step === step }" :disabled="readonly" :title="`${step} 度`" @click="patch('view', { angleSnap: { ...angleSnap, step } })">{{ step }}°</button>
        </div>
      </div>
    </section>
  </div>
</template>
