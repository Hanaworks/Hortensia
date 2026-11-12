export interface Submission {
  key: string;
  user_id: string;
  kind: "create" | "save" | "apply" | "admin";
  node_id?: string;
  stack_id?: string;
  version_id?: string;
  operation_id?: string;
}
const storageKey = "dm.submissions";
export class SubmissionStore {
  constructor(private storage: Storage) {}
  list(user: string): Submission[] {
    try {
      const v = JSON.parse(this.storage.getItem(storageKey) ?? "[]");
      return Array.isArray(v)
        ? v
            .filter(
              (x) =>
                x &&
                typeof x.key === "string" &&
                x.user_id === user &&
                ["create", "save", "apply", "admin"].includes(x.kind),
            )
            .map((x) => this.safe(x))
        : [];
    } catch {
      return [];
    }
  }
  private safe(s: Submission): Submission {
    return {
      key: s.key,
      user_id: s.user_id,
      kind: s.kind,
      ...(s.node_id ? { node_id: s.node_id } : {}),
      ...(s.stack_id ? { stack_id: s.stack_id } : {}),
      ...(s.version_id ? { version_id: s.version_id } : {}),
      ...(s.operation_id ? { operation_id: s.operation_id } : {}),
    };
  }
  put(s: Submission) {
    const entries = this.list(s.user_id).filter((x) => x.key !== s.key);
    entries.push(this.safe(s));
    this.storage.setItem(storageKey, JSON.stringify(entries));
  }
  remove(s: Submission) {
    const entries = this.list(s.user_id);
    // A late response can outlive an account change. Missing original identity
    // must not replace the current account's independently stored submissions.
    if (!entries.some((entry) => entry.key === s.key)) return;
    this.storage.setItem(
      storageKey,
      JSON.stringify(entries.filter((x) => x.key !== s.key)),
    );
  }
  clear() {
    this.storage.removeItem(storageKey);
  }
  retainUser(user: string) {
    this.storage.setItem(storageKey, JSON.stringify(this.list(user)));
  }
}
