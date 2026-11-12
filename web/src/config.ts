import type { Config, Draft } from "./types";
const encoder = new TextEncoder();
function fromBase64(value: string) {
  const binary = atob(value);
  if (btoa(binary) !== value) throw new Error("配置文件编码不规范，已停止编辑");
  return new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(
    Uint8Array.from(binary, (c) => c.charCodeAt(0)),
  );
}
function toBase64(value: string) {
  const bytes = encoder.encode(value);
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}
export function clone<T>(value: T): T {
  return JSON.parse(JSON.stringify(value)) as T;
}
export function emptyDraft(): Draft {
  return {
    files: [{ path: "compose.yaml", text: "services: {}\n" }],
    compose_files: ["compose.yaml"],
    interpolation_env_files: [],
    interpolation_env: {},
    profiles: [],
  };
}
export function decodeConfig(config: Config): Draft {
  if (
    !config ||
    !Array.isArray(config.files) ||
    !Array.isArray(config.compose_files) ||
    !Array.isArray(config.interpolation_env_files) ||
    !config.interpolation_env ||
    !Array.isArray(config.profiles)
  )
    throw new Error("配置返回格式无效");
  const draft = {
    ...clone(config),
    files: config.files.map((f) => ({
      path: f.path,
      text: fromBase64(f.content_b64),
    })),
  };
  validateDraft(draft);
  return draft;
}
export function validateDraft(draft: Draft) {
  if (draft.files.length < 1 || draft.files.length > 128)
    throw new Error("配置必须包含1—128个文件");
  const names = new Set<string>();
  let bytes = 0;
  for (const file of draft.files) {
    if (
      !/^(?!\/)(?!.*[\\:\u0000])(?!.*(?:^|\/)\.{1,2}(?:\/|$))[^/]+(?:\/[^/]+)*$/.test(
        file.path,
      ) ||
      encoder.encode(file.path).length > 240 ||
      names.has(file.path)
    )
      throw new Error("文件路径无效或重复：" + file.path);
    names.add(file.path);
    const n = encoder.encode(file.text).length;
    if (n > 1048576) throw new Error("单文件超过1 MiB");
    bytes += n;
  }
  if (bytes > 4194304) throw new Error("配置文件总量超过4 MiB");
  for (const list of [draft.compose_files, draft.interpolation_env_files]) {
    if (new Set(list).size !== list.length || list.some((p) => !names.has(p)))
      throw new Error("文件顺序列表存在重复或缺少的文件");
  }
  if (!draft.compose_files.length) throw new Error("至少选择一个Compose文件");
  if (
    Object.keys(draft.interpolation_env).length > 1024 ||
    Object.entries(draft.interpolation_env).some(
      ([k, v]) => !/^[A-Za-z_][A-Za-z0-9_]*$/.test(k) || typeof v !== "string",
    )
  )
    throw new Error("插值变量须为变量名到文本值的映射");
  if (
    draft.profiles.length > 128 ||
    new Set(draft.profiles).size !== draft.profiles.length ||
    draft.profiles.some((p) => !p || [...p].length > 128)
  )
    throw new Error("配置档案名称无效或重复");
}
export function encodeConfig(draft: Draft): Config {
  validateDraft(draft);
  return {
    ...clone(draft),
    files: draft.files.map((f) => ({
      path: f.path,
      content_b64: toBase64(f.text),
    })),
  };
}
