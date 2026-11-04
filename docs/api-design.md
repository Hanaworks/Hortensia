# Docker Manager 接口规格

更新日期：2026-11-04。状态：整体逻辑契约与 M1 v1 通信工作规格入口，尚无可调用服务。面向 Web、Manager 和 Agent 的接口实现者，承接 [arc42 构件接口、运行交互及共同概念](architecture.md)。第 1—5 节定义整体操作语义，第 6 节连接准确的 HTTP／Agent 专题和 Schema；运行机制仍需原型验证，O03、O07 尚未关闭。

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

本节保留共同约定和入口；具体内容已在 2026-11-04 推进为 `http-v1-draft.1` 与 `agent-v1-draft.1` 工作规格。这里的“提案”说明尚无运行验证，不表示实现者可以自行改动字段；实现或原型发现问题时同步修订主定义。不是用户逐项批准的事实，也不是完整产品协议。

### 6.1 共同编码和预算

Web 使用同源 HTTPS `/api/v1`，Agent 使用独立认证的 HTTPS `/agent/v1` 及 TLS WebSocket。管理器生成的身份为小写 UUIDv4；Docker 原生资源标识保留其专有格式，不强转 UUID。时间为 UTC RFC3339，revision 为非负 JSON 安全整数。请求拒绝未知字段、重复键和无效 UTF-8；空值和缺省按各对象规定。配置内容使用标准带填充 Base64，解码字节不改写。

v1 工作预算为 128 文件、单文件 1 MiB、原文件合计 4 MiB、HTTP 请求与 WebSocket 重组消息各 8 MiB；Base64／JSON 开销计入消息上限。路径最多 240 个 UTF-8 字节。接收文件包时校验原内容合计和冻结 payload 的编码大小。预算供首轮实现和实验使用，不是已验证容量；超限整体拒绝，不截断配置。

### 6.2 用户会话与权限

本地账号使用服务端会话；Cookie 为 `__Host-dm_session; Secure; HttpOnly; SameSite=Strict; Path=/`，无 Domain，数据库只保留随机会话秘密的摘要。空闲期限 30 分钟，绝对期限 12 小时；变更密码、停用和退出使已有会话失效。`GET /me` 返回会话绑定的 CSRF token；除登录外的浏览器变更同时验证 `X-CSRF-Token` 和精确 Origin，登录验证 Origin 和 JSON Content-Type。期限是当前实现参数，需经实验核实。

| 权限 | 节点范围内的能力 |
| --- | --- |
| system.admin | 账号、节点接入元信息、认领与授权管理；不隐式取得下列节点动作权限 |
| node.read | 节点及资源摘要；不含配置和命令原始输出 |
| config.read | Stack 原文与版本，包含用户在配置中保存的敏感值 |
| config.write | 保存／新建；同时要求 node.read 和 config.read |
| stack.deploy | 固定版本部署；同时要求 node.read 和 config.read |
| operation.read | 结果和观察摘要；同时要求 node.read |
| operation.output.read | 受限原始输出；同时要求 node.read 和 operation.read |
| operation.cancel | 请求取消；同时要求 node.read 和 operation.read |

Manager 每次请求和发放开始许可均复核授权，Web 权限摘要只用于显示。node.read 缺失时资源请求返回404；节点可见但动作不足返回403。system.admin 的节点元信息例外由 HTTP 规格明确裁剪，不扩大为资源读取权。登录／认领失败初始限速为每来源地址和账号／token 记录各 10 次／分钟，超限429附 Retry-After；未来调整先更新运行参数和验证。

### 6.3 精确规格与主定义

| 对接任务 | 必须采用的规格 | 说明 |
| --- | --- | --- |
| Web—Manager、Agent 认领／轮换 HTTP | [HTTP 工作规格](specifications/http-api.md) | 完整路由、对象、错误、幂等回执、权限裁剪、ETag、分页、输出和连续保存 |
| Manager—Agent 消息 | [Agent 协议](specifications/agent-protocol.md)及[消息 Schema](../contracts/agent-v1.schema.json) | 所有当前消息及嵌套类型；关联、状态前提、完整性与错序由正文说明 |
| 重复、恢复、资源占用 | [执行与恢复](specifications/execution-recovery.md) | outbox、归属证据、补观察、finalize／finalized 和扫描结束握手 |
| 数据含义和持久边界 | [数据设计](data-design.md) | 管理版本、执行事实、审计与资源观察分开；安全回执与去重证据持久化 |

消息传输成功不等于执行完成；ack 只确认结果证据持久化，finalized 才允许 Manager 完成本地占用释放。正常退出和完整资源观察分开编码；interrupted／unknown 不能被一次观察变成成功。真正未知结果的人工解除仍受维护手段验证限制，v1 原型保持隔离，不提供无证据解锁。

### 6.4 实现前后的验证界限

运行验证至少覆盖：同 key 保存和创建丢响应；许可与撤权两种先后；迟到许可；START_COMMITTED 后崩溃；result／ack／finalize／finalized 各点丢失；认领和轮换丢响应；账本丢失和旧备份的全历史扫描。静态结构通过不证明这些行为已经成立。

[测试计划](test-plan.md)记录可重复的检查与待运行实验。本期没有 Copy、迁移、广泛资源变更、实时输出或控制台消息；它们按既有增量路线扩展契约，不能把未知 type 转成任意命令执行。身份与会话设计参考 [OWASP Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)，条件写入沿 [RFC 9110](https://www.rfc-editor.org/rfc/rfc9110.html#name-if-match)和[缺少前提428](https://www.rfc-editor.org/rfc/rfc6585.html#section-3)；具体工程值来自本项目工作规格。
