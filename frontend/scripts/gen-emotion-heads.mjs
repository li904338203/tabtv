#!/usr/bin/env node
/**
 * 情绪调节的 25 张示意头像（docs/agent/imagetools IMG-F-16、IMGD-28）：**程序化生成 SVG**，纯 Node、无依赖。
 *
 * 用法：`node frontend/scripts/gen-emotion-heads.mjs`（幂等，重复跑产物逐字节相同）。
 * 产物：`frontend/public/imagetools/emotion/{arousal}_{warmth}.svg`，arousal / warmth 各取 -2..2，共 25 个。
 *
 * 为什么不用图片模型生成：同一个头像在 25 张图里必须**只有表情在变**（脸型、光线、角度都不动），
 * 否则用户会把「换了一个人」误读成「情绪不同」。程序化画出来天然满足这一点，体积也只有几 KB 一张。
 *
 * 坐标含义与服务端 `imagetool/domain.Emotions` 一致：
 * - arousal：+2 激动 … -2 平静 → 眼睛开合、眉毛高低、嘴张开的程度；
 * - warmth：+2 亲近 … -2 疏离 → 嘴角上扬 / 下撇；疏离且激动 = 怒（眉头内侧压低），疏离且平静 = 哀（眉头内侧抬起）。
 */
import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const OUT_DIR = join(dirname(fileURLToPath(import.meta.url)), "..", "public", "imagetools", "emotion");
const MAX_BYTES = 6 * 1024;

/** 保留一位小数，产物稳定且短。 */
const n = (value) => Number(value.toFixed(1));

