<script setup lang="ts">
/**
 * 营销输入框左侧的槽位卡片（MKT-T-12 → MKT-T-38 改为方形卡片；营销 dev/05 §4.4、MKD-38；
 * MKT-F-90/91、MKT-UX-13/16，对标截图 57）。
 *
 * 按当前槽位声明（技能的 `inputSlots`，未选技能用 `/marketing/config` 的 `slots`）渲染等大并排的方形卡片：
 * - **商品**：未选 = 标题（必填带 *）+ `＋`；已选 = 主图 + 名称 + 「共 N 张」+ `×`。点卡片 emit `open-picker("product")`，
 *   商品选择弹窗由输入框挂（同选项弹层的分工，dev/03 R-6 #5）。
 * - **角色**：已选 = ≤ 2 个头像 + 名称 + `×`。点卡片 emit `open-picker("character")`，由输入框打开角色选择弹窗
 *   （`CharacterPickerDialog`，MKT-T-39）。`openAssetPicker("character")` 保留但输入框已不再调用。
 * - **源视频**：形态统一，选择方式不变 —— 上传，或 `@` 本工作区上传过 / 生成过的视频。
 *   引用别人的视频时服务端回 40001 `details.slot=source_video`，输入框给这一张卡描边标红（`AgentModeComposer`）。
 *
 * 规则全在 `utils/marketingSlots.ts`（有单测），这里只管点击、上传与缩略图。
 *
 * ⚠️ 只在用户操作时上传（选文件 / 点重试），挂载时不发任何写请求。
 * ⚠️ 缩略图：刚选的源视频文件不出本地预览（视频）；只有媒体 ID 的（商品主图、角色资产、恢复的草稿）走
 *    `useAuthenticatedMedia`（160 档，卸载即 revoke），绝不 `<img src="/media/...">` 直链（401）。
 * ⚠️ 只读成员也能点开商品 / 角色弹窗浏览（MKT-UX-17）—— `disabled` 只禁用 `×` 与上传。
 * ⚠️ 上限来自配置（`maxProductsPerSession` / `maxCharacterRefs`），不写死。
 */
import { ElMessage } from "element-plus";
import { AtSign, LoaderCircle, Plus, RotateCw, TriangleAlert, UserRound, Video, X } from "@lucide/vue";
import MentionPicker from "../MentionPicker.vue";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import type { ComposerRef } from "~/utils/composerRefs";
import { SLOT_NOUN, type MarketingComposerState, type SlotDecl, type SlotKey } from "~/utils/marketingComposer";
import { marketingProductCountText } from "~/utils/marketingLibrary";
import {
  SLOT_KEYS,
  addSlotItems,
  clearSlotItems,
  emptySlotsValue,
  orphanPreviewIds,
  patchSlotItem,
  removeSlotItem,
  slotAcceptAttr,
  slotAcceptsFile,
  slotAddGate,
  slotFilled,
  slotLimit,
  slotsUploading,
  type MarketingSlotLimits,
  type MarketingSlotsValue,
  type SlotItem
} from "~/utils/marketingSlots";
import { referenceMediaType } from "~/utils/referenceUpload";

const props = withDefaults(defineProps<{
  /** 当前槽位声明。 */
  decls: SlotDecl[];
  /** `/marketing/config` 的 `maxProductsPerSession` / `maxCharacterRefs`。 */
  limits?: MarketingSlotLimits | null;
  disabled?: boolean;
  projectId?: string;
}>(), { limits: null, disabled: false, projectId: "" });

const model = defineModel<MarketingSlotsValue>({ default: () => emptySlotsValue() });
const emit = defineEmits<{
  /** 各槽条数（`MarketingComposerState.filled`）。 */
  filled: [filled: MarketingComposerState["filled"]];
  uploading: [uploading: boolean];
  /** 点了商品 / 角色卡：由输入框打开对应的选择弹窗。 */
  "open-picker": [slot: SlotKey];
}>();

const THUMB = 160;

const referenceUpload = useReferenceUpload();
const thumbs = useAuthenticatedMedia();
/** localId → 文件（重试要用；不进 v-model，草稿里存不下 File）。 */
const files = new Map<string, File>();
const fileInputs = ref<Partial<Record<SlotKey, HTMLInputElement>>>({});
const picker = ref<SlotKey | null>(null);

