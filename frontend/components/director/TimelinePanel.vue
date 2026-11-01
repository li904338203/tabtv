<script setup lang="ts">
/**
 * 时间轴（DRT-24，DR-F-120~124，dev/03 §10）：编辑器底部，可收起。自研 DOM 组件（DRD-38），跟随亮暗主题。
 *
 * - 头部：播放 / 暂停（空格）、循环、当前位置「帧 45 · 1.50 s」、总时长（1~15 秒）、缩放档位、选中关键帧的插值与删除；
 * - 标尺：整秒与半秒刻度，按下拖动即移动播放头；
 * - 行：有关键帧的对象与选中的对象（`directorTimelineRows`）；每行「在当前帧打关键帧」，记录对象**此刻显示**的状态；
 * - 改总时长先过 `directorCanShrink`：有关键帧落在新时长之外就拒绝并说出有几个（DR-F-123）；
 * - 播放状态（当前帧、是否在播）在 store 里但不进场景、不进撤销栈；关键帧与总时长、循环的改动都走 `store.commit`。
 */
import { ElMessage } from "element-plus";
import { ChevronDown, ChevronUp, Diamond, Pause, Play, Repeat, Trash2 } from "@lucide/vue";
import { directorLoadPoses, directorPoseFor, type DirectorPoseData } from "~/director/assets";
import {
  directorEditTimeline,
  directorKeyObjectAt,
  directorSetTimelineSettings,
  directorTimelineOf,
  directorTimelineRows,
  type DirectorKeyRef,
  type DirectorTimelineRow
} from "~/utils/directorAnimation";
import {
  DIRECTOR_FPS,
  DIRECTOR_MAX_DURATION_FRAMES,
  DIRECTOR_MIN_DURATION_FRAMES,
  DIRECTOR_TIMELINE_ZOOMS,
  directorCanShrink,
  directorDeleteKeyframe,
  directorFrameAtX,
  directorFrameTimeText,
  directorInterpolationName,
  directorMoveKeyframe,
  directorSetInterpolation,
  directorShrinkRejectedText,
  directorTimelineErrorText,
  directorTimelineTicks,
  type DirectorInterpolation,
  type DirectorPoseLookup,
  type DirectorTimelineError
} from "~/utils/directorTimeline";
import type { DirectorScene } from "~/utils/directorScene";
import NumberField from "./props/NumberField.vue";
import TimelineTrack from "./TimelineTrack.vue";

const props = defineProps<{ selectedId: string | null; readonly: boolean; readonlyText: string; selectedKey: DirectorKeyRef | null }>();
const emit = defineEmits<{
  seek: [frame: number];
  play: [];
  pause: [];
  "update:selectedKey": [key: DirectorKeyRef | null];
  "select-object": [id: string];
}>();
const store = useDirectorStore();

const open = ref(true);
const zoom = ref<number>(4);
const ruler = ref<HTMLElement | null>(null);
const poseMap = shallowRef<Map<string, DirectorPoseData> | null>(null);
let scrubbing: number | null = null;

const timeline = computed(() => directorTimelineOf(store.scene));
const duration = computed(() => timeline.value.durationFrames);
const rows = computed(() => directorTimelineRows(store.scene, props.selectedId));
const ticks = computed(() => directorTimelineTicks(duration.value));
const selectedKeyframe = computed(() => {
  const key = props.selectedKey;
  if (!key) return null;
  return timeline.value.tracks.find(t => t.objectId === key.objectId && t.channel === key.channel)?.keyframes.find(k => k.frame === key.frame) ?? null;
});
const selectedRow = computed(() => (props.selectedKey ? rows.value.find(r => r.objectId === props.selectedKey!.objectId && r.channel === props.selectedKey!.channel) : undefined));
const poses: DirectorPoseLookup = (code) => {
  const pose = directorPoseFor(poseMap.value, code);
  return pose ? { bones: pose.bones, ...(pose.lift !== undefined ? { lift: pose.lift } : {}) } : undefined;
};

