<script setup lang="ts">
/**
 * 资产库弹窗（画布左侧「资产」页签的「管理」打开，参考产品同款）：
 * - 生成历史：本项目 / 全部项目，按类型（带数量）筛，按日期分组，可以整天勾选；
 * - 我保存的：= 「保存到我的资产」存下来的（我的资产），按标签筛、搜索、新建文件夹，点文件夹进去；
 * - 我上传的：上传过的参考图片 / 视频 / 音频。
 * 三处都可以多选，底部操作条：已选缩略图、取消、（下载 / 评级 / 保存到资产 / 移动 / 删除，按所在页给）、添加到 ▾ 画布。按需加载。
 */
import { AudioLines, Check, ChevronDown, ChevronLeft, ChevronRight, Clock, Copy, Download, FileText, Folder, FolderInput, FolderOpen, LoaderCircle, Music, Plus, Search, Star, Trash2, Upload, Video, X } from "@lucide/vue";
import { ElMessage } from "element-plus";
import { appConfirmDanger, appPrompt } from "~/utils/appDialog";
import { directorCanEdit } from "~/utils/directorRole";
import { extensionFor, safeFilename, saveBlob } from "~/utils/download";
import { libraryCanvasResource, libraryThumbMediaId, libraryTime, type LibraryFolder, type LibraryItem } from "~/utils/assetLibrary";

const props = defineProps<{ projectId: string }>();
const emit = defineEmits<{ close: []; created: [ids: string[]] }>();
const workspace = useWorkspaceStore();
const assetLibrary = useAssetLibraryStore();
const canEdit = computed(() => directorCanEdit(workspace.current?.role));
const readonlyHint = "需要编辑权限：你在该工作区是只读成员";
const { addResourceNodes } = useCanvasResourceNodes();
const thumbs = useAuthenticatedMedia();
const media = useAuthenticatedMedia();

type Section = "history" | "saved" | "uploads";
const section = ref<Section>("history");

/** 三处统一的「一项」：选中、缩略图、添加到画布、下载都按它来。 */
type Entry = {
  key: string;
  kind: "text" | "image" | "video" | "audio";
  title: string;
  date: string;
  mediaId?: string;
  posterId?: string;
  text?: string;
  generationId?: string;
  model?: string;
  libraryItem?: LibraryItem;
};
const selected = ref<Map<string, Entry>>(new Map());
function toggle(entry: Entry) {
  const next = new Map(selected.value);
  if (next.has(entry.key)) next.delete(entry.key);
  else next.set(entry.key, entry);
  selected.value = next;
}
function clearSelection() {
  selected.value = new Map();
  barMenu.value = null;
}
watch(section, clearSelection);

/** 缩略图：图片取 160 宽缩略图；视频取封面帧（没给就问 /media/{id}/info）；文本、音频用图标。 */
const posterAsked = new Set<string>();
function wantThumbs(list: Entry[]) {
  const direct = list.filter(e => !thumbs.urls.value[e.key] && (e.kind === "image" ? e.mediaId : e.posterId));
  if (direct.length) void thumbs.appendAll(direct.map(e => ({ key: e.key, url: `/api/v1/media/${e.kind === "image" ? e.mediaId : e.posterId}?w=320` })));
  for (const e of list.filter(x => x.kind === "video" && !x.posterId && x.mediaId && !posterAsked.has(x.key))) {
    posterAsked.add(e.key);
    void useApi()<{ posterMediaId?: string }>(`/media/${e.mediaId}/info`).then((info) => {
      if (info.posterMediaId) void thumbs.appendAll([{ key: e.key, url: `/api/v1/media/${info.posterMediaId}?w=320` }]);
    }).catch(() => undefined);
  }
}
onBeforeUnmount(() => {
  thumbs.release();
  media.release();
});

// ---------------------------------------------------------------- 生成历史（/creations）

type Creation = { id: string; projectId: string; projectName: string; resultType: string; modelCode: string; modelName: string; createdAt: string; cover?: { type: string; mediaId: string } | null };
const historyScope = ref<"project" | "all">("project");
const historyType = ref("");
const historyCounts = ref<Record<string, number>>({});
const history = ref<Entry[]>([]);
const historyCursor = ref<string | null>(null);
const historyMore = ref(false);
const historyLoading = ref(false);
let historySeq = 0;
function historyQuery(type: string, limit: number, cursor?: string | null) {
  const q: Record<string, string> = { limit: String(limit) };
  if (historyScope.value === "project" && props.projectId) q.projectId = props.projectId;
  if (type) q.resultType = type;
  if (cursor) q.cursor = cursor;
  return q;
}
function creationEntry(c: Creation): Entry | null {
  if (!c.cover?.mediaId) return null;
  const kind = c.cover.type === "video" || c.cover.type === "audio" ? c.cover.type : "image";
  return { key: `g:${c.id}`, kind, title: c.projectName || c.modelName || "生成结果", date: c.createdAt, mediaId: c.cover.mediaId, generationId: c.id, model: c.modelName || c.modelCode };
}
async function loadHistory(reset = true) {
  const my = ++historySeq;
  historyLoading.value = true;
  try {
    if (reset) {
      const counts = await Promise.all(["", "image", "video", "audio"].map(t =>
        useApi()<{ total: number }>("/creations", { query: historyQuery(t, 1) }).then(r => r.total).catch(() => -1)));
      if (my !== historySeq) return;
      historyCounts.value = { "": counts[0]!, image: counts[1]!, video: counts[2]!, audio: counts[3]! };
    }
    const res = await useApi()<{ items: Creation[]; nextCursor?: string | null; hasMore: boolean }>("/creations", { query: historyQuery(historyType.value, 60, reset ? null : historyCursor.value) });
    if (my !== historySeq) return;
    const entries = res.items.map(creationEntry).filter((e): e is Entry => Boolean(e));
    history.value = reset ? entries : [...history.value, ...entries];
    historyCursor.value = res.nextCursor ?? null;
    historyMore.value = res.hasMore;
    wantThumbs(entries);
  } catch (cause) {
    if (my === historySeq) ElMessage.error(cause instanceof Error ? cause.message : "生成历史取不到");
  } finally {
    if (my === historySeq) historyLoading.value = false;
  }
}
watch([historyScope, historyType], () => {
  clearSelection();
  void loadHistory();
});
/** 按本地日期分组（同一天的放一起，整组可勾选）。 */
const historyGroups = computed(() => {
  const groups: Array<{ day: string; entries: Entry[] }> = [];
  for (const e of history.value) {
    const day = libraryTime(e.date).slice(0, 10);
    const last = groups.at(-1);
    if (last?.day === day) last.entries.push(e);
    else groups.push({ day, entries: [e] });
  }
  return groups;
});
function groupAllSelected(entries: Entry[]) {
  return entries.length > 0 && entries.every(e => selected.value.has(e.key));
}
function toggleGroup(entries: Entry[]) {
  const next = new Map(selected.value);
  const all = groupAllSelected(entries);
  for (const e of entries) {
    if (all) next.delete(e.key);
    else next.set(e.key, e);
  }
  selected.value = next;
}

