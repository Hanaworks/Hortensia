# Docker Manager 接口规格

更新日期：2026-11-04。状态：逻辑契约与 M1 物理接口提案，尚无可调用服务。面向 Web、Manager 和 Agent 的接口实现者，承接 [arc42 构件接口、运行交互及共同概念](architecture.md)。第 1—5 节定义整体操作语义，第 6 节细化首个完整流程；具体技术机制仍需原型验证，O03、O07 尚未关闭。

## 1. 接口所属构件与调用关系

| 边界 | 提供者／使用者 | 能力与职责 |
| --- | --- | --- |
| 用户管理 | Manager／Web | 登录、账号、授权、节点组织；所有管理请求绑定有效身份 |
| 配置和操作 | Manager／Web | 查询、条件保存、Copy、Apply、操作核对与取消、迁移准备验证清理 |
| 节点接入 | Manager／Agent | Agent 主动认证连接，注册、能力与会话、任务及结果恢复 |
| 节点执行 | Agent／Manager 经既有通道 | 原生校验、结构化资源动作、任务执行和带时间观察 |
| 诊断流 | Agent 经 Manager／获准 Web | 绑定用户、节点、对象的日志、统计和控制台会话 |

Manager 内部操作协调和配置管理的责任见[模块设计](components/manager.md)，Agent 的输入与执行边界见[Agent 设计](components/agent.md)。Web 不持有节点执行身份，不直接请求 Docker 或 Agent。

## 2. 共同请求和结果

查询输入明确 node_id 及资源标识，返回观察时间和错误，不用资源名称做唯一定位。变更另带提交标识、固定版本或输入、操作类型及确认范围。用户身份由认证通道取得，不信任请求正文自行声称的用户；Agent 身份同样绑定节点。

响应分别表示同步成功、已受理操作和拒绝。长任务受理返回 operation_id，不能用空成功响应代表执行完成。错误包含类别、相关对象／字段、可理解原因和关联标识；“可重试”若提供，只能说明适用方式，不能一律建议用新操作重做未知变更。

| 错误类别 | 语义与调用者处理 |
| --- | --- |
| invalid_input | 输入缺失或范围不合法，指出需修改处，无执行副作用 |
| permission_denied | 节点／动作／敏感读取未获准，不返回受限正文 |
| not_found | 在可见范围内无法定位；不能泄漏未授权对象存在性 |
| conflict | 版本、名称、占用或对象变化，调用者核对后明确处理 |
| unsupported | 当前能力、配置引用、构建或版本组合不支持，不静默降级 |
| unavailable | 节点或依赖当前不可用，说明未能确认的范围 |
| execution_failed | 已有确定执行失败证据，可能包含部分副作用 |
| reconciliation_required | 结果不能可信确定，应查询和核对，不能盲重放 |

逻辑字段及状态的主定义见[数据设计](data-design.md)。下面操作名为沟通标识；M1 对应的建议路由见第 6 节，后续功能没有因此取得可调用接口。

## 3. Web—Manager 主要操作

### 3.1 QueryResources 和 QueryOperation

QueryResources 输入节点、资源范围及筛选，前提是具备相应读取权限，返回带节点归属及采集时刻的结果。离线时可以返回明确标识的历史观察，不能伪装成当前空列表。敏感配置和诊断读取独立检查。

QueryOperation 输入 operation_id 或提交标识，返回调用者有权读取的操作快照、结果层次及事件位置。它不重发任务。提交超时后 Web 用原提交标识核对，未找到不等于所有网络竞态下绝对未受理，具体查询／去重窗口由物理契约约束。

### 3.2 SaveConfig

输入 Stack／目标、基准版本、文件集合与顺序、环境及项目输入。前提为目标配置维护权限、包路径有效和基准匹配。输出新不可变版本和诊断。更新当前版本与发布内容一致提交；版本冲突不修改现有版本，也不丢弃调用者草稿。新建与修改均不部署；未通过部署校验的配置可按清楚诊断保存，但不能因此得到“可运行”结论。

### 3.3 CopyConfig

输入来源 Stack 与固定版本、目标节点／名称、修改和提交标识。分别检查来源读取及目标配置维护权限，检查包完整性及目标冲突。输出独立目标 Stack、版本和复制结果。相同标识同输入返回同一目标，不再次创建；来源离线但配置完整可复制，来源运行资源不变。Copy 不包含实际 Apply，也不默认复制仓库或其他秘密凭据。

