<script setup lang="ts">
/**
 * 运镜卡片上的示意动画：同一个小场景（城市、桥面、人物），按运镜 code 让「镜头」动起来 ——
 * 推近就放大、左摇就整体右移、上升时前景比背景移得多（视差）、柯克变焦是背景拉伸而人物大小不变……
 *
 * 没有封面时用它（运营在后台给运镜配了封面就显示封面，见 MaterialPlaza）：运镜的价值在「镜头怎么动」，
 * 一张静态图说明不了；给 23 种运镜各拍一段示意视频则要花真钱。认不出的 code 显示静止的场景。
 * ⚠️ 只在悬停（playing）时动：一屏二十多张卡片一直动，费电也晃眼；系统开了「减少动态效果」时一律不动。
 */
defineProps<{ code: string; playing?: boolean }>();
</script>

<template>
  <div class="cm-preview" :class="[`m-${code}`, { playing }]" aria-hidden="true">
    <svg viewBox="0 0 120 120" preserveAspectRatio="xMidYMid slice">
      <g class="cm-world">
        <g class="cm-far">
          <rect x="-40" y="30" width="26" height="80" fill="#17212c" />
          <rect x="-12" y="14" width="22" height="96" fill="#1d2935" />
          <rect x="12" y="38" width="18" height="72" fill="#16202a" />
          <rect x="32" y="6" width="20" height="104" fill="#1b2733" />
          <rect x="70" y="18" width="22" height="92" fill="#1a2531" />
          <rect x="94" y="34" width="18" height="76" fill="#16202a" />
          <rect x="114" y="10" width="26" height="100" fill="#1d2935" />
          <rect x="142" y="40" width="22" height="70" fill="#17212c" />
          <g fill="#3d5266">
            <rect x="-8" y="22" width="3" height="2" /><rect x="-2" y="30" width="3" height="2" />
            <rect x="36" y="14" width="3" height="2" /><rect x="44" y="26" width="3" height="2" />
            <rect x="74" y="28" width="3" height="2" /><rect x="84" y="40" width="3" height="2" />
            <rect x="118" y="20" width="3" height="2" /><rect x="128" y="34" width="3" height="2" />
          </g>
          <rect x="40" y="34" width="2" height="16" fill="#ff4d5e" opacity=".85" />
          <rect x="88" y="26" width="2" height="12" fill="#ff4d5e" opacity=".75" />
          <rect x="16" y="46" width="2" height="9" fill="#38bdf8" opacity=".7" />
        </g>
        <g class="cm-near">
          <!-- 桥面一直画到画框外很远：拉远、俯拍（缩小画面）时底边不能露出来 -->
          <polygon points="-188,220 308,220 84,80 36,80" fill="#111921" />
          <line x1="60" y1="80" x2="60" y2="220" stroke="#3a4d5f" stroke-width=".8" stroke-dasharray="4 4" />
          <line x1="36" y1="80" x2="-188" y2="220" stroke="#5f7386" stroke-width="1.4" />
          <line x1="84" y1="80" x2="308" y2="220" stroke="#5f7386" stroke-width="1.4" />
          <line x1="30" y1="74" x2="-229" y2="220" stroke="#6f8396" stroke-width="1" />
          <line x1="90" y1="74" x2="349" y2="220" stroke="#6f8396" stroke-width="1" />
        </g>
        <g class="cm-subject">
          <rect x="53.4" y="88" width="5.2" height="20" rx="2.2" fill="#8f99a4" />
          <rect x="61.4" y="88" width="5.2" height="20" rx="2.2" fill="#8f99a4" />
          <path d="M50.5 68 Q60 63.5 69.5 68 L71 89 L49 89 Z" fill="#b9c2cb" />
          <path d="M53 55 Q60 46 67 55 L68 70 Q60 66 52 70 Z" fill="#eef0f2" />
          <ellipse cx="60" cy="58.5" rx="5" ry="5.8" fill="#e8e1da" />
          <rect x="55.4" y="56.8" width="9.2" height="2.2" rx="1.1" fill="#16191d" />
        </g>
      </g>
    </svg>
  </div>
