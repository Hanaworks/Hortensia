# M0 通信原型实际实现规格

更新日期：2026-11-07。运行行为基线：`98d9c13`；验收代码／测试基线：`94056e0`，运行代码未变。本文描述当前程序的输入、输出、交互、状态和异常处理；后续实现改变这些行为时同步更新本文。

文档形成时点：完整 v1 通信规格与消息 Schema 先于本轮代码存在；本篇由 `dc0529c` 根据已完成的原型回填。编码前的 M0 实施计划只定义实验目标、主要约束和验证任务，未完整列出最终参数、消息支持及 Gate 接口。本文提供当前实现的核对依据；实施期间形成的技术选择及其来源在第 7 节登记，不将回填内容追记为编码前已经确定或用户逐项批准。

[Agent v1 通信规格](agent-protocol.md)定义拟实现的完整任务协议，[Schema](../../contracts/agent-v1.schema.json)定义消息结构，本文明确原型实际落实到哪里。[实施记录](../implementation/m0-agent-channel.md)承载构建、运行、测试执行和修复记录；源码参考依据另见 [Portainer 对照](../research/portainer-agent-reference.md)。

## 1. 实现组成与边界

| 实现 | 当前职责 | 接入情况 |
| --- | --- | --- |
| [channelprobe](../../cmd/channelprobe/main.go) | 同一命令按 manager／agent 模式运行两个进程 | 使用真实 TLS WebSocket；Manager 一次配置一个 node_id／ledger_id／token 组合 |
| [channel](../../internal/channel/channel.go) | Upgrade 认证、hello/welcome、会话替换、消息读取与冻结诊断 | 已接入命令行；握手成功后始终保持 `reconcile_required=true` |
| [protocol](../../internal/protocol/validator.go) | 20 类消息的结构及部分解码约束校验 | 已用于通道读入、Agent 发出 hello 前的检查；可在测试中单独验证 prepare |
| [startgate](../../internal/startgate/gate.go) | 一个操作、一个 nonce 的内存开始许可门禁 | 仅由独立测试调用；尚无网络消息到 Gate 的适配器 |

当前channelprobe没有任务授权服务、执行器或配置版本仓储，也未接入独立ledger包。仓库随后实现的[开始账本](m0-agent-ledger.md)及[事件日志](m0-agent-journal.md)可持久保存开始事实、事件和占用，但尚未接到本篇网络路径；finalize握手仍未实现。独立Gate的提交回调本身不证明持久化，也不能据此解除通道冻结。

## 2. 命令行输入与可观察输出

### 2.1 参数

| 参数 | 默认／必需 | 当前含义及拒绝条件 |
| --- | --- | --- |
| `-mode` | 必填 | 仅 `manager`、`agent` |
| `-node-id`、`-ledger-id` | 两端必填 | 小写 UUIDv4；两端值须分别一致。由参数固定，程序不认领或持久生成身份 |
| `-token-file` | 两端必填 | 文件内容为 32 字节的规范 Base64url 无填充编码，即 43 字符；可带一个 LF、CR 或 CRLF 行尾，不接受额外空格或多行 |
| `-duration` | `30s` | Go duration 格式，必须大于 0；运行上下文的截止时间，不是部署期限 |
| `-listen` | `127.0.0.1:8443` | Manager 使用；要求字面量 loopback IP 和 0—65535 端口。端口 0 由系统分配；`localhost`、通配地址和普通网卡地址被拒绝 |
| `-cert`、`-key` | Manager 必填 | 可加载且匹配的 TLS 证书／私钥文件；承载服务器最低 TLS 1.2 |
| `-url` | Agent 必填 | `wss://host/agent/v1/connect`；拒绝 userinfo、query（包括空 `?`）及非空 fragment。不跟随 HTTP 重定向 |
| `-ca` | Agent 可选 | 未提供时使用系统 CA；提供时从该 PEM 文件建立专用根池，不与系统池自动合并；仍检查服务器名称 |

参数全部由命令行读取，没有配置文件或环境变量覆盖。`-listen/-cert/-key` 只用于 Manager，`-url/-ca` 只用于 Agent；解析器没有把另一模式的这些参数列为禁用参数。无法解析参数、额外位置参数或非法 mode／duration 返回 `invalid_arguments`。

### 2.2 stdout、stderr 与退出

stdout 是每条一行的 JSON；输出对象的键顺序没有约定。监听建立后 Manager 发一次：

```json
{"event":"listening","address":"127.0.0.1:8443","node_id":"00000000-0000-4000-8000-000000000001","ledger_id":"00000000-0000-4000-8000-000000000005"}
```

Agent 完成应用握手后发一次：

```json
{"event":"handshake","node_id":"00000000-0000-4000-8000-000000000001","ledger_id":"00000000-0000-4000-8000-000000000005","session_id":"00000000-0000-4000-8000-000000000008","reconcile_required":true}
```

Manager 的 listening 只表明监听已建立；Agent 的 handshake 表明 hello/welcome 已核对，不表明恢复完成或可以部署。stderr 使用固定诊断，不回显秘密、原始参数或文件内容。

| 退出码 | 当前情形与 stderr 诊断 |
| --- | --- |
| `0` | 已进入运行阶段后，duration 到期或收到可处理的中断信号后退出；不输出“部署成功”。Manager 先关闭所有已 Upgrade 连接，再等待 HTTP Shutdown，最多等 2 秒后强制关闭服务器 |
| `2` | 参数／本地材料错误：`invalid_arguments`、`loopback_listen_required`、`tls_files_required`、`invalid_agent_url`、`identity_files_required`、`invalid_token_file`、`invalid_tls_files`、Manager 的 `invalid_identity`、`invalid_ca_file` |
| `1` | 运行失败：`listen_failed`、`report_failed`、`server_failed`；Agent 收到 HTTP 401 为 `unauthorized`，其余连接／握手错误为 `connection_failed`，握手后非主动到期断线为 `connection_closed` |

