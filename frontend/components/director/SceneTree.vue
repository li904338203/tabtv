<script setup lang="ts">
/**
 * 场景树（DRT-15，DR-F-10 / 11 / 13）：按机位 / 角色 / 群众 / 道具分组，按名称搜索；
 * 双击行内改名（≤ 20 字）、显示 / 隐藏、锁定 / 解锁、复制、删除；输出机位带「输出」角标；「添加」菜单。
 *
 * - 点行 → 选中（引擎挂手柄）；视口里点选的对象在这里高亮（`selectedId` 由外壳给）；
 * - 锁定、隐藏的对象在这里照样能选中（视口里点不中），属性面板可以改回来；
 * - 只读：能选中、能看，改名 / 开关 / 添加 / 复制 / 删除都置灰并说明原因（DR-F-156）；
 * - 新对象的位置由外壳给（`placement`）：角色、道具放在视口中心看向的地面上，机位用当前导演视角。
 */
import { ElMessage } from "element-plus";
import { Box, Copy, Eye, EyeOff, Lock, LockOpen, Plus, Search, Trash2, User, Users, Video } from "@lucide/vue";
import { DIRECTOR_LIMITS, DIRECTOR_MODELS, DIRECTOR_PROP_SHAPES } from "~/utils/directorCatalog";
import {
  directorAddObject,
  directorDuplicateObject,
  directorLimitText,
  directorPatchObject,
  directorRemoveObject,
  directorRenameObject,
  directorSetLocked,
  directorSetTransform,
  directorSetVisible,
  type DirectorLimitError,
  type DirectorObject,
  type DirectorObjectKind,
  type DirectorScene
} from "~/utils/directorScene";
import type { DirectorPlacement } from "~/utils/directorStudio";

const props = defineProps<{ selectedId: string | null; readonly: boolean; readonlyText: string; placement: () => DirectorPlacement }>();
const emit = defineEmits<{ select: [id: string | null] }>();
const store = useDirectorStore();

const GROUPS: Array<{ kind: DirectorObjectKind; name: string; icon: unknown }> = [
  { kind: "camera", name: "机位", icon: Video },
  { kind: "character", name: "角色", icon: User },
  { kind: "crowd", name: "群众", icon: Users },
  { kind: "prop", name: "道具", icon: Box }
];
const SHAPE_NAMES: Record<string, string> = { box: "方块", cylinder: "圆柱", sphere: "球", plane: "平面" };

const keyword = ref("");
const addOpen = ref(false);
const renamingId = ref<string | null>(null);
const renameDraft = ref("");
const renameInput = ref<HTMLInputElement[]>([]);

const objects = computed(() => store.scene?.objects ?? []);
const groups = computed(() => {
  const q = keyword.value.trim().toLowerCase();
  return GROUPS.map(group => ({
    ...group,
    items: objects.value.filter(o => o.kind === group.kind && (!q || o.name.toLowerCase().includes(q)))
  })).filter(group => group.items.length || !q);
});
const disabledTitle = computed(() => (props.readonly ? props.readonlyText : ""));

/** 上限的说法只有一份（`directorLimitText`，DRT-22）：场景树、群众面板说同一句话。 */
function limitText(error: DirectorLimitError): string {
  return directorLimitText(error);
}

/**
 * 新加 / 复制出来的对象，等外壳把场景同步给引擎之后再选中。
 * 外壳靠 `watch(() => store.scene)`（pre-flush，异步）调 `setScene`；commit 之后立刻选中的话，
 * 引擎还不认识这个 id，`select` 把它当成不存在丢掉 —— 场景树高亮了、属性面板也出来了，
 * 视口里却没有手柄和选中框，刚加的对象拖不动（DRT-22 自查发现，所有类型都受影响）。
 */
function selectAfterSync(id: string) {
  void nextTick(() => emit("select", id));
}

/** 加一个对象并选中它。超上限时说明（DR-F-13），不加。 */
function add(kind: DirectorObjectKind, sub?: Record<string, unknown>) {
  addOpen.value = false;
  if (props.readonly || !store.scene) return;
  const place = props.placement();
  // ⚠️ 结果放在对象里带出来：在回调里给 `let` 赋值，TS 的控制流分析看不见，后面会被收窄成 null
  const outcome: { id: string; limit: DirectorLimitError | null } = { id: "", limit: null };
  store.commit((scene: DirectorScene) => {
    const result = directorAddObject(scene, kind, kind === "camera" ? undefined : place.ground ?? undefined);
    if ("error" in result) {
      outcome.limit = result;
      return scene;
    }
    const id = result.object.id;
    outcome.id = id;
    let next = result.scene;
    if (kind === "camera" && place.view) next = directorSetTransform(next, id, place.view);
    if (sub && (kind === "character" || kind === "prop")) next = directorPatchObject(next, id, kind, sub);
    return next;
  });
  if (outcome.limit) ElMessage.warning(limitText(outcome.limit));
  else if (outcome.id) selectAfterSync(outcome.id);
}

