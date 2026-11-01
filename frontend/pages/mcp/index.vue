<script setup lang="ts">
/**
 * MCP 接入指南（`/mcp`，MCPT-11F 改版）：让用户把 seeAitv 接进自己的 AI 工具。
 *
 * 三段：主视觉 · 接入卡片（客户端页签 × 一键接入 / 密钥 · CLI）· 能力示意。
 *
 * ⚠️ 打开时**只发一个 GET**（`/me/mcp/client-snippets`）。命令、配置、发给 Agent 的话术全部来自服务端
 *    （`items[].modes`），前端不拼命令；能力示意是写死的静态内容，不请求任何数据接口。
 * ⚠️ 本页**不持有任何密钥明文**：密钥方式的话术里是占位符，新建密钥在 `/account/mcp`。
 * ⚠️ 不要建 `pages/mcp.vue`：那会让 `/mcp/authorize`、`/mcp/approve/*` 变成它的子路由、套上这一页。
 */
import { ArrowRight, Bot, Check, CircleCheck, Clapperboard, Copy, KeyRound, Plug, SquareTerminal, Boxes, Workflow } from "@lucide/vue";
import { ElMessage } from "element-plus";
import type { Component } from "vue";
import McpClientIcon from "~/components/mcp/McpClientIcon.vue";
import { mcpLoadError } from "~/utils/mcpKeys";
import { snippetFormatLabel, snippetsUrlNotice, type McpClientSnippets } from "~/utils/mcpSnippets";
import {
  CLIENT_LABELS,
  GUIDE_MODES,
  HERO_CLIENTS,
  guideClients,
  guideCopyText,
  modeOf,
  pickClient,
  unsupportedText,
  type GuideMode,
} from "~/utils/mcpGuide";
import { SHOWCASE_DEMOS, showcaseOf, type ShowcaseKey } from "~/utils/mcpShowcase";

definePageMeta({ layout: "platform", middleware: "auth" });
useHead({ title: "MCP 接入指南" });

const snippets = ref<McpClientSnippets | null>(null);
const loading = ref(true);
const loadError = ref<{ text: string; retry: boolean } | null>(null);

const mode = ref<GuideMode>("oauth");
const selected = ref<string | null>(null);
const clients = computed(() => guideClients(snippets.value, mode.value));
const current = computed(() => pickClient(clients.value, selected.value));
const entry = computed(() => modeOf(current.value, mode.value));
const copyText = computed(() => guideCopyText(entry.value));
const urlNotice = computed(() => snippetsUrlNotice(snippets.value));
const placeholder = computed(() => snippets.value?.keyPlaceholder ?? "<YOUR_MCP_KEY>");

const modeIcons: Record<GuideMode, Component> = { oauth: Plug, key: SquareTerminal };

// 主视觉：左三个、右三个，越靠近中间越大越靠前。
const heroLeft = HERO_CLIENTS.slice(0, 3);
const heroRight = HERO_CLIENTS.slice(3);

const promptEl = ref<HTMLElement | null>(null);
const copied = ref(false);
let copiedTimer: ReturnType<typeof setTimeout> | undefined;

function online() {
  return !import.meta.client || navigator.onLine;
}

async function load() {
  loading.value = true;
  try {
    snippets.value = await useApi()<McpClientSnippets>("/me/mcp/client-snippets");
    loadError.value = null;
  } catch (cause) {
    loadError.value = mcpLoadError(cause, online());
  } finally {
    loading.value = false;
  }
}

function selectClient(client: string) {
  selected.value = client;
  copied.value = false;
}

function selectMode(next: GuideMode) {
  // 记住当前客户端：切到另一种方式时它若仍可见就保持（pickClient），不把人丢回第一个。
  if (current.value) selected.value = current.value.client;
  mode.value = next;
  copied.value = false;
}

async function copy() {
  const text = copyText.value;
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text);
    copied.value = true;
    clearTimeout(copiedTimer);
    copiedTimer = setTimeout(() => (copied.value = false), 1800);
  } catch {
    if (promptEl.value) window.getSelection()?.selectAllChildren(promptEl.value);
    ElMessage.warning("复制没有成功，已选中内容，请手动复制");
  }
}

