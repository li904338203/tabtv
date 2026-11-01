<script setup lang="ts">
import type { components } from '~/types/legacy';
import type { components as ApiComponents } from "@tabtv/api-client";
import { ArrowRight, Clock3, LoaderCircle, Pause, Play, Plus, Video, Volume2, VolumeX } from "@lucide/vue";
import { ElCarousel, ElCarouselItem } from "element-plus";
import type { ComponentPublicInstance } from "vue";
import { bannerImageId, bannerTriplet, isSafeHomeBannerLink, isVideoBanner, toCarouselBanners, type ApiHomeBanner } from "../../utils/bannerCarousel";
import { projectCoverUrl } from "~/utils/authorizedMedia";
import { projectBadgeVariant, projectCanvasPath, projectOpenPath } from "~/utils/projectKind";

// 模板里不直接调 navigateTo：自动导入在模板上下文里 vue-tsc 解析不到（运行时没问题，但门禁会红）。
const go = navigateTo

definePageMeta({ layout: "platform", middleware: "auth" });

type HomeResponse = components["schemas"]["HomeResp"];
type ProjectResponse = components["schemas"]["ProjectResp"];
/**
 * 项目摘要 —— 取自**生成的新契约**，不再手写。
 *
 * ⚠️ 卡片封面读 `displayCoverMediaId`（服务端算好的「显式封面 ?? 最近一张产出图」），
 *    不是 `coverMediaId`（只有手动设过才有）。两者都是**媒体 ID 不是 URL**，
 *    地址由 `projectCoverUrl()` 统一拼。此前这里手写的类型里还留着老系统的 `cover`，
 *    而新后端从来不发它。
 */
// 列表项 = Project + `agent`（最近活跃的 Agent 会话，没有为 null；AGT-45）
type ProjectSummary = ApiComponents["schemas"]["ProjectListItem"];
/**
 * 项目分页。
 *
 * ⚠️ **不再用老契约的 `PageResultVideoProjectSummaryResp`** —— 那描述的是
 * 老后端的 `{list,total}` 页码分页，而新后端是 `{items,nextCursor}` 游标分页。
 * 照着老类型写，TS 会告诉你 `response.items` 不存在，而真正错的是那个类型。
 * 两种都留着是为了兼容尚未重接的端点（P15-01 挂账：逐步迁到 @tabtv/api-client）。
 */
type ProjectPage = {
  items?: ProjectSummary[]
  nextCursor?: string
  hasMore?: boolean
  list?: ProjectSummary[]
  total?: number
};
type HomeBanner = components["schemas"]["BannerResp"];

const home = ref<HomeResponse>({});
const creating = ref(false);
// 解构出来才能在模板里自动解包；嵌套在对象里的 ref 模板不会解
const { urls: coverUrls, loadAll: loadCovers } = useAuthenticatedMedia();
const activeBannerIndex = ref(0);
const activeVideo = ref<HTMLVideoElement | null>(null);
const videoPaused = ref(true);
const videoMuted = ref(true);

/**
 * 顶部的运营轮播（「视频首页」位的 Banner）按用户要求先隐藏（2026-11-01）：不请求、不渲染。
 * 代码都留着，恢复时把它改回 true 即可；后台的 Banner 排期照常能配，只是这一页不显示。
 */
const SHOW_BANNERS = false;

// ⚠️ 轮播图单独一个实例：`loadAll` 每次先释放自己持有的全部 object URL，
//    与项目封面共用的话，刷新一次最近项目就会把轮播图一起释放掉。
const { urls: bannerImageUrls, loadAll: loadBannerImages } = useAuthenticatedMedia();
/** 新后端发的是 `HomeBanner`（linkUrl / mediaObjectId），这里补成轮播组件要的形状。 */
const apiBanners = computed(() => ((home.value as { banners?: ApiHomeBanner[] }).banners ?? []));
const banners = computed(() => toCarouselBanners(apiBanners.value, bannerImageUrls.value));
const activeBanner = computed(() => banners.value[activeBannerIndex.value]);
const carouselAutoplay = computed(() =>
  banners.value.length > 1 && (!isVideoBanner(activeBanner.value) || videoPaused.value));

/** 最近项目取 5 个：加上「开始创作」卡正好占满一行 6 格；完整列表在 /video/projects。 */
const RECENT_LIMIT = 5;
const projects = ref<ProjectSummary[]>([]);
const projectTotal = ref(0);
const projectsLoading = ref(true);

async function load() {
  // ⚠️ 参数是 `position` 不是老后端的 `productCode`：新后端不认识后者、回落到默认的 home 位，
  //    于是这里一直显示着「主工作台」的 Banner，而运营投到「视频首页」的一条都看不到。
  home.value = await useApi()<HomeResponse>("/home?position=video");
  activeBannerIndex.value = 0;
  // 轮播图是平台级素材，仍走受保护端点（`<img src>` 不带鉴权头）；用原图 —— 轮播卡片比缩略图的最大档还宽。
  await loadBannerImages(apiBanners.value.map((banner) => {
    const id = bannerImageId(banner);
    return { key: id ?? "", url: id ? `/media/${id}` : undefined };
  }));
  await syncVideoPlayback();
}

