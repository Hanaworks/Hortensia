import { afterEach, expect, it, vi } from "vitest";
import { createApp, nextTick, type App as VueApp } from "vue";
import { createModel, type Model } from "../src/model";
import { Api } from "../src/api";
import { SubmissionStore } from "../src/submissions";
import { encodeConfig } from "../src/config";
import type { Draft, Me, NodeInfo, Operation, Stack, User } from "../src/types";
import ConfigEditor from "../src/components/ConfigEditor.vue";
import App from "../src/App.vue";

const me: Me = {
  user_id: "user",
  username: "admin",
  auth_rev: 0,
  system_capabilities: ["system.admin"],
  csrf_token: "csrf",
  expires_at: "2030-01-01T00:00:00Z",
};
const node: NodeInfo = {
  node_id: "node",
  display_name: "test-node",
  status: "online",
  capabilities: [
    "node.read",
    "config.read",
    "config.write",
    "stack.deploy",
    "operation.read",
    "operation.output.read",
    "operation.cancel",
  ],
  agent_capabilities: [],
  last_seen_at: null,
  execution_profile: null,
  observation: null,
};
const stack: Stack = {
  node_id: "node",
  stack_id: "stack",
  display_name: "demo",
  project_name: "demo",
  current_version_id: "v1",
  revision: 1,
  last_applied_version_id: null,
  last_operation_id: null,
  observation: null,
};
const draft: Draft = {
  files: [
    { path: "compose.yaml", text: "services: {}\n" },
    { path: "override.yaml", text: "services: {}\n" },
    { path: "first.env", text: "A=1\n" },
    { path: "second.env", text: "B=2\n" },
  ],
  compose_files: ["compose.yaml"],
  interpolation_env_files: ["first.env"],
  interpolation_env: { SECRET: "existing-secret" },
  profiles: ["first"],
};
const user = (id: string): User => ({
  user_id: id,
  username: id,
  enabled: true,
  auth_rev: 0,
});
const op = (id: string): Operation => ({
  operation_id: id,
  node_id: "node",
  stack_id: "stack",
  version_id: "v1",
  operation_type: "stack.apply",
  revision: 1,
  state: "succeeded",
  cancel_requested: false,
  created_at: "2026-11-12T00:00:00Z",
  updated_at: "2026-11-12T00:00:00Z",
  result: null,
  release_status: "released",
});
const models: Model[] = [];
const apps: VueApp[] = [];
function response(value: unknown) {
  return value instanceof Response
    ? value
    : new Response(JSON.stringify(value), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
}
function rejected(status: number) {
  return new Response(
    JSON.stringify({
      error: {
        code: status === 412 ? "version_conflict" : "not_found",
        message: "rejected",
      },
    }),
    { status },
  );
}
function deferred() {
  let resolve!: (v: unknown) => void;
  let reject!: (e: Error) => void;
  const promise = new Promise((r, j) => {
    resolve = r;
    reject = j;
  });
  return { resolve, reject, promise };
}
function make(
  handler: (path: string, init: RequestInit) => unknown = () => ({}),
) {
  const api = new Api((async (p, i) =>
    response(await handler(String(p), i ?? {}))) as typeof fetch);
  const model = createModel(api, new SubmissionStore(sessionStorage));
  models.push(model);
  Object.assign(model.state, {
    me: structuredClone(me),
    node: structuredClone(node),
    stack: structuredClone(stack),
    draft: structuredClone(draft),
    baseVersion: "v1",
  });
  return model;
}
function mount(model: Model) {
  const root = document.createElement("div");
  document.body.append(root);
  const app = createApp(ConfigEditor, { model });
  apps.push(app);
  app.mount(root);
  return root;
}
function area(root: HTMLElement, label: string) {
  return [...root.querySelectorAll<HTMLTextAreaElement>("textarea")].find((x) =>
    x.closest("label")?.textContent?.includes(label),
  )!;
}
async function input(field: HTMLTextAreaElement, value: string) {
  field.value = value;
  field.dispatchEvent(new Event("input", { bubbles: true }));
  await nextTick();
}
afterEach(() => {
  apps.splice(0).forEach((a) => a.unmount());
  models.splice(0).forEach((m) => m.dispose());
  document.body.innerHTML = "";
  sessionStorage.clear();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it.each([
  ["Compose 文件", "compose_files", "compose.yaml", "override.yaml"],
  ["插值环境文件", "interpolation_env_files", "first.env", "second.env"],
  ["配置档案 profiles", "profiles", "first", "second"],
] as const)(
  "ordered %s accepts Enter followed by another value without swallowing input",
  async (label, key, first, second) => {
    const model = make();
    const root = mount(model);
    const field = area(root, label);
    await input(field, first + "\n");
    expect(field.value).toBe(first + "\n");
    for (const character of second) await input(field, field.value + character);
    expect(field.value).toBe(first + "\n" + second);
    expect(model.state.draft![key]).toEqual([first, second]);
  },
);

it("remounted editor initializes existing environment and default deployment version", async () => {
  const model = make();
  const root = mount(model);
  await nextTick();
  expect(area(root, "显式插值变量").value).toContain("existing-secret");
  expect(
    root.querySelector<HTMLButtonElement>('[data-action="apply"]')!.disabled,
  ).toBe(false);
});

it.each(["users-first", "grants-first"])(
  "independent account and grant pagination retains selectable targets: %s",
  async (order) => {
    const paths: string[] = [];
    const model = make((p) => {
      paths.push(p);
      if (p === "/api/v1/users")
        return { items: [user("u1")], next_cursor: "u2" };
      if (p === "/api/v1/users?cursor=u2")
        return { items: [user("u2")], next_cursor: null };
      if (p === "/api/v1/nodes/node/grants")
        return {
          items: [{ user_id: "u1", auth_rev: 0, capabilities: [] }],
          next_cursor: "g2",
        };
      if (p === "/api/v1/nodes/node/grants?cursor=g2")
        return {
          items: [{ user_id: "u2", auth_rev: 0, capabilities: [] }],
          next_cursor: null,
        };
      throw Error(p);
    });
    await model.loadAccess();
    paths.length = 0;
    if (order === "users-first") {
      await model.loadAccess(true);
      await model.loadAccess(false, true);
    } else {
      await model.loadAccess(false, true);
      await model.loadAccess(true);
    }
    expect(model.state.users.map((u) => u.user_id)).toEqual(["u1", "u2"]);
    expect(model.state.grants.map((g) => g.user_id)).toEqual(["u1", "u2"]);
    expect(paths).toHaveLength(2);
    expect(model.state.usersCursor).toBeNull();
    expect(model.state.grantsCursor).toBeNull();
  },
);

it.each([403, 404, 500])(
  "late output error %i cannot clear or pollute the next operation",
  async (status) => {
    const held = deferred();
    const model = make((p) =>
      p.includes("/output?") ? held.promise : op("B"),
    );
    model.state.operation = op("A");
    const old = model.loadOutput();
    await model.openOperation("B");
    held.resolve(rejected(status));
    await old;
    expect(model.state.operation?.operation_id).toBe("B");
    expect(model.state.node?.node_id).toBe("node");
    expect(model.state.error).toBe("");
  },
);
it.each([403, 404, 500])(
  "late cancel error %i cannot clear or pollute the next operation",
  async (status) => {
    const held = deferred();
    const model = make((p) => (p.endsWith("/cancel") ? held.promise : op("B")));
    model.state.operation = op("A");
    const old = model.cancel();
    await model.openOperation("B");
    held.resolve(rejected(status));
    await old;
    expect(model.state.operation?.operation_id).toBe("B");
    expect(model.state.node?.node_id).toBe("node");
    expect(model.state.error).toBe("");
  },
);

it.each([400, 412])(
  "definitive save rejection %i after navigation removes only its original recovery lock",
  async (status) => {
    const held = deferred();
    const model = make((p, i) => {
      if (i.method === "POST") return held.promise;
      if (p === "/api/v1/nodes/other") return { ...node, node_id: "other" };
      if (p.endsWith("/stacks")) return { items: [], next_cursor: null };
      throw Error(p);
    });
    const save = model.save();
    expect(model.state.records).toHaveLength(1);
    await model.selectNode("other");
    held.resolve(rejected(status));
    await save;
    expect(model.state.records).toHaveLength(0);
    expect(new SubmissionStore(sessionStorage).list(me.user_id)).toHaveLength(
      0,
    );
    expect(model.state.node?.node_id).toBe("other");
    expect(model.state.conflicted).toBe(false);
    expect(model.state.error).toBe("");
  },
);
it.each(["success", "network failure"])(
  "navigation retains original identity for receipt reconciliation after %s",
  async (outcome) => {
    const held = deferred();
    const model = make((p, i) => {
      if (i.method === "POST") return held.promise;
      if (p === "/api/v1/nodes/other") return { ...node, node_id: "other" };
      if (p.endsWith("/stacks")) return { items: [], next_cursor: null };
      throw Error(p);
    });
    const save = model.save();
    const key = model.state.records[0].key;
    await model.selectNode("other");
    if (outcome === "success") held.resolve({ version_id: "v2" });
    else held.reject(Error("network lost"));
    await save;
    expect(model.state.records[0]?.key).toBe(key);
    expect(model.state.node?.node_id).toBe("other");
    expect(model.state.draft).toBeNull();
    expect(model.state.baseVersion).toBeNull();
  },
);
it("late rejection cleanup does not erase a different signed-in user's pending record", async () => {
  const held = deferred();
  const model = make((p, i) => {
    if (p === "/api/v1/sessions") return { ...me, user_id: "other-user" };
    if (p === "/api/v1/nodes") return { items: [], next_cursor: null };
    if (i.method === "POST") return held.promise;
    throw Error(p);
  });
  const save = model.save();
  await model.login("other-user", "secret");
  const storage = new SubmissionStore(sessionStorage);
  storage.put({
    user_id: "other-user",
    key: "other-key",
    kind: "save",
    node_id: "other-node",
  });
  held.resolve(rejected(412));
  await save;
  expect(storage.list("other-user").map((r) => r.key)).toEqual(["other-key"]);
  expect(model.state.me?.user_id).toBe("other-user");
});
it("accepted deployment includes visible operation identity without fetching forbidden results", async () => {
  const paths: string[] = [];
  const model = make((p) => {
    paths.push(p);
    return { operation_id: "accepted-operation-id", state: "accepted" };
  });
  model.state.node!.capabilities = model.state.node!.capabilities.filter(
    (c) => c !== "operation.read",
  );
  await model.apply("v1");
  expect(model.state.message).toContain("accepted-operation-id");
  expect(paths).toEqual(["/api/v1/stacks/stack/apply"]);
  const root = mount(model);
  expect(root.textContent).toContain("accepted-operation-id");
});

it("App management round trip keeps loaded environment and default saved version", async () => {
  const transport = vi.fn(async (url: string) => {
    if (url === "/api/v1/me") return response(me);
    if (url === "/api/v1/nodes")
      return response({ items: [node], next_cursor: null });
    if (url === "/api/v1/nodes/node") return response(node);
    if (url === "/api/v1/nodes/node/stacks")
      return response({ items: [stack], next_cursor: null });
    if (url === "/api/v1/stacks/stack") return response(stack);
    if (url === "/api/v1/stacks/stack/versions/v1")
      return response({ version_id: "v1", config: encodeConfig(draft) });
    return response({ items: [], next_cursor: null });
  });
  vi.stubGlobal("fetch", transport);
  const root = document.createElement("div");
  document.body.append(root);
  const app = createApp(App);
  apps.push(app);
  app.mount(root);
  const flush = () => new Promise((r) => setTimeout(r, 0));
  await flush();
  root.querySelector<HTMLButtonElement>('[data-node="node"]')!.click();
  await flush();
  [...root.querySelectorAll<HTMLButtonElement>(".stack-nav button")]
    .find((b) => b.textContent === "demo")!
    .click();
  await flush();
  const toggle = root.querySelector<HTMLButtonElement>(
    '[data-action="access"]',
  )!;
  toggle.click();
  await flush();
  toggle.click();
  await flush();
  expect(area(root, "显式插值变量").value).toContain("existing-secret");
  expect(
    root.querySelector<HTMLButtonElement>('[data-action="apply"]')!.disabled,
  ).toBe(false);
});
