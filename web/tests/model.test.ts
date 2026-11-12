import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createModel } from "../src/model";
import { Api, HttpError } from "../src/api";
import { decodeConfig, encodeConfig } from "../src/config";
import { SubmissionStore } from "../src/submissions";

const config = {
  files: [{ path: "compose.yaml", content_b64: "c2VydmljZXM6IHt9DQo=" }],
  compose_files: ["compose.yaml"],
  interpolation_env_files: [],
  interpolation_env: { SECRET: "not-for-storage" },
  profiles: [],
};
const me = {
  user_id: "u",
  username: "user",
  auth_rev: 0,
  system_capabilities: [],
  csrf_token: "csrf",
  expires_at: "2030-01-01T00:00:00Z",
};
const node = (id: string) => ({
  node_id: id,
  display_name: id,
  status: "online",
  capabilities: [
    "node.read",
    "config.read",
    "config.write",
    "stack.deploy",
    "operation.read",
  ],
  agent_capabilities: [],
  last_seen_at: null,
  execution_profile: null,
  observation: null,
});
const stack = {
  node_id: "a",
  stack_id: "s",
  display_name: "demo",
  project_name: "demo",
  current_version_id: "v1",
  revision: 1,
  last_applied_version_id: null,
  last_operation_id: null,
  observation: null,
};
const models: ReturnType<typeof createModel>[] = [];
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}
function setup(handler: (path: string, init: RequestInit) => unknown) {
  const fetcher = vi.fn(
    async (url: string | URL | Request, init?: RequestInit) => {
      const v = await handler(String(url), init ?? {});
      if (v instanceof Response) return v;
      return new Response(JSON.stringify(v), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    },
  );
  const api = new Api(fetcher as typeof fetch);
  const model = createModel(api, new SubmissionStore(sessionStorage));
  models.push(model);
  return { model, api, fetcher };
}
async function opened(handler: (p: string, i: RequestInit) => unknown) {
  const ctx = setup((p, i) =>
    p === "/api/v1/me"
      ? me
      : p === "/api/v1/nodes"
        ? { items: [node("a"), node("b")], next_cursor: null }
        : p === "/api/v1/nodes/a"
          ? node("a")
          : p === "/api/v1/nodes/a/stacks"
            ? { items: [stack], next_cursor: null }
            : p === "/api/v1/stacks/s"
              ? stack
              : p === "/api/v1/stacks/s/versions/v1"
                ? { version_id: "v1", config }
                : handler(p, i),
  );
  await ctx.model.initialize();
  await ctx.model.selectNode("a");
  await ctx.model.openStack("s");
  return ctx;
}
beforeEach(() => sessionStorage.clear());
afterEach(() => {
  models.splice(0).forEach((m) => m.dispose());
  vi.useRealTimers();
});
describe("Web state safety", () => {
  it("sends explicit node grants with the target auth revision and keeps token out of recovery storage", async () => {
    const seen:RequestInit[]=[];
    const {model}=await opened((p,i)=>{seen.push(i); if(p.endsWith('/grants/target'))return {user_id:'target',capabilities:['node.read'],auth_rev:8};if(p.endsWith('/enrollment-tokens'))return {enrollment_id:'e',status:'pending',expires_at:'2026-01-01T00:10:00Z',token:'secret-enrollment'};throw Error(p)});
    model.state.me!.system_capabilities=['system.admin'];await model.manage('/nodes/a/grants/target',{capabilities:['node.read']},'PUT','7');expect((seen[0].headers as Record<string,string>)['If-Match']).toBe('"7"');expect(JSON.parse(String(seen[0].body))).toEqual({capabilities:['node.read']});await model.manage('/nodes/a/enrollment-tokens',{});expect(model.state.enrollment?.token).toBe('secret-enrollment');expect(sessionStorage.getItem('dm.submissions')).not.toContain('secret-enrollment');await model.selectNode('a');expect(model.state.enrollment).toBeNull();
  });
  it("does not let system administration implicitly grant configuration or deployment",async()=>{
    const {model,fetcher}=await opened(()=>{throw Error('unauthorized request')});model.state.me!.system_capabilities=['system.admin'];model.state.node!.capabilities=[];model.newStack();await model.save('demo');await model.apply('v1');expect(fetcher.mock.calls.some(([,i])=>i?.method==='POST')).toBe(false);
  });
  it("loads further pages using the opaque cursor rather than assuming first page is complete",async()=>{
    const paths:string[]=[];const {model}=setup(p=>{paths.push(p);if(p==='/api/v1/me')return me;if(p==='/api/v1/nodes')return {items:[node('a')],next_cursor:'opaque/next'};if(p==='/api/v1/nodes?cursor=opaque%2Fnext')return {items:[node('b')],next_cursor:null};throw Error(p)});await model.initialize();await model.loadNodes(true);expect(model.state.nodes.map(n=>n.node_id)).toEqual(['a','b']);expect(model.state.nodesCursor).toBeNull();expect(paths[2]).toBe('/api/v1/nodes?cursor=opaque%2Fnext');
  });
  it("rejects noncanonical base64 instead of silently changing saved bytes", () => {
    expect(() => decodeConfig({ ...config, files: [{path:"compose.yaml", content_b64:"YQ"}] })).toThrow();
  });
  it("never overlaps operation reads and polls pending release until confirmation", async () => {
    vi.useFakeTimers();
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const late=deferred<unknown>(); let reads=0;
    const op={operation_id:'o',node_id:'a',stack_id:'s',version_id:'v1',operation_type:'stack.apply',revision:1,state:'succeeded',cancel_requested:false,created_at:'2026-01-01T00:00:00Z',updated_at:'2026-01-01T00:00:00Z',result:null,release_status:'pending'};
    const {model}=await opened(p=>{if(p.endsWith('/operations/o')){reads++;return reads===1?late.promise:{...op,revision:2,release_status:'released'}}throw Error(p)});
    const first=model.openOperation('o'); await model.refreshOperation('o'); expect(reads).toBe(1);
    late.resolve(op);await first;await vi.advanceTimersByTimeAsync(2000);expect(reads).toBe(2);expect(model.state.operation?.release_status).toBe('released');await vi.advanceTimersByTimeAsync(10000);expect(reads).toBe(2);
  });
  it("preserves UTF-8, CRLF, explicit environment and order without normalizing", () => {
    const d = decodeConfig(config);
    expect(d.files[0].text).toBe("services: {}\r\n");
    expect(encodeConfig(d)).toEqual(config);
    expect(() =>
      decodeConfig({
        ...config,
        files: [{ path: "bad", content_b64: "/w==" }],
      }),
    ).toThrow();
  });
  it("retains edits typed during one in-flight save and advances the next base", async () => {
    const save = deferred<unknown>();
    const requests: RequestInit[] = [];
    const { model } = await opened((p, i) => {
      if (p.endsWith("/versions")) {
        requests.push(i);
        return save.promise;
      }
      throw Error(p);
    });
    model.edit((d) => {
      d.files[0].text = "first";
    });
    const waiting = model.save();
    model.edit((d) => {
      d.files[0].text = "second";
    });
    await model.save();
    expect(requests).toHaveLength(1);
    save.resolve({ version_id: "v2" });
    await waiting;
    expect(model.state.draft?.files[0].text).toBe("second");
    expect(model.state.baseVersion).toBe("v2");
    expect(model.state.dirty).toBe(true);
    expect((requests[0].headers as Record<string, string>)["If-Match"]).toBe(
      '"v1"',
    );
  });
  it("keeps unknown save locked and uses the original receipt key without storing secrets", async () => {
    let key = "";
    const { model } = await opened((p, i) => {
      if (p.endsWith("/versions")) {
        key = (i.headers as Record<string, string>)["Idempotency-Key"];
        throw new TypeError("network lost");
      }
      if (p.endsWith("/submissions/" + key))
        return {
          result_type: "version",
          result_id: "v2",
          response_status: 201,
        };
      throw Error(p);
    });
    model.edit((d) => {
      d.files[0].text = "secret-compose";
    });
    await model.save();
    expect(model.state.pending?.status).toBe("uncertain");
    expect(sessionStorage.getItem("dm.submissions")).not.toMatch(
      /secret-compose|not-for-storage|csrf/,
    );
    await model.reconcile();
    expect(model.state.baseVersion).toBe("v2");
    expect(model.state.dirty).toBe(false);
  });
  it("does not let node A late response replace node B", async () => {
    const late = deferred<unknown>();
    const { model } = setup((p) =>
      p === "/api/v1/me"
        ? me
        : p === "/api/v1/nodes"
          ? { items: [node("a"), node("b")], next_cursor: null }
          : p === "/api/v1/nodes/a"
            ? late.promise
            : p === "/api/v1/nodes/b"
              ? node("b")
              : p === "/api/v1/nodes/b/stacks"
                ? { items: [], next_cursor: null }
                : Promise.reject(Error(p)),
    );
    await model.initialize();
    const old = model.selectNode("a");
    await model.selectNode("b");
    late.resolve(node("a"));
    await old;
    expect(model.state.node?.node_id).toBe("b");
  });
  it("clears sensitive content on permission failure and rejects an older successful save", async () => {
    const late = deferred<unknown>();
    const { model } = await opened((p) =>
      p.endsWith("/versions") ? late.promise : Promise.reject(Error(p)),
    );
    model.edit((d) => {
      d.files[0].text = "secret";
    });
    const saving = model.save();
    model.handleError(new HttpError(403, "permission_denied", "权限已改变"));
    late.resolve({ version_id: "v2" });
    await saving;
    expect(model.state.draft).toBeNull();
    expect(model.state.baseVersion).toBeNull();
  });
  it("preserves a conflicting draft until the user explicitly adopts latest base", async () => {
    const { model } = await opened((p) =>
      p.endsWith("/versions")
        ? new Response(
            JSON.stringify({
              error: { code: "version_conflict", message: "冲突" },
            }),
            { status: 412 },
          )
        : Promise.reject(Error(p)),
    );
    model.edit((d) => {
      d.files[0].text = "my draft";
    });
    await model.save();
    expect(model.state.draft?.files[0].text).toBe("my draft");
    expect(model.state.conflicted).toBe(true);
  });
  it("sends cookies and CSRF on mutation, never on a cross-origin path", async () => {
    const { api, fetcher } = setup(() => ({ ok: true }));
    api.csrf = "token";
    await api.request("/nodes", {
      method: "POST",
      body: { display_name: "node" },
      key: "key",
    });
    expect(fetcher.mock.calls[0][0]).toBe("/api/v1/nodes");
    expect(fetcher.mock.calls[0][1]?.credentials).toBe("same-origin");
    expect(
      (fetcher.mock.calls[0][1]?.headers as Record<string, string>)[
        "X-CSRF-Token"
      ],
    ).toBe("token");
    await expect(api.request("https://evil.test")).rejects.toThrow();
  });
  it("blocks new same-node changes after reopening an unresolved submission", async () => {
    const { model, fetcher } = await opened(() => {
      throw Error("Unexpected mutation");
    });
    new SubmissionStore(sessionStorage).put({
      key: "old",
      user_id: "u",
      kind: "save",
      node_id: "a",
      stack_id: "s",
      version_id: "v1",
    });
    await model.refreshSession();
    model.edit((d) => {
      d.files[0].text = "later";
    });
    await model.save();
    expect(fetcher.mock.calls.some(([, i]) => i?.method === "POST")).toBe(
      false,
    );
    expect(model.state.error).toContain("核对");
  });
  it("preserves same-account submission identity across reauthentication", async () => {
    const { model } = setup((p) =>
      p === "/api/v1/sessions" ? me : { items: [], next_cursor: null },
    );
    new SubmissionStore(sessionStorage).put({
      key: "old",
      user_id: "u",
      kind: "apply",
      node_id: "a",
      stack_id: "s",
      version_id: "v1",
    });
    await model.login("user", "password");
    expect(model.state.records[0]?.key).toBe("old");
  });
  it("deploys a fixed version without polling when result permission is absent", async () => {
    let submitted: unknown;
    const { model, fetcher } = await opened((p, i) => {
      if (p.endsWith("/apply")) {
        submitted = JSON.parse(String(i.body));
        return { operation_id: "o", state: "accepted" };
      }
      throw Error(p);
    });
    model.state.node!.capabilities = model.state.node!.capabilities.filter(
      (c) => c !== "operation.read",
    );
    model.edit((d) => {
      d.files[0].text = "unsaved";
    });
    await model.apply("v1");
    expect(submitted).toEqual({ version_id: "v1" });
    expect(model.state.message).toContain("无结果查看权限");
    expect(
      fetcher.mock.calls.some(([p]) => String(p).includes("/operations/")),
    ).toBe(false);
  });
  it("keeps uncertain 404 receipt and exactly reuses the original frozen request", async () => {
    const mutations: RequestInit[] = [];
    const { model } = await opened((p, i) => {
      if (p.endsWith("/versions")) {
        mutations.push(i);
        if (mutations.length === 1) throw Error("lost");
        return { version_id: "v2" };
      }
      if (p.includes("/submissions/"))
        return new Response(
          JSON.stringify({ error: { code: "not_found", message: "none" } }),
          { status: 404 },
        );
      throw Error(p);
    });
    model.edit((d) => {
      d.files[0].text = "first";
    });
    await model.save();
    model.edit((d) => {
      d.files[0].text = "second";
    });
    await model.reconcile();
    expect(model.state.pending?.status).toBe("uncertain");
    await model.retryPending();
    expect(mutations[1].body).toBe(mutations[0].body);
    expect(mutations[1].headers).toEqual(mutations[0].headers);
    expect(model.state.draft?.files[0].text).toBe("second");
  });
  it("reads output using the server byte cursor only with separate output permission", async () => {
    const urls: string[] = [];
    const { model } = await opened((p) => {
      urls.push(p);
      return { text: "中", next_cursor: "3", truncated: false };
    });
    model.state.operation = {
      operation_id: "o",
      node_id: "a",
      stack_id: "s",
      version_id: "v1",
      operation_type: "stack.apply",
      revision: 1,
      state: "succeeded",
      cancel_requested: false,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      result: null,
      release_status: "released",
    };
    await model.loadOutput();
    expect(urls).toHaveLength(0);
    model.state.node!.capabilities.push("operation.output.read");
    await model.loadOutput();
    await model.loadOutput();
    expect(urls[1]).toContain("after=3");
    expect(model.state.output).toBe("中中");
  });
});
