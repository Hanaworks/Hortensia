# M1 HTTP 接口工作规格

版本：`http-v1-draft.1`，2026-11-04。本文承接 [接口总览](../api-design.md) 的 Web—Manager 和节点认领边界，以及 R01—R07、R11／R16／R17。用于三端实现与契约检查；没有可调用服务，未通过联网或故障验证。本文中的精确定义是本轮技术工作选择，不代表产品全部功能或所有平台已实现。

## 1. 定义归属与编码

原接口篇第 6 节保留机制摘要，本篇维护 HTTP 路由及对象的准确结构。[Agent 协议](agent-protocol.md)维护跨进程消息及共享配置、观察和执行事实；共享类型的字段形状由 [JSON Schema](../../contracts/agent-v1.schema.json) 的 `$defs` 定义。HTTP 不直接返回数据库行，也不要求浏览器理解 Agent 的内部账本。

下文对象使用 `名称{字段:类型}` 表示完整字段集合：未写 `?` 的键必须存在；`T|null` 表示键存在且允许空值；`字段?:T` 表示可省略；`T[]` 表示数组。没有注明开放映射的对象均拒绝额外字段。请求不自动把字符串转成数字／布尔值，重复 JSON 键、无效 UTF-8、尾随内容均返回 `400 invalid_json`。响应也必须满足本篇形状，客户端发现未知字段不得把它当作已支持的新能力。

| 类型 | 精确定义与含义 |
| --- | --- |
| Id | Schema `Id`：小写 UUIDv4；对象身份不由显示名、主机名或数组位置代替 |
| Timestamp | Schema `Timestamp`：UTC RFC3339；未知时显式 null；不把“尚未观察”编码成零时刻 |
| Revision | Schema `Revision`：非负 JSON 安全整数；只在同一对象／同一来源范围比较，不作为跨机器时间 |
| Name | 1—128 个 Unicode 码点；禁止 NUL、CR、LF；保留原文，不自动 trim 后改变身份判断 |
| Username | 3—64 个 ASCII 字符，正则 `^[a-z0-9][a-z0-9._-]{2,63}$`；区分于可编辑显示名；M1 不开放改名 |
| Password | 12—128 个 Unicode 码点的非空 UTF-8 字符串；不截断、不 trim、不做 Unicode 归一化；只用于设置和验证，绝不返回 |
| SecretToken | 32 个随机字节的无填充 Base64url，43 个字符；用于节点凭据、认领、会话 CSRF；不用于 UUID 身份 |
| Cursor | 不透明字符串，1—1024 个字符；缺省表示第一页，响应 null 表示结束；客户端不可自行拼接 |
| Config | Schema `Config`；文件原始字节、顺序和插值输入构成一个不可变版本；整个结构均受 `config.read` 保护 |
| Outcome／InterruptReason | Outcome 为 `not_started|normal_exit|interrupted|unknown`；InterruptReason 为 `canceled|deadline|agent_stopping|signal`。互斥、空值和退出码条件以 Schema ExecutionResult 为准 |
| Error／Observation／ExecutionProfile | 引用 Schema 同名类型；HTTP 只使用明确允许的摘要字段，不公开协议摘要或执行身份 |
| NodeCapability | 精确的动作字符串：`node.read`、`config.read`、`config.write`、`stack.deploy`、`operation.read`、`operation.output.read`、`operation.cancel` |

`Content-Type` 为 `application/json`，不带 BOM。所有路径 ID 必须符合 Id；请求体为空的操作不接受 `{}` 以外正文（无正文也可）。全部秘密响应设置 `Cache-Control: no-store`；M1 为简化缓存策略，所有 `/api/v1` 和 `/agent/v1` 响应均使用 no-store。禁止通过 URL 传入口令或令牌。

## 2. 会话、授权和公共失败

沿用接口篇的同源 HTTPS、`__Host-dm_session` Cookie 和 `X-CSRF-Token` 约定。所有浏览器变更校验精确 Origin；除登录外同时校验 CSRF。Cookie、账号启用状态和当前授权必须在每次请求复核。管理员具有账号／节点登记和授权的管理能力，但不因此拥有配置、部署、资源观察和输出权限。

