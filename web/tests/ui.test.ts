import { createApp, type App as VueApp } from "vue";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import App from "../src/App.vue";
const me = {
  user_id: "u",
  username: "reader",
  auth_rev: 1,
  system_capabilities: [],
  csrf_token: "token",
  expires_at: "2030-01-01T00:00:00Z",
};
const node = {
  node_id: "n",
  display_name: "测试节点",
  status: "online",
  capabilities: ["node.read", "config.read", "config.write", "stack.deploy"],
  agent_capabilities: [],
  last_seen_at: null,
  execution_profile: null,
  observation: null,
};
let app: VueApp | undefined;
const flushPromises = () => new Promise((resolve) => setTimeout(resolve, 0));
function mount() {
  const root = document.createElement("div");
  document.body.append(root);
  app = createApp(App);
  app.mount(root);
  return root;
}
beforeEach(() => {
  sessionStorage.clear();
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async (url: string) =>
        new Response(
          JSON.stringify(
            url.endsWith("/me")
              ? me
              : url.endsWith("/nodes")
                ? { items: [node], next_cursor: null }
                : url.endsWith("/nodes/n")
                  ? node
                  : { items: [], next_cursor: null },
          ),
          { headers: { "Content-Type": "application/json" } },
        ),
    ),
  );
});
afterEach(() => {
  app?.unmount();
  document.body.innerHTML = "";
  vi.unstubAllGlobals();
});
it("renders node context and requires separate save and saved-version deployment actions", async () => {
  const root = mount();
  await flushPromises();
  expect(root.textContent).toContain("测试节点");
  root.querySelector<HTMLButtonElement>('button[data-node="n"]')!.click();
  await flushPromises();
  root.querySelector<HTMLButtonElement>('[data-action="new-stack"]')!.click();
  await flushPromises();
  expect(root.textContent).toContain("新建 Stack");
  expect(root.querySelector('[data-action="save"]')).not.toBeNull();
  expect(root.querySelector('[data-action="apply"]')).toBeNull();
  expect(root.querySelector('textarea[aria-label="文件内容"]')).not.toBeNull();
});
it("keeps account management and output controls hidden without explicit rights", async () => {
  const root = mount();
  await flushPromises();
  expect(root.querySelector('[data-action="access"]')).toBeNull();
  expect(root.querySelector('[data-action="output"]')).toBeNull();
});
it("shows a login form on unauthenticated initialization without inventing nodes", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            error: { code: "unauthenticated", message: "请登录" },
          }),
          { status: 401 },
        ),
    ),
  );
  const root = mount();
  await flushPromises();
  expect(
    root.querySelector('input[autocomplete="current-password"]'),
  ).not.toBeNull();
  expect(root.textContent).not.toContain("测试节点");
});
