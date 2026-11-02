<script setup lang="ts">
/**
 * 角色选择弹窗（MKT-T-39，营销 dev/05 §4.4、§5；MKT-F-100~102、MKT-F-109~112、MKT-UX-14/16/17，对标截图 61）。
 *
 * 页签只有两个：
 * - **我的**：本工作区的角色库（搜索 / 排序 / 游标分页；悬停「编辑 / 删除」；从营销角色添加来的标「营销角色」）
 *   + 只读分组「**脚本角色资产**」（`GET /script-assets?type=character`，只能挑，不能在这里编辑，MKT-PARAM-24）。
 * - **营销角色**：平台预置（公开端点，图片直接 `<img>`）；「添加」= `adopt` 复制一份进「我的」再选中（MKD-30），
 *   同一版本重复添加服务端回同一份，不会多出副本。
 * 「＋ 新增」**直接**打开新建角色（只有非真人一种，MK-23）。可多选到上限（`maxCharacterRefs`，满了说「最多 2 个角色」）。
 *
 * **选择即放入，确认才写库**（dev/05 §5）：「添加」只把选择交回输入框；新增 / 编辑 / 删除 / 添加营销角色才写库 ——
 * 所以只读成员能浏览、能选，写库的按钮全禁用并说明（MKT-UX-17）。
 *
 * ⚠️ 打开只发 GET。工作区角色的图是受保护媒体（`useAuthenticatedMedia`，关闭即 revoke）；营销角色的图走公开端点 ——
 *    两种别混用（MKT-UX-16）。
 */
import type { components } from "@tabtv/api-client";
import { ElMessage, ElMessageBox } from "element-plus";
import { Check, LoaderCircle, Pencil, Plus, Search, Trash2, UserRound, X } from "@lucide/vue";
import CharacterEditorDialog from "./CharacterEditorDialog.vue";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { apiFailure } from "~/utils/agentPanelBridge";
import {
  MARKETING_CHARACTER_READ_ONLY_TEXT,
  MARKETING_LIBRARY_PAGE_SIZE,
  MARKETING_LIBRARY_SORTS,
  MARKETING_SEARCH_DEBOUNCE_MS,
  marketingCharacterDeleteConfirm,
  marketingCharacterEmptyText,
  marketingCharacterPickChanged,
  marketingCharacterQuota,
  marketingCharacterSourceLabel,
  marketingIsQuotaError,
  marketingLibraryQuery,
  marketingPickKey,
  marketingPresetImageUrl,
  marketingToggleCharacter,
  type MarketingCharacter,
  type MarketingCharacterLimits,
  type MarketingCharacterSummary,
  type MarketingLibrarySort,
  type MarketingPickedCharacter,
  type MarketingPresetCharacter
} from "~/utils/marketingLibrary";

type ScriptAsset = components["schemas"]["ScriptAsset"];

const props = withDefaults(defineProps<{
  /** 输入框里当前的角色（打开时保留为选中态，MKT-UX-14）。 */
  selected?: MarketingPickedCharacter[];
  limits: MarketingCharacterLimits;
  readOnly?: boolean;
  /** AI 帮写用的推理模型不能用的原因；为空可用。 */
  optimizeUnavailable?: string;
  optimizeModelCode?: string;
  defaultModelCode?: string;
}>(), { selected: () => [], readOnly: false, optimizeUnavailable: "", optimizeModelCode: undefined, defaultModelCode: "" });

const emit = defineEmits<{
  close: [];
  /** 「添加」：交回选中的角色（顺序即选择顺序）。 */
  pick: [characters: MarketingPickedCharacter[]];
  /** 某个角色被删除了（输入框若正选着它就移除）。 */
  deleted: [id: string];
  /** 某个角色被编辑了（输入框若正选着它就同步名称 / 主图）。 */
  updated: [character: MarketingPickedCharacter];
}>();