// ---- 能力示意（静态）
const demoIcons: Record<ShowcaseKey, Component> = { generate: Boxes, canvas: Workflow, film: Clapperboard };
const activeDemo = ref<ShowcaseKey>("generate");
const demo = computed(() => showcaseOf(activeDemo.value));

onMounted(load);
onBeforeUnmount(() => clearTimeout(copiedTimer));
</script>

<template>
  <main class="portal-page mcp-guide">
    <!-- 主视觉 -->
    <section class="mcp-hero">
      <div class="mcp-hero-icons" aria-hidden="true">
        <span v-for="(c, i) in heroLeft" :key="c" :class="['mcp-tile', `tone-${c}`, `ring-${3 - i}`]">
          <McpClientIcon :client="c" :label="CLIENT_LABELS[c]" :size="[16, 20, 26][i]" />
        </span>
        <span class="mcp-tile tone-seeaitv ring-0"><McpClientIcon client="seeaitv" :size="40" /></span>
        <span v-for="(c, i) in heroRight" :key="c" :class="['mcp-tile', `tone-${c}`, `ring-${i + 1}`]">
          <McpClientIcon :client="c" :label="CLIENT_LABELS[c]" :size="[26, 20, 16][i]" />
        </span>
      </div>
      <h1>seeAitv MCP，让你的 Agent 开始创作</h1>
      <p>
        把 seeAitv 接进 Claude Code、Codex、Cursor 等 AI 工具，用一句话生成图片与视频、读写画布和素材。查看
        <a href="#mcp-capabilities">使用指南</a>
      </p>
    </section>

    <!-- 接入卡片 -->
    <section class="mcp-connect" aria-label="接入方式">
      <header class="mcp-connect-head">
        <nav class="mcp-client-tabs" aria-label="选择客户端">
          <button
            v-for="item in clients"
            :key="item.client"
            type="button"
            :class="{ active: item.client === current?.client }"
            :aria-pressed="item.client === current?.client"
            :aria-label="item.label"
            :title="item.label"
            @click="selectClient(item.client)"
          >
            <McpClientIcon :client="item.client" :label="item.label" :size="18" />
          </button>
          <span v-if="loading && !clients.length" class="mcp-tabs-placeholder">正在加载客户端…</span>
        </nav>
        <div class="mcp-mode-switch" role="group" aria-label="接入方式">
          <button
            v-for="m in GUIDE_MODES"
            :key="m.mode"
            type="button"
            :class="{ active: m.mode === mode }"
            :aria-pressed="m.mode === mode"
            :title="m.hint"
            @click="selectMode(m.mode)"
          >
            <component :is="modeIcons[m.mode]" :size="14" />{{ m.label }}
          </button>
        </div>
      </header>

      <div v-if="loadError" class="portal-notice error mcp-connect-notice">
        {{ loadError.text }}
        <button v-if="loadError.retry" type="button" class="portal-link-button" @click="load">重试</button>
      </div>
      <p v-else-if="urlNotice" class="portal-notice mcp-connect-notice">{{ urlNotice }}</p>

      <div class="mcp-connect-body">
        <!-- ① 复制给 Agent -->
        <div class="mcp-step">
          <span class="mcp-step-no">1</span>
          <h2>复制并发送以下内容给你的 Agent</h2>
          <p class="mcp-step-desc">
            让你的 Agent 为你完成接入
            <template v-if="current"> · {{ current.label }}<template v-if="entry?.supported && entry.location">（{{ entry.location }}）</template></template>
          </p>

          <div v-if="loading && !snippets" class="mcp-prompt-box is-empty">正在加载接入内容…</div>
          <div v-else-if="!current" class="mcp-prompt-box is-empty">{{ loadError ? "接入内容没有加载出来" : "暂时没有可用的客户端" }}</div>
          <div v-else-if="!entry?.supported" class="mcp-prompt-box is-empty">
            <p>{{ unsupportedText(entry) }}</p>
            <button type="button" class="secondary-command" @click="selectMode('key')"><SquareTerminal :size="13" />改用密钥 · CLI</button>
          </div>
          <template v-else>
            <div class="mcp-prompt-box">
              <pre ref="promptEl">{{ copyText ?? "" }}</pre>
              <button
                type="button"
                class="mcp-copy"
                :disabled="!copyText"
                :aria-label="copied ? '已复制' : '复制'"
                :title="copied ? '已复制' : '复制'"
                @click="copy"
              >
                <Check v-if="copied" :size="15" />
                <Copy v-else :size="15" />
              </button>
            </div>
            <details class="mcp-raw">
              <summary>只看{{ entry.format === 'shell' ? '命令' : '配置' }}（{{ snippetFormatLabel(entry.format ?? 'json') }}）</summary>
              <pre>{{ entry.content }}</pre>
            </details>
          </template>
        </div>

        <!-- ② 连接账户 -->
        <div class="mcp-step">
          <span class="mcp-step-no">2</span>
          <h2>连接账户，开始创作</h2>
          <template v-if="mode === 'oauth'">
            <p class="mcp-step-desc">授权完成后，让 Agent 帮你生成图片、视频，或整理创作素材</p>
            <ol class="mcp-flow">
              <li>Agent 写好配置后，按它的提示在客户端里发起登录 —— 命令与配置里不带任何密钥</li>
              <li>浏览器会打开 seeAitv 授权页：确认登录的账号，选择工作区与权限，点「同意」</li>
              <li>花积分的操作都会先报价，经你确认后才扣费</li>
              <li>连接可以随时在「MCP 访问」里查看和撤销</li>
            </ol>
          </template>
          <template v-else>
            <p class="mcp-step-desc">用访问密钥连接：适合不支持一键登录的客户端、脚本与 stdio 桥接</p>
            <ol class="mcp-flow">
              <li>到「MCP 访问」新建一把访问密钥，选好工作区与权限；明文只在创建时显示一次</li>
              <li>把话术里的 <code>{{ placeholder }}</code> 换成你的密钥，再发给 Agent（也可以在创建后直接复制已填好密钥的配置）</li>
              <li>本页不会显示、也不会保存密钥明文；密钥丢了就撤销后重建</li>
              <li>花积分的操作都会先报价，经你确认后才扣费</li>
            </ol>
          </template>
          <p v-if="entry?.supported && entry.note" class="mcp-note"><strong>说明</strong>{{ entry.note }}</p>
          <NuxtLink to="/account/mcp" class="mcp-step-link">
            <KeyRound :size="14" />
            {{ mode === 'key' ? '新建访问密钥' : '管理已连接的客户端' }}
            <ArrowRight :size="14" />
          </NuxtLink>
        </div>
      </div>
    </section>

    <!-- 能力示意（静态） -->
    <section id="mcp-capabilities" class="mcp-capabilities">
      <h2>看看 seeAitv MCP 能做什么</h2>
      <nav class="mcp-demo-tabs" aria-label="能力示意">
        <button
          v-for="d in SHOWCASE_DEMOS"
          :key="d.key"
          type="button"
          :class="{ active: d.key === activeDemo }"
          :aria-pressed="d.key === activeDemo"
          @click="activeDemo = d.key"
        >
          <component :is="demoIcons[d.key]" :size="15" />{{ d.label }}
        </button>
      </nav>

      <div class="mcp-demo-grid">
        <article class="mcp-demo-chat">
          <header><Bot :size="18" /><span>你的 Agent</span><small>示意</small></header>
          <p class="mcp-bubble user">{{ demo.user }}</p>
          <ul class="mcp-calls">
            <li v-for="(step, i) in demo.steps" :key="i" :class="step.kind">
              <template v-if="step.kind === 'call'">
                <code>seeaitv · {{ step.call.tool }}</code>
                <span>{{ step.call.detail }}</span>
                <em v-if="step.call.cost">{{ step.call.cost }}</em>
              </template>
              <template v-else>
                <span class="mcp-confirm"><Check :size="12" />{{ step.text }}</span>
              </template>
            </li>
          </ul>
          <p class="mcp-bubble agent">{{ demo.reply }}</p>
        </article>

        <article class="mcp-demo-result">
          <header><CircleCheck :size="15" />生成结果</header>
          <div v-if="demo.key === 'generate'" class="mcp-result-images">
            <figure v-for="(name, i) in demo.result.items" :key="name" :class="`shade-${i}`">
              <svg viewBox="0 0 60 80" aria-hidden="true">
                <circle cx="44" cy="18" r="8" class="sun" />
                <path d="M24 30h12v6l3 6v28a3 3 0 0 1-3 3H24a3 3 0 0 1-3-3V42l3-6Z" class="bottle" />
                <path d="M0 62c12-6 24 4 36-2s18-4 24 0v20H0Z" class="wave" />
              </svg>
              <figcaption>{{ name }}</figcaption>
            </figure>
          </div>
          <div v-else-if="demo.key === 'canvas'" class="mcp-result-canvas">
            <svg viewBox="0 0 300 180" aria-hidden="true">
              <path d="M96 52 C130 52 130 90 160 90" class="edge new" />
              <path d="M96 128 C130 128 130 90 160 90" class="edge" />
              <path d="M236 90 H264" class="edge new" />
              <rect x="16" y="32" width="80" height="40" rx="8" class="node" />
              <rect x="16" y="108" width="80" height="40" rx="8" class="node new" />
              <rect x="160" y="70" width="76" height="40" rx="8" class="node" />
              <circle cx="272" cy="90" r="8" class="port" />
              <text x="56" y="56">{{ demo.result.items[0] }}</text>
              <text x="56" y="132">{{ demo.result.items[1] }}</text>
              <text x="198" y="94">{{ demo.result.items[2] }}</text>
              <text x="56" y="164" class="tag">MCP 新增</text>
            </svg>
          </div>
          <div v-else class="mcp-result-film">
            <div class="frames">
              <span v-for="(name, i) in demo.result.items" :key="name" :class="`shade-${i % 4}`">{{ name }}</span>
            </div>
            <div class="timeline" aria-hidden="true">
              <i v-for="(name, i) in demo.result.items" :key="name" :class="`shade-${i % 4}`" />
            </div>
          </div>
          <footer><strong>{{ demo.result.title }}</strong><small>{{ demo.result.caption }}</small></footer>
        </article>
      </div>
      <p class="mcp-demo-foot">以上为示意，金额只是举例；实际价格以报价为准。花积分的调用都要经你确认，确认前不扣费。</p>
    </section>
  </main>
