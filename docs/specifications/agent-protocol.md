# Manager—Agent 通信规格 v1

更新日期：2026-11-04。状态：M1 实现与对接的工作规格；部分结构、连接与许可规则已由 [M0 原型](../implementation/m0-agent-channel.md)验证，完整任务通道、持久化及 Docker 故障尚未验证。本篇承接 [Agent 构件](../architecture.md#5-构件视图与逐层展开)、[部署运行交互](../architecture.md#6-运行视图)和 R01、R04、R05、R07、R11—R14、Q02、Q04、Q05、Q07—Q10。沿用 P07—P10 的工作选择，不把新增字段、参数写成用户逐项确认的需求。

实现边界（2026-11-05 补记）：下文是完整任务通道的目标定义，20 类消息有 Schema 不等于全部已有处理器。当前原型的逐消息支持矩阵、实际 hello/welcome、冻结诊断、命令行参数及内存 Gate 状态，以 [M0 实际实现规格](m0-channel-probe.md)对应基线代码核对。

## 1. 职责与规范入口

Manager 负责接受用户意图、授权、冻结输入及形成操作结论；Agent 负责本地输入校验、执行事实和观察。Web 不连接本通道。认证、认领和轮换的 HTTP 入口由[接口规格](../api-design.md)承接；此处只规定已经认证的 WebSocket 会话。

| 内容 | 唯一维护位置 | 使用方法 |
| --- | --- | --- |
| 消息字段、嵌套对象、类型、必填、可空、枚举、形状约束 | [agent-v1.schema.json](../../contracts/agent-v1.schema.json) | 每条消息验证根 schema；解码的 prepare 输入另验证 `$defs/FrozenPayload` |
| 方向、关联、生命周期、幂等、故障与持久化顺序 | 本篇 | 结构校验成功后仍须执行这些检查 |
| 原生 Compose、资源登记、投递与核对处理 | [执行与恢复](execution-recovery.md) | 解释双方如何满足本协议；私有代码结构不属于线上契约 |
| 合法形状与结构反例 | [正例](../../contracts/examples/agent-v1.valid.json)、[反例](../../contracts/examples/agent-v1.invalid.json) | 正例是独立报文，不是已经运行成功的时间线；反例逐项给出拒绝原因 |

所有对象字段默认必须出现；无值只允许 schema 明示的 `null`，空数组不等于缺省。除配置的环境变量映射外，对象拒绝未知字段。schema 的 `$id` 使用 `.invalid` 标识命名空间，验证器从本地加载，不能为校验任意报文自动访问网络。

## 2. 连接、版本与传输

Agent 主动连接 `wss://<manager>/agent/v1/connect`；HTTP Upgrade 使用 `Authorization: Bearer <node_secret>`。身份绑定 node_id；Cookie、URL token、浏览器 Origin 均拒绝。TLS 最低 1.2，验证服务器名称及系统 CA 或配置的 CA，不提供跳过校验选项。HTTP 认证失败不进入应用消息交换。

握手成功后只接收 UTF-8 文本 WebSocket 消息，禁用压缩。一条重组消息上限 8 MiB，包含 JSON 和 Base64 开销；不能只限制每个帧。双方各在 10 秒内完成 hello／welcome，未完成关闭连接且不得接收 prepare。Manager 首次收到且只收到一个 hello；通过身份、ledger_id、能力和结构检查后分配新 session_id，回 welcome。

`protocol=1` 是完整线上契约版本；agent_version、compose_version 只报告软件信息，不决定兼容。双方不支持的主版本拒绝连接。v1 严格拒绝未知字段，因此任何线上新增必填或可选字段、枚举含义变化均不能悄悄部署到旧端；修改需要同步升级双方或建立新的协议版本。仅文字澄清且不改变接受集合和行为可保持 v1。

已登记节点重连必须核对；全新且确认无历史账本的节点允许 `reconcile_required=false`。同节点只保留一个有效 session，替换连接前旧 session 所有未消费许可失效；新连接不能重放旧许可。双方每 15 秒发送 Ping，连续 45 秒未收到 Pong 则断开。Agent 以 1—30 秒指数退避加随机抖动重连；401 停止自动重试，等待修复凭据。探活只说明通道可达。

WebSocket 层的无效 UTF-8、消息过大分别按 1007、1009 关闭；不接受的消息种类、协议版本、身份或字段违反约定按 1008 关闭。Close reason 仅含固定诊断短码，不带配置或凭据。1006 不作为发出的关闭码。[RFC 6455 关闭码](https://www.rfc-editor.org/rfc/rfc6455.html#section-7.4.1)

## 3. 消息信封与共同类型

每条消息恰有 `protocol、type、message_id、session_id、reply_to、body` 六个字段。message_id 是本次传输关联值，业务去重依赖 operation_id／event_seq 等持久键，不能只凭 message_id。重连重发可以生成新 message_id；同一连接中同 message_id 不允许更换 body。

hello 的 session_id 必为 null；welcome 的信封 session_id 与 body.session_id 相同；其余消息使用当前 session。hello 的 reply_to 必为 null。同步应答的 reply_to 指向同连接相应请求；异步 started／result 的 reply_to 为 null。ready 可关联本次 prepare，重连恢复的 ready 可为 null。ack 的 reply_to 指向本次收到的 started 或 result；重报事件只改变信封，不改变已持久事件 body。

| 共用类型 | 精确含义及额外校验 |
| --- | --- |
| Id | 小写 UUIDv4；node_id 是节点身份，stack_id 是管理对象，version_id 是不可变输入版本，operation_id 是一次用户操作，不能互换 |
| Timestamp | UTC RFC3339，至多毫秒；检查有效日历日期。采集时间用于说明事实时点，不替代单调时钟的许可期限 |
| Revision／EventSeq | 安全范围内整数；revision 从 0 起，事件序号从 1 起。Agent event_seq 每操作递增，不能与 Manager revision 比较大小 |
| Config | 原始配置文件包；结构由 schema 定义。逐文件解码≤1 MiB，总原文≤4 MiB，≤128 文件；每路径≤240 UTF-8 字节；全文件原文必须是 UTF-8 |
| ExecutionProfile | docker_endpoint_id 指 Agent 本地预配置端点身份，policy_revision 指执行配置修订号；不包含可由 Web 指定的 socket 或命令 |
| Error | category 决定错误类别，code 是机器诊断短码，message 仅供显示，retry_action 说明下一步；不得靠 message 字符串判断执行结局 |
| ResourceItem | M1 仅容器摘要；resource_id 为完整 64 位小写十六进制 Docker ID。project_name/service_name 是观察到的 Compose 标签，可空；stack_id 仅经本地管理登记确认后填写，标签相同不构成所有权 |
| Observation | 一次完整项目观察的聚合结果；status 与业务健康分开。健康 none 表示没有健康检查，unknown 表示无法判断；image_id 为实际镜像内容标识，可空，不用标签替代 |
| ExecutionResult | 执行事实、观察和受限输出。outcome 与 exit_code 共同表达结局，ownership_status 表达执行后资源归属核对；不把命令正常退出与进程中断混为一谈 |
| RecoveryRecord | 扫描时的持久任务事实与最新结果，不是允许重执行的指令；finalized 表示 Agent 已持久释放本地占用 |

Config 的路径不得重复、越界或经符号链接逃逸；compose_files 与 interpolation_env_files 中每个路径都必须存在于 files，顺序必须保留。环境键不能覆盖执行控制变量，详见 Agent 设计。profiles 限 128 项、显式插值映射限 1024 项是本版解析预算，并非业务容量承诺；预算变更同步 schema 及测试。

所有 Base64 需要严格解码后重新编码相等，以拒绝非规范填充和多种字节表示；`payload_b64` 解码后为完整 UTF-8 JSON，仍拒绝重复 JSON 键。`input_digest` 是 Manager 的 32 字节 HMAC Base64url，Agent 不持有密钥，必须用自己的 K_ledger 对实际 payload 原字节独立建立指纹。外层 node_id/stack_id/version_id 和 payload 必须一致；required_capabilities 必须是 hello.capabilities 的子集，且包含 `compose.apply.v1`、`config.bundle.v1`。

JSON Schema 的字符长度不能代替 UTF-8 字节预算，format 和 contentEncoding 也不能替代运行时日期、解码及正文校验。[字符串长度规则](https://json-schema.org/draft/2020-12/json-schema-validation#section-6.3.1)、[内容编码规则](https://json-schema.org/understanding-json-schema/reference/non_json_data)

数字在进入 Schema 前执行解析资源预算：单个 JSON 数字 token 不超过 128 字节，显式十进制指数绝对值不超过 128；本协议数字必须精确表示安全整数，再由所属字段限制具体范围。`1`、`1.0`、`1e0` 等价，不能用浮点舍入把小数或下溢值当作整数。外层消息与解码 FrozenPayload 采用相同检查；超预算返回固定错误，不暴露原始数据。这是 M0 根据实际极端输入修正的当前工作规格，尚无已发布客户端依赖旧解析行为。

## 4. 消息目录与调用约定

下表 M 表示 Manager、A 表示 Agent。每种 body 的全部字段由 schema 中同名 type 分支定义；本表规定其使用条件。

| type | 方向 | 前提、应答与重复行为 |
| --- | --- | --- |
| hello | A→M | 每连接首消息；报告已持久身份、boot_id、能力。身份不符关闭，不另建节点 |
| welcome | M→A | 回复 hello；分配 session，说明是否必须核对。Agent 安装新 session 前撤销旧 nonce |
| reconcile | M→A | 只在核对阶段；scope、scan_id、cursor、limit 按第 7 节使用，回复 reconcile_reply |
| reconcile_reply | A→M | 返回冻结记录页或明确错误；页收到不等于该节点恢复完成 |
| reconcile_complete | M→A | Manager 已持久核对全扫描及期间变化，引用 scan_id/ledger_id，请求结束冻结 |
| reconcile_completed | A→M | Agent 已完成扫描关闭检查；相同 scan_id 重复 complete 返回同一回执 |
| prepare | M→A | 仅正常可变更会话、目标无冲突、冻结输入匹配；新操作持久登记后准备，完成回 ready；重复只返回既有 ready 或事件事实 |
| ready | A→M | 工作区与原生预检成功，且尚未开始；准备失败发 not_started 的 result，不发 ok=false 的 ready |
| request_start | A→M | ready 后发一次新 nonce，同时开启 10 秒单调窗口；同连接可重发同 nonce，不延长窗口 |
| authorize_start | M→A | 回复 request_start；许可持久提交后发出，绑定操作、摘要、nonce、session、auth_rev、permit_id |
| deny_start | M→A | 回复 request_start；原因如撤权、取消、占用改变；Agent 持久未开始结果并回 result |
| started | A→M | START_COMMITTED 已持久，可能尚未创建进程；Manager 持久保存事件后 ack |
| result | A→M | 先持久保存事件及 body 再发送；Manager 按操作及 event_seq 幂等保存，随后 ack |
| ack | M→A | 只确认 started/result 已持久存储；不授权执行，也不等于释放资源占用 |
| cancel | M→A | 绑定原 operation_id/input_digest；未开始则持久阻止，已经开始则记录请求并按执行策略终止；结果仍经 result 返回。无此账本记录时回复 operation_not_found，不创建缺少目标的假账本 |
| observe | M→A | 只读容器查询；operation_id=null 为一般观察，非空为第 6 节的补观察请求 |
| observation | A→M | 回复 observe，保持 query_id 和 operation_id，分页面返回；失败给 error 而非伪造空项目 |
| finalize | M→A | Manager 已保存可终结结果，要求按该结果版本释放 Agent 本地占用，详见第 6 节 |
| finalized | A→M | Agent 幂等持久记录终态及释放后回复；Manager 持久收到才释放自己的占用 |
| protocol_error | 双向 | 已有有效 session 且原消息关联可识别，但调用状态／游标／查询对象不合法；只报告错误，不改变已有执行事实 |

响应消息不得从相反方向调用；例如 Agent 发送 authorize_start 无论结构是否有效都关闭连接。发送顺序不构成跨数据库原子性；服务重启后的重发只能使用持久记录，不从最新配置重建旧任务。

## 5. 从准备到结果的时序

1. Manager 在同一事务写 accepted、固定 payload、占用和 prepare 投递意图，提交后发送。Agent 验证身份、输入、支持能力，持久写 preparing 和本地占用，再物化固定工作区。
2. 预检成功持久 ready 后回 ready；失败持久 outcome=not_started 的 result。Manager 持久 ready，不据此开始 Docker 动作。
3. Agent 发送 request_start 并记录本地单调起点。Manager 复核当前权限、占用、取消和输入，事务提交 authorized 后发送 authorize_start。撤权和许可竞争以这个事务提交排序。
4. Agent 仅接收当前连接、同 nonce、同输入、10 秒内的许可；持久 START_COMMITTED 后再次检查窗口、连接和取消，再创建进程。重启后不得凭进程不存在补造“从未开始”。
5. started/result 为持久事件。Manager 必须核对 operation_id、input_digest、事件位置和 body 后写入再 ack；相同位置相同 body 重复确认，不同 body 进入核对。较低事件可补入历史，但不能覆盖较新结局。
6. Manager 按结果判据形成操作终态，随后完成 finalize/finalized。最终状态与占用释放状态分别保存；Web 可显示执行完成且释放待确认，不能因此开放同目标冲突变更。

prepare 等待 30 秒未得到 ready 不等于准备失败：同会话最多按 1、2、4、8、16、30 秒退避重投同输入；连接变化先核对。request_start 和许可仅在同一有效 10 秒窗口内使用，超时后转核对，不自动换 nonce 获得新许可。result 未收到 ack 时按相同退避重报；finalize 未收到 finalized 同样重报。普通只读查询 30 秒无应答返回 unavailable，不产生变更。

同一 `(operation_id,session_id,start_nonce)` 已持久授权后，request_start 重发读取同一 permit_id/auth_rev，不重新授权或延长期限；新的应答 reply_to 指向本次收到的请求。Agent 保留该窗口内同 nonce 的全部已发请求 message_id，因此原请求的迟到许可只要仍满足窗口和连接约束，也可正确关联。已提交许可后的撤权或取消按既有取消流程处理，不能把同 nonce 重试改成一份相反的新授权结论。deny_start 仅能改变仍处于对应未开始请求阶段的任务；提交开始记录期间或开始后的迟到否认不得覆盖执行事实。

每节点发送队列候选上限 32 个小消息、一次一个配置包；队列满保留持久投递意图并暂停，不丢弃结果、不无限存留秘密。结果事件必须可从账本重新读取；没有从磁盘成功保存 result 时不能假报完成。

## 6. 执行结局、补观察与释放

| outcome | 必须成立 | 可形成的操作结论 |
| --- | --- | --- |
| not_started | not_started=true，exit_code/interrupt_reason/observation/output 均 null，error 非空；ownership_status=not_checked；有本次存活流程的未启动证据 | 准备失败等为 failed；canceled_before_start 为 canceled |
| normal_exit | not_started=false；exit_code 是 0—255 整数；interrupt_reason=null；observation 非空；ownership_status 为 consistent/drifted/unknown | 观察完整且归属 consistent 后：0 为 succeeded，非 0 为 failed；否则保持核对 |
| interrupted | not_started=false；interrupt_reason 为 canceled/deadline/agent_stopping/signal；error 非空；exit_code 可空或已知整数；ownership_status 只可为 drifted/unknown | 即使 exit_code=0 仍为 reconciliation_required |
| unknown | not_started=false；exit_code=null、interrupt_reason=null、error 非空；ownership_status 只可为 drifted/unknown | reconciliation_required；不能由当前资源快照倒推出原操作结局 |

normal_exit 的非零退出必须带 execution_failed 类错误；正常退出 0 时 error 可空，观察失败独立记在 observation.error。ownership_status=consistent 表示执行器已按[资源登记规则](execution-recovery.md)核对并持久保存证据及候选 post 基线，正式 active 基线只在 finalize 事务中发布；drifted 表示资源身份或关联与执行证据冲突；unknown 表示依据不足。完整观察可以同时伴随 drifted，不允许据此 finalize。`finished_at` 是本次结局或未知判定记录时间，不伪造不可知的进程真实退出时刻。`output_b64=null` 表示没有保存输出；空字符串表示捕获了零字节，两者都不能用来判断是否执行。

新 query_id 的补观察只接受已经持久 normal_exit、观察尚未完整且未发现确凿 drifted 的 operation_id；输入摘要、目标和旧退出事实从原账本取，不从请求提供。一般查询的 operation_id 必须为 null。补观察请求的 stack_id 必须与原任务一致，cursor=null 仅限新查询的首请求。Agent 先检查 query_id 是否已存在：已存在且参数一致的请求按第 7.1 节重放固定快照或翻页，不重新采集、不增加 result event_seq，也不重新要求原操作仍是观察未完整状态。因此首次补观察已经使操作完整后，同 query_id 的有效重试和后续页仍可读取。

只有首次受理新 query_id 才执行一次只读全项目观察，持久生成更大的 result event_seq，保留原 outcome、exit_code、原始输出及原 finished_at，更新 observation，并按原执行前基线重新计算 ownership_status、持久保存核对证据及候选 post 基线，然后发 result。已有 drifted 不能用较新的快照抹除。observation 应答仅表示查询结果，不代替新 result/ack 链路。任何重试均不执行 Compose Apply；新 query_id 必须重新检查首请求的补观察前提。

Observation.complete 仅指目标项目查询全部完成、无缺页和采集错误；不是业务健康结论。结果中的聚合 items 若使整体消息超出 8 MiB，Agent 持久记录 incomplete/budget_exceeded，并只传能容纳的摘要；不得截断后仍宣称 complete。Agent 将 stdout/stderr 增量解码为 UTF-8，无效序列替换为 U+FFFD，按监督器收到的顺序形成固定输出流；不承诺两个管道之间的原始因果顺序。1 MiB 采集限额按这个规范化流的 UTF-8 字节计数，在完整字符边界截断，output_b64 编码同一组持久字节；HTTP 输出分页的字节游标也指向该流。超过限额持续排空原始管道并设置 output_truncated=true。M1 不对超预算项目承诺自动完成核对。

finalize 绑定 operation_id、input_digest、result_event_seq、terminal_state。Agent 只能按自身同一最新持久结果重新算出相同终态后接受；缺结果、未知/中断或版本已变化时 protocol_error/finalize_conflict，保留占用。成功时在单事务写 finalized、final_state、finalized_result_event_seq，按 normal_exit 的已核对候选 post 基线更新 active 归属（not_started 不改归属），并删除对应 local_claims，随后回 finalized。相同请求幂等返回同一事实；不同终态或版本不能覆盖已有释放记录。Manager 同事务保存 finalized 和释放 resource_claims；响应丢失继续保留 Manager 占用并重投 finalize。两端无需原子同时释放，任一端仍持有占用都会阻止冲突任务。

Manager 在发送 finalize 前可存最终 state，但 release_status 必为 pending；收到匹配 finalized 后为 released。ACK、连接关闭、扫描结束、页面超时均不能单独释放占用。尚无 Agent 记录且从未授权的已受理取消，需先通过可信 scope=all 完整扫描证明不存在且新 prepare 已冻结/停投，按[执行与恢复](execution-recovery.md)处理；pending 集合缺席不足以证明不存在，因为它省略已 finalized 历史。不发送伪造结果版本的 finalize，已经授权的未知操作不能使用这个例外。

## 7. 查询分页与账本恢复

### 7.1 一般观察

首次 observe 使用新 query_id、cursor=null。Agent 先完整采集目标容器摘要，固定已采集的集合、排序和 collected_at，再按 resource_id 升序分页面返回，limit 为每页最多条数且≤200。这里固定的是采集结果，不承诺 Docker 在整个采集期间没有外部变化；采集期间对象消失或无法读取时报告 observation_failed。不能边遍历变化的 Docker 列表边声称得到同一快照。后续页使用同 query_id、stack_id、resource_kind、operation_id、limit 及上页 next_cursor；改变查询条件报 cursor_invalid。

游标是 Agent 保存快照的随机不透明句柄，绑定本节点、查询及位置；空末页只能用于空集合。快照从创建起保存 5 分钟，Agent 重启失效；过期返回 error=cursor_expired、items=[]、next_cursor=null，Manager 丢弃未完成页并以新 query_id 重查。每页 error=null、最后 next_cursor=null且收齐全部页才可标记完整；某页失败不能把已经收到的部分当作完整快照。磁盘或内存预算不足时返回 unavailable/busy，不创建无限快照缓存。

### 7.2 恢复扫描

首次 reconcile 使用 scan_id=null、cursor=null、scope=pending 或 all；limit 1—200。Agent 冻结新 prepare 及未开始任务的开始许可，持久保留冻结状态；确定按 operation_id 排序的身份集合及 total，并为本轮冻结这些记录的枚举快照。pending 包含未 finalized 的任务及尚未确认事件；all 包括已 finalized、已 ack 历史，供旧 Manager 备份恢复。同一 scan_id 的 scope 不得改变。

需要从 pending 升级为全量核对时，Manager 可在当前有效会话发送新的 reconcile，使用 scan_id=null、cursor=null、scope=all。Agent 原子替代活动 pending 扫描并生成新 scan_id，冻结必须连续保持；旧扫描的游标和 reconcile_complete 立即失效，不能用旧扫描结束新扫描。新扫描重新冻结完整身份集合及记录快照，并继承旧扫描尚未确认的变化事件，按新扫描要求确认这些事件已在当前连接得到 ack。Manager 丢弃旧扫描未完成的分页，按新的 total 从首页核对，无需等待旧扫描的 15 分钟期限。

reconcile_reply 每页 records 按数量及总编码大小双重切分：不得超过请求 limit、200 项或 8 MiB；单条记录也必须满足结果预算。total 是本轮固定身份总数，complete=true 仅表示枚举到末页且 next_cursor=null。Manager 持久保存页和已见身份，检查重复、跳页、数量、node/ledger绑定；不能只信 complete。错误页 records=[]、complete=false、error 非空，不能被计入完成。

每个 RecoveryRecord 的 operation_id/input_digest/目标与 latest_result 必须一致；result_event_seq 等于 latest_result.event_seq；无 result 时两者均 null。last_event_seq 不小于任何已存事件。phase=finalized 时 claims_held=false，final_state 与 finalized_result_event_seq 非空，指向真实已终结结果；其他状态不得用 report_acked=true 推断已经释放。

原已开始进程可在扫描时结束。其新 started/result 继续持久上报；Agent 为本次扫描跟踪冻结后发生变化的事件，在当前连接得到 ack 前，不接受 reconcile_complete。未开始任务的取消可以产生 not_started 结果，但不能进入新的 START_COMMITTED。Manager 完成全部页后还要处理期间新增结果、核对旧授权和占用、保存缺失历史的 recovery_cases，并按结论完成 finalize。证据未知的情况仍隔离，不能以“全部页面收到”代替核对。

确认可以结束时 Manager 发 reconcile_complete；Agent 核验扫描已枚举到尾、ledger匹配且变化事件已经得到当前连接 ack，持久保存扫描完成记录、解除扫描冻结，再回 reconcile_completed。Manager 收到并持久保存后才开放新的受理。complete 丢响应时原 scan_id 重发，Agent 从持久完成记录回同一 completed；不能因为 Agent 重启后缓存不在就丢失“已经结束”事实。

活动扫描从创建起有效 15 分钟；相同 Agent boot 内断线可按原 scan_id、cursor 续页，期间变化事件需在新连接重新取得 ack。过期、Agent 重启或完整扫描缓存丢失返回 scan_expired，保持冻结并重新从首页扫描。扫描与观察缓存的到期时间使用 Agent 单调时钟。持久完成扫描记录保留到下次已完成扫描取代，以保证结束回执可恢复；旧完成记录仅可重放其回执，不能覆盖当前活动扫描。

## 8. 错误与非结构校验

Error.code 是可扩展的小写短码；接收者必须能按 category/retry_action 处理未知 code，不把未知代码当成功。M1 下列代码有固定使用语义；补充诊断代码不得改变相同 category 的副作用含义。

| code | category／处理 |
| --- | --- |
| input_invalid、state_conflict | invalid_input／修改输入或查询原操作；不新建业务意图自动重试 |
| permission_denied | permission_denied／拒绝新的开始许可 |
| operation_not_found | not_found／核对身份与账本，不视为从未执行证明 |
| input_conflict、resource_drift、finalize_conflict | conflict／保留证据与占用；需核对 |
| capability_unsupported、config_unsupported | unsupported／不降级执行 |
| canceled_before_start | execution_failed／仅用于已证明未开始的取消；最终操作为 canceled |
| permit_expired | unavailable／已证明未开始且许可过期；最终操作为 failed |
| precheck_failed | invalid_input／输入预检失败；若失败实为不支持能力或存储故障，分别使用对应的其他 code/category |
| process_failed | execution_failed／正常非零退出，保留已发生副作用 |
| process_interrupted、result_unknown | reconciliation_required／不能重跑原操作 |
| observation_failed、budget_exceeded | unavailable／观察不完整；仅允许补观察 |
| cursor_invalid、cursor_expired、scan_expired | invalid_input 或 unavailable／丢弃本轮不完整分页，按第 7 节重开 |
| busy、storage_error | unavailable／拒绝当前未提交动作；已提交未知事实保持核对 |

协议适配器按以下顺序检查：传输长度与UTF-8 → 拒绝重复JSON键 → schema → 方向及session → reply_to/持久身份关联 → 解码payload及嵌套schema → 字节预算与跨字段关系 → 当前允许状态 → 持久提交 → 副作用或应答。schema 不能证明权限、持久化、事件重复一致性、实际进程结局或资源归属。

身份不符、同一事件位置不同body、已finalized后出现矛盾结果均保留诊断并隔离对应节点／操作；不得用 protocol_error 清空已有证据。错误消息不能包含原始输入、环境值、口令、token、私有宿主机路径或任意 Docker inspect 正文。

## 9. 验证与参考使用

本轮可以验证 schema 自身、每种消息正例和结构反例、引用以及本篇与上下文的一致性；这些不证明真实网络、数据库与 Docker 的组合已经可靠。执行前仍需完成测试计划 V01—V11，增加补观察后的新result版本、finalized丢失、扫描结束回执丢失、扫描期间结果更新和恢复页超字节预算的故障断言。

实际采用的资料与用途如下；本协议的字段、上限和超时均是项目工作选择，不来自参考项目的配置值：

- [arc42 构件视图](https://docs.arc42.org/section-5/)、[运行视图](https://docs.arc42.org/section-6/)、[共同概念](https://docs.arc42.org/section-8/)：用于从 Manager／Agent 接口职责进入消息交互，并把共用类型与场景分开维护。
- [JSON Schema 2020-12 Core](https://json-schema.org/draft/2020-12/json-schema-core#section-8.2.4)、[Validation](https://json-schema.org/draft/2020-12/json-schema-validation)：用于 `$defs` 引用、封闭对象、必填字段、条件分支和字符长度；不会把结构验证夸大为协议验证。
- [Helichrysum Manifest 规范](D:/Project/Helichrysum/docs/manifest-schema.md)、[插件 API](D:/Project/Helichrysum/docs/plugin-api.md)：完整阅读其定义、责任、版本和现状区分方式；借鉴“准确类型／语义／实现状态分开”的表达，不采用其数据库 PRAGMA、删除重扫或插件方案。
