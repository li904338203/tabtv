<script setup lang="ts">
/**
 * 文字层编辑（SCT-22，SC-F-50）：内容、字号（画面高度的百分比）、颜色、描边、底框、对齐、位置、起止。
 * 本地草稿，点「应用」提交一次（一步撤销）。缺字提示来自排版接口（`X-Typeset-Missing`）：白名单字体里没有的字会画成替代字形。
 */
import { ElMessage } from "element-plus";
import { useSmartcutTypeset } from "~/composables/useSmartcutTypeset";
import { smartcutPatchText, smartcutRemoveText } from "~/utils/smartcutPanels";

const props = defineProps<{ textId: string; editable: boolean; disabledReason: string }>();
const store = useSmartcutStore();
const typeset = useSmartcutTypeset();

const item = computed(() => (store.timeline?.texts ?? []).find(x => x.id === props.textId) ?? null);

type Draft = {
  text: string;
  sizePct: number;
  color: string;
  stroke: boolean;
  strokeColor: string;
  strokePct: number;
  background: boolean;
  bgColor: string;
  bgOpacity: number;
  align: "left" | "center" | "right";
  xPct: number;
  yPct: number;
  anchor: "top" | "center" | "bottom";
  startSec: number;
  endSec: number;
};
const draft = ref<Draft | null>(null);

const num = (v: unknown, d: number) => (typeof v === "number" && Number.isFinite(v) ? v : d);
const str = (v: unknown, d: string) => (typeof v === "string" && v ? v : d);
const obj = (v: unknown) => (v && typeof v === "object" && !Array.isArray(v) ? v as Record<string, unknown> : null);

function fromItem(): Draft | null {
  const x = item.value;
  if (!x) return null;
  const style = obj(x.style) ?? {};
  const box = obj(x.box) ?? {};
  const stroke = obj(style.stroke);
  const bg = obj(style.background);
  return {
    text: x.text,
    sizePct: num(style.fontSizePct, 8),
    color: str(style.color, "#ffffff"),
    stroke: Boolean(stroke),
    strokeColor: str(stroke?.color, "#000000"),
    strokePct: num(stroke?.widthPct, 0.3),
    background: Boolean(bg),
    bgColor: str(bg?.color, "#000000"),
    bgOpacity: num(bg?.opacity, 0.6),
    align: (["left", "center", "right"].includes(String(style.align)) ? style.align : "center") as Draft["align"],
    xPct: num(box.xPct, 50),
    yPct: num(box.yPct, 50),
    anchor: (["top", "center", "bottom"].includes(String(box.anchor)) ? box.anchor : "center") as Draft["anchor"],
    startSec: x.startMs / 1000,
    endSec: x.endMs / 1000
  };
}
watch(() => [props.textId, JSON.stringify(item.value)], () => { draft.value = fromItem(); }, { immediate: true });

/** 缺字：当前内容排版后的缺字列表（排过才有；没排过时触发一次排版）。 */
const missing = computed(() => {
  const x = item.value;
  const t = store.timeline;
  if (!x || !t) return "";
  return typeset.entry({ kind: "text", text: x.text, style: x.style, box: x.box }, t.width, t.height)?.missing ?? "";
});
watch(item, (x) => {
  const t = store.timeline;
  if (x && t) typeset.schedule(`editor:${x.id}`, { kind: "text", text: x.text, style: x.style, box: x.box }, t.width, t.height);
}, { immediate: true });

function apply() {
  const d = draft.value;
  const x = item.value;
  if (!d || !x || !props.editable) return;
  const baseStyle = obj(x.style) ?? {};
  const baseBox = obj(x.box) ?? {};
  // 只改认得的键，未知字段原样保留（服务端与排版都会忽略它们，但用户的数据不能被我们丢掉）
  const { stroke: _s, background: _b, ...restStyle } = baseStyle;
  const style: Record<string, unknown> = {
    ...restStyle,
    fontSizePct: Number(d.sizePct),
    color: d.color,
    align: d.align,
    ...(d.stroke ? { stroke: { ...(obj(baseStyle.stroke) ?? {}), color: d.strokeColor, widthPct: Number(d.strokePct) } } : {}),
    ...(d.background ? { background: { ...(obj(baseStyle.background) ?? {}), color: d.bgColor, opacity: Number(d.bgOpacity) } } : {})
  };
  const box = { ...baseBox, xPct: Number(d.xPct), yPct: Number(d.yPct), anchor: d.anchor };
  let failure = "";
  store.commit((t) => {
    const r = smartcutPatchText(t, store.currentId(props.textId), {
      text: d.text, style, box, startMs: Math.round(Number(d.startSec) * 1000), endMs: Math.round(Number(d.endSec) * 1000)
    });
    if (!r.ok) {
      failure = r.reason;
      return t;
    }
    return r.value;
  });
  if (failure) ElMessage.warning(failure);
}