</template>

<style scoped>
.mcp-guide { display: grid; gap: 28px; padding-top: 40px; }

/* ---- 主视觉 */
.mcp-hero { display: grid; justify-items: center; gap: 12px; padding: 12px 0 8px; text-align: center; }
.mcp-hero-icons { display: flex; align-items: center; justify-content: center; margin-bottom: 18px; }
.mcp-tile { position: relative; display: grid; place-items: center; border: 1px solid var(--portal-line); border-radius: 30%; box-shadow: 0 10px 26px var(--portal-shadow); }
.mcp-tile + .mcp-tile { margin-left: -10px; }
.mcp-tile.ring-3 { width: 38px; height: 38px; z-index: 1; opacity: .78; transform: translateY(5px); }
.mcp-tile.ring-2 { width: 50px; height: 50px; z-index: 2; opacity: .9; transform: translateY(-4px); }
.mcp-tile.ring-1 { width: 62px; height: 62px; z-index: 3; }
.mcp-tile.ring-0 { width: 92px; height: 92px; z-index: 4; margin-left: -12px; margin-right: -2px; border-color: var(--portal-line-strong); border-radius: 26%; box-shadow: 0 18px 46px var(--portal-shadow-strong), 0 0 0 6px var(--portal-cyan-soft); }
.mcp-tile.ring-0 + .mcp-tile { margin-left: -12px; }
.tone-seeaitv { color: var(--portal-mark-text); background: var(--portal-mark-bg); }
.tone-claude_code { color: var(--portal-on-accent); background: var(--portal-coral); }
.tone-codex { color: var(--portal-text); background: var(--portal-surface-3); }
.tone-cursor { color: var(--portal-text); background: var(--portal-surface-3); }
.tone-gemini_cli { color: var(--portal-cyan); background: var(--portal-surface-2); }
.tone-opencode { color: var(--portal-text-soft); background: var(--portal-surface-2); }
.tone-workbuddy { color: var(--portal-green); background: var(--portal-surface-2); }
.mcp-hero h1 { margin: 0; font-size: clamp(22px, 3vw, 30px); font-weight: 700; }
.mcp-hero p { max-width: 640px; margin: 0; color: var(--portal-muted); font-size: 13px; line-height: 1.7; }
.mcp-hero a { color: var(--portal-text-soft); text-decoration: underline; text-underline-offset: 3px; }
.mcp-hero a:hover { color: var(--portal-cyan); }

