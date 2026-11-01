<script setup lang="ts">
/**
 * 新增 / 编辑商品（MKT-T-38，营销 dev/05 §4.4、§5；MKT-F-93/95、MKT-UX-15/17，对标截图 59 / 60）。
 *
 * - 图片条：多选上传（上传中 / 失败 / 重试三态）、拖动排序、「设为主图」、删除；首张标「主图」；
 *   满上限「＋」禁用并说「最多 10 张」（数字与格式都取 `/marketing/config`）。
 * - 名称\* / 卖点 / 描述：按码点计数（emoji 算 1）。
 * - 保存：新建 `POST`、编辑 `PUT` 带 `version`；40902 说清是谁改的、**保留本地输入**，用户自己点「重新加载」才覆盖；
 *   配额满（409 `reason=quota`）告诉列表把「新增」禁用。保存成功交回商品详情（列表据此自动选中，MKT-UX-14）。
 * - 关闭：有未保存改动或上传中时确认一次（MKT-UX-15）。
 *
 * 规则全在 `utils/marketingLibrary.ts`（有单测）。
 * ⚠️ 打开时只发 GET（编辑时取详情）；写请求只在点「保存」时发，上传只在选文件 / 点重试时发。
 * ⚠️ 刚选的图用本地 object URL 预览，已有的图走 `useAuthenticatedMedia`（受保护媒体，401）—— 两种都要释放。
 */
import { ElMessageBox } from "element-plus";
import { LoaderCircle, Plus, RotateCw, Star, TriangleAlert, X } from "@lucide/vue";
import { mediaThumbUrl } from "~/utils/authorizedMedia";
import { apiFailure } from "~/utils/agentPanelBridge";
import {
  marketingAddImages,
  marketingCloseConfirmText,
  marketingConflictText,
  marketingEmptyProductForm,
  marketingImageAccept,
  marketingImageAllowed,
  marketingImageGate,
  marketingImageHint,
  marketingIsQuotaError,
  marketingMoveImage,
  marketingPatchImage,
  marketingProductFormIssue,
  marketingProductFormOf,
  marketingProductInput,
  marketingRemoveImage,
  marketingSetMainImage,
  marketingTextCheck,
  type MarketingImageItem,
  type MarketingProduct,
  type MarketingProductForm,
  type MarketingProductLimits
} from "~/utils/marketingLibrary";

const props = withDefaults(defineProps<{
  /** 编辑哪个商品；不传 = 新增。 */
  productId?: string;
  limits: MarketingProductLimits;
  readOnly?: boolean;
}>(), { productId: "", readOnly: false });

const emit = defineEmits<{
  close: [];
  saved: [product: MarketingProduct, created: boolean];
  /** 服务端说配额满了（列表据此禁用「新增」）。 */
  "quota-full": [];
}>();

const referenceUpload = useReferenceUpload();
const thumbs = useAuthenticatedMedia();
const isEdit = computed(() => Boolean(props.productId));

const form = ref<MarketingProductForm>(marketingEmptyProductForm());
const original = ref<MarketingProductForm>(marketingEmptyProductForm());
const version = ref(0);
const loading = ref(false);
const loadError = ref("");
const saving = ref(false);
const saveError = ref("");
const conflict = ref("");
const notice = ref("");
const fileInput = ref<HTMLInputElement>();
/** localId → 本地预览 URL（刚选的文件）。 */
const previews = ref<Record<string, string>>({});
/** localId → 文件（重试用）。 */
const files = new Map<string, File>();
const dragFrom = ref<number | null>(null);
const dragOver = ref<number | null>(null);

const gate = computed(() => marketingImageGate(form.value.images, props.limits.maxImages));
const issue = computed(() => marketingProductFormIssue(form.value, props.limits, props.readOnly));
const nameCheck = computed(() => marketingTextCheck(form.value.name.trim(), props.limits.name, "商品名称"));
const pointsCheck = computed(() => marketingTextCheck(form.value.sellingPoints, props.limits.sellingPoints, "商品卖点"));
const descCheck = computed(() => marketingTextCheck(form.value.description, props.limits.description, "商品描述"));
const canSave = computed(() => !issue.value && !saving.value && !loading.value && !loadError.value);

function localId(): string {
  return `p_${Date.now().toString(36)}_${Math.random().toString(36).slice(2, 8)}`;
}

