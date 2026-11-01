<script setup lang="ts">
import type { components as ApiComponents } from "@tabtv/api-client";
import { ArrowLeft, RotateCcw, Trash2, TriangleAlert, Video } from "@lucide/vue";
import { ElMessage, ElMessageBox } from "element-plus";
import { projectCoverUrl } from "~/utils/authorizedMedia";

definePageMeta({ layout: "platform", middleware: "auth" });

// ⚠️ 类型取自**生成的新契约**。老类型描述的是 `{list,total}` 页码分页 ——
//    照它读 `response.list`，新后端下恒为 undefined，于是回收站**永远显示「是空的」**，
//    哪怕里面躺着刚删掉的项目。
type ProjectSummary = ApiComponents["schemas"]["Project"];
type ProjectPage = ApiComponents["schemas"]["ProjectPage"];

const PAGE_SIZE = 12;
/** 与后端 `project-trash.retention-days` 的默认值一致；只用于页头的说明文字。 */
const RETENTION_DAYS = 30;

const busyId = ref("");
const loadError = ref("");
// 解构出来才能在模板里自动解包；嵌套在对象里的 ref 模板不会解
const { urls: coverUrls, appendAll: loadCovers } = useAuthenticatedMedia();

/**
 * 剩余天数 —— **读服务端给的 `daysLeft`**，不在前端现算。
 *
 * ⚠️ 此前按 `updatedAt` 在前端算（注释说"老后端把 deleted_at 映射进了 updatedAt"）。
 *    新后端没有这层映射，而保留期**必须按 `deletedAt` 算**：恢复、改名都会刷
 *    updatedAt，按它算的话「删了又恢复又删」的项目每次重获 30 天。
 *    服务端已经按真实配置的保留期算好了（`daysLeft`），前端再算一遍只会多一个出错的地方。
 */
function daysLeft(project: ProjectSummary): number {
  const fromServer = (project as { daysLeft?: number }).daysLeft;
  return typeof fromServer === "number" ? fromServer : RETENTION_DAYS;
}

/**
 * ⚠️ 新后端是**游标分页**（`limit`+`cursor`、`{items,nextCursor,hasMore}`），
 *    没有总数 —— 所以原来的页码翻页器（1 2 3 …）在这里无从成立，
 *    改成与「全部项目」同一套滚动加载。
 */
const list = useInfiniteList<ProjectSummary>({
  pageSize: PAGE_SIZE,
  fetchPage: async (_next, cursor) => {
    const params = new URLSearchParams({ limit: String(PAGE_SIZE) });
    if (cursor) params.set("cursor", cursor);
    try {
      loadError.value = "";
      return await useApi()<ProjectPage>(`/projects/trash?${params}`);
    } catch (cause) {
      // 「取失败了」不能说成「回收站是空的」—— 那会让用户以为项目已经没了。
      loadError.value = (cause as { message?: string })?.message || "回收站加载失败";
      return null;
    }
  },
  onPage: items => loadCovers(items.map(item => ({ key: item.id ?? "", url: projectCoverUrl(item) })))
});

async function restore(project: ProjectSummary) {
  if (busyId.value) return;
  busyId.value = project.id ?? "";
  try {
    await useApi()(`/projects/${project.id}/restore`, { method: "POST" });
    ElMessage.success(`已恢复「${project.name}」`);
    await list.reset();
  } catch {
    ElMessage.error("恢复失败");
  } finally {
    busyId.value = "";
  }
}

async function purge(project: ProjectSummary) {
  if (busyId.value) return;
  try {
    await ElMessageBox.confirm(
      `「${project.name}」将被永久删除，包含画布内容且无法恢复。`,
      "彻底删除",
      { confirmButtonText: "永久删除", cancelButtonText: "取消", type: "warning" }
    );
  } catch {
    // ElMessageBox 取消时 reject，不是错误
    return;
  }
  busyId.value = project.id ?? "";
  try {
    await useApi()(`/projects/${project.id}/purge`, { method: "DELETE" });
    ElMessage.success("已永久删除");
    await list.reset();
  } catch {
    ElMessage.error("删除失败");
  } finally {
    busyId.value = "";
  }
}

onMounted(() => { void list.reset(); });
</script>

<template>
  <main class="portal-page">
    <ProductAvailabilityGate code="video">
      <header class="portal-page-head compact">
        <div>
          <NuxtLink to="/video/projects" class="text-link"><ArrowLeft :size="14" /> 返回全部项目</NuxtLink>
          <h1>回收站</h1>
          <p>
            <!-- 游标分页没有总数：只在全部加载完之后才报条数，否则那是个会变的数 -->
            {{ list.finished.value ? `${list.items.value.length} 个已删除项目` : '正在加载' }}
            · 删除后保留 {{ RETENTION_DAYS }} 天，到期自动清理
          </p>
        </div>
      </header>

      <section class="home-project-grid">
        <article v-for="project in list.items.value" :key="project.id" class="home-project-card trashed">
          <div class="home-project-thumb">
            <img v-if="coverUrls[project.id ?? '']" :src="coverUrls[project.id ?? '']" :alt="project.name">
            <Video v-else :size="26" />
          </div>
          <h3>{{ project.name }}</h3>
          <p>
            <TriangleAlert :size="12" />
            {{ daysLeft(project) > 0 ? `${daysLeft(project)} 天后清理` : '即将清理' }}
          </p>
          <div class="trash-actions">
            <button type="button" :disabled="busyId === project.id" @click="restore(project)">
              <RotateCcw :size="14" />恢复
            </button>
            <button type="button" class="danger" :disabled="busyId === project.id" @click="purge(project)">
              <Trash2 :size="14" />彻底删除
            </button>
          </div>
        </article>
        <div :ref="el => list.sentinel.value = (el as HTMLElement)" class="showcase-sentinel" />
        <div v-if="loadError && !list.items.value.length" class="portal-empty wide">
          回收站加载失败：{{ loadError }}
          <button type="button" class="portal-link-button" @click="list.reset()">重试</button>
        </div>
        <div v-else-if="list.loading.value && !list.items.value.length" class="portal-empty wide">正在加载</div>
        <div v-else-if="list.finished.value && !list.items.value.length" class="portal-empty wide">回收站是空的</div>
      </section>
      <p v-if="list.loading.value && list.items.value.length" class="library-end">正在加载</p>
    </ProductAvailabilityGate>
  </main>
</template>
