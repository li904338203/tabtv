<script setup lang="ts">
import { AudioLines, Image as ImageIcon, Video, X } from "@lucide/vue";
import { referenceResource } from "~/utils/canvas";

const props = defineProps<{ value: unknown; order: number }>();
const emit = defineEmits<{ remove: [] }>();
const media = useAuthenticatedMedia();
const resource = computed(() => referenceResource(props.value));
const previewUrl = ref("");

function releasePreview() {
  if (previewUrl.value.startsWith("blob:")) URL.revokeObjectURL(previewUrl.value);
  previewUrl.value = "";
}

async function loadPreview(id?: string) {
  releasePreview();
  const current = resource.value;
  if (!id || !current || current.id !== id || current.type !== "image") return;
  try {
    const response = await media.fetchMedia(current.url);
    if (!response.ok || resource.value?.id !== id) return;
    previewUrl.value = URL.createObjectURL(await response.blob());
  } catch {
    // The type icon remains available when a persisted preview cannot be loaded.
  }
}

watch(() => resource.value?.id, id => void loadPreview(id), { immediate: true });
onBeforeUnmount(releasePreview);

/** 从画布 `@` 引用来的（DRT-20）没有文件大小：说清来自哪个节点，不显示一个编出来的「1 KB」。 */
const tokenTitle = computed(() => {
  const current = resource.value;
  if (!current) return "";
  return current.fromNodeId
    ? `来自画布节点「${current.name}」`
    : `${current.name} · ${Math.max(1, Math.round(current.size / 1024))} KB`;
});

const icon = computed(() => ({
  image: ImageIcon,
  video: Video,
  audio: AudioLines
}[resource.value?.type ?? "image"]));
</script>

<template>
  <div v-if="resource" class="node-reference-token node-upload-reference" :class="{ 'from-node': resource.fromNodeId }" :title="tokenTitle">
    <div class="node-reference-open">
      <span class="node-reference-order">{{ order }}</span>
      <img v-if="resource.type === 'image' && previewUrl" :src="previewUrl" :alt="resource.name">
      <component :is="icon" v-else :size="23" />
    </div>
    <button class="node-reference-remove" type="button" :title="resource.fromNodeId ? '移除引用' : '移除上传参考'" @click="emit('remove')"><X :size="11" /></button>
  </div>
</template>
