<script setup lang="ts">
/**
 * 画布左侧「资产」页签 = 我的资产（迁移 00074）：只有保存过的生成产物（画布右键「保存到我的资产」）。按需加载。
 *
 * - 顶部：搜索、列表 / 网格、筛选（按标签，交集；可「新建标签」）、新建文件夹、管理（多选后批量移动 / 移出）；
 * - 列表：顶层资产在前、文件夹（单层，可展开）在后；每项「···」：重命名、移到文件夹、编辑标签、移出资产；
 * - 点一项：详情卡片（预览、创作者、创建于、资产来源、生成信息可复制、标签、评级、添加到画布）。
 * - 只读成员看得到、点不动写操作（title 说明原因）。
 */
import { Check, ChevronDown, ChevronRight, Copy, Download, FileText, Folder, FolderInput, FolderOpen, ListChecks, ListFilter, LoaderCircle, Minus, MoreHorizontal, Music, Plus, RotateCcw, Search, SquarePlus, Trash2, Video, X } from "@lucide/vue";
import { ElMessage } from "element-plus";
import { appConfirmDanger, appPrompt } from "~/utils/appDialog";
import { directorCanEdit } from "~/utils/directorRole";
import { extensionFor, safeFilename, saveBlob } from "~/utils/download";
import { LIBRARY_KIND_LABELS, libraryCanvasResource, libraryThumbMediaId, libraryTime, libraryTree, toggleTag, type LibraryFolder, type LibraryItem } from "~/utils/assetLibrary";

const emit = defineEmits<{ created: [ids: string[]]; manage: []; agent: [refs: { id: string; label: string; thumbMediaId?: string; hint?: string }[]] }>();
const workspace = useWorkspaceStore();
const assetLibrary = useAssetLibraryStore();
const canEdit = computed(() => directorCanEdit(workspace.current?.role));
const readonlyHint = "需要编辑权限：你在该工作区是只读成员";

const folders = ref<LibraryFolder[]>([]);
const items = ref<LibraryItem[]>([]);
const loading = ref(false);
const error = ref("");
const hasMore = ref(false);
const nextCursor = ref<string | null>(null);
const query = ref("");
const searchOpen = ref(false);
const appliedTags = ref<string[]>([]);
const expanded = ref<Set<string>>(new Set());
const thumbs = useAuthenticatedMedia();
let seq = 0;

async function load(reset = true) {
  const my = ++seq;
  loading.value = true;
  error.value = "";
  try {
    const q: Record<string, string> = { limit: "200" };
    if (query.value.trim()) q.q = query.value.trim();
    if (appliedTags.value.length) q.tags = appliedTags.value.join(",");
    if (!reset && nextCursor.value) q.cursor = nextCursor.value;
    const [folderRes, itemRes] = await Promise.all([
      reset ? useApi()<{ items: LibraryFolder[] }>("/library/folders") : Promise.resolve({ items: folders.value }),
      useApi()<{ items: LibraryItem[]; hasMore: boolean; nextCursor?: string | null }>("/library/items", { query: q })
    ]);
    if (my !== seq) return;
    folders.value = folderRes.items;
    items.value = reset ? itemRes.items : [...items.value, ...itemRes.items];
    hasMore.value = itemRes.hasMore;
    nextCursor.value = itemRes.nextCursor ?? null;
    const wanted = itemRes.items.map(item => ({ id: libraryThumbMediaId(item), key: item.id })).filter(x => x.id && !thumbs.urls.value[x.key]);
    if (wanted.length) void thumbs.appendAll(wanted.map(x => ({ key: x.key, url: `/api/v1/media/${x.id}?w=160` })));
  } catch (cause) {
    if (my === seq) error.value = cause instanceof Error ? cause.message : "资产取不到，请稍后再试";
  } finally {
    if (my === seq) loading.value = false;
  }
}
onMounted(() => void load());
watch(() => assetLibrary.version, () => void load());
let searchTimer: ReturnType<typeof setTimeout> | undefined;
watch(query, () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => void load(), 300);
});
onBeforeUnmount(() => {
  clearTimeout(searchTimer);
  thumbs.release();
});

const tree = computed(() => libraryTree(folders.value, items.value));
function toggleFolder(id: string) {
  const next = new Set(expanded.value);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  expanded.value = next;
}

// ---------------------------------------------------------------- 筛选（标签）

const tagsPreset = ref<string[]>([]);
const tagsCustom = ref<string[]>([]);
const allTags = computed(() => [...tagsPreset.value, ...tagsCustom.value]);
async function loadTags() {
  try {
    const res = await useApi()<{ preset: string[]; custom: string[] }>("/library/tags");
    tagsPreset.value = res.preset;
    tagsCustom.value = res.custom;
  } catch {
    // 取不到标签：筛选框里只是空着
  }
}
onMounted(() => void loadTags());
const filterOpen = ref(false);
const draftTags = ref<string[]>([]);
const newTagDraft = ref<string | null>(null);
function openFilter() {
  draftTags.value = [...appliedTags.value];
  newTagDraft.value = null;
  filterOpen.value = !filterOpen.value;
  if (filterOpen.value) void loadTags();
}
async function createTag() {
  const name = (newTagDraft.value ?? "").trim();
  newTagDraft.value = null;
  if (!name) return;
  try {
    await useApi()("/library/tags", { method: "POST", body: { name } });
    await loadTags();
    if (!draftTags.value.includes(name)) draftTags.value = [...draftTags.value, name];
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "新建标签失败");
  }
}
function applyFilter() {
  appliedTags.value = [...draftTags.value];
  filterOpen.value = false;
  void load();
}

// ---------------------------------------------------------------- 文件夹

/** 弹一个输入框要名字（取消时抛 "cancel"，同 ElMessageBox）。 */
async function askName(message: string, title: string, options: Parameters<typeof appPrompt>[2]): Promise<string> {
  const result = await appPrompt(message, title, options);
  return (result as { value: string }).value;
}