// ---------------------------------------------------------------- 我保存的（/library/*）

const tagsPreset = ref<string[]>([]);
const tagsCustom = ref<string[]>([]);
const savedTag = ref("");
const savedQuery = ref("");
const savedSearchOpen = ref(false);
const folders = ref<LibraryFolder[]>([]);
const currentFolder = ref<LibraryFolder | null>(null);
const saved = ref<Entry[]>([]);
const savedLoading = ref(false);
let savedSeq = 0;
function libraryEntry(it: LibraryItem): Entry {
  return { key: `l:${it.id}`, kind: it.kind, title: it.title, date: it.savedAt, mediaId: it.mediaId, posterId: libraryThumbMediaId(it) === it.posterMediaId ? it.posterMediaId : undefined, text: it.text, generationId: it.generationId, libraryItem: it };
}
const savedFlat = computed(() => Boolean(savedTag.value || savedQuery.value.trim()));
async function loadSaved() {
  const my = ++savedSeq;
  savedLoading.value = true;
  try {
    const q: Record<string, string> = { limit: "200" };
    if (savedTag.value) q.tags = savedTag.value;
    if (savedQuery.value.trim()) q.q = savedQuery.value.trim();
    // 有筛选时平铺所有文件夹里的；没有时只看当前这一层
    if (!savedFlat.value) q.folderId = currentFolder.value ? currentFolder.value.id : "root";
    const [f, items, tags] = await Promise.all([
      useApi()<{ items: LibraryFolder[] }>("/library/folders"),
      useApi()<{ items: LibraryItem[] }>("/library/items", { query: q }),
      useApi()<{ preset: string[]; custom: string[] }>("/library/tags")
    ]);
    if (my !== savedSeq) return;
    folders.value = f.items;
    saved.value = items.items.map(libraryEntry);
    tagsPreset.value = tags.preset;
    tagsCustom.value = tags.custom;
    wantThumbs(saved.value);
  } catch (cause) {
    if (my === savedSeq) ElMessage.error(cause instanceof Error ? cause.message : "我保存的取不到");
  } finally {
    if (my === savedSeq) savedLoading.value = false;
  }
}
let savedTimer: ReturnType<typeof setTimeout> | undefined;
watch(savedQuery, () => {
  clearTimeout(savedTimer);
  savedTimer = setTimeout(() => void loadSaved(), 300);
});
watch([savedTag, currentFolder], () => {
  clearSelection();
  void loadSaved();
});
async function newTag() {
  if (!canEdit.value) return;
  try {
    const result = await appPrompt("标签名称（最多 20 个字）", "新建标签", { confirmButtonText: "新建", cancelButtonText: "取消", inputPattern: /\S/, inputErrorMessage: "名称不能为空" });
    await useApi()("/library/tags", { method: "POST", body: { name: (result as { value: string }).value } });
    await loadSaved();
  } catch (cause) {
    if (cause !== "cancel" && cause !== "close") ElMessage.error(cause instanceof Error ? cause.message : "新建标签失败");
  }
}
async function newFolder() {
  if (!canEdit.value) return;
  try {
    const result = await appPrompt("文件夹名称", "新建文件夹", { confirmButtonText: "新建", cancelButtonText: "取消", inputPattern: /\S/, inputErrorMessage: "名称不能为空" });
    await useApi()("/library/folders", { method: "POST", body: { name: (result as { value: string }).value } });
    await loadSaved();
    assetLibrary.touch();
  } catch (cause) {
    if (cause !== "cancel" && cause !== "close") ElMessage.error(cause instanceof Error ? cause.message : "新建文件夹失败");
  }
}

// ---------------------------------------------------------------- 我上传的（/media）

type Upload = { id: string; kind: string; mime: string; createdAt?: string };
const uploadKind = ref("");
const uploads = ref<Entry[]>([]);
const uploadCursor = ref<string | null>(null);
const uploadMore = ref(false);
const uploadLoading = ref(false);
let uploadSeq = 0;
async function loadUploads(reset = true) {
  const my = ++uploadSeq;
  uploadLoading.value = true;
  try {
    const q: Record<string, string> = { limit: "60" };
    if (uploadKind.value) q.kind = uploadKind.value;
    if (!reset && uploadCursor.value) q.cursor = uploadCursor.value;
    const res = await useApi()<{ items: Upload[]; nextCursor?: string | null; hasMore: boolean }>("/media", { query: q });
    if (my !== uploadSeq) return;
    const entries: Entry[] = res.items.filter(u => ["image", "video", "audio"].includes(u.kind)).map(u => ({
      key: `u:${u.id}`, kind: u.kind as Entry["kind"], title: `上传的${u.kind === "image" ? "图片" : u.kind === "video" ? "视频" : "音频"}`,
      date: u.createdAt ?? "", mediaId: u.id
    }));
    uploads.value = reset ? entries : [...uploads.value, ...entries];
    uploadCursor.value = res.nextCursor ?? null;
    uploadMore.value = res.hasMore;
    wantThumbs(entries);
  } catch (cause) {
    if (my === uploadSeq) ElMessage.error(cause instanceof Error ? cause.message : "上传记录取不到");
  } finally {
    if (my === uploadSeq) uploadLoading.value = false;
  }
}
watch(uploadKind, () => {
  clearSelection();
  void loadUploads();
});

watch(section, (s) => {
  if (s === "history" && !history.value.length) void loadHistory();
  if (s === "saved") void loadSaved();
  if (s === "uploads" && !uploads.value.length) void loadUploads();
}, { immediate: true });

// ---------------------------------------------------------------- 底部操作条

