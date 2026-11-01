<script setup lang="ts">
import type { components as ApiComponents } from "@tabtv/api-client";
import { projectCoverUrl } from "~/utils/authorizedMedia";
import { PROJECT_KIND_OPTIONS, projectBadgeVariant, projectCanvasPath, projectKindEmptyText, projectOpenPath, readProjectKind, writeProjectKind, type ProjectKind } from "~/utils/projectKind";
import { ChevronLeft, FolderClosed, FolderPlus, MoreHorizontal, Pencil, Plus, Search, SquareArrowOutUpRight, Trash2, Video } from "@lucide/vue";
import { ElMessage, ElMessageBox } from "element-plus";

// 模板里不直接调 navigateTo：自动导入在模板上下文里 vue-tsc 解析不到（运行时没问题，但门禁会红）。
const go = navigateTo

definePageMeta({ layout: "platform", middleware: "auth" });

// ⚠️ 类型取自**生成的新契约**，不用 `types/legacy`（老 Java 契约的抄本）：
//    老类型里没有 `displayCoverMediaId`，分页是 `{list,total}` —— 照老类型写，
//    读对了新字段反而编译不过，读错了字段却能编译通过。
// 列表项 = Project + `agent`（最近活跃的 Agent 会话，没有为 null；AGT-45）
type ProjectSummary = ApiComponents["schemas"]["ProjectListItem"];
type ProjectPage = ApiComponents["schemas"]["ProjectPage"];
type ProjectResponse = ApiComponents["schemas"]["Project"];
type Folder = ApiComponents["schemas"]["Folder"];

const PAGE_SIZE = 12;

const route = useRoute();
const router = useRouter();
const folders = ref<Folder[]>([]);
const creating = ref(false);
/** 文件夹卡片的「⋯」菜单（项目卡片的菜单是 ProjectCardMenu，自己管开合）。 */
const menuOpenId = ref("");
/**
 * 项目卡片的「⋯」操作 —— 与工作台、AI 视频页**同一份**（useProjectActions）。
 * ⚠️ 此前这一页自己写着一份菜单，而另两页要加同样的菜单；照抄两份的话，
 *    确认文案、报错文案、调哪个接口会散成三份，改一处另两处不报错。
 */
const projectActions = useProjectActions();
/** 就地重命名：记住正在编辑哪张卡（项目用 id，文件夹用 f-id），以及草稿值。 */
const editingId = ref("");
const editDraft = ref("");
const editInput = ref<HTMLInputElement>();
const keyword = ref(typeof route.query.q === "string" ? route.query.q : "");
/** 当前所在文件夹；空串表示根目录。根目录下同时显示文件夹卡与未分类项目。 */
const currentFolder = ref(typeof route.query.folder === "string" ? route.query.folder : "");
/**
 * 类型筛选「全部 ｜ 画布 ｜ Agent ｜ 营销」（AGT-45、MKT-F-60）：**服务端筛选**（`kind=`），不在客户端过滤 ——
 * 客户端过滤会让一页只剩两三张卡、甚至空页还能继续翻，滚动加载就乱了。
 * 记住上次的选择；挂载后才读本地存储（SSR 时没有 localStorage）。
 */
const kind = ref<ProjectKind>("all");
// 解构出来才能在模板里自动解包；嵌套在对象里的 ref 模板不会解
const { urls: coverUrls, appendAll: loadCovers } = useAuthenticatedMedia();


const currentFolderName = computed(() =>
  folders.value.find(item => item.id === currentFolder.value)?.name ?? "");

/**
 * 文件夹只在根目录、且没有搜索时露出。
 *
 * <p>单层结构下文件夹里不会再有文件夹；搜索时用户找的是项目，混进文件夹卡只会碍事。
 */
const showFolders = computed(() => !currentFolder.value && !keyword.value.trim() && kind.value === "all");

/**
 * 根目录下是否跨文件夹列项目：搜索或按类型筛选时是（用户找的是项目，放在文件夹里的 Agent 项目也要找得到），
 * 否则只列未分类的（文件夹里的由文件夹卡代表）。
 */
const crossFolder = computed(() => Boolean(keyword.value.trim()) || kind.value !== "all");

/**
 * 列表加载失败时的那句话。
 *
 * ⚠️ 此前 `fetchPage` 是 `catch { return null }` —— 失败被吞掉，界面显示成
 *    「没有项目」。全部项目页正是这样**一直是空的**：请求带着老系统的
 *    `folderId=unfiled`，新后端回 400「folderId 非法」，而用户看到的是一个
 *    空列表，谁也想不到是请求错了。**「还没有」与「取失败了」必须分开说**。
 */
const loadError = ref("");