function loadThumbs(images: readonly MarketingImageItem[]) {
  const wanted = images.filter(item => item.mediaId && !previews.value[item.localId])
    .map(item => ({ key: item.mediaId!, url: mediaThumbUrl(item.mediaId, 320) }));
  if (wanted.length) void thumbs.appendAll(wanted);
}

function thumbOf(item: MarketingImageItem): string | undefined {
  return previews.value[item.localId] ?? (item.mediaId ? thumbs.urls.value[item.mediaId] : undefined);
}

function applyProduct(product: MarketingProduct) {
  const next = marketingProductFormOf(product, localId);
  form.value = next;
  original.value = { ...next, images: [...next.images] };
  version.value = product.version;
  loadThumbs(next.images);
}

async function load() {
  if (!props.productId) return;
  loading.value = true;
  loadError.value = "";
  try {
    applyProduct(await useApi()<MarketingProduct>(`/marketing/products/${encodeURIComponent(props.productId)}`));
  } catch (cause) {
    const failure = apiFailure(cause);
    loadError.value = !navigator.onLine ? "网络已断开，恢复后再试" : (failure.code === 40401 || (cause as { statusCode?: number }).statusCode === 404 ? "这个商品已不存在" : failure.message);
  } finally {
    loading.value = false;
  }
}

// ---------------------------------------------------------------- 图片

function pickFiles() {
  if (props.readOnly || !gate.value.ok) return;
  fileInput.value?.click();
}

function onFiles(event: Event) {
  const input = event.target as HTMLInputElement;
  const picked = Array.from(input.files ?? []);
  input.value = "";
  const allowed: Array<{ item: MarketingImageItem; file: File }> = [];
  const refused: string[] = [];
  for (const file of picked) {
    if (!marketingImageAllowed(file.type, props.limits.mimes)) {
      refused.push(file.name);
      continue;
    }
    allowed.push({ item: { localId: localId(), status: "uploading", name: file.name }, file });
  }
  const result = marketingAddImages(form.value.images, allowed.map(entry => entry.item), props.limits.maxImages);
  notice.value = [
    refused.length ? `${refused.join("、")}：${marketingImageHint(props.limits).split("，")[0]}` : "",
    result.notice
  ].filter(Boolean).join("；");
  form.value = { ...form.value, images: result.items };
  const added = new Set(result.added.map(item => item.localId));
  for (const { item, file } of allowed) {
    if (!added.has(item.localId)) continue;
    files.set(item.localId, file);
    previews.value = { ...previews.value, [item.localId]: URL.createObjectURL(file) };
    void uploadOne(item.localId);
  }
}

async function uploadOne(id: string) {
  const file = files.get(id);
  if (!file) return;
  form.value = { ...form.value, images: marketingPatchImage(form.value.images, id, { status: "uploading", error: undefined }) };
  try {
    const mediaId = await referenceUpload.upload(file, { mediaType: "image" });
    form.value = { ...form.value, images: marketingPatchImage(form.value.images, id, { status: "done", mediaId, error: undefined }) };
  } catch (cause) {
    const message = !navigator.onLine ? "网络已断开，恢复后点重试" : (cause as { message?: string })?.message || "上传没有成功，请重试";
    form.value = { ...form.value, images: marketingPatchImage(form.value.images, id, { status: "failed", error: message }) };
  }
}

function retry(item: MarketingImageItem) {
  if (files.has(item.localId)) void uploadOne(item.localId);
  else remove(item);
}

function remove(item: MarketingImageItem) {
  if (props.readOnly) return;
  form.value = { ...form.value, images: marketingRemoveImage(form.value.images, item.localId) };
  files.delete(item.localId);
  const url = previews.value[item.localId];
  if (url) {
    URL.revokeObjectURL(url);
    const { [item.localId]: _drop, ...rest } = previews.value;
    previews.value = rest;
  }
}

function makeMain(index: number) {
  if (props.readOnly) return;
  form.value = { ...form.value, images: marketingSetMainImage(form.value.images, index) };
}

function onDragStart(index: number, event: DragEvent) {
  if (props.readOnly) return;
  dragFrom.value = index;
  event.dataTransfer?.setData("text/plain", String(index));
  if (event.dataTransfer) event.dataTransfer.effectAllowed = "move";
}

function onDrop(index: number) {
  if (dragFrom.value !== null) form.value = { ...form.value, images: marketingMoveImage(form.value.images, dragFrom.value, index) };
  dragFrom.value = null;
  dragOver.value = null;
}

// ---------------------------------------------------------------- 保存 / 重新加载 / 关闭

