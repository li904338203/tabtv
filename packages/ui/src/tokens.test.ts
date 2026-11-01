import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join } from 'node:path'

import { describe, expect, it } from 'vitest'

/**
 * 设计 token 的绊线（P15-01、FE-11）。
 *
 * ⚠️ 起因：换 token 层时发现 **11 个 token 被引用却从来没有定义过**
 * （`--color-accent` / `--color-border` / `--surface-base` / `--fs-15` …，
 * 共 28 处引用）。CSS 变量**取不到值时既不报错也不告警**，组件就那样渲染成
 * 没有背景、没有描边 —— 而它看起来只是"这块样式还没做"。
 * 编译过、类型检查过、测试全绿，只有肉眼能发现，而肉眼看不出"少了一条 1px 的线"。
 *
 * 根因是**两套命名并存**（`--color-*` 与语义名各一套）。修法是改组件指向
 * 正确的名字，**不是给拼错的名字补定义** —— 补定义等于把两套命名坐实。
 */

const ROOT = join(__dirname, '../../..')
const TOKENS = join(__dirname, 'tokens.css')

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    if (name === 'node_modules' || name === '.nuxt' || name === '.output' || name === 'dist') continue
    const p = join(dir, name)
    if (statSync(p).isDirectory()) walk(p, out)
    else if (/\.(vue|css|ts)$/.test(name)) out.push(p)
  }
  return out
}

/**
 * 定义源 = `tokens.css` **加上**各 app 全局加载的样式表。
 *
 * ⚠️ 原先只认 `tokens.css` 一处。移植老前端之后 `frontend/assets/css/canvas.css`
 * 带来了自己的一整套 `--cv-*` / `--canvas-*`（它在 `nuxt.config.ts` 的 `css`
 * 数组里全局加载，`:root` 里实打实定义着），于是这条断言开始报出二十几个
 * "未定义" —— **而它们全都定义了**。
 *
 * ⚠️ 这是"红的是断言不是产品"的又一次：照着它去给 canvas.css 里的名字补定义，
 * 就会把同一个 token 定义两遍，而那才是真正的两套命名并存。
 *
 * 这条规则要守住的东西没有变：**引用了却哪儿都没定义**（CSS 变量取不到值时
 * 既不报错也不告警，组件就那样渲染成没有背景、没有描边）。
 */
function definedTokens(): Set<string> {
  const defined = new Set<string>()
  for (const file of allSourceFiles()) {
    // ⚠️ **不能锚 `^`**：`:root { --portal-page-max: 1314px; --portal-page-gutter: 64px; }`
    //    这种单行写法、以及 `.canvas-template-grid button { --template-accent: …; }`
    //    这种局部作用域的赋值，都不在行首 —— 锚了就把它们当成"没定义"，
    //    而它们定义得好好的。**报一个假的缺失会让人去补第二份定义，
    //    而两套并存正是这条规则要防的事**。
    for (const m of readFileSync(file, 'utf8').matchAll(/(--[a-z0-9-]+)\s*:\s*[^;]/g)) {
      defined.add(m[1]!)
    }
  }
  return defined
}

function allSourceFiles(): string[] {
  return [join(ROOT, 'frontend'), join(ROOT, 'packages')].flatMap(dir => walk(dir))
}

function referencedTokens(): Map<string, string[]> {
  const refs = new Map<string, string[]>()
  {
    for (const file of allSourceFiles()) {
      const text = readFileSync(file, 'utf8')
      for (const m of text.matchAll(/var\((--[a-z0-9-]+)\)/g)) {
        const name = m[1]!
        if (!refs.has(name)) refs.set(name, [])
        const list = refs.get(name)!
        if (!list.includes(file)) list.push(file)
      }
    }
  }
  return refs
}

describe('设计 token', () => {
  // ⚠️ 这条是整个文件的理由：CSS 变量取不到值时**静默失败**。
  it('被引用的 token 必须都有定义', () => {
    const defined = definedTokens()
    const dangling: string[] = []
    for (const [name, files] of referencedTokens()) {
      if (!defined.has(name)) {
        dangling.push(`${name}（${files.length} 个文件，如 ${files[0]!.replace(ROOT, '')}）`)
      }
    }
    expect(dangling, `这些 token 被引用却没有定义 —— 组件会静默渲染成没有颜色：\n${dangling.join('\n')}`)
      .toEqual([])
  })

  // ⚠️ 基线是**老系统**（已通过评审）：深色优先、品牌青。
  // 这条钉住的是 P15-01 的那个决定本身 —— 有人改回浅色/玫红时要当场红。
  it('深色优先，品牌色是老系统的青', () => {
    const css = readFileSync(TOKENS, 'utf8')
    expect(css).toMatch(/--portal-cyan:\s*#32bfd0/)
    // 默认（无 data-theme）即深色：老系统 `:root` 与 `:root[data-theme="dark"]` 同块。
    expect(css).toMatch(/:root\[data-theme="dark"\]/)
    expect(css).toMatch(/:root\[data-theme="light"\]/)
    // 语义名映射到 portal，而不是自己再定一个色值。
    expect(css).toMatch(/--brand-primary:\s*var\(--portal-cyan\)/)
  })

  // ⚠️ 别名层只做映射：出现一个 portal 里没有的 hex，就等于开了第二个色彩来源，
  // 而那正是「切主题漏色」的起点。图表序列色是**写明过的例外**（docs/10 §5.1）。
  it('别名层不引入 portal 之外的色值（图表序列色除外）', () => {
    const css = readFileSync(TOKENS, 'utf8')
    const aliasPart = css.slice(css.indexOf('语义别名层'))
    const hexes = [...aliasPart.matchAll(/(--[a-z0-9-]+):\s*(#[0-9a-fA-F]{3,8})/g)]
    for (const [, name] of hexes) {
      expect(name, `${name} 在别名层里直接写了色值`).toMatch(/^--series-/)
    }
  })
})
