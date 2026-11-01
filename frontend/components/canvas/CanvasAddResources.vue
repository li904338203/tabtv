<script setup lang="ts">
/**
 * 加节点菜单下半截的「添加资源」：上传图片 / 视频 / 音频、从生成历史选择。按需加载（菜单打开时才取这段代码），
 * 画布页首屏包体积有上限（scripts/director-bundle-check.mjs）。
 */
import { History } from "@lucide/vue";
import ReferenceUploadButton from "./ReferenceUploadButton.vue";
import type { UploadedReference } from "~/utils/canvas";
import type { ResourceMediaType } from "~/utils/canvasResources";

defineProps<{ projectId: string }>();
const emit = defineEmits<{ history: []; created: [ids: string[]] }>();
const { addResourceNodes } = useCanvasResourceNodes();

async function onUploaded(list: UploadedReference[]) {
  const ids = await addResourceNodes(list
    .filter(ref => ref.type === "image" || ref.type === "video" || ref.type === "audio")
    .map(ref => ({ type: ref.type as ResourceMediaType, mediaId: ref.id, title: ref.name || "上传素材" })));
  if (ids.length) emit("created", ids);
}
</script>

<template>
  <ReferenceUploadButton
    :project-id="projectId"
    node-id=""
    :model-value="[]"
    :config="{ accept: 'image/*,video/*,audio/*', maxCount: 9, maxSizeByType: { image: 20971520, video: 524288000, audio: 104857600 } }"
    name="素材"
    label="上传"
    icon="upload"
    class="node-add-resource"
    data-testid="add-resource-upload"
    @update:model-value="onUploaded"
  />
  <button type="button" class="node-add-resource" data-testid="add-resource-history" @click="emit('history')">
    <History :size="20" /><span>从生成历史选择</span>
  </button>
</template>