const barMenu = ref<"add" | "move" | "rate" | null>(null);
const selection = computed(() => [...selected.value.values()]);
async function addToCanvas() {
  barMenu.value = null;
  const resources = selection.value.map((e) => {
    if (e.libraryItem) return libraryCanvasResource(e.libraryItem);
    if (e.kind === "text") return { type: "text" as const, text: e.text ?? "", title: e.title };
    return e.mediaId ? { type: e.kind, mediaId: e.mediaId, title: e.title } : null;
  }).filter((r): r is NonNullable<typeof r> => Boolean(r));
  if (!resources.length) return;
  const ids = await addResourceNodes(resources);
  if (ids.length) {
    emit("created", ids);
    ElMessage.success(`已添加 ${ids.length} 项到画布`);
    clearSelection();
  }
}
async function downloadSelection() {
  for (const e of selection.value) {
    try {
      if (e.kind === "text") {
        saveBlob(new Blob([e.text ?? ""], { type: "text/markdown;charset=utf-8" }), `${safeFilename(e.title, "文本")}.md`);
        continue;
      }
      if (!e.mediaId) continue;
      const blob = await media.blob(`/api/v1/media/${e.mediaId}`);
      saveBlob(blob, `${safeFilename(e.title)}.${extensionFor(blob.type)}`);
    } catch {
      ElMessage.error(`「${e.title}」下载失败`);
    }
  }
}
async function saveSelection() {
  const list = selection.value.filter(e => e.generationId && !e.libraryItem);
  let ok = 0;
  for (const e of list) {
    try {
      await useApi()("/library/items", { method: "POST", body: { generationId: e.generationId, outputIndex: 0, title: e.title } });
      ok += 1;
    } catch {
      // 逐项报：前面存进去的仍然有效
    }
  }
  assetLibrary.touch();
  ElMessage[ok === list.length ? "success" : "warning"](ok === list.length ? `已保存 ${ok} 项到我的资产` : `保存了 ${ok} / ${list.length} 项`);
  clearSelection();
}
async function rateSelection(star: number) {
  barMenu.value = null;
  const ids = [...new Set(selection.value.map(e => e.generationId).filter((id): id is string => Boolean(id)))];
  try {
    await Promise.all(ids.map(id => useApi()(`/library/ratings/${id}`, { method: "PUT", body: { rating: star } })));
    assetLibrary.touch();
    ElMessage.success(`已给 ${ids.length} 项评 ${star} 星`);
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "评级失败");
  }
}
/** 删除上传的素材：逐个删，还被画布 / 时间线引用的服务端会拒绝（说清是被什么用着），其余照删。 */
async function removeUploads() {
  const list = selection.value.filter(e => e.mediaId);
  try {
    await appConfirmDanger(`删除 ${list.length} 份上传的素材？画布上还在用的删不掉。`, "删除", "删除");
  } catch {
    return;
  }
  const failed: string[] = [];
  for (const e of list) {
    try {
      await useApi()(`/media/${e.mediaId}`, { method: "DELETE" });
      uploads.value = uploads.value.filter(x => x.key !== e.key);
    } catch (cause) {
      failed.push(cause instanceof Error ? cause.message : "删除失败");
    }
  }
  clearSelection();
  if (failed.length) ElMessage.warning(`有 ${failed.length} 份没删掉：${failed[0]}`);
  else ElMessage.success(`已删除 ${list.length} 份`);
}
async function moveSelection(folderId: string | null) {
  barMenu.value = null;
  const ids = selection.value.map(e => e.libraryItem?.id).filter((id): id is string => Boolean(id));
  try {
    await useApi()("/library/items/batch", { method: "POST", body: { action: "move", ids, folderId: folderId ?? "" } });
    assetLibrary.touch();
    clearSelection();
    await loadSaved();
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "移动失败");
  }
}
async function removeSelection() {
  if (section.value === "uploads") {
    await removeUploads();
    return;
  }
  const ids = selection.value.map(e => e.libraryItem?.id).filter((id): id is string => Boolean(id));
  try {
    await appConfirmDanger(`把 ${ids.length} 项移出我的资产？画布上的节点和生成结果不受影响。`, "删除", "删除");
    await useApi()("/library/items/batch", { method: "POST", body: { action: "remove", ids } });
    assetLibrary.touch();
    clearSelection();
    await loadSaved();
  } catch (cause) {
    if (cause !== "cancel" && cause !== "close") ElMessage.error(cause instanceof Error ? cause.message : "删除失败");
  }
}

// ---------------------------------------------------------------- 查看（点卡片打开，参考产品同款：左边大图 / 播放，右边信息）

