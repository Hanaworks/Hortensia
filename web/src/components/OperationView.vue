<script setup lang="ts">
import type { Model } from "../model";
import ObservationView from "./ObservationView.vue";
defineProps<{ model: Model }>();
const states: Record<string, string> = {
  accepted: "已受理",
  preparing: "准备中",
  ready: "已准备",
  authorized: "已授权",
  running: "执行中",
  succeeded: "已完成",
  failed: "失败",
  canceled: "已取消",
  reconciliation_required: "需核对",
};
const outcomes: Record<string, string> = {
  not_started: "未开始",
  normal_exit: "进程正常退出",
  interrupted: "已中断，需核对",
  unknown: "执行结果未知，需核对",
};
</script>
<template>
  <section v-if="model.state.operation" class="panel">
    <div class="section-head">
      <h2>部署结果</h2>
      <span class="badge">{{ states[model.state.operation.state] }}</span>
    </div>
    <dl>
      <dt>操作</dt>
      <dd>{{ model.state.operation.operation_id }}</dd>
      <dt>此次部署版本</dt>
      <dd>{{ model.state.operation.version_id }}</dd>
      <dt>最近查询</dt>
      <dd>{{ model.state.lastChecked ?? "尚未查询" }}</dd>
      <dt>释放状态</dt>
      <dd>
        {{
          {
            held: "仍占用资源",
            pending: "结果已确认，释放待确认",
            released: "释放已确认",
          }[model.state.operation.release_status]
        }}
      </dd>
    </dl>
    <div v-if="model.state.operation.result">
      <h3>命令结局</h3>
      <p>
        {{ outcomes[model.state.operation.result.outcome] }} · 退出码：{{
          model.state.operation.result.exit_code ?? "无"
        }}
      </p>
      <p v-if="model.state.operation.result.interrupt_reason">
        中断原因：{{ model.state.operation.result.interrupt_reason }}
      </p>
      <p>资源归属：{{ model.state.operation.result.ownership_status }}</p>
      <p v-if="model.state.operation.result.error" class="notice">
        {{ model.state.operation.result.error.message }}
      </p>
      <ObservationView :value="model.state.operation.result.observation" />
    </div>
    <p v-else>尚未取得执行结果，请继续查询。</p>
    <p
      v-if="
        model.state.operation.state === 'reconciliation_required' ||
        ['unknown', 'interrupted'].includes(
          model.state.operation.result?.outcome ?? '',
        )
      "
      class="notice"
    >
      需要核对。请保留现有证据，勿重复部署；当前页面不能解除隔离。
    </p>
    <p v-if="model.state.operation.cancel_requested" class="notice">
      已请求取消，实际结果仍以本页查询为准；取消不保证回滚。
    </p>
    <div class="row">
      <button @click="model.refreshOperation()">刷新结果</button
      ><button
        v-if="model.has('operation.cancel')"
        :disabled="
          model.state.operation.cancel_requested ||
          ['succeeded', 'failed', 'canceled'].includes(
            model.state.operation.state,
          )
        "
        @click="model.cancel"
      >
        请求取消（不保证回滚）
      </button>
    </div>
    <details v-if="model.has('operation.output.read')" class="settings">
      <summary>命令输出</summary>
      <button data-action="output" @click="model.loadOutput">
        读取后续输出
      </button>
      <p v-if="model.state.outputTruncated" class="notice">
        输出已达到采集上限，内容有截断。
      </p>
      <pre>{{ model.state.output || "尚无输出。执行结果可用后再读取。" }}</pre>
    </details>
  </section>
</template>
