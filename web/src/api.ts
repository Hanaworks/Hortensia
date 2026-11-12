export class HttpError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public retryAction = "none",
  ) {
    super(message);
  }
}
export interface RequestOptions {
  method?: string;
  body?: unknown;
  key?: string;
  etag?: string;
}
export class Api {
  csrf = "";
  constructor(
    private readonly transport: typeof fetch = globalThis.fetch.bind(
      globalThis,
    ),
  ) {}
  async request<T>(path: string, options: RequestOptions = {}): Promise<T> {
    if (!path.startsWith("/") || path.startsWith("//") || path.includes("://"))
      throw new Error("接口地址无效");
    const method = options.method ?? "GET";
    const headers: Record<string, string> = { Accept: "application/json" };
    if (options.body !== undefined)
      headers["Content-Type"] = "application/json";
    if (method !== "GET" && method !== "HEAD" && this.csrf)
      headers["X-CSRF-Token"] = this.csrf;
    if (options.key) headers["Idempotency-Key"] = options.key;
    if (options.etag) headers["If-Match"] = '"' + options.etag + '"';
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15000);
    try {
      const response = await this.transport("/api/v1" + path, {
        method,
        headers,
        credentials: "same-origin",
        cache: "no-store",
        redirect: "error",
        signal: controller.signal,
        body:
          options.body === undefined ? undefined : JSON.stringify(options.body),
      });
      if (response.status === 204) return undefined as T;
      const value = await response.json().catch(() => {
        throw new Error("服务返回无法读取，请核对原提交");
      });
      if (!response.ok)
        throw new HttpError(
          response.status,
          value?.error?.code ?? "invalid_response",
          value?.error?.message ?? "请求未完成",
          value?.error?.retry_action,
        );
      return value as T;
    } finally {
      clearTimeout(timer);
    }
  }
}