Agent 的 UUID／连接配置最终检查发生在 Connect 中，失败归为 `connection_failed`；并非所有身份错误都用退出码 2。若建立连接期间 duration 已到期，也按连接失败返回1，不属于运行阶段的正常到期退出。Agent 每次调用只尝试连接一次，任何失败或断线都退出，尚无自动重连调度。`-h` 当前同样返回 `invalid_arguments`，尚无帮助输出。

## 3. 当前网络交互

### 3.1 Upgrade 与应用握手

1. Manager 仅为精确路径 `/agent/v1/connect` 提供服务，其他路径 HTTP 404；所有响应加 `Cache-Control: no-store`。
2. 该路径须为 TLS 上的 GET，只有一个 Authorization 值且精确等于 `Bearer <配置 token>`；存在 Cookie、Origin 或任何 query 都 HTTP 401。随后才由 WebSocket 库核对 Upgrade。handler 本身要求 `r.TLS` 存在，最低 TLS 版本由承载它的命令行服务器设置。
3. 接收第一条 UTF-8 文本消息，执行共享校验器；必须为 hello，body 中 node_id／ledger_id 与固定配置一致。Manager 不创建新身份，也没有能力协商或实际 Docker 能力检查。
4. Manager 分配新随机 UUIDv4 session，替换旧 current session，发出 welcome；旧连接以 1008／`session_replaced` 关闭。旧连接清理仅在自身仍为 current 时清空引用，不删除新连接。
5. Manager 从 Upgrade 完成后为 hello 读取及 welcome 写入共用一个 10 秒上下文。Agent 则从 Dial 开始到 welcome 校验结束共用最长 10 秒上下文，并受 duration 的更早截止限制。
6. Agent 核对 welcome 的 type、reply_to 等于本次 hello 的 message_id、node_id 一致、信封与 body 的 session_id 一致，并要求 reconcile_required 为 true。目标协议允许的 false 在当前探针中不被接受。

### 3.2 实际发送的 hello／welcome

信封使用 v1 原有六个字段，不增加探针专用消息。下例 UUID 是固定示例；程序中 message_id、boot_id、docker_endpoint_id 和 Manager session_id 均随机生成。

```json
{
  "protocol":1,
  "type":"hello",
  "message_id":"00000000-0000-4000-8000-000000000100",
  "session_id":null,
  "reply_to":null,
  "body":{
    "node_id":"00000000-0000-4000-8000-000000000001",
    "ledger_id":"00000000-0000-4000-8000-000000000005",
    "boot_id":"00000000-0000-4000-8000-000000000006",
    "agent_version":"m0-transport-probe",
    "compose_version":"unavailable",
    "capabilities":[],
    "execution_profile":{
      "docker_endpoint_id":"00000000-0000-4000-8000-000000000009",
      "policy_revision":0
    }
  }
}
```

```json
{
  "protocol":1,
  "type":"welcome",
  "message_id":"00000000-0000-4000-8000-000000000101",
  "session_id":"00000000-0000-4000-8000-000000000008",
  "reply_to":"00000000-0000-4000-8000-000000000100",
  "body":{
    "node_id":"00000000-0000-4000-8000-000000000001",
    "session_id":"00000000-0000-4000-8000-000000000008",
    "reconcile_required":true
  }
}
```

boot_id 和 docker_endpoint_id 在每次 Connect 生成，只作当前原型字段占位，不是已持久的进程／Docker 端点登记。Agent 发出的 capabilities 固定为空。Manager 即使收到其他符合 Schema 的能力数组，也始终冻结，不启用相应操作。

### 3.3 20 类消息的实际支持范围

共享校验器可检查全部 20 类消息的结构。通道的业务实现范围如下；“通过结构检查”不表示执行了该类操作。

| 消息 | 当前通道行为 |
| --- | --- |
| `hello`、`welcome` | 仅用于首次握手，按上节核对 |
| `reconcile_reply`、`reconcile_completed`、`ready`、`request_start`、`started`、`result`、`observation`、`finalized`、`protocol_error` | Manager 在当前 session 上允许进入冻结诊断分支；不建立业务状态、不保存结果、不 ack，不完成恢复或释放 |
| `reconcile`、`reconcile_complete`、`prepare`、`authorize_start`、`deny_start`、`ack`、`cancel`、`observe`、`finalize` | Manager 当前不发送；Agent 侧没有这些操作的处理器。若客户端向 Manager 反向发送，结构有效仍按方向错误关闭 |

冻结诊断分支回复 `type=protocol_error`，session_id 为当前会话，reply_to 为收到消息的 message_id。body 精确为：

```json
{"error":{"category":"unavailable","code":"prototype_frozen","message":"Recovery and execution are not implemented in this probe.","retry_action":"none"}}
```

这个应答供受控通道测试读取。当前 Agent 的 Peer.Run 在握手后只维持探活和读循环；收到任何通过共享校验器检查的应用消息（包括上述 protocol_error）都以 1008／`unexpected_message` 关闭，并由 CLI 报 `connection_closed`。未通过检查的消息先按对应的读取／校验错误关闭。它没有部署报文消费循环，startgate 也未接在这里。

Manager 不检查这些诊断消息是否曾有业务请求可关联。它只检查结构、当前 session、方向和消息 ID 一致性；完整 reply_to、事件序号与持久结果的核对留待任务通道实现。

