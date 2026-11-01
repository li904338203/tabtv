import { ElMessageBox, type ElMessageBoxOptions } from "element-plus";

// 确认 / 输入弹窗的统一外观（样式在 main.css 的 .app-dialog）。
//
// Element Plus 默认是蓝色按钮 + 橙色感叹号，和主界面的黑白主色、红色危险色对不上。
// 各处直接调 ElMessageBox 的话样式要每处各配一遍，漏一处就又是一个蓝按钮 —— 所以收在这里。

/** 删除这类不可撤销的操作：红色确认按钮、中性取消、不带警告图标。取消时 reject（与 ElMessageBox 一致）。 */
export function appConfirmDanger(message: string, title = "删除", confirmText = "删除") {
  return ElMessageBox.confirm(message, title, {
    confirmButtonText: confirmText,
    cancelButtonText: "取消",
    customClass: "app-dialog",
    confirmButtonClass: "app-dialog-danger",
    cancelButtonClass: "app-dialog-cancel",
    autofocus: false
  });
}

/** 输入一个名字（新建 / 重命名）：主按钮是主界面的深色主色。 */
export function appPrompt(message: string, title: string, options: ElMessageBoxOptions = {}) {
  return ElMessageBox.prompt(message, title, {
    confirmButtonText: "确定",
    cancelButtonText: "取消",
    ...options,
    customClass: "app-dialog",
    confirmButtonClass: "app-dialog-primary",
    cancelButtonClass: "app-dialog-cancel"
  });
}