async function save() {
  if (!canSave.value) return;
  saving.value = true;
  saveError.value = "";
  try {
    const body = marketingProductInput(form.value);
    const product = isEdit.value
      ? await useApi()<MarketingProduct>(`/marketing/products/${encodeURIComponent(props.productId)}`, { method: "PUT", body: { ...body, version: version.value } })
      : await useApi()<MarketingProduct>("/marketing/products", { method: "POST", body });
    conflict.value = "";
    original.value = form.value;
    emit("saved", product, !isEdit.value);
  } catch (cause) {
    if (!navigator.onLine) {
      saveError.value = "网络已断开，恢复后再保存";
      return;
    }
    const failure = apiFailure(cause);
    if (failure.code === 40902) {
      conflict.value = marketingConflictText(failure.details);
      return;
    }
    if (marketingIsQuotaError(failure)) emit("quota-full");
    saveError.value = failure.message;
  } finally {
    saving.value = false;
  }
}

async function reload() {
  try {
    await ElMessageBox.confirm("重新加载会用最新内容替换你当前的输入。", "重新加载？", { confirmButtonText: "重新加载", cancelButtonText: "取消" });
  } catch {
    return;
  }
  conflict.value = "";
  releasePreviews();
  await load();
}

async function requestClose() {
  if (saving.value) return;
  const text = props.readOnly ? "" : marketingCloseConfirmText(form.value, original.value);
  if (text) {
    try {
      await ElMessageBox.confirm(text, "放弃修改？", { confirmButtonText: "放弃", cancelButtonText: "继续编辑" });
    } catch {
      return;
    }
  }
  emit("close");
}

function releasePreviews() {
  for (const url of Object.values(previews.value)) URL.revokeObjectURL(url);
  previews.value = {};
  files.clear();
}

function onKeydown(event: KeyboardEvent) {
  if (event.key !== "Escape") return;
  // 商品选择弹窗也在听 Esc：这里先处理并拦下，否则一次 Esc 把两个弹窗都关了。
  event.stopImmediatePropagation();
  void requestClose();
}

onMounted(() => {
  window.addEventListener("keydown", onKeydown, true);
  void load();
});
onBeforeUnmount(() => {
  window.removeEventListener("keydown", onKeydown, true);
  releasePreviews();
});

defineExpose({ requestClose });
</script>

