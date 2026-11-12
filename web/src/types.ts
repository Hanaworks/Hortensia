// HTTP objects follow docs/specifications/http-api.md; shared payloads follow contracts/agent-v1.schema.json.
export type Capability =
  | "node.read"
  | "config.read"
  | "config.write"
  | "stack.deploy"
  | "operation.read"
  | "operation.output.read"
  | "operation.cancel";
export interface Me {
  user_id: string;
  username: string;
  auth_rev: number;
  system_capabilities: string[];
  csrf_token: string;
  expires_at: string;
}
export interface User {
  user_id: string;
  username: string;
  enabled: boolean;
  auth_rev: number;
}
export interface Grant {
  user_id: string;
  capabilities: Capability[];
  auth_rev: number;
}
export interface ServiceError {
  category: string;
  code: string;
  message: string;
  retry_action: string;
}
export interface Observation {
  status: "complete" | "incomplete" | "error";
  collected_at: string;
  items: {
    resource_kind: "container";
    resource_id: string;
    name: string;
    project_name: string | null;
    service_name: string | null;
    stack_id: string | null;
    image_id: string | null;
    state: string;
    health: string;
  }[];
  error: ServiceError | null;
}
export interface NodeInfo {
  node_id: string;
  display_name: string;
  status: "pending" | "online" | "offline" | "reconciling" | "revoked";
  capabilities: Capability[];
  agent_capabilities: string[];
  last_seen_at: string | null;
  execution_profile: {
    docker_endpoint_id: string;
    policy_revision: number;
  } | null;
  observation: Observation | null;
}
export interface Config {
  files: { path: string; content_b64: string }[];
  compose_files: string[];
  interpolation_env_files: string[];
  interpolation_env: Record<string, string>;
  profiles: string[];
}
export interface Draft extends Omit<Config, "files"> {
  files: { path: string; text: string }[];
}
export interface Stack {
  stack_id: string;
  node_id: string;
  display_name: string;
  project_name: string;
  current_version_id: string;
  revision: number;
  last_applied_version_id: string | null;
  last_operation_id: string | null;
  observation: Observation | null;
}
export interface Version {
  version_id: string;
  config: Config;
}
export type OperationState =
  | "accepted"
  | "preparing"
  | "ready"
  | "authorized"
  | "running"
  | "succeeded"
  | "failed"
  | "canceled"
  | "reconciliation_required";
export interface Operation {
  operation_id: string;
  node_id: string;
  stack_id: string;
  version_id: string;
  operation_type: "stack.apply";
  revision: number;
  state: OperationState;
  cancel_requested: boolean;
  created_at: string;
  updated_at: string;
  result: {
    outcome: "not_started" | "normal_exit" | "interrupted" | "unknown";
    ownership_status: string;
    interrupt_reason: string | null;
    exit_code: number | null;
    error: ServiceError | null;
    observation: Observation | null;
    not_started: boolean;
    output_truncated: boolean;
    finished_at: string;
  } | null;
  release_status: "held" | "pending" | "released";
}
export interface Page<T> {
  items: T[];
  next_cursor: string | null;
}
export interface Receipt {
  result_type:
    | "user"
    | "node"
    | "grant"
    | "enrollment"
    | "stack"
    | "version"
    | "operation";
  result_id: string;
  response_status: number;
}
export interface Enrollment {
  enrollment_id: string;
  status: string;
  expires_at: string;
  token: string | null;
}
