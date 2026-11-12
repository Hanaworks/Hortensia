<script setup lang="ts">
import type { Observation } from "../types";
defineProps<{ value: Observation | null }>();
const health: Record<string, string> = {
  none: "无健康检查",
  starting: "检查中",
  healthy: "健康",
  unhealthy: "不健康",
  unknown: "未知",
};
</script>
<template>
  <div class="observation">
    <p v-if="!value" class="muted">尚无资源观察。</p>
    <template v-else
      ><p>
        资源观察：{{
          { complete: "完整", incomplete: "不完整", error: "采集失败" }[
            value.status
          ]
        }}
        · {{ value.collected_at }}
      </p>
      <p v-if="value.error" class="notice">{{ value.error.message }}</p>
      <div class="table-wrap">
        <table v-if="value.items.length">
          <thead>
            <tr>
              <th>容器</th>
              <th>服务</th>
              <th>运行状态</th>
              <th>健康</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in value.items" :key="item.resource_id">
              <td>{{ item.name }}</td>
              <td>{{ item.service_name ?? "—" }}</td>
              <td>{{ item.state }}</td>
              <td>{{ health[item.health] ?? item.health }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="muted">本次观察没有容器记录。</p>
      </div></template
    >
  </div>
</template>
