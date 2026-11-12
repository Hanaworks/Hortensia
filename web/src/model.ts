import { reactive } from "vue";
import { Api, HttpError, type RequestOptions } from "./api";
import { clone, decodeConfig, emptyDraft, encodeConfig } from "./config";
import { SubmissionStore, type Submission } from "./submissions";
import type {
  Capability,
  Draft,
  Enrollment,
  Grant,
  Me,
  NodeInfo,
  Operation,
  Page,
  Receipt,
  Stack,
  User,
  Version,
} from "./types";

interface Pending {
  record: Submission;
  status: "sending" | "uncertain";
  path: string;
  options: RequestOptions;
  draft?: Draft;
  editRevision?: number;
  generation: number;
}
interface State {
  contextRevision: number;
  me: Me | null;
  nodes: NodeInfo[];
  nodesCursor: string | null;
  node: NodeInfo | null;
  stacks: Stack[];
  stacksCursor: string | null;
  stack: Stack | null;
  draft: Draft | null;
  baseVersion: string | null;
  dirty: boolean;
  editRevision: number;
  pending: Pending | null;
  records: Submission[];
  conflicted: boolean;
  latest: { version: string; draft: Draft } | null;
  operation: Operation | null;
  operations: Operation[];
  operationsCursor: string | null;
  output: string;
  outputCursor: string;
  outputTruncated: boolean;
  users: User[];
  usersCursor: string | null;
  grants: Grant[];
  grantsCursor: string | null;
  enrollment: Enrollment | null;
  message: string;
  error: string;
  loading: boolean;
  lastChecked: string | null;
}
export function createModel(api: Api, submissions: SubmissionStore) {
  const state = reactive<State>({
    contextRevision: 0,
    me: null,
    nodes: [],
    nodesCursor: null,
    node: null,
    stacks: [],
    stacksCursor: null,
    stack: null,
    draft: null,
    baseVersion: null,
    dirty: false,
    editRevision: 0,
    pending: null,
    records: [],
    conflicted: false,
    latest: null,
    operation: null,
    operations: [],
    operationsCursor: null,
    output: "",
    outputCursor: "0",
    outputTruncated: false,
    users: [],
    usersCursor: null,
    grants: [],
    grantsCursor: null,
    enrollment: null,
    message: "",
    error: "",
    loading: false,
    lastChecked: null,
  });
  let generation = 0;
  let identity = 0;
  let pollTimer: ReturnType<typeof setTimeout> | undefined;
  let refreshTimer: ReturnType<typeof setTimeout> | undefined;
  let disposed = false;
  let operationGeneration = 0;
  let operationInFlight: number | null = null;
  let polling = false;
  let outputBusy = false;
  let failures = 0;
  const has = (...caps: Capability[]) =>
    caps.every((c) => state.node?.capabilities.includes(c));
  const admin = () =>
    state.me?.system_capabilities.includes("system.admin") ?? false;
  function records() {
    state.records = state.me ? submissions.list(state.me.user_id) : [];
  }
  function clearContext() {
    generation++;
    state.contextRevision++;
    operationGeneration++;
    clearTimeout(pollTimer);
    state.node = null;
    state.stack = null;
    state.stacks = [];
    state.stacksCursor = null;
    state.draft = null;
    state.baseVersion = null;
    state.dirty = false;
    state.editRevision = 0;
    state.pending = null;
    state.conflicted = false;
    state.latest = null;
    state.operation = null;
    state.operations = [];
    state.operationsCursor = null;
    state.output = "";
    state.outputCursor = "0";
    state.outputTruncated = false;
    state.grants = [];
    state.grantsCursor = null;
    state.enrollment = null;
    state.loading = false;
    state.lastChecked = null;
  }
  function clearIdentity() {
    identity++;
    clearContext();
    state.me = null;
    api.csrf = "";
    state.nodes = [];
    state.users = [];
    state.records = [];
    clearTimeout(refreshTimer);
  }
  function handleError(error: unknown, g = generation) {
    if (g !== generation) return;
    state.error = error instanceof Error ? error.message : "请求未完成";
    if (error instanceof HttpError) {
      if (error.status === 401) {
        clearIdentity();
      } else if (error.status === 403 || error.status === 404) {
        clearContext();
        state.error = "对象不可用或权限已变化，请重新选择节点。";
      }
    }
  }
  function setMe(me: Me) {
    if (
      !me ||
      typeof me.user_id !== "string" ||
      !Array.isArray(me.system_capabilities) ||
      typeof me.csrf_token !== "string"
    )
      throw Error("身份返回格式无效");
    state.me = me;
    api.csrf = me.csrf_token;
    records();
  }
  async function initialize() {
    const i = identity;
    state.loading = true;
    try {
      const me = await api.request<Me>("/me");
      if (i !== identity) return;
      setMe(me);
      await loadNodes();
    } catch (e) {
      if (i === identity) handleError(e);
    } finally {
      if (i === identity) state.loading = false;
    }
  }
  async function login(username: string, password: string) {
    clearIdentity();
    const i = identity;
    state.loading = true;
    state.error = "";
    try {
      const me = await api.request<Me>("/sessions", {
        method: "POST",
        body: { username, password },
      });
      if (i !== identity) return;
      submissions.retainUser(me.user_id);
      setMe(me);
      await loadNodes();
      start();
    } catch (e) {
      if (i === identity) handleError(e);
    } finally {
      if (i === identity) state.loading = false;
    }
  }
  async function logout() {
    const csrf = api.csrf;
    clearIdentity();
    submissions.clear();
    api.csrf = csrf;
    try {
      await api.request("/sessions/current", { method: "DELETE" });
    } catch {
      state.error = "退出请求未确认，请关闭页面并重新核对会话。";
    } finally {
      api.csrf = "";
    }
  }
  async function loadNodes(more = false) {
    const i = identity;
    const cursor = more ? state.nodesCursor : null;
    try {
      const p = await api.request<Page<NodeInfo>>(
        "/nodes" + (cursor ? "?cursor=" + encodeURIComponent(cursor) : ""),
      );
      if (i !== identity) return;
      state.nodes = more ? [...state.nodes, ...p.items] : p.items;
      state.nodesCursor = p.next_cursor;
    } catch (e) {
      if (i === identity) handleError(e);
    }
  }
  async function selectNode(id: string) {
    clearContext();
    state.error = "";
    state.message = "";
    const g = generation;
    state.loading = true;
    try {
      const n = await api.request<NodeInfo>("/nodes/" + id);
      if (g !== generation) return;
      if (n.node_id !== id) throw Error("节点返回归属不符");
      state.node = n;
      if (has("node.read")) await loadStacks();
    } catch (e) {
      handleError(e, g);
    } finally {
      if (g === generation) state.loading = false;
    }
  }
  async function loadStacks(more = false) {
    if (!state.node || !has("node.read")) return;
    const g = generation;
    const id = state.node.node_id;
    try {
      const p = await api.request<Page<Stack>>(
        "/nodes/" +
          id +
          "/stacks" +
          (more && state.stacksCursor
            ? "?cursor=" + encodeURIComponent(state.stacksCursor)
            : ""),
      );
      if (g !== generation) return;
      if (p.items.some((s) => s.node_id !== id))
        throw Error("项目返回归属不符");
      state.stacks = more ? [...state.stacks, ...p.items] : p.items;
      state.stacksCursor = p.next_cursor;
    } catch (e) {
      handleError(e, g);
    }
  }
  function resetEditor() {
    generation++;
    state.contextRevision++;
    state.draft = null;
    state.stack = null;
    state.baseVersion = null;
    state.dirty = false;
    state.editRevision = 0;
    state.pending = null;
    state.conflicted = false;
    state.latest = null;
    state.error = "";
    state.message = "";
    state.operation = null;
    state.output = "";
    operationGeneration++;
    clearTimeout(pollTimer);
  }
  function newStack() {
    if (!has("node.read", "config.read", "config.write")) return;
    resetEditor();
    state.draft = emptyDraft();
    state.dirty = true;
  }
  async function openStack(id: string, version?: string) {
    if (!state.node || !has("node.read")) return;
    resetEditor();
    const g = generation;
    const nodeId = state.node.node_id;
    state.loading = true;
    try {
      const s = await api.request<Stack>("/stacks/" + id);
      if (g !== generation) return;
      if (s.node_id !== nodeId || s.stack_id !== id)
        throw Error("项目返回归属不符");
      state.stack = s;
      if (has("config.read")) {
        const v = await api.request<Version>(
          "/stacks/" + id + "/versions/" + (version ?? s.current_version_id),
        );
        if (g !== generation) return;
        if (v.version_id !== (version ?? s.current_version_id))
          throw Error("配置版本返回不符");
        state.draft = decodeConfig(v.config);
        state.baseVersion = v.version_id;
      }
    } catch (e) {
      handleError(e, g);
    } finally {
      if (g === generation) state.loading = false;
    }
  }
  function edit(change: (draft: Draft) => void) {
    if (!state.draft || !has("config.write")) return;
    change(state.draft);
    state.editRevision++;
    state.dirty = true;
  }
  function begin(
    record: Submission,
    path: string,
    options: RequestOptions,
    draft?: Draft,
  ): Pending {
    records();
    if (
      state.records.some(
        (r) =>
          r.kind === "admin" ||
          record.kind === "admin" ||
          r.node_id === record.node_id,
      )
    )
      throw Error("存在尚未确认的提交，请先核对原提交。");
    submissions.put(record);
    records();
    const p: Pending = {
      record,
      status: "sending",
      path,
      options: { ...options, key: record.key },
      draft,
      editRevision: state.editRevision,
      generation,
    };
    state.pending = p;
    return p;
  }
  async function send(p: Pending) {
    try {
      const r = await api.request<Record<string, unknown>>(p.path, p.options);
      if (p.generation !== generation) return;
      await finish(p, r);
    } catch (e) {
      const uncertain =
        !(e instanceof HttpError) || e.status >= 500 || e.status === 429;
      if (!uncertain) {
        // A definite rejection terminates this original submission even after
        // navigation. View updates below still belong to the captured generation.
        try {
          submissions.remove(p.record);
          records();
        } catch (storageError) {
          handleError(storageError, p.generation);
          return;
        }
      }
      if (p.generation !== generation) return;
      if (uncertain) {
        state.pending = { ...p, status: "uncertain" };
        state.error = "提交结果待核对。请先核对原提交，勿重新创建。";
        return;
      }
      state.pending = null;
      if (e instanceof HttpError && e.status === 412) {
        state.conflicted = true;
        state.error = "保存冲突：草稿仍保留，请读取最新版本后比较。";
      } else handleError(e, p.generation);
    }
  }
  async function finish(p: Pending, r: Record<string, unknown>) {
    if (p.record.kind === "save" || p.record.kind === "create") {
      if (typeof r.version_id !== "string")
        throw Error("保存响应缺少版本，需核对原提交");
      state.baseVersion = r.version_id;
      state.dirty = state.editRevision !== p.editRevision;
      state.conflicted = false;
      state.latest = null;
      if (p.record.kind === "create") {
        if (typeof r.stack_id !== "string")
          throw Error("保存响应缺少项目，需核对原提交");
        const name = (p.options.body as { project_name: string }).project_name;
        state.stack = {
          stack_id: r.stack_id,
          node_id: p.record.node_id!,
          display_name: name,
          project_name: name,
          current_version_id: r.version_id,
          revision: 0,
          last_applied_version_id: null,
          last_operation_id: null,
          observation: null,
        };
      } else if (state.stack) state.stack.current_version_id = r.version_id;
      state.message = state.dirty
        ? "已保存提交的内容，后续编辑尚未保存。"
        : "配置已保存。部署需单独提交。";
    } else if (p.record.kind === "apply") {
      if (typeof r.operation_id !== "string")
        throw Error("部署回执缺少操作身份，请核对");
      state.message = has("operation.read")
        ? "部署已受理。操作：" + r.operation_id
        : "已受理，无结果查看权限。操作：" + r.operation_id;
      if (state.stack) state.stack.last_operation_id = r.operation_id;
      p.record.operation_id = r.operation_id;
    } else {
      state.message = "管理变更已受理。";
      if ("enrollment_id" in r) state.enrollment = r as unknown as Enrollment;
    }
    submissions.remove(p.record);
    state.pending = null;
    records();
    if (
      p.record.kind === "apply" &&
      p.record.operation_id &&
      has("operation.read")
    )
      await openOperation(p.record.operation_id);
  }
  async function save(projectName = "") {
    if (
      state.pending ||
      state.conflicted ||
      !state.draft ||
      !state.node ||
      !state.me ||
      !has("node.read", "config.read", "config.write")
    )
      return;
    try {
      const draft = clone(state.draft);
      const config = encodeConfig(draft);
      const kind = state.stack ? "save" : "create";
      if (kind === "create" && !/^[a-z0-9][a-z0-9_-]{0,62}$/.test(projectName))
        throw Error(
          "项目名须为1—63位小写字母、数字、下划线或连字符，以字母或数字开头",
        );
      const record: Submission = {
        key: crypto.randomUUID(),
        user_id: state.me.user_id,
        kind,
        node_id: state.node.node_id,
        stack_id: state.stack?.stack_id,
        version_id: state.baseVersion ?? undefined,
      };
      const path = state.stack
        ? "/stacks/" + state.stack.stack_id + "/versions"
        : "/nodes/" + state.node.node_id + "/stacks";
      const p = begin(
        record,
        path,
        {
          method: "POST",
          body: state.stack
            ? { config }
            : { project_name: projectName, config },
          etag: state.baseVersion ?? undefined,
        },
        draft,
      );
      state.error = "";
      await send(p);
    } catch (e) {
      handleError(e);
    }
  }
  async function reconcile(record = state.pending?.record) {
    if (!record || !state.me || record.user_id !== state.me.user_id) return;
    const g = generation;
    try {
      const receipt = await api.request<Receipt>("/submissions/" + record.key);
      if (g !== generation) return;
      const p = state.pending?.record.key === record.key ? state.pending : null;
      if (p) {
        if (receipt.result_type === "version")
          await finish(p, { version_id: receipt.result_id });
        else if (receipt.result_type === "operation")
          await finish(p, { operation_id: receipt.result_id });
        else if (receipt.result_type === "stack") {
          const original = await api.request<Record<string, unknown>>(
            p.path,
            p.options,
          );
          if (g === generation) await finish(p, original);
        } else {
          await finish(p, {});
          state.message =
            "原管理提交已确认；认领令牌无法回读，请按需重新签发。";
        }
      } else {
        submissions.remove(record);
        records();
        state.message = "原提交已确认：" + receipt.result_id;
        if (
          receipt.result_type === "operation" &&
          state.node?.node_id === record.node_id &&
          has("operation.read")
        )
          await openOperation(receipt.result_id);
        else if (
          receipt.result_type === "version" &&
          record.stack_id &&
          state.node?.node_id === record.node_id
        )
          await openStack(record.stack_id, receipt.result_id);
        else if (
          receipt.result_type === "stack" &&
          state.node?.node_id === record.node_id
        )
          await openStack(receipt.result_id);
      }
    } catch (e) {
      if (g !== generation) return;
      if (e instanceof HttpError && e.status === 404) {
        state.error =
          "尚未查到回执，不代表原请求未提交。原输入仍在本页时可用同一提交重试；重开后仅能继续核对。";
      } else handleError(e, g);
    }
  }
  async function retryPending() {
    if (state.pending?.status !== "uncertain") return;
    const p = state.pending;
    state.pending = { ...p, status: "sending" };
    await send(p);
  }
  async function loadConflict() {
    if (!state.stack || !state.conflicted) return;
    const g = generation;
    try {
      const s = await api.request<Stack>("/stacks/" + state.stack.stack_id);
      const v = await api.request<Version>(
        "/stacks/" + s.stack_id + "/versions/" + s.current_version_id,
      );
      if (g !== generation) return;
      if (
        s.node_id !== state.node?.node_id ||
        v.version_id !== s.current_version_id
      )
        throw Error("冲突版本归属不符");
      state.latest = { version: v.version_id, draft: decodeConfig(v.config) };
    } catch (e) {
      handleError(e, g);
    }
  }
  function adoptLatest() {
    if (!state.latest) return;
    state.baseVersion = state.latest.version;
    state.latest = null;
    state.conflicted = false;
    state.dirty = true;
    state.message = "已选择最新基准，保留你的草稿。请确认内容后保存。";
  }
  async function apply(version = state.baseVersion) {
    if (
      state.pending ||
      !state.stack ||
      !state.me ||
      !version ||
      !has("node.read", "config.read", "stack.deploy")
    )
      return;
    const record: Submission = {
      key: crypto.randomUUID(),
      user_id: state.me.user_id,
      kind: "apply",
      node_id: state.node!.node_id,
      stack_id: state.stack.stack_id,
      version_id: version,
    };
    try {
      const p = begin(record, "/stacks/" + record.stack_id + "/apply", {
        method: "POST",
        body: { version_id: version },
      });
      await send(p);
    } catch (e) {
      handleError(e);
    }
  }
  async function loadOperations(more = false) {
    if (!state.node || !has("node.read", "operation.read")) return;
    const g = generation;
    try {
      const p = await api.request<Page<Operation>>(
        "/operations?node_id=" +
          state.node.node_id +
          (more && state.operationsCursor
            ? "&cursor=" + encodeURIComponent(state.operationsCursor)
            : ""),
      );
      if (g !== generation) return;
      if (p.items.some((o) => o.node_id !== state.node?.node_id))
        throw Error("操作返回归属不符");
      state.operations = more ? [...state.operations, ...p.items] : p.items;
      state.operationsCursor = p.next_cursor;
    } catch (e) {
      handleError(e, g);
    }
  }
  function shouldPoll(o: Operation) {
    return !(
      ["succeeded", "failed", "canceled"].includes(o.state) &&
      o.release_status === "released"
    );
  }
  async function openOperation(id: string) {
    if (!has("node.read", "operation.read")) return;
    operationGeneration++;
    clearTimeout(pollTimer);
    state.operation = null;
    state.output = "";
    state.outputCursor = "0";
    state.outputTruncated = false;
    failures = 0;
    await refreshOperation(id);
  }
  async function refreshOperation(id = state.operation?.operation_id) {
    if (!id || !has("operation.read") || disposed) return;
    const g = generation;
    const og = operationGeneration;
    if (operationInFlight === og) return;
    operationInFlight = og;
    clearTimeout(pollTimer);
    try {
      const op = await api.request<Operation>("/operations/" + id);
      if (g !== generation || og !== operationGeneration) return;
      if (op.node_id !== state.node?.node_id || op.operation_id !== id)
        throw Error("操作返回归属不符");
      if (!state.operation || op.revision >= state.operation.revision)
        state.operation = op;
      state.lastChecked = new Date().toISOString();
      failures = 0;
    } catch (e) {
      if (g === generation && og === operationGeneration) {
        failures++;
        handleError(e, g);
      }
    } finally {
      if (operationInFlight === og) operationInFlight = null;
      if (
        g === generation &&
        og === operationGeneration &&
        state.operation &&
        shouldPoll(state.operation) &&
        !disposed
      ) {
        clearTimeout(pollTimer);
        pollTimer = setTimeout(
          () => void refreshOperation(id),
          failures
            ? Math.min(30000, 2000 * 2 ** failures)
            : document.hidden
              ? 10000
              : 2000,
        );
      }
    }
  }
  async function cancel() {
    if (!state.operation || !has("operation.read", "operation.cancel")) return;
    const g = generation;
    const og = operationGeneration;
    const id = state.operation.operation_id;
    try {
      const op = await api.request<Operation>("/operations/" + id + "/cancel", {
        method: "POST",
      });
      if (
        g !== generation ||
        og !== operationGeneration ||
        state.operation?.operation_id !== id
      )
        return;
      if (op.revision >= state.operation.revision) state.operation = op;
      state.message = "已提交取消请求，请继续核对实际结果。";
    } catch (e) {
      if (og === operationGeneration) handleError(e, g);
    }
  }
  async function loadOutput() {
    if (
      outputBusy ||
      !state.operation ||
      !has("node.read", "operation.read", "operation.output.read")
    )
      return;
    const g = generation;
    const og = operationGeneration;
    const id = state.operation.operation_id;
    outputBusy = true;
    try {
      const p = await api.request<{
        text: string;
        next_cursor: string;
        truncated: boolean;
      }>(
        "/operations/" +
          id +
          "/output?after=" +
          state.outputCursor +
          "&limit=65536",
      );
      if (g !== generation || og !== operationGeneration) return;
      state.output += p.text;
      state.outputCursor = p.next_cursor;
      state.outputTruncated = p.truncated;
    } catch (e) {
      if (og === operationGeneration) handleError(e, g);
    } finally {
      outputBusy = false;
    }
  }
  async function loadAccess(moreUsers = false, moreGrants = false) {
    if (!admin()) return;
    const g = generation;
    const refreshBoth = !moreUsers && !moreGrants;
    try {
      if (refreshBoth || (moreUsers && state.usersCursor)) {
        const p = await api.request<Page<User>>(
          "/users" +
            (moreUsers && state.usersCursor
              ? "?cursor=" + encodeURIComponent(state.usersCursor)
              : ""),
        );
        if (g !== generation) return;
        state.users = moreUsers ? [...state.users, ...p.items] : p.items;
        state.usersCursor = p.next_cursor;
      }
      if (state.node && (refreshBoth || (moreGrants && state.grantsCursor))) {
        const grants = await api.request<Page<Grant>>(
          "/nodes/" +
            state.node.node_id +
            "/grants" +
            (moreGrants && state.grantsCursor
              ? "?cursor=" + encodeURIComponent(state.grantsCursor)
              : ""),
        );
        if (g !== generation) return;
        state.grants = moreGrants
          ? [...state.grants, ...grants.items]
          : grants.items;
        state.grantsCursor = grants.next_cursor;
      }
    } catch (e) {
      handleError(e, g);
    }
  }
  async function manage(
    path: string,
    body?: unknown,
    method = "POST",
    etag?: string,
  ) {
    if (!admin() || state.pending || !state.me) return;
    try {
      state.enrollment = null;
      const p = begin(
        {
          key: crypto.randomUUID(),
          user_id: state.me.user_id,
          kind: "admin",
          node_id: state.node?.node_id,
        },
        path,
        { method, body, etag },
      );
      await send(p);
    } catch (e) {
      handleError(e);
    }
  }
  async function refreshSession() {
    if (!state.me || polling || disposed) return;
    polling = true;
    const i = identity;
    const g = generation;
    try {
      const me = await api.request<Me>("/me");
      if (i !== identity) return;
      if (
        me.user_id !== state.me.user_id ||
        me.auth_rev !== state.me.auth_rev
      ) {
        clearContext();
        state.users = [];
        setMe(me);
        state.message = "权限已更新，请重新选择节点。";
      } else setMe(me);
      if (g === generation && state.node) {
        const node = await api.request<NodeInfo>(
          "/nodes/" + state.node.node_id,
        );
        if (g !== generation) return;
        if (
          JSON.stringify(node.capabilities) !==
          JSON.stringify(state.node.capabilities)
        ) {
          clearContext();
          state.message = "节点权限已更新，请重新选择节点。";
        } else state.node = node;
      }
      await loadNodes();
    } catch (e) {
      if (i === identity) handleError(e, g);
    } finally {
      polling = false;
    }
  }
  function start() {
    disposed = false;
    clearTimeout(refreshTimer);
    const tick = async () => {
      await refreshSession();
      if (!disposed && state.me) refreshTimer = setTimeout(tick, 15000);
    };
    refreshTimer = setTimeout(tick, 15000);
  }
  function dispose() {
    disposed = true;
    clearTimeout(refreshTimer);
    clearTimeout(pollTimer);
    clearIdentity();
  }
  return {
    state,
    has,
    admin,
    initialize,
    login,
    logout,
    loadNodes,
    selectNode,
    loadStacks,
    newStack,
    openStack,
    edit,
    save,
    reconcile,
    retryPending,
    loadConflict,
    adoptLatest,
    apply,
    loadOperations,
    openOperation,
    refreshOperation,
    cancel,
    loadOutput,
    loadAccess,
    manage,
    refreshSession,
    start,
    dispose,
    handleError,
  };
}
export type Model = ReturnType<typeof createModel>;
