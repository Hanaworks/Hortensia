# 数据与 API 设计初稿 v0.1

日期：2026-10-30。**本文具体方案均为助手推荐、待用户审阅，不代表用户已确认或接口已冻结。**
依据：[需求基线](requirements.md)、[待定事项](open-questions.md)、[架构初稿](architecture.md)。字段和路径为草案，尚未编码验证。

## 1. 数据实体

ID 建议使用不可猜测的稳定标识；UTC 存储时间，UI 按用户时区显示。名称不充当主键。

| 实体 | 核心字段 | 约束与含义 |
| --- | --- | --- |
| User | id, login_name, state, role | 无公开注册；role 首版仅 admin；禁用即撤销会话 |
| LocalCredential | user_id, password_hash, changed_at | 不保存明文密码；哈希算法与参数实施前验证 |
| ExternalIdentity | user_id, issuer, sub | `(issuer, sub)` 唯一；邮箱仅展示 |
| Session | id_hash, user_id, expires_at, revoked_at | 服务端可撤销；不把令牌原文写审计 |
| Node | id, name, connection_state, last_seen, capabilities | 连接状态与 Docker 可用状态分开，离线观察带时间 |
| NodeCredential | id, node_id, fingerprint, expires_at, revoked_at | 每节点独立；证书私钥留在节点 |
| Enrollment | id, token_hash, node_binding, expires_at, consumed_at | 短期、一次使用；消费与绑定事务完成 |
| Stack | id, node_id, name, draft_version, project_name, work_dir_key | node_id / project_name 创建后固定；不靠重命名实现迁移 |
| Draft | stack_id, version, manifest, updated_by, updated_at | 可编辑；乐观并发；删除文件须更新完整清单 |
| Revision | id, stack_id, digest, manifest, created_by, created_at | 不可变；来源原文与输入环境确定 |
| Blob | digest, size, storage_key, encrypted | 内容寻址；权限检查不能只凭摘要下载 |
| CrossOrigin | target_stack_id, source_stack_id, source_revision_id, source_draft_version | 选已保存草稿时先快照；只记录来源，不后续同步 |
| Task | id, node_id, stack_id, action, revision_id, state, idempotency_key | 持久队列；保存请求摘要、错误、阶段与结果 |
| ExecutionRecord | task_id, agent_epoch, phase, command_digest, exit_code | Agent 本地持久记录；Manager 保留收到的证据 |
| TaskEvent | task_id, seq, type, payload, observed_at | task 内 seq 唯一；事件可重传去重 |
| ResourceSnapshot | node_id, docker_id, kind, observed_at, payload | 观察缓存，不替代 Docker 或配置权威 |
| AuditEvent | actor, action, object_id, request_id, result, occurred_at | 不记秘密正文；区分用户操作与 Agent 事件 |

配置 manifest 至少包含逻辑相对路径、用途、blob 摘要、大小和敏感标记；敏感标记不是安全边界。
环境文件与附件可能含秘密，按敏感正文保护；不把解析后的完整环境写入普通日志或错误回包。
数据库保存引用，正文写入成功且校验后再提交引用；无引用 blobs 延迟回收，备份与任务引用期间不得回收。
数据库与 blobs 分开不天然一致；备份先暂停配置写入/任务创建或采用一致性快照协议，不能随意复制活动数据库文件。

## 2. 不变量与版本

- `(node_id, normalized_stack_name)` 唯一；名称标准化规则要固定并在 UI 提示，不能静默改用户名称。
- 同节点实际 Compose 项目名唯一且创建后固定；从原生输入解析建议值后由用户明确确认，执行时通过可见的原生 `-p` 参数传入，记录在执行上下文，不附加额外前缀；输入与固定名矛盾须提示处理。其他资源冲突独立检查。
- 一个 Revision 只属于一个 Stack；跨节点复制建立目标草稿，发布目标自己的 Revision。
- 保存草稿只增加 Draft.version；部署时固定为不可变 revision，不承诺每次保存都提供可浏览历史或回滚功能。
- Stack 同时最多一个变更任务；queued / running / unknown 均阻止新运行变更与注销，允许保存草稿；容器直接变更共用锁。
- Task 绑定动作、node、stack、revision 与请求摘要；执行时不引用“最新版本”或可变草稿。
- `last_successful_revision_id` 仅由有可靠执行结果的任务更新；另存最后尝试 revision 与实时运行观察。
- 资源来自 Docker labels 和观察结果，不能只按容器名认领；发现外部同项目资源时阻止接管式写操作。
- Stack 删除只删除配置对象须另定义保留期；停止、down、删卷是不同动作，不隐式连带执行。