const viewerKey = ref<string | null>(null);
/** 当前这一栏能翻的那一串（文件夹不算）。 */
const viewerList = computed<Entry[]>(() => section.value === "history" ? history.value : section.value === "saved" ? saved.value : uploads.value);
const viewerIndex = computed(() => viewerList.value.findIndex(e => e.key === viewerKey.value));
const viewer = computed(() => (viewerIndex.value >= 0 ? viewerList.value[viewerIndex.value]! : null));
const viewerSrc = ref("");
const viewerLoading = ref(false);
const viewerMedia = useAuthenticatedMedia();
let viewerSeq = 0;
function openViewer(entry: Entry) {
  viewerKey.value = entry.key;
}
function closeViewer() {
  viewerKey.value = null;
}
function stepViewer(delta: number) {
  const next = viewerList.value[viewerIndex.value + delta];
  if (next) viewerKey.value = next.key;
}
watch(viewer, async (e) => {
  const my = ++viewerSeq;
  if (viewerSrc.value) URL.revokeObjectURL(viewerSrc.value);
  viewerSrc.value = "";
  viewerMedia.release();
  if (!e?.mediaId) return;
  viewerLoading.value = true;
  try {
    // 视频取预览版（短边 480），没有时取原片；图片、音频取原件
    let id = e.mediaId;
    if (e.kind === "video") {
      const assets = await useApi()<{ preview?: { mediaId?: string } | null }>(`/media/${e.mediaId}/preview-assets`).catch(() => null);
      id = assets?.preview?.mediaId ?? e.mediaId;
    }
    const blob = await viewerMedia.blob(`/api/v1/media/${id}`);
    if (my !== viewerSeq) return;
    viewerSrc.value = URL.createObjectURL(blob);
  } catch {
    // 取不到：只显示图标
  } finally {
    if (my === viewerSeq) viewerLoading.value = false;
  }
});
onBeforeUnmount(() => {
  if (viewerSrc.value) URL.revokeObjectURL(viewerSrc.value);
  viewerMedia.release();
});
// 悬浮预览：鼠标停在视频卡片上 250ms 后取预览版（短边 480）静音循环播放，移开即停并释放。
// ⚠️ 只留一份：同时只有一张卡片在播，移开就 revoke —— 扫过一屏视频不会在内存里留一串整段视频。
const hoverKey = ref<string | null>(null);
const hoverSrc = ref("");
const hoverMedia = useAuthenticatedMedia();
let hoverTimer: ReturnType<typeof setTimeout> | undefined;
let hoverSeq = 0;
function hoverStop() {
  clearTimeout(hoverTimer);
  hoverSeq++;
  hoverKey.value = null;
  if (hoverSrc.value) URL.revokeObjectURL(hoverSrc.value);
  hoverSrc.value = "";
  hoverMedia.release();
}
function hoverIn(e: Entry) {
  if (e.kind !== "video" || !e.mediaId) return;
  hoverStop();
  const my = hoverSeq;
  hoverKey.value = e.key;
  hoverTimer = setTimeout(async () => {
    try {
      const assets = await useApi()<{ preview?: { mediaId?: string } | null }>(`/media/${e.mediaId}/preview-assets`).catch(() => null);
      const blob = await hoverMedia.blob(`/api/v1/media/${assets?.preview?.mediaId ?? e.mediaId}`);
      if (my !== hoverSeq) return;
      hoverSrc.value = URL.createObjectURL(blob);
    } catch {
      // 取不到就停在封面上
    }
  }, 250);
}
function hoverOut(e: Entry) {
  if (hoverKey.value === e.key) hoverStop();
}
onBeforeUnmount(hoverStop);
const viewerCreator = computed(() => viewer.value?.libraryItem?.creatorName ?? "");
const viewerDate = computed(() => libraryTime(viewer.value?.libraryItem?.generatedAt ?? viewer.value?.date ?? ""));
const viewerPrompt = computed(() => viewer.value?.libraryItem?.prompt ?? "");
const viewerModel = computed(() => viewer.value?.libraryItem?.modelName || viewer.value?.libraryItem?.modelCode || viewer.value?.model || "");
/** 删除：我保存的＝移出资产；我上传的＝删掉这份上传（还被引用时服务端会拒绝并说明）；生成历史不给删。 */
const viewerCanDelete = computed(() => section.value !== "history" && canEdit.value);
async function deleteViewer() {
  const e = viewer.value;
  if (!e) return;
  const isUpload = section.value === "uploads";
  try {
    await appConfirmDanger(isUpload ? "删除这份上传的素材？画布上还在用它时删不掉。" : `把「${e.title}」移出我的资产？画布上的节点和生成结果不受影响。`, "删除", "删除");
    if (isUpload) await useApi()(`/media/${e.mediaId}`, { method: "DELETE" });
    else await useApi()("/library/items/batch", { method: "POST", body: { action: "remove", ids: [e.libraryItem?.id] } });
    const index = viewerIndex.value;
    if (isUpload) uploads.value = uploads.value.filter(x => x.key !== e.key);
    else {
      saved.value = saved.value.filter(x => x.key !== e.key);
      assetLibrary.touch();
    }
    const next = viewerList.value[Math.min(index, viewerList.value.length - 1)];
    viewerKey.value = next?.key ?? null;
  } catch (cause) {
    if (cause !== "cancel" && cause !== "close") ElMessage.error(cause instanceof Error ? cause.message : "删除失败");
  }
}
async function addViewerToCanvas() {
  const e = viewer.value;
  if (!e) return;
  selected.value = new Map([[e.key, e]]);
  await addToCanvas();
}
async function downloadViewer() {
  const e = viewer.value;
  if (!e) return;
  const keep = selected.value;
  selected.value = new Map([[e.key, e]]);
  await downloadSelection();
  selected.value = keep;
}
async function copyPrompt() {
  try {
    await navigator.clipboard.writeText(viewerPrompt.value);
    ElMessage.success("已复制生成信息");
  } catch {
    ElMessage.error("复制失败：浏览器不允许访问剪贴板");
  }
}

function onKey(event: KeyboardEvent) {
  if (viewer.value) {
    if (event.key === "Escape") {
      event.stopPropagation();
      closeViewer();
    } else if (event.key === "ArrowLeft") stepViewer(-1);
    else if (event.key === "ArrowRight") stepViewer(1);
    return;
  }
  if (event.key === "Escape") {
    event.stopPropagation();
    if (barMenu.value) barMenu.value = null;
    else emit("close");
  }
}
function onOutside(event: PointerEvent) {
  if (event.target instanceof Element && !event.target.closest(".ald-bar-anchor")) barMenu.value = null;
}
onMounted(() => {
  window.addEventListener("keydown", onKey, true);
  document.addEventListener("pointerdown", onOutside, true);
});
onBeforeUnmount(() => {
  window.removeEventListener("keydown", onKey, true);
  document.removeEventListener("pointerdown", onOutside, true);
  clearTimeout(savedTimer);
});
const TYPE_TABS = [{ code: "", label: "全部" }, { code: "image", label: "图片" }, { code: "video", label: "视频" }, { code: "audio", label: "音频" }];
</script>

