<script setup lang="ts">
/**
 * 选项详情抽屉（MKT-T-13；MKT-UX-03、dev/03 §3.2）：说明、适用输入、是否依赖出声、推荐模型与画幅时长、封面大图。
 *
 * - **不显示提示词片段**（MKT-AC-06）：接口本来就不给（`MarketingOption` 上没有这个字段），这里也不留位置。
 * - 打开时先用列表里那一份立刻显示，再取一次详情（`GET /marketing/options/{code}`）刷新 ——
 *   弹层开着的时候运营可能下线了它；取到 404 说清「已下线」并禁用「选用」，而不是让他选了之后在发送时才被拒。
 * - 封面与示例视频走公开端点，可直接 `<img>` / `<video>`。
 * - 不可用的照样能看详情，「选用」按钮按 `block.selectable` 决定，并把原因写在按钮上方。
 */
import { ChevronLeft, ChevronRight, Volume2, X } from "@lucide/vue";
import {
  KIND_LABEL,
  inputRequirementText,
  optionCoverUrl,
  optionPreviewUrl,
  recommendedParamsText,
  type MarketingOption,
  type OptionBlock
} from "~/utils/marketingOptions";

const props = defineProps<{
  option: MarketingOption;
  block?: OptionBlock | null;
  selected?: boolean;
}>();
const emit = defineEmits<{ select: [option: MarketingOption]; close: [] }>();

const apiBase = useRuntimeConfig().public.apiBase as string;
const current = ref<MarketingOption>(props.option);
const gone = ref(false);
const coverIndex = ref(0);
const panel = ref<HTMLElement>();

const label = computed(() => KIND_LABEL[current.value.kind] ?? "选项");
const outputs = computed(() => (current.value.outputTypes ?? []).map(item => (item === "video" ? "视频" : item === "image" ? "图片" : item)).join("、") || "不限");
const params = computed(() => recommendedParamsText(current.value.recommendedParams));
const canSelect = computed(() => !gone.value && (!props.block || props.block.selectable));

async function refresh(code: string) {
  gone.value = false;
  try {
    const fresh = await useApi()<MarketingOption>(`/marketing/options/${encodeURIComponent(code)}`);
    if (fresh?.code === code) current.value = fresh;
  } catch (cause) {
    const status = (cause as { statusCode?: number })?.statusCode;
    // 只有 404 说「已下线」；断网 / 5xx 不改判断，继续显示列表里那一份。
    if (status === 404) gone.value = true;
  }
}

watch(() => props.option, (next) => {
  current.value = next;
  coverIndex.value = 0;
  void refresh(next.code);
}, { immediate: true });

function stepCover(delta: number) {
  const count = current.value.coverCount;
  if (count > 1) coverIndex.value = (coverIndex.value + delta + count) % count;
}

function onKey(event: KeyboardEvent) {
  if (event.key === "Escape") {
    // 只关抽屉，不连带关掉弹层
    event.stopPropagation();
    emit("close");
  }
}

onMounted(() => nextTick(() => panel.value?.focus()));
</script>

<template>
  <Teleport to="body">
    <div class="option-detail-layer" data-option-detail @keydown="onKey">
      <div class="option-detail-scrim" @click="emit('close')" />
      <aside ref="panel" class="option-detail" role="dialog" :aria-label="`${label}详情：${current.name}`" tabindex="-1">
        <header class="detail-head">
          <span class="detail-kind">{{ label }}</span>
          <strong>{{ current.name }}</strong>
          <button type="button" class="detail-close" aria-label="关闭详情" @click="emit('close')"><X :size="16" /></button>
        </header>
        <div class="detail-body">
          <div class="detail-media">
            <video
              v-if="current.hasPreview && !gone"
              :key="`v-${current.code}`"
              :src="optionPreviewUrl(apiBase, current.code)"
              :poster="current.coverCount ? optionCoverUrl(apiBase, current.code, 0, 640) : undefined"
              controls
              muted
              playsinline
              preload="metadata"
            />
            <img v-else-if="current.coverCount" :src="optionCoverUrl(apiBase, current.code, coverIndex, 640)" :alt="current.name">
            <div v-else class="detail-placeholder">{{ current.name.slice(0, 1) }}</div>
            <template v-if="!current.hasPreview && current.coverCount > 1">
              <button type="button" class="detail-step prev" aria-label="上一张封面" @click="stepCover(-1)"><ChevronLeft :size="16" /></button>
              <button type="button" class="detail-step next" aria-label="下一张封面" @click="stepCover(1)"><ChevronRight :size="16" /></button>
              <span class="detail-count">{{ coverIndex + 1 }} / {{ current.coverCount }}</span>
            </template>
          </div>
          <p v-if="gone" class="detail-gone">这个{{ label }}已下线，换一个吧</p>
          <p class="detail-desc">{{ current.description || '暂无说明' }}</p>
          <dl class="detail-facts">
            <dt>适用输入</dt>
            <dd>{{ inputRequirementText(current.inputRequirements) }}</dd>
            <dt>适用产出</dt>
            <dd>{{ outputs }}</dd>
            <template v-if="current.isAudioRequired">
              <dt>出声</dt>
              <dd class="detail-audio"><Volume2 :size="13" />需要能出声的视频模型才生效</dd>
            </template>
            <template v-if="current.recommendedModels.length">
              <dt>推荐模型</dt>
              <dd>
                <span v-for="model in current.recommendedModels" :key="model.code" class="detail-model" :class="{ off: !model.available }" :title="model.available ? '' : '此刻不可用'">
                  {{ model.name || model.code }}<small v-if="!model.available">（此刻不可用）</small>
                </span>
                <small class="detail-note">只在「智能匹配模型」时参考，不会替你切换</small>
              </dd>
            </template>
            <template v-if="params">
              <dt>推荐画幅时长</dt>
              <dd>{{ params }}</dd>
            </template>
            <template v-if="current.tags.length">
              <dt>标签</dt>
              <dd>{{ current.tags.join(' · ') }}</dd>
            </template>
          </dl>
        </div>
        <footer class="detail-foot">
          <p v-if="block && !gone" class="detail-reason" :class="block.userFixable ? 'fixable' : 'ops'">
            {{ block.userFixable ? block.text : `暂不可用：${block.text}` }}
          </p>
          <button type="button" class="detail-select" :disabled="!canSelect || selected" @click="emit('select', current)">
            {{ selected ? '已选用' : `选用这个${label}` }}
          </button>
        </footer>
      </aside>
    </div>
  </Teleport>
