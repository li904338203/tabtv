<script setup lang="ts">
/**
 * 顶栏头像的账号面板：鼠标悬停打开，移入面板保持，移出 150ms 后关闭；
 * 点击头像也能开关（触屏没有悬停）；Esc / 点外面 / 点菜单项关闭。
 *
 * ⚠️ 面板**只读**：打开时最多发几条 GET 刷新数据，不写任何东西。
 *    唯一的「写」是模式切换，它走 useTheme（本地偏好），不碰服务端。
 * ⚠️ 拿不到的数据整块不显示，不造数字（判定见 utils/accountPanel.ts）。
 * ⚠️ 不新增轮询：未读数复用 notification store，打开时顺手刷一次。
 *
 * 两种用法（2026-11-01 画布顶栏接入）：
 * - `variant="account"`（默认）：头像的账号面板 —— 身份、会员、积分、账号菜单；
 * - `variant="credits"`：⚡ 积分按钮的积分面板 —— 只有会员卡、积分卡（各来源竖排）和积分相关入口。
 * 触发器的样式由 `triggerClass` 给（门户 portal-avatar；画布 canvas-avatar / credit-pill），内容可经默认插槽换掉。
 * 同一页上可以有好几个面板：id 用 useId，不写死。
 */
import {
  Bell,
  Cable,
  Check,
  ChevronRight,
  Contrast,
  Copy,
  Crown,
  LogOut,
  Moon,
  ReceiptText,
  Stamp,
  Sun,
  UserRound,
  Zap
} from "@lucide/vue";
import type { Component } from "vue";
import {
  accountContact,
  buildAccountMenu,
  buildCreditMenu,
  creditBreakdown,
  membershipAction,
  membershipLine,
  membershipView
} from "~/utils/accountPanel";

const props = withDefaults(defineProps<{ variant?: "account" | "credits"; triggerClass?: string }>(), {
  variant: "account",
  triggerClass: "portal-avatar"
});
/** 每次打开时发一次（画布顶栏借它顺手刷新 ⚡ 上的余额）。 */
const emit = defineEmits<{ open: [] }>();

const auth = useAuthStore();
const notifications = useNotificationStore();
const route = useRoute();
const { resolved: theme, set: setTheme } = useTheme();

/** 悬停离开后多久关闭：给鼠标从头像挪到面板留出余量。 */
const CLOSE_DELAY = 150;
/** 打开时刷新数据的最小间隔：鼠标来回划过头像不该每次都打三条请求。 */
const REFRESH_INTERVAL = 30_000;

const open = ref(false);
const root = ref<HTMLElement | null>(null);
const trigger = ref<HTMLButtonElement | null>(null);
const panel = ref<HTMLElement | null>(null);
const copied = ref(false);
let closeTimer: ReturnType<typeof setTimeout> | undefined;
let copiedTimer: ReturnType<typeof setTimeout> | undefined;
let openedByHoverAt = 0;
let lastRefreshAt = 0;

const isCredits = computed(() => props.variant === "credits");
const panelId = useId();
const panelLabel = computed(() => (isCredits.value ? "积分面板" : "账号面板"));
const initial = computed(() => auth.user?.nickname?.slice(0, 1) || "A");
const contact = computed(() => accountContact(auth.user));
const membership = computed(() => membershipView(auth.membership, auth.membershipHistory, new Date()));
const creditParts = computed(() => creditBreakdown(auth.creditGrants));
const menu = computed(() => (isCredits.value
  ? buildCreditMenu()
  : buildAccountMenu({
    unread: notifications.unread,
    watermarkOff: typeof auth.user?.isAiWatermarkOff === "boolean" ? auth.user.isAiWatermarkOff : null
  })));

const menuIcons: Record<string, Component> = {
  account: UserRound,
  orders: ReceiptText,
  theme: Contrast,
  watermark: Stamp,
  mcp: Cable,
  notifications: Bell,
  logout: LogOut,
  credits: Zap,
  membership: Crown
};

function cancelClose() {
  if (closeTimer) clearTimeout(closeTimer);
  closeTimer = undefined;
}