const apiBase = String(useRuntimeConfig().public.apiBase);
const thumbs = useAuthenticatedMedia();
const tab = ref<"mine" | "preset">("mine");
const q = ref("");
const appliedQ = ref("");
const sort = ref<MarketingLibrarySort>("updated");
const loadError = ref("");
const presetError = ref("");
const selection = ref<MarketingPickedCharacter[]>([...props.selected]);
const notice = ref("");
const serverQuotaFull = ref(false);
const editor = ref<{ open: boolean; characterId: string }>({ open: false, characterId: "" });
const busyDelete = ref("");
const adopting = ref("");
/** 本次打开里添加过的营销角色 → 工作区副本 ID（卡片上显示「已添加」）。 */
const adopted = ref<Record<string, string>>({});
const assets = ref<ScriptAsset[]>([]);
const assetsLoading = ref(false);
const brokenPresetImages = ref(new Set<string>());
let assetSeq = 0;

const mine = useInfiniteList<MarketingCharacterSummary>({
  pageSize: MARKETING_LIBRARY_PAGE_SIZE,
  async fetchPage(_page, cursor) {
    loadError.value = "";
    try {
      return await useApi()<{ items: MarketingCharacterSummary[]; nextCursor: string | null }>("/marketing/characters", {
        query: marketingLibraryQuery({ q: appliedQ.value, sort: sort.value, cursor })
      });
    } catch (cause) {
      loadError.value = !navigator.onLine ? "网络已断开，恢复后点重试" : apiFailure(cause).message || "角色加载失败";
      return null;
    }
  },
  onPage(items) {
    const wanted = items.filter(item => item.mainMediaId).map(item => ({ key: item.mainMediaId!, url: mediaThumbUrl(item.mainMediaId, 320) }));
    if (wanted.length) void thumbs.appendAll(wanted);
  }
});

const presets = useInfiniteList<MarketingPresetCharacter>({
  pageSize: MARKETING_LIBRARY_PAGE_SIZE,
  async fetchPage(_page, cursor) {
    presetError.value = "";
    try {
      return await useApi()<{ items: MarketingPresetCharacter[]; nextCursor: string | null }>("/marketing/preset-characters", {
        query: marketingLibraryQuery({ q: appliedQ.value, cursor })
      });
    } catch (cause) {
      presetError.value = !navigator.onLine ? "网络已断开，恢复后点重试" : apiFailure(cause).message || "营销角色加载失败";
      return null;
    }
  }
});

/** 脚本角色资产（只读分组）：服务端按类型筛（B-20），不在浏览器里滤。 */
async function loadAssets() {
  const seq = ++assetSeq;
  assetsLoading.value = true;
  try {
    const list = await useApi()<ScriptAsset[]>("/script-assets", { query: { type: "character", limit: 60, ...(appliedQ.value ? { keyword: appliedQ.value } : {}) } });
    if (seq !== assetSeq) return;
    assets.value = (list ?? []).filter(item => item.type === "character");
    const wanted = assets.value.filter(item => item.mediaId).map(item => ({ key: String(item.mediaId), url: mediaThumbUrl(String(item.mediaId), 320) }));
    if (wanted.length) void thumbs.appendAll(wanted);
  } catch {
    if (seq === assetSeq) assets.value = [];
  } finally {
    if (seq === assetSeq) assetsLoading.value = false;
  }
}

const changed = computed(() => marketingCharacterPickChanged(props.selected, selection.value));
const quota = computed(() => marketingCharacterQuota({
  loaded: mine.items.value.length,
  finished: mine.finished.value,
  max: props.limits.quota,
  serverFull: serverQuotaFull.value
}));
const writeReason = computed(() => (props.readOnly ? MARKETING_CHARACTER_READ_ONLY_TEXT : quota.value.reason));
const mineFirstLoad = computed(() => mine.loading.value && mine.items.value.length === 0);
const mineEmpty = computed(() => !mine.items.value.length && !assets.value.length && !mine.loading.value && !assetsLoading.value);
const selectedKeys = computed(() => new Set(selection.value.map(marketingPickKey)));

let presetsLoaded = false;
let searchTimer: ReturnType<typeof setTimeout> | undefined;
watch(q, value => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    if (value.trim() === appliedQ.value) return;
    appliedQ.value = value.trim();
    void mine.reset();
    void loadAssets();
    if (presetsLoaded) void presets.reset();
  }, MARKETING_SEARCH_DEBOUNCE_MS);
});
watch(sort, () => void mine.reset());

watch(tab, value => {
  notice.value = "";
  if (value === "preset" && !presetsLoaded) {
    presetsLoaded = true;
    void presets.reset();
  }
});

