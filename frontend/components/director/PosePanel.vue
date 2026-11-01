<script setup lang="ts">
/**
 * 姿势页签（DRT-21，DR-F-41~45，dev/03 §9.2~9.3）：选中角色时出现在右侧「姿势」页签。
 *
 * - 姿势库：六类分组、按名称与说明搜索、缩略图（`public/director/poses/*.webp`，静态资源，不受保护）；
 *   点一下即套用（一步撤销），当前姿势高亮；
 * - 「重置姿势」回到自然站立并清空覆盖；「左右镜像」镜像整套姿势（预设 + 覆盖，见 `directorMirrorPose`）；
 * - 关节模式：视口里每个关节一个小球；这里另有一份关节清单（键盘用户点不到视口里的小球），
 *   「重置这个关节」与关节模式下的 Delete 同一个动作（只去掉这根骨骼的覆盖，不删角色）；
 * - 脚底锁定（DR-F-43）：锁着的脚在拖骨盆、拖动角色时留在原地。
 * 改场景一律经 `store.commit`，规则在 `utils/directorPose.ts`。
 */
import { FlipHorizontal2, Footprints, RotateCcw, ScanSearch, Search, Spline } from "@lucide/vue";
import { directorLoadPoses, directorPoseFor, type DirectorPoseData } from "~/director/assets";
import { DIRECTOR_POSE_CATEGORIES, DIRECTOR_POSES } from "~/utils/directorCatalog";
import {
  DIRECTOR_JOINT_BONES,
  directorApplyPosePreset,
  directorJointLabel,
  directorJointTranslatable,
  directorMirrorCharacterPose,
  directorResetPose,
  directorSetFootLock
} from "~/utils/directorPose";
import type { DirectorObject } from "~/utils/directorScene";

const props = defineProps<{ object: DirectorObject; readonly: boolean; readonlyText: string; jointMode: boolean; jointBone: string | null }>();
const emit = defineEmits<{ "toggle-joint-mode": []; "select-joint": [bone: string | null]; "reset-joint": []; "pose-from-image": [] }>();
const store = useDirectorStore();

const query = ref("");
const library = shallowRef<Map<string, DirectorPoseData> | null>(null);
const libraryError = ref("");

type CharacterSub = { posePreset?: string; boneOverrides?: Record<string, unknown>; footLock?: { left?: boolean; right?: boolean } };
const character = computed(() => (props.object.character ?? {}) as CharacterSub);
const current = computed(() => character.value.posePreset ?? "");
const overrideCount = computed(() => Object.keys(character.value.boneOverrides ?? {}).length);
const footLock = computed(() => ({ left: character.value.footLock?.left === true, right: character.value.footLock?.right === true }));
const disabledTitle = computed(() => (props.readonly ? props.readonlyText : ""));
const knownPose = computed(() => DIRECTOR_POSES.some(pose => pose.code === current.value));

const groups = computed(() => {
  const keyword = query.value.trim().toLowerCase();
  return DIRECTOR_POSE_CATEGORIES.map(category => ({
    ...category,
    poses: DIRECTOR_POSES.filter(pose => pose.category === category.code
      && (!keyword || pose.name.toLowerCase().includes(keyword) || pose.description.toLowerCase().includes(keyword) || pose.code.includes(keyword)))
  })).filter(group => group.poses.length);
});

function thumbnailOf(code: string): string {
  return library.value?.get(code)?.thumbnail ?? "";
}

function commit(fn: Parameters<typeof store.commit>[0]) {
  if (props.readonly) return;
  store.commit(fn, { touched: [props.object.id] });
}

function apply(code: string) {
  commit(scene => directorApplyPosePreset(scene, props.object.id, code));
}

function reset() {
  commit(scene => directorResetPose(scene, props.object.id));
}

function mirror() {
  // 镜像要的是「现在这个姿势」的完整骨骼：预设码认不出时引擎按自然站立显示，这里取同一份（directorPoseFor）
  const preset = directorPoseFor(library.value, current.value)?.bones ?? {};
  commit(scene => directorMirrorCharacterPose(scene, props.object.id, preset));
}

function setFoot(side: "left" | "right", on: boolean) {
  commit(scene => directorSetFootLock(scene, props.object.id, side, on));
}

onMounted(async () => {
  try {
    library.value = await directorLoadPoses();
  } catch (cause) {
    libraryError.value = cause instanceof Error ? cause.message : "姿势库加载失败";
  }
});
</script>

