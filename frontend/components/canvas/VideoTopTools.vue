<script setup lang="ts">
import type { components } from '~/types/legacy';
import { Camera, MapPin, Sparkles, UserRound } from "@lucide/vue";
import ReferenceUploadButton from "./ReferenceUploadButton.vue";
import type { JsonObject } from "~/types/canvas";
import { asObject } from "~/utils/canvas";
import { canvasCameraMove } from "~/utils/materialPlaza";

type ParamConfig = components["schemas"]["ParamResp"];
const props = defineProps<{
  controls: ParamConfig[];
  params: JsonObject;
  projectId: string;
  nodeId: string;
}>();
const emit = defineEmits<{
  update: [code: string, value: unknown];
  action: [value: string];
}>();
/**
 * 顶部一行：上传 · 角色库 · 运镜 · 特效（「标记」是老系统的，新后端没有这项配置时不出现）。
 * 有哪几项由后端配置决定（node_param），这里只管顺序与写法。
 */
const order = { reference: 1, roleLib: 2, camera: 3, effectLib: 4, mark: 5 };
const icons = { mark: MapPin, effectLib: Sparkles, roleLib: UserRound, camera: Camera };
const controls = computed(() => [...props.controls].sort((left, right) =>
  (order[left.code as keyof typeof order] ?? 99) - (order[right.code as keyof typeof order] ?? 99)
));

function config(control: ParamConfig) {
  return asObject(control.config);
}
function label(control: ParamConfig) {
  if (control.code === "camera") return "运镜";
  if (control.code === "reference") return "上传";
  return control.name ?? control.code ?? "工具";
}
/** 悬停提示取配置（`config.hint`），没配就是按钮上的字。 */
function hint(control: ParamConfig) {
  const value = config(control).hint;
  return typeof value === "string" && value ? value : label(control);
}
function active(code?: string) {
  if (!code) return false;
  // 运镜按「选了一个运镜」算（`params.camera.code`）；此前短暂用过的三组选项对象不算
  if (code === "camera") return Boolean(canvasCameraMove(props.params.camera));
  const value = props.params[code];
  if (Array.isArray(value)) return value.length > 0;
  if (typeof value === "object" && value !== null) return Object.keys(value).length > 0;
  if (value) return true;
  return false;
}
function activate(control: ParamConfig) {
  const code = control.code ?? "";
  if (code === "mark") {
    emit("update", code, !props.params[code]);
    return;
  }
  const action = config(control).action;
  if (typeof action === "string" && action) emit("action", action);
}
</script>

<template>
  <div class="video-top-tools">
    <template v-for="control in controls" :key="control.code">
      <ReferenceUploadButton
        v-if="control.code === 'reference'"
        :project-id="projectId"
        :node-id="nodeId"
        :model-value="params[control.code]"
        :config="config(control)"
        name="参考"
        :label="label(control)"
        variant="video"
        @update:model-value="emit('update', control.code ?? '', $event)"
      />
      <button v-else class="video-top-tool" type="button" :class="{ active: active(control.code) }" :title="hint(control)" @click="activate(control)">
        <component :is="icons[control.code as keyof typeof icons] ?? Camera" :size="14" />
        <span>{{ label(control) }}</span>
      </button>
    </template>
  </div>
</template>