<template>
  <Teleport to="body">
    <div class="ald-overlay" data-testid="asset-library-dialog" @mousedown.self="emit('close')">
      <section class="ald" role="dialog" aria-modal="true" aria-label="资产库">
        <nav class="ald-nav">
          <strong>资产库</strong>
          <button type="button" :class="{ active: section === 'history' }" @click="section = 'history'"><Clock :size="16" />生成历史</button>
          <button type="button" :class="{ active: section === 'saved' }" @click="section = 'saved'"><FolderOpen :size="16" />我保存的</button>
          <button type="button" :class="{ active: section === 'uploads' }" @click="section = 'uploads'"><Upload :size="16" />我上传的</button>
        </nav>
        <div class="ald-main">
          <button type="button" class="ald-close" aria-label="关闭" @click="emit('close')"><X :size="18" /></button>

          <!-- 生成历史 -->
          <template v-if="section === 'history'">
            <header class="ald-head">
              <button type="button" :class="{ active: historyScope === 'project' }" @click="historyScope = 'project'">本项目</button>
              <button type="button" :class="{ active: historyScope === 'all' }" @click="historyScope = 'all'">全部项目</button>
            </header>
            <div class="ald-chips">
              <button v-for="t in TYPE_TABS" :key="t.code" type="button" :class="{ active: historyType === t.code }" @click="historyType = t.code">
                {{ t.label }}<small v-if="(historyCounts[t.code] ?? -1) >= 0">{{ historyCounts[t.code] }}</small>
              </button>
            </div>
            <div class="ald-body">
              <section v-for="group in historyGroups" :key="group.day" class="ald-group">
                <div class="ald-day">
                  <span>{{ group.day }}</span>
                  <button type="button" class="ald-check" :class="{ on: groupAllSelected(group.entries) }" :aria-label="`选择 ${group.day} 的全部`" @click="toggleGroup(group.entries)"><Check v-if="groupAllSelected(group.entries)" :size="12" stroke-width="3" /></button>
                </div>
                <div class="ald-grid">
                  <button v-for="e in group.entries" :key="e.key" type="button" class="ald-card" :class="{ picked: selected.has(e.key) }" data-testid="ald-card" @click="openViewer(e)" @mouseenter="hoverIn(e)" @mouseleave="hoverOut(e)">
                    <span class="ald-thumb">
                      <img v-if="thumbs.urls.value[e.key]" :src="thumbs.urls.value[e.key]" alt="" :draggable="false">
                      <Video v-else-if="e.kind === 'video'" :size="26" /><AudioLines v-else-if="e.kind === 'audio'" :size="26" /><LoaderCircle v-else :size="18" class="spin" />
                      <video v-if="hoverKey === e.key && hoverSrc" class="ald-hover-video" :src="hoverSrc" muted loop autoplay playsinline data-testid="ald-hover-video" />
                      <i class="ald-check corner" :class="{ on: selected.has(e.key) }" role="checkbox" :aria-checked="selected.has(e.key)" aria-label="选择" data-testid="ald-check" @click.stop="toggle(e)"><Check v-if="selected.has(e.key)" :size="12" stroke-width="3" /></i>
                    </span>
                  </button>
                </div>
              </section>
              <p v-if="!historyLoading && !history.length" class="ald-empty">{{ historyScope === 'project' ? '本项目还没有生成过图片、视频或音频' : '还没有生成过图片、视频或音频' }}</p>
              <button v-if="historyMore" type="button" class="ald-more" :disabled="historyLoading" @click="loadHistory(false)">加载更多</button>
              <p v-if="historyLoading && !history.length" class="ald-empty"><LoaderCircle :size="14" class="spin" /> 加载中</p>
            </div>
          </template>

          <!-- 我保存的 -->
          <template v-else-if="section === 'saved'">
            <header class="ald-head">
              <button type="button" class="active">个人资产</button>
              <span v-if="currentFolder" class="ald-crumb"><ChevronRight :size="14" /><button type="button" class="link" @click="currentFolder = null">全部</button><ChevronRight :size="14" />{{ currentFolder.name }}</span>
            </header>
            <div class="ald-chips">
              <button type="button" :class="{ active: !savedTag }" @click="savedTag = ''">全部</button>
              <button v-for="tag in [...tagsPreset, ...tagsCustom]" :key="tag" type="button" :class="{ active: savedTag === tag }" @click="savedTag = savedTag === tag ? '' : tag">{{ tag }}</button>
              <button type="button" class="ghost" :disabled="!canEdit" :title="canEdit ? '' : readonlyHint" @click="newTag"><Plus :size="13" />新建标签</button>
              <span class="ald-spacer" />
              <label v-if="savedSearchOpen" class="ald-search"><Search :size="14" /><input v-model="savedQuery" placeholder="搜索名称、内容或提示词" aria-label="搜索我保存的"></label>
              <button v-else type="button" class="ald-icon" aria-label="搜索" @click="savedSearchOpen = true"><Search :size="16" /></button>
              <button type="button" class="ald-new" :disabled="!canEdit" :title="canEdit ? '新建文件夹' : readonlyHint" @click="newFolder"><Plus :size="15" />新建</button>
            </div>
            <div class="ald-body">
              <div class="ald-grid big">
                <template v-if="!savedFlat && !currentFolder">
                  <button v-for="folder in folders" :key="folder.id" type="button" class="ald-card" data-testid="ald-folder" @click="currentFolder = folder">
                    <span class="ald-thumb folder"><Folder :size="44" stroke-width="1.2" /></span>
                    <span class="ald-name">{{ folder.name }}</span>
                    <span class="ald-date">{{ libraryTime(folder.createdAt).slice(0, 10) }} · {{ folder.itemCount }} 项</span>
                  </button>
                </template>
                <button v-for="e in saved" :key="e.key" type="button" class="ald-card" :class="{ picked: selected.has(e.key) }" data-testid="ald-card" @click="openViewer(e)" @mouseenter="hoverIn(e)" @mouseleave="hoverOut(e)">
                  <span class="ald-thumb">
                    <img v-if="thumbs.urls.value[e.key]" :src="thumbs.urls.value[e.key]" alt="" :draggable="false">
                    <FileText v-else-if="e.kind === 'text'" :size="30" stroke-width="1.4" /><Video v-else-if="e.kind === 'video'" :size="26" /><Music v-else-if="e.kind === 'audio'" :size="26" /><LoaderCircle v-else :size="18" class="spin" />
                    <video v-if="hoverKey === e.key && hoverSrc" class="ald-hover-video" :src="hoverSrc" muted loop autoplay playsinline data-testid="ald-hover-video" />
                    <i class="ald-check corner" :class="{ on: selected.has(e.key) }" role="checkbox" :aria-checked="selected.has(e.key)" aria-label="选择" data-testid="ald-check" @click.stop="toggle(e)"><Check v-if="selected.has(e.key)" :size="12" stroke-width="3" /></i>
                  </span>
                  <span class="ald-name">{{ e.title }}</span>
                  <span class="ald-date">{{ libraryTime(e.date).slice(0, 10) }}</span>
                </button>
              </div>
              <p v-if="!savedLoading && !saved.length && (savedFlat || currentFolder || !folders.length)" class="ald-empty">
                {{ savedFlat ? '没有符合条件的资产' : currentFolder ? '这个文件夹是空的' : '还没有保存的资产。在画布上右键生成结果，选「保存到我的资产」。' }}
              </p>
            </div>
          </template>

          <!-- 我上传的 -->
          <template v-else>
            <header class="ald-head"><button type="button" class="active">我上传的</button></header>
            <div class="ald-chips">
              <button v-for="t in TYPE_TABS" :key="t.code" type="button" :class="{ active: uploadKind === t.code }" @click="uploadKind = t.code">{{ t.label }}</button>
            </div>
            <div class="ald-body">
              <div class="ald-grid big">
                <button v-for="e in uploads" :key="e.key" type="button" class="ald-card" :class="{ picked: selected.has(e.key) }" data-testid="ald-card" @click="openViewer(e)" @mouseenter="hoverIn(e)" @mouseleave="hoverOut(e)">
                  <span class="ald-thumb">
                    <img v-if="thumbs.urls.value[e.key]" :src="thumbs.urls.value[e.key]" alt="" :draggable="false">
                    <Video v-else-if="e.kind === 'video'" :size="26" /><AudioLines v-else-if="e.kind === 'audio'" :size="26" /><LoaderCircle v-else :size="18" class="spin" />
                    <video v-if="hoverKey === e.key && hoverSrc" class="ald-hover-video" :src="hoverSrc" muted loop autoplay playsinline data-testid="ald-hover-video" />
                    <i class="ald-check corner" :class="{ on: selected.has(e.key) }" role="checkbox" :aria-checked="selected.has(e.key)" aria-label="选择" data-testid="ald-check" @click.stop="toggle(e)"><Check v-if="selected.has(e.key)" :size="12" stroke-width="3" /></i>
                  </span>
                </button>
              </div>
              <p v-if="!uploadLoading && !uploads.length" class="ald-empty">还没有上传过素材</p>
              <button v-if="uploadMore" type="button" class="ald-more" :disabled="uploadLoading" @click="loadUploads(false)">加载更多</button>
            </div>
          </template>

          <!-- 底部操作条（选了东西才出现） -->
          <footer v-if="selected.size" class="ald-bar" data-testid="ald-bar">
            <div class="ald-picked">
              <span v-for="e in selection.slice(0, 8)" :key="e.key" class="ald-picked-thumb">
                <img v-if="thumbs.urls.value[e.key]" :src="thumbs.urls.value[e.key]" alt="">
                <FileText v-else-if="e.kind === 'text'" :size="14" /><Video v-else-if="e.kind === 'video'" :size="14" /><Music v-else :size="14" />
              </span>
              <small v-if="selection.length > 8">+{{ selection.length - 8 }}</small>
            </div>
            <span class="ald-spacer" />
            <button type="button" class="ald-btn" @click="clearSelection">取消</button>
            <button v-if="section !== 'history'" type="button" class="ald-btn icon danger" :disabled="!canEdit" :title="canEdit ? (section === 'saved' ? '删除（移出我的资产）' : '删除上传的素材（画布还在用的删不掉）') : readonlyHint" aria-label="删除" data-testid="ald-delete" @click="removeSelection"><Trash2 :size="16" /></button>
            <button type="button" class="ald-btn icon" title="下载" aria-label="下载" @click="downloadSelection"><Download :size="16" /></button>
            <div v-if="section !== 'uploads'" class="ald-bar-anchor">
              <button type="button" class="ald-btn icon" :disabled="!canEdit" :title="canEdit ? '评级' : readonlyHint" aria-label="评级" @click="barMenu = barMenu === 'rate' ? null : 'rate'"><Star :size="16" /></button>
              <div v-if="barMenu === 'rate'" class="ald-pop stars">
                <button v-for="star in 5" :key="star" type="button" :aria-label="`${star} 星`" @click="rateSelection(star)">{{ '★'.repeat(star) }}</button>
              </div>
            </div>
            <div v-if="section === 'saved'" class="ald-bar-anchor">
              <button type="button" class="ald-btn" :disabled="!canEdit" :title="canEdit ? '' : readonlyHint" @click="barMenu = barMenu === 'move' ? null : 'move'"><FolderInput :size="15" />移动到</button>
              <div v-if="barMenu === 'move'" class="ald-pop">
                <button type="button" @click="moveSelection(null)"><Folder :size="14" />个人资产库</button>
                <button v-for="folder in folders" :key="folder.id" type="button" @click="moveSelection(folder.id)"><Folder :size="14" />{{ folder.name }}</button>
              </div>
            </div>
            <button v-if="section === 'history'" type="button" class="ald-btn" :disabled="!canEdit" :title="canEdit ? '' : readonlyHint" data-testid="ald-save" @click="saveSelection"><FolderOpen :size="15" />保存到资产</button>
            <div class="ald-bar-anchor">
              <button type="button" class="ald-btn primary" data-testid="ald-add" @click="barMenu = barMenu === 'add' ? null : 'add'"><Plus :size="15" />添加到<ChevronDown :size="13" /></button>
              <div v-if="barMenu === 'add'" class="ald-pop">
                <button type="button" data-testid="ald-add-canvas" @click="addToCanvas">添加到画布</button>
              </div>
            </div>
          </footer>
        </div>

        <!-- 查看：左边大图 / 播放（背景是它自己的模糊放大），右边信息与操作 -->
        <div v-if="viewer" class="ald-viewer" role="dialog" :aria-label="viewer.title" data-testid="ald-viewer">
          <div class="ald-viewer-stage" :class="{ plain: viewer.kind === 'text' || !viewerSrc }">
            <img v-if="viewer.kind === 'image' && viewerSrc" class="ald-viewer-blur" :src="viewerSrc" alt="" aria-hidden="true">
            <img v-if="viewer.kind === 'image' && viewerSrc" class="ald-viewer-media" :src="viewerSrc" :alt="viewer.title">
            <video v-else-if="viewer.kind === 'video' && viewerSrc" class="ald-viewer-media" :src="viewerSrc" controls autoplay playsinline />
            <div v-else-if="viewer.kind === 'audio' && viewerSrc" class="ald-viewer-audio"><Music :size="48" stroke-width="1.3" /><audio :src="viewerSrc" controls /></div>
            <FileText v-else-if="viewer.kind === 'text'" :size="56" stroke-width="1.3" class="ald-viewer-icon" />
            <LoaderCircle v-else-if="viewerLoading" :size="24" class="spin ald-viewer-icon" />
            <Video v-else-if="viewer.kind === 'video'" :size="48" stroke-width="1.3" class="ald-viewer-icon" />
            <button type="button" class="ald-viewer-nav prev" :disabled="viewerIndex <= 0" aria-label="上一个" @click="stepViewer(-1)"><ChevronLeft :size="20" /></button>
            <button type="button" class="ald-viewer-nav next" :disabled="viewerIndex >= viewerList.length - 1" aria-label="下一个" @click="stepViewer(1)"><ChevronRight :size="20" /></button>
          </div>
          <aside class="ald-viewer-info">
            <header><strong>{{ viewer.title }}</strong><button type="button" class="ald-icon-btn" aria-label="关闭查看" data-testid="ald-viewer-close" @click="closeViewer"><X :size="16" /></button></header>
            <dl>
              <div v-if="viewerCreator"><dt>创作者</dt><dd>{{ viewerCreator }}</dd></div>
              <div><dt>创建于</dt><dd>{{ viewerDate }}</dd></div>
            </dl>
            <div v-if="viewerPrompt || viewerModel" class="ald-viewer-prompt">
              <div v-if="viewerPrompt"><span>生成信息</span><button type="button" class="ald-icon-btn" aria-label="复制生成信息" @click="copyPrompt"><Copy :size="14" /></button></div>
              <p v-if="viewerPrompt">{{ viewerPrompt }}</p>
              <span v-if="viewerModel" class="ald-viewer-model">{{ viewerModel }}</span>
            </div>
            <footer>
              <button type="button" class="ald-viewer-add" data-testid="ald-viewer-add" @click="addViewerToCanvas">添加到画布</button>
              <div>
                <button v-if="section !== 'history'" type="button" class="ald-viewer-del" :disabled="!viewerCanDelete" :title="viewerCanDelete ? '' : readonlyHint" @click="deleteViewer"><Trash2 :size="15" />删除</button>
                <button type="button" class="ald-viewer-dl" @click="downloadViewer"><Download :size="15" />下载</button>
              </div>
            </footer>
          </aside>
        </div>
      </section>
    </div>
  </Teleport>