/* ---- 接入卡片 */
.mcp-connect { width: min(1180px, 100%); justify-self: center; border: 1px solid var(--portal-line); border-radius: 22px; padding: 8px; background: var(--portal-panel); }
.mcp-connect-head { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 10px; padding: 4px 6px 10px; }
.mcp-client-tabs { display: flex; flex-wrap: wrap; align-items: center; gap: 4px; min-height: 40px; }
.mcp-client-tabs button { width: 44px; height: 40px; display: grid; place-items: center; border: 0; border-radius: 12px; color: var(--portal-muted); background: transparent; cursor: pointer; }
.mcp-client-tabs button:hover { color: var(--portal-text); background: var(--portal-hover); }
.mcp-client-tabs button.active { color: var(--portal-text); background: var(--portal-surface-3); }
.mcp-tabs-placeholder { color: var(--portal-faint); font-size: 12px; padding: 0 8px; }
.mcp-mode-switch { display: inline-flex; gap: 2px; border-radius: 12px; padding: 3px; background: var(--portal-surface); }
.mcp-mode-switch button { display: inline-flex; align-items: center; gap: 6px; min-height: 34px; border: 0; border-radius: 10px; padding: 0 14px; color: var(--portal-muted); background: transparent; font-size: 13px; cursor: pointer; }
.mcp-mode-switch button:hover { color: var(--portal-text); }
.mcp-mode-switch button.active { color: var(--portal-text); background: var(--portal-surface-3); }
.mcp-connect-notice { margin: 0 6px 10px; }
.mcp-connect-body { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); border-radius: 16px; background: var(--portal-surface); }
.mcp-step { min-width: 0; display: grid; align-content: start; gap: 8px; padding: 22px 24px 24px; }
.mcp-step + .mcp-step { border-left: 1px solid var(--portal-line); }
.mcp-step-no { width: 22px; height: 22px; display: grid; place-items: center; border-radius: 50%; color: var(--portal-text-soft); background: var(--portal-surface-3); font-size: 11px; font-weight: 600; }
.mcp-step h2 { margin: 10px 0 0; font-size: 15px; font-weight: 650; }
.mcp-step-desc { margin: 0 0 8px; color: var(--portal-muted); font-size: 12px; }
.mcp-prompt-box { position: relative; border: 1px solid var(--portal-line); border-radius: 12px; background: var(--portal-panel); }
.mcp-prompt-box pre { max-height: 236px; overflow: auto; margin: 0; padding: 14px 48px 14px 16px; color: var(--portal-text-soft); font: 12px/1.75 Inter, "PingFang SC", "Microsoft YaHei", sans-serif; white-space: pre-wrap; word-break: break-word; }
.mcp-prompt-box.is-empty { display: grid; justify-items: start; gap: 10px; padding: 16px; color: var(--portal-muted); font-size: 12px; line-height: 1.7; }
.mcp-prompt-box.is-empty p { margin: 0; }
.mcp-copy { position: absolute; top: 8px; right: 8px; width: 30px; height: 30px; display: grid; place-items: center; border: 1px solid var(--portal-line); border-radius: 8px; color: var(--portal-muted); background: var(--portal-surface); cursor: pointer; }
.mcp-copy:hover:not(:disabled) { color: var(--portal-text); border-color: var(--portal-line-strong); }
.mcp-copy:disabled { opacity: .5; cursor: not-allowed; }
.mcp-raw { color: var(--portal-faint); font-size: 11px; }
.mcp-raw summary { cursor: pointer; width: fit-content; }
.mcp-raw summary:hover { color: var(--portal-muted); }
.mcp-raw pre { overflow-x: auto; margin: 8px 0 0; border: 1px solid var(--portal-line-soft); border-radius: 10px; padding: 12px 14px; color: var(--portal-text-soft); background: var(--portal-code-bg); font: 11px/1.6 ui-monospace, SFMono-Regular, Menlo, monospace; white-space: pre; }
.mcp-flow { display: grid; gap: 8px; margin: 0; padding-left: 18px; color: var(--portal-text-soft); font-size: 12px; line-height: 1.7; }
.mcp-flow li::marker { color: var(--portal-faint); }
.mcp-flow code { border-radius: 4px; padding: 1px 5px; background: var(--portal-surface-3); font: 11px ui-monospace, SFMono-Regular, Menlo, monospace; }
.mcp-note { margin: 6px 0 0; border: 1px solid var(--portal-line-soft); border-radius: 10px; padding: 10px 12px; color: var(--portal-muted); background: var(--portal-panel); font-size: 11px; line-height: 1.7; }
.mcp-note strong { margin-right: 6px; color: var(--portal-text-soft); font-weight: 600; }
.mcp-step-link { display: inline-flex; align-items: center; gap: 6px; width: fit-content; margin-top: 6px; border: 1px solid var(--portal-line); border-radius: 10px; padding: 7px 12px; color: var(--portal-text); font-size: 12px; }
.mcp-step-link:hover { border-color: var(--portal-line-strong); background: var(--portal-hover); }