<template>
  <section class="dr-section pose-panel" aria-label="姿势">
    <div class="pose-actions">
      <button type="button" class="dr-btn" :disabled="readonly" :title="disabledTitle || '回到自然站立，清掉手动调整'" @click="reset">
        <RotateCcw :size="13" />重置姿势
      </button>
      <button type="button" class="dr-btn" :disabled="readonly" :title="disabledTitle || '左右对调整套姿势'" @click="mirror">
        <FlipHorizontal2 :size="13" />左右镜像
      </button>
      <button
        type="button"
        class="dr-btn"
        :class="{ primary: jointMode }"
        :aria-pressed="jointMode"
        :disabled="readonly"
        :title="disabledTitle || '在视口里逐个关节调整'"
        data-testid="joint-mode"
        @click="emit('toggle-joint-mode')"
      >
        <Spline :size="13" />关节模式
      </button>
      <button
        type="button"
        class="dr-btn"
        :disabled="readonly"
        :title="disabledTitle || '选一张人物照片，角色摆出照片里的姿势（照片只在浏览器里识别，不上传）'"
        @click="emit('pose-from-image')"
      >
        <ScanSearch :size="13" />从图片识别
      </button>
    </div>
    <p v-if="readonly" class="dr-hint">{{ readonlyText }}</p>

    <div v-if="jointMode" class="pose-joints" aria-label="关节模式">
      <p class="dr-hint">
        点视口里的关节小球再拖动：E 转动关节；骨盆和手脚按 W 可以拖着走（手脚用 IK 带动整条胳膊 / 腿）。
        Delete 只把当前关节恢复成姿势里的样子，不会删掉角色。
      </p>
      <div class="dr-field">
        <span>关节</span>
        <select
          class="dr-select"
          :value="jointBone ?? ''"
          aria-label="选中的关节"
          data-testid="joint-select"
          @change="emit('select-joint', ($event.target as HTMLSelectElement).value || null)"
        >
          <option value="">（没有选中）</option>
          <option v-for="joint in DIRECTOR_JOINT_BONES" :key="joint.bone" :value="joint.bone">
            {{ joint.label }}{{ directorJointTranslatable(joint.bone) ? "（可拖动）" : "" }}
          </option>
        </select>
      </div>
      <div v-if="jointBone" class="pose-joint-current">
        <strong>{{ directorJointLabel(jointBone) }}</strong>
        <button type="button" class="dr-btn" :disabled="readonly || !character.boneOverrides?.[jointBone]" :title="character.boneOverrides?.[jointBone] ? '恢复成姿势里的样子（Delete）' : '这个关节没有手动调整'" @click="emit('reset-joint')">
          重置这个关节
        </button>
      </div>
      <div class="pose-feet" role="group" aria-label="脚底锁定">
        <Footprints :size="14" />
        <label class="dr-check"><input type="checkbox" :checked="footLock.left" :disabled="readonly" data-testid="foot-lock-left" @change="setFoot('left', ($event.target as HTMLInputElement).checked)">锁定左脚</label>
        <label class="dr-check"><input type="checkbox" :checked="footLock.right" :disabled="readonly" data-testid="foot-lock-right" @change="setFoot('right', ($event.target as HTMLInputElement).checked)">锁定右脚</label>
      </div>
      <p class="dr-hint">锁着的脚在拖骨盆、拖动角色时留在原地；腿伸直也够不着时会自动解锁。</p>
    </div>

    <p v-if="overrideCount" class="dr-hint">已手动调整 {{ overrideCount }} 根骨骼；换姿势会清掉这些调整（可以撤销）。</p>
    <p v-if="current && !knownPose" class="dr-note">姿势 {{ current }} 暂不支持，先按自然站立显示。</p>

    <label class="pose-search">
      <Search :size="13" />
      <input v-model="query" type="search" class="dr-input" placeholder="搜索姿势（名称或说明）" aria-label="搜索姿势">
    </label>
    <p v-if="libraryError" class="dr-note">{{ libraryError }}：缩略图暂时显示不了，姿势照样能用。</p>
    <p v-if="!groups.length" class="dr-empty">没有匹配「{{ query }}」的姿势</p>
    <div v-for="group in groups" :key="group.code" class="pose-group">
      <h4>{{ group.name }}</h4>
      <div class="pose-grid" role="listbox" :aria-label="group.name">
        <button
          v-for="pose in group.poses"
          :key="pose.code"
          type="button"
          role="option"
          class="pose-tile"
          :class="{ active: pose.code === current }"
          :aria-selected="pose.code === current"
          :disabled="readonly"
          :title="readonly ? readonlyText : pose.description"
          :data-pose="pose.code"
          @click="apply(pose.code)"
        >
          <img v-if="thumbnailOf(pose.code)" :src="thumbnailOf(pose.code)" alt="" loading="lazy" draggable="false">
          <span v-else class="pose-tile-placeholder" aria-hidden="true" />
          <span class="pose-tile-name">{{ pose.name }}</span>
        </button>
      </div>
    </div>
  </section>
</template>

<style scoped>
.pose-panel { display: grid; gap: 10px; }
.pose-actions { display: flex; flex-wrap: wrap; gap: 6px; }
.pose-joints { display: grid; gap: 8px; padding: 8px; border: 1px solid var(--portal-line); border-radius: 6px; background: var(--portal-surface-2); }
.pose-joint-current { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.pose-feet { display: flex; align-items: center; gap: 12px; color: var(--portal-text-soft); }
.pose-search { display: flex; align-items: center; gap: 6px; color: var(--portal-muted); }
.pose-search input { flex: 1; }
.pose-group h4 { margin: 4px 0 6px; color: var(--portal-muted); font-size: 11px; font-weight: 500; }
.pose-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 6px; }
.pose-tile {
  display: grid;
  gap: 2px;
  justify-items: center;
  padding: 4px 2px 6px;
  border: 1px solid var(--portal-line);
  border-radius: 6px;
  color: var(--portal-text-soft);
  background: var(--portal-surface-2);
  font-size: 11px;
  cursor: pointer;
}
.pose-tile img,
.pose-tile-placeholder { width: 100%; aspect-ratio: 1; object-fit: contain; }
.pose-tile:hover:not(:disabled) { background: var(--portal-hover); }
.pose-tile.active { border-color: var(--portal-cyan-line); color: var(--portal-text); background: var(--portal-hover); }
.pose-tile:disabled { opacity: 0.55; cursor: not-allowed; }
.pose-tile-name { max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