const list = useInfiniteList<ProjectSummary>({
  pageSize: PAGE_SIZE,
  fetchPage: async (_next, cursor) => {
    // ⚠️ 新后端是**游标分页**（`limit` + `cursor`）。老的 `page`/`size` 它会忽略 ——
    //    于是第 2 页原样返回第 1 页，滚动加载出一排重复的卡片，而且不报错。
    const params = new URLSearchParams({ limit: String(PAGE_SIZE) });
    if (cursor) params.set("cursor", cursor);
    if (currentFolder.value) params.set("folderId", currentFolder.value);
    // 根目录只列未分类项目：文件夹里的项目由文件夹卡代表，两边都列会重复。
    // 但搜索要跨文件夹找得到，这时就不能再限制在未分类里。
    // ⚠️ 「未分类」在新后端是 **`none`**，不是老系统的 `unfiled`（传错 → 400）。
    else if (!crossFolder.value) params.set("folderId", "none");
    if (keyword.value.trim()) params.set("keyword", keyword.value.trim());
    if (kind.value !== "all") params.set("kind", kind.value);
    try {
      loadError.value = "";
      return await useApi()<ProjectPage>(`/projects?${params}`);
    } catch (cause) {
      loadError.value = (cause as { message?: string })?.message || "项目列表加载失败";
      return null;
    }
  },
  // 带 ?w=320 请求缩略图：滚动加载下 blob 不释放，原图会把标签页撑爆
  onPage: items => loadCovers(
    items.map(item => ({ key: item.id ?? "", url: projectCoverUrl(item) }))
  )
});

function syncQuery() {
  void router.replace({
    query: {
      ...(currentFolder.value ? { folder: currentFolder.value } : {}),
      ...(keyword.value.trim() ? { q: keyword.value.trim() } : {})
    }
  });
}

async function loadFolders() {
  try {
    folders.value = await useApi()<Folder[]>("/folders");
  } catch {
    folders.value = [];
  }
}

function openFolder(folder: Folder) {
  closeMenu();
  currentFolder.value = folder.id ?? "";
  syncQuery();
  void list.reset();
}

function selectKind(next: ProjectKind) {
  if (next === kind.value) return;
  kind.value = next;
  writeProjectKind(window.localStorage, next);
  void list.reset();
}

function backToRoot() {
  currentFolder.value = "";
  syncQuery();
  void list.reset();
}

let searchTimer: ReturnType<typeof setTimeout> | undefined;
function onSearch() {
  // 防抖：每敲一个字就查一次，翻页状态和封面 blob 会被反复重建
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    syncQuery();
    void list.reset();
  }, 300);
}

async function createProject() {
  if (creating.value) return;
  creating.value = true;
  try {
    const response = await useApi()<ProjectResponse>("/projects", {
      method: "POST",
      body: { name: "未命名" }
    });
    if (!response.id) return;
    // 在文件夹里新建就直接归到该文件夹，否则用户还得手动移一次
    if (currentFolder.value) {
      await useApi()(`/projects/${response.id}`, {
        method: "PATCH",
        body: { folderId: currentFolder.value }
      });
    }
    await navigateTo(`/video/canvas/${response.id}`);
  } finally {
    creating.value = false;
  }
}

/**
 * 新建文件夹。
 *
 * <p>不弹输入框：直接建一个「未命名文件夹 N」再就地改名 —— 与重命名同一套交互，
 * 也省掉「先想名字才能创建」这一步。名字带序号是因为同名会被唯一索引拒绝。
 */
async function createFolder() {
  const base = "未命名文件夹";
  const taken = new Set(folders.value.map(item => item.name));
  let name = base;
  for (let index = 1; taken.has(name); index += 1) name = `${base}${index}`;
  try {
    const created = await useApi()<Folder>("/folders", { method: "POST", body: { name } });
    await loadFolders();
    if (created.id) await startEdit(`f-${created.id}`, name);
  } catch {
    ElMessage.error("创建文件夹失败");
  }
}

async function removeFolder(folder: Folder) {
  menuOpenId.value = "";
  try {
    await ElMessageBox.confirm(
      `删除「${folder.name}」后，里面的 ${folder.projectCount} 个项目会移回未分类，项目本身不会被删除。`,
      "删除文件夹",
      { confirmButtonText: "删除", cancelButtonText: "取消", type: "warning" }
    );
  } catch {
    return;
  }
  try {
    await useApi()(`/folders/${folder.id}`, { method: "DELETE" });
    await Promise.all([loadFolders(), list.reset()]);
  } catch {
    ElMessage.error("删除文件夹失败");
  }
}

/**
 * 就地重命名。
 *
 * <p>用输入框而不是弹窗：改个名字弹一个模态框太重，而且弹窗会盖住列表，
 * 用户看不到自己正在改的是哪一张卡。
 */