/* ---- 能力示意 */
.mcp-capabilities { width: min(1180px, 100%); justify-self: center; display: grid; justify-items: center; gap: 18px; margin-top: 44px; scroll-margin-top: 80px; }
.mcp-capabilities > h2 { margin: 0; font-size: 22px; font-weight: 650; }
.mcp-demo-tabs { display: flex; flex-wrap: wrap; justify-content: center; gap: 6px; }
.mcp-demo-tabs button { display: inline-flex; align-items: center; gap: 7px; min-height: 38px; border: 0; border-radius: 12px; padding: 0 16px; color: var(--portal-muted); background: transparent; font-size: 13px; cursor: pointer; }
.mcp-demo-tabs button:hover { color: var(--portal-text); }
.mcp-demo-tabs button.active { color: var(--portal-text); background: var(--portal-surface-2); }
.mcp-demo-grid { width: 100%; display: grid; grid-template-columns: minmax(0, 1.5fr) minmax(0, 1fr); gap: 10px; }
.mcp-demo-chat, .mcp-demo-result { min-width: 0; display: grid; align-content: start; gap: 14px; border: 1px solid var(--portal-line); border-radius: 18px; padding: 20px 22px; background: var(--portal-panel); }
.mcp-demo-chat > header { display: flex; align-items: center; gap: 8px; color: var(--portal-text-soft); font-size: 13px; }
.mcp-demo-chat > header small { margin-left: auto; border: 1px solid var(--portal-line); border-radius: 999px; padding: 1px 8px; color: var(--portal-faint); font-size: 10px; }
.mcp-bubble { max-width: 86%; margin: 0; border-radius: 14px; padding: 10px 14px; font-size: 12px; line-height: 1.7; }
.mcp-bubble.user { justify-self: end; color: var(--portal-text); background: var(--portal-surface-3); }
.mcp-bubble.agent { justify-self: start; color: var(--portal-text-soft); background: var(--portal-surface); border: 1px solid var(--portal-line-soft); }
.mcp-calls { display: grid; gap: 6px; margin: 0; padding: 0 0 0 12px; border-left: 2px solid var(--portal-line); list-style: none; }
.mcp-calls li { display: flex; flex-wrap: wrap; align-items: baseline; gap: 4px 10px; font-size: 12px; line-height: 1.6; }
.mcp-calls li.confirm { justify-content: flex-end; }
.mcp-calls code { border-radius: 6px; padding: 1px 7px; color: var(--portal-cyan); background: var(--portal-cyan-soft); font: 11px ui-monospace, SFMono-Regular, Menlo, monospace; }
.mcp-calls span { color: var(--portal-muted); }
.mcp-calls em { border-radius: 6px; padding: 0 6px; color: var(--portal-gold); background: var(--portal-gold-soft); font-size: 11px; font-style: normal; }
.mcp-confirm { display: inline-flex; align-items: center; gap: 5px; border: 1px solid var(--portal-gold-line); border-radius: 999px; padding: 1px 10px; color: var(--portal-gold) !important; font-size: 11px; }
.mcp-demo-result > header { display: flex; align-items: center; justify-content: center; gap: 6px; color: var(--portal-text-soft); font-size: 13px; }
.mcp-demo-result > footer { display: grid; gap: 2px; text-align: center; }
.mcp-demo-result > footer strong { font-size: 13px; font-weight: 600; }
.mcp-demo-result > footer small { color: var(--portal-faint); font-size: 11px; }
.mcp-demo-foot { margin: 0; color: var(--portal-faint); font-size: 11px; text-align: center; }

