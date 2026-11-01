<script setup lang="ts">
import type { components } from '~/types/legacy';
import {
  ArrowLeft,
  ArrowRight,
  AudioLines,
  Check,
  Heart,
  LoaderCircle,
  Mic2,
  Pause,
  Play,
  Search,
  SlidersHorizontal,
  X
} from "@lucide/vue";
import { ElMessage } from "element-plus";
import { voiceGenderLabel, voiceLanguageLabel } from "~/utils/voice";

type VoicePage = components["schemas"]["VoicePageResp"];
type VoiceItem = components["schemas"]["VoiceResp"];
type FavoriteResp = components["schemas"]["VoiceFavoriteResp"];
type VoiceTab = "lib" | "mine" | "fav";

const props = defineProps<{ modelValue: boolean; selectedId?: string }>();
const emit = defineEmits<{
  "update:modelValue": [value: boolean];
  select: [item: VoiceItem];
}>();

const pageData = ref<VoicePage>({});
const loading = ref(false);
const favoriteBusy = ref<string | null>(null);
const previewingId = ref<string | null>(null);
const tab = ref<VoiceTab>("lib");
const keyword = ref("");
const language = ref("");
const gender = ref("");
const page = ref(1);
const size = 20;
let currentAudio: HTMLAudioElement | null = null;

// ⚠️ 新后端的 `/voices` **返回裸数组且不分页**（老后端是 `{list,total}` + page/size）。
//    照老结构读的话 `pageData.list` 恒为 undefined —— **音色库永远是空的，且不报错**。
//    这里在一处适配：两种形状都认，分页在前端本地切。
const items = computed(() => {
  const d = pageData.value as unknown;
  const all = Array.isArray(d) ? d : ((d as { list?: unknown[] })?.list ?? []);
  const start = (page.value - 1) * size;
  return all.slice(start, start + size) as NonNullable<typeof pageData.value.list>;
});
const totalPages = computed(() => {
  const d = pageData.value as unknown;
  const n = Array.isArray(d) ? d.length : ((d as { total?: number })?.total ?? 0);
  return Math.max(1, Math.ceil(n / size));
});

function close() {
  stopPreview();
  emit("update:modelValue", false);
}

async function load() {
  loading.value = true;
  try {
    // ⚠️ 参数名全都不同：`kw`→`keyword`、`lang`→`language`、
    //    `tab=fav`→`favorited=true`；`page`/`size` 新后端根本不认（它不分页）。
    //    参数名写错**不会报错** —— 服务端忽略它们，返回全量未筛选的结果，
    //    表现是"筛选点了没反应"。
    const params = new URLSearchParams();
    if (keyword.value.trim()) params.set("keyword", keyword.value.trim());
    if (language.value) params.set("language", language.value);
    if (gender.value) params.set("gender", gender.value);
    if (tab.value === "fav") params.set("favorited", "true");
    pageData.value = await useApi()<VoicePage>(`/voices?${params}`);
  } catch (cause) {
    pageData.value = [] as unknown as VoicePage;
    ElMessage.error(cause instanceof Error ? cause.message : "音色库加载失败");
  } finally {
    loading.value = false;
  }
}

function search() {
  page.value = 1;
  void load();
}

function changeTab(value: VoiceTab) {
  tab.value = value;
  keyword.value = "";
  language.value = "";
  gender.value = "";
  search();
}

function changePage(value: number) {
  if (value < 1 || value > totalPages.value) return;
  page.value = value;
  void load();
}

async function toggleFavorite(item: VoiceItem) {
  if (!item.id || favoriteBusy.value) return;
  favoriteBusy.value = item.id;
  try {
    const result = await useApi()<FavoriteResp>(`/voices/${item.id}/favorite`, {
      method: "POST",
      body: { favored: !item.favorite }
    });
    item.favorite = Boolean(result.favored);
    item.favoriteCount = result.favoriteCount ?? item.favoriteCount;
    if (tab.value === "fav" && !item.favorite) await load();
  } catch (cause) {
    ElMessage.error(cause instanceof Error ? cause.message : "收藏操作失败");
  } finally {
    favoriteBusy.value = null;
  }
}

function stopPreview() {
  if (currentAudio) {
    currentAudio.pause();
    currentAudio = null;
  }
  window.speechSynthesis?.cancel();
  previewingId.value = null;
}

function speak(item: VoiceItem) {
  if (!("speechSynthesis" in window) || !item.id) {
    previewingId.value = null;
    return;
  }
  const utterance = new SpeechSynthesisUtterance(`你好，我是${item.name ?? "官方音色"}，很高兴参与你的创作。`);
  utterance.lang = item.language || "zh-CN";
  utterance.pitch = item.gender === "female" ? 1.15 : 0.88;
  utterance.rate = 0.92;
  utterance.onend = () => { if (previewingId.value === item.id) previewingId.value = null; };
  utterance.onerror = () => { if (previewingId.value === item.id) previewingId.value = null; };
  window.speechSynthesis.speak(utterance);
}