function pickedOfSummary(item: Pick<MarketingCharacterSummary, "id" | "name" | "mainMediaId">): MarketingPickedCharacter {
  return { kind: "marketing_character", id: item.id, name: item.name, mainMediaId: item.mainMediaId };
}

function pickedOfAsset(item: ScriptAsset): MarketingPickedCharacter {
  return { kind: "asset", id: String(item.id), name: item.name, mainMediaId: item.mediaId ? String(item.mediaId) : null };
}

function toggle(item: MarketingPickedCharacter) {
  const result = marketingToggleCharacter(selection.value, item, props.limits.maxRefs);
  selection.value = result.selected;
  notice.value = result.notice;
}

function isSelected(item: Pick<MarketingPickedCharacter, "kind" | "id">) {
  return selectedKeys.value.has(marketingPickKey(item));
}

function confirm() {
  if (!changed.value) return;
  emit("pick", selection.value);
}

// ---------------------------------------------------------------- 营销角色：添加（复制进「我的」）

async function adopt(preset: MarketingPresetCharacter) {
  if (writeReason.value || adopting.value) return;
  adopting.value = preset.id;
  notice.value = "";
  try {
    const character = await useApi()<MarketingCharacter>(`/marketing/preset-characters/${encodeURIComponent(preset.id)}/adopt`, { method: "POST" });
    const already = mine.items.value.some(item => item.id === character.id) || Object.values(adopted.value).includes(character.id);
    adopted.value = { ...adopted.value, [preset.id]: character.id };
    if (!mine.items.value.some(item => item.id === character.id)) {
      mine.items.value = [{
        id: character.id, name: character.name, imageCount: character.imageCount, mainMediaId: character.mainMediaId, hasVoice: Boolean(character.voiceMediaId),
        source: character.source, version: character.version, createdAt: character.createdAt, updatedAt: character.updatedAt
      }, ...mine.items.value];
      if (character.mainMediaId) void thumbs.appendAll([{ key: character.mainMediaId, url: mediaThumbUrl(character.mainMediaId, 320) }]);
    }
    const picked = pickedOfSummary(character);
    if (!isSelected(picked)) toggle(picked);
    if (!notice.value) {
      notice.value = already
        ? `「${character.name}」已在「我的」里，没有重复添加`
        : `已添加到「我的」：${character.name}`;
    }
  } catch (cause) {
    const failure = apiFailure(cause);
    if (marketingIsQuotaError(failure)) serverQuotaFull.value = true;
    ElMessage.error(!navigator.onLine ? "网络已断开，恢复后再试" : failure.message);
  } finally {
    adopting.value = "";
  }
}

function onPresetImageError(id: string) {
  brokenPresetImages.value = new Set([...brokenPresetImages.value, id]);
}

// ---------------------------------------------------------------- 新增 / 编辑 / 删除

function openEditor(characterId = "") {
  if (props.readOnly) return;
  if (!characterId && quota.value.full) return;
  editor.value = { open: true, characterId };
}

function onSaved(character: MarketingCharacter, created: boolean) {
  editor.value = { open: false, characterId: "" };
  const picked = pickedOfSummary(character);
  if (character.mainMediaId) void thumbs.appendAll([{ key: character.mainMediaId, url: mediaThumbUrl(character.mainMediaId, 320) }]);
  if (created) {
    // 新建的那一个回到「我的」并自动选中（MKT-UX-14）；满了只提示不挤掉已选的。
    tab.value = "mine";
    if (!isSelected(picked)) toggle(picked);
    void mine.reset();
    return;
  }
  mine.items.value = mine.items.value.map(item => (item.id === character.id
    ? { ...item, name: character.name, imageCount: character.imageCount, mainMediaId: character.mainMediaId, hasVoice: Boolean(character.voiceMediaId), version: character.version, updatedAt: character.updatedAt }
    : item));
  selection.value = selection.value.map(item => (item.kind === "marketing_character" && item.id === character.id ? picked : item));
  emit("updated", picked);
}