/** 一张头像的 SVG 文本。 */
export function emotionHeadSvg(arousal, warmth) {
  const a = arousal;
  const w = warmth;
  const pos = Math.max(0, w);
  const neg = Math.max(0, -w);
  const excited = Math.max(0, a);
  const calm = Math.max(0, -a);

  // —— 眼睛：激动睁大、平静半闭；亲近时笑眼（下眼睑上推）
  const eyeOpen = Math.max(0.6, 5.2 + 1.5 * a - 0.7 * pos * excited - 0.4 * neg * calm);
  const eyeY = 150;
  const eyeDx = 30;

  // —— 眉毛：整体随激动抬高；怒 = 内侧压低，哀 = 内侧抬起，亲近 = 轻微上挑的弧
  const browBase = 128 - 3 * a;
  const anger = neg * excited;
  const sorrow = neg * (calm + (a === 0 ? 0.5 : 0));
  const innerDy = 2.2 * anger - 2.4 * sorrow - 0.8 * pos;
  const outerDy = -0.6 * anger + 1.2 * sorrow;
  const browArc = -2 - 1.2 * pos + 0.8 * anger;

  // —— 嘴：嘴角高低按 warmth，张开程度按 arousal（惊讶那格 a=+1,w=0 是「O」形）
  const mouthY = 206;
  const smile = 5 * w - 1.5 * anger;
  const halfW = 19 + 3.5 * pos + 2 * excited - 2 * calm - 1.5 * neg * calm;
  const surprise = a > 0 && w === 0;
  const open = surprise ? 7 + 3 * a : Math.max(0, 7 * (a - 0.3) + (anger > 2 ? 3 : 0));

  const brow = (side) => {
    const s = side; // -1 左 / +1 右
    const inner = { x: 120 + s * 12, y: browBase + innerDy };
    const outer = { x: 120 + s * 48, y: browBase + outerDy };
    const ctrl = { x: (inner.x + outer.x) / 2, y: (inner.y + outer.y) / 2 + browArc };
    return `M${n(inner.x)} ${n(inner.y)}Q${n(ctrl.x)} ${n(ctrl.y)} ${n(outer.x)} ${n(outer.y)}`;
  };

  const eye = (side) => {
    const cx = 120 + side * eyeDx;
    const ry = n(eyeOpen);
    const lid = n(eyeY - eyeOpen - 1.5);
    return [
      `<ellipse cx="${cx}" cy="${eyeY}" rx="11" ry="${ry}" fill="url(#ey)" stroke="#77756f" stroke-width="1.6"/>`,
      eyeOpen > 1.4 ? `<circle cx="${cx}" cy="${eyeY}" r="${n(Math.min(4.2, eyeOpen * 0.75))}" fill="#4a4c50" opacity=".55"/>` : "",
      `<path d="M${cx - 13} ${lid + 2}Q${cx} ${lid - 2} ${cx + 13} ${lid + 2}" class="l"/>`
    ].join("");
  };

  const left = { x: 120 - halfW, y: mouthY - smile };
  const right = { x: 120 + halfW, y: mouthY - smile };
  const upperCtrlY = mouthY + smile * 0.7 - (surprise ? open : open * 0.25);
  const lowerCtrlY = mouthY + smile * 0.7 + open * 2.2;
  const ends = `${n(left.x)} ${n(left.y)}`;
  const mouth = open > 1
    ? `<path d="M${ends}Q120 ${n(upperCtrlY)} ${n(right.x)} ${n(right.y)}Q120 ${n(lowerCtrlY)} ${ends}Z" fill="#55575c" stroke="#6b6d72" stroke-width="2.4" stroke-linejoin="round"/>`
    : `<path d="M${ends}Q120 ${n(upperCtrlY + 1)} ${n(right.x)} ${n(right.y)}" class="l" stroke-width="3"/>`;

  // 笑时的法令纹，怒时的眉间纹：都很淡，只为让表情读得出来
  const folds = [];
  if (pos >= 1) {
    folds.push(`<path d="M${n(120 - halfW - 8)} ${n(mouthY - 22)}Q${n(120 - halfW - 12)} ${mouthY - 6} ${n(120 - halfW - 4)} ${n(mouthY + 4)}" class="f"/>`);
    folds.push(`<path d="M${n(120 + halfW + 8)} ${n(mouthY - 22)}Q${n(120 + halfW + 12)} ${mouthY - 6} ${n(120 + halfW + 4)} ${n(mouthY + 4)}" class="f"/>`);
  }
  if (anger >= 1) folds.push(`<path d="M114 ${n(browBase + 4)}l2 9M126 ${n(browBase + 4)}l-2 9" class="f"/>`);
  if (sorrow >= 1.5) folds.push(`<path d="M106 ${n(browBase - 9)}q14 -4 28 0" class="f"/>`);

  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 240 280" role="img" aria-label="arousal ${a} warmth ${w}">`
    + "<defs>"
    + `<radialGradient id="sk" cx=".42" cy=".36" r=".72"><stop offset="0" stop-color="#eceae6"/><stop offset=".55" stop-color="#c9c7c3"/><stop offset="1" stop-color="#8f8e8b"/></radialGradient>`
    + `<linearGradient id="nk" x1="0" x2="1"><stop offset="0" stop-color="#a9a7a3"/><stop offset=".5" stop-color="#c4c2be"/><stop offset="1" stop-color="#8a8986"/></linearGradient>`
    + `<radialGradient id="ey" cx=".5" cy=".4" r=".6"><stop offset="0" stop-color="#d6d4d0"/><stop offset="1" stop-color="#9c9a97"/></radialGradient>`
    + `<radialGradient id="bg" cx=".5" cy=".45" r=".7"><stop offset="0" stop-color="#6d6f74"/><stop offset="1" stop-color="#3a3c40"/></radialGradient>`
    + "<style>.l{fill:none;stroke:#7a7975;stroke-width:3.4;stroke-linecap:round}.f{fill:none;stroke:#9a9894;stroke-width:1.6;stroke-linecap:round;opacity:.8}</style>"
    + "</defs>"
    + `<rect width="240" height="280" fill="url(#bg)"/>`
    // 脖子与肩
    + `<path d="M92 228L88 270Q120 284 152 270L148 228Z" fill="url(#nk)"/>`
    + `<path d="M30 280Q40 252 88 246L152 246Q200 252 210 280Z" fill="#9e9c98"/>`
    // 耳朵
    + `<ellipse cx="58" cy="158" rx="9" ry="20" fill="#b4b2ae"/><ellipse cx="182" cy="158" rx="9" ry="20" fill="#a3a19d"/>`
    // 头
    + `<path d="M120 52C78 52 58 88 58 136C58 186 84 236 120 240C156 236 182 186 182 136C182 88 162 52 120 52Z" fill="url(#sk)"/>`
    // 鼻子
    + `<path d="M116 150Q113 172 109 182Q114 190 120 188Q126 190 131 182" fill="none" stroke="#9f9d99" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/>`
    + `<path d="${brow(-1)}" class="l" stroke-width="4"/><path d="${brow(1)}" class="l" stroke-width="4"/>`
    + eye(-1) + eye(1)
    + folds.join("")
    + mouth
    + "</svg>\n";
}

function main() {
  mkdirSync(OUT_DIR, { recursive: true });
  let total = 0;
  for (let a = -2; a <= 2; a += 1) {
    for (let w = -2; w <= 2; w += 1) {
      const svg = emotionHeadSvg(a, w);
      const bytes = Buffer.byteLength(svg);
      if (bytes > MAX_BYTES) throw new Error(`${a}_${w}.svg 有 ${bytes} 字节，超过 ${MAX_BYTES}`);
      writeFileSync(join(OUT_DIR, `${a}_${w}.svg`), svg);
      total += 1;
    }
  }
  console.log(`已生成 ${total} 张示意头像 → ${OUT_DIR}`);
}

main();