<template>
  <Teleport to="body">
    <div class="portal-overlay product-editor-overlay" @click.self="requestClose">
      <form class="product-editor" role="dialog" aria-modal="true" :aria-label="isEdit ? '编辑商品' : '新增商品'" @submit.prevent="save">
        <header class="pe-head">
          <h2>{{ isEdit ? "编辑商品" : "新增商品" }}</h2>
          <button type="button" class="pe-close" title="关闭（Esc）" aria-label="关闭" @click="requestClose"><X :size="15" /></button>
        </header>

        <p v-if="readOnly" class="pe-banner">你在该工作区是只读，不能新增、编辑或删除商品</p>
        <p v-if="loadError" class="pe-banner danger">{{ loadError }}</p>
        <div v-if="conflict" class="pe-banner warn" role="alert">
          <span>{{ conflict }}</span>
          <button type="button" class="pe-link" @click="reload">重新加载</button>
        </div>

        <div v-if="loading" class="pe-loading"><LoaderCircle :size="18" class="spin" />加载中…</div>
        <template v-else>
          <section class="pe-field">
            <span class="pe-label">商品图<em>*</em></span>
            <input ref="fileInput" type="file" class="pe-file" multiple :accept="marketingImageAccept(limits.mimes)" tabindex="-1" @change="onFiles">
            <ul class="pe-images">
              <li
                v-for="(item, index) in form.images"
                :key="item.localId"
                class="pe-image"
                :class="[item.status, { over: dragOver === index }]"
                :title="item.error || item.name"
                :draggable="!readOnly"
                @dragstart="onDragStart(index, $event)"
                @dragover.prevent="dragOver = index"
                @dragleave="dragOver = dragOver === index ? null : dragOver"
                @drop.prevent="onDrop(index)"
                @dragend="dragFrom = null; dragOver = null"
              >
                <img v-if="thumbOf(item)" :src="thumbOf(item)" :alt="item.name" draggable="false">
                <span v-if="index === 0" class="pe-main">主图</span>
                <span v-if="item.status === 'uploading'" class="pe-overlay" aria-label="上传中"><LoaderCircle :size="18" class="spin" /></span>
                <button v-else-if="item.status === 'failed'" type="button" class="pe-overlay failed" title="重新上传" @click="retry(item)">
                  <TriangleAlert :size="14" /><RotateCw :size="14" />
                </button>
                <button v-if="index > 0 && item.status === 'done' && !readOnly" type="button" class="pe-act pe-star" title="设为主图" aria-label="设为主图" @click="makeMain(index)"><Star :size="12" /></button>
                <button v-if="!readOnly" type="button" class="pe-act pe-remove" title="删除" aria-label="删除这张图" @click="remove(item)"><X :size="12" /></button>
              </li>
              <li>
                <button
                  type="button"
                  class="pe-add"
                  :disabled="readOnly || !gate.ok"
                  :title="readOnly ? '你在该工作区是只读' : gate.reason || '添加商品图'"
                  :aria-label="gate.reason || '添加商品图'"
                  @click="pickFiles"
                >
                  <Plus :size="20" />
                  <small v-if="!gate.ok">{{ gate.reason }}</small>
                </button>
              </li>
            </ul>
            <p class="pe-hint">{{ marketingImageHint(limits) }}<template v-if="form.images.length > 1">；拖动或点 ☆ 调整顺序，第一张为主图</template></p>
            <p v-if="notice" class="pe-hint warn">{{ notice }}</p>
          </section>

          <label class="pe-field">
            <span class="pe-label">商品名称<em>*</em><small :class="{ over: nameCheck.error }">{{ nameCheck.counter }}</small></span>
            <input v-model="form.name" type="text" placeholder="输入商品名称" :disabled="readOnly" :aria-invalid="Boolean(nameCheck.error)">
          </label>
          <label class="pe-field">
            <span class="pe-label">商品卖点（可选）<small :class="{ over: pointsCheck.error }">{{ pointsCheck.counter }}</small></span>
            <textarea v-model="form.sellingPoints" rows="2" placeholder="示例：高品质、耐用……" :disabled="readOnly" :aria-invalid="Boolean(pointsCheck.error)" />
          </label>
          <label class="pe-field">
            <span class="pe-label">商品描述（可选）<small :class="{ over: descCheck.error }">{{ descCheck.counter }}</small></span>
            <textarea v-model="form.description" rows="2" placeholder="示例：颜色、价格、营销活动……" :disabled="readOnly" :aria-invalid="Boolean(descCheck.error)" />
          </label>
        </template>

        <p v-if="saveError" class="pe-error" role="alert">{{ saveError }}</p>
        <footer class="pe-foot">
          <span v-if="issue && !loading && !loadError && !readOnly" class="pe-issue">{{ issue }}</span>
          <button type="button" class="pe-btn" @click="requestClose">取消</button>
          <button type="submit" class="pe-btn primary" :disabled="!canSave" :title="issue">
            <LoaderCircle v-if="saving" :size="14" class="spin" />保存
          </button>
        </footer>
      </form>
    </div>
  </Teleport>
</template>