function refresh() {
  const now = Date.now();
  if (now - lastRefreshAt < REFRESH_INTERVAL) return;
  lastRefreshAt = now;
  // 失败只是面板上少几块，不打断任何操作
  auth.loadProfile().catch(() => {});
  void notifications.refresh();
}

function show() {
  cancelClose();
  if (open.value) return;
  open.value = true;
  refresh();
  emit("open");
}

function hide(returnFocus = false) {
  cancelClose();
  if (!open.value) return;
  open.value = false;
  if (returnFocus) trigger.value?.focus();
}

function onPointerEnter(event: PointerEvent) {
  if (event.pointerType !== "mouse") return;
  if (!open.value) openedByHoverAt = Date.now();
  show();
}

function onPointerLeave(event: PointerEvent) {
  if (event.pointerType !== "mouse") return;
  cancelClose();
  closeTimer = setTimeout(() => hide(), CLOSE_DELAY);
}

async function onTriggerClick(event: MouseEvent) {
  if (open.value) {
    // 悬停刚打开就点下去（鼠标用户的习惯动作），别立刻又关掉
    if (Date.now() - openedByHoverAt < 400) return;
    hide();
    return;
  }
  openedByHoverAt = 0;
  show();
  // detail===0 是键盘触发（Enter/空格）：把焦点送进面板，键盘用户才够得着里面的菜单
  if (event.detail === 0) {
    await nextTick();
    panel.value?.querySelector<HTMLElement>("a, button")?.focus();
  }
}

function onTriggerKeydown(event: KeyboardEvent) {
  if (event.key !== "ArrowDown") return;
  event.preventDefault();
  show();
  void nextTick(() => panel.value?.querySelector<HTMLElement>("a, button")?.focus());
}

function onFocusOut(event: FocusEvent) {
  const next = event.relatedTarget as Node | null;
  // relatedTarget 为空是点到了页面空白处，交给 pointerdown 处理
  if (next && !root.value?.contains(next)) hide();
}

function onDocumentPointerDown(event: PointerEvent) {
  if (root.value && !root.value.contains(event.target as Node)) hide();
}

function onDocumentKeydown(event: KeyboardEvent) {
  if (event.key !== "Escape") return;
  const focusInside = Boolean(root.value?.contains(document.activeElement));
  hide(focusInside);
}

watch(open, (value) => {
  if (!import.meta.client) return;
  if (value) {
    document.addEventListener("pointerdown", onDocumentPointerDown);
    document.addEventListener("keydown", onDocumentKeydown);
  } else {
    document.removeEventListener("pointerdown", onDocumentPointerDown);
    document.removeEventListener("keydown", onDocumentKeydown);
  }
});

// 换页即关：菜单项是 NuxtLink，点了之后路由会变
watch(() => route.fullPath, () => hide());

onBeforeUnmount(() => {
  cancelClose();
  if (copiedTimer) clearTimeout(copiedTimer);
  if (!import.meta.client) return;
  document.removeEventListener("pointerdown", onDocumentPointerDown);
  document.removeEventListener("keydown", onDocumentKeydown);
});

async function copyUserId() {
  const id = auth.user?.id;
  if (!id) return;
  try {
    // ⚠️ 局域网 IP 访问（http）不是安全上下文，navigator.clipboard 不存在 —— 退回 execCommand
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(id);
    } else {
      const area = document.createElement("textarea");
      area.value = id;
      area.setAttribute("readonly", "");
      area.style.position = "fixed";
      area.style.opacity = "0";
      document.body.appendChild(area);
      area.select();
      document.execCommand("copy");
      area.remove();
    }
    copied.value = true;
    if (copiedTimer) clearTimeout(copiedTimer);
    copiedTimer = setTimeout(() => { copied.value = false; }, 1500);
  } catch {
    // 复制失败不打断；ID 本身就显示在旁边，用户可以手选
  }
}

async function logout() {
  hide();
  await auth.logout();
  await navigateTo("/login");
}
</script>