async function remove(item: MarketingCharacterSummary) {
  if (props.readOnly || busyDelete.value) return;
  const text = marketingCharacterDeleteConfirm(item.name);
  try {
    await ElMessageBox.confirm(text.message, text.title, { confirmButtonText: "删除", cancelButtonText: "取消", type: "warning" });
  } catch {
    return;
  }
  busyDelete.value = item.id;
  const drop = () => {
    mine.items.value = mine.items.value.filter(entry => entry.id !== item.id);
    selection.value = selection.value.filter(entry => !(entry.kind === "marketing_character" && entry.id === item.id));
    adopted.value = Object.fromEntries(Object.entries(adopted.value).filter(([, id]) => id !== item.id));
    serverQuotaFull.value = false;
    emit("deleted", item.id);
  };
  try {
    await useApi()(`/marketing/characters/${encodeURIComponent(item.id)}`, { method: "DELETE" });
    drop();
  } catch (cause) {
    // 已经被别人删掉了：结果与想要的一致。
    if ((cause as { statusCode?: number }).statusCode === 404) drop();
    else ElMessage.error(!navigator.onLine ? "网络已断开，恢复后再试" : apiFailure(cause).message);
  } finally {
    busyDelete.value = "";
  }
}

// ---------------------------------------------------------------- 关闭

function onKeydown(event: KeyboardEvent) {
  // 编辑弹窗开着时 Esc 归它（它自己会确认是否放弃修改）。
  if (event.key !== "Escape" || editor.value.open) return;
  event.stopPropagation();
  emit("close");
}

onMounted(() => {
  window.addEventListener("keydown", onKeydown, true);
  void mine.reset();
  void loadAssets();
  const wanted = props.selected.filter(item => item.mainMediaId).map(item => ({ key: item.mainMediaId!, url: mediaThumbUrl(item.mainMediaId, 320) }));
  if (wanted.length) void thumbs.appendAll(wanted);
});
onBeforeUnmount(() => {
  window.removeEventListener("keydown", onKeydown, true);
  clearTimeout(searchTimer);
});
</script>