</template>

<style scoped>
.option-detail-layer { position: fixed; inset: 0; z-index: 120; }
.option-detail-scrim { position: absolute; inset: 0; background: var(--portal-scrim-soft); }
.option-detail {
  position: absolute;
  top: 0;
  right: 0;
  bottom: 0;
  width: min(380px, 100vw);
  display: flex;
  flex-direction: column;
  border-left: 1px solid var(--portal-line-strong);
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: -18px 0 48px var(--portal-shadow-strong);
  font-size: 13px;
  outline: none;
}
.detail-head { display: flex; align-items: center; gap: 8px; padding: 14px 16px; border-bottom: 1px solid var(--portal-line-soft); }
.detail-head strong { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 15px; }
.detail-kind { flex: none; border-radius: 999px; padding: 2px 8px; color: var(--portal-cyan); background: var(--portal-cyan-soft); font-size: 11px; }
.detail-close { display: grid; place-items: center; width: 28px; height: 28px; border: 0; border-radius: 6px; color: var(--portal-muted); background: transparent; cursor: pointer; }
.detail-close:hover { color: var(--portal-text); background: var(--portal-hover); }
.detail-body { flex: 1; overflow-y: auto; padding: 14px 16px; display: grid; align-content: start; gap: 12px; }
.detail-media { position: relative; display: grid; place-items: center; border-radius: 10px; overflow: hidden; background: var(--portal-media-letterbox); aspect-ratio: 3 / 4; max-height: 52vh; }
.detail-media img, .detail-media video { width: 100%; height: 100%; object-fit: contain; }
:root[data-theme="light"] .detail-media { box-shadow: 0 0 0 1px var(--portal-line); }
.detail-placeholder { font-size: 40px; color: var(--portal-faint); }
.detail-step { position: absolute; top: 50%; transform: translateY(-50%); display: grid; place-items: center; width: 28px; height: 28px; border: 0; border-radius: 50%; color: var(--portal-on-media); background: var(--portal-on-media-bg); cursor: pointer; }
.detail-step:hover { background: var(--portal-on-media-bg-hover); }
.detail-step.prev { left: 8px; }
.detail-step.next { right: 8px; }
.detail-count { position: absolute; bottom: 8px; right: 10px; border-radius: 999px; padding: 1px 8px; color: var(--portal-on-media); background: var(--portal-on-media-bg); font-size: 11px; }
.detail-gone { margin: 0; color: var(--portal-danger); }
.detail-desc { margin: 0; color: var(--portal-text-soft); line-height: 20px; white-space: pre-line; }
.detail-facts { display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 8px 12px; margin: 0; }
.detail-facts dt { color: var(--portal-muted); }
.detail-facts dd { margin: 0; display: flex; flex-wrap: wrap; align-items: center; gap: 4px 8px; color: var(--portal-text-soft); }
.detail-audio { color: var(--portal-gold) !important; }
.detail-model { border: 1px solid var(--portal-line); border-radius: 999px; padding: 1px 8px; }
.detail-model.off { color: var(--portal-muted); border-style: dashed; }
.detail-note { width: 100%; color: var(--portal-muted); }
.detail-foot { display: grid; gap: 8px; padding: 12px 16px; border-top: 1px solid var(--portal-line-soft); }
.detail-reason { margin: 0; font-size: 12px; }
.detail-reason.fixable { color: var(--portal-gold); }
.detail-reason.ops { color: var(--portal-muted); }
.detail-select { height: 36px; border: 0; border-radius: 8px; color: var(--portal-on-accent); background: var(--portal-cyan); font: inherit; cursor: pointer; }
.detail-select:disabled { color: var(--portal-disabled-text); background: var(--portal-disabled-bg); cursor: not-allowed; }
</style>