const shownDecls = computed(() => SLOT_KEYS
  .map(key => props.decls.find(decl => decl.key === key))
  .filter((decl): decl is SlotDecl => Boolean(decl)));

function limitOf(decl: SlotDecl): number {
  return slotLimit(decl, props.limits);
}

function gateOf(decl: SlotDecl) {
  return slotAddGate(model.value.items[decl.key], limitOf(decl), decl.key);
}

/**
 * 最近一次写出去的值。⚠️ 父组件绑定 v-model 时，`model.value = x` 之后**同一个 tick 里读 `model.value` 还是旧值**
 * （要等父组件把新 prop 传回来）。连着写两次（加项 → 标上传中）的话第二次会在旧值上改，把刚加的整批抹掉。
 * 所以写入一律基于它，外部改值（如换技能清槽、弹窗选商品）由下面的 watch 同步回来。
 */
let latest: MarketingSlotsValue = model.value;

function commit(next: MarketingSlotsValue) {
  latest = next;
  model.value = next;
}

function setItems(key: SlotKey, change: (items: SlotItem[]) => SlotItem[]) {
  // 上传是异步的，期间用户可能删了别的项：每次都在最新的值上改。
  commit({ ...latest, items: { ...latest.items, [key]: change(latest.items[key]) } });
}

function localId(): string {
  return `s_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 8)}`;
}

// ---------------------------------------------------------------- 卡片点击

function openCard(key: SlotKey) {
  picker.value = null;
  emit("open-picker", key);
}

// ---------------------------------------------------------------- 上传（源视频）

function pickFiles(key: SlotKey) {
  if (props.disabled) return;
  picker.value = null;
  fileInputs.value[key]?.click();
}

function onFiles(decl: SlotDecl, event: Event) {
  const input = event.target as HTMLInputElement;
  const picked = Array.from(input.files ?? []);
  input.value = "";
  const accepted: Array<{ item: SlotItem; file: File }> = [];
  for (const file of picked) {
    if (!slotAcceptsFile(decl, file.type)) {
      ElMessage.error(`${file.name}：${SLOT_NOUN[decl.key]}只能是${decl.accept.includes("video") ? "视频" : "图片"}`);
      continue;
    }
    const hint = referenceMediaType(file) ?? "video";
    accepted.push({ item: { localId: localId(), kind: "media", status: "uploading", label: file.name, hint }, file });
  }
  if (!accepted.length) return;
  const result = addSlotItems(latest.items[decl.key], accepted.map(entry => entry.item), limitOf(decl), decl.key);
  if (result.notice) ElMessage.warning(result.notice);
  const added = new Set(result.items.map(item => item.localId));
  setItems(decl.key, () => result.items);
  for (const { item, file } of accepted) {
    if (!added.has(item.localId)) continue;
    files.set(item.localId, file);
    void uploadOne(decl.key, item.localId);
  }
}

async function uploadOne(key: SlotKey, id: string) {
  const file = files.get(id);
  if (!file) return;
  setItems(key, items => patchSlotItem(items, id, { status: "uploading", error: undefined }));
  try {
    const mediaId = await referenceUpload.upload(file, { mediaType: referenceMediaType(file), projectId: props.projectId || undefined });
    setItems(key, items => patchSlotItem(items, id, { status: "done", id: mediaId, error: undefined }));
  } catch (cause) {
    const message = typeof navigator !== "undefined" && !navigator.onLine
      ? "网络已断开，恢复后点重试"
      : (cause as { message?: string })?.message || "上传没有成功，请重试";
    setItems(key, items => patchSlotItem(items, id, { status: "failed", error: message }));
  }
}

function retry(key: SlotKey, item: SlotItem) {
  if (props.disabled) return;
  if (!files.has(item.localId)) {
    // 文件已经不在了（例如恢复出来的失败项）：只能重新选。
    remove(key, item);
    ElMessage.warning("请重新添加这个文件");
    return;
  }
  void uploadOne(key, item.localId);
}

function remove(key: SlotKey, item: SlotItem) {
  if (props.disabled) return;
  setItems(key, items => removeSlotItem(items, item.localId));
}

// ---------------------------------------------------------------- @ 引用（源视频；角色在 MKT-T-39 之前的临时入口）