<template>
  <Teleport to="body">
    <div class="portal-overlay character-picker-overlay" @click.self="emit('close')">
      <section class="character-picker-dialog" role="dialog" aria-modal="true" aria-label="选择角色">
        <header class="cp-head">
          <h2>角色</h2>
          <label class="cp-search">
            <Search :size="14" />
            <input v-model="q" type="search" placeholder="搜索角色" aria-label="按名称搜索角色">
          </label>
          <button type="button" class="cp-close" title="关闭（Esc）" aria-label="关闭" @click="emit('close')"><X :size="15" /></button>
        </header>
        <div class="cp-tools">
          <div class="cp-tabs" role="tablist">
            <button type="button" role="tab" class="cp-tab" :class="{ on: tab === 'mine' }" :aria-selected="tab === 'mine'" @click="tab = 'mine'">我的</button>
            <button type="button" role="tab" class="cp-tab" :class="{ on: tab === 'preset' }" :aria-selected="tab === 'preset'" @click="tab = 'preset'">营销角色</button>
          </div>
          <span v-if="writeReason" class="cp-note">{{ writeReason }}</span>
          <select v-if="tab === 'mine'" v-model="sort" class="cp-pill" aria-label="排序">
            <option v-for="option in MARKETING_LIBRARY_SORTS" :key="option.value" :value="option.value">{{ option.label }}</option>
          </select>
          <i class="cp-sep" aria-hidden="true" />
          <button type="button" class="cp-pill" :disabled="Boolean(writeReason)" :title="writeReason || '新建角色'" @click="openEditor()">
            <Plus :size="13" />新增
          </button>
        </div>

        <div class="cp-body">
          <!-- 我的 -->
          <template v-if="tab === 'mine'">
            <div v-if="mineFirstLoad" class="cp-empty"><LoaderCircle :size="18" class="spin" />加载中…</div>
            <div v-else-if="loadError && !mine.items.value.length && !assets.length" class="cp-empty">
              <span>{{ loadError }}</span>
              <button type="button" class="cp-link" @click="mine.reset()">重试</button>
            </div>
            <div v-else-if="mineEmpty" class="cp-empty">{{ marketingCharacterEmptyText("mine", appliedQ) }}</div>
            <template v-else>
              <ul v-if="mine.items.value.length" class="cp-grid">
                <li v-for="item in mine.items.value" :key="item.id" class="cp-item" :class="{ on: isSelected({ kind: 'marketing_character', id: item.id }) }">
                  <button type="button" class="cp-card" :aria-pressed="isSelected({ kind: 'marketing_character', id: item.id })" :title="item.name" @click="toggle(pickedOfSummary(item))">
                    <span class="cp-thumb">
                      <img v-if="item.mainMediaId && thumbs.urls.value[item.mainMediaId]" :src="thumbs.urls.value[item.mainMediaId]" :alt="item.name" draggable="false">
                      <UserRound v-else :size="28" class="cp-ph" />
                      <span v-if="isSelected({ kind: 'marketing_character', id: item.id })" class="cp-check"><Check :size="13" /></span>
                      <span v-if="marketingCharacterSourceLabel(item.source)" class="cp-badge">{{ marketingCharacterSourceLabel(item.source) }}</span>
                    </span>
                    <strong>{{ item.name }}</strong>
                  </button>
                  <span class="cp-actions">
                    <button type="button" :disabled="readOnly" :title="readOnly ? MARKETING_CHARACTER_READ_ONLY_TEXT : '编辑'" :aria-label="`编辑「${item.name}」`" @click="openEditor(item.id)"><Pencil :size="12" /></button>
                    <button type="button" :disabled="readOnly || busyDelete === item.id" :title="readOnly ? MARKETING_CHARACTER_READ_ONLY_TEXT : '删除'" :aria-label="`删除「${item.name}」`" @click="remove(item)"><Trash2 :size="12" /></button>
                  </span>
                </li>
                <li v-if="!mine.finished.value" class="cp-sentinel">
                  <span :ref="el => { mine.sentinel.value = (el as HTMLElement) || undefined }"><LoaderCircle v-if="mine.loading.value" :size="14" class="spin" /></span>
                </li>
              </ul>
              <p v-if="loadError && mine.items.value.length" class="cp-note warn">{{ loadError }} <button type="button" class="cp-link" @click="mine.loadNext()">重试</button></p>
              <section v-if="assets.length" class="cp-group">
                <h3>脚本角色资产<small>来自分镜脚本，只能挑选，不能在这里编辑</small></h3>
                <ul class="cp-grid">
                  <li v-for="asset in assets" :key="asset.id" class="cp-item" :class="{ on: isSelected({ kind: 'asset', id: String(asset.id) }) }">
                    <button type="button" class="cp-card" :aria-pressed="isSelected({ kind: 'asset', id: String(asset.id) })" :title="asset.name" @click="toggle(pickedOfAsset(asset))">
                      <span class="cp-thumb">
                        <img v-if="asset.mediaId && thumbs.urls.value[String(asset.mediaId)]" :src="thumbs.urls.value[String(asset.mediaId)]" :alt="asset.name" draggable="false">
                        <UserRound v-else :size="28" class="cp-ph" />
                        <span v-if="isSelected({ kind: 'asset', id: String(asset.id) })" class="cp-check"><Check :size="13" /></span>
                      </span>
                      <strong>{{ asset.name }}</strong>
                    </button>
                  </li>
                </ul>
              </section>
            </template>
          </template>

          <!-- 营销角色 -->
          <template v-else>
            <div v-if="presets.loading.value && !presets.items.value.length" class="cp-empty"><LoaderCircle :size="18" class="spin" />加载中…</div>
            <div v-else-if="presetError && !presets.items.value.length" class="cp-empty">
              <span>{{ presetError }}</span>
              <button type="button" class="cp-link" @click="presets.reset()">重试</button>
            </div>
            <div v-else-if="!presets.items.value.length" class="cp-empty">{{ marketingCharacterEmptyText("preset", appliedQ) }}</div>
            <ul v-else class="cp-grid">
              <li v-for="preset in presets.items.value" :key="preset.id" class="cp-item">
                <div class="cp-card preset">
                  <span class="cp-thumb">
                    <img
                      v-if="preset.imageCount > 0 && !brokenPresetImages.has(preset.id)"
                      :src="marketingPresetImageUrl(apiBase, preset.id, 0, 320)"
                      :alt="preset.name"
                      loading="lazy"
                      draggable="false"
                      @error="onPresetImageError(preset.id)"
                    >
                    <UserRound v-else :size="28" class="cp-ph" />
                    <span v-if="adopted[preset.id] && isSelected({ kind: 'marketing_character', id: adopted[preset.id]! })" class="cp-check"><Check :size="13" /></span>
                  </span>
                  <strong>{{ preset.name }}</strong>
                  <small :title="preset.summary">{{ preset.summary }}</small>
                  <button
                    type="button"
                    class="cp-adopt"
                    :disabled="Boolean(writeReason) || adopting === preset.id"
                    :title="writeReason || '复制一份到「我的」并选中'"
                    @click="adopt(preset)"
                  >
                    <LoaderCircle v-if="adopting === preset.id" :size="12" class="spin" />
                    <template v-else>{{ adopted[preset.id] ? "已添加" : "添加" }}</template>
                  </button>
                </div>
              </li>
              <li v-if="!presets.finished.value" class="cp-sentinel">
                <span :ref="el => { presets.sentinel.value = (el as HTMLElement) || undefined }"><LoaderCircle v-if="presets.loading.value" :size="14" class="spin" /></span>
              </li>
            </ul>
          </template>
        </div>

        <footer class="cp-foot">
          <span class="cp-picked">已选 {{ selection.length }} / {{ limits.maxRefs }}<template v-if="selection.length">：{{ selection.map(item => item.name).join("、") }}</template></span>
          <span v-if="notice" class="cp-notice" role="status">{{ notice }}</span>
          <button type="button" class="cp-btn" @click="emit('close')">取消</button>
          <button type="button" class="cp-btn primary" :disabled="!changed" :title="changed ? '' : '选择没有变化'" @click="confirm">添加</button>
        </footer>
      </section>
    </div>

    <CharacterEditorDialog
      v-if="editor.open"
      :character-id="editor.characterId"
      :limits="limits"
      :read-only="readOnly"
      :optimize-unavailable="optimizeUnavailable"
      :optimize-model-code="optimizeModelCode"
      :default-model-code="defaultModelCode"
      @close="editor = { open: false, characterId: '' }"
      @saved="onSaved"
      @quota-full="serverQuotaFull = true"
    />
  </Teleport>