// 选中的关键帧被撤销、删掉、挪走了：取消选中，免得菜单指着一个不存在的关键帧
watch(selectedKeyframe, (key) => {
  if (props.selectedKey && !key) emit("update:selectedKey", null);
});

function fail(error: DirectorTimelineError) {
  ElMessage.warning(directorTimelineErrorText(error));
}

function edit(fn: (scene: DirectorScene) => { scene: DirectorScene } | DirectorTimelineError): boolean {
  if (props.readonly) return false;
  let failure: DirectorTimelineError | null = null;
  const accepted = store.commit((scene) => {
    const result = fn(scene);
    if ("error" in result) {
      failure = result;
      return scene;
    }
    return result.scene;
  });
  if (failure) fail(failure);
  return accepted;
}

function keyRow(row: DirectorTimelineRow) {
  const frame = store.playhead;
  if (edit(scene => directorKeyObjectAt(scene, row.objectId, row.channel, frame, poses))) {
    emit("update:selectedKey", { objectId: row.objectId, channel: row.channel, frame });
  }
}

function moveKey(row: DirectorTimelineRow, from: number, to: number) {
  if (edit(scene => directorEditTimeline(scene, t => directorMoveKeyframe(t, row.objectId, row.channel, from, to)))) {
    emit("update:selectedKey", { objectId: row.objectId, channel: row.channel, frame: Math.min(duration.value, Math.max(0, Math.round(to))) });
  }
}

function selectKey(row: DirectorTimelineRow, frame: number) {
  emit("update:selectedKey", { objectId: row.objectId, channel: row.channel, frame });
  emit("seek", frame);
}

function setInterpolation(interpolation: DirectorInterpolation) {
  const key = props.selectedKey;
  if (!key) return;
  edit(scene => directorEditTimeline(scene, t => directorSetInterpolation(t, key.objectId, key.channel, key.frame, interpolation)));
}

function deleteKey() {
  const key = props.selectedKey;
  if (!key) return;
  if (edit(scene => directorEditTimeline(scene, t => directorDeleteKeyframe(t, key.objectId, key.channel, key.frame)))) {
    emit("update:selectedKey", null);
  }
}

/** 改总时长（秒，1~15）：缩短时有关键帧落在外面就拒绝并说出有几个（DR-F-123）。被拒时输入框回到原值。 */
function setDuration(seconds: number) {
  const frames = Math.min(DIRECTOR_MAX_DURATION_FRAMES, Math.max(DIRECTOR_MIN_DURATION_FRAMES, Math.round(seconds * DIRECTOR_FPS)));
  if (frames < duration.value) {
    const check = directorCanShrink(timeline.value, frames);
    if (!check.ok) {
      ElMessage.warning(directorShrinkRejectedText(check.outside, frames));
      return;
    }
  }
  edit(scene => ({ scene: directorSetTimelineSettings(scene, { durationFrames: frames }) }));
}

function toggleLoop() {
  edit(scene => ({ scene: directorSetTimelineSettings(scene, { loop: !timeline.value.loop }) }));
}

function frameFromRuler(event: PointerEvent): number {
  const rect = ruler.value?.getBoundingClientRect();
  return rect ? directorFrameAtX(event.clientX - rect.left, zoom.value, duration.value) : 0;
}

function onRulerDown(event: PointerEvent) {
  if (event.button !== 0) return;
  (event.currentTarget as HTMLElement).setPointerCapture(event.pointerId);
  scrubbing = event.pointerId;
  emit("seek", frameFromRuler(event));
}

function onRulerMove(event: PointerEvent) {
  if (scrubbing === event.pointerId) emit("seek", frameFromRuler(event));
}

function onRulerUp(event: PointerEvent) {
  if (scrubbing === event.pointerId) scrubbing = null;
}

onMounted(async () => {
  try {
    poseMap.value = await directorLoadPoses();
  } catch {
    // 姿势库取不到时姿势通道的关键帧按「无预设」记录；位置、机位不受影响
  }
});
</script>