async function loadProjects() {
  projectsLoading.value = true;
  try {
    // 直接查视频域自己的列表，不从首页的跨产品 recentItems 里筛 ——
    // 那份是三产品混排后取前 6，写作/数字人的新项目会挤占视频页的名额
    // ⚠️ 新后端是**游标分页**且字段是 `items`（老后端 `page/size` + `list`）。
    //    参数名错了不会报错 —— 服务端忽略它们、按默认条数返回；
    //    但 `response.list` 取不到，`projects` 恒为空数组，
    //    **表现就是"这个页面没有最近项目"，而不报任何错**。
    const response = await useApi()<ProjectPage>(`/projects?limit=${RECENT_LIMIT}`);
    projects.value = response.items ?? response.list ?? [];
    // 游标分页没有总数，用已加载条数兜底（与 useInfiniteList 同一条取舍）。
    projectTotal.value = response.total ?? projects.value.length;
    // 封面是鉴权产出，必须换成 object URL 才能交给 <img>
    // ⚠️ 曾经把**媒体 ID 当成 URL** 传进去（`url: item.coverMediaId`），拼出
    //    `/api/v1/9646…` → 404，于是就算有封面也显示不出来。地址统一走 projectCoverUrl，
    //    且读服务端算好的 `displayCoverMediaId`（显式封面 ?? 最近一张产出图）。
    await loadCovers(projects.value.map(item => ({ key: item.id ?? "", url: projectCoverUrl(item) })));
  } finally {
    projectsLoading.value = false;
  }
}

/**
 * 最近项目卡片上「⋯」菜单的操作 —— 与全部项目页同一份（useProjectActions）。
 * 操作完重取这一块的列表：改名/复制会刷新 updatedAt，排序本来就会变。
 */
const projectActions = useProjectActions();
const editingId = ref("");

async function afterAction(done: Promise<boolean>) {
  if (await done) await loadProjects();
}

function onRename(project: ProjectSummary, next: string) {
  void afterAction(projectActions.rename(project, next));
}

async function createProject() {
  if (creating.value) return;
  creating.value = true;
  try {
    const response = await useApi()<ProjectResponse>("/projects", {
      method: "POST",
      body: { name: "未命名" }
    });
    if (response.id) await navigateTo(`/video/canvas/${response.id}`);
  } finally {
    creating.value = false;
  }
}

function setActiveVideo(element: Element | ComponentPublicInstance | null) {
  activeVideo.value = element instanceof HTMLVideoElement ? element : null;
}

async function syncVideoPlayback() {
  await nextTick();
  const video = activeVideo.value;
  if (!video) {
    videoPaused.value = true;
    return;
  }
  video.muted = videoMuted.value;
  try {
    await video.play();
    videoPaused.value = false;
  } catch {
    videoPaused.value = true;
  }
}

function onBannerChange(index: number) {
  activeBannerIndex.value = index;
  void syncVideoPlayback();
}

function toggleVideoPlayback() {
  const video = activeVideo.value;
  if (!video) return;
  if (video.paused) void video.play();
  else video.pause();
}

function toggleVideoMuted() {
  videoMuted.value = !videoMuted.value;
  if (activeVideo.value) activeVideo.value.muted = videoMuted.value;
}

async function openBanner(item: HomeBanner) {
  if (!isSafeHomeBannerLink(item.link)) return;
  if (item.link?.startsWith("/")) await navigateTo(item.link);
  else window.open(item.link, "_blank", "noopener,noreferrer");
}

onMounted(() => {
  // load() 只取轮播（/home?position=video）：轮播隐藏时连请求都不发
  if (SHOW_BANNERS) void load();
  void loadProjects();
});
</script>

