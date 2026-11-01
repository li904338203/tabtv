/**
 * Markdown → 安全 HTML（AGT-08）。全站唯一的 Markdown 渲染入口，只由
 * `components/common/MarkdownView.vue` 调用。
 *
 * 内容来源包括模型输出与用户粘贴的外部文本，一律按不可信处理。两层防线都要有：
 * - `html: false` 挡住源文本里的 HTML（`<b>x</b>` 显示成字面量，不渲染）；
 * - DOMPurify 挡住 markdown-it 自身或将来插件产出的危险标签/属性/协议。
 * 只留一层的话，将来有人开个插件就破了。
 *
 * ⚠️ 版本钉死（package.json 无 `^`），升级任一依赖都要重跑 `markdown.test.ts`。
 */
import DOMPurify, { type DOMPurify as Purifier } from "dompurify";
import MarkdownIt from "markdown-it";

/**
 * `linkify` 在 markdown-it 的行内阶段先于强调规则识别裸 URL，
 * 所以 `https://example.com/a_b_c` 里的下划线不会变斜体（对标缺陷 #15）。
 * ⚠️ 不开 `typographer`：它会改引号与破折号，用户写的提示词被悄悄改字。
 */
const md = new MarkdownIt({ html: false, linkify: true, breaks: true, typographer: false });

/**
 * 图片按链接文本显示，不渲染 `<img>`：受保护媒体用 `<img src>` 一律 401
 * （必须走 useAuthenticatedMedia），外链图片又会带出 Referer 与追踪像素。
 * 危险协议的图片在 markdown-it 的 validateLink 阶段就不会成为 image token。
 */
md.renderer.rules.image = (tokens, idx, options, env, self) => {
  const token = tokens[idx]!;
  const src = String(token.attrGet("src") ?? "");
  const alt = self.renderInlineAsText(token.children ?? [], options, env).trim();
  const escape = md.utils.escapeHtml;
  return `<a href="${escape(src)}">${escape(alt || src)}</a>`;
};

const ALLOWED_TAGS = [
  "p", "br", "hr",
  "h1", "h2", "h3", "h4", "h5", "h6",
  "strong", "em", "s", "del",
  "code", "pre", "blockquote",
  "ul", "ol", "li",
  "a",
  "table", "thead", "tbody", "tr", "th", "td"
];

const PURIFY_CONFIG = {
  ALLOWED_TAGS,
  ALLOWED_ATTR: ["href", "title", "target", "rel"],
  // 只放行 http(s) 与 mailto；相对地址、javascript:、data: 一律去掉 href
  ALLOWED_URI_REGEXP: /^(https?:|mailto:)/i
};

let purifier: Purifier | null = null;

/**
 * 惰性建一个独立实例，而不是在默认实例上挂钩子：钩子是实例级全局状态，
 * 挂在默认实例上会影响任何别处对 DOMPurify 的使用。
 */
function getPurifier(): Purifier | null {
  if (typeof window === "undefined") return null;
  if (purifier) return purifier;
  const instance = DOMPurify(window);
  if (!instance.isSupported) return null;
  instance.addHook("afterSanitizeAttributes", (node) => {
    // 链接一律新窗口打开并切断 opener / Referer
    if (node.tagName === "A" && node.hasAttribute("href")) {
      node.setAttribute("target", "_blank");
      node.setAttribute("rel", "noopener noreferrer");
    }
  });
  purifier = instance;
  return purifier;
}

/**
 * 第二层防线：按白名单清洗一段 HTML。没有 DOM 时退化为整段转义（宁可显示源码，
 * 也不放出未清洗的 HTML）。单独导出是为了让测试能绕过 `html: false` 直接验这一层 ——
 * 否则这层坏了也不会有任何用例变红。
 */
export function sanitizeHtml(html: string): string {
  const purify = import.meta.server ? null : getPurifier();
  if (!purify) return md.utils.escapeHtml(html);
  return purify.sanitize(html, PURIFY_CONFIG);
}

/** 渲染并清洗。空白输入返回空串。 */
export function renderMarkdown(source: string): string {
  if (!source || !source.trim()) return "";
  const purify = import.meta.server ? null : getPurifier();
  // 服务端没有 DOM，不在那里跑 DOMPurify：只给转义后的纯文本，客户端再渲染
  if (!purify) return md.utils.escapeHtml(source);
  return purify.sanitize(md.render(source), PURIFY_CONFIG);
}
