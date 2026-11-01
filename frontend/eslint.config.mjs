// @ts-check

// eslint-flat-config-utils uses the ES2024 helper that is not available in Node 20.
if (!Object.groupBy) {
  Object.groupBy = (items, callback) => {
    const groups = Object.create(null)
    let index = 0
    for (const item of items) {
      const key = callback(item, index++)
      groups[key] ??= []
      groups[key].push(item)
    }
    return groups
  }
}

const { default: withNuxt } = await import('./.nuxt/eslint.config.mjs')

export default withNuxt({
  rules: {
    // AGT-08：全站 v-html 只允许出现在 components/common/MarkdownView.vue（该处单行豁免）。
    // 要显示 Markdown 一律用那个组件 —— 它的内容经过 html:false + DOMPurify 两层清洗。
    'vue/no-v-html': 'error',
  },
})
