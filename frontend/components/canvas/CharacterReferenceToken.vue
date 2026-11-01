<script setup lang="ts">
/**
 * 节点参考条上的一个角色（视频节点「角色库」选的）：主图缩略图 + 角标 + 移除。
 * 角标与连线、上传的参考连续编号，就是提示词里可以写的「图N」。
 *
 * ⚠️ 主图是受保护媒体：fetch → blob → objectURL，卸载时释放（useAuthenticatedMedia）。
 */
import { UserRound, X } from "@lucide/vue";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import type { CanvasRoleCharacter } from "~/utils/canvas";

const props = defineProps<{ character: CanvasRoleCharacter; order: number }>();
const emit = defineEmits<{ remove: [] }>();
const media = useAuthenticatedMedia();
const preview = computed(() => media.urls.value.thumb);

watch(() => props.character.mediaId, (id) => {
  media.release();
  if (id) void media.appendAll([{ key: "thumb", url: mediaThumbUrl(id, 160) }]);
}, { immediate: true });
</script>

<template>
  <div class="node-reference-token node-character-reference" :title="`角色：${character.name}`">
    <div class="node-reference-open">
      <span class="node-reference-order">{{ order }}</span>
      <img v-if="preview" :src="preview" :alt="character.name">
      <UserRound v-else :size="23" />
    </div>
    <span class="node-character-badge" aria-hidden="true"><UserRound :size="9" /></span>
    <button class="node-reference-remove" type="button" title="移除角色" @click="emit('remove')"><X :size="11" /></button>
  </div>
</template>
