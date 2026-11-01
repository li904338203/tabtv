<script setup lang="ts">
/**
 * 截图条（DRT-17，DR-F-75）：本场景的截图按时间倒序；缩略图取 320 档（受保护媒体经 useAuthenticatedMedia 取成 blob，
 * DRD-40）；点开看大图；多选后「发布到画布」；可以隐藏 / 取消隐藏（文件不删），有「显示已隐藏」开关；
 * 每条标出是否过期（记录的指纹 ≠ 场景当前的指纹，前端不算指纹，DRD-13）。
 * viewer 看得到截图条，不能隐藏（DR-F-76）；发布到画布是在画布上建节点，同样要编辑权限。
 */
import { ElMessage } from "element-plus";
import { ChevronDown, ChevronUp, Eye, EyeOff, Maximize2, Upload } from "@lucide/vue";
import type { components } from "@tabtv/api-client";
import PreviewDialog from "~/components/media/PreviewDialog.vue";

type MediaRecord = components["schemas"]["DirectorMediaRecord"];

const props = defineProps<{
  records: MediaRecord[];
  renderFingerprint: string;
  readonly: boolean;
  readonlyText: string;
  /** 截控制图为什么点不了（只读、提案预览中、没有机位……）；能点时为空串（DRT-27）。 */
  controlBlocked: string;
  busy: boolean;
}>();
const emit = defineEmits<{ publish: [records: MediaRecord[]]; control: [kind: "depth" | "pose"] }>();
const store = useDirectorStore();

const KIND_NAMES: Record<string, string> = { capture: "截图", depth: "深度图", pose: "骨架图", preview: "预演", splat: "泼溅" };
const open = ref(true);
const showHidden = ref(false);
const picked = ref<Set<string>>(new Set());
const previewing = ref<MediaRecord | null>(null);
const thumbs = useAuthenticatedMedia();
const full = useAuthenticatedMedia();

/** 截图条只列图片类（截图 / 深度图 / 骨架图）；预演与泼溅各有自己的入口。 */
const listed = computed(() => props.records.filter(record => ["capture", "depth", "pose"].includes(record.kind) && (showHidden.value || !record.hidden)));
const pickedRecords = computed(() => listed.value.filter(record => picked.value.has(record.id)));
const stale = (record: MediaRecord) => Boolean(record.renderFingerprint && props.renderFingerprint && record.renderFingerprint !== props.renderFingerprint);

watch(listed, (items) => {
  void thumbs.appendAll(items.filter(item => !thumbs.urls.value[item.id]).map(item => ({ key: item.id, url: `/api/v1/media/${item.mediaId}?w=320` })));
  picked.value = new Set([...picked.value].filter(id => items.some(item => item.id === id)));
}, { immediate: true });

function toggle(id: string) {
  const next = new Set(picked.value);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  picked.value = next;
}

async function setHidden(record: MediaRecord, hidden: boolean) {
  try {
    await store.setRecordHidden(record.id, hidden);
  } catch (cause) {
    ElMessage.error(cause instanceof Error && cause.message ? cause.message : "操作失败");
  }
}

function publish() {
  if (props.readonly || !pickedRecords.value.length) return;
  emit("publish", pickedRecords.value);
  picked.value = new Set();
}

async function openPreview(record: MediaRecord) {
  previewing.value = record;
  full.release();
  await full.appendAll([{ key: "full", url: `/api/v1/media/${record.mediaId}` }]);
}

function closePreview() {
  previewing.value = null;
  full.release();
}
</script>