async function newFolder() {
  if (!canEdit.value) return;
  try {
    const value = await askName("文件夹名称", "新建文件夹", { inputValue: "", confirmButtonText: "新建", cancelButtonText: "取消", inputPattern: /\S/, inputErrorMessage: "名称不能为空" });
    await useApi()("/library/folders", { method: "POST", body: { name: value } });
    await load();
  } catch (cause) {
    if (cause !== "cancel" && cause !== "close") ElMessage.error(cause instanceof Error ? cause.message : "新建失败");
  }
}
async function renameFolder(folder: LibraryFolder) {
  closeMenus();
  try {
    const value = await askName("文件夹名称", "重命名", { inputValue: folder.name, confirmButtonText: "保存", cancelButtonText: "取消", inputPattern: /\S/, inputErrorMessage: "名称不能为空" });
    await useApi()(`/library/folders/${folder.id}`, { method: "PATCH", body: { name: value } });
    await load();
  } catch (cause) {
    if (cause !== "cancel" && cause !== "close") ElMessage.error(cause instanceof Error ? cause.message : "重命名失败");
  }
}
async function deleteFolder(folder: LibraryFolder) {
  closeMenus();
  try {
    await appConfirmDanger(`删除文件夹「${folder.name}」？里面的 ${folder.itemCount} 项资产会退回顶层，不会被删除。`, "删除文件夹", "删除");
    await useApi()(`/library/folders/${folder.id}`, { method: "DELETE" });
    await load();
  } catch (cause) {
    if (cause !== "cancel" && cause !== "close") ElMessage.error(cause instanceof Error ? cause.message : "删除失败");
  }
}

// ---------------------------------------------------------------- 单项菜单

/**
 * 「···」菜单浮在页面最上层（挂到 body、按按钮位置摆），不再塞在行里被列表的滚动区裁掉 / 盖住下一行（2026-10-30 用户反馈）。
 * 菜单项照参考产品：添加到画布、移动到 ›、设置标签 ›、下载、重命名、删除（＝移出资产）。
 */
type MenuTarget = { kind: "item"; item: LibraryItem } | { kind: "folder"; folder: LibraryFolder };
const itemMenu = ref<(MenuTarget & { x: number; y: number }) | null>(null);
const submenu = ref<"move" | "tags" | null>(null);
const floatMenu = ref<HTMLElement | null>(null);
function closeMenus() {
  itemMenu.value = null;
  submenu.value = null;
}
function openMenu(event: MouseEvent, target: MenuTarget) {
  const same = itemMenu.value && (itemMenu.value.kind === target.kind)
    && (target.kind === "item" ? (itemMenu.value as { item?: LibraryItem }).item?.id === target.item.id : (itemMenu.value as { folder?: LibraryFolder }).folder?.id === target.folder.id);
  if (same) {
    closeMenus();
    return;
  }
  const rect = (event.currentTarget as HTMLElement).getBoundingClientRect();
  itemMenu.value = { ...target, x: rect.right - 24, y: rect.bottom + 6 };
  submenu.value = null;
  if (target.kind === "item") void loadTags();
  // 贴着屏幕下沿 / 右沿时整体挪回可视区
  void nextTick(() => {
    const el = floatMenu.value;
    const m = itemMenu.value;
    if (!el || !m) return;
    const x = Math.max(8, Math.min(m.x, window.innerWidth - el.offsetWidth - 8));
    const y = m.y + el.offsetHeight > window.innerHeight - 8 ? Math.max(8, rect.top - el.offsetHeight - 6) : m.y;
    itemMenu.value = { ...m, x, y };
  });
}
async function toggleItemTag(item: LibraryItem, tag: string) {
  const next = toggleTag(item.tags, tag);
  try {
    await useApi()(`/library/items/${item.id}`, { method: "PATCH", body: { tags: next } });
    item.tags = next;
    for (const it of items.value) if (it.id === item.id) it.tags = next;
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "设置标签失败");
  }
}
const downloads = useAuthenticatedMedia();
async function downloadItem(item: LibraryItem) {
  closeMenus();
  if (item.kind === "text") {
    saveBlob(new Blob([item.text ?? ""], { type: "text/markdown;charset=utf-8" }), `${safeFilename(item.title, "文本")}.md`);
    return;
  }
  if (!item.mediaId) return;
  try {
    const blob = await downloads.blob(`/api/v1/media/${item.mediaId}`);
    saveBlob(blob, `${safeFilename(item.title)}.${extensionFor(blob.type)}`);
  } catch {
    ElMessage.error("下载失败，请稍后再试");
  }
}
async function renameItem(item: LibraryItem) {
  closeMenus();
  try {
    const value = await askName("资产名称", "重命名", { inputValue: item.title, confirmButtonText: "保存", cancelButtonText: "取消", inputPattern: /\S/, inputErrorMessage: "名称不能为空" });
    await useApi()(`/library/items/${item.id}`, { method: "PATCH", body: { title: value } });
    await load();
  } catch (cause) {
    if (cause !== "cancel" && cause !== "close") ElMessage.error(cause instanceof Error ? cause.message : "重命名失败");
  }
}
async function moveItems(ids: string[], folderId: string | null) {
  try {
    await useApi()("/library/items/batch", { method: "POST", body: { action: "move", ids, folderId: folderId ?? "" } });
    closeMenus();
    if (folderId) expanded.value = new Set([...expanded.value, folderId]);
    selected.value = new Set();
    await load();
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "移动失败");
  }
}
async function removeItems(ids: string[]) {
  closeMenus();
  try {
    await appConfirmDanger(`把 ${ids.length} 项移出我的资产？画布上的节点和生成结果不受影响。`, "移出资产", "移出");
    await useApi()("/library/items/batch", { method: "POST", body: { action: "remove", ids } });
    if (detail.value && ids.includes(detail.value.id)) detail.value = null;
    selected.value = new Set();
    await load();
  } catch (cause) {
    if (cause !== "cancel" && cause !== "close") ElMessage.error(cause instanceof Error ? cause.message : "移出失败");
  }
}
// ---------------------------------------------------------------- 拖拽（拖进文件夹 / 拖回顶层）

const dragging = ref<LibraryItem | null>(null);
const dropTarget = ref<string | null>(null); // 文件夹 ID；"root" = 顶层
function onDragStart(event: DragEvent, item: LibraryItem) {
  if (!canEdit.value || managing.value) {
    event.preventDefault();
    return;
  }
  dragging.value = item;
  closeMenus();
  event.dataTransfer?.setData("text/plain", item.title);
  if (event.dataTransfer) event.dataTransfer.effectAllowed = "move";
}
function onDragEnd() {
  dragging.value = null;
  dropTarget.value = null;
}
function onDragOver(event: DragEvent, target: string) {
  if (!dragging.value) return;
  event.preventDefault();
  if (event.dataTransfer) event.dataTransfer.dropEffect = "move";
  dropTarget.value = target;
}
function onDrop(event: DragEvent, target: string) {
  const item = dragging.value;
  onDragEnd();
  if (!item) return;
  event.preventDefault();
  const folderId = target === "root" ? null : target;
  if ((item.folderId ?? null) === folderId) return;
  void moveItems([item.id], folderId);
}

