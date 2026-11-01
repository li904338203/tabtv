import type { Component } from "vue";
import ButtonControl from "./ButtonControl.vue";
import DialogControl from "./DialogControl.vue";
import GridControl from "./GridControl.vue";
import InlineControl from "./InlineControl.vue";
import SegmentedControl from "./SegmentedControl.vue";
import SelectControl from "./SelectControl.vue";
import SliderControl from "./SliderControl.vue";
import SwitchControl from "./SwitchControl.vue";
import TagGroupControl from "./TagGroupControl.vue";
import TextControl from "./TextControl.vue";
import UploadControl from "./UploadControl.vue";

export const CONTROL_MAP: Record<string, Component> = {
  select: SelectControl,
  segmented: SegmentedControl,
  slider: SliderControl,
  grid: GridControl,
  tag_group: TagGroupControl,
  switch: SwitchControl,
  dialog: DialogControl,
  button: ButtonControl,
  upload: UploadControl,
  inline: InlineControl,
  // ⚠️ 新后端才有的控件类型（`negative_prompt`）。老 CONTROL_MAP 没有它，
  //    而未知控件是静默跳过的 —— 缺失表现为"这个参数不存在"，不报错。
  text: TextControl
};
