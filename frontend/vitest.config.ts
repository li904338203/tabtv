import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

/**
 * ⚠️ 在此之前用户端**没有 vitest 配置**，于是 `~` 别名在测试里根本不存在。
 *
 * 一直没暴露是因为所有 `~/...` 的引用都是 `import type`（编译期擦除，
 * 解析不到也不报错）。第一次出现**值导入**（`utils/canvas.ts` 引入
 * `optionValue`）就整条测试套件加载失败 —— 而报错指向的是被引入的那个新文件，
 * 看起来像"新文件写错了"，真因是别名从来就没配过。
 *
 * 别名与 `tsconfig.json` / Nuxt 的 `~` 保持一致，指向 app 根目录。
 */
export default defineConfig({
  resolve: {
    alias: {
      "~": fileURLToPath(new URL(".", import.meta.url)),
      "@": fileURLToPath(new URL(".", import.meta.url))
    }
  }
});