</template>

<style scoped>
/* 画面是「媒体」，两套主题同色（同视频缩略图）。 */
.cm-preview {
  width: 100%;
  height: 100%;
  overflow: hidden;
  background: linear-gradient(180deg, #0c131b 0%, #1a2632 46%, #33475a 100%);
}
.cm-preview svg { width: 100%; height: 100%; display: block; }
.cm-world,
.cm-far,
.cm-near,
.cm-subject {
  transform-box: view-box;
  transform-origin: 60px 76px;
  animation-duration: 2.8s;
  animation-timing-function: ease-in-out;
  animation-iteration-count: infinite;
  animation-direction: alternate;
  animation-play-state: paused;
}
.cm-preview.playing :is(.cm-world, .cm-far, .cm-near, .cm-subject) { animation-play-state: running; }

/* 固定镜头：镜头不动，只有人物有一点呼吸。 */
.m-fixed .cm-subject { animation-name: cm-idle; animation-duration: 1.6s; }
/* 跟随：背景向后掠过（前景比远景快），人物一直在中间、带着步伐。 */
.m-follow .cm-far { animation-name: cm-follow-far; }
.m-follow .cm-near { animation-name: cm-follow-near; }
.m-follow .cm-subject { animation-name: cm-walk; animation-duration: .5s; }
/* 盘旋抬升 / 下降：边转边升（降），越来越俯视。 */
.m-orbit_rise .cm-world { animation-name: cm-orbit-rise; }
.m-orbit_descend .cm-world { animation-name: cm-orbit-rise; animation-direction: alternate-reverse; }
/* 摇镜：机位不动，整个画面朝反方向移。 */
.m-tilt_up .cm-world { animation-name: cm-tilt; }
.m-tilt_down .cm-world { animation-name: cm-tilt; animation-direction: alternate-reverse; }
.m-pan_left .cm-world { animation-name: cm-pan; }
.m-pan_right .cm-world { animation-name: cm-pan; animation-direction: alternate-reverse; }
/* 移镜：机位整体移动，前景比远景移得多（视差）。 */
.m-crane_up .cm-far { animation-name: cm-crane-far; }
.m-crane_up :is(.cm-near, .cm-subject) { animation-name: cm-crane-near; }
.m-crane_down .cm-far { animation-name: cm-crane-far; animation-direction: alternate-reverse; }
.m-crane_down :is(.cm-near, .cm-subject) { animation-name: cm-crane-near; animation-direction: alternate-reverse; }
.m-truck_left .cm-far { animation-name: cm-truck-far; }
.m-truck_left :is(.cm-near, .cm-subject) { animation-name: cm-truck-near; }
.m-truck_right .cm-far { animation-name: cm-truck-far; animation-direction: alternate-reverse; }
.m-truck_right :is(.cm-near, .cm-subject) { animation-name: cm-truck-near; animation-direction: alternate-reverse; }
/* 前推 / 后移：近处放大得比远处快；变焦：整幅画面一起放大，透视不变。 */
.m-push_in .cm-far { animation-name: cm-push-far; }
.m-push_in :is(.cm-near, .cm-subject) { animation-name: cm-push-near; }
.m-pull_out .cm-far { animation-name: cm-push-far; animation-direction: alternate-reverse; }
.m-pull_out :is(.cm-near, .cm-subject) { animation-name: cm-push-near; animation-direction: alternate-reverse; }
.m-zoom_in .cm-world { animation-name: cm-zoom; }
.m-zoom_out .cm-world { animation-name: cm-zoom; animation-direction: alternate-reverse; }
/* 柯克变焦：背景被拉伸，人物大小不变。 */
.m-dolly_zoom .cm-far { animation-name: cm-dolly-far; }
.m-dolly_zoom .cm-near { animation-name: cm-dolly-near; }
/* 环绕：远近两层朝相反方向掠过，人物像转了个身。 */
.m-orbit .cm-far { animation-name: cm-orbit-far; }
.m-orbit .cm-near { animation-name: cm-orbit-near; }
.m-orbit .cm-subject { animation-name: cm-orbit-subject; }
.m-roll .cm-world { animation-name: cm-roll; animation-duration: 6s; animation-timing-function: linear; animation-direction: normal; }
.m-fpv .cm-world { animation-name: cm-fpv; animation-duration: 3.2s; animation-timing-function: linear; }
.m-drone .cm-world { animation-name: cm-drone; animation-duration: 3.4s; }
.m-aerial .cm-world { animation-name: cm-aerial; animation-duration: 4s; }
.m-handheld .cm-world { animation-name: cm-shake; animation-duration: 1.4s; }

@keyframes cm-idle { from { transform: translateY(0); } to { transform: translateY(-1px); } }
@keyframes cm-walk { from { transform: translateY(0); } to { transform: translateY(-1.6px); } }
@keyframes cm-follow-far { from { transform: translateX(0); } to { transform: translateX(-10px); } }
@keyframes cm-follow-near { from { transform: translateX(0); } to { transform: translateX(-24px); } }
@keyframes cm-orbit-rise {
  from { transform: rotate(-7deg) translateY(-6px) scale(1.05); }
  to { transform: rotate(7deg) translateY(12px) scale(.84); }
}
@keyframes cm-tilt { from { transform: translateY(-16px); } to { transform: translateY(14px); } }
@keyframes cm-pan { from { transform: translateX(-16px); } to { transform: translateX(16px); } }
@keyframes cm-crane-far { from { transform: translateY(-4px); } to { transform: translateY(5px); } }
@keyframes cm-crane-near { from { transform: translateY(-10px); } to { transform: translateY(15px); } }
@keyframes cm-truck-far { from { transform: translateX(-5px); } to { transform: translateX(5px); } }
@keyframes cm-truck-near { from { transform: translateX(-16px); } to { transform: translateX(16px); } }
@keyframes cm-push-far { from { transform: scale(1); } to { transform: scale(1.12); } }
@keyframes cm-push-near { from { transform: scale(1); } to { transform: scale(1.5); } }
@keyframes cm-zoom { from { transform: scale(1); } to { transform: scale(1.55); } }
@keyframes cm-dolly-far { from { transform: scale(1); } to { transform: scale(1.75); } }
@keyframes cm-dolly-near { from { transform: scale(1); } to { transform: scale(1.25); } }
@keyframes cm-orbit-far { from { transform: translateX(16px); } to { transform: translateX(-16px); } }
@keyframes cm-orbit-near { from { transform: translateX(8px); } to { transform: translateX(-8px); } }
@keyframes cm-orbit-subject { from { transform: scaleX(1); } to { transform: scaleX(.78); } }
@keyframes cm-roll { from { transform: rotate(0deg) scale(1.32); } to { transform: rotate(360deg) scale(1.32); } }
@keyframes cm-fpv {
  0% { transform: scale(1) translateY(0); }
  25% { transform: scale(1.08) translateY(1.6px); }
  50% { transform: scale(1.16) translateY(0); }
  75% { transform: scale(1.24) translateY(1.6px); }
  100% { transform: scale(1.32) translateY(0); }
}
@keyframes cm-drone { from { transform: translateY(-8px) scale(1.05); } to { transform: translateY(12px) scale(1.3); } }
@keyframes cm-aerial { from { transform: scale(.62) rotate(-4deg); } to { transform: scale(.7) rotate(6deg); } }
@keyframes cm-shake {
  0% { transform: translate(0, 0) rotate(0deg); }
  20% { transform: translate(1.4px, -.8px) rotate(.6deg); }
  40% { transform: translate(-1px, 1px) rotate(-.5deg); }
  60% { transform: translate(1.2px, .6px) rotate(.4deg); }
  80% { transform: translate(-1.4px, -.6px) rotate(-.6deg); }
  100% { transform: translate(.6px, 1px) rotate(.3deg); }
}

@media (prefers-reduced-motion: reduce) {
  .cm-preview * { animation: none !important; }
}
</style>
