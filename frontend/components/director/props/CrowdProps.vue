<script setup lang="ts">
/**
 * 群众属性（DRT-22，DR-F-50 / 13）：行列（1~10，乘积不超过 100）、间距（0.5~5 米）、素模、统一姿势、错位与「换一组随机」。
 *
 * - 行列用步进器：乘积超过单阵上限时先拦下并说「单阵最多 100 人」，再看单轴上限 —— 10 × 10 时点「+」，
 *   用户想要的是「再多一排人」，该告诉他的是人数上限，而不是「行数最多 10」；
 * - 错位由 `seed` 决定（同一种子重开不变），「换一组随机」就是 seed + 1；不错位时种子不起作用，按钮置灰并说明；
 * - 每一项都是一次提交（一步撤销）；间距滑块松手时提交（`change` 不是 `input`）。
 * 姿势目录先用 `DIRECTOR_POSES`（姿势库面板在 DRT-21，群众只用预设、不调关节）。
 */
import { ElMessage } from "element-plus";
import { Minus, Plus, Shuffle } from "@lucide/vue";
import {
  DIRECTOR_LIMITS,
  DIRECTOR_MODELS,
  DIRECTOR_OBJECT_DEFAULTS,
  DIRECTOR_POSE_CATEGORIES,
  DIRECTOR_POSES
} from "~/utils/directorCatalog";
import { directorCrowdSizeError, directorLimitText, directorPatchObject, type DirectorObject } from "~/utils/directorScene";
import NumberField from "./NumberField.vue";

const props = defineProps<{ object: DirectorObject; readonly: boolean }>();
const store = useDirectorStore();

/** 种子的取值范围与场景校验一致（0 ~ 2³¹ − 1）：加到头就回到 0。 */
const MAX_SEED = 2147483647;
type CrowdSub = { model: string; rows: number; cols: number; spacing: number; posePreset: string; jitter: boolean; seed: number };
const crowd = computed(() => ({ ...DIRECTOR_OBJECT_DEFAULTS.crowd, ...(props.object.crowd ?? {}) }) as CrowdSub);
const count = computed(() => crowd.value.rows * crowd.value.cols);
const atLimit = computed(() => count.value >= DIRECTOR_LIMITS.maxCrowdSize);
const posesByCategory = computed(() => DIRECTOR_POSE_CATEGORIES.map(category => ({
  ...category,
  poses: DIRECTOR_POSES.filter(pose => pose.category === category.code)
})));

function patch(sub: Record<string, unknown>) {
  store.commit(scene => directorPatchObject(scene, props.object.id, "crowd", sub), { touched: [props.object.id] });
}

/** 改行数或列数（步进器与数值框共用）：先看总人数，再看单轴上限；拦下时说明，不改。 */
function setAxis(axis: "rows" | "cols", value: number) {
  const next = Math.round(value);
  if (!Number.isFinite(next) || next < 1 || next === crowd.value[axis]) return;
  const rows = axis === "rows" ? next : crowd.value.rows;
  const cols = axis === "cols" ? next : crowd.value.cols;
  const size = directorCrowdSizeError(rows, cols);
  if (size) {
    ElMessage.warning(directorLimitText(size));
    return;
  }
  const max = axis === "rows" ? DIRECTOR_LIMITS.maxCrowdRows : DIRECTOR_LIMITS.maxCrowdCols;
  if (next > max) {
    ElMessage.warning(`${axis === "rows" ? "行数" : "列数"}最多 ${max}`);
    return;
  }
  patch({ [axis]: next });
}

function reshuffle() {
  if (!crowd.value.jitter) return;
  patch({ seed: crowd.value.seed >= MAX_SEED ? 0 : crowd.value.seed + 1 });
}
</script>