function addCharacter(code: string) {
  const model = DIRECTOR_MODELS.find(item => item.code === code);
  add("character", model ? { model: model.code, height: model.heightM } : undefined);
}

function duplicate(object: DirectorObject) {
  if (props.readonly) return;
  const outcome: { id: string; limit: DirectorLimitError | null } = { id: "", limit: null };
  store.commit((scene: DirectorScene) => {
    const result = directorDuplicateObject(scene, object.id);
    if ("error" in result) {
      if (result.error === "limit") outcome.limit = result as DirectorLimitError;
      return scene;
    }
    outcome.id = result.object.id;
    return result.scene;
  });
  if (outcome.limit) ElMessage.warning(limitText(outcome.limit));
  else if (outcome.id) selectAfterSync(outcome.id);
}

function remove(object: DirectorObject) {
  if (props.readonly) return;
  if (store.commit(scene => directorRemoveObject(scene, object.id)) && props.selectedId === object.id) emit("select", null);
}

function toggleVisible(object: DirectorObject) {
  if (!props.readonly) store.commit(scene => directorSetVisible(scene, object.id, object.visible === false), { touched: [object.id] });
}

function toggleLocked(object: DirectorObject) {
  if (!props.readonly) store.commit(scene => directorSetLocked(scene, object.id, object.locked !== true), { touched: [object.id] });
}

async function startRename(object: DirectorObject) {
  if (props.readonly) return;
  renamingId.value = object.id;
  renameDraft.value = object.name;
  await nextTick();
  renameInput.value[0]?.select();
}

function commitRename() {
  const id = renamingId.value;
  renamingId.value = null;
  const name = Array.from(renameDraft.value.trim()).slice(0, DIRECTOR_LIMITS.maxNameChars).join("");
  if (!id || !name) return;
  store.commit(scene => directorRenameObject(scene, id, name), { touched: [id] });
}
</script>

<template>
  <aside class="scene-tree" aria-label="场景对象">
    <div class="scene-tree-head">
      <label class="scene-tree-search"><Search :size="13" /><input v-model="keyword" type="search" placeholder="搜索对象" aria-label="搜索对象"></label>
      <div class="scene-tree-add">
        <button type="button" class="dr-btn" :disabled="readonly" :title="disabledTitle || '添加对象'" aria-label="添加对象" :aria-expanded="addOpen" @click="addOpen = !addOpen">
          <Plus :size="14" />添加
        </button>
        <div v-if="addOpen" class="scene-tree-menu" role="menu">
          <strong>素模</strong>
          <button v-for="model in DIRECTOR_MODELS" :key="model.code" type="button" role="menuitem" @click="addCharacter(model.code)"><User :size="13" />{{ model.name }}</button>
          <strong>机位</strong>
          <button type="button" role="menuitem" title="在当前导演视角的位置与朝向" @click="add('camera')"><Video :size="13" />机位</button>
          <strong>道具</strong>
          <button v-for="shape in DIRECTOR_PROP_SHAPES" :key="shape" type="button" role="menuitem" @click="add('prop', { shape })"><Box :size="13" />{{ SHAPE_NAMES[shape] ?? shape }}</button>
          <strong>群众</strong>
          <button type="button" role="menuitem" title="一个对象摆出一群人（默认 3 × 3，可在属性里改）" @click="add('crowd')"><Users :size="13" />群众阵列</button>
        </div>
      </div>
    </div>
    <div class="scene-tree-groups">
      <section v-for="group in groups" :key="group.kind" class="scene-tree-group">
        <h3><component :is="group.icon" :size="13" />{{ group.name }}<small>{{ group.items.length }}</small></h3>
        <p v-if="!group.items.length" class="dr-empty">还没有{{ group.name }}</p>
        <ul>
          <li
            v-for="item in group.items"
            :key="item.id"
            :class="{ selected: item.id === selectedId, hidden: item.visible === false }"
            :data-object-id="item.id"
            :aria-selected="item.id === selectedId"
            role="option"
            tabindex="0"
            @click="emit('select', item.id)"
            @keydown.enter.self="emit('select', item.id)"
          >
            <input
              v-if="renamingId === item.id"
              ref="renameInput"
              v-model="renameDraft"
              class="dr-input"
              :maxlength="DIRECTOR_LIMITS.maxNameChars"
              aria-label="对象名称"
              @click.stop
              @blur="commitRename"
              @keydown.enter.prevent="commitRename"
              @keydown.esc.stop.prevent="renamingId = null"
            >
            <span v-else class="scene-tree-name" :title="readonly ? item.name : `${item.name}（双击改名）`" @dblclick.stop="startRename(item)">{{ item.name }}</span>
            <small v-if="item.kind === 'camera' && store.scene?.outputCameraId === item.id" class="scene-tree-badge">输出</small>
            <small v-if="item.source === 'ai'" class="scene-tree-badge ai" title="AI 搭景放的，改动后变为你的">AI</small>
            <span class="scene-tree-actions" @click.stop>
              <button type="button" :disabled="readonly" :title="disabledTitle || (item.visible === false ? '显示' : '隐藏')" :aria-label="item.visible === false ? '显示' : '隐藏'" @click="toggleVisible(item)">
                <EyeOff v-if="item.visible === false" :size="13" /><Eye v-else :size="13" />
              </button>
              <button type="button" :disabled="readonly" :title="disabledTitle || (item.locked ? '解锁' : '锁定（视口里不能点选、拖动）')" :aria-label="item.locked ? '解锁' : '锁定'" @click="toggleLocked(item)">
                <Lock v-if="item.locked" :size="13" /><LockOpen v-else :size="13" />
              </button>
              <button type="button" :disabled="readonly" :title="disabledTitle || '复制'" aria-label="复制" @click="duplicate(item)"><Copy :size="13" /></button>
              <button type="button" :disabled="readonly" :title="disabledTitle || '删除'" aria-label="删除" @click="remove(item)"><Trash2 :size="13" /></button>
            </span>
          </li>
        </ul>
      </section>
    </div>
  </aside>