async function preview(item: VoiceItem) {
  if (!item.id) return;
  if (previewingId.value === item.id) {
    stopPreview();
    return;
  }
  stopPreview();
  previewingId.value = item.id;
  if (!item.sampleUrl) {
    speak(item);
    return;
  }
  const audio = new Audio(item.sampleUrl);
  currentAudio = audio;
  audio.onended = stopPreview;
  audio.onerror = () => {
    currentAudio = null;
    speak(item);
  };
  try {
    await audio.play();
  } catch {
    currentAudio = null;
    speak(item);
  }
}

function choose(item: VoiceItem) {
  if (item.status !== "enabled") return;
  stopPreview();
  emit("select", item);
}

watch(() => props.modelValue, (open) => {
  if (!open) {
    stopPreview();
    return;
  }
  tab.value = "lib";
  keyword.value = "";
  language.value = "";
  gender.value = "";
  page.value = 1;
  void load();
});

onBeforeUnmount(stopPreview);
</script>

<template>
  <Teleport to="body">
    <div v-if="modelValue" class="voice-picker-overlay" @mousedown.self="close">
      <section class="voice-picker" role="dialog" aria-modal="true" aria-label="音色选择">
        <header class="voice-picker-head">
          <div><span>VOICE LIBRARY</span><h2>音色选择</h2></div>
          <button type="button" title="关闭音色选择" @click="close"><X :size="19" /></button>
        </header>

        <div class="voice-picker-toolbar">
          <div class="voice-tabs">
            <button type="button" :class="{ active: tab === 'lib' }" @click="changeTab('lib')">音色库</button>
            <button type="button" :class="{ active: tab === 'mine' }" @click="changeTab('mine')">我的音色</button>
            <button type="button" :class="{ active: tab === 'fav' }" @click="changeTab('fav')"><Heart :size="13" />收藏音色</button>
          </div>
          <button class="voice-clone-command" type="button" disabled title="克隆新音色"><Mic2 :size="14" />克隆新音色</button>
          <label class="voice-picker-search"><Search :size="15" /><input v-model="keyword" placeholder="搜索音色库" @keyup.enter="search"><button type="button" title="搜索" @click="search"><ArrowRight :size="14" /></button></label>
        </div>

        <div class="voice-picker-filters">
          <SlidersHorizontal :size="14" />
          <label><span>语言</span><select v-model="language" @change="search"><option value="">全部语言</option><option v-for="item in pageData.languages ?? []" :key="item" :value="item">{{ voiceLanguageLabel(item) }}</option></select></label>
          <label><span>性别</span><select v-model="gender" @change="search"><option value="">全部性别</option><option value="male">男声</option><option value="female">女声</option></select></label>
          <small>共 {{ pageData.total ?? 0 }} 条音色</small>
        </div>

        <div class="voice-picker-body">
          <div v-if="loading" class="voice-picker-empty"><LoaderCircle :size="24" class="spin" /><span>正在加载音色</span></div>
          <div v-else-if="!items.length" class="voice-picker-empty"><AudioLines :size="28" /><strong>{{ tab === "fav" ? "暂无收藏音色" : tab === "mine" ? "暂无个人音色" : "暂无符合条件的音色" }}</strong></div>
          <div v-else class="voice-list">
            <article v-for="item in items" :key="item.id" :class="['voice-row', { selected: selectedId === item.id, disabled: item.status !== 'enabled' }]">
              <span class="voice-list-avatar" :class="item.gender"><AudioLines :size="19" /></span>
              <div class="voice-row-copy"><strong>{{ item.name }}</strong><span>{{ voiceLanguageLabel(item.language) }} · {{ voiceGenderLabel(item.gender) }}</span></div>
              <div class="voice-wave" aria-hidden="true"><i v-for="(height, index) in [10, 18, 13, 24, 16, 21, 12, 18, 9, 15]" :key="index" :style="{ height: `${height}px` }" /></div>
              <button class="voice-play" type="button" :title="previewingId === item.id ? '停止试听' : '试听音色'" @click="preview(item)"><Pause v-if="previewingId === item.id" :size="15" /><Play v-else :size="15" /></button>
              <button class="voice-choose" type="button" :disabled="item.status !== 'enabled'" @click="choose(item)"><Check v-if="selectedId === item.id" :size="14" />{{ selectedId === item.id ? "已选择" : "选择" }}</button>
              <button class="voice-favorite" type="button" :class="{ active: item.favorite }" :disabled="favoriteBusy === item.id || item.status !== 'enabled'" :title="item.favorite ? '取消收藏' : '收藏音色'" @click="toggleFavorite(item)"><LoaderCircle v-if="favoriteBusy === item.id" :size="14" class="spin" /><Heart v-else :size="15" :fill="item.favorite ? 'currentColor' : 'none'" /><small>{{ item.favoriteCount ?? 0 }}</small></button>
            </article>
          </div>
        </div>

        <footer class="voice-picker-footer">
          <span>每页 {{ size }} 条</span>
          <div><button type="button" title="上一页" :disabled="page <= 1" @click="changePage(page - 1)"><ArrowLeft :size="15" /></button><strong>{{ page }} / {{ totalPages }}</strong><button type="button" title="下一页" :disabled="page >= totalPages" @click="changePage(page + 1)"><ArrowRight :size="15" /></button></div>
        </footer>
      </section>
    </div>
  </Teleport>
</template>