/* 结果示意（纯 CSS / SVG，不取任何真实数据） */
.shade-0 { background: linear-gradient(160deg, var(--portal-cyan-soft), var(--portal-surface-3)); }
.shade-1 { background: linear-gradient(160deg, var(--portal-surface-3), var(--portal-cyan-soft)); }
.shade-2 { background: linear-gradient(160deg, var(--portal-green-soft), var(--portal-surface-3)); }
.shade-3 { background: linear-gradient(160deg, var(--portal-gold-soft), var(--portal-surface-3)); }
.mcp-result-images { width: min(100%, 300px); justify-self: center; display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px; }
.mcp-result-images figure { position: relative; overflow: hidden; margin: 0; aspect-ratio: 3 / 4; border: 1px solid var(--portal-line-soft); border-radius: 10px; }
.mcp-result-images svg { width: 100%; height: 100%; display: block; }
.mcp-result-images .sun { fill: var(--portal-gold); opacity: .55; }
.mcp-result-images .bottle { fill: var(--portal-cyan); opacity: .5; }
.mcp-result-images .wave { fill: var(--portal-cyan); opacity: .22; }
.mcp-result-images figcaption { position: absolute; left: 8px; bottom: 6px; color: var(--portal-text-soft); font-size: 10px; }
.mcp-result-canvas { border: 1px solid var(--portal-line-soft); border-radius: 12px; background-color: var(--portal-surface); background-image: radial-gradient(var(--portal-line) 1px, transparent 1px); background-size: 14px 14px; }
.mcp-result-canvas svg { width: 100%; display: block; }
.mcp-result-canvas .node { fill: var(--portal-surface-2); stroke: var(--portal-line-strong); }
.mcp-result-canvas .node.new { stroke: var(--portal-cyan); stroke-dasharray: 4 3; fill: var(--portal-cyan-soft); }
.mcp-result-canvas .edge { fill: none; stroke: var(--portal-line-strong); stroke-width: 1.5; }
.mcp-result-canvas .edge.new { stroke: var(--portal-cyan); }
.mcp-result-canvas .port { fill: var(--portal-surface-3); stroke: var(--portal-line-strong); }
.mcp-result-canvas text { fill: var(--portal-text-soft); font-size: 11px; text-anchor: middle; }
.mcp-result-canvas text.tag { fill: var(--portal-cyan); font-size: 10px; }
.mcp-result-film { display: grid; gap: 10px; }
.mcp-result-film .frames { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 6px; }
.mcp-result-film .frames span { display: flex; align-items: end; aspect-ratio: 16 / 9; border: 1px solid var(--portal-line-soft); border-radius: 8px; padding: 4px 6px; color: var(--portal-text-soft); font-size: 10px; }
.mcp-result-film .timeline { display: flex; gap: 2px; height: 14px; border-radius: 6px; overflow: hidden; }
.mcp-result-film .timeline i { flex: 1; }

/* ---- 窄屏：两栏变上下 */
@media (max-width: 900px) {
  .mcp-connect-body, .mcp-demo-grid { grid-template-columns: minmax(0, 1fr); }
  .mcp-step + .mcp-step { border-left: 0; border-top: 1px solid var(--portal-line); }
  .mcp-step { padding: 18px 16px 20px; }
  .mcp-tile.ring-3 { display: none; }
}
@media (max-width: 560px) {
  .mcp-connect-head { flex-direction: column; align-items: stretch; }
  .mcp-mode-switch { align-self: start; }
  .mcp-tile.ring-0 { width: 76px; height: 76px; }
}
</style>