function openAssetPicker(key: SlotKey) {
  if (props.disabled) return;
  const decl = props.decls.find(item => item.key === key);
  if (!decl) return;
  const gate = gateOf(decl);
  if (!gate.ok) {
    ElMessage.warning(gate.reason);
    return;
  }
  picker.value = picker.value === key ? null : key;
}

function onPick(decl: SlotDecl, ref: Omit<ComposerRef, "auto">) {
  picker.value = null;
  if (ref.kind !== "asset" && ref.kind !== "media") return;
  const item: SlotItem = {
    localId: localId(),
    kind: ref.kind,
    id: ref.id,
    status: "done",
    label: ref.label,
    thumbMediaId: ref.thumbMediaId,
    hint: ref.hint
  };
  const result = addSlotItems(latest.items[decl.key], [item], limitOf(decl), decl.key);
  if (result.notice) ElMessage.warning(result.notice);
  setItems(decl.key, () => result.items);
}

// ---------------------------------------------------------------- 缩略图与释放

function thumbOf(item: SlotItem): string | undefined {
  return item.thumbMediaId && item.hint !== "video" ? thumbs.urls.value[item.thumbMediaId] : undefined;
}

watch(model, value => {
  latest = value;
  // 删除 / 清空 / 被外部替换的项：丢掉它留着的重试文件。
  for (const id of orphanPreviewIds(files.keys(), value)) files.delete(id);
  const wanted = SLOT_KEYS.flatMap(key => value.items[key])
    .filter(item => item.thumbMediaId && item.hint !== "video")
    .map(item => ({ key: item.thumbMediaId!, url: mediaThumbUrl(item.thumbMediaId, THUMB) }));
  if (wanted.length) void thumbs.appendAll(wanted);
  emit("filled", slotFilled(value));
  emit("uploading", slotsUploading(value));
}, { immediate: true });

onBeforeUnmount(() => files.clear());

/** 清空若干槽（换技能确认之后）。 */
function clear(keys: readonly SlotKey[]) {
  commit(clearSlotItems(latest, keys).value);
}

defineExpose({ clear, openAssetPicker });
</script>