### 3.4 去重、探活与关闭

| 项目 | 当前具体规则 |
| --- | --- |
| 消息 ID 一致性 | Manager 从 hello 开始记录消息指纹；同 ID 指纹改变则关闭。指纹基于整个信封解码再编码的结果，忽略空白、键顺序，并统一1／1.0／1e0等形式；不保证所有数学等价写法同指纹，具体编码边界见第8.3节；不只比较 body |
| 指纹表预算 | 最多 1024 项，包含 hello。重复 ID 不增加项数；达到 1024 项后再收到任何握手后消息都会关闭，即使是同内容重试 |
| 发送与存活 | Peer.Run 启动后每 15 秒独立发 Ping，按最后一次收到 Pong 的时刻计算 45 秒无响应期限；任何 Pong 都刷新存活时间，Ping 调用自身另按 payload 关联应答 |
| 在途预算 | 最多 4 个尚未完成的 Ping 调用；耗尽时报内部 `heartbeat_pending_budget`，不是宣称已经 45 秒没有 Pong |
| 退出清理 | 取消上下文、关闭连接、等待在途 Ping goroutine 回收；Server.Close 也关闭尚在等待 hello 的已 Upgrade 连接 |

| 触发条件 | 当前关闭行为 |
| --- | --- |
| 非文本消息 | 1008，`text_required` |
| 非法 UTF-8 | 1007，`invalid_utf8` |
| 重组消息超过 8 MiB | WebSocket 库关闭 1009；覆盖合并后的消息，不仅单帧 |
| Schema、JSON、编码或解码预算失败 | 1008，`invalid_message`；不把校验器内部错误或秘密放入 reason |
| 第一条消息不是 hello，或 node／ledger 不符 | 1008，`identity_mismatch` |
| welcome 身份／关联／会话／核对标记不符 | Agent 以 1008，`welcome_mismatch` 关闭 |
| 重复 hello、方向错误、旧 session、指纹冲突或表预算耗尽 | 1008，`unexpected_message` |
| 应用握手上下文到期、探活故障、主动退出 | 由上下文或 CloseNow 结束连接；不保证收到带诊断 reason 的正常关闭帧 |

内部 `heartbeat_silence`、`heartbeat_pending_budget`、`heartbeat_transport_failure` 没有单独向 CLI 透出，非到期断线最终汇总为 `connection_closed`。连接关闭本身不生成操作失败或“未开始”结果。

## 4. 已实现的解析与输入约束

校验入口为 `Validator.Validate(raw) (Envelope,error)`：先做 8 MiB、严格 JSON 与数字预算检查，再验证嵌入的 Schema，随后验证规范 Base64／摘要编码；prepare 另外解码并校验 FrozenPayload。编译 Schema 时拒绝外部资源加载。

| 输入 | 当前检查 |
| --- | --- |
| JSON | UTF-8、单个完整值、没有尾随第二个值、没有重复对象键；递归解析深度上限沿用 10000。未知字段、必填与 null 规则由 Schema 决定 |
| 数字 | token ≤128 字节，显式指数绝对值≤128；精确整数且范围在 ±9007199254740991 内；再检查所属字段范围。1、1.0、1e0 等价，小数不能舍入成整数 |
| 字符串模式与日期 | ECMAScript 正则引擎，单次匹配超时 100 ms 后不匹配；日期另用 RFC3339 解析验证真实日历值 |
| 编码 | Base64 严格解码后重编码相同；input_digest 为规范无填充 Base64url 的32字节值；输出若非 null，解码须 UTF-8 且≤1 MiB |
| prepare | 内层 node_id／stack_id／version_id 等于外层；FrozenPayload 满足 Schema；路径不重复，拒绝空路径段、点段、反斜杠、冒号及 NUL，路径≤240 UTF-8 字节；compose/env 引用均须在 files 中；单文件≤1 MiB、总量≤4 MiB、内容为 UTF-8，文件数≤128由 Schema 约束 |

校验失败只返回固定码：`invalid_json`、`schema_invalid`、`encoding_invalid`、`payload_invalid`、`budget_exceeded`、`number_invalid`。构造阶段的本地 Schema 编译错误也用固定码。`NewMessage` 只构造信封及随机 message_id，不自动证明方向、会话或业务状态合法。

当前没有验证 Docker 能力、原生 YAML／Compose 语义、文件系统符号链接、HMAC 业务真实性、required_capabilities 与当前 Agent 能力的对应关系，也没有完整检查所有报文的跨字段／跨消息关联。prepare 的独立校验通过不代表原型能够接收或执行 prepare。

## 5. 独立开始许可门禁

### 5.1 最终接口与输入

入口为 `startgate.New(Request, Hooks) (*Gate,error)`。每个 Gate 只表示一个操作、一个 session 和一个 nonce；创建时记录 `Now()+10s`，到达该时刻即过期。调用方应在首次 request_start 的发送边界创建 Gate，不能在 ready 阶段提前创建。

| 输入 | 字段／调用约定 |
| --- | --- |
| Request | `Session, Operation, Digest, Nonce, MessageID string`；New 仅检查这些字段非空；RegisterRequest 要求前四项与初次请求一致，记录新的 MessageID |
| Permit | `Session, Operation, Digest, Nonce, PermitID string; AuthRev uint64; ReplyTo string`；检查身份／摘要／nonce、ReplyTo 属于已登记请求、PermitID非空、AuthRev≤安全整数上限；首次选定后相同 Gate 不能改 PermitID／AuthRev |
| Denial | `Session, Operation, Nonce, ReplyTo, Code string`；检查身份和关联、Code非空，没有 Digest 字段 |
| Hooks | `Now func() time.Time`、`CommitStart func(Permit) error`、`Start func(Permit)` 均须非空；调用方保证时钟单调。CommitStart 在锁外运行；Start 在最终串行区内，仅启动工作并快速返回，不能重入 Gate |
| 方法 | `RegisterRequest(Request) error`、`Authorize(Permit) error`、`Deny(Denial) error`、`Cancel() error`、`Invalidate()`、`State() State` |

