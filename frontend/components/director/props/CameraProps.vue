<script setup lang="ts">
/**
 * 机位属性（DRT-15，DR-F-23 / 24 / 26）：焦距（滑块 + 快捷档）、画幅、设为输出机位、对准某个对象、
 * 用当前导演视角新建机位；高级里是传感器宽度、近 / 远裁剪、横滚。
 *
 * 「对准」「新建」要用引擎（对象中心、当前视角），所以只发事件，由外壳调引擎再提交。
 * 焦距滑块松手时提交一次；快捷档点一下提交一次。
 * 镜头文字（DRT-26，DR-F-130 / 131）：按播放头这一帧算，对象一动就跟着重算；「复制」写进剪贴板，**不自动写进任何提示词**（DR-F-132）。
 */
import { ElMessage } from "element-plus";
import { ChevronDown, ChevronRight, Copy, Crosshair, Video } from "@lucide/vue";
import { directorDisplayedScene } from "~/utils/directorAnimation";
import { DIRECTOR_FOCAL_MAX, DIRECTOR_FOCAL_MIN, DIRECTOR_FOCAL_PRESETS, directorShotText } from "~/utils/directorCamera";
import { DIRECTOR_ASPECTS, DIRECTOR_OBJECT_DEFAULTS } from "~/utils/directorCatalog";
import { directorPatchObject, directorSetOutputCamera, type DirectorObject } from "~/utils/directorScene";
import NumberField from "./NumberField.vue";

const props = defineProps<{ object: DirectorObject; readonly: boolean }>();
const emit = defineEmits<{ aim: [targetId: string]; "camera-from-view": [] }>();
const store = useDirectorStore();

type CameraSub = { focalLengthMm?: number; sensorWidthMm?: number; aspect?: string; near?: number; far?: number; roll?: number };
const camera = computed(() => ({ ...DIRECTOR_OBJECT_DEFAULTS.camera, ...(props.object.camera ?? {}) }) as Required<CameraSub>);
const isOutput = computed(() => store.scene?.outputCameraId === props.object.id);
const targets = computed(() => (store.scene?.objects ?? []).filter(o => o.kind !== "camera"));
const advancedOpen = ref(false);
const aimTarget = ref("");
const shotText = computed(() => (store.scene ? directorShotText({ scene: directorDisplayedScene(store.scene, store.playhead), cameraId: props.object.id }) : ""));

async function copyShotText() {
  try {
    await navigator.clipboard.writeText(shotText.value);
    ElMessage.success("已复制镜头文字");
  } catch {
    ElMessage.warning("浏览器不允许写剪贴板，请手动选中复制");
  }
}

function patch(sub: Record<string, unknown>) {
  store.commit(scene => directorPatchObject(scene, props.object.id, "camera", sub), { touched: [props.object.id] });
}

function setOutput() {
  store.commit(scene => directorSetOutputCamera(scene, props.object.id));
}

function aim() {
  if (aimTarget.value) emit("aim", aimTarget.value);
}
</script>