`ErrorResponse{error:HttpError}`，其中 `HttpError{category:string,code:string,message:string,request_id:Id,field_errors:FieldError[],retry_action:RetryAction}`；`FieldError{path:string,code:string,message:string}`。path 为 JSON Pointer，整体请求错误用空字符串。message 仅用于人类显示，不作为程序判定依据，不回显秘密、原配置或数据库错误。RetryAction 为 `none|same_request|query_operation|refresh_and_review`。

| HTTP／code | category | 适用情况与调用者动作 |
| --- | --- | --- |
| 400 `invalid_json`／`invalid_field` | invalid_input | 编码、字段类型或约束不合法；修正输入，none |
| 401 `unauthenticated`／`invalid_credentials` | permission_denied | 无有效会话／认证失败；登录失败不区分账号存在性，none |
| 403 `origin_rejected`／`csrf_rejected`／`permission_denied` | permission_denied | 来源、CSRF 或可见节点上的动作权限不足；none |
| 404 `not_found` | not_found | 对象不存在或调用者不可见；清除该上下文缓存，none |
| 409 `idempotency_conflict` | conflict | 同 key 不同输入；不可重用该 key，refresh_and_review |
| 409 `name_conflict`／`resource_busy`／`recovery_required` | conflict | 同名、资源占用、节点隔离；核对对象，refresh_and_review |
| 409 `enrollment_active`／`node_already_enrolled` | conflict | 认领材料或身份不满足入口条件，refresh_and_review |
| 410 `cursor_expired` | invalid_input | 游标超时；重新查询第一页，refresh_and_review |
| 412 `version_conflict` | conflict | If-Match 过期；保留编辑内容并重新读取，refresh_and_review |
| 413 `payload_too_large` | invalid_input | 超过已声明预算；不截断受理，none |
| 422 `unsupported` | unsupported | 已理解输入，但当前执行能力不支持，none |
| 428 `precondition_required` | invalid_input | 缺少必要 If-Match；读取对象后提交，refresh_and_review |
| 429 `rate_limited` | unavailable | 附整数秒 Retry-After；到期以相同输入／key 重试，same_request |
| 503 `node_unavailable`／`storage_busy`／`storage_full` | unavailable | 当前无法受理；变更先查原 key 回执，same_request |
| 500 `internal_error` | unavailable | 不泄漏异常正文；变更结果不确定时查原 key，same_request |

同一种 HTTP 状态可以有多种 code，其他未列 HTTP code 不自行加入 v1。Agent 执行错误使用协议中的 Error，并通过操作快照返回；已经受理的执行失败不是查询请求的 HTTP 500。普通查询的网络失败不发起任何变更。

## 3. 条件更新、幂等与分页

所有表中标记 K 的变更必须有 `Idempotency-Key: <Id>`，同一用户跨这些路由共用 key 空间。先查询 key，再检查 If-Match；同 key、相同方法／规范路径／If-Match／正文返回同一逻辑结果，不重新生成身份或重做变更。并发同 key 通过唯一约束合并；未提交的处理不产生回执。原 key 请求已受理但客户端丢响应，先查询 submissions，再决定同 key 重发。401、403 和对象不可见优先保护信息，旧回执不绕过当前读取权限。

业务请求摘要沿原接口篇 HMAC 规范。密码等敏感输入只保留带密钥摘要，不进入幂等表明文。受理事务同时保存不可变的安全响应正文、HTTP 状态及 ETag：例如新建 Stack 的原 stack_id/version_id、修改 User／Grant 的原 auth_rev 及当时响应字段。重放先检查当前权限，仍获准才返回原响应；不得查询当前可变对象重新拼出不同 ETag 或版本。安全响应仅允许本篇 K 路由定义的非秘密字段，禁止口令、配置、CSRF、长期秘密和认领 token。涉及秘密的一次展示采用第 5 节的明确例外，不承诺重放秘密正文。

标记 U 的用户／Grant 更新使用 `If-Match: "<auth_rev>"`；标记 V 的配置保存使用 `If-Match: "<current_version_id>"`，均拒绝弱 ETag 和 `*`。ETag 只用于对应对象的条件写入，不跨账号或 Stack 比较。GET user、GET me、PATCH user 的 ETag 为 auth_rev；Grant 行自带 auth_rev，GET grants 集合不发送一个代表所有用户的 ETag。