<template>
  <main class="workbench-main">
    <ProductAvailabilityGate code="video">
      <header class="workbench-welcome">
        <div>
          <span class="page-kicker video-tone"><Video :size="15" />画布创造</span>
          <h1>视频项目</h1>
        </div>
        <button type="button" class="product-primary-command" :disabled="creating" @click="createProject">
          <LoaderCircle v-if="creating" :size="17" class="spin" /><Plus v-else :size="17" />
          {{ creating ? '正在创建' : '新建视频项目' }}
        </button>
      </header>

      <section v-if="SHOW_BANNERS && banners.length" class="hero-carousel" aria-label="创作动态">
        <ElCarousel
          height="286px"
          :autoplay="carouselAutoplay"
          :interval="6000"
          :arrow="banners.length > 1 ? 'always' : 'never'"
          :indicator-position="banners.length > 1 ? '' : 'none'"
          :loop="banners.length > 1"
          :pause-on-hover="true"
          trigger="click"
          @change="onBannerChange"
        >
          <ElCarouselItem v-for="(slide, slideIndex) in banners" :key="slide.id || slideIndex">
            <div class="hero-frame-grid" :class="{ single: banners.length === 1 }">
              <article
                v-for="item in bannerTriplet(banners, slideIndex)"
                :key="`${slideIndex}-${item.position}-${item.banner.id}`"
                class="hero-card"
                :class="[item.position, { linked: isSafeHomeBannerLink(item.banner.link) }]"
                :aria-label="item.banner.title || '视频轮播'"
                :role="isSafeHomeBannerLink(item.banner.link) ? 'link' : undefined"
                :tabindex="item.position === 'current' && isSafeHomeBannerLink(item.banner.link) ? 0 : undefined"
                @click="item.position === 'current' && openBanner(item.banner)"
                @keydown.enter="item.position === 'current' && openBanner(item.banner)"
              >
                <video
                  v-if="item.position === 'current' && slideIndex === activeBannerIndex && isVideoBanner(item.banner)"
                  :ref="setActiveVideo"
                  :src="item.banner.videoUrl"
                  :poster="item.banner.cover"
                  muted
                  playsinline
                  preload="metadata"
                  @play="videoPaused = false"
                  @pause="videoPaused = true"
                  @ended="videoPaused = true"
                  @click.stop="toggleVideoPlayback"
                />
                <img v-else :src="item.banner.cover" :alt="item.banner.title || '视频轮播'">
                <div
                  v-if="item.position === 'current' && slideIndex === activeBannerIndex && isVideoBanner(item.banner)"
                  class="hero-video-controls"
                >
                  <button type="button" class="round-play" :title="videoPaused ? '播放视频' : '暂停视频'" @click.stop="toggleVideoPlayback">
                    <Play v-if="videoPaused" :size="17" fill="currentColor" />
                    <Pause v-else :size="17" fill="currentColor" />
                  </button>
                  <button type="button" class="hero-sound" :title="videoMuted ? '打开声音' : '静音'" @click.stop="toggleVideoMuted">
                    <VolumeX v-if="videoMuted" :size="15" />
                    <Volume2 v-else :size="15" />
                  </button>
                </div>
              </article>
            </div>
          </ElCarouselItem>
        </ElCarousel>
      </section>

      <div class="section-head">
        <div>
          <h2>最近项目</h2>
          <p>{{ projectTotal ? '继续上次的画布创作' : '从一个视频工作流开始' }}</p>
        </div>
        <!-- 这里只给最近 6 个摘要，完整列表在 /video/projects -->
        <NuxtLink to="/video/projects" class="text-link">全部项目 <ArrowRight :size="14" /></NuxtLink>
      </div>
      <section class="home-project-grid">
        <article class="home-project-card">
          <button type="button" class="home-project-thumb create" :disabled="creating" @click="createProject">
            <LoaderCircle v-if="creating" :size="25" class="spin" /><Plus v-else :size="25" />
            <span>{{ creating ? "正在创建" : "开始创作" }}</span>
          </button>
          <h3>新建项目</h3><p>进入空白画布</p>
        </article>
        <article v-for="project in projects" :key="project.id" class="home-project-card">
          <!-- Agent 项目点开进对话页的最近会话；普通项目进画布（AGT-45） -->
          <button type="button" class="home-project-thumb" @click="go(projectOpenPath(project))">
            <ProjectAgentBadge v-if="projectBadgeVariant(project)" :variant="projectBadgeVariant(project) ?? undefined" />
            <img v-if="coverUrls[project.id ?? '']" :src="coverUrls[project.id ?? '']" :alt="project.name">
            <Video v-else :size="26" />
          </button>
          <!-- 名称行右侧的「⋯」与全部项目页是**同一个**组件与同一套操作（useProjectActions） -->
          <div class="card-meta-row">
            <div class="card-meta-text">
              <ProjectCardTitle
                :name="project.name ?? ''"
                :editing="editingId === project.id"
                compact
                @update:editing="value => editingId = value ? (project.id ?? '') : ''"
                @commit="value => onRename(project, value)"
              />
              <ProjectAgentSubtitle v-if="project.agent" :agent="project.agent" />
              <p>
                <Clock3 :size="12" />
                {{ project.updatedAt ? new Date(project.updatedAt).toLocaleDateString('zh-CN') : '最近更新' }}
              </p>
            </div>
            <ProjectCardMenu
              :project="project"
              :folders="projectActions.folders.value"
              :agent="Boolean(project.agent)"
              @opened="projectActions.loadFolders()"
              @open="go(projectOpenPath(project))"
              @open-canvas="go(projectCanvasPath(project))"
              @rename="editingId = project.id ?? ''"
              @duplicate="afterAction(projectActions.duplicate(project))"
              @move="folderId => afterAction(projectActions.moveToFolder(project, folderId))"
              @remove="afterAction(projectActions.remove(project))"
            />
          </div>
        </article>
        <div v-if="projectsLoading && !projects.length" class="portal-empty wide">正在加载项目</div>
      </section>

      <div class="section-head"><div><h2>灵感与作品</h2><p>平台精选创作</p></div></div>
      <!-- 与工作台首页、创造页同一个组件；分类走服务端 type 参数，没有搜索框（后端不支持关键词） -->
      <ShowcaseGrid show-categories />
    </ProductAvailabilityGate>
  </main>
</template>
