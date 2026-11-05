# M0 通信原型实际实现规格

更新日期：2026-11-05。实现基线：`98d9c13`。本文按该提交的代码描述当前程序的输入、输出、交互、状态和异常处理；后续实现改变这些行为时同步更新本文。

[Agent v1 通信规格](agent-protocol.md)定义拟实现的完整任务协议，[Schema](../../contracts/agent-v1.schema.json)定义消息结构，本文明确原型实际落实到哪里。[实施记录](../implementation/m0-agent-channel.md)承载构建、运行、测试执行和修复记录；源码参考依据另见 [Portainer 对照](../research/portainer-agent-reference.md)。

## 1. 实现组成与边界

| 实现 | 当前职责 | 接入情况 |
| --- | --- | --- |
| [channelprobe](../../cmd/channelprobe/main.go) | 同一命令按 manager／agent 模式运行两个进程 | 使用真实 TLS WebSocket；Manager 一次配置一个 node_id／ledger_id／token 组合 |
| [channel](../../internal/channel/channel.go) | Upgrade 认证、hello/welcome、会话替换、消息读取与冻结诊断 | 已接入命令行；握手成功后始终保持 `reconcile_required=true` |
| [protocol](../../internal/protocol/validator.go) | 20 类消息的结构及部分解码约束校验 | 已用于通道读入、Agent 发出 hello 前的检查；可在测试中单独验证 prepare |
| [startgate](../../internal/startgate/gate.go) | 一个操作、一个 nonce 的内存开始许可门禁 | 仅由独立测试调用；尚无网络消息到 Gate 的适配器 |

当前程序没有任务授权服务、执行器、配置版本仓储或恢复账本。`START_COMMITTED`、持久事件、资源占用和 finalize 握手仍属于目标规格；Gate 中的提交回调不表示这些存储已经实现。

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
| 消息 ID 一致性 | Manager 从 hello 开始记录消息指纹；同 ID 内容改变则关闭。指纹基于整个信封的 JSON 规范化结果，忽略空白、键顺序和等价整数表示差异；不只比较 body |
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

| 规格行为 | 已有测试入口 | 证据边界 |
| --- | --- | --- |
| CLI、证书信任、HTTP 401、两个进程握手与退出 | [process_test.go](../../cmd/channelprobe/process_test.go)、[main_test.go](../../cmd/channelprobe/main_test.go) | 真实回环 TLS 与独立 OS 子进程；没有实际 Docker 节点 |
| 身份、session替换、冻结诊断、消息ID与关闭码、分片超限 | [channel_test.go](../../internal/channel/channel_test.go) | 真实TLS连接；没有恢复扫描或业务事件处理 |
| 15／45秒、迟到Pong、在途预算和清理 | [heartbeat_test.go](../../internal/channel/heartbeat_test.go) | 可控时钟和传输回调，未做真实等待45秒的网络丢包实验 |
| 21正例／11反例、解码包、路径／预算、极端数字 | [validator_test.go](../../internal/protocol/validator_test.go) | 结构及列明的解析约束；不证明所有运行关联或原生Compose兼容 |
| 许可绑定、10秒边界、重复、提交前后取消／失效 | [gate_test.go](../../internal/startgate/gate_test.go) | 内存状态与回调顺序；没有真实授权事务、耐久性或崩溃恢复 |

表中链接便于从规则追到代码与测试，不表示每个参数取值、输出错误或预算边界都有单独用例。当前环境不能运行CGO race检查；已有并发行为断言不等于竞态检测通过。构建与复验命令仍在实施记录维护。

下一步将持久许可／事件记录接入任务通道时，需要实现相应网络消息、恢复扫描和结果确认，并据新行为更新本篇的支持矩阵；不能把Schema已有定义直接登记为能力已实现。
