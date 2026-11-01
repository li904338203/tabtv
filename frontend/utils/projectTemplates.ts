export const PROJECT_TEMPLATES = [
  {
    code: "story_script",
    name: "故事脚本生成",
    cover: "/media/show-green.jpg",
    icon: "script",
    tone: "blue"
  },
  {
    code: "role_three_view",
    name: "角色三视图",
    cover: "/media/character-board.jpg",
    icon: "role",
    tone: "red"
  },
  {
    code: "first_frame_video",
    name: "首帧图生视频",
    cover: "/media/camera-grid.jpg",
    icon: "image",
    tone: "gold"
  },
  {
    code: "audio_video",
    name: "音频生视频",
    cover: "/media/show-dragon.jpg",
    icon: "audio",
    tone: "green"
  }
] as const;

export type ProjectTemplateCode = typeof PROJECT_TEMPLATES[number]["code"];