async function startEdit(key: string, name: string) {
  closeMenu();
  editingId.value = key;
  editDraft.value = name;
  await nextTick();
  // 全选而不是把光标放末尾：默认名「未命名」几乎总是要整个替换掉
  editInput.value?.select();
}

function cancelEdit() {
  editingId.value = "";
}

/** 文件夹改名（项目改名走 ProjectCardTitle + useProjectActions）。 */
async function commitFolderEdit(id: string, previous: string) {
  if (!editingId.value) return;
  const next = editDraft.value.trim();
  editingId.value = "";
  if (!next || next === previous) return;
  if (next.length > 60) {
    ElMessage.error("名称不能超过 60 个字符");
    return;
  }
  try {
    await useApi()(`/folders/${id}`, { method: "PUT", body: { name: next } });
    await loadFolders();
  } catch {
    ElMessage.error("文件夹已存在或重命名失败");
  }
}

async function renameProject(project: ProjectSummary, next: string) {
  if (await projectActions.rename(project, next)) await list.reset();
}

async function duplicateProject(project: ProjectSummary) {
  if (!(await projectActions.duplicate(project))) return;
  syncQuery();
  await list.reset();
}

/** 移动/删除都会改变文件夹里的项目数，文件夹卡片要跟着刷新。 */
async function moveProject(project: ProjectSummary, folderId: string) {
  if (await projectActions.moveToFolder(project, folderId)) {
    await Promise.all([loadFolders(), list.reset()]);
  }
}

async function removeProject(project: ProjectSummary) {
  if (await projectActions.remove(project)) await Promise.all([loadFolders(), list.reset()]);
}

function closeMenu() {
  menuOpenId.value = "";
}

onMounted(() => {
  kind.value = readProjectKind(window.localStorage);
  void loadFolders();
  void list.reset();
  // 点空白处关菜单：只靠再点一次触发器关，用户会觉得菜单「粘住了」
  document.addEventListener("click", closeMenu);
});
onBeforeUnmount(() => {
  document.removeEventListener("click", closeMenu);
  clearTimeout(searchTimer);
});
</script>