## 3. 任务状态与恢复语义

| 当前状态 | 可转向 | 触发条件 |
| --- | --- | --- |
| queued | running, canceled, failed, unknown | Agent 已开始；确认尚未执行并撤回；预检明确失败或未投递时离线；已投递但回执丢失无法确认开始情况 |
| running | succeeded, failed, unknown | 收到可信完成记录；收到明确失败；结果无法确认 |
| unknown | running, succeeded, failed, canceled | 依据重连核对证据恢复；无完成证据不能直接宣称成功、失败或取消 |
| succeeded / failed / canceled | 无自动迁移 | 重试创建新 task；不覆盖历史结果 |

`canceled` 仅表示确认未开始执行且撤回成功。首版 running 不支持强制取消，返回明确冲突；queued 与开始执行发生竞争时，只有 Agent 确认未开始且不会再开始，才可标为 canceled。
投递未获确认、连接丢失、Manager/Agent 重启均要核对任务阶段，不以 HTTP 超时当执行失败。
unknown 的人工关闭记录 audited_resolution、核对者和证据，保留 unknown。解除门禁必须有旧执行已停止且不能再次开始的证据，并在 Agent 持久标记该 task 禁止再执行；节点离线无法核对时不能仅凭管理员点击解除。不能凭人工关闭宣称成功、失败或已取消。
Manager 只能在原 task_id 下重传同一请求；Agent 已有该 ID 时返回原记录，不再次启动命令。
节点失去执行记录时禁止自动重放副作用；检查文件、进程记录与 Docker 资源后再由用户决定下一步。
任务进程结束、文件集摘要、revision / command 摘要和执行结果共同形成证据；单看健康容器不足以归因。
日志和结果上报可重传；Manager 按事件序号去重并确认游标，Agent 在确认前保留结果。
保留期、日志大小、节点并发数、心跳和超时值需要测量，不在初稿中假定数值。

## 4. Web REST 草案

统一 `/api/v1`，浏览器使用用户会话；表中资源写操作都要求已登录 admin。
读接口分页并返回观察时间；日志持续流先建议 SSE，重连按事件游标补齐。