<template>
  <section class="dr-section" aria-label="机位">
    <h3>机位
      <span v-if="isOutput" class="dr-unit">输出机位</span>
    </h3>
    <div class="dr-field camera-shot">
      <span>镜头</span>
      <div class="camera-shot-text">
        <strong data-testid="shot-text">{{ shotText || "—" }}</strong>
        <button type="button" class="dr-btn" :disabled="!shotText" title="复制镜头文字" aria-label="复制镜头文字" @click="copyShotText"><Copy :size="13" /></button>
      </div>
    </div>
    <p class="dr-hint">角度按机位相对画面中人物的高低判断：机位低于人物是仰拍，高于人物是俯拍（不是相机自己的俯仰）。</p>
    <div class="dr-field">
      <span>焦距</span>
      <div class="dr-range">
        <input
          type="range"
          :min="DIRECTOR_FOCAL_MIN"
          :max="DIRECTOR_FOCAL_MAX"
          step="1"
          :value="camera.focalLengthMm"
          :disabled="readonly"
          aria-label="焦距（毫米）"
          @change="patch({ focalLengthMm: Number(($event.target as HTMLInputElement).value) })"
        >
        <NumberField :model-value="camera.focalLengthMm" :min="DIRECTOR_FOCAL_MIN" :max="DIRECTOR_FOCAL_MAX" :digits="0" :disabled="readonly" label="焦距（毫米）" @commit="patch({ focalLengthMm: $event })" />
        <span class="dr-unit">毫米</span>
      </div>
    </div>
    <div class="dr-seg" aria-label="焦距快捷档">
      <button
        v-for="mm in DIRECTOR_FOCAL_PRESETS"
        :key="mm"
        type="button"
        :class="{ active: Math.abs(camera.focalLengthMm - mm) < 0.5 }"
        :disabled="readonly"
        :title="`${mm} 毫米`"
        @click="patch({ focalLengthMm: mm })"
      >{{ mm }}</button>
    </div>
    <div class="dr-field">
      <span>画幅</span>
      <div class="dr-seg" role="radiogroup" aria-label="画幅">
        <button
          v-for="item in DIRECTOR_ASPECTS"
          :key="item.code"
          type="button"
          role="radio"
          :aria-checked="item.code === camera.aspect"
          :class="{ active: item.code === camera.aspect }"
          :disabled="readonly"
          @click="patch({ aspect: item.code })"
        >{{ item.code }}</button>
      </div>
    </div>
    <div class="dr-seg">
      <button type="button" class="dr-btn" :disabled="readonly || isOutput" :title="isOutput ? '已是输出机位' : '下游拿到的截图取自输出机位'" @click="setOutput">
        <Video :size="13" />{{ isOutput ? "已是输出机位" : "设为输出机位" }}
      </button>
      <button type="button" class="dr-btn" :disabled="readonly" title="在当前导演视角的位置与朝向新建一个机位" @click="emit('camera-from-view')">
        用当前导演视角新建机位
      </button>
    </div>
    <div class="dr-field">
      <span>对准</span>
      <div class="dr-range">
        <select v-model="aimTarget" class="dr-select" :disabled="readonly || !targets.length" aria-label="对准哪个对象">
          <option value="">选择对象</option>
          <option v-for="item in targets" :key="item.id" :value="item.id">{{ item.name }}</option>
        </select>
        <button type="button" class="dr-btn" :disabled="readonly || !aimTarget" title="机位朝向所选对象的中心" aria-label="对准" @click="aim"><Crosshair :size="13" /></button>
      </div>
    </div>
    <button type="button" class="dr-btn" :aria-expanded="advancedOpen" @click="advancedOpen = !advancedOpen">
      <ChevronDown v-if="advancedOpen" :size="13" /><ChevronRight v-else :size="13" />高级
    </button>
    <template v-if="advancedOpen">
      <div class="dr-field">
        <span>传感器宽度</span>
        <div class="dr-range">
          <NumberField :model-value="camera.sensorWidthMm" :min="10" :max="70" :digits="1" :disabled="readonly" label="传感器宽度（毫米，指长边）" @commit="patch({ sensorWidthMm: $event })" />
          <span class="dr-unit">毫米</span>
        </div>
      </div>
      <div class="dr-field">
        <span>近裁剪</span>
        <div class="dr-range">
          <NumberField :model-value="camera.near" :min="0.01" :max="10" :disabled="readonly" label="近裁剪（米）" @commit="patch({ near: $event })" />
          <span class="dr-unit">米</span>
        </div>
      </div>
      <div class="dr-field">
        <span>远裁剪</span>
        <div class="dr-range">
          <NumberField :model-value="camera.far" :min="10" :max="10000" :digits="0" :disabled="readonly" label="远裁剪（米）" @commit="patch({ far: $event })" />
          <span class="dr-unit">米</span>
        </div>
      </div>
      <div class="dr-field">
        <span>横滚</span>
        <div class="dr-range">
          <NumberField :model-value="camera.roll" :min="-180" :max="180" :digits="1" :disabled="readonly" label="横滚（度）" @commit="patch({ roll: $event })" />
          <span class="dr-unit">度</span>
        </div>
      </div>
      <p class="dr-hint">传感器宽度指长边（竖幅时是高）；视角由焦距与传感器宽度推导。</p>
    </template>
  </section>
</template>

<style scoped>
.camera-shot-text { display: flex; align-items: center; gap: 6px; min-width: 0; }
.camera-shot-text strong { min-width: 0; overflow-wrap: anywhere; font-weight: 500; }
</style>
