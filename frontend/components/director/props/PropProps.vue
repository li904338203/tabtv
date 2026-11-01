<script setup lang="ts">
/**
 * 道具属性（DRT-15，DR-F-51）：形状、三轴尺寸、灰阶色板（DRT-22，DR-F-52）。
 * 尺寸范围与场景校验一致（0.01~100 米）；平面平铺在地面上，尺寸取 x 与 z（dev/03 R-5 #10）。
 * 色板只有 8 档灰阶（`DIRECTOR_PROP_COLORS`，与 catalog 同一张表）：白模构图要的是体块与明暗，彩色道具会被下游当成画面内容。
 */
import { DIRECTOR_OBJECT_DEFAULTS, DIRECTOR_PROP_COLORS, DIRECTOR_PROP_SHAPES } from "~/utils/directorCatalog";
import { directorPatchObject, type DirectorObject, type DirectorVec3 } from "~/utils/directorScene";
import NumberField from "./NumberField.vue";

const props = defineProps<{ object: DirectorObject; readonly: boolean }>();
const store = useDirectorStore();

const SHAPE_NAMES: Record<string, string> = { box: "方块", cylinder: "圆柱", sphere: "球", plane: "平面" };
const AXES = ["x", "y", "z"] as const;
type PropSub = { shape?: string; size?: DirectorVec3; color?: string };
const prop = computed(() => ({ ...DIRECTOR_OBJECT_DEFAULTS.prop, ...(props.object.prop ?? {}) }) as Required<PropSub>);

function patch(sub: Record<string, unknown>) {
  store.commit(scene => directorPatchObject(scene, props.object.id, "prop", sub), { touched: [props.object.id] });
}

function setSize(axis: (typeof AXES)[number], value: number) {
  patch({ size: { ...prop.value.size, [axis]: value } });
}
</script>

<template>
  <section class="dr-section" aria-label="道具">
    <h3>道具</h3>
    <div class="dr-field">
      <span>形状</span>
      <div class="dr-seg" role="radiogroup" aria-label="形状">
        <button
          v-for="shape in DIRECTOR_PROP_SHAPES"
          :key="shape"
          type="button"
          role="radio"
          :aria-checked="shape === prop.shape"
          :class="{ active: shape === prop.shape }"
          :disabled="readonly"
          @click="patch({ shape })"
        >{{ SHAPE_NAMES[shape] ?? shape }}</button>
      </div>
    </div>
    <h3>尺寸 <span class="dr-unit">米</span></h3>
    <div class="dr-vec">
      <label v-for="axis in AXES" :key="axis">{{ axis.toUpperCase() }}
        <NumberField
          :model-value="prop.size[axis]"
          :min="0.01"
          :max="100"
          :disabled="readonly || (prop.shape === 'plane' && axis === 'y')"
          :label="`尺寸 ${axis.toUpperCase()}（米）`"
          @commit="setSize(axis, $event)"
        />
      </label>
    </div>
    <p v-if="prop.shape === 'plane'" class="dr-hint">平面平铺在地面上，尺寸取 X 与 Z；要立起来当墙用，旋转它即可。</p>
    <div class="dr-field">
      <span>颜色</span>
      <div class="prop-swatches" role="radiogroup" aria-label="颜色">
        <button
          v-for="color in DIRECTOR_PROP_COLORS"
          :key="color"
          type="button"
          role="radio"
          :aria-checked="color.toLowerCase() === prop.color.toLowerCase()"
          :class="{ active: color.toLowerCase() === prop.color.toLowerCase() }"
          :style="{ background: color }"
          :disabled="readonly"
          :title="readonly ? '你在该工作区是只读' : `灰阶 ${color}`"
          :aria-label="`灰阶 ${color}`"
          :data-color="color"
          @click="patch({ color })"
        />
      </div>
    </div>
  </section>
</template>

<style scoped>
.prop-swatches { display: flex; flex-wrap: wrap; gap: 6px; }
.prop-swatches button { width: 22px; height: 22px; padding: 0; border: 1px solid var(--portal-line-strong); border-radius: 5px; cursor: pointer; }
.prop-swatches button.active { outline: 2px solid var(--portal-cyan-line); outline-offset: 1px; }
.prop-swatches button:disabled { opacity: 0.45; cursor: not-allowed; }
</style>