Gate 不自行做 UUID、摘要编码或完整 deny_start 结构及 code 格式校验，也不读取真实连接或权限仓储。请求关联集合当前没有额外数量上限。未来调用方必须先做消息校验、绑定实际连接，并在连接关闭／被替换时调用 Invalidate；当前网络程序尚未调用这套接口。

### 5.2 状态、提交与并发

| 状态 | 进入条件与实际含义 |
| --- | --- |
| `waiting` | New成功；允许登记同 nonce 的其他请求 ID；不延长窗口 |
| `committing` | Authorize 校验通过并选择许可，准备在锁外调用 CommitStart；并发重复授权返回 ErrCommitting |
| `started` | CommitStart成功，重获锁后状态仍为committing且未到期，设置状态并调用 Start。仅证明该回调被调用，不证明进程／Docker启动或持久成功 |
| `canceled` | waiting／committing 上 Cancel成功；重复 Cancel仍成功。不能覆盖started或uncertain |
| `invalidated` | waiting／committing 上 Invalidate；另置独立断线标志，使已started的Gate也拒绝旧许可重放，同时保留started事实 |
| `denied` | waiting且未到期时收到相关联的Deny；提交中或开始后的否认不能改写状态 |
| `expired` | waiting 的有效待处理检查发现已到期，或提交成功后的第二次检查到期；仅推进时间不主动改变State，须经方法检查 |
| `uncertain` | CommitStart报错且返回时状态仍为committing；该Gate不能再次尝试提交或启动 |

正常顺序为 `校验 → committing → CommitStart → 再检查状态／期限 → started → Start`。CommitStart期间 Cancel或Invalidate无需等待该回调结束即可改变状态；此时回调返回后不会调用Start。提交报错总返回带 ErrCommitFailed 的错误；若期间已取消／失效，保留该状态，不强制覆盖为uncertain。回调没有独立超时，Gate也不捕获回调panic；这些都是调用方边界。

同一已started、未断线的Gate收到匹配许可重放时，返回nil但不再次提交或启动；即使原窗口随后过去，也只确认已有选择。换 PermitID／AuthRev 返回 ErrPermitConflict，未知请求关联或不符的身份按对应错误拒绝；同窗口内另一个已登记的 ReplyTo 可以接受。

Authorize 在断线检查后依次检查 session→operation→nonce→digest→reply_to→许可字段→已选许可冲突→started幂等→待处理状态及期限。因此字段错误或许可冲突可能先于终态错误返回。RegisterRequest／Deny 先检查各自身份和字段，再检查待处理状态。Cancel不检查期限；Invalidate对非waiting／committing状态只标记断线，不覆盖原状态。因此仅推进时间但尚未触发期限检查的waiting，仍可被Cancel改为canceled。

主要可判定错误为 `ErrSession/ErrOperation/ErrDigest/ErrNonce/ErrReplyTo`、`ErrPermit/ErrPermitConflict`、`ErrExpired/ErrCanceled/ErrInvalidated`、`ErrCommitting/ErrAlreadyStarted/ErrDenied/ErrUncertain`、`ErrRequest/ErrCommitFailed`，分别对应身份关联、许可、状态或回调失败。它们是 Go 包内错误，不是已在网上发出的 Error 报文；CommitStart返回的外部错误还会与ErrCommitFailed合并，不承诺其错误文本可直接安全记录。

## 6. 规格到验证的对应