| 方法与路径 | 输入 / 结果 | 关键约束 |
| --- | --- | --- |
| POST /auth/login；POST /auth/logout | 本地凭据 / 撤销会话 | OIDC-only 禁用本地登录；限速、CSRF 防护 |
| GET /auth/oidc/start；GET /auth/oidc/callback | 标准 OIDC 重定向 | state / nonce 与会话绑定；严格回调地址 |
| GET /me | 当前用户、登录模式 | 不返回认证密钥 |
| POST /auth/bootstrap | 一次性部署引导材料及首个管理员信息 | 仅未初始化实例；本地或预置 OIDC 身份分支；初始化与关闭入口原子完成 |
| GET /nodes | 节点及状态 | connection / engine / observed_at 分别返回 |
| POST /node-enrollments | 预绑定节点与有效期 | 一次性令牌只展示一次，审计不记令牌 |
| POST /nodes/{id}/revoke | 撤销节点凭据 | 立即禁止新任务；执行中任务需核对 |
| GET /nodes/{id}/resources | kind、游标 | 原生资源与受管归属分开展示 |
| POST /nodes/{id}/stacks | name、project_name、初始配置 | 显式确认项目名；同名冲突拒绝；不部署 |
| GET /stacks/{id} | 草稿版本、任务、观察摘要 | 不把旧缓存当实时结果 |
| PUT /stacks/{id}/draft | 完整 manifest + 正文；If-Match | 单事务更新草稿版本；原文不改写 |
| POST /stacks/{id}/revisions | draft_version | 固定不可变快照；可作为部署内部步骤，不承诺独立历史 UI |
| POST /stacks/{id}/validations | revision_id | 目标侧校验任务；记录能力、摘要及时间 |
| POST /stacks/{id}/deployments | draft_version 或 revision_id、预检引用 | 草稿先快照；返回 202 + task_id；执行前再核对 |
| POST /stacks/{id}/stop；POST /stacks/{id}/start | 已部署 revision_id | 分别停止或启动已有项目容器，不隐式把最新草稿部署；复用 Stack 任务门禁与幂等规则 |
| POST /stacks/{id}/down | 已部署 revision_id、影响确认 | 创建明确卸载任务；默认不删除卷、bind 数据或镜像；保留管理配置 |
| DELETE /stacks/{id} | mode=unmanage、影响确认、If-Match | 仅注销管理；禁止未决任务期间执行，不触碰运行资源；历史保留期另定 |
| POST /stacks/{id}/cross-previews | 来源 revision_id 或已保存 draft_version、目标节点及项目名 | 草稿先固定为快照，返回 preview_id、来源摘要、完整文件清单和目标依赖 |
| POST /stacks/{id}/cross-copies | source_revision_id 或 source_draft_version、target_node_id、target_name、target_project_name、selected_files、secret_choices | 草稿先快照；显式确认目标项目名和敏感项；事务创建独立目标 Stack / Draft；不部署 |
| GET /tasks/{id}；GET /tasks/{id}/events | 任务或事件游标 | 状态与运行健康独立 |
| POST /tasks/{id}/cancel | 取消原因 | 仅 queued；未投递可直接取消，已投递须 Agent 确认未开始且不能再开始；running 拒绝 |
| POST /tasks/{id}/resolve | 人工核对结论、证据、版本 | 仅 unknown；验证旧执行不能继续及 Agent 持久禁止再执行后，审计并解除门禁 |

部署示例：`POST /api/v1/stacks/st_1/deployments`，请求 `{"revision_id":"rev_7","validation_id":"val_3"}`。
请求头带 `Idempotency-Key` 与 Stack 的 `If-Match`；响应 `202 {"task_id":"task_8","state":"queued"}`。
部署前校验 revision 归属、节点身份与能力、最近校验摘要；预检失败明确返回可修正项。
按 draft_version 部署时先比对当前草稿版本并固定快照，预检与执行绑定同一摘要；版本不符拒绝，不自动选最新草稿。
预检不是端口或路径预留；最终执行时发生冲突仍须报告实际失败与部分变更。
跨节点复制对源 revision 做读取授权、目标做写入授权；同名冲突事务回滚，不能覆盖已有草稿。
从预览确认时必须提交 preview_id，复制只使用其固定来源快照与经确认的材料选择；预览后来源变化不混入目标。直接提交来源草稿版本时须比对版本，过期则拒绝。缺失敏感值或必需附件记录为目标草稿的待补项，不能用空字符串静默替代后继续部署。
正文下载按 Stack 授权；跨节点复制明确列出敏感文件且允许排除，不向 Agent 分发 Manager 密钥。

F08—F12 的容器启停、镜像与导出等首版补齐接口将在对应功能实施前补齐；均复用节点归属检查、用户鉴权、任务记录与敏感信息规则，不由前端直接调用 Engine。此表优先定义核心 Stack 流程，不宣称是完整公开 API。

所有修改请求建议带 `If-Match`，版本过期返回 412；缺少必需条件返回 428。
异步变更和跨节点创建要求幂等键，作用域为用户 + 操作路由，并保存正文摘要；同键同正文返回原结果，同键不同正文返回 409。
幂等记录保存期须覆盖任务保留与客户端重试窗口；幂等键不是鉴权凭据，也不能代替 Stack 锁。
并发编辑返回冲突与当前版本，不自动用最后写入覆盖；客户端保留用户未保存内容。
统一错误 `{"error":{"code":"STACK_BUSY","message":"该 Stack 有未决任务","request_id":"...","details":{}}}`。
建议状态码：400 结构错误、401 未登录、403 无权、404 不存在、409 名称/执行冲突、413 超限、422 配置无效或功能不支持、503 节点不可用。
结构化 code 另含 `REVISION_MISMATCH / UNSUPPORTED_FEATURE / NODE_OFFLINE / CONFIG_DRIFT / PATH_CONFLICT / RESULT_UNKNOWN`。
错误不回显密码、token、完整 env 或宿主机敏感内容；详细执行日志仍需鉴权与敏感处理。