<template>
  <section class="capture-strip" :class="{ collapsed: !open }" aria-label="截图条">
    <header>
      <button type="button" class="dr-btn" :aria-expanded="open" :title="open ? '收起截图条' : '展开截图条'" @click="open = !open">
        <ChevronDown v-if="open" :size="13" /><ChevronUp v-else :size="13" />截图 {{ listed.length }}
      </button>
      <label v-if="open" class="dr-check"><input v-model="showHidden" type="checkbox">显示已隐藏</label>
      <!-- 控制图（DRT-27）：作用于当前机位，与截图同尺寸；目前接入的模型只把它们当普通参考图 -->
      <button
        type="button"
        class="dr-btn"
        :disabled="Boolean(controlBlocked) || busy"
        :title="controlBlocked || '截当前机位的深度图：近白远黑，近远写进记录与文件名'"
        aria-label="截深度图"
        data-testid="capture-depth"
        @click="emit('control', 'depth')"
      >深度图</button>
      <button
        type="button"
        class="dr-btn"
        :disabled="Boolean(controlBlocked) || busy"
        :title="controlBlocked || '截当前机位的骨架图：OpenPose 18 点，被挡住的关节照常画，群众不出骨架'"
        aria-label="截骨架图"
        data-testid="capture-pose"
        @click="emit('control', 'pose')"
      >骨架图</button>
      <button
        v-if="open"
        type="button"
        class="dr-btn primary"
        :disabled="readonly || !pickedRecords.length"
        :title="readonly ? readonlyText : pickedRecords.length ? `把选中的 ${pickedRecords.length} 张发布为画布上的图片节点` : '先勾选要发布的截图'"
        @click="publish"
      ><Upload :size="13" />发布到画布{{ pickedRecords.length ? `（${pickedRecords.length}）` : "" }}</button>
    </header>
    <ul v-if="open" class="capture-list">
      <li v-if="!listed.length" class="dr-empty">还没有截图：顶栏「截当前机位」「截全部机位」</li>
      <li v-for="record in listed" :key="record.id" :class="{ picked: picked.has(record.id), hidden: record.hidden }" :data-record-id="record.id">
        <label class="capture-thumb" :title="`${record.cameraName ?? '机位'} · ${KIND_NAMES[record.kind] ?? record.kind}`">
          <input type="checkbox" :checked="picked.has(record.id)" :aria-label="`选择 ${record.cameraName ?? '机位'} 的${KIND_NAMES[record.kind] ?? '截图'}`" @change="toggle(record.id)">
          <img v-if="thumbs.urls.value[record.id]" :src="thumbs.urls.value[record.id]" :alt="record.cameraName ?? '截图'">
          <span v-if="stale(record)" class="capture-stale" title="截图之后场景有改动">已过期</span>
          <span v-if="record.hidden" class="capture-hidden">已隐藏</span>
        </label>
        <div class="capture-meta">
          <span>{{ record.cameraName ?? "机位" }} · {{ KIND_NAMES[record.kind] ?? record.kind }}</span>
          <span class="capture-actions">
            <button type="button" title="看大图" aria-label="看大图" @click="openPreview(record)"><Maximize2 :size="12" /></button>
            <button
              type="button"
              :disabled="readonly"
              :title="readonly ? readonlyText : record.hidden ? '取消隐藏' : '从截图条隐藏（文件不删）'"
              :aria-label="record.hidden ? '取消隐藏' : '隐藏'"
              @click="setHidden(record, !record.hidden)"
            ><Eye v-if="record.hidden" :size="12" /><EyeOff v-else :size="12" /></button>
          </span>
        </div>
      </li>
    </ul>
    <Teleport to="body">
      <PreviewDialog
        v-if="previewing"
        type="image"
        :src="full.urls.value.full"
        :title="`${previewing.cameraName ?? '机位'} · ${KIND_NAMES[previewing.kind] ?? previewing.kind}`"
        :subtitle="previewing.width && previewing.height ? `${previewing.width} × ${previewing.height}` : undefined"
        :loading="!full.urls.value.full"
        @close="closePreview"
      />
    </Teleport>
  </section>
</template>

<style scoped>
.capture-strip {
  position: absolute;
  left: 12px;
  right: 12px;
  bottom: 12px;
  z-index: 7;
  display: grid;
  gap: 6px;
  max-height: 168px;
  padding: 6px 8px;
  border: 1px solid var(--portal-line);
  border-radius: 8px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 6px 18px var(--portal-shadow);
  font-size: 12px;
}
.capture-strip.collapsed { left: auto; right: 12px; }
.capture-strip header { display: flex; align-items: center; gap: 10px; }
.capture-strip header .dr-btn.primary { margin-left: auto; }
.capture-list { display: flex; gap: 8px; margin: 0; padding: 0 0 2px; overflow-x: auto; list-style: none; }
.capture-list > li { flex: 0 0 auto; display: grid; gap: 3px; width: 148px; }
.capture-list > li.dr-empty { width: auto; padding: 12px 4px; }
.capture-thumb { position: relative; display: block; height: 84px; overflow: hidden; border: 1px solid var(--portal-line); border-radius: 5px; background: var(--portal-media-letterbox); cursor: pointer; }
.capture-list li.picked .capture-thumb { border-color: var(--portal-cyan); box-shadow: 0 0 0 1px var(--portal-cyan); }
.capture-list li.hidden .capture-thumb img { opacity: 0.45; }
.capture-thumb img { width: 100%; height: 100%; object-fit: contain; }
.capture-thumb input { position: absolute; left: 5px; top: 5px; z-index: 1; accent-color: var(--portal-cyan); }
.capture-stale,
.capture-hidden { position: absolute; right: 4px; top: 4px; padding: 0 5px; border-radius: 999px; color: var(--portal-on-media); background: var(--portal-on-media-bg); font-size: 10px; }
.capture-hidden { top: auto; bottom: 4px; }
.capture-meta { display: flex; align-items: center; gap: 4px; color: var(--portal-text-soft); font-size: 11px; }
.capture-meta > span:first-child { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.capture-actions { display: flex; gap: 2px; }
.capture-actions button { display: grid; place-items: center; width: 20px; height: 20px; padding: 0; border: 0; border-radius: 4px; color: var(--portal-text-soft); background: transparent; cursor: pointer; }
.capture-actions button:hover:not(:disabled) { color: var(--portal-text); background: var(--portal-hover); }
.capture-actions button:disabled { opacity: 0.4; cursor: not-allowed; }
</style>