<template>
  <div
    ref="root"
    class="account-menu-anchor"
    :class="{ 'is-open': open }"
    @pointerenter="onPointerEnter"
    @pointerleave="onPointerLeave"
    @focusout="onFocusOut"
  >
    <button
      ref="trigger"
      type="button"
      :class="triggerClass"
      :aria-label="panelLabel"
      aria-haspopup="true"
      :aria-expanded="open"
      :aria-controls="panelId"
      @click="onTriggerClick"
      @keydown="onTriggerKeydown"
    ><slot>{{ initial }}</slot></button>

    <Transition name="account-panel">
      <div v-if="open" :id="panelId" ref="panel" class="account-panel" :class="{ 'is-credits': isCredits }" role="dialog" :aria-label="panelLabel">
        <section v-if="!isCredits" class="ap-card ap-identity">
          <span class="ap-avatar" aria-hidden="true">{{ initial }}</span>
          <div class="ap-identity-text">
            <strong>{{ auth.user?.nickname || 'AITv 用户' }}</strong>
            <small v-if="contact">{{ contact }}</small>
            <span v-if="auth.user?.id" class="ap-uid">
              <span>ID {{ auth.user.id }}</span>
              <button type="button" :title="copied ? '已复制' : '复制用户 ID'" :aria-label="copied ? '已复制' : '复制用户 ID'" @click="copyUserId">
                <Check v-if="copied" :size="12" /><Copy v-else :size="12" />
              </button>
            </span>
          </div>
        </section>

        <section v-if="auth.membershipLoaded" class="ap-card ap-member" :class="membership.kind">
          <span>{{ membershipLine(membership) }}</span>
          <NuxtLink to="/membership" class="ap-pill" @click="hide()">{{ membershipAction(membership) }}</NuxtLink>
        </section>

        <section v-if="auth.creditsLoaded" class="ap-card ap-credits">
          <div class="ap-credits-head">
            <NuxtLink to="/credits" class="ap-balance" @click="hide()">
              积分余额 <strong>{{ auth.creditBalance.toLocaleString('zh-CN') }}</strong> 点<ChevronRight :size="14" />
            </NuxtLink>
            <NuxtLink to="/credits#packages" class="ap-text-link" @click="hide()">充值</NuxtLink>
          </div>
          <!-- 积分面板里各来源一行一条（参考图）；账号面板里横排省地方 -->
          <p v-if="creditParts.length" class="ap-credit-parts" :class="{ 'is-list': isCredits }">
            <span v-for="part in creditParts" :key="part.source">{{ part.label }}{{ isCredits ? '：' : ' ' }}{{ part.amount.toLocaleString('zh-CN') }} 点</span>
          </p>
        </section>

        <nav class="ap-menu" :aria-label="isCredits ? '积分菜单' : '账号菜单'">
          <template v-for="item in menu" :key="item.key">
            <NuxtLink v-if="item.kind === 'link'" :to="item.to" class="ap-menu-item" @click="hide()">
              <component :is="menuIcons[item.key]" :size="17" :stroke-width="1.75" />
              <span>{{ item.label }}</span>
              <em v-if="item.badge" class="ap-badge" :aria-label="`${item.badge} 条未读`">{{ item.badge }}</em>
              <small v-else-if="item.hint">{{ item.hint }}</small>
              <ChevronRight v-else-if="isCredits" :size="15" class="ap-menu-chevron" aria-hidden="true" />
            </NuxtLink>
            <div v-else-if="item.kind === 'theme'" class="ap-menu-item ap-theme-row">
              <component :is="menuIcons.theme" :size="17" :stroke-width="1.75" />
              <span>{{ item.label }}</span>
              <span class="ap-theme-switch" role="group" aria-label="模式切换">
                <button type="button" title="白天模式" aria-label="白天模式" :aria-pressed="theme === 'light'" :class="{ active: theme === 'light' }" @click="setTheme('light')"><Sun :size="14" /></button>
                <button type="button" title="黑夜模式" aria-label="黑夜模式" :aria-pressed="theme === 'dark'" :class="{ active: theme === 'dark' }" @click="setTheme('dark')"><Moon :size="14" /></button>
              </span>
            </div>
            <button v-else type="button" class="ap-menu-item ap-logout" @click="logout">
              <component :is="menuIcons.logout" :size="17" :stroke-width="1.75" />
              <span>{{ item.label }}</span>
            </button>
          </template>
        </nav>
      </div>
    </Transition>
  </div>
</template>