<template>
  <main class="portal-page">
    <ProductAvailabilityGate code="video">
      <header class="library-head">
        <div class="library-crumb">
          <button v-if="currentFolder" type="button" class="crumb-back" @click="backToRoot">
            <ChevronLeft :size="15" />返回
          </button>
          <NuxtLink v-else to="/video" class="crumb-back"><ChevronLeft :size="15" />返回</NuxtLink>
          <span class="crumb-divider" />
          <h1>
            <button v-if="currentFolder" type="button" class="crumb-link" @click="backToRoot">全部项目</button>
            <template v-else>全部项目</template>
            <template v-if="currentFolder"> / <strong>{{ currentFolderName }}</strong></template>
          </h1>
        </div>
        <div class="library-tools">
          <div class="work-filter" role="tablist" aria-label="项目类型">
            <button
              v-for="option in PROJECT_KIND_OPTIONS"
              :key="option.code"
              type="button"
              role="tab"
              :aria-selected="kind === option.code"
              :class="{ active: kind === option.code }"
              @click="selectKind(option.code)"
            >{{ option.name }}</button>
          </div>
          <label class="portal-search">
            <Search :size="16" />
            <input v-model="keyword" type="search" placeholder="搜索项目" @input="onSearch">
          </label>
          <NuxtLink to="/video/trash" class="library-command"><Trash2 :size="15" />回收站</NuxtLink>
          <button v-if="currentFolder" type="button" class="library-command" :disabled="creating" @click="createProject">
            <Plus :size="15" />新建项目
          </button>
          <button v-else type="button" class="library-command" @click="createFolder">
            <FolderPlus :size="15" />新建文件夹
          </button>
        </div>
      </header>

      <section class="library-grid">
        <!-- 开始创作固定排第一，位置稳定，用户不用找 -->
        <article class="library-card">
          <button type="button" class="library-thumb create" :disabled="creating" @click="createProject">
            <Plus :size="26" /><span>开始创作</span>
          </button>
          <div class="library-meta">
            <div class="library-meta-text"><h3>创建新的视频项目</h3></div>
          </div>
        </article>

        <template v-if="showFolders">
          <article v-for="folder in folders" :key="`f-${folder.id}`" class="library-card">
            <button type="button" class="library-thumb folder" @click="openFolder(folder)">
              <FolderClosed :size="34" />
            </button>
            <div class="library-meta">
              <div class="library-meta-text">
                <input
                  v-if="editingId === `f-${folder.id}`"
                  ref="editInput"
                  v-model="editDraft"
                  class="library-name-input"
                  maxlength="60"
                  aria-label="文件夹名称"
                  @click.stop
                  @keyup.enter="commitFolderEdit(folder.id ?? '', folder.name ?? '')"
                  @keyup.esc="cancelEdit"
                  @blur="commitFolderEdit(folder.id ?? '', folder.name ?? '')"
                >
                <h3 v-else title="双击可重命名" @dblclick.stop="startEdit(`f-${folder.id}`, folder.name ?? '')">
                  {{ folder.name }}
                </h3>
                <!-- ⚠️ 新后端的文件夹**只有 createdAt**（老后端是 updatedAt）。读 updatedAt 的话卡片上的日期恒为空，不报错。 -->
                <p>{{ folder.createdAt ? new Date(folder.createdAt).toLocaleDateString('zh-CN') : '' }}</p>
              </div>
              <div
                class="library-actions"
                @click.stop
                @mouseenter="menuOpenId = `f-${folder.id}`"
                @mouseleave="closeMenu"
              >
                <button
                  type="button"
                  class="library-actions-trigger"
                  :title="`${folder.name} 的操作`"
                  :aria-label="`${folder.name} 的操作`"
                  @click="menuOpenId = menuOpenId === `f-${folder.id}` ? '' : `f-${folder.id}`"
                ><MoreHorizontal :size="16" /></button>
                <div v-if="menuOpenId === `f-${folder.id}`" class="library-actions-menu" role="menu">
                  <button type="button" role="menuitem" @click="openFolder(folder)">
                    <SquareArrowOutUpRight :size="15" />打开
                  </button>
                  <button type="button" role="menuitem" @click="startEdit(`f-${folder.id}`, folder.name ?? '')">
                    <Pencil :size="15" />重命名
                  </button>
                  <button type="button" role="menuitem" class="danger" @click="removeFolder(folder)">
                    <Trash2 :size="15" />删除文件夹
                  </button>
                </div>
              </div>
            </div>
          </article>
        </template>

        <article v-for="project in list.items.value" :key="project.id" class="library-card">
          <!-- Agent 项目点开进对话页的最近会话；普通项目进画布（AGT-45）。营销项目同时是 Agent 项目，只显示「营销」角标（MKT-F-61） -->
          <button type="button" class="library-thumb" @click="go(projectOpenPath(project))">
            <ProjectAgentBadge v-if="projectBadgeVariant(project)" :variant="projectBadgeVariant(project) ?? undefined" />
            <img v-if="coverUrls[project.id ?? '']" :src="coverUrls[project.id ?? '']" :alt="project.name">
            <Video v-else :size="26" />
          </button>
          <div class="library-meta">
            <div class="library-meta-text">
              <ProjectCardTitle
                :name="project.name ?? ''"
                :editing="editingId === project.id"
                @update:editing="value => editingId = value ? (project.id ?? '') : ''"
                @commit="value => renameProject(project, value)"
              />
              <ProjectAgentSubtitle v-if="project.agent" :agent="project.agent" />
              <p>{{ project.updatedAt ? new Date(project.updatedAt).toLocaleDateString('zh-CN') : '' }}</p>
            </div>
            <!-- 与工作台、AI 视频页的最近项目是**同一个**菜单组件 -->
            <ProjectCardMenu
              :project="project"
              :folders="folders"
              :agent="Boolean(project.agent)"
              @open="go(projectOpenPath(project))"
              @open-canvas="go(projectCanvasPath(project))"
              @rename="closeMenu(); editingId = project.id ?? ''"
              @duplicate="duplicateProject(project)"
              @move="folderId => moveProject(project, folderId)"
              @remove="removeProject(project)"
            />
          </div>
        </article>

        <div :ref="el => list.sentinel.value = (el as HTMLElement)" class="showcase-sentinel" />
        <!--
          ⚠️ 「取失败了」与「还没有」分开说：此前失败被吞成空列表，
             全部项目页因此一直显示「没有项目」，而真因是请求被拒（400）。
        -->
        <div v-if="loadError && !list.items.value.length" class="portal-empty wide">
          项目列表加载失败：{{ loadError }}
          <button type="button" class="portal-link-button" @click="list.reset()">重试</button>
        </div>
        <div v-else-if="!list.loading.value && !list.items.value.length && !showFolders" class="portal-empty wide">
          {{ keyword.trim() ? `没有找到名称包含「${keyword.trim()}」的项目` : projectKindEmptyText(kind) }}
        </div>
        <!-- 根目录下没有文件夹也没有未分类项目时，也要说一句，不留一片空白 -->
        <div v-else-if="!list.loading.value && !list.items.value.length && showFolders && !folders.length" class="portal-empty wide">
          这里还没有项目，点「新建项目」开始
        </div>
      </section>

      <p v-if="list.loading.value" class="library-end">正在加载</p>
      <p v-else-if="list.finished.value && list.items.value.length" class="library-end">没有更多了</p>
    </ProductAvailabilityGate>
  </main>
</template>