function remove() {
  if (!props.editable) return;
  if (store.commit(t => smartcutRemoveText(t, store.currentId(props.textId)))) store.select(null);
}
</script>

<template>
  <section v-if="item && draft" class="sc-panel" aria-label="文字层" data-testid="smartcut-text-editor">
    <div class="sc-section">
      <h3>文字层 <button type="button" class="sc-btn danger" :disabled="!editable" :title="editable ? '删除这条文字' : disabledReason" aria-label="删除这条文字" @click="remove">删除</button></h3>
      <textarea v-model="draft.text" class="sc-textarea" rows="3" aria-label="文字内容" :disabled="!editable" />
      <p v-if="missing" class="sc-warn" role="note">这些字在字体里没有，会画成替代字形：{{ missing }}</p>
      <p v-if="!editable" class="sc-note">{{ disabledReason }}</p>
    </div>
    <div class="sc-section">
      <h3>样式</h3>
      <div class="sc-field"><label for="sc-tx-size">字号（画面高度 %）</label><input id="sc-tx-size" v-model.number="draft.sizePct" class="sc-input" type="number" min="2" max="20" step="0.5" :disabled="!editable"></div>
      <div class="sc-field"><label for="sc-tx-color">颜色</label><input id="sc-tx-color" v-model="draft.color" class="sc-input" type="color" :disabled="!editable"></div>
      <div class="sc-field">
        <label for="sc-tx-stroke">描边</label>
        <div class="sc-seg">
          <input id="sc-tx-stroke" v-model="draft.stroke" type="checkbox" :disabled="!editable" aria-label="描边">
          <input v-if="draft.stroke" v-model="draft.strokeColor" class="sc-input sc-narrow" type="color" aria-label="描边颜色" :disabled="!editable">
          <input v-if="draft.stroke" v-model.number="draft.strokePct" class="sc-input sc-narrow" type="number" min="0" max="5" step="0.1" aria-label="描边粗细（画面高度 %）" :disabled="!editable">
        </div>
      </div>
      <div class="sc-field">
        <label for="sc-tx-bg">底框</label>
        <div class="sc-seg">
          <input id="sc-tx-bg" v-model="draft.background" type="checkbox" :disabled="!editable" aria-label="底框">
          <input v-if="draft.background" v-model="draft.bgColor" class="sc-input sc-narrow" type="color" aria-label="底框颜色" :disabled="!editable">
          <input v-if="draft.background" v-model.number="draft.bgOpacity" class="sc-input sc-narrow" type="number" min="0" max="1" step="0.1" aria-label="底框不透明度" :disabled="!editable">
        </div>
      </div>
      <div class="sc-field">
        <label for="sc-tx-align">对齐</label>
        <select id="sc-tx-align" v-model="draft.align" class="sc-select" :disabled="!editable">
          <option value="left">左对齐</option><option value="center">居中</option><option value="right">右对齐</option>
        </select>
      </div>
    </div>
    <div class="sc-section">
      <h3>位置与时间</h3>
      <div class="sc-field"><label for="sc-tx-x">水平中心 %</label><input id="sc-tx-x" v-model.number="draft.xPct" class="sc-input" type="number" min="0" max="100" step="1" :disabled="!editable"></div>
      <div class="sc-field"><label for="sc-tx-y">垂直位置 %</label><input id="sc-tx-y" v-model.number="draft.yPct" class="sc-input" type="number" min="0" max="100" step="1" :disabled="!editable"></div>
      <div class="sc-field">
        <label for="sc-tx-anchor">对齐到</label>
        <select id="sc-tx-anchor" v-model="draft.anchor" class="sc-select" :disabled="!editable">
          <option value="top">上边</option><option value="center">中心</option><option value="bottom">下边</option>
        </select>
      </div>
      <div class="sc-field"><label for="sc-tx-start">开始（秒）</label><input id="sc-tx-start" v-model.number="draft.startSec" class="sc-input" type="number" min="0" step="0.1" :disabled="!editable"></div>
      <div class="sc-field"><label for="sc-tx-end">结束（秒）</label><input id="sc-tx-end" v-model.number="draft.endSec" class="sc-input" type="number" min="0" step="0.1" :disabled="!editable"></div>
    </div>
    <button type="button" class="sc-btn primary" :disabled="!editable" :title="editable ? '应用（一次撤销）' : disabledReason" data-testid="smartcut-text-apply" @click="apply">应用</button>
  </section>
</template>

<style scoped>
.sc-narrow { width: 64px; }
</style>