列表的公共 query 为 `limit?:integer`（默认 50，1—200）、`cursor?:Cursor`；operations 另要求 `node_id:Id`。未知 query、重复 query 键均 400。返回 `Page<T>{items:T[],next_cursor:Cursor|null}`。M1 不提供 total 和 offset。默认按不可变 `(created_at,id)` 升序作 keyset 查询，Grant 列表按对应 user 的该二元组排序。游标编码/签名由服务端私有实现，但必须绑定路由、筛选、用户、auth_rev、上次排序键和 15 分钟到期时刻；篡改／换范围 400，授权变化 403 或不可见 404，过期 410。每页重查权限。普通列表不是全局时间点快照；新数据可能出现在后续页，不以翻页证明恢复完整。恢复扫描采用 Agent 协议的固定集合。

## 4. 准确返回对象

| 对象 | 字段及约束 |
| --- | --- |
| User | `{user_id:Id,username:Username,enabled:boolean,auth_rev:Revision}`；不含密码摘要 |
| Me | `{user_id:Id,username:Username,auth_rev:Revision,system_capabilities:string[],csrf_token:SecretToken,expires_at:Timestamp}`；system_capabilities 只能为空或 `["system.admin"]` |
| Node | `{node_id:Id,display_name:Name,status:NodeStatus,capabilities:NodeCapability[],agent_capabilities:string[],last_seen_at:Timestamp|null,execution_profile:ExecutionProfile|null,observation:Observation|null}`；capabilities 是当前调用者的显式 Grant，agent_capabilities 是 Agent 协议能力，二者不可混用；observation 为最近节点容器观察，采集时刻不能替代 last_seen_at |
| Grant | `{user_id:Id,capabilities:NodeCapability[],auth_rev:Revision}`；能力去重并按字典序返回 |
| Stack | `{stack_id:Id,node_id:Id,display_name:Name,project_name:string,current_version_id:Id,revision:Revision,last_applied_version_id:Id|null,last_operation_id:Id|null,observation:Observation|null}`；observation 使用 Schema 同名类型；未观察为 null |
| Version | `{version_id:Id,config:Config}`；仅返回指定保存版本，和当前部署版本没有隐含等价关系 |
| Operation | `{operation_id:Id,node_id:Id,stack_id:Id,version_id:Id,operation_type:"stack.apply",revision:Revision,state:OperationState,cancel_requested:boolean,created_at:Timestamp,updated_at:Timestamp,result:OperationResult|null,release_status:"held"或"pending"或"released"}` |
| OperationResult | `{outcome:Outcome,ownership_status:OwnershipStatus,interrupt_reason:InterruptReason|null,exit_code:integer|null,error:Error|null,observation:Observation|null,not_started:boolean,output_truncated:boolean,finished_at:Timestamp}`；OwnershipStatus 为 Schema 同名枚举，其他取值及组合条件与协议 ExecutionResult 相同。只取这里列出的字段，不公开 input_digest、Agent event_seq、phase 或原始输出；后续 Agent 增加字段也不会自动扩展 HTTP 返回对象 |
| Receipt | `{result_type:"user"或"node"或"grant"或"enrollment"或"stack"或"version"或"operation",result_id:Id,response_status:integer}`；Grant 的 result_id 为被更新 user_id，路径中的 node_id 仍由客户端原提交上下文保存 |

NodeStatus 为 `pending|online|offline|reconciling|revoked`：无认领身份为 pending；已撤销优先为 revoked；存在恢复隔离优先为 reconciling；其余按当前已认证且 hello/welcome 完成的通道分 online/offline。online 仅表示通道，Docker 能力或业务健康由能力及观察说明。last_seen_at 记录最近收到有效当前会话帧的 Manager 时间；没有有效接入为 null。历史能力在离线时可保留，不能据此授权部署。

Node 的管理元信息可以由 system.admin 读取，以完成节点认领与授权；这不开放 Stack、观察、配置或执行输出。普通用户必须有 node.read 才能读取 Node。管理员没有 node.read 时返回 capabilities 空数组且 agent_capabilities 空数组、execution_profile 和 observation 均为 null；只能看到 node_id/display_name/status/last_seen_at，其余保留键并按上述方式脱敏。节点和 Stack 查询使用已保存观察，不等待实时 CLI；尚未采集为 null，采集失败保留 error 状态或上一份带原采集时间的观察，不能刷新时间冒充新结果。