本节提供测试入口。[实施与验收中的 AC01—AC11](../implementation/m0-agent-channel.md#本轮验收标准)定义本切片的接受条件、原缺口和完成判据；2026-11-07 已完成该切片逐项验收，结果与验证方法在该文维护。以下文件级索引不能代替逐条判定。

| 规格行为 | 已有测试入口 | 证据边界 |
| --- | --- | --- |
| CLI、证书信任、HTTP 401、两个进程握手与退出 | [process_test.go](../../cmd/channelprobe/process_test.go)、[main_test.go](../../cmd/channelprobe/main_test.go) | 真实回环 TLS 与独立 OS 子进程；没有实际 Docker 节点 |
| 身份、session替换、冻结诊断、消息ID与关闭码、分片超限 | [channel_test.go](../../internal/channel/channel_test.go) | 真实TLS连接；没有恢复扫描或业务事件处理 |
| 15／45秒、迟到Pong、在途预算和清理 | [heartbeat_test.go](../../internal/channel/heartbeat_test.go) | 可控时钟和传输回调，未做真实等待45秒的网络丢包实验 |
| 21正例／11反例、解码包、路径／预算、极端数字 | [validator_test.go](../../internal/protocol/validator_test.go) | 结构及列明的解析约束；不证明所有运行关联或原生Compose兼容 |
| 许可绑定、10秒边界、重复、提交前后取消／失效 | [gate_test.go](../../internal/startgate/gate_test.go) | 内存状态与回调顺序；没有真实授权事务、耐久性或崩溃恢复 |

2026-11-07 新增[通道补验](../../internal/channel/acceptance_test.go)、[CLI补验](../../cmd/channelprobe/acceptance_test.go)、[解析补验](../../internal/protocol/acceptance_test.go)及[Gate补验](../../internal/startgate/acceptance_test.go)，补齐逐类消息、真实握手截止、指纹和输入预算边界、固定诊断与退出、状态竞争；原会话替换用例等待旧处理器清理完毕后再验证新连接。具体对应和证据边界以AC结果表为准。

表中链接便于从规则追到代码与测试，不表示所有操作系统故障都做过注入；CLI的`server_failed`永久Accept故障分支仅静态审阅，限制明确记录在AC02。当前环境不能运行CGO race检查；已有并发行为断言不等于竞态检测通过。构建与复验命令仍在实施记录维护。

下一步将持久许可／事件记录接入任务通道时，需要实现相应网络消息、恢复扫描和结果确认，并据新行为更新本篇的支持矩阵；不能把Schema已有定义直接登记为能力已实现。

## 7. 规则来源与实施选择

本轮实现同时包含既有规格的落实和实施期间作出的技术选择。下表记录两者的依据；工作参数可以依据后续实验调整，数值本身不代表容量或性能结论。

| 具体规则／选择 | 来源与采用理由 | 当前性质 |
| --- | --- | --- |
| 六字段信封、20类消息结构、UUID及编码、文件数量／大小／路径预算 | 既有 [v1 通信规格](agent-protocol.md)与 [Schema](../../contracts/agent-v1.schema.json)；三端沿用同一字段含义 | 实现前已有契约；本轮只落实结构校验及本篇列明的处理器 |
| TLS最低1.2、Upgrade Bearer、10秒应用握手、单有效session、15秒Ping／45秒无Pong期限 | 既有通信规格第2节；身份、关联与存活分别核对 | 实现前已有规则；本轮探活需独立发送和最后Pong计时才能满足该规则 |
| 10秒开始许可、同nonce重发不延时、提交开始前后复核 | 既有通信规格第5节及执行与恢复；固定一次操作的授权边界 | 基本规则先于代码；原许可重放和多请求关联在实现时进一步澄清并回写目标规格 |
| Manager固定一个身份组合，仅命令行配置；回环监听；默认运行30秒；只连接一次 | M0只做可控连接实验，未实现认领、持久身份与重连调度；有界运行便于两进程测试退出 | 实施时的原型范围与默认值；不作为正式Manager的部署限制或产品运行期限 |
| 空capabilities、welcome始终要求核对、握手后只返回冻结诊断 | 当前网络路径没有Compose执行和恢复扫描，也未接入独立账本，不能依据传输连通开放任务 | 为本轮能力边界作出的实现选择；后续完成相应机制后再改变支持矩阵 |
| 独立startgate及Now／CommitStart／Start回调 | 先验证许可竞争、提交顺序和调用次数，使这些规则可重复观察；当前网络和存储不具备完整任务链 | 实施时的实验结构；回调测试不构成真实持久提交或端到端执行证据 |
| 最多4个未完成Ping | 限制依赖库Ping等待产生的在途goroutine；发送周期与无Pong期限独立后，需要单独的资源预算 | 实施时选定的有界工作值，未做容量调优；耗尽按budget故障关闭，不冒充silence超时 |
| 包含hello的1024项消息指纹表 | 对同连接消息ID复用检查设置内存增长上限；满后的实际行为见第3.4节 | 实施时选定的有界工作值；不是吞吐指标，也不是持久业务去重方案 |
| 数字token≤128字节、指数绝对值≤128、精确安全整数 | 审阅和回归复现了极端指数输入触发Schema依赖异常；先设解析预算，再精确检查整数，避免异常或浮点舍入 | 实施中发现问题后增加的限制，已同步目标通信规格；预算值可随受支持输入实验修订 |
| ECMAScript引擎、正则100ms预算、固定依赖版本 | 已有Schema使用Go标准正则不支持的前瞻；采用Schema库官方接入方式，超时拒绝以限制匹配资源 | 实施时的库与资源选择；版本和许可证记录在实施记录及go.mod/go.sum |
| 连接建立、应用握手、执行结果分别表达；重连不清空执行证据 | [Portainer源码对照](../research/portainer-agent-reference.md)提供具体调用链与故障情景，与本项目既有恢复要求共同约束实现 | 参考影响分层与验证场景；本项目字段、许可、时间参数仍按自己的规格，不宣称由Portainer直接提供 |

今后实施时新增会改变可观察行为的参数、状态、错误或支持范围，应在同轮更新本篇的对应规则、来源和验证入口；需要产品裁定且现有需求与参考不能决定的取舍，按 D13 向用户确认。实施记录继续记录执行经过和测试结果，不能单独替代这些行为定义。

## 8. 接手维护时的数据与协作关系

本节于2026-11-07交付后补充，并在同日按“相对独立模块可单独理解”的要求展开；运行行为基线仍同页首，未改写第6节及实施记录中的验收时间或结论。CLI、校验器、连接、探活和Gate合载于本篇：共享消息字段引用[Schema](../../contracts/agent-v1.schema.json)，外部约定继续查第2—5节，以下只定义各单元自己的数据、处理和维护边界。

### 8.1 CLI：把一次有界实验组织成进程

[main.go](../../cmd/channelprobe/main.go)的输入是命令行、token／TLS／可选CA文件、父context及输出Writer；输出是第2节的JSON事件、固定诊断和退出码。`main`只建立`os.Interrupt`信号context，调用`run`，解除信号订阅后退出。`run`依赖`channel`组织网络，不直接解析协议消息、不调用Gate或账本。

| CLI自有数据／资源 | 来源和存续范围 | 处理约束 |
| --- | --- | --- |
| `options` | `parse`将参数写入一次运行的值对象 | 先做通用及模式校验，再读本地材料；另一模式的参数保留但不用，不作运行时热更新 |
| token及TLS材料 | token文件经`ParseToken`规范化；Manager加载证书对；Agent按需建立CA池 | 读取后仅供本次运行；没有身份登记、文件写回或动态轮换，固定诊断不包含原文 |
| duration子context | 参数和token通过后才创建 | 该期限覆盖随后TLS材料处理、连接及运行，未覆盖此前的参数解析和token读取；材料读取本身没有context取消接口 |
| Manager监听器、HTTP服务器、`served` | 监听成功后启动一个Serve goroutine，容量1的channel收其退出结果 | JSON监听事件写出失败也关闭服务器并收取Serve结果；HTTP错误日志丢弃，错误由本程序归类 |
| Agent的Peer | Connect完整成功才交还给CLI | 先写handshake事件，再运行Peer；从得到Peer起以defer关闭，事件写出失败也走清理 |

Manager退出由CLI协调两个资源层：先`handler.Close`关闭WebSocket连接，再对HTTP服务器做最多2秒Shutdown，失败则Close，最后等待Serve返回；handler本身不拥有监听器。Agent在`Peer.Run`返回时用父运行context判断是否属于主动到期／取消，其他断线归为运行失败。因此排查“同样是到期却退出码不同”应先看失败发生在Connect还是Run，不从最后一条日志推断成功。创建新进程是新实验，旧进程没有可供它恢复的CLI状态。

### 8.2 protocol：从不可信字节到受约束信封

[Validator](../../internal/protocol/validator.go)是可独立调用的内存校验单元，输入完整重组的消息字节，成功返回`Envelope`，失败返回零值Envelope和固定错误；不打开网络、文件或事务，也不保存校验历史。`NewValidator`从[contracts嵌入资源](../../contracts/embed.go)取私有副本，编译根消息Schema和`$defs/FrozenPayload`两个规则对象；编译后不改写，可被Server的多个连接共享。Agent每次Connect另建一个Validator，成功后由Peer继续持有；每次Validate的临时解析数据不跨调用保留。Validator无Close或后台任务，无引用后由运行时回收。

| 内部数据 | 用途和边界 |
| --- | --- |
| `message`／`payload` | Validator持有的两份已编译规则；外部加载器始终拒绝资源请求，Schema的`.invalid`标识只作命名空间 |
| `doc`解析树 | 每次校验独有的map／array／标量树；数字以精确规范化的`json.Number`保存，供Schema检查，调用间不共享 |
| `Envelope` | 六字段结构映射到Go；`SessionID`／`ReplyTo`指针保留null，`Body`保留输入中body的原始JSON字节；不会替调用方解出具体任务对象 |
| prepare的`paths`和`total` | 单次解包局部数据：已见路径集合与文件原文字节累计量；用于重复／引用核对及总预算，校验后丢弃，不创建工作区 |

处理顺序必须保留，因为后层的类型断言和资源使用依赖前层已经拒绝的输入：

1. 对外层字节先检查8 MiB，再由`strictJSON`检查UTF-8、单值、重复键和嵌套。数字在交给Schema前经`boundedInteger`先限token／指数预算，再用有理数精确判断整数和安全范围；不能改成float64舍入后检查。
2. 用根Schema检查类型、形状及字段约束；格式断言启用，日期用RFC3339Nano解析验证日历，正则用ECMAScript引擎及匹配时限。只有通过后才将`doc`断言为对象、body断言为对象。
3. `checkEncodedValues`递归检查已过Schema的body里`input_digest`、`payload_b64`和`output_b64`的规范编码及输出预算。这里的递归尚未进入payload内部的环境变量映射；变量名恰好叫`input_digest`不会被当作协议字段。
4. 仅prepare进入`validatePrepare`：解码payload，重新走严格JSON与数字检查，验证FrozenPayload，再核对外内目标一致性；逐文件检查路径、唯一性、规范内容编码、UTF-8和累计字节，最后确认compose/env引用都指向已见文件。文件顺序和原文不被重写；通过也不计算HMAC或调用Compose。
5. 全部通过才从原字节提取信封字段，并将已经验证为v1的`Protocol`赋值为1。这样`protocol:1.0`可按整数语义接受，同时body原文字节保持不变。

第4节六种错误是Validate的数据错误分类；构造阶段另有`bundled_schema_invalid`、`bundled_schema_compile_failed`和`bundled_payload_compile_failed`，分别指嵌入资源解析／注册、根Schema编译、内层Schema编译失败。失败立即返回，不保证同一报文同时含多个错误时一定先报告哪一个字段；特别是对象遍历顺序不作为接口约定。网络层又统一将校验失败映射为`invalid_message`，不能把连接关闭码当作具体文件故障定位结果。

`NewMessage`是另一条出站辅助路径：生成随机message_id、编码body和信封，空session／reply字符串编码为null；它不调用Validate。`NewID`随机源失败返回`message_id_failed`；`NewMessage`的JSON编码失败返回`message_encode_failed`。调用方仍负责出站消息校验及方向／会话约束，不能因为能构造JSON就认为消息可发。

可追踪例子：将prepare外层version_id改为另一合法UUID，保持payload里的version_id不变。它可通过根Schema和Base64检查，但在第4步返回`payload_invalid`，不返回“部分已接受”的Envelope，也不产生磁盘文件；对应[解析补验](../../internal/protocol/acceptance_test.go)的`TestAcceptancePrepareTargetsMatchEnvelope`。数字等价化仅用于检查；若后续模块需要按原始payload/body字节建立持久指纹，不能从`doc`重编码代替原字节。

### 8.3 channel：认证、会话归属和连接寿命

[channel.go](../../internal/channel/channel.go)对外有两组入口：Manager用`NewServer(Config)`取得HTTP handler，并负责承载TLS及调用Close；Agent用`Connect(context, ClientConfig)`取得已完成握手的Peer，再调用Run／Close。Config承载固定node／ledger／token；ClientConfig另有URL、RootCAs和可选ServerName。CLI没有ServerName参数，通常由URL确定服务器名称；包级调用者提供该值时仍执行TLS名称验证。依赖是协议校验器、Go TLS／HTTP及WebSocket库，没有持久仓储。

| 自有数据 | 所有者、用途和生命周期 |
| --- | --- |
| `Server.cfg`／`v` | 构造时检查并保存的固定身份配置及共享校验器，Server生存期间不热替换 |
| `current` | 当前被选中的Peer；Server锁保护，`CurrentSession`只读取其session字符串或空值，不证明对端已经收到welcome |
| `connections` | Server锁保护的已Upgrade连接集合，包含等待hello者；每个handler登记，defer删除，供整体关闭而非消息路由使用 |
| `closed` | Close置位后不可恢复；阻止已进入handler的连接继续登记／成为current，重开需要新Server |
| `Peer` | 一个连接的session、冻结标志、WebSocket、Validator和heartbeat；字段在构造后供运行路径读取，调用方不应改写公开会话字段 |
| `seen` | Manager单handler拥有的`message_id → SHA-256`指纹表，从hello开始；读循环串行使用，随连接结束丢弃，没有跨连接幂等含义 |

`ServeHTTP`先做第3.1节HTTP检查，再Upgrade并登记连接；`readRaw`依次处理WebSocket读取／文本类型／UTF-8／Validate。通过后才核对hello身份、生成session。Server在锁内把新Peer放入current，锁外请求旧连接关闭，然后才写welcome；welcome写失败会清理新current，**不会恢复旧连接**。各handler清理时只在`current == 自己`时清空引用，防止迟到的旧处理器误删新会话。会话替换不等于Gate失效已接通：本路径没有Gate实例。

握手后的Manager回调按“仍为current且报文session匹配 → 方向允许 → 指纹冲突 → 表预算 → 保存指纹 → 写冻结诊断”串行执行；应答写入使用独立10秒context。指纹由已校验的完整信封解码再重编码后求SHA-256，忽略空白及键顺序，仍覆盖type、关联和body。这里重新解码为float64，没有复用Validator的精确数字解析树；1／1.0／1e0会统一，而负零可保留不同于零的编码，不能承诺任意数学等价写法都按同一指纹处理。它与[事件日志](m0-agent-journal.md)按原始body字节识别结果重试不同，不能互换。相同ID、相同指纹在预算未满时会再发一条新message_id的冻结诊断，不是缓存重放原应答，更不是执行幂等。

Agent的Connect先验证本地连接配置，建立专用HTTP transport并禁重定向；Dial到welcome共享握手期限。失败会关闭已经建立的WebSocket，返回前关闭空闲HTTP连接；成功则将活动连接交给Peer。没有连接池复用或自动再拨号。每个Peer只应启动一个Run：它在调用goroutine中持续读消息，在另一个goroutine运行heartbeat；后者结束会取消前者，前者退出则取消、关闭连接并等待heartbeat回收。Agent的接收回调固定拒绝应用消息，Manager用上述冻结回调。库处理Pong依赖持续读取，不能只启动探活而停止读循环。

`Server.Close`只置closed、清空current并关闭所跟踪连接，不等同HTTP Shutdown，也没有等待所有handler返回的接口；CLI负责外层关闭。排查退出残留应同时看等待hello、旧会话关闭、读循环及探活工作者，不能只检查current为空。所有这些内存对象在进程终止后消失，关闭连接没有操作结果可落库。

### 8.4 heartbeat：发送节拍与存活期限分开维护

[heartbeat.go](../../internal/channel/heartbeat.go)是`channel`私有的单连接探活单元。输入为context、WebSocket、库触发的Pong通知；输出为Ping、连接关闭和内部错误。`newHeartbeat`绑定真实时钟、`Conn.Ping`与`CloseNow`；测试可替换时钟／调用边界。一个实例只运行一次Run，握手成功后由Peer启动；Run把当前时刻记为初始lastPong，尚未收到任何Pong时也从此计算45秒。

| 内部数据／资源 | 含义、同步和更新规则 |
| --- | --- |
| `lastPong`与`mu` | 最后收到Pong的时刻，由回调在互斥锁内写入；读取剩余期限也持锁，不以最后一次Ping发送或完成时间替代 |
| `pongs` | 容量1的唤醒channel；Pong回调先更新时刻再非阻塞投递，满时合并通知，不是Pong计数器，真实依据仍是lastPong |
| `inFlight` | 原子计数，发起Ping工作者前加一、其退出时减一；限制尚未退出的调用，不因收到任意Pong就直接扣减 |
| 15秒ticker／45秒timer | Run拥有；前者触发独立发送，后者按最新lastPong重算余量；timer触发也再次核对时刻，避免已收到迟到Pong却按旧期限关闭 |
| `results`／`workers` | Run局部的容量4错误channel及WaitGroup；前者收Ping完成结果，后者保证退出回收；不作为持久诊断历史 |

收到任意Pong只刷新存活时刻；WebSocket库仍按Ping payload匹配具体等待调用，所以“不再静默”与“在途Ping已释放”是两件事。每次ticker先检查context和静默期限，再排空已排队完成结果，遇到错误按传输故障退出，最后检查4个在途预算并启动新工作者。完成成功不会额外刷新lastPong。达到存活期限、传输失败、预算耗尽或context取消均走统一清理：取消子context、关闭连接以解除I/O等待、等待全部工作者、停止ticker和timer。注入的Ping函数也须能响应取消／关闭，否则WaitGroup会等待它；Run没有另一条强制杀死goroutine的路径。

可追踪例子：t=15发Ping，t=29收到迟到Pong，t=30仍按节拍发下一次Ping。t=60触发检查时距最后Pong只有31秒，剩余14秒，直到t=74且没有新Pong才按静默退出；[探活测试](../../internal/channel/heartbeat_test.go)的`TestHeartbeatLatePongRefreshesDeadline`以可控时钟核对这条时间线。另一个`TestHeartbeatInFlightBudgetIsDistinctFromSilence`让Pong持续刷新、四个Ping却仍等待，下一发送时触发预算错误。两例分别定位时间依据与资源占用，不能合并成一个“心跳超时”。Peer目前丢弃探活具体错误并取消读循环，CLI最终分类仍见第3.4节。

### 8.5 startgate：一个请求窗口的许可消费

[gate.go](../../internal/startgate/gate.go)不依赖网络或数据库，输入是已由调用方校验、绑定连接的Request／Permit／Denial及三个Hooks，输出是方法错误、局部State和最多一次Start调用。字段与错误优先级见第5节；此处解释Gate如何保存约束。零值不能使用，必须由New构造，构造后不可复制；它没有Close、后台计时器、持久恢复或更换session的方法。

| Gate自有数据 | 来源、意义及生命周期 |
| --- | --- |
| `request` | New时按值保存首次请求；session／operation／digest／nonce固定为整个Gate的身份，重发不能替换 |
| `deadline`／`hooks.Now` | 构造时以Now加10秒固定期限，后续只比较不延长；真实调用保留`time.Now`的单调时间部分，测试使用可控单调时钟 |
| `requests` | 首次message_id及RegisterRequest成功登记的同窗口重发ID集合；为ReplyTo提供可接受关联，重复登记同ID不新增项；Gate内存释放前不删除 |
| `chosen` | 首次通过Authorize检查时保存的Permit副本，先选定再提交；PermitID／AuthRev锁定后不被后来的许可替换，ReplyTo仍可为另一个已登记请求 |
| `state` | 第5.2节的局部门禁状态；State只读该值，不触发到期检查，也不推导持久开始／执行结果 |
| `disconnected` | Invalidate永久置位的独立连接失效标志；已started时仍保留state，却使旧许可重放被拒绝 |
| `mu`／`hooks` | mutex串行保护上述状态；Hooks由调用方提供，CommitStart锁外运行，期限复核时的Now和最终Start在受保护路径调用 |

调用方必须先登记重发的MessageID，才能接受对它的应答；不能靠Authorize临时加入ReplyTo。New本身既不发送request_start，也不证明请求已发送，故它应在首次发送边界调用。Now必须快速返回且不可重入Gate；Start也不可重入（包括State）或等执行结束，否则会阻塞同一mutex。CommitStart可以回调Cancel／Invalidate；它返回前，Gate已经选定许可并处于committing，并发重复Authorize不会再提交。所有调用方应持有同一`*Gate`，不能复制一个对象来制造第二套独立锁和状态。

Gate取消只改变本地决定，不取消CommitStart外部副作用。外部提交已经发生、但CommitStart尚未返回时，Cancel／Invalidate仍可改变状态；提交返回后若检查发现这些变化或已到期，则不调用Start，但外部持久记录可能已存在，必须由对应存储／恢复模块解释，不能把canceled／expired直接写成“已证明未开始”。最终检查与Start持同一把锁，晚于Start的取消不能回退started事实。Start返回也仅证明回调返回，不能当作进程成功退出。提交错误通过`errors.Join`保留`ErrCommitFailed`和外部错误，调用方以`errors.Is`分类；不得直接将外部错误文本写入线上诊断。

可追踪例子：首次request-1在t=0建Gate，t=9登记同nonce的request-2；t=10收到ReplyTo=request-2的有效许可，pending检查使Gate变expired，CommitStart和Start都不调用。若在期限内进入CommitStart，期间Invalidate，再等提交成功返回，则保留invalidated且不Start，已提交事实交给外部模块处理。这两条分别由[Gate测试](../../internal/startgate/gate_test.go)的`TestRequestRetryDoesNotExtendTenSecondWindow`和`TestInvalidationAndCancellationDoNotWaitForCommit`核对；[补验](../../internal/startgate/acceptance_test.go)另核对外部提交失败时保留期间发生的取消／失效。

Gate到达终态后，调用方可在不再有方法／回调使用时释放引用；重连或uncertain之后重新New一个Gate并不授予同操作重试许可，必须先满足[执行与恢复](execution-recovery.md)的核对条件。独立账本已有[Gate持久提交接入](m0-agent-ledger.md)，但本篇CLI／网络仍未调用它；修改这里不能仅凭该包已存在就广播执行能力或解除冻结。

### 8.6 修改与交接时保持的边界

外部参数、退出与报告查8.1及第2节；接收输入与编码查8.2及第4节；握手、current归属和指纹查8.3及第3节；读循环依赖、计时和回收查8.4；许可窗口、关联和外部副作用查8.5及第5节。每项修改按第6节定位验证，新增线上字段仍先改共享Schema和通信主定义，不在本篇另建一套字段契约。

本次补充依据现行代码和已有测试的具体断言，属于文档交接修订，没有新增软件测试通过结论。尤其不得把受控时钟例子写成真实网络45秒实验，把Gate状态写成数据库结局，或把Server.current／handshake事件写成节点恢复已完成。