<style scoped>
.product-editor-overlay { z-index: 90; }
.product-editor {
  width: min(680px, 100%);
  max-height: calc(100vh - 48px);
  overflow-y: auto;
  display: grid;
  gap: 16px;
  border: 1px solid var(--portal-line-strong);
  border-radius: 18px;
  padding: 22px 24px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 28px 70px var(--portal-shadow-strong);
}
.pe-head { display: flex; align-items: center; justify-content: space-between; }
.pe-head h2 { margin: 0; font-size: 16px; }
.pe-close {
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
.pe-close:hover { color: var(--portal-text); background: var(--portal-hover); }
.pe-banner {
  margin: 0;
  display: flex;
  align-items: center;
  gap: 10px;
  border-radius: 8px;
  padding: 8px 10px;
  color: var(--portal-text-soft);
  background: var(--portal-surface-2);
  font-size: 12px;
}
.pe-banner.warn { background: var(--portal-gold-soft); }
.pe-banner.danger { color: var(--portal-danger); background: var(--portal-danger-soft); }
.pe-link { margin-left: auto; border: 0; padding: 0; color: var(--portal-cyan); background: transparent; font: inherit; cursor: pointer; white-space: nowrap; }
.pe-loading { display: flex; align-items: center; justify-content: center; gap: 8px; min-height: 160px; color: var(--portal-muted); }
.pe-field { display: grid; gap: 8px; min-width: 0; }
.pe-label { display: flex; align-items: baseline; gap: 2px; font-size: 13px; font-weight: 600; }
.pe-label em { color: var(--portal-danger); font-style: normal; }
.pe-label small { margin-left: auto; color: var(--portal-faint); font-size: 11px; font-weight: 400; font-variant-numeric: tabular-nums; }
.pe-label small.over { color: var(--portal-danger); }
.pe-file { display: none; }
.pe-images { display: flex; flex-wrap: wrap; gap: 8px; margin: 0; padding: 0; list-style: none; }
.pe-image,
.pe-add {
  position: relative;
  width: 112px;
  height: 112px;
  border-radius: 12px;
}
.pe-image {
  display: grid;
  place-items: center;
  overflow: hidden;
  border: 1px solid var(--portal-line);
  background: var(--portal-surface-2);
}
.pe-image[draggable="true"] { cursor: grab; }
.pe-image.over { border-color: var(--portal-cyan); }
.pe-image.failed { border-color: var(--portal-danger-line); }
.pe-image img { width: 100%; height: 100%; object-fit: contain; }
.pe-main {
  position: absolute;
  top: 5px;
  left: 5px;
  border-radius: 4px;
  padding: 0 5px;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  font-size: 10px;
  line-height: 16px;
}
.pe-overlay {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 3px;
  border: 0;
  color: var(--portal-on-media);
  background: var(--portal-scrim-soft);
}
.pe-overlay.failed { background: var(--portal-scrim); cursor: pointer; }
.pe-act {
  position: absolute;
  bottom: 5px;
  width: 22px;
  height: 22px;
  display: grid;
  place-items: center;
  border: 0;
  border-radius: 6px;
  padding: 0;
  color: var(--portal-on-media);
  background: var(--portal-on-media-bg);
  cursor: pointer;
  opacity: 0;
  transition: opacity .12s;
}
.pe-star { left: 5px; }
.pe-remove { right: 5px; top: 5px; bottom: auto; }
.pe-image:hover .pe-act,
.pe-image:focus-within .pe-act { opacity: 1; }
@media (hover: none) { .pe-act { opacity: 1; } }
.pe-act:hover { background: var(--portal-on-media-bg-hover); }
.pe-add {
  display: grid;
  place-items: center;
  align-content: center;
  gap: 4px;
  border: 0;
  color: var(--portal-text-soft);
  background: var(--portal-surface-2);
  font: inherit;
  cursor: pointer;
}
.pe-add small { font-size: 11px; color: var(--portal-muted); }
.pe-add:hover:not(:disabled) { color: var(--portal-text); background: var(--portal-hover); }
.pe-add:disabled { cursor: not-allowed; opacity: .55; }
.pe-hint { margin: 0; color: var(--portal-muted); font-size: 12px; }
.pe-hint.warn { color: var(--portal-danger); }
.pe-field input,
.pe-field textarea {
  width: 100%;
  box-sizing: border-box;
  border: 1px solid var(--portal-line);
  border-radius: 10px;
  padding: 9px 12px;
  color: var(--portal-text);
  background: var(--portal-input-bg);
  font: inherit;
  font-size: 13px;
  resize: vertical;
}
.pe-field input { border-radius: 999px; }
.pe-field input::placeholder,
.pe-field textarea::placeholder { color: var(--portal-placeholder); }
.pe-field input:focus,
.pe-field textarea:focus { outline: none; border-color: var(--portal-cyan-line); }
.pe-field [aria-invalid="true"] { border-color: var(--portal-danger-line); }
.pe-field input:disabled,
.pe-field textarea:disabled { opacity: .7; cursor: not-allowed; }
.pe-error { margin: 0; color: var(--portal-danger); font-size: 12px; }
.pe-foot { display: flex; align-items: center; justify-content: flex-end; gap: 10px; }
.pe-issue { margin-right: auto; color: var(--portal-muted); font-size: 12px; }
.pe-btn {
  min-width: 72px;
  height: 34px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
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
.pe-btn:hover:not(:disabled) { background: var(--portal-hover); }
.pe-btn.primary { border-color: transparent; color: var(--portal-mark-text); background: var(--portal-mark-bg); }
.pe-btn.primary:hover:not(:disabled) { background: var(--portal-mark-bg); opacity: .9; }
.pe-btn:disabled { cursor: not-allowed; color: var(--portal-disabled-text); background: var(--portal-disabled-bg); border-color: transparent; }
.spin { animation: pe-spin 1s linear infinite; }
@keyframes pe-spin { to { transform: rotate(360deg); } }
</style>