// ---------------------------------------------------------------- 管理（多选）

const managing = ref(false);
const selected = ref<Set<string>>(new Set());
function toggleSelect(id: string) {
  const next = new Set(selected.value);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  selected.value = next;
}
function toggleManage() {
  managing.value = !managing.value;
  selected.value = new Set();
  barMenu.value = null;
}
const barMenu = ref<"add" | "move" | null>(null);
const allSelected = computed(() => items.value.length > 0 && items.value.every(item => selected.value.has(item.id)));
function toggleSelectAll() {
  selected.value = allSelected.value ? new Set() : new Set(items.value.map(item => item.id));
}
function folderState(list: LibraryItem[]): "none" | "some" | "all" {
  const n = list.filter(item => selected.value.has(item.id)).length;
  return n === 0 ? "none" : n === list.length ? "all" : "some";
}
function toggleFolderSelection(list: LibraryItem[]) {
  const next = new Set(selected.value);
  const all = folderState(list) === "all";
  for (const item of list) {
    if (all) next.delete(item.id);
    else next.add(item.id);
  }
  selected.value = next;
}
const selectedItems = computed(() => items.value.filter(item => selected.value.has(item.id)));
async function addSelectedToCanvas() {
  barMenu.value = null;
  const resources = selectedItems.value.map(libraryCanvasResource).filter((r): r is NonNullable<typeof r> => Boolean(r));
  if (!resources.length) return;
  const ids = await addResourceNodes(resources);
  if (ids.length) {
    emit("created", ids);
    ElMessage.success(`已添加 ${ids.length} 项到画布`);
  }
}
/** 添加到 Agent：图片 / 视频 / 音频作为媒体附件放进 Agent 输入框；文本没有媒体文件，跳过并说一声。 */
function addSelectedToAgent() {
  barMenu.value = null;
  const media = selectedItems.value.filter(item => item.kind !== "text" && item.mediaId);
  const skipped = selectedItems.value.length - media.length;
  if (media.length) {
    emit("agent", media.map(item => ({ id: item.mediaId!, label: item.title, thumbMediaId: libraryThumbMediaId(item) || undefined, hint: item.kind })));
  }
  if (skipped) ElMessage.info(media.length ? `已添加 ${media.length} 项；${skipped} 项文本不能作为附件，已跳过` : "文本资产不能作为 Agent 附件");
}
async function downloadSelected() {
  for (const item of selectedItems.value) await downloadItem(item);
}

// ---------------------------------------------------------------- 详情卡片

const detail = ref<LibraryItem | null>(null);
const detailMedia = useAuthenticatedMedia();
const detailSrc = ref("");
let detailSeq = 0;
watch(detail, async (item) => {
  const my = ++detailSeq;
  detailMedia.release();
  detailSrc.value = "";
  if (!item?.mediaId) return;
  try {
    // 视频取预览版（短边 480），没有时取原片；图片取 640 宽缩略图
    let url = `/api/v1/media/${item.mediaId}?w=640`;
    if (item.kind === "video" || item.kind === "audio") {
      const assets = await useApi()<{ preview?: { mediaId?: string } | null }>(`/media/${item.mediaId}/preview-assets`).catch(() => null);
      url = `/api/v1/media/${assets?.preview?.mediaId ?? item.mediaId}`;
    }
    const blob = await detailMedia.blob(url);
    if (my !== detailSeq) return;
    detailSrc.value = URL.createObjectURL(blob);
  } catch {
    // 取不到预览：卡片只显示图标
  }
});
onBeforeUnmount(() => {
  if (detailSrc.value) URL.revokeObjectURL(detailSrc.value);
  detailMedia.release();
});
function openItem(item: LibraryItem) {
  if (managing.value) {
    toggleSelect(item.id);
    return;
  }
  detail.value = detail.value?.id === item.id ? null : item;
}
async function copyPrompt(text: string) {
  try {
    await navigator.clipboard.writeText(text);
    ElMessage.success("已复制生成信息");
  } catch {
    ElMessage.error("复制失败：浏览器不允许访问剪贴板");
  }
}
async function rate(item: LibraryItem, star: number) {
  if (!canEdit.value) return;
  const next = item.rating === star ? 0 : star;
  try {
    await useApi()(`/library/ratings/${item.generationId}`, { method: "PUT", body: { rating: next } });
    for (const it of items.value) if (it.generationId === item.generationId) it.rating = next;
    if (detail.value?.generationId === item.generationId) detail.value = { ...detail.value, rating: next };
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "评级失败");
  }
}
const { addResourceNodes } = useCanvasResourceNodes();
async function addToCanvas(item: LibraryItem) {
  const resource = libraryCanvasResource(item);
  if (!resource) {
    ElMessage.error("这项资产没有可用的内容");
    return;
  }
  const ids = await addResourceNodes([resource]);
  if (ids.length) {
    emit("created", ids);
    ElMessage.success("已添加到画布");
  }
}

function onOutside(event: PointerEvent) {
  const target = event.target;
  if (!(target instanceof Element)) return;
  if (!target.closest(".alp-float, .alp-more")) closeMenus();
  if (!target.closest(".alp-filter, .alp-filter-btn")) filterOpen.value = false;
  if (!target.closest(".alp-bar-anchor")) barMenu.value = null;
}
onMounted(() => document.addEventListener("pointerdown", onOutside, true));
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside, true));
</script>