</template>

<style scoped>
.ald-overlay { position: fixed; inset: 0; z-index: 120; display: grid; place-items: center; padding: 24px; background: rgba(15, 16, 18, .42); }
.ald {
  width: min(1200px, 100%); height: min(780px, calc(100vh - 48px)); display: grid; grid-template-columns: 180px minmax(0, 1fr); grid-template-rows: minmax(0, 1fr); overflow: hidden;
  border: 1px solid var(--canvas-line); border-radius: 18px; color: var(--cv-n90, var(--canvas-text)); background: var(--cv-n15, var(--canvas-panel, #fff));
  box-shadow: 0 28px 70px var(--cv-shade-42, rgba(0, 0, 0, .3));
}
.ald-nav { min-height: 0; display: flex; flex-direction: column; gap: 4px; padding: 18px 12px; border-right: 1px solid var(--canvas-line); }
.ald-nav strong { margin: 4px 8px 12px; color: var(--canvas-muted); font-size: 13px; font-weight: 500; }
.ald-nav button { height: 38px; display: flex; align-items: center; gap: 10px; border: 0; border-radius: 10px; padding: 0 10px; color: var(--cv-n80, var(--canvas-text)); background: transparent; font-size: 14px; cursor: pointer; }
.ald-nav button svg { color: var(--canvas-muted); }
.ald-nav button.active, .ald-nav button:hover { background: var(--cv-n22, var(--canvas-line)); }
.ald-main { position: relative; min-width: 0; min-height: 0; overflow: hidden; display: flex; flex-direction: column; }
.ald-close { position: absolute; top: 14px; right: 14px; width: 32px; height: 32px; display: grid; place-items: center; border: 0; border-radius: 8px; color: var(--canvas-muted); background: transparent; cursor: pointer; }
.ald-close:hover { background: var(--cv-n22, var(--canvas-line)); }
.ald-head { display: flex; align-items: center; gap: 18px; padding: 20px 24px 0; }
.ald-head > button { border: 0; padding: 0; color: var(--canvas-muted); background: transparent; font-size: 15px; cursor: pointer; }
.ald-head > button.active { color: var(--cv-n96, var(--canvas-text)); font-weight: 600; }
.ald-crumb { display: inline-flex; align-items: center; gap: 6px; color: var(--cv-n90); font-size: 14px; }
.ald-crumb .link { border: 0; padding: 0; color: var(--canvas-muted); background: transparent; font-size: 14px; cursor: pointer; }
.ald-chips { display: flex; align-items: center; flex-wrap: wrap; gap: 6px; padding: 14px 24px 6px; }
.ald-chips > button { height: 32px; display: inline-flex; align-items: center; gap: 4px; border: 0; border-radius: 999px; padding: 0 14px; color: var(--cv-n70, var(--canvas-muted)); background: transparent; font-size: 13px; cursor: pointer; }
.ald-chips > button small { color: var(--canvas-muted); font-size: 12px; }
.ald-chips > button.active { color: var(--cv-n96, var(--canvas-text)); background: var(--cv-n22, var(--canvas-line)); }
.ald-chips > button.ghost:disabled { opacity: .45; cursor: not-allowed; }
.ald-spacer { flex: 1; }
.ald-chips > .ald-icon { width: 32px; padding: 0; height: 32px; display: grid; place-items: center; border: 0; border-radius: 8px; color: var(--canvas-muted); background: transparent; cursor: pointer; }
.ald-search { height: 32px; display: flex; align-items: center; gap: 6px; padding: 0 10px; border-radius: 8px; color: var(--canvas-muted); background: var(--cv-n22, var(--canvas-line)); }
.ald-search input { width: 200px; border: 0; outline: 0; color: var(--canvas-text); background: transparent; font-size: 13px; }
.ald-chips > .ald-new { height: 34px; display: inline-flex; align-items: center; gap: 4px; border: 0; border-radius: 10px; padding: 0 14px; color: var(--cv-n08, #fff); background: var(--cv-n94, #262729); font-size: 13px; cursor: pointer; }
.ald-chips > .ald-new:disabled { opacity: .45; cursor: not-allowed; }
.ald-body { min-height: 0; flex: 1; overflow-y: auto; padding: 10px 24px 20px; }
.ald-group + .ald-group { margin-top: 18px; }
.ald-day { display: flex; align-items: center; gap: 8px; margin-bottom: 10px; color: var(--canvas-muted); font-size: 13px; }
.ald-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(140px, 1fr)); gap: 12px; }
.ald-grid.big { grid-template-columns: repeat(auto-fill, minmax(150px, 1fr)); }
.ald-card { display: flex; flex-direction: column; gap: 6px; border: 0; padding: 0; color: inherit; background: transparent; text-align: left; cursor: pointer; }
.ald-thumb {
  position: relative; width: 100%; aspect-ratio: 1; display: grid; place-items: center; overflow: hidden; border: 2px solid transparent; border-radius: 14px;
  color: var(--cv-n57, #9aa0a6); background: var(--cv-n22, #f2f3f5);
}
.ald-thumb.folder { color: #9a9ca1; }
.ald-card.picked .ald-thumb { border-color: var(--cv-n94, #111); }
.ald-thumb img { width: 100%; height: 100%; object-fit: cover; }
.ald-hover-video { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; background: #000; }
.ald-name { overflow: hidden; font-size: 13px; text-overflow: ellipsis; white-space: nowrap; }
.ald-date { color: var(--canvas-muted); font-size: 12px; }
.ald-check {
  width: 18px; height: 18px; display: grid; place-items: center; padding: 0; border: 1.5px solid var(--cv-n57, #a0a3a8); border-radius: 5px;
  color: var(--cv-n08, #fff); background: rgba(255, 255, 255, .7); cursor: pointer;
}
.ald-check.on { border-color: var(--cv-n94, #111); background: var(--cv-n94, #111); }
.ald-check.corner { position: absolute; top: 8px; left: 8px; opacity: 0; transition: opacity 120ms ease; }
.ald-card:hover .ald-check.corner, .ald-check.corner.on { opacity: 1; }
.ald-empty { margin: 40px 0; color: var(--canvas-muted); font-size: 13px; text-align: center; }
.ald-more { display: block; margin: 18px auto 0; border: 1px solid var(--canvas-line); border-radius: 8px; padding: 6px 16px; color: var(--cv-n80); background: transparent; font-size: 12px; cursor: pointer; }
.ald-bar { display: flex; align-items: center; gap: 8px; padding: 12px 18px; border-top: 1px solid var(--canvas-line); }
.ald-picked { display: flex; align-items: center; gap: 6px; min-width: 0; overflow: hidden; }
.ald-picked-thumb { width: 36px; height: 36px; flex: 0 0 auto; display: grid; place-items: center; overflow: hidden; border: 1px solid var(--canvas-line); border-radius: 8px; color: var(--canvas-muted); background: var(--cv-n22, #f2f3f5); }
.ald-picked-thumb img { width: 100%; height: 100%; object-fit: cover; }
.ald-picked small { color: var(--canvas-muted); font-size: 12px; }
.ald-btn {
  height: 36px; display: inline-flex; align-items: center; gap: 6px; border: 0; border-radius: 10px; padding: 0 14px;
  color: var(--cv-n90, var(--canvas-text)); background: var(--cv-n22, #f2f3f5); font-size: 13px; white-space: nowrap; cursor: pointer;
}
.ald-btn.icon { width: 36px; padding: 0; justify-content: center; }
.ald-btn.danger { color: var(--portal-danger, #e5484d); background: color-mix(in srgb, var(--portal-danger, #e5484d) 12%, transparent); }
.ald-btn.primary { color: var(--cv-n08, #fff); background: var(--cv-n94, #262729); }
.ald-btn:disabled { opacity: .45; cursor: not-allowed; }
.ald-bar-anchor { position: relative; }
.ald-pop {
  position: absolute; right: 0; bottom: calc(100% + 8px); z-index: 5; min-width: 160px; padding: 6px; border: 1px solid var(--canvas-line); border-radius: 12px;
  background: var(--cv-n15, #fff); box-shadow: 0 14px 36px var(--cv-shade-30, rgba(0, 0, 0, .18));
}
.ald-pop button { width: 100%; height: 34px; display: flex; align-items: center; gap: 8px; border: 0; border-radius: 8px; padding: 0 12px; color: var(--cv-n90, var(--canvas-text)); background: transparent; font-size: 13px; text-align: left; white-space: nowrap; cursor: pointer; }
.ald-pop button:hover { background: var(--cv-n22, var(--canvas-line)); }
.ald-pop.stars button { color: #f5b301; letter-spacing: 1px; }
/* 查看器（盖在整个弹窗上） */
.ald { position: relative; }
.ald-viewer { position: absolute; inset: 0; z-index: 20; display: grid; grid-template-columns: minmax(0, 1fr) 320px; background: var(--cv-n15, #fff); }
.ald-viewer-stage { position: relative; min-width: 0; display: grid; place-items: center; overflow: hidden; background: #1c1d1f; }
.ald-viewer-stage.plain { background: #a9aaac; }
.ald-viewer-blur { position: absolute; inset: -40px; width: calc(100% + 80px); height: calc(100% + 80px); object-fit: cover; filter: blur(40px) brightness(.7); }
.ald-viewer-media { position: relative; max-width: 100%; max-height: 100%; object-fit: contain; }
.ald-viewer-audio { position: relative; display: grid; justify-items: center; gap: 18px; color: #e9eaec; }
.ald-viewer-audio audio { width: min(420px, 70vw); }
.ald-viewer-icon { position: relative; color: #6b6d71; }
.ald-viewer-nav {
  position: absolute; top: 50%; z-index: 2; width: 40px; height: 40px; display: grid; place-items: center; transform: translateY(-50%);
  border: 0; border-radius: 50%; color: #222; background: rgba(255, 255, 255, .78); cursor: pointer;
}
.ald-viewer-nav:disabled { opacity: .4; cursor: default; }
.ald-viewer-nav.prev { left: 20px; }
.ald-viewer-nav.next { right: 20px; }
.ald-viewer-info { min-height: 0; display: flex; flex-direction: column; padding: 20px 18px 16px; border-left: 1px solid var(--canvas-line); }
.ald-viewer-info header { display: flex; align-items: center; justify-content: space-between; gap: 8px; margin-bottom: 18px; }
.ald-viewer-info header strong { min-width: 0; overflow: hidden; font-size: 16px; text-overflow: ellipsis; white-space: nowrap; }
.ald-icon-btn { width: 28px; height: 28px; flex: 0 0 auto; display: grid; place-items: center; border: 0; border-radius: 6px; color: var(--canvas-muted); background: transparent; cursor: pointer; }
.ald-icon-btn:hover { background: var(--cv-n22, var(--canvas-line)); }
.ald-viewer-info dl { display: grid; gap: 14px; margin: 0; }
.ald-viewer-info dl > div { display: flex; justify-content: space-between; gap: 12px; font-size: 13px; }
.ald-viewer-info dt { color: var(--canvas-muted); }
.ald-viewer-info dd { margin: 0; text-align: right; }
.ald-viewer-prompt { margin-top: 14px; font-size: 13px; }
.ald-viewer-prompt > div { display: flex; align-items: center; justify-content: space-between; color: var(--canvas-muted); }
.ald-viewer-prompt p { max-height: 240px; margin: 8px 0; overflow-y: auto; line-height: 1.65; white-space: pre-wrap; }
.ald-viewer-model { display: inline-block; padding: 2px 8px; border-radius: 6px; color: var(--canvas-muted); background: var(--cv-n22, #f2f3f5); font-size: 12px; }
.ald-viewer-info footer { display: grid; gap: 8px; margin-top: auto; }
.ald-viewer-info footer > div { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; }
.ald-viewer-add { height: 38px; border: 0; border-radius: 10px; color: var(--cv-n08, #fff); background: var(--cv-n94, #151617); font-size: 14px; cursor: pointer; }
.ald-viewer-del, .ald-viewer-dl { height: 38px; display: inline-flex; align-items: center; justify-content: center; gap: 6px; border: 0; border-radius: 10px; font-size: 13px; cursor: pointer; }
.ald-viewer-del { color: var(--portal-danger, #e5484d); background: color-mix(in srgb, var(--portal-danger, #e5484d) 12%, transparent); }
.ald-viewer-del:disabled { opacity: .45; cursor: not-allowed; }
.ald-viewer-dl { color: var(--cv-n90, var(--canvas-text)); background: var(--cv-n22, #f2f3f5); }
.ald-viewer-info footer > div:has(> :only-child) { grid-template-columns: 1fr; }
</style>
