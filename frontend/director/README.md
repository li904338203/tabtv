# frontend/director/

3D 导演台的浏览器端引擎与资源加载（`docs/agent/director/dev/03-前端开发设计.md`，DRD-33）。

- **这个目录不在 Nuxt 自动导入范围里**：`utils/`、`composables/` 下的导出会被 Nuxt 按名字自动导入，只要画布引用了其中一个名字，它 import 的 three 就会被拉进画布首屏的包里。所以用到 three 的代码一律放在这里。
- **能 import three 的只有两处**：这个目录，以及 `components/director/*`。`utils/` 与 `composables/` 里**不得** import three，否则 `pnpm --filter @tabtv/web run bundle:director` 会红（它检查画布页的静态依赖）。
- **编辑器只有一个入口**：画布里的 `defineAsyncComponent(() => import("~/components/director/DirectorStudio.vue"))`。不要写 `<DirectorStudio>` 或 `<LazyDirectorStudio>` 标签：前者经自动注册变成静态 import，后者会多出第二个引用点。
- 与 three 无关的纯函数（场景、机位、时间轴……）放 `utils/director*.ts`，加 `director` 前缀并显式 import。