<template>
  <section class="dr-section" aria-label="群众">
    <h3>群众 <span class="dr-unit" data-testid="crowd-count">共 {{ count }} 人</span></h3>
    <div v-for="axis in (['rows', 'cols'] as const)" :key="axis" class="dr-field">
      <span>{{ axis === 'rows' ? '行数' : '列数' }}</span>
      <div class="dr-range crowd-stepper">
        <button type="button" class="dr-btn" :disabled="readonly || crowd[axis] <= 1" :aria-label="`${axis === 'rows' ? '行数' : '列数'}减一`" :title="readonly ? '你在该工作区是只读' : '减一'" @click="setAxis(axis, crowd[axis] - 1)"><Minus :size="12" /></button>
        <NumberField :model-value="crowd[axis]" :min="1" :max="DIRECTOR_LIMITS.maxCrowdRows" :digits="0" :disabled="readonly" :label="axis === 'rows' ? '行数' : '列数'" @commit="setAxis(axis, $event)" />
        <button type="button" class="dr-btn" :disabled="readonly" :aria-label="`${axis === 'rows' ? '行数' : '列数'}加一`" :data-testid="`crowd-${axis}-plus`" :title="readonly ? '你在该工作区是只读' : '加一'" @click="setAxis(axis, crowd[axis] + 1)"><Plus :size="12" /></button>
      </div>
    </div>
    <p v-if="atLimit" class="dr-hint">已到单阵上限 {{ DIRECTOR_LIMITS.maxCrowdSize }} 人；要更多人可以再加一个阵列。</p>
    <div class="dr-field">
      <span>间距</span>
      <div class="dr-range">
        <input
          type="range"
          min="0.5"
          max="5"
          step="0.1"
          :value="crowd.spacing"
          :disabled="readonly"
          aria-label="间距（米）"
          @change="patch({ spacing: Number(($event.target as HTMLInputElement).value) })"
        >
        <NumberField :model-value="crowd.spacing" :min="0.5" :max="5" :digits="2" :disabled="readonly" label="间距（米）" @commit="patch({ spacing: $event })" />
        <span class="dr-unit">米</span>
      </div>
    </div>
    <div class="dr-field">
      <span>素模</span>
      <div class="dr-seg" role="radiogroup" aria-label="素模">
        <button
          v-for="item in DIRECTOR_MODELS"
          :key="item.code"
          type="button"
          role="radio"
          :aria-checked="item.code === crowd.model"
          :class="{ active: item.code === crowd.model }"
          :disabled="readonly"
          @click="patch({ model: item.code })"
        >{{ item.name }}</button>
      </div>
    </div>
    <div class="dr-field">
      <span>姿势</span>
      <select class="dr-select" :value="crowd.posePreset" :disabled="readonly" aria-label="统一姿势" @change="patch({ posePreset: ($event.target as HTMLSelectElement).value })">
        <optgroup v-for="group in posesByCategory" :key="group.code" :label="group.name">
          <option v-for="pose in group.poses" :key="pose.code" :value="pose.code" :title="pose.description">{{ pose.name }}</option>
        </optgroup>
      </select>
    </div>
    <div class="dr-field">
      <label class="dr-check"><input type="checkbox" :checked="crowd.jitter" :disabled="readonly" aria-label="随机错位" @change="patch({ jitter: ($event.target as HTMLInputElement).checked })">错位</label>
      <button
        type="button"
        class="dr-btn"
        :disabled="readonly || !crowd.jitter"
        :title="readonly ? '你在该工作区是只读' : crowd.jitter ? '换一组随机的位置与朝向（同一组随机重开不变）' : '先打开错位'"
        data-testid="crowd-reshuffle"
        @click="reshuffle"
      ><Shuffle :size="12" />换一组随机</button>
    </div>
    <p class="dr-hint">错位：每人随机偏移不超过间距的 1/4、朝向不超过 ±30°；同一组随机重开不变。</p>
  </section>
</template>

<style scoped>
.crowd-stepper .dr-btn { width: 26px; padding: 0; }
</style>
