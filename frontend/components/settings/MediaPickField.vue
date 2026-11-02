<script setup lang="ts">
/**
 * 平台素材选择字段（TT-T-21；照来源系统管理端 `MediaPickField.vue`）：选了文件立刻上传到
 * `POST /admin/media`（平台级素材，workspace_id=0），把返回的媒体 ID 交给表单。
 *
 * ⚠️ 先上传、再保存：保存失败时已上传的文件不会自动清理（来源系统同样如此，平台素材数量很小）。
 *    但同一个 File 重试保存时不再重传 —— 那是表单层的事，这里每次选择就是一次新上传。
 * ⚠️ 只读本机素材：营销 Agent 用的是 adopt 后复制到工作区的副本，这里改图不影响已 adopt 的旧副本。
 */
import { ElButton, ElMessage } from "element-plus";
import { settingsApiError, settingsMediaUrl } from "~/utils/settingsApi";

const props = withDefaults(defineProps<{
  ids: string[];
  max?: number;
  kind?: "image" | "audio" | "video";
  purpose?: "material_cover" | "avatar" | "banner";
  disabled?: boolean;
  hint?: string;
}>(), { max: 1, kind: "image", purpose: "material_cover", disabled: false, hint: "" });
const emit = defineEmits<{ (e: "update:ids", v: string[]): void }>();

const uploading = ref(false);
const input = ref<HTMLInputElement | null>(null);
const accept = computed(() => ({ image: "image/png,image/jpeg,image/webp", audio: "audio/*", video: "video/mp4,video/webm,video/quicktime" })[props.kind]);
const full = computed(() => props.ids.length >= props.max);

async function onPick(event: Event) {
  const files = [...((event.target as HTMLInputElement).files ?? [])];
  (event.target as HTMLInputElement).value = "";
  if (!files.length) return;
  const room = props.max - props.ids.length;
  if (files.length > room) ElMessage.warning(`最多 ${props.max} 个，只上传前 ${room} 个`);
  uploading.value = true;
  const next = [...props.ids];
  try {
    for (const file of files.slice(0, room)) {
      const form = new FormData();
      form.append("file", file);
      form.append("purpose", props.purpose);
      form.append("expectKind", props.kind);
      const out = await useApi()<{ id: string }>("/admin/media", { method: "POST", body: form });
      next.push(out.id);
    }
  } catch (e) {
    ElMessage.error(`上传失败：${settingsApiError(e).message}`);
  } finally {
    uploading.value = false;
    emit("update:ids", next);
  }
}

function remove(i: number) {
  emit("update:ids", props.ids.filter((_, j) => j !== i));
}
function first(i: number) {
  if (i <= 0) return;
  const next = [...props.ids];
  const [it] = next.splice(i, 1);
  emit("update:ids", [it!, ...next]);
}
</script>

<template>
  <div class="media-pick">
    <div class="media-pick-list">
      <figure v-for="(id, i) in ids" :key="id + i" class="media-pick-item">
        <img v-if="kind === 'image'" :src="settingsMediaUrl(id)" alt="">
        <audio v-else-if="kind === 'audio'" :src="settingsMediaUrl(id)" controls />
        <MediaPlayableVideo v-else :src="settingsMediaUrl(id)" muted />
        <figcaption>
          <button v-if="i > 0 && !disabled" type="button" class="portal-link-button" @click="first(i)">设为第一张</button>
          <button v-if="!disabled" type="button" class="portal-link-button danger" @click="remove(i)">移除</button>
        </figcaption>
      </figure>
    </div>
    <input ref="input" type="file" :accept="accept" :multiple="max > 1" hidden @change="onPick">
    <ElButton v-if="!disabled && !full" size="small" :loading="uploading" @click="input?.click()">
      {{ ids.length ? "继续添加" : "选择文件" }}
    </ElButton>
    <small class="media-pick-hint">{{ hint || `最多 ${max} 个` }}（已选 {{ ids.length }}）</small>
  </div>
</template>

<style scoped>
.media-pick { display: flex; flex-direction: column; gap: 8px; width: 100%; }
.media-pick-list { display: flex; flex-wrap: wrap; gap: 8px; }
.media-pick-item { display: flex; flex-direction: column; gap: 4px; margin: 0; }
.media-pick-item img, .media-pick-item video { width: 96px; height: 96px; object-fit: cover; border: 1px solid var(--portal-line); border-radius: 8px; background: var(--portal-surface-2); }
.media-pick-item audio { width: 240px; }
.media-pick-item figcaption { display: flex; gap: 8px; font-size: 12px; }
.media-pick-hint { color: var(--portal-faint); font-size: 12px; }
.danger { color: var(--portal-danger); }
</style>
