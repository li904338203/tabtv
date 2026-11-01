<script setup lang="ts">
/**
 * 素材变化条（SCT-31，SC-F-150~153）：上游重新生成过、时间线里用的还是旧产物时出现。两个处理，都不自动做：
 * - 「只替换变了的那几段」：同一来源节点的新产物原位换进片段，裁切、变速与转场保留；新素材更短时截到素材长度，
 *   被截短的段逐条说出来（`shortened`）；
 * - 「按新素材重新剪」：重新 AI 剪辑（用户改过的片段先提示，可撤销）。
 * 哪些段变了由服务端判（素材条的 `changed`，SCR-11），这里只按来源节点对上段号。
 */
import { RefreshCw, Scissors, X } from "@lucide/vue";
import { smartcutChangedText } from "~/utils/smartcutPlan";

const props = defineProps<{
  /** 变了的段号（从 1 起）；摘要说素材有变化但没有片段用到变了的素材时为空。 */
  positions: number[];
  /** 不能点的原因（只读、进行中）；空 = 能点。 */
  disabledReason?: string;
  /** 「按新素材重新剪」不能点的原因（另外还要看 AI 剪辑可不可用）。 */
  replanDisabledReason?: string;
  /** 「按新素材重新剪」出不出现（AI 剪辑关着时不出现）；缺省出现。 */
  allowReplan?: boolean;
  /** 上一次替换的结果（被截短的段）。 */
  notice?: { text: string; details: string[] } | null;
}>();
const emit = defineEmits<{ replace: []; replan: []; dismiss: [] }>();

const text = computed(() => smartcutChangedText(props.positions));
const replaceReason = computed(() => props.disabledReason || (props.positions.length ? "" : "时间线里没有用到变了的素材"));
</script>

<template>
  <div class="sc-changed" role="status" data-testid="smartcut-inputs-changed">
    <RefreshCw :size="14" aria-hidden="true" />
    <p>{{ text }}</p>
    <div class="sc-seg">
      <button
        type="button"
        class="sc-btn"
        :disabled="Boolean(replaceReason)"
        :title="replaceReason || '同一来源的新产物原位换进片段，保留裁切、变速与转场（写入前存快照，可撤销）'"
        data-testid="smartcut-replace-changed"
        @click="emit('replace')"
      ><RefreshCw :size="13" />只替换变了的那几段</button>
      <button
        v-if="allowReplan !== false"
        type="button"
        class="sc-btn"
        :disabled="Boolean(replanDisabledReason)"
        :title="replanDisabledReason || '按新素材重新 AI 剪辑（免费，可撤销）'"
        data-testid="smartcut-replan"
        @click="emit('replan')"
      ><Scissors :size="13" />按新素材重新剪</button>
    </div>
    <button type="button" class="sc-btn icon" aria-label="先不处理" title="先不处理（节点卡上仍会提示素材有变化）" @click="emit('dismiss')"><X :size="13" /></button>
    <ul v-if="notice?.details.length" class="sc-changed-details" data-testid="smartcut-replace-shortened">
      <li class="sc-note">{{ notice.text }}</li>
      <li v-for="(line, i) in notice.details" :key="i" class="sc-warn">{{ line }}</li>
    </ul>
  </div>
</template>

<style scoped>
.sc-changed {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  min-height: 36px;
  padding: 4px 10px 4px 14px;
  border-bottom: 1px solid var(--portal-gold-line);
  color: var(--portal-text);
  background: var(--portal-gold-soft);
  font-size: 12px;
}
.sc-changed > svg { flex-shrink: 0; color: var(--portal-gold); }
.sc-changed p { flex: 1; min-width: 160px; margin: 0; }
.sc-changed-details { flex-basis: 100%; display: grid; gap: 2px; margin: 0; padding: 0 0 4px 22px; list-style: none; }
</style>
