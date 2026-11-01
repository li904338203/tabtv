// @vitest-environment jsdom
/**
 * Markdown 渲染的 XSS 与解析回归（AGT-08）。
 *
 * ⚠️ markdown-it / dompurify 的版本是钉死的：升级任何一个都要重跑这组用例 ——
 * XSS 回归只能靠它发现。DOM 环境只对本文件生效（文件头注释），不改全局环境。
 */
import { describe, expect, it } from "vitest";
import { renderMarkdown, sanitizeHtml } from "./markdown";

/** 断言子树里没有任何 on* 属性。 */
function expectNoEventAttrs(el: Element) {
  for (const node of Array.from(el.querySelectorAll("*"))) {
    for (const attr of Array.from(node.attributes)) {
      expect(attr.name.toLowerCase().startsWith("on")).toBe(false);
    }
  }
}

/** 把渲染结果装进一个游离节点，按 DOM 断言而不是按字符串猜。 */
function dom(source: string): HTMLElement {
  const el = document.createElement("div");
  el.innerHTML = renderMarkdown(source);
  return el;
}

describe("renderMarkdown", () => {
  it("清洗 script 标签", () => {
    const html = renderMarkdown("前文\n\n<script>alert(1)</script>\n\n后文");
    expect(html).not.toMatch(/<script/i);
    expect(dom("<script>alert(1)</script>").querySelector("script")).toBeNull();
  });

  it("清洗事件属性", () => {
    // 第一层：html:false，源文本里的标签只是字面量
    const el = dom('<img src="x" onerror="alert(1)">\n\n<a href="https://a.com" onclick="alert(1)">x</a>');
    expect(el.querySelector("img")).toBeNull();
    expectNoEventAttrs(el);

    // 第二层：假设将来有插件放出了原始 HTML，DOMPurify 仍要把它清掉
    const holder = document.createElement("div");
    holder.innerHTML = sanitizeHtml(
      '<img src="x" onerror="alert(1)"><a href="https://a.com" onclick="alert(1)" onmouseover="x()">x</a>' +
        '<svg onload="alert(1)"></svg><p onclick="x()">p</p><script>alert(1)</script>'
    );
    expect(holder.querySelector("img, svg, script")).toBeNull();
    expectNoEventAttrs(holder);
    expect(holder.querySelector("a")?.getAttribute("href")).toBe("https://a.com");
    expect(holder.querySelector("p")?.textContent).toBe("p");
  });

  it("拒绝 javascript 协议链接", () => {
    const sources = [
      "[点我](javascript:alert(1))",
      "[点我](JaVaScRiPt:alert(1))",
      "[点我](  javascript:alert(1))",
      "<javascript:alert(1)>",
      "[点我](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)",
      "[点我](vbscript:msgbox(1))"
    ];
    for (const source of sources) {
      const el = dom(source);
      for (const a of Array.from(el.querySelectorAll("a"))) {
        const href = a.getAttribute("href") ?? "";
        expect(href).toMatch(/^(https?:|mailto:)/i);
      }
      expect(el.innerHTML).not.toMatch(/href="\s*(javascript|data|vbscript):/i);
    }
    // 第二层：绕过 markdown-it 的 validateLink，DOMPurify 也要去掉危险 href
    const holder = document.createElement("div");
    holder.innerHTML = sanitizeHtml(
      '<a href="javascript:alert(1)">a</a><a href=" JAVASCRIPT:alert(1)">b</a><a href="data:text/html,x">c</a><a href="/relative">d</a>'
    );
    for (const a of Array.from(holder.querySelectorAll("a"))) {
      expect(a.hasAttribute("href")).toBe(false);
      expect(a.hasAttribute("target")).toBe(false);
    }
  });

  it("拒绝 iframe 与内联样式", () => {
    const el = dom('<iframe src="https://evil.com"></iframe>\n\n<p style="color:red">x</p>');
    expect(el.querySelector("iframe")).toBeNull();
    expect(el.querySelector("[style]")).toBeNull();
    const holder = document.createElement("div");
    holder.innerHTML = sanitizeHtml('<iframe src="https://evil.com"></iframe><p style="color:red">x</p>');
    expect(holder.querySelector("iframe")).toBeNull();
    expect(holder.querySelector("[style]")).toBeNull();
    // 表格对齐会让 markdown-it 自己产出 style —— 那一层也要被清掉
    const table = dom("| a | b |\n|:--|--:|\n| 1 | 2 |");
    expect(table.querySelector("table")).not.toBeNull();
    expect(table.querySelector("[style]")).toBeNull();
  });

  it("URL 里的下划线不变斜体", () => {
    const url = "https://example.com/a_b_c_d";
    const bare = dom(`看这里 ${url} 结束`);
    expect(bare.querySelector("em")).toBeNull();
    expect(bare.querySelector("a")?.getAttribute("href")).toBe(url);

    const linked = dom(`[文档](https://example.com/x_y_z?q=a_b)`);
    expect(linked.querySelector("em")).toBeNull();
    expect(linked.querySelector("a")?.getAttribute("href")).toBe("https://example.com/x_y_z?q=a_b");

    const two = dom("https://a.com/x_y 和 https://b.com/p_q");
    expect(two.querySelector("em")).toBeNull();
    expect(Array.from(two.querySelectorAll("a")).map((a) => a.getAttribute("href"))).toEqual([
      "https://a.com/x_y",
      "https://b.com/p_q"
    ]);
  });

  it("裸文本 snake_case 不变斜体", () => {
    const el = dom("变量 snake_case_name 与 another_long_name 都是原样");
    expect(el.querySelector("em")).toBeNull();
    expect(el.textContent).toContain("snake_case_name");
    expect(el.textContent).toContain("another_long_name");
  });

  it("源文本 HTML 被转义", () => {
    const el = dom("<b>x</b>");
    expect(el.querySelector("b")).toBeNull();
    expect(el.textContent).toContain("<b>x</b>");
  });

  it("裸 URL 自动成链接", () => {
    const a = dom("访问 https://example.com/path 查看").querySelector("a");
    expect(a?.getAttribute("href")).toBe("https://example.com/path");
    expect(a?.textContent).toBe("https://example.com/path");
  });

  it("外链新窗口且 noopener", () => {
    const el = dom("[外链](https://example.com) 与 https://b.com 与 [信](mailto:a@b.com)");
    const links = Array.from(el.querySelectorAll("a"));
    expect(links).toHaveLength(3);
    for (const a of links) {
      expect(a.getAttribute("target")).toBe("_blank");
      expect(a.getAttribute("rel")).toBe("noopener noreferrer");
    }
  });

  it("图片语法显示为链接文本不渲染 img", () => {
    const el = dom("![角色设定](https://example.com/a.png)");
    expect(el.querySelector("img")).toBeNull();
    const a = el.querySelector("a");
    expect(a?.textContent).toBe("角色设定");
    expect(a?.getAttribute("href")).toBe("https://example.com/a.png");

    // 没有 alt 时用地址当文本，不显示成一个空链接
    const noAlt = dom("![](https://example.com/b.png)").querySelector("a");
    expect(noAlt?.textContent).toBe("https://example.com/b.png");

    // 危险协议的图片不成链接
    const evil = dom('![x](javascript:alert(1))');
    expect(evil.querySelector("img")).toBeNull();
    expect(evil.innerHTML).not.toMatch(/href="javascript:/i);
  });

  it("代码块原样保留", () => {
    const code = 'const a_b = "<b>x</b>"; // _not_ em';
    const el = dom("```ts\n" + code + "\n```");
    const pre = el.querySelector("pre code");
    expect(pre?.textContent).toBe(code + "\n");
    expect(el.querySelector("b")).toBeNull();
    expect(el.querySelector("em")).toBeNull();
    expect(dom("行内 `a_b_c` 代码").querySelector("code")?.textContent).toBe("a_b_c");
  });

  it("空字符串返回空串", () => {
    expect(renderMarkdown("")).toBe("");
    expect(renderMarkdown("   \n  ")).toBe("");
  });

  it("常规 Markdown 结构正常渲染", () => {
    const el = dom("# 标题\n\n**粗** *斜*\n第二行\n\n- 一\n- 二\n\n> 引用");
    expect(el.querySelector("h1")?.textContent).toBe("标题");
    expect(el.querySelector("strong")?.textContent).toBe("粗");
    expect(el.querySelector("em")?.textContent).toBe("斜");
    // breaks: true —— 单个换行变 <br>
    expect(el.querySelector("p br")).not.toBeNull();
    expect(el.querySelectorAll("li")).toHaveLength(2);
    expect(el.querySelector("blockquote")).not.toBeNull();
  });

  it("不改写用户的引号与破折号", () => {
    // 不开 typographer：用户写的提示词不能被悄悄改字
    expect(dom('他说 "你好" -- 再见...').textContent?.trim()).toBe('他说 "你好" -- 再见...');
  });
});