<template>
  <div class="alp" data-testid="asset-library-panel">
    <div class="alp-toolbar">
      <button type="button" class="alp-icon" :class="{ active: searchOpen }" title="搜索" aria-label="搜索" @click="searchOpen = !searchOpen"><Search :size="16" /></button>
      <button type="button" class="alp-icon" :class="{ active: managing }" :title="managing ? '退出多选' : '多选'" aria-label="多选" :aria-pressed="managing" data-testid="asset-multi" @click="toggleManage"><ListChecks :size="16" /></button>
      <button type="button" class="alp-icon alp-filter-btn" :class="{ active: appliedTags.length }" title="筛选" aria-label="筛选" data-testid="asset-filter" @click="openFilter"><ListFilter :size="16" /><i v-if="appliedTags.length">{{ appliedTags.length }}</i></button>
      <span class="alp-spacer" />
      <button type="button" class="alp-icon" :disabled="!canEdit" :title="canEdit ? '新建文件夹' : readonlyHint" aria-label="新建文件夹" data-testid="asset-new-folder" @click="newFolder"><Plus :size="17" /></button>
      <button type="button" class="alp-manage" title="打开资产库：生成历史、我保存的、我上传的" data-testid="asset-manage" @click="emit('manage')"><FolderOpen :size="15" />管理</button>

      <div v-if="filterOpen" class="alp-filter" role="dialog" aria-label="筛选">
        <header><strong>筛选</strong><button type="button" class="alp-reset" @click="draftTags = []"><RotateCcw :size="13" />重置</button></header>
        <small>标签</small>
        <div class="alp-chips">
          <input v-if="newTagDraft !== null" v-model="newTagDraft" class="alp-chip-input" maxlength="20" placeholder="标签名" @keydown.enter.prevent="createTag" @blur="createTag">
          <button v-else type="button" class="alp-chip" :disabled="!canEdit" :title="canEdit ? '新建标签' : readonlyHint" @click="newTagDraft = ''"><Plus :size="13" />新建标签</button>
          <button v-for="tag in allTags" :key="tag" type="button" class="alp-chip" :class="{ on: draftTags.includes(tag) }" @click="draftTags = toggleTag(draftTags, tag)">{{ tag }}</button>
        </div>
        <footer><button type="button" class="alp-btn" @click="filterOpen = false">取消</button><button type="button" class="alp-btn primary" @click="applyFilter">应用</button></footer>
      </div>
    </div>
    <label v-if="searchOpen" class="alp-search"><Search :size="14" /><input v-model="query" placeholder="搜索名称、内容或提示词" aria-label="搜索资产"><button v-if="query" type="button" aria-label="清空" @click="query = ''"><X :size="13" /></button></label>
    <p v-if="appliedTags.length" class="alp-applied">标签：{{ appliedTags.join('、') }}<button type="button" @click="appliedTags = []; load()">清除</button></p>

    <!-- 拖到列表空白处 = 挪回顶层 -->
    <div
      class="alp-list"
      :class="{ 'drop-root': dragging && dropTarget === 'root' }"
      @dragover="onDragOver($event, 'root')"
      @drop="onDrop($event, 'root')"
    >
      <p v-if="error" class="alp-empty" role="alert">{{ error }} <button type="button" @click="load()">重试</button></p>
      <p v-else-if="!loading && !items.length && !folders.length" class="alp-empty">
        {{ query || appliedTags.length ? '没有符合条件的资产' : '还没有保存的资产。在画布上右键生成结果，选「保存到我的资产」。' }}
      </p>
      <template v-for="item in tree.root" :key="item.id">
        <div
          class="alp-row"
          :class="{ active: detail?.id === item.id, picked: selected.has(item.id), dragging: dragging?.id === item.id }"
          data-testid="asset-item"
          :draggable="canEdit && !managing"
          @click="openItem(item)"
          @dragstart="onDragStart($event, item)"
          @dragend="onDragEnd"
        >
          <span class="alp-thumb" :class="item.kind">
            <img v-if="thumbs.urls.value[item.id]" :src="thumbs.urls.value[item.id]" alt="" :draggable="false">
            <FileText v-else-if="item.kind === 'text'" :size="16" /><Video v-else-if="item.kind === 'video'" :size="16" /><Music v-else :size="16" />
          </span>
          <span class="alp-title">{{ item.title }}</span>
          <span v-if="managing" class="alp-check" :class="{ on: selected.has(item.id) }" aria-hidden="true"><Check v-if="selected.has(item.id)" :size="12" stroke-width="3" /></span>
          <button v-else type="button" class="alp-more" aria-label="更多" @click.stop="openMenu($event, { kind: 'item', item })"><MoreHorizontal :size="16" /></button>
        </div>
      </template>
      <template v-for="node in tree.folders" :key="node.folder.id">
        <div
          class="alp-row folder"
          :class="{ 'drop-over': dropTarget === node.folder.id }"
          data-testid="asset-folder"
          @click="toggleFolder(node.folder.id)"
          @dragover="onDragOver($event, node.folder.id)"
          @dragleave="dropTarget === node.folder.id && (dropTarget = null)"
          @drop.stop="onDrop($event, node.folder.id)"
        >
          <span class="alp-caret"><ChevronDown v-if="expanded.has(node.folder.id)" :size="14" /><ChevronRight v-else :size="14" /></span>
          <span class="alp-folder-icon"><Folder :size="20" /></span>
          <span class="alp-title">{{ node.folder.name }}</span>
          <button
            v-if="managing"
            type="button"
            class="alp-check"
            :class="{ on: folderState(node.items) === 'all', part: folderState(node.items) === 'some' }"
            :disabled="!node.items.length"
            :aria-label="`选择「${node.folder.name}」里的全部资产`"
            @click.stop="toggleFolderSelection(node.items)"
          ><Check v-if="folderState(node.items) === 'all'" :size="12" stroke-width="3" /><Minus v-else-if="folderState(node.items) === 'some'" :size="12" stroke-width="3" /></button>
          <button v-else type="button" class="alp-more" aria-label="更多" @click.stop="openMenu($event, { kind: 'folder', folder: node.folder })"><MoreHorizontal :size="16" /></button>
        </div>
        <template v-if="expanded.has(node.folder.id)">
          <div
            v-for="item in node.items"
            :key="item.id"
            class="alp-row nested"
            :class="{ active: detail?.id === item.id, picked: selected.has(item.id), dragging: dragging?.id === item.id }"
            data-testid="asset-item"
            :draggable="canEdit && !managing"
            @click="openItem(item)"
            @dragstart="onDragStart($event, item)"
            @dragend="onDragEnd"
          >
            <span class="alp-thumb" :class="item.kind">
              <img v-if="thumbs.urls.value[item.id]" :src="thumbs.urls.value[item.id]" alt="" :draggable="false">
              <FileText v-else-if="item.kind === 'text'" :size="16" /><Video v-else-if="item.kind === 'video'" :size="16" /><Music v-else :size="16" />
            </span>
            <span class="alp-title">{{ item.title }}</span>
            <span v-if="managing" class="alp-check" :class="{ on: selected.has(item.id) }" aria-hidden="true"><Check v-if="selected.has(item.id)" :size="12" stroke-width="3" /></span>
            <button v-else type="button" class="alp-more" aria-label="更多" @click.stop="openMenu($event, { kind: 'item', item })"><MoreHorizontal :size="16" /></button>
          </div>
          <p v-if="!node.items.length" class="alp-empty nested">空文件夹</p>
        </template>
      </template>
      <button v-if="hasMore" type="button" class="alp-more-load" :disabled="loading" @click="load(false)">加载更多</button>
      <p v-if="loading && !items.length" class="alp-empty"><LoaderCircle :size="14" class="spin" /> 加载中</p>
    </div>

    <!-- 多选的操作条：退出、全选 / 已选几项、添加到 ▴（Agent / 画布）、移动到 ▴、下载、删除。侧栏只有 232px 放不下「已选择 N」，只显示数字（完整说法在 title / aria-label）、添加到画布、移动到 ▾、下载、删除 -->
    <div v-if="managing" class="alp-bar" data-testid="asset-batch-bar">
      <button type="button" class="alp-bar-icon plain" title="退出多选" aria-label="退出多选" @click="toggleManage"><X :size="15" /></button>
      <button type="button" class="alp-bar-count" :title="`已选择 ${selected.size} 项，点击${allSelected ? '取消全选' : '全选'}`" :aria-label="`已选择 ${selected.size} 项`" @click="toggleSelectAll">
        <span class="alp-check" :class="{ on: allSelected, part: selected.size > 0 && !allSelected }"><Check v-if="allSelected" :size="11" stroke-width="3" /><Minus v-else-if="selected.size" :size="11" stroke-width="3" /></span>
        <em data-testid="asset-batch-count">{{ selected.size }}</em>
      </button>
      <span class="alp-spacer" />
      <div class="alp-bar-anchor">
        <button type="button" class="alp-bar-icon wide" :class="{ open: barMenu === 'add' }" :disabled="!selected.size" title="添加到" aria-label="添加到" data-testid="asset-batch-add" @click="barMenu = barMenu === 'add' ? null : 'add'"><SquarePlus :size="15" /><ChevronDown :size="11" /></button>
        <div v-if="barMenu === 'add'" class="alp-bar-pop center">
          <button type="button" data-testid="asset-batch-agent" @click="addSelectedToAgent">添加到Agent</button>
          <button type="button" @click="addSelectedToCanvas">添加到画布</button>
        </div>
      </div>
      <div class="alp-bar-anchor">
        <button type="button" class="alp-bar-icon" :disabled="!selected.size || !canEdit" :title="canEdit ? '移动到' : readonlyHint" aria-label="移动到" @click="barMenu = barMenu === 'move' ? null : 'move'"><FolderInput :size="15" /></button>
        <div v-if="barMenu === 'move'" class="alp-bar-pop">
          <button type="button" @click="moveItems([...selected], null); barMenu = null"><Folder :size="14" />个人资产库</button>
          <button v-for="folder in folders" :key="folder.id" type="button" @click="moveItems([...selected], folder.id); barMenu = null"><Folder :size="14" />{{ folder.name }}</button>
        </div>
      </div>
      <button type="button" class="alp-bar-icon" :disabled="!selected.size" title="下载" aria-label="下载" @click="downloadSelected"><Download :size="15" /></button>
      <button type="button" class="alp-bar-icon danger" :disabled="!selected.size || !canEdit" :title="canEdit ? '删除（移出我的资产）' : readonlyHint" aria-label="删除" @click="removeItems([...selected])"><Trash2 :size="15" /></button>
    </div>


    <!-- 「···」菜单：挂到 body，浮在列表之上 -->
    <Teleport to="body">
      <div v-if="itemMenu" ref="floatMenu" class="alp-float" :style="{ left: `${itemMenu.x}px`, top: `${itemMenu.y}px` }" role="menu" @click.stop>
        <template v-if="itemMenu.kind === 'item'">
          <button type="button" role="menuitem" @click="addToCanvas(itemMenu.item); closeMenus()">添加到画布</button>
          <div class="alp-sub-anchor" @mouseenter="submenu = 'move'" @mouseleave="submenu === 'move' && (submenu = null)">
            <button type="button" role="menuitem" :disabled="!canEdit" :title="canEdit ? '' : readonlyHint" @click="submenu = submenu === 'move' ? null : 'move'">移动到<ChevronRight :size="14" /></button>
            <div v-if="submenu === 'move' && canEdit" class="alp-sub" role="menu">
              <button type="button" :class="{ current: !itemMenu.item.folderId }" @click="moveItems([itemMenu.item.id], null)">顶层<Check v-if="!itemMenu.item.folderId" :size="13" /></button>
              <button v-for="folder in folders" :key="folder.id" type="button" :class="{ current: itemMenu.item.folderId === folder.id }" @click="moveItems([itemMenu.item.id], folder.id)">
                <span><Folder :size="13" />{{ folder.name }}</span><Check v-if="itemMenu.item.folderId === folder.id" :size="13" />
              </button>
              <p v-if="!folders.length" class="alp-sub-empty">还没有文件夹</p>
            </div>
          </div>
          <div class="alp-sub-anchor" @mouseenter="submenu = 'tags'" @mouseleave="submenu === 'tags' && (submenu = null)">
            <button type="button" role="menuitem" :disabled="!canEdit" :title="canEdit ? '' : readonlyHint" @click="submenu = submenu === 'tags' ? null : 'tags'">设置标签<ChevronRight :size="14" /></button>
            <div v-if="submenu === 'tags' && canEdit" class="alp-sub" role="menu">
              <button v-for="tag in allTags" :key="tag" type="button" role="menuitemcheckbox" :aria-checked="itemMenu.item.tags.includes(tag)" @click="toggleItemTag(itemMenu.item, tag)">
                <span>{{ tag }}</span><Check v-if="itemMenu.item.tags.includes(tag)" :size="13" />
              </button>
            </div>
          </div>
          <hr>
          <button type="button" role="menuitem" @click="downloadItem(itemMenu.item)">下载</button>
          <button type="button" role="menuitem" :disabled="!canEdit" @click="renameItem(itemMenu.item)">重命名</button>
          <button type="button" role="menuitem" class="danger" :disabled="!canEdit" @click="removeItems([itemMenu.item.id])">删除</button>
        </template>
        <template v-else>
          <button type="button" role="menuitem" :disabled="!canEdit" @click="renameFolder(itemMenu.folder)">重命名</button>
          <button type="button" role="menuitem" class="danger" :disabled="!canEdit" @click="deleteFolder(itemMenu.folder)">删除文件夹</button>
        </template>
      </div>
    </Teleport>

    <!-- 详情卡片 -->
    <Teleport to="body">
      <section v-if="detail" class="alp-detail" role="dialog" :aria-label="detail.title" data-testid="asset-detail">
        <button type="button" class="alp-detail-close" aria-label="关闭" @click="detail = null"><X :size="16" /></button>
        <div class="alp-detail-media">
          <img v-if="detail.kind === 'image' && detailSrc" :src="detailSrc" :alt="detail.title">
          <video v-else-if="detail.kind === 'video' && detailSrc" :src="detailSrc" controls playsinline />
          <audio v-else-if="detail.kind === 'audio' && detailSrc" :src="detailSrc" controls />
          <!-- 文本不在卡片上显示内容（参考产品同款：只放一个图标），添加到画布后在节点上看 -->
          <span v-else-if="detail.kind === 'text'" class="alp-detail-icon"><FileText :size="44" stroke-width="1.4" /></span>
          <span v-else-if="detail.kind === 'audio' && !detailSrc" class="alp-detail-icon"><Music :size="40" stroke-width="1.4" /></span>
          <span v-else class="alp-detail-icon"><LoaderCircle :size="22" class="spin" /></span>
        </div>
        <div class="alp-detail-body">
          <strong class="alp-detail-title">{{ detail.title }}</strong>
          <dl>
            <div><dt>创作者</dt><dd>{{ detail.creatorName || '—' }}</dd></div>
            <div><dt>创建于</dt><dd>{{ libraryTime(detail.generatedAt) }}</dd></div>
            <div v-if="detail.modelName || detail.modelCode"><dt>资产来源</dt><dd>{{ detail.modelName || detail.modelCode }}</dd></div>
            <div><dt>类型</dt><dd>{{ LIBRARY_KIND_LABELS[detail.kind] }}</dd></div>
            <div>
              <dt>评级</dt>
              <dd class="alp-stars">
                <button v-for="star in 5" :key="star" type="button" :class="{ on: star <= detail.rating }" :disabled="!canEdit" :aria-label="`${star} 星`" @click="rate(detail, star)">★</button>
              </dd>
            </div>
            <div v-if="detail.tags.length"><dt>标签</dt><dd class="alp-tags"><span v-for="tag in detail.tags" :key="tag">{{ tag }}</span></dd></div>
          </dl>
          <div v-if="detail.prompt" class="alp-prompt">
            <div><span>生成信息</span><button type="button" title="复制" aria-label="复制生成信息" @click="copyPrompt(detail.prompt)"><Copy :size="14" /></button></div>
            <p>{{ detail.prompt }}</p>
          </div>
        </div>
        <footer><button type="button" class="alp-add" data-testid="asset-add-to-canvas" @click="addToCanvas(detail)">添加到画布</button></footer>
      </section>
    </Teleport>
  </div>
