<script setup lang="ts">
import { ref, watch } from "vue";
import type { Model } from "../model";
import type { Capability, User } from "../types";
const { model } = defineProps<{ model: Model }>();
const s = model.state;
const username = ref("");
const password = ref("");
const nodeName = ref("");
const selected = ref("");
const caps = ref<Capability[]>([]);
const newPassword = ref("");
const choices: { key: Capability; label: string }[] = [
  { key: "node.read", label: "节点和项目查看" },
  { key: "config.read", label: "配置读取" },
  { key: "config.write", label: "配置维护" },
  { key: "stack.deploy", label: "部署" },
  { key: "operation.read", label: "结果读取" },
  { key: "operation.output.read", label: "命令输出读取" },
  { key: "operation.cancel", label: "取消操作" },
];
watch([selected, () => s.grants], () => {
  caps.value = [
    ...(s.grants.find((g) => g.user_id === selected.value)?.capabilities ?? []),
  ];
});
async function createUser() {
  const secret = password.value;
  password.value = "";
  await model.manage("/users", { username: username.value, password: secret });
  await model.loadAccess();
}
async function createNode() {
  await model.manage("/nodes", { display_name: nodeName.value });
  await model.loadNodes();
}
async function setEnabled(user: User) {
  if (
    !confirm(
      `${user.enabled ? "禁用" : "启用"}账号 ${user.username}？现有会话将失效。`,
    )
  )
    return;
  await model.manage(
    "/users/" + user.user_id,
    { enabled: !user.enabled },
    "PATCH",
    String(user.auth_rev),
  );
  await model.loadAccess();
}
async function changePassword() {
  const user = s.users.find((u) => u.user_id === selected.value);
  if (!user) return;
  const secret = newPassword.value;
  newPassword.value = "";
  await model.manage(
    "/users/" + user.user_id,
    { password: secret },
    "PATCH",
    String(user.auth_rev),
  );
  await model.loadAccess();
}
async function grant() {
  const g = s.grants.find((x) => x.user_id === selected.value);
  if (!g || !s.node) return;
  await model.manage(
    "/nodes/" + s.node.node_id + "/grants/" + g.user_id,
    { capabilities: [...caps.value].sort() },
    "PUT",
    String(g.auth_rev),
  );
  await model.loadAccess();
  await model.refreshSession();
}
async function revoke() {
  if (
    s.node &&
    confirm(
      `撤销 ${s.node.display_name} 的接入身份？现有应用不会被删除，未结束操作仍需核对。`,
    )
  )
    await model.manage("/nodes/" + s.node.node_id + "/revoke");
}
</script>
<template>
  <section v-if="model.admin()" class="panel">
    <h2>账号与节点管理</h2>
    <p class="muted">
      系统管理员仍需明确取得节点动作权限，才能读取配置或部署。
    </p>
    <div class="grid two">
      <form @submit.prevent="createUser">
        <h3>创建账号</h3>
        <label
          >登录名<input
            v-model="username"
            required
            pattern="[a-z0-9][a-z0-9._-]{2,63}"
            autocomplete="off" /></label
        ><label
          >初始密码<input
            v-model="password"
            required
            type="password"
            minlength="12"
            maxlength="256"
            autocomplete="new-password" /></label
        ><button :disabled="!!s.pending">创建账号</button>
      </form>
      <form @submit.prevent="createNode">
        <h3>登记节点</h3>
        <label
          >节点名称<input v-model="nodeName" required maxlength="128" /></label
        ><button :disabled="!!s.pending">登记节点</button>
      </form>
    </div>
    <h3>现有账号</h3>
    <button @click="model.loadAccess()">刷新账号与授权</button>
    <table>
      <thead>
        <tr>
          <th>账号</th>
          <th>状态</th>
          <th>操作</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="user in s.users" :key="user.user_id">
          <td>{{ user.username }}</td>
          <td>{{ user.enabled ? "启用" : "禁用" }}</td>
          <td>
            <button :disabled="!!s.pending" @click="setEnabled(user)">
              {{ user.enabled ? "禁用" : "启用" }}
            </button>
          </td>
        </tr>
      </tbody>
    </table>
    <button v-if="s.usersCursor" @click="model.loadAccess(true)">
      更多账号
    </button>
    <label
      >选择管理账号<select v-model="selected">
        <option value="">选择账号</option>
        <option
          v-for="user in s.users"
          :key="user.user_id"
          :value="user.user_id"
        >
          {{ user.username }}
        </option>
      </select></label
    >
    <form v-if="selected" @submit.prevent="changePassword">
      <label
        >设置新密码<input
          v-model="newPassword"
          type="password"
          minlength="12"
          required
          autocomplete="new-password" /></label
      ><button :disabled="!!s.pending">更新密码并使旧会话失效</button>
    </form>
    <template v-if="s.node"
      ><h3>{{ s.node.display_name }} · 节点动作授权</h3>
      <p v-if="!s.grants.some((g) => g.user_id === selected)" class="muted">
        选择已加载授权行的账号。授权更多页可继续加载。
      </p>
      <fieldset
        :disabled="!!s.pending || !s.grants.some((g) => g.user_id === selected)"
      >
        <label v-for="choice in choices" :key="choice.key" class="check"
          ><input v-model="caps" type="checkbox" :value="choice.key" />{{
            choice.label
          }}</label
        ><button @click="grant">保存此账号的节点授权</button>
      </fieldset>
      <button v-if="s.grantsCursor" @click="model.loadAccess(false, true)">
        更多授权
      </button>
      <h3>Agent 接入</h3>
      <p class="muted">
        签发后在节点上使用 Agent 的认领流程。令牌仅本次显示，有效期10分钟。
      </p>
      <div class="row">
        <button
          :disabled="!!s.pending || s.node.status !== 'pending'"
          @click="
            model.manage('/nodes/' + s.node.node_id + '/enrollment-tokens', {})
          "
        >
          生成认领令牌</button
        ><button
          :disabled="!!s.pending || s.node.status !== 'pending'"
          @click="
            model.manage('/nodes/' + s.node.node_id + '/enrollment-tokens', {
              replace_pending: true,
            })
          "
        >
          重新签发并废止旧令牌</button
        ><button
          class="danger"
          :disabled="!!s.pending || s.node.status === 'revoked'"
          @click="revoke"
        >
          撤销节点接入
        </button>
      </div>
      <div v-if="s.enrollment" class="notice">
        <p>到期：{{ s.enrollment.expires_at }}</p>
        <label v-if="s.enrollment.token"
          >认领令牌（离开后不可回读）<input
            :value="s.enrollment.token"
            readonly
            autocomplete="off"
            spellcheck="false"
        /></label>
        <p v-else>令牌已无法回读，需要时明确重新签发。</p>
        <button @click="s.enrollment = null">隐藏认领令牌</button>
      </div></template
    >
    <p v-else class="muted">先从左侧选择节点，再设置授权和接入材料。</p>
  </section>
</template>