### 3.4 ApplyConfig 和 ResourceAction

Apply 输入目标、固定版本、项目参数、提交标识；先检查目标部署权限、节点可用性、冲突和能力，再持久受理。返回 operation_id。受理后授权变化、配置校验或节点失败记录在操作中；不得仅凭请求成功声称应用可用。

ResourceAction 使用明确动作与目标对象集合，例如启动、停止、镜像拉取或资源删除；参数按动作白名单定义，不接受任意 CLI 字符串。删除另带确认范围，涉及数据再检查相应权限。执行结果含实际影响对象及失败，不把同名资源当作原对象。

### 3.5 CancelOperation

输入 operation_id，检查取消权限和当前阶段。未开始时可确认取消；已开始时仅记录和转交取消请求。响应说明是否只是请求，最终结果通过 QueryOperation 核对。重复取消不产生另一个底层变更，不承诺回滚已经执行的 Docker 操作。

### 3.6 PrepareMigration 和 ExecuteMigration

Prepare 输入来源、目标、版本和意图，检查两端权限、依赖和目标冲突，返回迁移记录及尚未满足的条件。准备不是迁移完成，也不自动停止来源。

Execute 仅在 O05 已确定并批准具体策略后开放：引用准备记录、策略、明确确认和数据准备信息，返回阶段操作引用。策略失效、对象变化或授权不足拒绝开始。完整切换和失败状态仍需补齐；此接口当前不能用于实现一个虚假的“Copy 后改归属”迁移。

### 3.7 VerifyMigration 和 CleanupSource

Verify 引用迁移、约定检查项及必要业务确认，返回逐项结果和证据。证据不全、验证失败或未知不会产生通过结论。机器观察与用户声明分开记录，不能把声明改写为机器已验证事实。

Cleanup 输入迁移、验证引用、来源实际对象集合、选择的删除范围与提交标识；开始前重新核对验证有效性、目标接替状态、来源对象未被替换及删除权限。未通过或失效拒绝，不向 Agent 发删除任务。通过后受理独立清理操作，失败单独记录；保留来源无需调用 Cleanup。持久数据清理的具体策略继续受 O05／O06 约束。

### 3.8 ConvertRun 和 OpenDiagnostic

ConvertRun 输入命令文本及声明语法，输出可编辑配置、映射、未支持项、语义差异和外部依赖。无节点副作用，不读取服务环境猜变量、不执行 shell。带入编辑器后另行 Save／Apply。

OpenDiagnostic 输入节点、资源、流类型及受支持参数，检查类型对应权限并返回绑定上下文的会话。关闭、撤权、目标变化或连接终止释放会话；恢复新会话重新授权。控制台使用结构化命令参数，不把它扩大成 Agent 任意系统 shell。

## 4. Manager—Agent 操作交接

| 操作 | 输入／前提 | 状态变化、结果与重复处理 |
| --- | --- | --- |
| Register／Connect | 接入身份、管理端信任、Agent 版本与能力；Agent 主动连接 | 绑定 node_id 与会话代次；失败不取得执行权；M1 认领及重装边界见 6.7，O03 跟踪验证 |
| Observe | 明确资源范围、当前认证身份 | 返回节点、时间、资源或采集失败；不改变运行资源 |
| PrepareTask | operation_id、目标、固定输入、摘要、所需能力 | Agent 先登记去重再准备工作区；同标识同摘要返回已有状态，不重复执行 |
| RequestStart／AuthorizeStart | 准备事实、操作及会话关联 | Manager 重新检查权限和占用；Agent 持久许可与开始事实后才执行，M1 格式与竞争边界见 6.5—6.6，O03 跟踪验证 |
| ReportResult／Acknowledge | 操作、摘要、阶段、结果和观察证据 | Manager 校验关联后持久保存再确认；重复报告可去重，确认丢失则重报而不重执行 |
| Reconcile | 双方未终结任务和账本摘要、身份和当前观察 | 匹配后恢复已知结果；矛盾或缺失保留未知，不创建新操作绕过核对 |
| Close／Revoke | 会话或节点身份引用及撤销原因 | 停止后续任务和流；已执行副作用不因撤销自动消失 |

