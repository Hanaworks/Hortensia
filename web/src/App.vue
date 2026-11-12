<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref, watch } from "vue";
import { Api } from "./api";
import { createModel } from "./model";
import { SubmissionStore } from "./submissions";
import ConfigEditor from "./components/ConfigEditor.vue";
import OperationView from "./components/OperationView.vue";
import ObservationView from "./components/ObservationView.vue";
import AccessPanel from "./components/AccessPanel.vue";
const model = createModel(new Api(), new SubmissionStore(sessionStorage));
const s = model.state;
const username = ref("");
const password = ref("");
const access = ref(false);
const statuses: Record<string, string> = {
  pending: "待认领",
  online: "已连接",
  offline: "离线",
  reconciling: "核对中",
  revoked: "已撤销",
};
function canLeave() {
  return (
    !(s.dirty || s.pending) ||
    confirm(
      "离开将丢弃本页草稿；已提交的请求仍可在待核对记录中查询。继续离开？",
    )
  );
}
async function select(id: string) {
  if (canLeave()) {
    await model.selectNode(id);
    if (access.value) await model.loadAccess();
  }
}
async function open(id: string) {
  if (canLeave()) await model.openStack(id);
}
async function login() {
  const secret = password.value;
  password.value = "";
  await model.login(username.value, secret);
}
function beforeUnload(event: BeforeUnloadEvent) {
  if (s.dirty || s.pending) {
    event.preventDefault();
    event.returnValue = "";
  }
}
watch(
  () => s.me?.user_id,
  () => {
    password.value = "";
    access.value = false;
  },
);
onMounted(async () => {
  window.addEventListener("beforeunload", beforeUnload);
  await model.initialize();
  model.start();
});
onBeforeUnmount(() => {
  window.removeEventListener("beforeunload", beforeUnload);
  model.dispose();
});
</script>
<template>
  <header class="topbar">
    <div class="brand">
      <span class="brand-mark">▦</span><strong>Docker Manager</strong>
    </div>
    <div v-if="s.me" class="row">
      <span>{{ s.me.username }}</span
      ><button
        v-if="model.admin()"
        data-action="access"
        @click="
          access = !access;
          if (access) model.loadAccess();
        "
      >
        {{ access ? "返回工作区" : "账号与节点管理" }}</button
      ><button @click="canLeave() && model.logout()">退出</button>
    </div>
  </header>
  <main v-if="!s.me" class="login">
    <div class="panel">
      <p class="eyebrow">多节点容器管理</p>
      <h1>登录工作区</h1>
      <p class="muted">使用管理器中的本地账号。</p>
      <p v-if="s.error" role="alert" class="error">{{ s.error }}</p>
      <form @submit.prevent="login">
        <label
          >登录名<input
            v-model="username"
            required
            autocomplete="username"
            autofocus /></label
        ><label
          >密码<input
            v-model="password"
            type="password"
            required
            autocomplete="current-password" /></label
        ><button class="primary" :disabled="s.loading">
          {{ s.loading ? "正在连接…" : "登录" }}
        </button>
      </form>
      <small class="muted">首次使用，请先在管理器主机初始化管理员账号。</small>
    </div>
  </main>
  <div v-else class="workspace">
    <aside class="sidebar">
      <div class="section-head">
        <h2>节点</h2>
        <button aria-label="刷新节点" @click="model.loadNodes()">刷新</button>
      </div>
      <p v-if="!s.nodes.length" class="muted">暂无可见节点。</p>
      <button
        v-for="node in s.nodes"
        :key="node.node_id"
        class="node"
        :class="{ selected: s.node?.node_id === node.node_id }"
        :data-node="node.node_id"
        @click="select(node.node_id)"
      >
        <strong>{{ node.display_name }}</strong
        ><span class="muted">{{ statuses[node.status] }}</span></button
      ><button v-if="s.nodesCursor" @click="model.loadNodes(true)">
        更多节点
      </button>
      <div v-if="s.node && model.has('node.read')" class="stack-nav">
        <div class="section-head">
          <h3>Stack</h3>
          <button
            v-if="model.has('config.read', 'config.write')"
            data-action="new-stack"
            @click="canLeave() && model.newStack()"
          >
            新建
          </button>
        </div>
        <button
          v-for="stack in s.stacks"
          :key="stack.stack_id"
          :class="{ selected: s.stack?.stack_id === stack.stack_id }"
          @click="open(stack.stack_id)"
        >
          {{ stack.display_name }}
        </button>
        <p v-if="!s.stacks.length" class="muted">暂无已保存项目。</p>
        <button v-if="s.stacksCursor" @click="model.loadStacks(true)">
          更多项目
        </button>
      </div>
    </aside>
    <main class="content">
      <p v-if="s.error" role="alert" class="error banner">{{ s.error }}</p>
      <p v-if="s.message" role="status" class="notice">{{ s.message }}</p>
      <div v-if="s.pending?.status === 'uncertain'" class="notice">
        <p>提交结果待核对。当前页面保留了原始请求。</p>
        <button @click="model.reconcile()">核对原提交</button>
        <button @click="model.retryPending">用原请求重试</button>
      </div>
      <details v-if="s.records.length" class="panel">
        <summary>待核对的提交（{{ s.records.length }}）</summary>
        <p class="muted">
          刷新会丢失草稿，但不会撤销已提交请求。先核对原提交，避免重复操作。
        </p>
        <div v-for="record in s.records" :key="record.key" class="row">
          <code>{{ record.key }}</code
          ><span>{{
            {
              create: "新建配置",
              save: "保存配置",
              apply: "部署",
              admin: "管理变更",
            }[record.kind]
          }}</span
          ><button
            :disabled="s.pending?.status === 'sending'"
            @click="model.reconcile(record)"
          >
            核对
          </button>
        </div>
      </details>
      <AccessPanel v-if="access" :key="s.contextRevision" :model="model" />
      <template v-else
        ><section v-if="s.node" class="panel">
          <div class="section-head">
            <div>
              <p class="eyebrow">当前节点</p>
              <h1>{{ s.node.display_name }}</h1>
            </div>
            <span class="badge">{{ statuses[s.node.status] }}</span>
          </div>
          <p class="muted">
            最近连接活动：{{ s.node.last_seen_at ?? "尚未接入" }}
          </p>
          <p v-if="!model.has('node.read')" class="notice">
            仅有节点管理权限。请在账号与节点管理中明确授予所需动作。
          </p>
          <details v-else>
            <summary>节点能力与最近资源观察</summary>
            <p>
              Agent 能力：{{
                s.node.agent_capabilities.join("、") || "尚无报告"
              }}
            </p>
            <ObservationView :value="s.node.observation" />
          </details>
        </section>
        <section v-else class="panel empty">
          <h1>选择一个节点</h1>
          <p class="muted">
            在节点中保存配置、部署已保存版本，并核对实际结果。
          </p>
        </section>
        <ConfigEditor :key="s.contextRevision" :model="model" />
        <section v-if="s.stack && !s.draft && !s.loading" class="panel">
          <h2>{{ s.stack.display_name }}</h2>
          <p>没有配置读取权限。</p>
        </section>
        <OperationView :model="model" />
        <section v-if="s.node && model.has('operation.read')" class="panel">
          <div class="section-head">
            <h2>操作记录</h2>
            <button @click="model.loadOperations()">读取记录</button>
          </div>
          <div
            v-for="op in s.operations"
            :key="op.operation_id"
            class="history"
          >
            <button @click="model.openOperation(op.operation_id)">
              {{ op.created_at }} · {{ op.operation_id }}</button
            ><span>{{ op.state }} · {{ op.release_status }}</span>
          </div>
          <button v-if="s.operationsCursor" @click="model.loadOperations(true)">
            更多记录
          </button>
        </section></template
      >
    </main>
  </div>
</template>