## 5. Agent 协议草案

Agent 专用 `/agent/v1`，不接受浏览器会话认证；注册入口只接受预绑定一次性令牌。
注册交换建议为：校验 Manager TLS → Agent 生成私钥/CSR → 消费注册令牌 → Manager 签发节点证书 → WSS mTLS 连接。
节点身份取自服务端证书绑定，不信任消息自报 node_id；证书轮换、撤销和协议版本不兼容必须明确失败。
连接 hello 包含 `protocol_version, agent_version, node_id, agent_epoch, engine_version, compose_version, capabilities, last_ack_seq`。
服务端返回协议选择与任务核对列表；版本不兼容只上报诊断，不继续写操作。
任务信封至少包含：

```json
{
  "protocol_version": "draft-1", "message_id": "msg_1", "task_id": "task_8",
  "node_id": "node_2", "stack_id": "st_1", "action": "compose.deploy",
  "revision_id": "rev_7", "manifest_digest": "sha256:...", "request_digest": "sha256:...",
  "idempotency_key": "...", "expected_previous_revision_id": "rev_6",
  "work_dir_key": "stable-name", "project_name": "user-confirmed-name", "deadline_at": "...", "issued_at": "..."
}
```

工作目录由 Agent 根据受信映射解析，不接受远端任意绝对执行路径；支持动作是枚举，不接受 shell 命令字符串。
附件通过已认证 HTTPS 获取，逐项校验大小、路径与摘要；拒绝不匹配或未列入 manifest 的文件。
Agent 依次记录 `accepted / preparing / executing / completed` 阶段，阶段是诊断信息，不扩展统一任务状态枚举。
结果含 task_id、seq、agent_epoch、阶段、exit_code、文件/命令摘要、错误、开始/结束时间与观察摘要。
心跳只证明连接活动，不能代替任务完成；截止时间仅防止过期任务开始，不保证正在运行的命令已被终止。
重连核对先交换 task_id 与记录摘要，再补传未确认事件；Manager 无记录的任务不得凭消息直接执行。
部署命令、工作目录、环境与版本均记摘要；凭据、env 明文和展开后的秘密不进入协议诊断日志。

## 6. 路径、凭据与恢复边界

附件仅允许规范化相对路径；拒绝绝对路径、`..`、软链接越界、设备路径和与保留目录/运行数据重叠。
路径检查同时覆盖 Windows / Unix 表示与大小写冲突；写入时防符号链接替换竞态，不只检查字符串前缀。
Compose 中绝对 bind 路径是用户授权的宿主依赖，不等于允许把附件上传到该路径；二者权限分开。
Agent 无法检查 daemon 宿主绝对路径时返回预检 unknown，不宣称存在或可写；容器化 Agent 须验证同路径宿主映射。
文本附件下载不执行内容；归档若后续支持，必须独立实现解包边界，本稿不默认接受任意压缩包。
Manager 配置正文建议静态加密，主密钥从受限外部文件提供；备份需保存可恢复密钥但不混入公开导出。
导出默认提示并允许排除敏感配置；日志脱敏不能保证识别应用输出中的全部秘密，访问和保留期同样受控。
Agent 私钥与执行记录用系统权限保护；Docker socket 访问仍具有宿主高权限，安装页面应说明实际权限。
恢复 Manager 后保持调度暂停，先核对节点凭据、工作目录、revision 与未决任务，再恢复写操作。
配置备份恢复不恢复卷/bind 数据；节点状态目录丢失不能靠 Manager 任务表推定“从未执行”。
验收需覆盖重复提交、断线后已完成、命令中途崩溃、节点记录丢失、文件 drift、相对 bind 连续升级和跨节点同名冲突。
这些是后续验证场景，当前没有已通过测试的实现；认证依据与 Docker 官方链接见 [架构初稿](architecture.md)。
