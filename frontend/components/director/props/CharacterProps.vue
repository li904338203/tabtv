<script setup lang="ts">
/**
 * 角色属性（DRT-15，DR-F-40 / 41）：素模、身高、体型；姿势只显示当前是哪个，姿势库与关节编辑在「姿势」页签（DRT-21，PosePanel）。
 *
 * - 换素模时：身高还是上一个素模的默认值，就跟着换成新素模的默认值；用户调过的身高保留；
 * - 身高滑块松手时提交一次（`change` 不是 `input`），否则拖一下就是几十步撤销。
 */
import { ScanSearch } from "@lucide/vue";
import { DIRECTOR_BODIES, DIRECTOR_MODELS, DIRECTOR_POSES, directorModelOf } from "~/utils/directorCatalog";
import { directorPatchObject, type DirectorObject } from "~/utils/directorScene";
import NumberField from "./NumberField.vue";

const props = defineProps<{ object: DirectorObject; readonly: boolean }>();
const emit = defineEmits<{ "open-poses": []; "pose-from-image": [] }>();
const store = useDirectorStore();

type CharacterSub = { model?: string; height?: number; body?: string; posePreset?: string; boneOverrides?: Record<string, unknown> };
const character = computed(() => (props.object.character ?? {}) as CharacterSub);
const model = computed(() => directorModelOf(String(character.value.model ?? "")) ?? DIRECTOR_MODELS[0]!);
const height = computed(() => Number(character.value.height ?? model.value.heightM));
const overrideCount = computed(() => Object.keys(character.value.boneOverrides ?? {}).length);
// 没有预设码 + 有覆盖 = 自己摆的姿势（参考图识别的结果就是这样存的，DRT-23）
const poseName = computed(() => DIRECTOR_POSES.find(pose => pose.code === character.value.posePreset)?.name
  ?? (character.value.posePreset ? `${character.value.posePreset}（暂不支持）` : overrideCount.value ? "自定义姿势" : "自然站立"));

function patch(sub: Record<string, unknown>) {
  store.commit(scene => directorPatchObject(scene, props.object.id, "character", sub), { touched: [props.object.id] });
}

function changeModel(code: string) {
  const next = directorModelOf(code);
  if (!next || next.code === model.value.code) return;
  const followDefault = Math.abs(height.value - model.value.heightM) < 1e-6;
  patch({ model: next.code, ...(followDefault ? { height: next.heightM } : {}) });
}

</script>

<template>
  <section class="dr-section" aria-label="角色">
    <h3>角色</h3>
    <div class="dr-field">
      <span>素模</span>
      <div class="dr-seg" role="radiogroup" aria-label="素模">
        <button
          v-for="item in DIRECTOR_MODELS"
          :key="item.code"
          type="button"
          role="radio"
          :aria-checked="item.code === model.code"
          :class="{ active: item.code === model.code }"
          :disabled="readonly"
          @click="changeModel(item.code)"
        >{{ item.name }}</button>
      </div>
    </div>
    <div class="dr-field">
      <span>身高</span>
      <div class="dr-range">
        <input
          type="range"
          min="0.8"
          max="2.3"
          step="0.01"
          :value="height"
          :disabled="readonly"
          aria-label="身高（米）"
          @change="patch({ height: Number(($event.target as HTMLInputElement).value) })"
        >
        <NumberField :model-value="height" :min="0.8" :max="2.3" :digits="2" :disabled="readonly" label="身高（米）" @commit="patch({ height: $event })" />
        <span class="dr-unit">米</span>
      </div>
    </div>
    <div class="dr-field">
      <span>体型</span>
      <div class="dr-seg" role="radiogroup" aria-label="体型">
        <button
          v-for="item in DIRECTOR_BODIES"
          :key="item.code"
          type="button"
          role="radio"
          :aria-checked="item.code === (character.body ?? 'standard')"
          :class="{ active: item.code === (character.body ?? 'standard') }"
          :disabled="readonly"
          @click="patch({ body: item.code })"
        >{{ item.name }}</button>
      </div>
    </div>
    <div class="dr-field">
      <span>姿势</span>
      <div class="character-pose">
        <strong>{{ poseName }}</strong><small v-if="overrideCount">（手动调整 {{ overrideCount }} 根骨骼）</small>
        <button type="button" class="dr-btn" data-testid="open-poses" @click="emit('open-poses')">姿势库与关节</button>
        <button
          type="button"
          class="dr-btn"
          :disabled="readonly"
          :title="readonly ? '只读成员不能改姿势' : '选一张人物照片，角色摆出照片里的姿势（照片只在浏览器里识别，不上传）'"
          data-testid="pose-from-image"
          @click="emit('pose-from-image')"
        ><ScanSearch :size="13" />从图片识别</button>
      </div>
    </div>
  </section>
</template>

<style scoped>
.character-pose { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; }
.character-pose small { color: var(--portal-muted); }
.character-pose .dr-btn:first-of-type { margin-left: auto; }
</style>