Stack project_name 必须匹配 `^[a-z0-9][a-z0-9_-]{0,62}$`，长度 1—63；这是 M1 输入预算，不代表 Compose 的全部命名能力。新建请求只指定 project_name，初始 display_name 与之相同；M1 暂无重命名 API。last_applied_version_id 仅当 `outcome=normal_exit`、`exit_code=0`、`observation.status=complete` 且 `ownership_status=consistent` 时更新；它记录一次已确认成功输入，不声称实际资源之后没有被外部修改。失败或未知操作保留原成功引用，last_operation_id 仍指最新受理操作。

OperationState 使用 Schema 枚举和数据篇的含义。result 为 null 表示尚无可用执行事实。held 表示未取得终态依据；pending 表示已持久终态但双方占用释放握手尚未完成；released 表示 Manager 已持久收到释放确认。界面不能仅凭终态推定目标已可接受新变更；所有变更仍由 Manager 核查占用。

## 5. 路由全集与成功响应

权限缩写：S=system.admin；N=node.read；C=config.read；W=config.write；D=stack.deploy；O=operation.read；X=operation.output.read；Z=operation.cancel。每个节点动作均限于请求目标节点，系统管理权限不替代动作权限。除 sessions 和 Agent HTTP 外，均需要用户会话。

| 方法与路径 | 权限／条件 | 完整请求体 | 成功状态／正文 |
| --- | --- | --- | --- |
| POST /api/v1/sessions | 同源 Origin、JSON | `{username:Username,password:Password}` | 200 Me，设置 Cookie；失败 401 |
| DELETE /api/v1/sessions/current | 同源 Origin；有效会话时检查 CSRF | 无 | 204 无正文并过期 Cookie；已无会话重复退出也 204 |
| GET /api/v1/me | 会话 | 无 | 200 Me、ETag |
| POST /api/v1/users | S、K | `{username:Username,password:Password}` | 201 `{user_id:Id}`；新用户 enabled=true、auth_rev=0、无 Grant、不自动成为管理员 |
| GET /api/v1/users | S、分页 | 无 | 200 Page<User> |
| GET /api/v1/users/{user_id} | S | 无 | 200 User、ETag；供条件管理读取 |
| PATCH /api/v1/users/{user_id} | S、K、U | `{enabled:boolean}` 或 `{password:Password}`，恰选一个 | 200 User、ETag；变更递增 auth_rev 并失效旧会话；M1 不提供增删系统管理员的 API |
| POST /api/v1/nodes | S、K | `{display_name:Name}` | 201 `{node_id:Id,status:"pending"}`；尚无 Agent 身份 |
| GET /api/v1/nodes | N 或 S、分页 | 无 | 200 Page<Node>，按第 4 节裁剪管理员元信息 |
| GET /api/v1/nodes/{node_id} | N 或 S | 无 | 200 Node |
| GET /api/v1/nodes/{node_id}/grants | S、分页 | 无 | 200 Page<Grant>；每个现存用户有一项，无授权时空数组 |
| PUT /api/v1/nodes/{node_id}/grants/{user_id} | S、K、U | `{capabilities:NodeCapability[]}` | 200 Grant、对应用户 auth_rev ETag；空数组撤销；重复能力400；同 key 重放不再次递增 |
| POST /api/v1/nodes/{node_id}/enrollment-tokens | S、K | `{replace_pending?:boolean}`，缺省 false | 201 `{enrollment_id:Id,status:"pending",expires_at:Timestamp,token:SecretToken|null}`；一次展示规则见下 |
| POST /api/v1/nodes/{node_id}/revoke | S、K | 无 | 200 `{node_id:Id,status:"revoked"}`；已有有效许可和执行副作用仍需核对 |
| GET /api/v1/nodes/{node_id}/stacks | N、分页 | 无 | 200 Page<Stack> |
| POST /api/v1/nodes/{node_id}/stacks | N、C、W、K | `{project_name:string,config:Config}` | 201 `{stack_id:Id,version_id:Id}`，ETag 为新 version_id；仅保存 |
| GET /api/v1/stacks/{stack_id} | N | 无 | 200 Stack、当前 version_id ETag |
| GET /api/v1/stacks/{stack_id}/versions/{version_id} | N、C | 无 | 200 Version；须属于同 Stack，返回该 version_id ETag |
| POST /api/v1/stacks/{stack_id}/versions | N、C、W、K、V | `{config:Config}` | 201 `{version_id:Id}`，新 ETag |
| POST /api/v1/stacks/{stack_id}/apply | N、C、D、K | `{version_id:Id}` | 202 `{operation_id:Id,state:"accepted"}`，Location 为该操作路径 |
| GET /api/v1/operations/{operation_id} | N、O | 无 | 200 Operation |
| GET /api/v1/operations | N、O、node_id、分页 | 无 | 200 Page<Operation> |
| GET /api/v1/submissions/{idempotency_key} | 当前用户及目标当前读取权限 | 无 | 200 Receipt；不存在404不证明在途请求绝不可能提交 |
| POST /api/v1/operations/{operation_id}/cancel | N、O、Z | 无 | 200 Operation；天然按 operation_id 幂等，无新业务 key |
| GET /api/v1/operations/{operation_id}/output | N、O、X、after/limit | 无 | 200 OutputPage，规则见第 6 节 |