</template>

<style scoped>
.character-picker-dialog {
  width: min(960px, 100%);
  height: min(740px, calc(100vh - 48px));
  display: grid;
  grid-template-rows: auto auto minmax(0, 1fr) auto;
  gap: 12px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 20px;
  padding: 20px 22px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 28px 70px var(--portal-shadow-strong);
}
.cp-head { display: flex; align-items: center; gap: 12px; }
.cp-head h2 { margin: 0 auto 0 0; font-size: 16px; }
.cp-search {
  width: min(280px, 50%);
  height: 34px;
  display: flex;
  align-items: center;
  gap: 6px;
  border-radius: 999px;
  padding: 0 12px;
  color: var(--portal-muted);
  background: var(--portal-surface-2);
}
.cp-search input { min-width: 0; flex: 1; border: 0; color: var(--portal-text); background: transparent; font: inherit; font-size: 13px; outline: none; }
.cp-search input::placeholder { color: var(--portal-placeholder); }
.cp-close {
  width: 28px;
  height: 28px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 50%;
  color: var(--portal-muted);
  background: var(--portal-surface-2);
  cursor: pointer;
}
.cp-close:hover { color: var(--portal-text); background: var(--portal-hover); }
.cp-tools { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; }
.cp-tabs { display: flex; gap: 8px; margin-right: auto; }
.cp-tab {
  height: 32px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 999px;
  padding: 0 14px;
  color: var(--portal-text-soft);
  background: transparent;
  font: inherit;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}
.cp-tab.on { border-color: transparent; color: var(--portal-text); background: var(--portal-surface-3, var(--portal-hover)); }
.cp-note { color: var(--portal-muted); font-size: 12px; }
.cp-note.warn { margin: 8px 0 0; color: var(--portal-danger); }
.cp-sep { width: 1px; height: 18px; background: var(--portal-line); }
.cp-pill {
  height: 32px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 999px;
  padding: 0 14px;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}