<template>
  <div class="marketing-slots" :class="[{ disabled }, `count-${shownDecls.length}`]" aria-label="输入素材">
    <section
      v-for="decl in shownDecls"
      :key="decl.key"
      class="slot-card"
      :class="[`slot-${decl.key}`, { filled: model.items[decl.key].length > 0 }]"
    >
      <input
        :ref="el => { fileInputs[decl.key] = (el as HTMLInputElement) || undefined }"
        type="file"
        class="slot-file"
        :accept="slotAcceptAttr(decl)"
        tabindex="-1"
        @change="onFiles(decl, $event)"
      >

      <!-- 商品：一个 -->
      <template v-if="decl.key === 'product'">
        <button
          type="button"
          class="slot-hit"
          :aria-label="model.items.product[0] ? `更换商品（当前：${model.items.product[0].label}）` : `选择${decl.label || '商品'}`"
          :title="model.items.product[0] ? '点击更换商品' : '从商品库选择'"
          @click="openCard('product')"
        >
          <template v-if="model.items.product[0]">
            <img v-if="thumbOf(model.items.product[0])" class="slot-cover" :src="thumbOf(model.items.product[0])" :alt="model.items.product[0].label" draggable="false">
            <span v-else class="slot-cover blank" aria-hidden="true" />
            <span class="slot-caption">
              <strong>{{ model.items.product[0].label }}</strong>
              <small v-if="typeof model.items.product[0].imageCount === 'number'">{{ marketingProductCountText(model.items.product[0].imageCount) }}</small>
            </span>
          </template>
          <Plus v-else :size="18" class="slot-plus" />
        </button>
        <span class="slot-title" :class="{ 'on-media': model.items.product[0] }">{{ decl.label || SLOT_NOUN.product }}<em v-if="decl.required" title="必填">*</em></span>
        <button
          v-if="model.items.product[0]"
          type="button"
          class="slot-remove"
          title="移除商品"
          aria-label="移除商品"
          :disabled="disabled"
          @click="remove('product', model.items.product[0])"
        ><X :size="12" /></button>
      </template>

      <!-- 角色：≤ N 个 -->
      <template v-else-if="decl.key === 'character'">
        <button
          type="button"
          class="slot-hit"
          :aria-label="`选择${decl.label || '角色'}`"
          :title="gateOf(decl).ok ? '选择角色' : gateOf(decl).reason"
          @click="openCard('character')"
        >
          <template v-if="model.items.character.length">
            <span class="slot-avatars">
              <span v-for="item in model.items.character" :key="item.localId" class="slot-avatar" :title="item.label">
                <img v-if="thumbOf(item)" :src="thumbOf(item)" :alt="item.label" draggable="false">
                <UserRound v-else :size="16" />
              </span>
            </span>
            <span class="slot-names">{{ model.items.character.map(item => item.label).join("、") }}</span>
          </template>
          <Plus v-else :size="18" class="slot-plus" />
        </button>
        <span class="slot-title">{{ decl.label || SLOT_NOUN.character }}<em v-if="decl.required" title="必填">*</em></span>
        <span v-if="model.items.character.length" class="slot-chips">
          <button
            v-for="item in model.items.character"
            :key="item.localId"
            type="button"
            class="slot-chip-remove"
            :title="`移除角色「${item.label}」`"
            :aria-label="`移除角色「${item.label}」`"
            :disabled="disabled"
            @click="remove('character', item)"
          ><X :size="10" /></button>
        </span>
        <MentionPicker
          v-if="picker === 'character'"
          class="slot-picker"
          only="character"
          @select="onPick(decl, $event)"
          @close="picker = null"
        />
      </template>

      <!-- 源视频：上传或 @（选择方式不变） -->
      <template v-else>
        <span class="slot-title">{{ decl.label || SLOT_NOUN.source_video }}<em v-if="decl.required" title="必填">*</em></span>
        <div v-if="model.items[decl.key][0]" class="slot-video" :class="model.items[decl.key][0]!.status" :title="model.items[decl.key][0]!.error || model.items[decl.key][0]!.label">
          <img v-if="thumbOf(model.items[decl.key][0]!)" class="slot-cover" :src="thumbOf(model.items[decl.key][0]!)" :alt="model.items[decl.key][0]!.label">
          <Video v-else :size="20" class="slot-icon" />
          <span class="slot-names">{{ model.items[decl.key][0]!.label }}</span>
          <span v-if="model.items[decl.key][0]!.status === 'uploading'" class="slot-overlay" aria-label="上传中"><LoaderCircle :size="16" class="spin" /></span>
          <button
            v-else-if="model.items[decl.key][0]!.status === 'failed'"
            type="button"
            class="slot-overlay failed"
            title="重新上传"
            :disabled="disabled"
            @click="retry(decl.key, model.items[decl.key][0]!)"
          >
            <TriangleAlert :size="13" /><RotateCw :size="13" />
          </button>
          <button type="button" class="slot-remove" title="移除" aria-label="移除源视频" :disabled="disabled" @click="remove(decl.key, model.items[decl.key][0]!)"><X :size="12" /></button>
        </div>
        <div v-else class="slot-video-actions">
          <button type="button" class="slot-mini" :disabled="disabled" title="上传视频" aria-label="上传视频" @click="pickFiles(decl.key)"><Plus :size="15" /></button>
          <button
            type="button"
            class="slot-mini"
            :disabled="disabled"
            title="引用本工作区上传或生成过的视频"
            aria-label="选已有视频"
            :aria-expanded="picker === decl.key"
            @click="openAssetPicker(decl.key)"
          ><AtSign :size="14" /></button>
        </div>
        <MentionPicker
          v-if="picker === decl.key"
          class="slot-picker"
          only="video"
          @select="onPick(decl, $event)"
          @close="picker = null"
        />
      </template>
    </section>
  </div>
</template>

<style scoped>
/*
 * 方形卡片等大并排（MKT-UX-13）：两张 96px，三张时缩到 84px；窄屏（输入框 < 640px，槽位移到正文上方）同样横排。
 * 尺寸只由卡片自身决定，不随输入框变宽而拉伸 —— 对标截图 57。
 */