新增的 GET user 用于读取完整条件更新依据；认领入口的 replace_pending 使“显式重新签发”成为可调用行为。已有待消费 token 时，false 返回409；true 在同一事务中废止旧 token 并创建新 token。已认领节点仍拒绝，不允许用该参数替换执行身份。token 有效期固定为生成后 10 分钟；生成事务提交后只在首次响应内返回明文，同 key 重试返回相同 enrollment_id/expires_at，但 token=null，status 按当前记录为 pending/consumed/expired/revoked。因此该响应 status 实际类型为这四个值；首次创建必为 pending。token 丢失时用户用新 key 和 replace_pending=true 明确生成新材料。

K 的其他响应丢失由 Receipt 找回结果身份；节点撤销 Receipt.result_type=node，用户更新为 user，Grant 更新为 grant。所有管理变更均与审计原子提交。没有秘密回读接口。M1 的第一个 system.admin 由本机初始化流程建立；防止禁用最后一个启用管理员，返回409 `last_admin`（category=conflict，retry_action=refresh_and_review）。

submissions 仅查询当前提交用户自己的 key；其当前目标权限按下表检查，不能凭猜中 key 读取别人的回执。相同规则也用于原请求的幂等重放读取检查；变更尚未受理时仍需完整写权限。

| result_type | 读取原回执所需当前权限 |
| --- | --- |
| user／grant／enrollment | S；回执及重放永不包含原认领 token 或密码 |
| node | S；只返回管理变更的非秘密回执 |
| stack | N；原新建的 stack_id/version_id 不含配置正文 |
| version | N、C；不需要再次拥有 W 才可知道原保存结果 |
| operation | N，且满足 O 或（C 与 D）；只回原 operation_id／受理状态，读取操作详情仍必须 O |

具有部署权但没有 O 的用户可以提交并取得受理回执；Web 显示“已受理，无结果查看权限”，不循环请求必定403的操作详情。撤掉 N 后配置／操作回执404，不泄漏目标存在性。原请求已受理后的重放不会再次检查写权限后执行，而是先按上述读取规则返回安全回执；两者不可混成第二次授权执行。

保存只进行包结构、路径及大小检查，Compose 语法和当前部署支持范围的诊断不改变已保存原文；部署前才必须通过执行预检。Apply 固定指定版本，即使用户随后保存新版本也不改变它。离线／隔离节点的新增 Apply 拒绝503／409，不保留未来自动启动的排队意图。已经受理的任务失联按恢复规则处理。

## 6. 输出读取及前端连续保存

`OutputPage{text:string,next_cursor:string,truncated:boolean}`。after 缺省为十进制字符串 `"0"`，否则必须为非负十进制字节偏移；limit 缺省 65536，范围 1—262144 字节。偏移相对于本操作已保存的输出字节流，最大 1 MiB。Agent 输出的无效 UTF-8 先按 Unicode 替换规则转成合法 UTF-8，随后执行采集预算并固定字节流；此规范必须同时用于 Agent 输出及 HTTP 切块，不能两端分别替换后比较偏移。