<template>
  <section class="director-timeline" :class="{ collapsed: !open }" aria-label="时间轴" data-testid="timeline">
    <header class="timeline-head">
      <button
        type="button"
        class="dr-btn"
        :title="store.playing ? '暂停（空格）' : '播放（空格）'"
        :aria-label="store.playing ? '暂停' : '播放'"
        data-testid="timeline-play"
        @click="store.playing ? emit('pause') : emit('play')"
      >
        <Pause v-if="store.playing" :size="14" /><Play v-else :size="14" />
      </button>
      <button
        type="button"
        class="dr-btn"
        :class="{ primary: timeline.loop }"
        :aria-pressed="timeline.loop"
        :disabled="readonly"
        :title="readonly ? readonlyText : '循环播放'"
        aria-label="循环播放"
        @click="toggleLoop"
      ><Repeat :size="14" /></button>
      <strong class="timeline-now" data-testid="timeline-now">{{ directorFrameTimeText(store.playhead) }}</strong>
      <label class="timeline-duration">时长
        <NumberField :model-value="duration / DIRECTOR_FPS" :min="1" :max="15" :digits="1" :disabled="readonly" label="总时长（秒）" @commit="setDuration" />
        <span class="dr-unit">秒</span>
      </label>
      <div class="dr-seg" role="radiogroup" aria-label="缩放">
        <button v-for="value in DIRECTOR_TIMELINE_ZOOMS" :key="value" type="button" role="radio" :aria-checked="zoom === value" :class="{ active: zoom === value }" @click="zoom = value">{{ value }}×</button>
      </div>
      <template v-if="selectedKey && selectedKeyframe">
        <span class="timeline-key-info">{{ selectedRow?.label ?? "" }} · 第 {{ selectedKey.frame }} 帧</span>
        <div class="dr-seg" role="radiogroup" aria-label="插值方式">
          <button
            v-for="value in (['linear', 'smooth', 'hold'] as const)"
            :key="value"
            type="button"
            role="radio"
            :aria-checked="(selectedKeyframe.interpolation ?? 'linear') === value"
            :class="{ active: (selectedKeyframe.interpolation ?? 'linear') === value }"
            :disabled="readonly"
            @click="setInterpolation(value)"
          >{{ directorInterpolationName(value) }}</button>
        </div>
        <button type="button" class="dr-btn" :disabled="readonly" :title="readonly ? readonlyText : '删除这个关键帧'" aria-label="删除关键帧" @click="deleteKey"><Trash2 :size="13" /></button>
      </template>
      <button type="button" class="dr-btn timeline-toggle" :aria-expanded="open" :title="open ? '收起时间轴' : '展开时间轴'" :aria-label="open ? '收起时间轴' : '展开时间轴'" @click="open = !open">
        <ChevronDown v-if="open" :size="14" /><ChevronUp v-else :size="14" />
      </button>
    </header>

    <div v-if="open" class="timeline-body">
      <div class="timeline-labels">
        <div class="timeline-corner" />
        <div v-for="row in rows" :key="`${row.objectId}:${row.channel}`" class="timeline-label">
          <button type="button" class="timeline-label-name" :title="row.label" @click="emit('select-object', row.objectId)">{{ row.label }}</button>
          <button
            type="button"
            class="timeline-key-button"
            :disabled="readonly"
            :title="readonly ? readonlyText : `在第 ${store.playhead} 帧打关键帧`"
            :aria-label="`${row.label}：在当前帧打关键帧`"
            :data-row="`${row.objectId}:${row.channel}`"
            @click="keyRow(row)"
          ><Diamond :size="12" /></button>
        </div>
        <p v-if="!rows.length" class="timeline-empty">选中机位或角色，在当前帧打关键帧</p>
      </div>
      <div class="timeline-scroll">
        <div class="timeline-content" :style="{ width: `${duration * zoom + 24}px` }">
          <div
            ref="ruler"
            class="timeline-ruler"
            :style="{ width: `${duration * zoom}px` }"
            aria-label="播放头：按下拖动"
            data-testid="timeline-ruler"
            @pointerdown="onRulerDown"
            @pointermove="onRulerMove"
            @pointerup="onRulerUp"
            @pointercancel="onRulerUp"
          >
            <span v-for="tick in ticks" :key="tick.frame" class="timeline-tick" :class="{ major: tick.major }" :style="{ left: `${tick.frame * zoom}px` }">
              <small v-if="tick.label">{{ tick.label }}</small>
            </span>
          </div>
          <div v-for="row in rows" :key="`${row.objectId}:${row.channel}`" class="timeline-row">
            <TimelineTrack
              :row="row"
              :zoom="zoom"
              :duration="duration"
              :selected-frame="selectedKey && selectedKey.objectId === row.objectId && selectedKey.channel === row.channel ? selectedKey.frame : null"
              :readonly="readonly"
              @select="selectKey(row, $event)"
              @move="(from, to) => moveKey(row, from, to)"
            />
          </div>
          <div class="timeline-playhead" :style="{ left: `${store.playhead * zoom}px` }" aria-hidden="true" />
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.director-timeline {
  display: grid;
  grid-template-rows: 36px minmax(0, 1fr);
  height: 184px;
  border-top: 1px solid var(--portal-line);
  background: var(--portal-panel);
  color: var(--portal-text);
  font-size: 12px;
}
.director-timeline.collapsed { height: 36px; }
.timeline-head { display: flex; align-items: center; gap: 8px; padding: 0 10px; min-width: 0; overflow: hidden; }
.timeline-now { min-width: 120px; font-variant-numeric: tabular-nums; }
.timeline-duration { display: flex; align-items: center; gap: 4px; color: var(--portal-text-soft); }
.timeline-duration :deep(input) { width: 52px; }
.timeline-key-info { color: var(--portal-text-soft); white-space: nowrap; }
.timeline-toggle { margin-left: auto; }
.timeline-body { display: grid; grid-template-columns: 168px minmax(0, 1fr); min-height: 0; border-top: 1px solid var(--portal-line); }
.timeline-labels { overflow: hidden; border-right: 1px solid var(--portal-line); }
.timeline-corner { height: 24px; border-bottom: 1px solid var(--portal-line); }
.timeline-label { display: flex; align-items: center; gap: 4px; height: 28px; padding: 0 6px; border-bottom: 1px solid var(--portal-line); }
.timeline-label-name { flex: 1; min-width: 0; overflow: hidden; padding: 0; border: 0; color: var(--portal-text-soft); background: none; font: inherit; text-align: left; text-overflow: ellipsis; white-space: nowrap; cursor: pointer; }
.timeline-key-button { display: grid; place-items: center; width: 22px; height: 22px; padding: 0; border: 1px solid var(--portal-line); border-radius: 4px; color: var(--portal-text-soft); background: transparent; cursor: pointer; }
.timeline-key-button:hover:not(:disabled) { background: var(--portal-hover); }
.timeline-key-button:disabled { opacity: 0.45; cursor: not-allowed; }
.timeline-empty { margin: 8px; color: var(--portal-muted); font-size: 11px; }
.timeline-scroll { position: relative; overflow: auto; }
.timeline-content { position: relative; min-height: 100%; padding-left: 12px; }
.timeline-ruler { position: relative; height: 24px; border-bottom: 1px solid var(--portal-line); cursor: col-resize; touch-action: none; }
.timeline-tick { position: absolute; bottom: 0; width: 1px; height: 6px; background: var(--portal-line-strong); }
.timeline-tick.major { height: 11px; }
.timeline-tick small { position: absolute; left: 3px; bottom: 9px; color: var(--portal-muted); font-size: 10px; white-space: nowrap; }
.timeline-row { position: relative; height: 28px; border-bottom: 1px solid var(--portal-line); }
.timeline-playhead { position: absolute; top: 0; bottom: 0; width: 2px; margin-left: 11px; background: var(--portal-cyan); pointer-events: none; }
</style>
