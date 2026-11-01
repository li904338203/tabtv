export type ScriptSourceType = "script" | "character" | "manual";
export type ScriptEntryMode = "direct" | "text_reference" | "character_reference" | "manual";
export type ScriptPromptType = "image" | "video";

export type ScriptInputReference = {
  nodeId: string;
  type: "text" | "image";
  title?: string | null;
  text?: string | null;
  generationId?: string | null;
};

export type ScriptInput = {
  text?: string | null;
  assetIds?: string[];
  references?: ScriptInputReference[];
};

/** 镜头里出现的实体（新后端 `shot.entities`）：决定资产引用与参考图的锚定顺序。 */
export type ScriptEntity = { name: string; type: string };

export type ScriptShot = {
  id: string;
  shotNo: number;
  durationSec: number;
  description: string;
  shotSize?: string | null;
  lighting?: string | null;
  dialogue?: string | null;
  soundEffect?: string | null;
  cameraMove?: string | null;
  imagePrompt?: string | null;
  videoPrompt?: string | null;
  imagePromptStatus: string;
  videoPromptStatus: string;
  imagePromptSource?: string | null;
  videoPromptSource?: string | null;
  contentVersion: number;
  sort: number;
  /** 本镜引用的资产（服务端按实体对账得出，按锚定顺序）。只读：改引用要改 `entities` 再对账。 */
  assetIds: string[];
  entities?: ScriptEntity[];
  updatedAt?: string;
};

export type ScriptAsset = {
  id: string;
  assetType: string;
  name: string;
  description?: string | null;
  imageUrl?: string | null;
  /** 设定图的媒体 ID：组子项的参考图只带这个 ID，靠它认出是哪个资产（CD-82） */
  mediaId?: string | null;
  status: string;
  progress: number;
  source?: string | null;
  version: number;
  shotIds: string[];
};

export type ScriptGroup = {
  id: string;
  groupType: string;
  name: string;
  status: string;
  itemCount: number;
  updatedAt?: string;
};

export type GroupItem = {
  itemId: string;
  shotId: string;
  assetId?: string | null;
  shotVersion: number;
  canvasNodeId: string;
  generationId?: string | null;
  status: string;
  progress: number;
  outputType?: string | null;
  outputUrl?: string | null;
  paramsSnapshot?: Record<string, unknown>;
  inputRefs?: unknown;
  error?: string | null;
};

export type GroupDetail = {
  /** 执行时用的模型：新后端组上不存模型，建组时选的记在画布节点上（视频组先落 draft，执行时要给） */
  modelCode?: string;
  groupId: string;
  projectId: string;
  scriptId: string;
  canvasNodeId: string;
  groupType: string;
  name: string;
  status: string;
  version: number;
  items: GroupItem[];
};

export type ScriptProgress = {
  completed: number;
  total: number;
  status: string;
};

export type ScriptDetail = {
  scriptId: string;
  projectId: string;
  nodeId: string;
  name: string;
  sourceType: ScriptSourceType;
  globalStyle?: string | null;
  currentStep: number;
  status: string;
  version: number;
  input: ScriptInput;
  shotsProgress: ScriptProgress;
  assetsProgress: ScriptProgress;
  promptsProgress: ScriptProgress;
  shots: ScriptShot[];
  assets: ScriptAsset[];
  groups: ScriptGroup[];
  /** 分镜生成是否正在进行，刷新后据此恢复"生成中"态 */
  storyboardRunning?: boolean;
  updatedAt?: string;
};

export type ScriptCreated = {
  scriptId: string;
  currentStep: number;
  version: number;
};

export type PromptComposeResponse = {
  shotId: string;
  imagePrompt?: string | null;
  videoPrompt?: string | null;
  imagePromptStatus: string;
  videoPromptStatus: string;
  version: number;
  creditsCost: number;
};

export type ScriptShotDraft = Omit<ScriptShot, "id" | "shotNo" | "contentVersion" | "sort" | "updatedAt"> & {
  id?: string;
  shotNo?: number;
  contentVersion?: number;
  sort?: number;
};
