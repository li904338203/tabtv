<script setup lang="ts">
/**
 * 位置、旋转、缩放的数值输入（DRT-15，DR-F-30）：失焦或回车时提交一次（一步撤销）。
 * 角色与机位不显示缩放：角色改高度用身高（DR-F-33），机位不能缩放；群众只等比缩放。
 * 范围与场景校验一致：坐标 ±maxCoord 米、角度 ±36000°、缩放 0.01~100。
 */
import { DIRECTOR_LIMITS } from "~/utils/directorCatalog";
import type { DirectorObject, DirectorVec3 } from "~/utils/directorScene";
import { directorSetTransform } from "~/utils/directorScene";
import NumberField from "./NumberField.vue";

const props = defineProps<{ object: DirectorObject; readonly: boolean }>();
const store = useDirectorStore();

const AXES = ["x", "y", "z"] as const;
const showScale = computed(() => props.object.kind === "prop" || props.object.kind === "crowd");
const uniformScale = computed(() => props.object.kind === "crowd");

function commit(part: "position" | "rotation" | "scale", axis: (typeof AXES)[number], value: number) {
  const t = props.object.transform;
  const vec: DirectorVec3 = { ...t[part], [axis]: value };
  const next = { position: { ...t.position }, rotation: { ...t.rotation }, scale: { ...t.scale }, [part]: vec };
  if (part === "scale" && uniformScale.value) next.scale = { x: value, y: value, z: value };
  store.commit(scene => directorSetTransform(scene, props.object.id, next), { touched: [props.object.id] });
}
</script>

<template>
  <section class="dr-section" aria-label="位置与朝向">
    <h3>位置 <span class="dr-unit">米</span></h3>
    <div class="dr-vec">
      <label v-for="axis in AXES" :key="`p-${axis}`">{{ axis.toUpperCase() }}
        <NumberField :model-value="object.transform.position[axis]" :min="-DIRECTOR_LIMITS.maxCoord" :max="DIRECTOR_LIMITS.maxCoord" :disabled="readonly" :label="`位置 ${axis.toUpperCase()}（米）`" @commit="commit('position', axis, $event)" />
      </label>
    </div>
    <h3>旋转 <span class="dr-unit">度</span></h3>
    <div class="dr-vec">
      <label v-for="axis in AXES" :key="`r-${axis}`">{{ axis.toUpperCase() }}
        <NumberField :model-value="object.transform.rotation[axis]" :min="-36000" :max="36000" :digits="1" :disabled="readonly" :label="`旋转 ${axis.toUpperCase()}（度）`" @commit="commit('rotation', axis, $event)" />
      </label>
    </div>
    <template v-if="showScale">
      <h3>缩放 <span class="dr-unit">倍</span></h3>
      <div v-if="uniformScale" class="dr-field">
        <span>等比</span>
        <NumberField :model-value="object.transform.scale.x" :min="0.01" :max="100" :disabled="readonly" label="等比缩放（倍）" @commit="commit('scale', 'x', $event)" />
      </div>
      <div v-else class="dr-vec">
        <label v-for="axis in AXES" :key="`s-${axis}`">{{ axis.toUpperCase() }}
          <NumberField :model-value="object.transform.scale[axis]" :min="0.01" :max="100" :disabled="readonly" :label="`缩放 ${axis.toUpperCase()}（倍）`" @commit="commit('scale', axis, $event)" />
        </label>
      </div>
    </template>
  </section>
</template>