select.cp-pill { padding-right: 10px; }
.cp-pill:hover:not(:disabled) { background: var(--portal-hover); }
.cp-pill:disabled { cursor: not-allowed; opacity: .5; }
.cp-body { min-height: 0; overflow-y: auto; }
.cp-empty {
  height: 100%;
  min-height: 200px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  color: var(--portal-muted);
  font-size: 13px;
}
.cp-link { border: 0; padding: 0; color: var(--portal-cyan); background: transparent; font: inherit; cursor: pointer; }
.cp-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(140px, 1fr));
  gap: 14px;
  margin: 0;
  padding: 2px;
  list-style: none;
}
.cp-group { margin-top: 18px; }
.cp-group h3 { display: flex; align-items: baseline; gap: 8px; margin: 0 0 10px; font-size: 13px; }
.cp-group h3 small { color: var(--portal-muted); font-size: 11px; font-weight: 400; }
.cp-item { position: relative; min-width: 0; }
.cp-card {
  width: 100%;
  display: grid;
  gap: 4px;
  border: 0;
  padding: 0;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.cp-card.preset { cursor: default; }
.cp-thumb {
  position: relative;
  aspect-ratio: 1;
  display: grid;
  place-items: center;
  overflow: hidden;
  border: 2px solid transparent;
  border-radius: 12px;
  background: var(--portal-surface-2);
}
.cp-item.on .cp-thumb { border-color: var(--portal-cyan); }
.cp-card:not(.preset):hover .cp-thumb { background: var(--portal-hover); }
.cp-thumb img { width: 100%; height: 100%; object-fit: cover; }
.cp-ph { color: var(--portal-faint); }
.cp-check {
  position: absolute;
  top: 6px;
  left: 6px;
  width: 20px;
  height: 20px;
  display: grid;
  place-items: center;
  border-radius: 50%;
  color: var(--portal-on-accent);
  background: var(--portal-cyan);
}
.cp-badge {
  position: absolute;
  bottom: 6px;
  left: 6px;
  border-radius: 4px;
  padding: 0 5px;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  font-size: 10px;
  line-height: 16px;
}
.cp-card strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12px; font-weight: 600; }
.cp-card small { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--portal-muted); font-size: 11px; }
.cp-adopt {
  height: 28px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border: 1px solid var(--portal-line-strong);
  border-radius: 999px;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}
.cp-adopt:hover:not(:disabled) { background: var(--portal-hover); }
.cp-adopt:disabled { cursor: not-allowed; opacity: .5; }
.cp-actions { position: absolute; top: 6px; right: 6px; display: flex; gap: 4px; opacity: 0; transition: opacity .12s; }
.cp-item:hover .cp-actions,
.cp-item:focus-within .cp-actions { opacity: 1; }
@media (hover: none) { .cp-actions { opacity: 1; } }
.cp-actions button {
  width: 24px;
  height: 24px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 6px;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  cursor: pointer;
}
.cp-actions button:hover:not(:disabled) { background: var(--portal-on-media-bg-hover); }
.cp-actions button:disabled { cursor: not-allowed; opacity: .5; }
.cp-sentinel { grid-column: 1 / -1; display: flex; justify-content: center; min-height: 20px; color: var(--portal-muted); }
.cp-foot { display: flex; flex-wrap: wrap; align-items: center; justify-content: flex-end; gap: 10px; }
.cp-picked { margin-right: auto; overflow: hidden; color: var(--portal-muted); font-size: 12px; text-overflow: ellipsis; white-space: nowrap; }
.cp-notice { color: var(--portal-gold); font-size: 12px; }
.cp-btn {
  min-width: 72px;
  height: 34px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 999px;
  padding: 0 16px;
  color: var(--portal-text);
  background: transparent;
  font: inherit;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
}
.cp-btn:hover:not(:disabled) { background: var(--portal-hover); }
.cp-btn.primary { border-color: transparent; color: var(--portal-mark-text); background: var(--portal-mark-bg); }
.cp-btn.primary:hover:not(:disabled) { opacity: .9; }
.cp-btn:disabled { cursor: not-allowed; color: var(--portal-disabled-text); background: var(--portal-disabled-bg); border-color: transparent; }
.spin { animation: cp-spin 1s linear infinite; }
@keyframes cp-spin { to { transform: rotate(360deg); } }
@media (max-width: 639px) {
  .character-picker-dialog { padding: 16px; border-radius: 16px; }
  .cp-grid { grid-template-columns: repeat(auto-fill, minmax(104px, 1fr)); gap: 10px; }
  .cp-search { width: auto; flex: 1; }
}
</style>