服务端选择不超过 limit 的完整 UTF-8 码点结尾，不拆一个码点。若 limit 小于下一完整码点长度，400 invalid_field；after 落在码点中间或超过已保存长度也400。next_cursor 为已返回片段之后的十进制偏移，暂时无新数据时仍返回同值且 text 为空；truncated 仅表示采集预算截断，不代表本页还有后续。M1 在执行结果可用后提供输出，执行中可返回空文本／0／false，不伪装成实时日志流。客户端结合操作状态停止或继续查询，绝不通过输出接口再次启动命令。

编辑器一次只允许一个保存请求在途。保存过程中允许继续编辑，但第二次“保存”暂不可提交，界面明确显示正在保存；成功后用新 version_id 作为下一次基准，保留请求快照之后的本地编辑。超时先核对原 key，不能以新 key 保存后续内容覆盖未知结果；确认前保留草稿。冲突需读取最新版本并让用户处理，不能后台静默合并。此规则补足 Web 文档原有 saving 状态，不强制编辑器内部组件结构。

## 7. Agent 的 HTTP 引导

这些路由不用浏览器 Cookie、Origin 或 CSRF，拒绝带 Origin 的浏览器请求；只接受 HTTPS JSON 和 Authorization Bearer。凭据只通过受保护请求头／正文传送。

| 方法与路径 | Authorization | 请求 | 成功结果 |
| --- | --- | --- | --- |
| POST /agent/v1/enroll | 一次性 token | `{node_id:Id,ledger_id:Id,credential_secret:SecretToken}` | 200 `{node_id:Id,ledger_id:Id,credential_generation:Revision}`；首次为1 |
| POST /agent/v1/credentials/rotate | 当前 node_secret | `{ledger_id:Id,credential_secret:SecretToken}` | 200 `{node_id:Id,ledger_id:Id,credential_generation:Revision}`，代次递增且旧连接失效 |

认领先在 Agent 本机耐久保存 ledger_id 和候选秘密，再发送。消费 token、绑定身份和存储凭据摘要原子完成。消费后的相同 token／同目标／同账本／同候选请求返回同一绑定，原记录须保留；不同候选或不同绑定401，不显示合法绑定信息。失效且未消费的 token 返回401。响应丢失优先用已保存候选建立 WebSocket 确认身份，不能另造节点。

轮换提交后旧凭据立即失效；丢响应时用已保存候选连接确认。仅候选明确401而旧凭据仍有效时可用旧凭据重发相同候选；两者均401进入隔离维护，不新建账本。重复旧秘密的轮换请求若已失效将401，这是确定的认证结果而不是换第三个秘密的理由。两路由复用 ErrorResponse，request_id 由 Manager 生成；限速429，存储故障503，输入无效400，身份或凭据不符401。

## 8. 示例、验收与适用依据

一个最小配置请求如下（演示值，不连接任何节点）：

```json
{"config":{"files":[{"path":"compose.yaml","content_b64":"c2VydmljZXM6IHt9Cg=="}],"compose_files":["compose.yaml"],"interpolation_env_files":[],"interpolation_env":{},"profiles":[]}}
```

该输入可用于包保存结构测试；其中 services 为空不满足本期部署要求，应在 Apply 预检中失败，不能把“可保存”当成“可执行”。Agent 协议的合法／非法报文集覆盖共享配置及执行结构；HTTP 实现还须对本篇逐路由完成请求／响应断言，包括：遗漏必需键、null 与缺省、空能力数组、错误 If-Match、同 key 异输入、游标跨用户、秘密一次展示、UTF-8 输出边界及连续保存。

本篇沿 [arc42 构件视图](https://docs.arc42.org/section-5/)展开对接边界，字段取舍参考 [NASA Software Data Dictionary](https://swehb.nasa.gov/spaces/SWEHBVD/pages/140640601/SDD%2B-%2BSoftware%2BData%2BDictionary) 的含义／表示／范围／空值要求；参考 [Helichrysum Manifest](D:/Project/Helichrysum/docs/manifest-schema.md) 与 [插件 API](D:/Project/Helichrysum/docs/plugin-api.md) 的定义、责任和状态分离写法。其对象、值及实现状态不作为本项目事实。HTTP 条件写入与错误机制沿原接口篇引用的 RFC 9110／6585，不在本轮另造传输协议。