</template>

<style scoped>
.scene-tree { display: grid; grid-template-rows: auto 1fr; min-height: 0; border-right: 1px solid var(--portal-line); background: var(--portal-panel); color: var(--portal-text); font-size: 12px; }
.scene-tree-head { display: flex; gap: 6px; padding: 10px; border-bottom: 1px solid var(--portal-line); }
.scene-tree-search { display: flex; flex: 1 1 auto; align-items: center; gap: 4px; min-width: 0; padding: 0 6px; border: 1px solid var(--portal-line); border-radius: 5px; color: var(--portal-muted); background: var(--portal-input-bg); }
.scene-tree-search input { width: 100%; min-width: 0; height: 26px; border: 0; outline: 0; color: var(--portal-text); background: transparent; font-size: 12px; }
.scene-tree-add { position: relative; }
.scene-tree-menu { position: absolute; top: 32px; right: 0; z-index: 20; display: grid; gap: 2px; width: 160px; padding: 6px; border: 1px solid var(--portal-line-strong); border-radius: 8px; background: var(--portal-elevated); box-shadow: 0 10px 28px var(--portal-shadow); }
.scene-tree-menu strong { padding: 4px 6px 2px; color: var(--portal-muted); font-size: 11px; font-weight: 600; }
.scene-tree-menu button { display: flex; align-items: center; gap: 6px; height: 28px; padding: 0 8px; border: 0; border-radius: 5px; color: var(--portal-text); background: transparent; font-size: 12px; text-align: left; cursor: pointer; }
.scene-tree-menu button:hover { background: var(--portal-hover); }
.scene-tree-groups { min-height: 0; overflow-y: auto; padding: 6px 0 12px; }
.scene-tree-group h3 { display: flex; align-items: center; gap: 6px; margin: 8px 10px 4px; color: var(--portal-muted); font-size: 11px; font-weight: 600; }
.scene-tree-group h3 small { margin-left: auto; font-weight: 400; }
.scene-tree-group .dr-empty { padding: 4px 10px; text-align: left; }
.scene-tree-group ul { margin: 0; padding: 0; list-style: none; }
.scene-tree-group li { display: flex; align-items: center; gap: 6px; min-height: 30px; padding: 0 8px 0 14px; cursor: pointer; }
.scene-tree-group li:hover { background: var(--portal-hover); }
.scene-tree-group li.selected { background: var(--portal-active); }
.scene-tree-group li.hidden .scene-tree-name { color: var(--portal-muted); text-decoration: line-through; }
.scene-tree-group li:focus-visible { outline: 1px solid var(--portal-cyan-line); outline-offset: -1px; }
.scene-tree-name { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.scene-tree-badge { padding: 0 5px; border-radius: 999px; color: var(--portal-on-accent); background: var(--portal-text); font-size: 10px; }
.scene-tree-badge.ai { color: var(--portal-text); background: var(--portal-cyan-soft); }
.scene-tree-actions { display: flex; gap: 1px; opacity: 0; }
.scene-tree-group li:hover .scene-tree-actions,
.scene-tree-group li.selected .scene-tree-actions,
.scene-tree-group li:focus-within .scene-tree-actions { opacity: 1; }
.scene-tree-actions button { display: grid; place-items: center; width: 22px; height: 22px; padding: 0; border: 0; border-radius: 4px; color: var(--portal-text-soft); background: transparent; cursor: pointer; }
.scene-tree-actions button:hover:not(:disabled) { color: var(--portal-text); background: var(--portal-hover); }
.scene-tree-actions button:disabled { opacity: 0.4; cursor: not-allowed; }
</style>