.marketing-slots {
  --slot-size: 96px;
  display: flex;
  flex-wrap: nowrap;
  gap: 10px;
  color: var(--portal-text);
  font-size: 12px;
}
.marketing-slots.count-3 { --slot-size: 84px; gap: 8px; }
.slot-card {
  position: relative;
  width: var(--slot-size);
  height: var(--slot-size);
  flex: none;
  border: 1px solid var(--portal-line);
  border-radius: 12px;
  background: var(--portal-surface-2);
  transition: border-color .12s, background .12s;
}
.slot-card:hover { border-color: var(--portal-line-strong); }
.slot-file { display: none; }
.slot-hit {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 4px;
  border: 0;
  border-radius: 11px;
  padding: 22px 6px 6px;
  overflow: hidden;
  color: var(--portal-muted);
  background: transparent;
  font: inherit;
  cursor: pointer;
}
.slot-hit:hover { color: var(--portal-text); background: var(--portal-hover); }
.slot-hit:focus-visible { outline: 2px solid var(--portal-muted); outline-offset: 1px; }
.slot-plus { color: var(--portal-text-soft); }
.slot-title {
  position: absolute;
  top: 7px;
  left: 9px;
  z-index: 1;
  font-size: 11px;
  font-weight: 600;
  color: var(--portal-text);
  pointer-events: none;
}
.slot-title em { margin-left: 2px; color: var(--portal-danger); font-style: normal; }
.slot-title.on-media {
  border-radius: 4px;
  padding: 0 4px;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
}
/* 已选商品：主图铺满，名称与张数压在底部。 */
.slot-product.filled .slot-hit { padding: 0; justify-content: flex-end; }
.slot-cover { position: absolute; inset: 0; width: 100%; height: 100%; object-fit: cover; }
.slot-cover.blank { background: var(--portal-surface-3); }
.slot-caption {
  position: relative;
  width: 100%;
  display: grid;
  gap: 0;
  padding: 14px 6px 5px;
  color: var(--portal-on-media);
  background: linear-gradient(180deg, transparent, var(--portal-scrim-soft));
  text-align: left;
}
.slot-caption strong,
.slot-names {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 11px;
  font-weight: 600;
}
.slot-caption small { font-size: 10px; opacity: .85; }
.slot-names { max-width: 100%; color: var(--portal-text-soft); font-weight: 500; }
.slot-avatars { display: flex; }
.slot-avatar {
  width: 34px;
  height: 34px;
  display: grid;
  place-items: center;
  border: 2px solid var(--portal-surface-2);
  border-radius: 50%;
  overflow: hidden;
  color: var(--portal-muted);
  background: var(--portal-surface-3);
}
.slot-avatar + .slot-avatar { margin-left: -10px; }
.slot-avatar img { width: 100%; height: 100%; object-fit: cover; }
.slot-remove,
.slot-chip-remove {
  width: 18px;
  height: 18px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 50%;
  padding: 0;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  cursor: pointer;
}
.slot-remove { position: absolute; top: 5px; right: 5px; z-index: 2; }
.slot-chips { position: absolute; top: 5px; right: 5px; z-index: 2; display: flex; gap: 3px; }
.slot-chip-remove { width: 16px; height: 16px; }
.slot-remove:hover:not(:disabled),
.slot-chip-remove:hover:not(:disabled) { background: var(--portal-on-media-bg-hover); }
.slot-remove:disabled,
.slot-chip-remove:disabled,
.slot-mini:disabled { cursor: not-allowed; opacity: .5; }
/* 源视频 */
.slot-video {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: flex-end;
  gap: 4px;
  border-radius: 11px;
  padding: 6px;
  overflow: hidden;
}
.slot-video .slot-names { position: relative; }
.slot-video.failed { box-shadow: inset 0 0 0 1px var(--portal-danger-line); }
.slot-icon { margin: auto 0; color: var(--portal-muted); }
.slot-video-actions { position: absolute; inset: 22px 0 0; display: flex; align-items: center; justify-content: center; gap: 6px; }
.slot-mini {
  width: 28px;
  height: 28px;
  display: grid;
  place-items: center;
  border: 1px dashed var(--portal-line-strong);
  border-radius: 8px;
  color: var(--portal-text-soft);
  background: transparent;
  cursor: pointer;
}
.slot-mini:hover:not(:disabled) { color: var(--portal-text); background: var(--portal-hover); }
.slot-overlay {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 2px;
  border: 0;
  color: var(--portal-on-media);
  background: var(--portal-scrim-soft);
}
.slot-overlay.failed { background: var(--portal-scrim); cursor: pointer; }
.slot-card .slot-picker { bottom: auto; top: calc(100% + 4px); left: 0; }
.spin { animation: slot-spin 1s linear infinite; }
@keyframes slot-spin { to { transform: rotate(360deg); } }
</style>