</template>

<style scoped>
.alp { position: relative; min-width: 0; min-height: 0; flex: 1; display: flex; flex-direction: column; }
.alp-toolbar { position: relative; display: flex; align-items: center; gap: 2px; padding: 4px 0 10px; }
.alp-spacer { flex: 1; }
.alp-icon {
  position: relative; width: 30px; height: 30px; flex: 0 0 auto; display: grid; place-items: center; border: 0; border-radius: 8px;
  color: var(--cv-n70, var(--canvas-muted)); background: transparent; cursor: pointer;
}
.alp-icon:hover:not(:disabled), .alp-icon.active { color: var(--cv-n96, var(--canvas-text)); background: var(--cv-n22, var(--canvas-line)); }
.alp-icon:disabled { opacity: .45; cursor: not-allowed; }
.alp-icon i { position: absolute; top: 2px; right: 2px; min-width: 14px; height: 14px; border-radius: 7px; color: #fff; background: var(--canvas-cyan); font-size: 10px; font-style: normal; line-height: 14px; }
.alp-manage { height: 30px; flex: 0 0 auto; display: inline-flex; align-items: center; gap: 4px; border: 0; border-radius: 8px; padding: 0 8px; white-space: nowrap; color: var(--cv-n80, var(--canvas-text)); background: transparent; font-size: 13px; cursor: pointer; }
.alp-manage:hover:not(:disabled), .alp-manage.active { background: var(--cv-n22, var(--canvas-line)); }
.alp-manage:disabled { opacity: .45; cursor: not-allowed; }
.alp-search { display: flex; align-items: center; gap: 6px; height: 32px; margin: 0 4px 8px; padding: 0 10px; border-radius: 8px; color: var(--canvas-muted); background: var(--cv-n22, var(--canvas-line)); }
.alp-search input { min-width: 0; flex: 1; border: 0; outline: 0; color: var(--canvas-text); background: transparent; font-size: 13px; }
.alp-search button { display: grid; place-items: center; border: 0; color: var(--canvas-muted); background: transparent; cursor: pointer; }
.alp-applied { display: flex; align-items: center; gap: 6px; margin: 0 6px 8px; color: var(--canvas-muted); font-size: 12px; }
.alp-applied button { border: 0; color: var(--canvas-cyan); background: transparent; font-size: 12px; cursor: pointer; }
.alp-list { min-height: 0; flex: 1; overflow-y: auto; display: grid; align-content: start; gap: 2px; }
.alp-row {
  position: relative; height: 48px; display: flex; align-items: center; gap: 10px; padding: 0 6px 0 8px; border-radius: 10px;
  color: var(--cv-n90, var(--canvas-text)); cursor: pointer;
}
.alp-row:hover, .alp-row.active { background: var(--cv-n22, var(--canvas-line)); }
.alp-row.picked { background: color-mix(in srgb, var(--canvas-cyan) 16%, transparent); }
.alp-row.nested { padding-left: 34px; }
.alp-thumb { width: 36px; height: 36px; flex: 0 0 auto; display: grid; place-items: center; overflow: hidden; border-radius: 8px; color: var(--cv-n63, var(--canvas-muted)); background: var(--cv-n22, var(--canvas-line)); }
.alp-thumb img { width: 100%; height: 100%; object-fit: cover; }
.alp-caret { width: 16px; display: grid; place-items: center; color: var(--canvas-muted); }
.alp-folder-icon { color: var(--cv-n63, #9aa0a6); }
.alp-title { min-width: 0; flex: 1; overflow: hidden; font-size: 13px; text-overflow: ellipsis; white-space: nowrap; }
.alp-more { width: 26px; height: 26px; display: grid; place-items: center; border: 0; border-radius: 6px; color: var(--canvas-muted); background: transparent; opacity: 0; cursor: pointer; }
.alp-row:hover .alp-more, .alp-row.active .alp-more { opacity: 1; }
.alp-more:hover { color: var(--canvas-text); background: var(--cv-n15, rgba(0, 0, 0, .06)); }
.alp-check {
  width: 18px; height: 18px; flex: 0 0 auto; display: grid; place-items: center; margin-right: 6px; padding: 0; border: 1.5px solid var(--cv-n57, #a0a3a8);
  border-radius: 5px; color: var(--cv-n08, #fff); background: transparent; cursor: pointer;
}
.alp-check.on, .alp-check.part { border-color: var(--cv-n94, #111); background: var(--cv-n94, #111); }
.alp-check:disabled { opacity: .35; cursor: default; }
/* 多选操作条：贴在面板底部 */
.alp-bar {
  position: sticky; bottom: 8px; z-index: 5; min-width: 0; display: flex; align-items: center; gap: 2px; margin: 8px 0; padding: 4px;
  border: 1px solid var(--canvas-line); border-radius: 14px; background: var(--cv-n15, var(--canvas-panel, #fff));
  box-shadow: 0 8px 24px var(--cv-shade-30, rgba(0, 0, 0, .14));
}
.alp-bar-icon {
  height: 30px; min-width: 30px; flex: 0 0 auto; display: inline-flex; align-items: center; justify-content: center; gap: 1px; padding: 0 5px; border: 0; border-radius: 8px;
  color: var(--cv-n80, var(--canvas-text)); background: var(--cv-n22, rgba(0, 0, 0, .05)); cursor: pointer;
}
.alp-bar-icon.plain { background: transparent; }
.alp-bar-icon.wide { padding: 0 3px; }
.alp-bar-icon:hover:not(:disabled) { color: var(--cv-n96, var(--canvas-text)); }
.alp-bar-icon:disabled { opacity: .4; cursor: not-allowed; }
.alp-bar-icon.danger { color: var(--portal-danger, #e5484d); background: color-mix(in srgb, var(--portal-danger, #e5484d) 12%, transparent); }
.alp-bar-count { min-width: 0; flex: 0 1 auto; overflow: hidden; display: inline-flex; align-items: center; gap: 5px; border: 0; padding: 0 2px; color: var(--cv-n90, var(--canvas-text)); background: transparent; font-size: 13px; white-space: nowrap; cursor: pointer; }
.alp-bar-count .alp-check { width: 16px; height: 16px; margin: 0; }
.alp-bar-count em { min-width: 0; overflow: hidden; font-style: normal; font-weight: 600; text-overflow: ellipsis; }
.alp-bar-anchor { position: relative; }
.alp-bar-pop.center { right: auto; left: 50%; min-width: 136px; transform: translateX(-50%); }
.alp-bar-pop {
  position: absolute; bottom: calc(100% + 8px); right: 0; z-index: 10; min-width: 160px; padding: 6px; border: 1px solid var(--canvas-line); border-radius: 12px;
  background: var(--cv-n15, var(--canvas-panel, #fff)); box-shadow: 0 14px 36px var(--cv-shade-30, rgba(0, 0, 0, .18));
}
.alp-bar-pop button { width: 100%; height: 34px; display: flex; align-items: center; gap: 8px; border: 0; border-radius: 8px; padding: 0 12px; color: var(--cv-n90, var(--canvas-text)); background: transparent; font-size: 13px; text-align: left; white-space: nowrap; cursor: pointer; }
.alp-bar-pop button:hover { background: var(--cv-n22, var(--canvas-line)); }
.alp-bar-pop svg { color: var(--canvas-muted); }
.alp-filter, .alp-move {
  position: absolute; z-index: 40; border: 1px solid var(--canvas-line); border-radius: 12px;
  background: var(--cv-n15, var(--canvas-panel)); box-shadow: 0 14px 36px var(--cv-shade-30, rgba(0, 0, 0, .18));
}
.alp-move > button {
  width: 100%; height: 32px; display: flex; align-items: center; gap: 8px; border: 0; border-radius: 6px; padding: 0 10px;
  color: var(--cv-n90, var(--canvas-text)); background: transparent; font-size: 13px; text-align: left; cursor: pointer;
}
.alp-move > button:hover { background: var(--cv-n22, var(--canvas-line)); }
/* 「···」浮动菜单与子菜单（挂在 body 上） */
.alp-float, .alp-sub {
  min-width: 180px; padding: 6px; border: 1px solid var(--canvas-line); border-radius: 12px;
  background: var(--cv-n15, var(--canvas-panel, #fff)); box-shadow: 0 14px 36px var(--cv-shade-30, rgba(0, 0, 0, .18));
}
.alp-float { position: fixed; z-index: 70; }
.alp-float button, .alp-sub button {
  width: 100%; height: 36px; display: flex; align-items: center; justify-content: space-between; gap: 8px; border: 0; border-radius: 8px; padding: 0 12px;
  color: var(--cv-n90, var(--canvas-text)); background: transparent; font-size: 14px; text-align: left; cursor: pointer;
}
.alp-float button:hover:not(:disabled), .alp-sub button:hover { background: var(--cv-n22, var(--canvas-line)); }
.alp-float button:disabled { color: var(--canvas-muted); cursor: not-allowed; }
.alp-float .danger { color: var(--portal-danger, #e5484d); }
.alp-float hr { height: 1px; margin: 5px 8px; border: 0; background: var(--canvas-line); }
.alp-float svg, .alp-sub svg { flex: 0 0 auto; color: var(--canvas-muted); }
.alp-sub-anchor { position: relative; }
.alp-sub { position: absolute; top: -6px; left: calc(100% + 4px); max-height: 280px; overflow-y: auto; }
.alp-sub button span { display: inline-flex; align-items: center; gap: 6px; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.alp-sub-empty { margin: 8px 12px; color: var(--canvas-muted); font-size: 12px; }
/* 拖拽 */
.alp-row.dragging { opacity: .45; }
.alp-row.folder.drop-over { background: color-mix(in srgb, var(--canvas-cyan) 16%, transparent); box-shadow: inset 0 0 0 1px var(--canvas-cyan); }
.alp-list.drop-root { box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--canvas-cyan) 50%, transparent); border-radius: 10px; }
.alp-filter { top: 40px; left: 0; width: 280px; padding: 14px 16px 16px; }
.alp-filter header, .alp-move header { display: flex; align-items: center; justify-content: space-between; margin-bottom: 10px; font-size: 14px; }
.alp-filter > small { display: block; margin-bottom: 8px; color: var(--canvas-muted); font-size: 12px; }
.alp-reset { display: inline-flex; align-items: center; gap: 4px; border: 0; color: var(--canvas-muted); background: transparent; font-size: 12px; cursor: pointer; }
.alp-chips { display: flex; flex-wrap: wrap; gap: 8px; }
.alp-chip {
  height: 30px; display: inline-flex; align-items: center; gap: 4px; padding: 0 12px; border: 1px solid var(--canvas-line); border-radius: 999px;
  color: var(--cv-n90, var(--canvas-text)); background: transparent; font-size: 13px; cursor: pointer;
}
.alp-chip.on { border-color: var(--canvas-cyan); color: var(--canvas-cyan); background: color-mix(in srgb, var(--canvas-cyan) 10%, transparent); }
.alp-chip:disabled { opacity: .45; cursor: not-allowed; }
.alp-chip-input { width: 110px; height: 30px; padding: 0 12px; border: 1px solid var(--canvas-cyan); border-radius: 999px; outline: 0; color: var(--canvas-text); background: transparent; font-size: 13px; }
.alp-filter footer, .alp-move footer { display: flex; justify-content: flex-end; gap: 8px; margin-top: 16px; }
.alp-btn { height: 34px; display: inline-flex; align-items: center; gap: 6px; padding: 0 18px; border: 0; border-radius: 10px; color: var(--cv-n90, var(--canvas-text)); background: var(--cv-n22, var(--canvas-line)); font-size: 13px; cursor: pointer; }
.alp-btn.primary { color: var(--cv-n08, #fff); background: var(--cv-n94, #111); }
.alp-btn.danger { color: var(--portal-danger, #e5484d); }
.alp-btn:disabled { opacity: .45; cursor: not-allowed; }
.alp-move { top: 40px; left: 4px; right: 4px; padding: 12px; }
.alp-empty { margin: 16px 8px; color: var(--canvas-muted); font-size: 12px; line-height: 1.6; }
.alp-empty.nested { margin: 4px 0 6px 40px; }
.alp-empty button { border: 0; color: var(--canvas-cyan); background: transparent; cursor: pointer; }
.alp-more-load { margin: 8px auto; border: 1px solid var(--canvas-line); border-radius: 8px; padding: 4px 14px; color: var(--cv-n80); background: transparent; font-size: 12px; cursor: pointer; }
/* 详情卡片（挂在 body 上，贴在侧栏右边） */
.alp-detail {
  position: fixed; z-index: 60; top: 110px; left: 252px; width: 300px; max-height: calc(100vh - 140px); display: flex; flex-direction: column; overflow: hidden;
  border: 1px solid var(--canvas-line); border-radius: 18px; color: var(--cv-n90, var(--canvas-text)); background: var(--cv-n15, var(--canvas-panel));
  box-shadow: 0 24px 60px var(--cv-shade-34, rgba(0, 0, 0, .22));
}
.alp-detail-close { position: absolute; top: 10px; right: 10px; z-index: 2; width: 28px; height: 28px; display: grid; place-items: center; border: 0; border-radius: 8px; color: #fff; background: rgba(0, 0, 0, .45); cursor: pointer; }
.alp-detail-media { min-height: 220px; max-height: 300px; display: grid; place-items: center; overflow: hidden; background: #333436; }
.alp-detail-media img, .alp-detail-media video { width: 100%; max-height: 300px; object-fit: contain; }
.alp-detail-media audio { width: 90%; }
.alp-detail-icon { display: grid; place-items: center; color: #8d9096; }
.alp-detail-body { min-height: 0; overflow-y: auto; padding: 14px 18px 4px; }
.alp-detail-title { display: block; margin-bottom: 10px; font-size: 14px; }
.alp-detail dl { display: grid; gap: 12px; margin: 0; }
.alp-detail dl > div { display: flex; justify-content: space-between; gap: 12px; font-size: 13px; }
.alp-detail dt { flex: 0 0 auto; color: var(--canvas-muted); }
.alp-detail dd { min-width: 0; margin: 0; overflow: hidden; text-align: right; text-overflow: ellipsis; white-space: nowrap; }
.alp-stars button { border: 0; padding: 0 1px; color: var(--cv-n40, #c5c7cb); background: transparent; font-size: 15px; cursor: pointer; }
.alp-stars button.on { color: #f5b301; }
.alp-stars button:disabled { cursor: default; }
.alp-tags { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 4px; white-space: normal; }
.alp-tags span { padding: 1px 8px; border-radius: 999px; background: var(--cv-n22, var(--canvas-line)); font-size: 12px; }
.alp-prompt { margin-top: 12px; font-size: 13px; }
.alp-prompt > div { display: flex; align-items: center; justify-content: space-between; color: var(--canvas-muted); }
.alp-prompt button { display: grid; place-items: center; border: 0; color: var(--canvas-muted); background: transparent; cursor: pointer; }
.alp-prompt p { max-height: 96px; margin: 6px 0 0; overflow-y: auto; line-height: 1.6; white-space: pre-wrap; }
.alp-detail footer { padding: 12px 16px 16px; border-top: 1px solid var(--canvas-line); }
.alp-add { width: 100%; height: 40px; border: 0; border-radius: 10px; color: #fff; background: #151617; font-size: 14px; cursor: pointer; }
:global(:root[data-theme="dark"]) .alp-add { color: #111; background: #f2f3f5; }
</style>
