<script setup lang="ts">
import { computed, ref, watch } from "vue";
import type { Model } from "../model";
const { model } = defineProps<{ model: Model }>();
const s = model.state;
const fileIndex = ref(0);
const projectName = ref("");
const env = ref("");
const envError = ref("");
const composeFilesText = ref("");
const envFilesText = ref("");
const profilesText = ref("");
const chosenVersion = ref("");
const showConfirm = ref(false);
const file = computed(() => s.draft?.files[fileIndex.value]);
watch(
  () => s.draft,
  () => {
    fileIndex.value = 0;
    env.value = JSON.stringify(s.draft?.interpolation_env ?? {}, null, 2);
    envError.value = "";
    showConfirm.value = false;
    syncListText();
  },
  { immediate: true },
);
watch(
  () => s.baseVersion,
  (v) => {
    chosenVersion.value = v ?? "";
    showConfirm.value = false;
  },
  { immediate: true },
);
function updateEnv(value: string) {
  env.value = value;
  try {
    const parsed: unknown = JSON.parse(value);
    if (
      !parsed ||
      Array.isArray(parsed) ||
      typeof parsed !== "object" ||
      Object.values(parsed).some((v) => typeof v !== "string")
    )
      throw Error();
    model.edit((d) => {
      d.interpolation_env = parsed as Record<string, string>;
    });
    envError.value = "";
  } catch {
    envError.value = "填写JSON对象，所有变量值须为文本。当前无效输入尚未保存。";
  }
}
function lines(value: string) {
  return value.split(/\r?\n/).filter((v) => v !== "");
}
function syncListText() {
  composeFilesText.value = s.draft?.compose_files.join("\n") ?? "";
  envFilesText.value = s.draft?.interpolation_env_files.join("\n") ?? "";
  profilesText.value = s.draft?.profiles.join("\n") ?? "";
}
function updateList(
  key: "compose_files" | "interpolation_env_files" | "profiles",
  value: string,
) {
  const text =
    key === "compose_files"
      ? composeFilesText
      : key === "interpolation_env_files"
        ? envFilesText
        : profilesText;
  text.value = value;
  model.edit((d) => {
    d[key] = lines(value);
  });
}
function addFile() {
  model.edit((d) => {
    d.files.push({ path: "file-" + (d.files.length + 1) + ".yaml", text: "" });
    fileIndex.value = d.files.length - 1;
  });
}
function removeFile() {
  model.edit((d) => {
    const [removed] = d.files.splice(fileIndex.value, 1);
    d.compose_files = d.compose_files.filter((p) => p !== removed.path);
    d.interpolation_env_files = d.interpolation_env_files.filter(
      (p) => p !== removed.path,
    );
    fileIndex.value = 0;
  });
  syncListText();
}
function rename(value: string) {
  model.edit((d) => {
    const old = d.files[fileIndex.value].path;
    d.files[fileIndex.value].path = value;
    d.compose_files = d.compose_files.map((p) => (p === old ? value : p));
    d.interpolation_env_files = d.interpolation_env_files.map((p) =>
      p === old ? value : p,
    );
  });
  syncListText();
}
async function deploy() {
  const version = chosenVersion.value;
  showConfirm.value = false;
  await model.apply(version);
}
</script>
<template>
  <section v-if="s.draft" class="panel editor">
    <div class="section-head">
      <div>
        <p class="eyebrow">{{ s.node?.display_name }} / 配置</p>
        <h2>{{ s.stack?.display_name ?? "新建 Stack" }}</h2>
      </div>
      <span class="badge">{{
        s.pending?.status === "sending"
          ? "正在提交"
          : s.dirty
            ? "有未保存修改"
            : "已保存"
      }}</span>
    </div>
    <label v-if="!s.stack"
      >项目名<input
        v-model="projectName"
        placeholder="例如 my-app"
        pattern="[a-z0-9][a-z0-9_-]{0,62}"
        maxlength="63"
    /></label>
    <p class="muted">
      保存只保留配置。部署会使用明确选择的已保存版本；不会自动包含后续编辑。
    </p>
    <div class="file-layout">
      <nav aria-label="配置文件" class="files">
        <button
          v-for="(f, i) in s.draft.files"
          :key="i"
          :class="{ selected: i === fileIndex }"
          @click="fileIndex = i"
        >
          {{ f.path }}</button><button
          v-if="model.has('config.write')"
          @click="addFile"
        >
          ＋ 添加文件
        </button>
      </nav>
      <div v-if="file" class="file-body">
        <div class="row">
          <label class="grow"
            >包内相对路径<input
              :value="file.path"
              :disabled="!model.has('config.write')"
              @input="
                rename(($event.target as HTMLInputElement).value)
              " /></label
          ><button
            v-if="model.has('config.write')"
            :disabled="s.draft.files.length === 1"
            @click="removeFile"
          >
            移除文件
          </button>
        </div>
        <textarea
          class="code"
          aria-label="文件内容"
          :value="file.text"
          :readonly="!model.has('config.write')"
          spellcheck="false"
          @input="
            model.edit((d) => {
              d.files[fileIndex].text = (
                $event.target as HTMLTextAreaElement
              ).value;
            })
          "
        />
      </div>
    </div>
    <details class="settings">
      <summary>文件顺序、插值变量与配置档案</summary>
      <div class="grid two">
        <label
          >Compose 文件（每行一个，按执行顺序）<textarea
            :value="composeFilesText"
            :readonly="!model.has('config.write')"
            @input="
              updateList(
                'compose_files',
                ($event.target as HTMLTextAreaElement).value,
              )
            "
          /></label
        ><label
          >插值环境文件（每行一个，按顺序）<textarea
            :value="envFilesText"
            :readonly="!model.has('config.write')"
            @input="
              updateList(
                'interpolation_env_files',
                ($event.target as HTMLTextAreaElement).value,
              )
            "
          /></label
        ><label
          >显式插值变量（JSON 文本映射）<textarea
            class="code short"
            :value="env"
            :readonly="!model.has('config.write')"
            spellcheck="false"
            @input="updateEnv(($event.target as HTMLTextAreaElement).value)"
          /><small
            >用于 Compose 插值，不等于容器环境。值仅在本页内存中保留。</small
          ><span v-if="envError" class="error">{{ envError }}</span></label
        ><label
          >启用的配置档案 profiles（每行一个）<textarea
            :value="profilesText"
            :readonly="!model.has('config.write')"
            @input="
              updateList(
                'profiles',
                ($event.target as HTMLTextAreaElement).value,
              )
            "
          />
        </label>
      </div>
    </details>
    <div class="row">
      <button
        v-if="model.has('config.write')"
        class="primary"
        data-action="save"
        :disabled="!!s.pending || s.conflicted || !!envError || !s.dirty"
        @click="model.save(projectName)"
      >
        {{
          s.pending?.status === "sending" ? "正在提交…" : "保存配置"
        }}</button><span class="muted"
        >基准版本：{{ s.baseVersion ?? "尚未保存" }}</span
      >
    </div>
    <div v-if="s.conflicted" class="notice">
      <p>保存冲突。你的草稿仍保留，请比较最新内容后再选择。</p>
      <button @click="model.loadConflict">读取最新版本并比较</button>
      <div v-if="s.latest">
        <p>服务端最新版本：{{ s.latest.version }}</p>
        <details v-for="f in s.latest.draft.files" :key="f.path">
          <summary>{{ f.path }} · 服务端内容</summary>
          <pre>{{ f.text }}</pre>
        </details>
        <details>
          <summary>服务端文件顺序与插值设置</summary>
          <pre>{{
            {
              compose_files: s.latest.draft.compose_files,
              interpolation_env_files: s.latest.draft.interpolation_env_files,
              interpolation_env: s.latest.draft.interpolation_env,
              profiles: s.latest.draft.profiles,
            }
          }}</pre>
        </details>
        <button @click="model.adoptLatest">以新版为基准，保留我的编辑</button>
      </div>
    </div>
    <div
      v-if="
        s.baseVersion && model.has('node.read', 'config.read', 'stack.deploy')
      "
      class="deploy"
    >
      <h3>部署已保存版本</h3>
      <p>
        最新保存：{{ s.stack?.current_version_id }}<br />最近确认成功：{{
          s.stack?.last_applied_version_id ?? "尚无"
        }}
      </p>
      <p v-if="s.stack?.last_operation_id">
        最近受理操作：<code>{{ s.stack.last_operation_id }}</code>
      </p>
      <label
        >此次部署版本<input
          v-model="chosenVersion"
          :placeholder="s.baseVersion"
      /></label>
      <p v-if="s.dirty" class="notice">
        尚有未保存修改。先保存，或明确部署上方已保存版本。
      </p>
      <button
        data-action="apply"
        :disabled="!!s.pending || s.node?.status !== 'online' || !chosenVersion"
        @click="showConfirm = true"
      >
        核对部署
      </button>
      <p v-if="s.node?.status !== 'online'" class="muted">
        节点目前不能接受新部署，请先恢复连接并完成核对。
      </p>
      <div v-if="showConfirm" class="notice">
        <h3>确认部署到 {{ s.node?.display_name }}</h3>
        <p>
          项目：{{ s.stack?.project_name }}<br />固定版本：{{ chosenVersion }}
        </p>
        <p>将按配置变更该节点的项目资源。未保存内容不包含在内。</p>
        <button class="primary" @click="deploy">部署此已保存版本</button>
        <button @click="showConfirm = false">返回编辑</button>
      </div>
    </div>
  </section>
</template>