传输层重试与业务重试分开：网络帧重发必须关联同一操作；重做一次业务变更需要新明确意图，未知结果未解除前不能借新标识绕过冲突。

## 5. 事件、并发与协议落地

操作事件至少带操作标识、位置和阶段；资源快照带节点和采集时间。订阅断点无法续接时退回快照核对，不能假设客户端见到所有事件。授权变化后不再向旧订阅发送受限内容。流量、输出大小和保留窗口需要有界，但阈值随 O08／O11 确定。

M1 的路由、序列化、身份、会话、幂等、限制与开始边界由第 6 节承接；浏览器先轮询操作快照，事件续传和诊断流属于后续增量。重复、迟到、断线和撤权原型尚未运行，不能把物理接口提案当作已证明的执行保证。

本篇沿 [arc42 构件](https://docs.arc42.org/section-5/)、[运行](https://docs.arc42.org/section-6/)和[共同概念](https://docs.arc42.org/section-8/)展开模块的公开操作；生命周期与重复调用表达参考 [OpenTelemetry End](https://opentelemetry.io/docs/specs/otel/trace/api/#end)。接口契约验证对应 T03—T10、T13—T15、T17，见[测试计划](test-plan.md)。

## 6. M1 物理接口提案

### 6.1 编码、限制与权限

Web 与 Manager 使用同源 HTTPS、`/api/v1`、UTF-8 JSON；Agent 使用独立认证的 `/agent/v1`。ID 为小写 UUIDv4 字符串，时间为 UTC RFC3339 字符串，变更版本 `revision` 为非负安全整数。请求拒绝未知字段、重复 JSON 键及不符合类型的值；空值与缺省不混用。配置原文通过标准带填充 Base64 编码，解码后须为有效 UTF-8，不改换行、空格或 Unicode 字符。

M1 建议上限为 128 个文件、单文件 1 MiB、全部原文件合计 4 MiB，HTTP 请求体和 WebSocket 重组后的单条消息各 8 MiB；Base64 和 JSON 开销计入消息上限。接受配置前还需试算冻结执行消息的编码大小，超限拒绝，不能截断或等到执行才发现无法投递。路径为包内 POSIX 相对路径，最多 240 个 UTF-8 字节；显示名 1—128 字符；分页 `limit` 默认 50、最大 200。上限是待验证的实现提案，不是用户已定的容量要求。

| 权限 | M1 授权范围 |
| --- | --- |
| `system.admin` | 账号、节点登记、认领材料和授权管理；不隐式授予节点资源操作，可显式给自己授予节点权限 |
| `node.read` | 指定节点的元信息、能力、资源摘要；不包括配置原文和命令输出 |
| `config.read` | 指定节点 Stack 的配置原文及历史版本，包含其中的敏感值 |
| `config.write` | 新建和条件保存该节点配置；保存需要同时具备 `node.read`、`config.read` |
| `stack.deploy` | 部署该节点固定版本；同时需要 `node.read`、`config.read` |
| `operation.read` | 该节点操作的阶段、错误类别和资源观察摘要；同时需要 `node.read` |
| `operation.output.read` | 该节点命令输出，可能含应用输出的秘密；同时需要操作读取权限 |
| `operation.cancel` | 请求取消该节点操作；同时需要操作读取权限 |

权限表由 Manager 在每个请求和开始许可时检查，前端能力摘要只辅助显示。节点被移出可见范围返回 404；节点可见但缺所需动作返回 403。M1 不引入组权限继承。后续 Copy、删除、控制台和迁移另加能力，不借用 `stack.deploy` 扩权。

### 6.2 用户会话与管理入口

使用本地账号、服务端会话；登录成功生成 32 字节随机会话秘密，数据库只存其摘要，Cookie 为 `__Host-dm_session`、`Secure; HttpOnly; SameSite=Strict; Path=/`，不设置 Domain。建议空闲 30 分钟、绝对 12 小时到期；改密码、停用账号和退出立即使服务端会话失效。密码保护见[数据设计](data-design.md)，认证失败统一返回错误，不区分账号不存在与密码不符。

`GET /api/v1/me` 返回用户、管理权限、会话期限及会话绑定的 CSRF token。除登录外的浏览器变更请求携带 `X-CSRF-Token`，检查精确 Origin；登录要求同源 Origin 与 JSON Content-Type，也不开放跨域。无效 Origin、缺少／错误 CSRF token 均拒绝；Cookie 属性不能替代 CSRF 检查。这一机制参考 [OWASP 会话管理](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)，具体期限为本项目提案。

| 方法与路径 | 请求及成功结果 |
| --- | --- |
| `POST /api/v1/sessions` | `{username,password}` → 200、会话 Cookie；无公开自注册 |
| `DELETE /api/v1/sessions/current` | 当前会话 → 204，并清除 Cookie；重复退出无副作用 |
| `GET /api/v1/me` | → 200 `{user_id,username,auth_rev,system_capabilities,csrf_token,expires_at}` |
| `POST /api/v1/users` | 管理员提交 `{username,password}` 及幂等键 → 201 `{user_id}` |
| `GET /api/v1/users` | 管理员查询 `{user_id,username,enabled,auth_rev}` 列表，不返回口令摘要 |
| `PATCH /api/v1/users/{user_id}` | 管理员提交 `{enabled}` 或 `{password}`、If-Match 及幂等键 → 200；使既有会话失效并递增授权版本 |
| `POST /api/v1/nodes` | 管理员提交 `{display_name}` 及幂等键 → 201 `{node_id,status:"pending"}`，尚无执行身份 |
| `GET /api/v1/nodes`、`GET /api/v1/nodes/{node_id}` | → 获准节点及能力、连接状态、观察时间；详情含当前用户 `capabilities` |
| `GET /api/v1/nodes/{node_id}/grants` | 管理员查询各用户显式能力及其 `auth_rev`，包含未授权用户的空集合 |
| `PUT /api/v1/nodes/{node_id}/grants/{user_id}` | 管理员提交 `{capabilities:[...]}`、If-Match 及幂等键 → 200、递增 `auth_rev`；空集合撤销该节点授权 |
| `POST /api/v1/nodes/{node_id}/enrollment-tokens` | 管理员及幂等键 → 201、仅一次展示的 10 分钟认领 token；重试只返回 token 记录 ID 和状态，不再回显秘密 |
| `POST /api/v1/nodes/{node_id}/revoke` | 管理员及幂等键 → 200；撤销节点凭据、阻止新许可并关闭会话，已授权操作进入核对 |

管理变更有审计；修改账号／Grant 的 If-Match 使用双引号包围该用户的 auth_rev，缺失 428、过期 412，防止旧管理页面覆盖刚完成的撤权。同一节点同时最多一个待消费认领 token。认领响应丢失且 token 未取得时，管理员显式重新签发并废止旧 token，不尝试从摘要恢复秘密。节点已经登记身份时，不允许新 token 静默替换身份；重装的隔离流程见 6.7。登录与认领失败使用有界限速，建议每来源地址与账号／token 记录各 10 次每分钟，超限 429 并附 `Retry-After`，不将此初始值作为性能结论。

### 6.3 配置、部署和查询路由

| 方法与路径 | 输入、结果及关键约束 |
| --- | --- |
| `GET /api/v1/nodes/{node_id}/stacks` | 获准 Stack 元数据、保存／部署版本与观察；无配置原文 |
| `POST /api/v1/nodes/{node_id}/stacks` | `{display_name,project_name,config}`＋幂等键 → 201 `{stack_id,version_id}`；Stack、首版本与去重记录同事务建立 |
| `GET /api/v1/stacks/{stack_id}` | → 200 元数据，`ETag` 为当前保存版本 ID 的双引号形式 |
| `GET /api/v1/stacks/{stack_id}/versions/{version_id}` | → 200 `{version_id,config}`；版本必须属于该 Stack，读取原文需 `config.read` |
| `POST /api/v1/stacks/{stack_id}/versions` | `{config}`＋`If-Match`＋幂等键 → 201 `{version_id}` 和新 `ETag`；只保存，不部署 |
| `POST /api/v1/stacks/{stack_id}/apply` | `{version_id}`＋幂等键 → 202 `{operation_id,state:"accepted"}`，`Location` 指向操作；目标由 Stack 固定，不另接受冲突 node_id |
| `GET /api/v1/operations/{operation_id}` | → 200 操作快照，包括目标、版本、`revision`、`state`、取消请求、错误及观察；不夹带配置或完整命令输出 |
| `GET /api/v1/operations?node_id=...&limit=...&cursor=...` | 获准节点的操作列表，供重开浏览器和恢复任务视图使用 |
| `GET /api/v1/submissions/{idempotency_key}` | 只查询当前用户的提交，→ `{result_type,result_id,response_status}`；404 只表示查询时未见提交 |
| `POST /api/v1/operations/{operation_id}/cancel` | 无额外动作参数 → 200 当前操作及 `cancel_requested`；重复请求幂等，不创建新业务操作 |
| `GET /api/v1/operations/{operation_id}/output?after=...&limit=...` | 经输出权限检查返回有界文本块、游标和截断标记；不得用此接口读取任意磁盘文件 |

输出接口的 limit 单位为字节，默认 64 KiB、最大 256 KiB，独立于列表的条数 limit；返回 `{text,next_cursor,truncated}`。单任务采集总量候选 1 MiB，超限保留截断标记和退出结果，不能为等待日志消费而阻塞或丢失终态证据。

M1 资源观察随节点／Stack 查询提供；资源删除、控制台和迁移不是本节的隐含接口。`config` 的必填字段为 `files:[{path,content_b64}]`、非空有序 `compose_files:[path]`、有序 `interpolation_env_files:[path]`、`interpolation_env:{name:string}`、`profiles:[string]`。后面三项可为空但不能缺省；所有主文件和插值文件必须存在于 files。路径不得重复，`project_name` 在创建后 M1 不允许通过保存重命名；Compose 项目名和外部引用的限制见 [Agent 工作区设计](components/agent.md)。

已有 Stack 保存必须提供强 `If-Match: "<current_version_id>"`，不接受 `*` 或弱 ETag；缺失返回 428，不匹配返回 412 `version_conflict`，不更新指针。新建没有基准版本，以节点内项目名唯一约束防止另一个请求建出同名项目。保存的并发语义采用 [HTTP 条件请求](https://www.rfc-editor.org/rfc/rfc9110.html#name-if-match)，缺少必要前提采用 [428](https://www.rfc-editor.org/rfc/rfc6585.html#section-3)。

以下为协议结构示例，ID 与摘要均为占位说明，不是运行结果：

```http
POST /api/v1/stacks/<stack_id>/apply
Content-Type: application/json
Idempotency-Key: <client_generated_uuid_v4>
X-CSRF-Token: <session_csrf_token>

{"version_id":"<saved_version_id>"}
```

```json
{"operation_id":"<operation_id>","state":"accepted"}
```

### 6.4 幂等、错误与浏览器轮询

`Idempotency-Key` 由调用者为一次明确意图生成 UUIDv4；所有表中要求幂等键的变更共用 `(user_id,idempotency_key)` 唯一域，不能跨路由复用。Manager 先鉴权，再查既有提交；同键同摘要返回已有结果定位与原成功状态，不再次执行条件保存；同键异摘要返回 409 `idempotency_conflict`。因此一次成功保存丢失响应后，重试原请求不会因自己的旧 If-Match 被误判为新的编辑冲突。原权限已撤销时仍拒绝读取旧结果。

请求摘要输入包括方法、规范路径、If-Match 及验证后的正文，排除 Cookie、CSRF、请求追踪号和服务端生成的对象 ID。采用 [RFC 8785 JSON 规范化](https://www.rfc-editor.org/rfc/rfc8785.html)，映射键有序、数组保序；原文件 Base64 使用标准唯一编码且不变更解码字节。摘要为 Manager 私有 HMAC，保存 `digest_key_id`，重试按旧记录对应密钥核对，不公开可用于猜秘密的内容哈希。成功提交的去重索引在 M1 生命周期内保留，清理大输出不清理该索引；更换密钥不提前删除仍被去重记录引用的旧摘要密钥。

受理前的拒绝不占用幂等键；受理与业务记录必须原子提交，数据库错误使两者一起回滚。提交超时先查 submissions；404 仍可用原键原输入再次提交，由唯一约束处理迟到原请求。调用者只有在得到确定拒绝或结果、并形成新的意图后才生成新键。仍未知的目标占用不会因新键被绕过。

错误正文固定为 `{error:{category,code,message,request_id,field_errors,retry_action}}`；`field_errors` 只给字段位置和原因，不回显配置、口令或凭据。`retry_action` 取 `none`、`same_request`、`query_operation`、`refresh_and_review`。HTTP 400 表示编码／schema 无效，401 未认证，403 缺动作权限，404 不可见或不存在，409 幂等／占用／名称冲突，412 版本冲突，413 体积超限，422 能力或配置组合不支持，428 缺 If-Match，429 限速，503 节点离线／服务暂不可用，500 未分类内部错误。已受理任务的执行失败通过 200 操作快照的 `state/error` 表达，不将查询变成 HTTP 500。

M1 前台每 2 秒轮询活动操作，后台标签页每 10 秒，终态停止，网络失败退避至 30 秒并显示最后更新时间；这些是初始客户端参数。快照 `revision` 单调递增，旧响应不得覆盖新快照；请求上下文同时检查用户和节点。服务端对每次轮询重新鉴权。全量命令输出不得在每次快照轮询重复传输。

### 6.5 Agent 连接与消息信封

Agent 通过 `wss://<manager>/agent/v1/connect` 主动连接，HTTP Upgrade 只接受 `Authorization: Bearer <node_secret>`，不接受浏览器 Cookie、查询串 token 或带 Origin 的浏览器接入。验证系统信任或显式配置的 CA 和目标主机名，不提供跳过 TLS 校验选项；最低 TLS 1.2。禁用 WebSocket 压缩，重组大小上限为 8 MiB。认证与消息尺寸需应用自己处理，WebSocket 不自动提供任务身份或幂等保证。[RFC 6455](https://www.rfc-editor.org/rfc/rfc6455.html#section-10.5)、[OWASP WebSocket 安全](https://cheatsheetseries.owasp.org/cheatsheets/WebSocket_Security_Cheat_Sheet.html)

每条文本消息为 `{protocol:1,type,message_id,session_id,reply_to,body}`；`message_id` 为 UUIDv4，首条 hello 的 session_id 和无应答关联的 reply_to 为 null，其余会话消息必须匹配 welcome 分配的随机 session_id。协议主版本不匹配拒绝连接；M1 不猜测未知类型或默默忽略未知执行字段。版本字符串只作能力报告，变更需满足具体能力集合。

| 类型／方向 | body 必需字段与处理 |
| --- | --- |
| `hello` Agent → Manager | `node_id,ledger_id,boot_id,agent_version,compose_version,capabilities,execution_profile`；身份由握手认证取得，正文必须匹配，boot_id 每次进程启动重新生成 |
| `welcome` Manager → Agent | `node_id,session_id,reconcile_required`；Agent 先使旧连接所有待开始 nonce 失效，才安装新连接 |
| `reconcile`／`reconcile_reply` 双向 | `scope,scan_id,cursor,complete` 及账本身份／阶段／摘要／结果；每页最多 200 项；scope=pending 包含未终结或结果尚未确认记录，备份恢复必须 scope=all 包含已终结且已 ack 的记录 |
| `prepare` Manager → Agent | `operation_id,node_id,stack_id,version_id,input_digest,payload_b64`；payload 是持久化冻结 UTF-8 JSON 的原字节，不临时重组 |
| `ready` Agent → Manager | `operation_id,input_digest,precheck_result`；仅校验成功才能请求开始 |
| `request_start` Agent → Manager | `operation_id,input_digest,start_nonce`；单调时钟开始 10 秒有效窗口 |
| `authorize_start` Manager → Agent | `operation_id,input_digest,start_nonce,permit_id,auth_rev`；只能应答当前连接的对应请求 |
| `deny_start` Manager → Agent | `operation_id,start_nonce,code`；无副作用，持久记录不开始 |
| `started`／`result` Agent → Manager | `operation_id,input_digest,event_seq` 及阶段；result 加 `exit_code,error,observation,not_started`，不重复发送原始配置 |
| `ack` Manager → Agent | `operation_id,event_seq`；对应事件已持久保存后才发送 |
| `cancel` Manager → Agent | `operation_id,input_digest`；未开始阻止启动，已开始只尝试终止并回报事实 |
| `observe` Manager → Agent | `query_id,resource_kind,stack_id,cursor,limit`；仅只读查询，limit 最多 200，stack_id 可为空表示节点概况 |
| `observation` Agent → Manager | `query_id,node_id,collected_at,items,next_cursor,error`；资源项含稳定 ID、类型、项目关联和允许的状态摘要，不回报完整环境或任意 inspect 正文 |

`payload_b64` 解码所得对象固定为 `{operation_type:"stack.apply",node_id,stack_id,version_id,project_name,config,required_capabilities,execution_profile}`，config 使用 6.3 的同一结构；required_capabilities 至少含 `compose.apply.v1`、`config.bundle.v1`，与用户权限名不同。execution_profile 含节点声明的 `docker_endpoint_id` 与 `policy_revision`，绑定 Agent 的本地受控执行配置，不能由 Web 提交任意 host 或 CLI。Agent 比较内外层目标、版本和类别；本地执行配置在准备后变化使准备失效，不能继续启动。

秘密原文只在受保护通道中发送。`input_digest` 在线为 32 字节 HMAC 的无填充 Base64url 字符串；它由 Manager 产生，Agent 不持有该密钥。Agent 用本机私有账本密钥对实际 payload 字节计算 `local_fingerprint`，同操作必须同时匹配两者。重复 prepare 返回账本状态，不重复准备或启动；协议不能用重排、重新序列化或改变文件字节掩盖输入变化。

result 的原始输出通过可空 `output_b64` 与 `output_truncated` 返回，合计最多 1 MiB 原始字节；普通 snapshot 只提取错误类别与观察摘要，正文按输出权限单独读取。result 的 observation 带 `status,collected_at,items,error`，状态为 complete／incomplete／error；M1 Apply 结果与占用释放严格按[数据判据](data-design.md#41-m1-apply-的结果与占用释放)处理。所有观察查询都有 query_id 和采集时间，返回迟到时不得冒充较新观察；查询超时仅报告 unavailable，不触发变更。完整资源类型字段在各资源切片补齐，M1 只定义项目／容器状态摘要。

ready 的 `precheck_result` 为 `{ok:true,checked_at,compose_version,workspace_ref}`；预检不通过则直接发送 result，`not_started:true`、exit_code 为 null，并带确定错误。started 事件表示本地开始事实已持久化；result 的 exit_code 在进程真实退出时为整数，其余为 null，error 可以为 null，observation 独立说明采集成功或失败。`event_seq` 是 Agent 在每个操作内耐久递增的正整数；Manager 自己的状态事件使用独立 source 命名空间，不占 Agent 的序列，浏览器整体排序只使用操作 revision。

连接使用 Ping/Pong 每 15 秒探活，45 秒未收到 Pong 断开；Agent 以 1—30 秒指数退避加随机抖动重连，认证失败停止自动重试并显示需重新登记。心跳只判断通道可达，不替代 Docker 能力或业务健康。每节点只允许一个活动 Agent 实例；本地工作目录排他锁、持久 ledger_id 与重连核对共同约束，复制凭据运行多个独立账本不是受支持的容灾方案。

### 6.6 开始许可、取消与迟到消息

开始的授权生效点是 Manager 在同一数据库写事务中复核账号启用、当前 Grant/auth_rev、节点凭据、当前 session_id、目标占用、输入摘要和取消状态，并提交 `authorized` 与唯一 permit_id。撤权／取消也走该串行写边界：先提交撤权或取消则不再批准；先提交许可则该操作可能随后开始，不能承诺撤权立即收回已发送许可。

Agent 的 start_nonce 只属于当前连接、当前操作和一次本地 10 秒窗口。Manager 对重复 request_start 只返回该连接和 nonce 已持久化的同一 permit_id，不为同一任务换 nonce 自动发第二张许可。接收许可时匹配 session_id、nonce、摘要、permit_id、未取消状态和单调时钟；在排他执行区耐久记录 `START_COMMITTED`，启动前再次检查连接和期限，再创建进程。换连接、过期或本地取消的许可直接丢弃；不能保存为离线启动凭证。10 秒限制许可接受和启动检查，不把网络、落盘和 Docker 副作用包装成原子事务。

`START_COMMITTED` 对外对应 running／可能已有副作用；写记录后、创建进程前崩溃也必须核对，不能自动重跑。许可过期不使 Manager 自动释放占用；Agent 明确持久报告 `not_started:true` 后，才以 `failed/permit_expired` 等结果终结，重新执行需新的明确意图。未开始证据只能来自尚未提交开始的拒绝，或同一存活执行流程在 START_COMMITTED 后明确没有调用进程创建并耐久记录的拒绝；重启后不能根据进程不存在补造此证据。许可不跨连接重发；同连接重复许可只回报账本，不再次启动。

受理但尚未签发许可时，Manager 可持久取消并阻止后续许可；authorized 之后只返回取消请求，必须取得 Agent 未开始证明或实际结果才可终结。Manager 重启、结果超时和通道断开均不能推定未执行；未知操作继续占用目标。正常结束结果以 `(operation_id,event_seq)` 去重，矛盾结果转核对；ack 丢失只重报结果。

### 6.7 节点认领、轮换与恢复

Agent 首次创建持久 ledger_id 和 32 字节密码学随机候选 node_secret，先以仅服务账号可读的文件耐久保存，再通过 HTTPS `POST /agent/v1/enroll` 发送 `{node_id,ledger_id,credential_secret}`，Authorization 使用一次性认领 token。Manager 原子消费 token 并绑定 node_id、ledger_id 和凭据摘要；不保存可恢复的长期秘密明文。响应只含绑定标识；丢响应时先用本地候选凭据连接确认，仍需重发认领时，只允许原 token、同节点、同 ledger_id、同凭据摘要返回同一绑定，不能借此建立第二个节点。

正常轮换保留完整账本：Agent 先保存新候选秘密，再用当前凭据调用 `POST /agent/v1/credentials/rotate`，正文 `{ledger_id,credential_secret}`；Manager 原子替换摘要并作废旧连接，Agent 用候选重连后移除旧秘密。若提交响应丢失，候选连接成功即确认完成；候选明确收到 401 时才以旧凭据重试相同轮换，网络超时不构成候选失效证明，绝不生成第三个候选覆盖未确认状态。两套凭据均无效进入人工恢复，不循环认领。长期秘密只出现在受保护请求体或 Authorization，不进入 URL、审计和普通日志。

丢失账本、安装目录或 ledger_id 时，旧 token／同名节点均不足以证明执行历史。Manager 将该节点隔离、保留未知占用；管理员核对旧操作和资源后才能显式废止旧身份并登记新实例。新实例不能把旧操作 ID 当作没有执行过。Manager 从备份首次恢复也在隔离模式，完成身份、账本、授权和目标观察核对后才开放变更。

备份恢复以 scope=all 枚举 Agent 保留的全部执行记录，包括已经终结且收到 ack 的历史；普通重连的 pending 集合不足以证明恢复完整。Agent 建立 scan_id 时冻结新 Prepare，确定本轮操作身份集合及总数，每页返回 operation_id、输入摘要、目标、阶段和确认状态；分页结束校对扫描数量。期间旧操作可能结束，须继续重报其结果并再次核对非终态记录。断线可在同一有效扫描上续页；Agent 重启或扫描失效则重新扫描，不能把中断当 complete。

Manager 对本库缺失的历史建立 recovery_cases，保留 Agent 证据及实际目标，既不忽略，也不伪造已丢失的配置、发起者或受理时间。全部页面及未决结果核对完成、缺口记录有依据地处理后，才允许解除该节点隔离。替换 node_id 或重新认领同一实际 Docker 端点不绕过旧隔离；身份无法对应时保持隔离供人工核对。

### 6.8 实现前必须证明的协议行为

至少覆盖：新建和条件保存丢响应仍只有一份结果；同键异输入拒绝；许可前撤权与许可后撤权两种顺序；旧连接许可迟到、10 秒期限到达、START_COMMITTED 后崩溃；结果 ack 丢失；认领和轮换提交后丢响应；账本丢失及 Manager 备份恢复。用例需检查持久记录与实际 Docker 副作用，不能仅观察 HTTP／WebSocket 返回码。具体实验安排见[测试计划](test-plan.md)；当前这些均是待执行验证。
