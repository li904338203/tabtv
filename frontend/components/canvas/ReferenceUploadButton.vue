<script setup lang="ts">
import { LoaderCircle, Plus, Upload } from "@lucide/vue";
import { ElMessage } from "element-plus";
import { referenceResources, uploadedReferenceOf, type UploadedReference } from "~/utils/canvas";
import { referenceMediaType, referenceUploadForm } from "~/utils/referenceUpload";

const props = withDefaults(defineProps<{
  projectId: string;
  nodeId: string;
  modelValue?: unknown;
  config: Record<string, unknown>;
  name?: string;
  /** 按钮上的字（视频节点写「上传」）；提示语里仍用 name（「最多 9 份参考」）。 */
  label?: string;
  variant?: "image" | "video";
  /** 图标：缺省「+」；画布加节点菜单的「上传」用上传箭头。 */
  icon?: "plus" | "upload";
}>(), {
  icon: "plus",
  name: "参考",
  label: undefined,
  variant: "image",
  modelValue: undefined
});
const emit = defineEmits<{ "update:modelValue": [value: UploadedReference[]] }>();
const uploading = ref(false);
const current = computed(() => referenceResources(props.modelValue));
/** 缺省 1：老配置里没有 maxCount，此时必须维持单张语义，不能悄悄放开。 */
const maxCount = computed(() => {
  const value = Number(props.config.maxCount ?? 1);
  return Number.isFinite(value) && value >= 1 ? Math.floor(value) : 1;
});
const remaining = computed(() => Math.max(0, maxCount.value - current.value.length));

function accepts(file: File) {
  const accept = String(props.config.accept ?? "*/*");
  return accept.split(",").map(item => item.trim()).some((rule) => {
    if (!rule || rule === "*/*") return true;
    if (rule.endsWith("/*")) return file.type.startsWith(rule.slice(0, -1));
    if (rule.startsWith(".")) return file.name.toLowerCase().endsWith(rule.toLowerCase());
    return file.type === rule;
  });
}

/**
 * ⚠️ 带上 `mediaType`（服务端再按文件头核对一遍）；响应是新后端的 `{id, kind, mime, sizeBytes}`，
 *    按老形状（`type` / `contentType`）读的话**每次上传都成功、却被判成响应无效**，节点上什么都没有。
 */
async function upload(file: File) {
  const body = referenceUploadForm(file, { mediaType: referenceMediaType(file), projectId: props.projectId });
  const response = await useApi()<unknown>("/media/references", { method: "POST", body });
  const resource = uploadedReferenceOf(response, file);
  if (!resource) throw new Error("参考资源接口响应无效");
  return resource;
}

/** 单份大小上限：按类型给（图 / 视频 / 音频差一个数量级，与服务端上传校验一致），没配时用总上限。 */
function maxSizeOf(file: File): number {
  const type = referenceMediaType(file);
  const byType = props.config.maxSizeByType;
  const value = type && byType && typeof byType === "object" ? (byType as Record<string, unknown>)[type] : undefined;
  return Number(value ?? props.config.maxSize ?? 50 * 1024 * 1024);
}

async function pick(event: Event) {
  const input = event.target as HTMLInputElement;
  const picked = Array.from(input.files ?? []);
  input.value = "";
  if (!picked.length || uploading.value) return;
  if (!remaining.value) {
    ElMessage.warning(`最多只能添加 ${maxCount.value} 份${props.name}`);
    return;
  }

  // 超出名额的直接截断并告知，不要静默丢弃 —— 用户选了 8 张只上传了 6 张却毫无提示最容易踩坑
  const overflow = Math.max(0, picked.length - remaining.value);
  const files = picked.slice(0, remaining.value);
  if (overflow) ElMessage.warning(`最多 ${maxCount.value} 份${props.name}，超出的 ${overflow} 个已忽略`);

  uploading.value = true;
  const uploaded: UploadedReference[] = [];
  try {
    for (const file of files) {
      if (!accepts(file)) {
        ElMessage.error(`${file.name}：不支持该参考资源格式`);
        continue;
      }
      const maxSize = maxSizeOf(file);
      if (!Number.isFinite(maxSize) || file.size > maxSize) {
        ElMessage.error(`${file.name}：不能超过 ${Math.max(1, Math.round(maxSize / 1024 / 1024))} MB`);
        continue;
      }
      try {
        uploaded.push(await upload(file));
      } catch (cause) {
        // 逐个上报而不是整批中断：前面已经传上去的仍然有效，全丢掉等于白传
        ElMessage.error(`${file.name}：${cause instanceof Error ? cause.message : "上传失败"}`);
      }
    }
    if (uploaded.length) emit("update:modelValue", [...current.value, ...uploaded]);
  } finally {
    uploading.value = false;
  }
}
</script>

<template>
  <label
    class="reference-upload-button"
    :class="[
      variant === 'video' ? 'video-top-tool' : 'canvas-control control-button',
      { active: current.length > 0, loading: uploading, disabled: !remaining }
    ]"
    :title="remaining ? `上传${name}（还可添加 ${remaining} 份）` : `${name}已达上限 ${maxCount} 份`"
  >
    <LoaderCircle v-if="uploading" :size="14" class="spin" />
    <Upload v-else-if="icon === 'upload'" :size="15" />
    <Plus v-else :size="15" />
    <span>{{ uploading ? "上传中" : (label ?? name) }}<template v-if="maxCount > 1 && current.length"> {{ current.length }}/{{ maxCount }}</template></span>
    <input
      type="file"
      :accept="String(config.accept ?? '*/*')"
      :multiple="maxCount > 1"
      :disabled="uploading || !remaining"
      @change="pick"
    >
  </label>
</template>
