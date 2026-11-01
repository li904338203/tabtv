import { fileURLToPath } from "node:url";

export default defineNuxtConfig({
  compatibilityDate: "2026-07-01",
  devtools: { enabled: false },
  // 桌面应用：前端是静态产物，由 Wails 的 AssetServer 直接提供（dev/03 TTD-30）。
  ssr: false,
  modules: ["@pinia/nuxt", "@nuxt/eslint"],
  css: [
    "element-plus/dist/index.css",
    // Element Plus 暗色变量，作用于 html.dark（由 useTheme 的 apply 切换）
    "element-plus/theme-chalk/dark/css-vars.css",
    "@vue-flow/core/dist/style.css",
    "@vue-flow/core/dist/theme-default.css",
    "@vue-flow/minimap/dist/style.css",
    "~/assets/css/main.css",
    "~/assets/css/canvas.css"
  ],
  app: {
    head: {
      // 品牌图标（tabtv）：标签页 32px、iOS 主屏 180px；/favicon.ico 也放着，给只认根路径的浏览器与爬虫
      link: [
        { rel: "icon", type: "image/png", sizes: "32x32", href: "/brand/favicon-32.png" },
        { rel: "apple-touch-icon", sizes: "180x180", href: "/brand/apple-touch-icon.png" }
      ],
      script: [
        {
          // 首次绘制前同步定主题，避免暗色底闪一帧白。
          // 与 composables/useTheme.ts 的 resolveTheme 保持同一套判定：
          // 有偏好用偏好，否则跟随系统，无法判断兜底 dark。
          key: "theme-preflight",
          innerHTML:
            "(function(){try{var p=localStorage.getItem('aitv-theme');" +
            "var t=(p==='light'||p==='dark')?p:" +
            "(window.matchMedia('(prefers-color-scheme: light)').matches?'light':'dark');" +
            "document.documentElement.dataset.theme=t;}catch(e){" +
            "document.documentElement.dataset.theme='dark';}})();",
          tagPosition: "head"
        }
      ]
    }
  },
  // 开发服务监听全部地址（IPv4 + IPv6）。只听 localhost 的话，别的设备（手机、同事的电脑）
  // 用局域网地址打不开页面；本机用 127.0.0.1 也打不开（它只绑在了 [::1] 上）。
  devServer: {
    host: "::"
  },
  runtimeConfig: {
    public: {
      apiBase: "/api/v1",
      // ⚠️ 新后端只有**一条** WS：`/ws/events`（老系统是 /ws/generation 与
      // /ws/writing 两条）。写作域已排除，所以这里也只剩一条。
      // 留空 = 跟着页面的主机名走（utils/realtime 的 realtimeUrl）：开发时补 API 端口 wsApiPort，
      // 生产构建不补（见 $production）。要连别的地址就设 NUXT_PUBLIC_WS_URL（完整地址）。
      // ⚠️ 此前写死 `ws://localhost:8180`，从别的设备打开页面时 localhost 指向那台设备自己，实时通道连不上。
      wsUrl: "",
      wsApiPort: "8180",
      paymentChannel: "dev",
      // 智能剪辑里要调用模型的功能（AI 剪辑、识别字幕、翻译、补镜、重新生成、去字幕、配乐与音效）：
      // 这一版只保留手动剪辑，缺省关（utils/smartcutFeatures）。打开：NUXT_PUBLIC_SMARTCUT_AI=true。
      smartcutAi: false
    }
  },
  // 3D 导演台的开发用沙盒（DRT-12，dev/03 §14）：**只在开发态**注册 `/__director-sandbox`，生产构建里没有这个路由。
  // 画布接入之前，引擎与编辑器的浏览器检查都在这里做（检查脚本拦截场景 fixture，不碰用户的真实画布）。
  $development: {
    hooks: {
      "pages:extend"(pages) {
        pages.push({
          name: "director-sandbox",
          path: "/__director-sandbox",
          file: fileURLToPath(new URL("./director/dev/DirectorSandbox.vue", import.meta.url))
        });
        // 智能剪辑的开发用沙盒（SCT-20）：同上，只在开发态注册，检查脚本拦截接口返回 fixture。
        pages.push({
          name: "smartcut-sandbox",
          path: "/__smartcut-sandbox",
          file: fileURLToPath(new URL("./smartcut/dev/SmartcutSandbox.vue", import.meta.url))
        });
      }
    }
  },
  // 三平台探针（TT-T-03）：只在 TABTV_PROBE=1 构建时注册 `/__probe`，正式产物里没有这个路由。
  hooks: {
    "pages:extend"(pages) {
      if (process.env.TABTV_PROBE !== "1") return;
      // 探针构建把首页也换成探针页：桌面窗口打开即是 "/"，不必手动跳转。
      const home = pages.find(p => p.path === "/");
      if (home) home.file = fileURLToPath(new URL("./probe/ProbePage.vue", import.meta.url));
      pages.push({
        name: "probe",
        path: "/__probe",
        file: fileURLToPath(new URL("./probe/ProbePage.vue", import.meta.url))
      });
    }
  },
  // 生产里页面与 API 经反向代理同域：实时通道连页面自己的域名，不补端口。
  $production: {
    runtimeConfig: {
      public: {
        wsApiPort: ""
      }
    }
  },
  // ⚠️ @lucide/vue 必须与应用打进同一份 Vue，否则**只有生产 SSR 会 500**。
  //
  // 成因：pnpm 给 lucide 装了隔离的 vue 副本，Nitro 又把 lucide 当外部依赖放进
  // .output/server/node_modules，于是产物里存在两份 vue。lucide 的图标组件调
  // inject(LUCIDE_CONTEXT, {}) 时用的是它自己那份 vue —— 那份的 currentInstance 始终为 null，
  // 而 Vue 在 setup 上下文之外会**忽略默认值直接返回 undefined**，于是解构 size 就炸。
  // 开发态由 Vite 统一解析依赖，碰不到这条路径，所以本地怎么试都是好的。
  build: {
    transpile: ["@lucide/vue"]
  },
  vite: {
    resolve: { dedupe: ["vue"] },
    // 3D 导演台的姿势识别跑在 Web Worker 里（DRT-23，DRD-41）：MediaPipe 的模块化 Worker 要 ES 格式。
    worker: { format: "es" },
    // 开发态预先打包导演台懒加载的库（DRT-25 / 23 / 28）：只在第一次导出、识别姿势、加载泼溅时才 import 的话，
    // Vite 那时才发现它、重新打包并整页刷新 —— 导出做到一半、识别做到一半页面没了。
    // 只影响开发服务器，不改生产构建的分包（MediaPipe 只在 Worker 的包里）。
    optimizeDeps: { include: ["mediabunny", "@mediapipe/tasks-vision", "@sparkjsdev/spark"] }
  },
  nitro: {
    // ⚠️ pnpm 的非扁平 node_modules 会让 Nitro 的依赖追踪漏掉部分传递依赖：
    // element-plus 的 avatar-group 运行时 import @popperjs/core，但它没被复制进
    // .output/server/node_modules，结果是**只有生产构建、且只有用到该组件的路由**才 500
    // （ERR_MODULE_NOT_FOUND），首页完全正常，所以本地只试首页是发现不了的。
    externals: {
      // 把 element-plus 打进服务端 bundle，而不是留作 .output/server/node_modules 下的外部依赖。
      // 外部化时它的传递依赖要靠 Nitro 追踪复制，而 pnpm 的非扁平 node_modules 会让追踪漏包
      // （实测漏了 @popperjs/core）。内联后没有运行时解析，也就没有漏包的可能。
      inline: ["element-plus"]
    }
  },
  typescript: {
    strict: true,
    typeCheck: true
  }
});
