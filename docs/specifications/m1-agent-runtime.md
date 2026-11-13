# M1 Agent真实运行、任务与Compose实施规格

现行阅读提示（2026-11-12，实施后追加）：保留下文事前基线；v5账本及正式服务已有本机实现，审阅／恢复修订见§4及文末，逐项结果在[AR记录](../implementation/m1-agent-runtime.md)和[AS记录](../implementation/m1-agent-service.md)。真实运行入口见[接入说明](../agent-first-run.md)，生产Docker接受表仍为空，不能把本机通过读成真实部署完成。

2026-11-12编码前基线。状态：待实施及接线复核；本篇不是已有Agent的安装说明。用户已确认M1首个完整流程，Linux两节点的操作方式稍后说明；本轮继续本地实现，不连接或配置这些节点。本篇的内部API与版本选择为实施决定，不表述为用户逐项确认。

## 1. 范围、依据和可并行边界

目标是`agent init/enroll/serve`产生真实持久身份，Agent主动TLS WebSocket接入正式Manager，同一个固定版本完成准备、原生Compose执行、资源观察、结果重报与双端释放。涵盖取消、进程关闭和可证明的恢复；真正unknown仍隔离，解除隔离依赖[O12](../decisions.md#3-未决事项和解决时点)。不实现Copy、迁移、自动接管、卷删除、远程Docker、任意shell、终端或全平台兼容。

共同定义依次引用[Agent协议](agent-protocol.md)、[Schema](../../contracts/agent-v1.schema.json)、[执行与恢复](execution-recovery.md)、[Agent组件](../components/agent.md)、[运行配置及恢复](../operations.md)、[Manager服务](m1-manager-service.md)。准备支持矩阵以执行与恢复§2为主，本篇补齐实际程序、API、事务和数据生命周期。范围及退出条件由[首版记录](../implementation/m1-first-version.md)承接。

采用现有SQLite、协议校验器、startgate和coder/websocket；保留channelprobe作为冻结通信实验。研究材料中Portainer的连接分层、状态重报值得复用，但其隧道和内嵌Compose调用不等于本项目的主动任务通道／进程事实，见[源码研究](../research/portainer-agent-reference.md)。不另建第二个Manager操作库，不把Fixture改名充当生产服务。

| 独立模块／拟实现位置 | 输入→输出及所有权 | 可并行工作及接线条件 |
| --- | --- | --- |
| `internal/agentwire` | 已通过TLS的socket→经共享Schema校验的消息；控制方向、大小、探活及关联；不执行SQL | 连接实现可与账本和工作区并行，Manager和Agent使用同一传输边界 |
| `internal/manager/agenttransport` | 正式凭据／hello→现有Store认证绑定；入站事实→Store提交；Store意图→发送 | 根负责；全部授权、占用、结果与恢复判定仍在`operations.Store` |
| `internal/agent/ledger` v5 | 固定收件、准备证据、监督结局及归属候选→持久事件、扫描和终结回执 | 账本负责人独占现有ledger生产文件；不得由runtime直接SQL |
| `internal/agent/workspace`、`compose`、`executor` | 冻结原字节→私有工作区→只读预检→受监督命令→资源证据 | 与账本按§4接口并行；不直接发送消息、不释放claims |
| `internal/agent/service`及`cmd/agent` | 配置／身份／信号→连接和单操作调度器生命周期 | 汇总以上模块，唯一允许调用真实执行器的路径是已持久开始门禁 |

内核负责数据真相，适配器负责可信边界，监督器负责进程事实。进程锁覆盖同一状态根，单节点一个网络读循环、一个写循环；每操作的准备／开始／取消串行。不同项目可拥有独立操作，但M1先限制一个正在准备或启动的Compose任务，已接收任务继续持有claims并排队；不以排队超时伪造失败。

### 1.1 代码核对及必须先修正的真实缺口

| 基线源码 | 实际能力／限制 | 本轮改变 |
| --- | --- | --- |
| `ledger/records.go:RecordReadyFixture`、v4 DDL | 直接写ready；无preparing、原payload、工作区或预检；旧boot重复收件被拒绝 | 正式收件先保存preparing、原文和双claims；成功预检后才ready |
| `ledger/results.go`、`recovery.go`、`events.go` | Fixture信任调用者给出的result；首结果后禁止新结果，最多两事件；新结果必须原boot／prepared session | 生产API按进程证据生成result，断线执行可提交；重开未开始取消及正常退出补观察有单独条件 |
| Agent v4 `execution_events`、`finalizations` | 每kind唯一，finalize结果序号≤2；无project_bindings | v5允许一个started和多个受约束result，保存归属证据并在finalize事务发布 |
| `operations/events.go:eventPosition`、Manager v4 DDL | not_started只能seq1，其他result只能seq2，多表序号≤2 | Manager也需显式新运行格式；不能仅放开Agent序号 |
| `operations/recovery_business.go:ProcessRecovery` | 未授权ready/preparing分支调用RequestCancelFixture；该Fixture拒绝v4 | 正式恢复事务复核round／session／未授权状态后调用内部cancelTx，不经过Fixture |
| `operations` v4 outbox | 只有pending/confirmed/suppressed，无发送尝试关联与waiting调度字段 | 新运行格式补持久投递元数据；发送成功不确认业务 |
| `internal/channel/channel.go` | 固定单凭据、随机端点、空能力、永久冻结，Peer不暴露业务流 | 新正式适配器复用已验证传输规则，不解除探针冻结冒充完整Agent |

版本选择：Agent使用显式`CreateRuntime/OpenRuntime`、schema v5；Manager使用显式`CreateRuntimeService/OpenRuntimeService`、schema v5，继续同一个operations.Store。保留v1—v4入口和测试，不原地重写旧格式或静默迁移。尚未发布产品，可用明确的新状态目录开展M1；已有实验身份及unknown不能通过重建同身份空库绕过。实现Manager格式前由根同步服务／运行规格，避免本篇形成第二份Manager字段主定义。协议线上保持v1：现有Schema已允许preparing和递增event_seq，本轮不新增线上字段。

## 2. 身份与服务程序

### 2.1 入口、配置、持有资源

拟交付命令为`agent init --config <绝对路径> --node-id <Manager已登记UUID>`、`agent enroll --config <绝对路径> --token-stdin`、`agent serve --config <绝对路径>`、`agent rotate-credentials --config <绝对路径>`。认领token只从受限标准输入读取一行，不接受命令行秘密；长期秘密永不输出。init不联网，enroll不执行Docker，serve只打开已有身份／库。帮助及版本信息不读取密钥。

配置沿[operations§10](../operations.md#10-m0m1-运行配置工作定义)，严格UTF-8 JSON、最大64KiB；未知／重复键、null、错类型、相对路径及尾随内容拒绝。环境变量不覆盖配置。manager_origin只能HTTPS origin，同origin派生enroll、rotate和connect；拒绝重定向，附加CA加入系统CA，TLS≥1.2且始终校验主机名。docker_cli为绝对可执行路径、docker_socket仅同机unix；Windows可以编译及运行受控测试，不对其开放真实Compose执行能力。

所有命令取得`state_dir/.agent.lock`操作系统排他锁；Linux flock，进程退出自动释放，文件保留。禁止通过删锁文件、PID探测或不同锁路径绕开同库实例。私有目录0700、身份／钥匙／账本0600；拒绝链接、目录逃逸、非服务账号可写的状态根及关键路径。Windows本地测试的mode位不当作ACL验证。

| 持久材料 | 来源、约束与生命周期 |
| --- | --- |
| identity_file | format_version、node_id、ledger_id、schema_version=5、ledger_key_id、journal_key_id、manager_origin、credential_state、active_secret、candidate_secret、credential_generation。私有JSON；secret为独立32字节随机数的规范Base64url；credential_state为candidate／active／rotation_pending |
| keys_dir/ledger.key及journal.key | 两份独立32字节随机密钥；原字节长度严格，独占写、同步文件和目录。前者指纹，后者AEAD保护本地payload／结果／归属；错钥不替换 |
| state_dir/agent.db | ledger_id及执行真相；v5 schema／完整性／密钥检查；WAL/FULL和现有立即事务约束保留 |
| runtime_profile | ledger内端点UUID、policy_revision、配置指纹、首次核实的Docker Engine ID、CLI／Compose版本证据。socket字符串不单独证明端点身份 |
| workspaces及operations目录 | §5的不可变原文字节和包外运行材料；可被容器挂载，M1不自动删除 |

init先确认所有目标不存在，再独占写两钥、创建v5库并取得ledger_id、持久候选identity。任一步失败保留材料，serve拒绝不完整集合，init不覆盖；该未联网状态只能由操作者检查后选全新目录重做。候选身份在任何认领请求前耐久保存。身份文件更新用同目录临时文件、Sync、原子rename、目录Sync；不能在成功响应后才第一次保存候选。

### 2.2 认领、轮换、重启与关闭

enroll先用已保存候选尝试完成认证hello/welcome，若成功则原子提升为active并关闭连接；这用于认领响应丢失恢复。明确401且没有已确认active时，才使用输入的一次性token调用`POST /agent/v1/enroll`，字段沿HTTP主定义；网络错误保持候选，不改ledger／secret、不盲目生成新节点。响应必须匹配node/ledger且generation有效后发布active；重复同token／同候选由Manager幂等处理。完成认证不等于完成扫描，也不开放任务。

轮换先写rotation_pending（保留active和新candidate），再调用既定rotate路由。响应丢失时下次先试candidate；成功即提升；仅candidate明确401后试旧active，旧active仍可认证才重发同候选轮换。两者均拒绝则停下保留证据。serve遇401停止自动重试；不会退回认领或重装身份。轮换成功使旧session失效并重新扫描。

serve取得锁→读身份／钥匙→OpenRuntime冻结→检查工作区、数据库可写及CLI能力→确认runtime_profile→生成本次boot并握手→恢复扫描→正常收件。能力不足仍可连接并回报空能力、重报已存事实，不能把Docker不可达说成空资源。首次端点绑定保存真实Engine ID；后续ID变化保持隔离。相同端点的CLI、执行配置改变递增policy_revision并使旧ready失效，存在未核清任务时不通过修改配置避开旧端点责任。

SIGINT／SIGTERM首先停止新收件、使未消费Gate失效，再中断已知活动进程组（§6），等待结果耐久保存及发送尽力完成，最后停止连接、关Store、清密钥并释放锁。普通网络断线不停止已启动命令。服务关闭预算至少30秒，其中进程TERM宽限10秒、结果及只读观察余量；预算到期无法得到可靠结果时保持unknown，不强写normal_exit。不得持Gate锁等待进程退出或调用Store.Close。

## 3. 主动通道及Manager接线

### 3.1 共同传输规则

`agentwire`负责coder/websocket适配，不复制身份存储。接口建议：`NewPeer(conn, side, sessionID) *Peer`，`Read(ctx) (raw []byte, envelope protocol.Envelope, err error)`，`Write(ctx, raw []byte) error`，`Close(code, diagnostic)`；构造只能由已完成握手的客户端／服务端使用。侧别是Manager或Agent，严格检查消息方向。握手函数将真实Identity/Profile/Capabilities传入，禁止内部生成随机ledger或端点。

保持现有8MiB完整消息上限、文本／UTF-8／重复JSON键／Schema检查，压缩关闭，hello/welcome总窗口10秒，Ping15秒／Pong45秒；1—30秒指数退避带0—20%抖动，稳定连接30秒后重置退避。每节点写队列最多32个小消息且一次一个配置包；队列满只延迟从持久意图读取，不无限保存秘密。读循环不得被Compose等待或网络写阻塞。每次消息body原字节独立保存，事件重发只换信封。

关联表只保留尚待回应的请求及窗口内重试ID。ACK必须指向本连接已发started/result且operation/seq相符，不能仅凭body调用AcknowledgeEvent。相同message_id不同body拒绝；为避免无限历史增长，每连接最多65536个已见ID，达到上限主动重连并核对，不能遗忘旧ID后允许换body。错误只暴露固定短码／ID，不含配置、输出、密钥及Docker inspect正文。

### 3.2 Manager正式Store边界

认证继续用`AuthenticateNodeCredential`；`BindAuthenticatedNodeSession`、`RecordNodeSeen`、`DisconnectNodeSession`、`ReceiveReady`已存在。新`BindAgentHello(ctx, auth *AuthenticatedNode, rawHello []byte) (Binding, welcome []byte, error)`在Store内核对凭据身份与hello node/ledger、生成session并复用绑定事务；外部不能从hello自行声明已认证Binding。welcome固定reconcile_required=true，即使空新节点也执行一次空扫描，减少可变更入口。

绑定后adapter只把这个Binding交给现有`StartRecovery`、`ReceiveRecoveryPage`、`NextRecoveryRequest`、`ProcessRecovery`、`PrepareRecoveryComplete`、`ReceiveRecoveryCompleted`、`AuthorizeStart`、`ReceiveEvent`、`PrepareFinalize`、`ReceiveFinalized`。每个事务继续核对当前session和Manager boot；撤销／轮换须在同Store事务清空session并冻结，旧连接即使尚未关闭也不能写入或获得许可。网络层不缓存授权结论。

需要补齐的最小生产API（由Manager负责人在服务规格维护其实际类型）：

```go
BindAgentHello(ctx context.Context, auth *AuthenticatedNode, rawHello []byte) (Binding, []byte, error)
NextNodeDelivery(ctx context.Context, binding Binding, now time.Time) (*Delivery, error)
RecordDeliveryFailure(ctx context.Context, binding Binding, messageID, code string, now time.Time) error
```

`Delivery`只有`MessageID、Kind、OperationID、Generation、Raw`，Raw由Store从被冻结输入和当前session构造。NextNodeDelivery同事务复核当前绑定、恢复状态、意图及目标，登记waiting／attempt／next_attempt／last_session／message_id后返回；null表示暂时无工作。无网络IO在事务内。读原payload不会从最新版本重建旧操作。Prepare只在正常会话投递；恢复期间只允许cancel/finalize和显式只读observe。finalize须复用已有PrepareFinalize的当前恢复轮次及确认MAC逻辑，不能另造释放消息。

业务证据提交才把对应意图confirmed：ready/started/result确认prepare，result确认cancel，finalized确认finalize；重试等待1、2、4、8、16、30秒封顶。waiting重开保留并暂停，先恢复再重新排队。authorize_start不入通用outbox，仍只回答当前nonce的请求；发送失败不新建许可。响应ACK只在ReceiveEvent成功返回后发出。`ProcessRecovery`的生产取消改用受round保护的内部事务，不能使用拒绝v4的RequestCancelFixture。

### 3.3 恢复驱动循环

认证绑定→StartRecovery(pending,200)→持久接收页／NextRecoveryRequest→ProcessRecovery。need_all时新round执行all，不在旧scan修改scope；waiting时继续重报Agent事件及发送已持久cancel/finalize；blocked保留隔离；ready才PrepareRecoveryComplete。Agent完成回执和Manager ReceiveRecoveryCompleted提交后，Manager才开放受理。正常重启不恢复旧授权窗口，也不自动继续旧preparing/ready的up。

接收started/result、finalized或扫描页后唤醒同节点worker重新推进，不用轮询触发重复业务。2026-11-12传输接线前细化：用1秒调度检查补丢失内存通知并检查持久next_attempt，避免将1秒重试统一拖到5秒；实际发送仍以Store到期状态为准，不因tick重执行业务。Manager收到扫描内最新result后不能省略当前连接变化事件ACK；Agent继续PendingEvents重报原body，收ACK再允许complete。未知操作、损坏账本、已授权但缺失历史仍按现有recovery_case隔离。

断线先让Agent未消费Gate失效，再绑定新session；同boot正在执行的监督器可保存结果，它的事实不依赖prepared_session仍为当前连接。重开后preparing/ready只能在当前恢复会话收到cancel时写not_started（无START_COMMITTED和spawn记录），不重新准备或重新请求许可；旧START_COMMITTED没有可靠结局则unknown，保留双claims。

## 4. Agent v5执行账本小程序

### 4.1 数据与正式API

v5沿既有密钥校验、AEAD AAD绑定、额度预留不退还、WAL/FULL、当前boot写屏障、扫描冻结及双端释放算法，新增以下私有数据。不把共享FrozenPayload、ExecutionResult或RecoveryRecord再定义一遍。

| 数据／关键字段 | 含义、来源及约束 |
| --- | --- |
| executions.phase | preparing／ready／START_COMMITTED；由收件、预检和Gate事务推进；result/finalized仍由事件与回执投影 |
| execution_inputs | operation_id唯一；payload密文／key_id／fingerprint，收到的原payload字节，不先重排JSON。生命周期与操作同长 |
| preparation_evidence | operation_id唯一；workspace相对标识、manifest指纹、profile指纹、CLI版本、pre-baseline及ownership_revision；只有完整预检才写入，敏感证据加密 |
| process_facts | operation_id唯一；owner_boot、spawn_state（not_attempted／created／exited）、本次进程组诊断、取消原因、受监督wait结局。开始事务预置not_attempted，实际Start返回后记录created；这段崩溃空隙只能unknown，PID仅诊断，不能跨boot认领或作为结局 |
| execution_events/event_state | 每操作严格递增seq，唯一started只能seq1；result可追加，已有body不可改；latest_result_seq指向最新，必须存在完整可核验事件链 |
| project_bindings | endpoint/project唯一→stack；字段与baseline条目沿[执行归属主定义](execution-recovery.md#3-资源归属与合法替换)。原active不能被普通观察改写 |
| ownership_candidates | operation_id/result_seq唯一；pre revision、完整post基线、合法替换映射、采集时点、核对状态；与result同事务加密保存，finalize校验后才发布 |
| observation_queries | query_id及目标／limit／operation／创建boot／产生的result_seq；重复query不二次采集；固定页面缓存5分钟，重启失效返回cursor_expired |

正式API以执行数据命名，而不是接收任意成功JSON。拟定接口由ledger包持有：

```go
CreateRuntime(ctx context.Context, path, nodeID, keyID string, key []byte, journal JournalKey, options RuntimeOptions) (*Store, error)
OpenRuntime(ctx context.Context, path string, identity Identity, key []byte, journal JournalKey, options RuntimeOptions) (*Store, error)
BindManagerSession(ctx context.Context, welcome []byte, helloMessageID string) error
DisconnectManagerSession(ctx context.Context, sessionID string) error
ReceivePrepare(ctx context.Context, raw []byte) (PrepareReceipt, error)
ReadExecutionInput(ctx context.Context, operationID string) ([]byte, error)
CommitPrepared(ctx context.Context, operationID string, evidence PreparationEvidence) (Record, error)
RecordPreparationFailure(ctx context.Context, operationID string, failure Failure) (Event, error)
CancelExecution(ctx context.Context, rawCancel []byte) (CancelDecision, error)
RecordProcessExit(ctx context.Context, operationID string, fact ProcessOutcome) error
RecordProcessOutcome(ctx context.Context, operationID string, fact ProcessOutcome, observation ObservationEvidence) (Event, error)
RecordObservationUpdate(ctx context.Context, operationID, queryID string, evidence ObservationEvidence) (Event, error)
PendingEventRefs(ctx context.Context, sessionID string, limit int) ([]EventRef, error)
BootID() string
```

`RuntimeOptions`包含时钟和执行profile校验参数；创建时只生成持久endpoint UUID，首次serve核实Engine后补齐绑定，未核实时无变更能力。禁止将测试任意结果提供器放进生产options。`PrepareReceipt`含Record及Disposition（new／preparing／ready／reported／finalized）而非允许执行的bool。PreparationEvidence包含上述持久准备字段；Failure只含既定Error和本机未启动证明。ProcessOutcome含监督器的outcome、exit_code、interrupt_reason、finished_at、固定输出及截断标记；ObservationEvidence含协议Observation、私有pre/post归属证据和候选。Store分配event_seq、核对输入／boot／阶段和合法转换，组装协议body；外部不得指定seq或terminal_state。RecordProcessExit在观察前独立持久化不可变进程事实；随后RecordProcessOutcome须与之相同，重开也只能基于已存事实补只读观察。BootID返回Store已生成的本次boot，hello不另造第二个boot。PendingEventRefs只返回operation_id/seq/kind元数据、limit为1—32；发送者逐条ReadEvent，避免一次把所有敏感历史加载进内存。

CancelDecision返回not_started已持久事件或interrupt_required，后者由调度器对当前已知进程组处理；没有记录返回operation_not_found。生产调用继续复用NewGate，但v5门禁须验证preparation_evidence及profile，START_COMMITTED同事务保存started seq1和process_facts(not_attempted)。BindManagerSession仅由校验TLS／welcome关联后的客户端调用，Store复核node、hello关联、当前身份和冻结；它不从任意session字符串授予执行。

### 4.2 事务、幂等与当前存活证明

收件事务保存preparing＋payload＋event_state＋两个local_claims（stack、endpoint/project）；提交后才允许物化。同operation同Manager digest和本地原字节指纹只返回已有进度；任一不同隔离，不能覆盖。CommitPrepared比较原输入／manifest／profile／基线修订和claims后写ready；准备失败生成not_started seq1仍保留claims，等待finalize。

Gate提交及最终检查之间禁止长IO。启动前工作区／目标复核先完成，再进入Gate的短Start回调；回调在内存登记本次尝试，直接调用受控`Cmd.Start`、注册监督器后立即返回，created事实随即由监督器保存。回调不得先做另一次SQLite写入、工作区扫描或Docker查询再启动，以免绕过最终10秒检查。许可提交后仍须检查连接、10秒单调窗口和取消；不得把创建goroutine当作已完成最后检查，稍后在失效窗口启动进程。资源复核和Docker动作之间无原子锁，外部写入竞态按已知漂移处理。

现行执行规格允许START_COMMITTED后、本次存活进程证明没有spawn的not_started；v5具体为started seq1后result seq2，只有同owner_boot、门禁停止路径未曾进入Start／或Cmd.Start明确失败，且本次监督器保存的事实一致时成立。该证据写入与结果同事务；重开时not_attempted字段本身不能推断尚未执行。磁盘提交失败或提交返回不确定时不启动、不伪造not_started，保持核对。该细化必须同步Manager事件位置／恢复检查和相应验收，不能沿用“not_started必seq1”。

正常退出先持久进程结局（包括原输出、finished_at）再采集资源，避免观察进程失败丢失已知退出。最终result与归属候选同事务保存；仅观察未完整且没有drift的normal_exit允许RecordObservationUpdate追加seq，原outcome、exit、finished_at、输出逐字节保持。不能由新的query改变原进程事实或把interrupted变normal_exit。query_id先登记，重复不再采集；采集完成／新增result与query关联同事务提交，崩溃后返回过期／继续重报既有结果，不默默重复采集。

Finalize沿已有API增加原子归属发布：验证最新可终结result、无冲突、候选pre revision仍匹配→发布active新基线／revision+1→保存finalized→删除恰好两个claims→提交后回复。not_started不发布新基线；首次准备创建的provisional绑定可保留给同Stack复用，但不能成为已管理资源凭据。任何失败保留claims；ACK不发布基线，重复finalize只读原回执。旧结果和迟到started可补历史但不覆盖新结果；已释放后矛盾事件重新隔离。

2026-11-12本地实施后核对：v5实现见`runtime_store.go`、`runtime_prepare.go`、`runtime_results.go`及`runtime_ownership.go`；仅本机受控账本验证，正式服务与真实Docker仍待组合验收。锁顺序沿现有“不得持Store.mu等待SQLite、不得持事务等待Gate”的约束。调度器的操作串行区与Store事务是两个边界：任何真实进程开始都须同时通过当前Gate和持久事务。扫描冻结允许现存进程结局、未开始取消和只读补观察，但不允许新prepare／新开始。

### 4.3 2026-11-12 v5账本编码前接口细化

`runtime_types.go`为适配器共享的Go模型。`RuntimeOptions.Profile`固定本进程核实的endpoint、policy revision、配置指纹、Engine ID和CLI版本；Engine ID为空仅允许恢复／只读，不能ready。`PreparationEvidence`绑定operation及本地原payload指纹，保存相对WorkspaceID、manifest指纹、profile／Engine／CLI、pre基线、ownership revision及原生模型的ExpectedResource逻辑键（容器可用service覆盖service/number实例，网络／卷用配置逻辑键）；Store核对原输入／当前profile／active基线，首次项目必须空。`ResourceRecord`仅保留kind、真实ref、逻辑键、classification、created_at、project、service、container_number、driver，不存inspect全文。外部依赖不进入managed基线。`ObservationEvidence`以原协议Observation字节＋post集合＋替换映射＋实际冲突资源为输入，Store重新计算归属，不接收trusted或consistent布尔。

新增正式只读方法`ReadProcessOutcome(ctx, operationID string) (ProcessOutcome,error)`和`ReadPreparationEvidence(ctx, operationID string) (PreparationEvidence,error)`，用于重开后读取已知退出／准备事实，不附着旧PID。新增正式方法`RuntimeProfile(ctx) (RuntimeProfile,error)`、`BindRuntimeProfile(ctx, RuntimeProfile) error`、`RecordProcessCreated(ctx, operationID string, processID int) error`、`RecordStartFailure(ctx, operationID string, Failure) (Event,error)`、`BeginObservationQuery(ctx, operationID,queryID string,limit int) (ObservationQuery,error)`和`ReadProjectBinding(ctx,endpointID,projectName string) (ProjectBinding,error)`。processID只诊断；created只能在本次Gate回调真实进入后、owner_boot相同且尚无结局时保存。Gate封装在内存登记entered，开始提交成功、开始门禁失效或Start明确失败的存活证据只在本Store有效；内存另外记住提交成功／失败，提交返回不确定不能因随后取消而生成未启动结果；重开不从not_attempted推断not_started。取消请求先记录，已created返回interrupt_required，随后监督结局必须interrupted。原normal_exit结局可在重开后用于只读补观察，但不能恢复执行。

Failure的Code用于固定错误，Message／Retryable不进入公开错误或日志；网络错误、原生输出不能填充普通错误消息。ProcessOutcome由监督器提供，服务补充本Store BootID；RecordProcessCreated在Gate短回调返回后、开始Wait结果持久化之前调用。未知／中断可以不带Observation，归属为unknown；normal_exit必须带完整或明确失败的Observation。

辅助证据统一存`runtime_documents`密文表，主键(operation_id,kind,seq)，使用既有journal独立额度预留及AAD绑定，kind隔离input/preparation/process/candidate。profile及绑定持元数据，baseline密文引用本操作candidate，首次provisional为空。归属发布在现有Finalize事务内复核pre revision并写active引用，再保存finalized和删除两个claims。`observation_queries`在采集前登记，boot／5分钟过期后拒绝重复采集；同query重放既有seq。本切片尚未提供Observation页面快照及分页接口，须由Compose/service组合实现，不能将只保存query元数据记作AR13分页通过。序号校验以Store版本分派，v1—v4仍最多两个事件，v5完整验证递增历史。

### 4.4 2026-11-12独立审阅后的账本修订基线（修复前保存）

第一轮独立复核发现两处真实偏差：`ReadProjectBinding`只认证consistent候选，未认证finalized发布，破坏active指针后仍可能把未释放候选读成active；`CancelExecution`在核对input_digest前调用Gate.Cancel，错误请求虽返回冲突却已阻止合法许可。受影响为AR04／AR05／AR11／AR15；原本地测试记录不撤销，但不能当作这两个边界已通过。

发布读边界修订：候选密文只证明资源解释，不证明发布。带baseline指针的active／drifted绑定必须在同一数据库快照认证目标操作的同seq finalized回执、最新result、正常可终结结局及claims为空，再匹配候选pre revision+1、Stack、endpoint和project；无指针只能为revision0的provisional／drifted，provisional不能带已发布指针。损坏回执、未finalize候选、其他对象／序号／修订的指针返回ErrEvidence。Finalize仍先校验原active再在同一事务更新新指针、写finalized并释放claims；读路径不得要求尚在事务中形成的当前发布中间态已经对外可读。

取消顺序修订：先只读事务复核本Store身份／boot写权、当前session、operation原输入digest及事件／claims，关闭该事务后才等待Gate.Cancel；随后正式事务再次复核相同谓词并保存取消事实／结果。连接替换与取消通过既有recovery串行锁协调，保持“无SQLite事务等待Gate、无Store.mu等待SQLite”。错误digest、旧session、缺operation或预校验失败不改变Gate、事件或claims。已通过校验的真实cancel可先关闭Gate，后续存储失败时保持停止、原claims及未知／待重报状态，绝不重开许可；同一有效取消重试在存储恢复后收敛。开始已经进入时只返回实际interrupt_required，不伪造未启动；开始提交不确定继续隔离。

§4.4实施中追加（扩展负例RED后、修复前）：仅认证目标finalized仍不足以检测“指针＋revision同时回退到旧合法发布”，也不能检测无真实漂移证据的status改写。读取时从同endpoint/project的认证candidate＋finalized历史重建连续、无分叉的已发布修订头，与当前指针相符；status由是否已有发布及当前未释放操作的认证drifted候选派生。active/provisional/drifted元数据不能自行授予或撤销归属。当前采用同一快照按该项目历史核对，避免增加新Schema；历史读取成本随该项目已发布操作数增加，优化不能牺牲完整认证链。受影响仍为AR11／AR15，不扩展自动接管行为。

## 5. 安全工作区与原生预检小程序

输入为已登记operation的原payload，不接受任意本机路径。`workspace.Materialize(ctx, input) (Package, error)`发布固定包；`workspace.Verify(ctx, package) error`用于ready前和开始前；`compose.Prepare(ctx, package, profile) (PreparationEvidence, error)`仅只读；`compose.Invocation(package, profile) (executor.Command, error)`构造同一固定命令上下文。Package持有stack/version、绝对package目录、清单／原文指纹及有序文件列表，不持有可变“最新配置”指针。

路径布局沿组件：`workspaces/{stack_id}/{version_id}/package/`原文，`operations/{operation_id}/`包外manifest、受控HOME、DOCKER_CONFIG和空env。staging与目标同文件系统，逐层不跟随链接，文件O_EXCL写入及Sync、父目录Sync后rename发布；不能只先检查字符串后跟随被替换的链接。Linux使用目录句柄及不跟随链接的相对open，固定root由私有服务账号持有；主机root恶意改写不在隔离保证内。已有目标必须比对完整清单、类型和原字节，包括额外文件；差异workspace_drift，不覆盖。

静态解析复用成熟YAML AST库（实施候选`go.yaml.in/yaml/v3 v3.0.6`），保留节点源位置；先遍历AST拒绝重复／非字符串键、循环alias、自定义tag、超过深度64和展开100000节点，再进行受预算的alias／merge展开。不要用文本正则解析YAML，不重新生成供CLI使用的YAML。完整模型合并／插值仍由原生`config --format json`执行；该命令负责规范化，但不能替代调用前文件边界检查。[YAML Node接口](https://pkg.go.dev/go.yaml.in/yaml/v3)、[原生config](https://docs.docker.com/reference/cli/docker/compose/config/)

未知属性拒绝采用固定Compose schema结构与本项目读文件入口拒绝表共同校验；复用`github.com/compose-spec/compose-go/v2 v2.14.0`的`schema.Validate(map[string]interface{})`离线校验，记录许可证，不调用会读取文件／解析环境的loader。锁定版本只是结构校验依赖，不等于其所有属性已允许；Schema通过不能放开include/provider/hooks等拒绝项，也不能取代AST来源检查。[Compose schema包](https://pkg.go.dev/github.com/compose-spec/compose-go/v2/schema)

支持与拒绝按执行规格§2逐项落实，覆盖未启用profile和被后续覆盖的定义。env_file、label_file、configs/secrets file都只能读取包内普通文件；路径含`$`／`~`／远程URL拒绝，错误带源文件和字段而无秘密内容。插值env文件用上述同版本`dotenv.ParseWithLookup`只消费包内字节、检查键并处理export、多行和注释，不调用Load／Read等隐式文件或进程环境入口；lookup只供无秘密的键检查，返回值不用于运行变量或改变原文件。拒绝DOCKER_*／COMPOSE_*／PATH／HOME及其他固定控制键；不能只按每行第一个等号扫描而漏掉多行。容器env_file不当作主机控制环境。[dotenv解析接口](https://pkg.go.dev/github.com/compose-spec/compose-go/v2/dotenv)

原生前缀固定绝对docker_cli、`--host <socket> compose --ansi never --project-name <project> --project-directory <package>`，按原顺序添加env files、`-f`和profiles；显式环境与包内`.env`或包外空env防止机器环境污染。HOME／DOCKER_CONFIG由Agent管理，PATH/locale固定，不继承父进程全环境。预检和up使用相同目录、文件、profile和环境。build原文保留，有image且不会读取未支持构建外部材料才允许；缺image或pull_policy=build拒绝。M1不提供私有仓库登录管理，拉取失败作为原生执行错误。

预检按渲染模型检查bind／volume／tmpfs，不手工按冒号拆挂载；包内相对只读普通文件允许，相对目录／可写包内挂载拒绝；原文显式绝对宿主路径作为外部应用依赖，Agent不读写或复制其内容。external网络／卷必须存在，用户指定container_name冲突和项目归属在ready前及up前核对。预检超时30秒、模型输出上限8MiB、stderr受限不写普通日志；超时／超限均不写ready。最终调用仅`up --detach --no-build`，不附加隐含重建、清孤儿或删除卷参数。[原生up](https://docs.docker.com/reference/cli/docker/compose/up/)

## 6. 进程监督、观察和归属小程序

### 6.1 受监督命令与取消

`executor.Command`包含绝对Program、Args数组、Dir、显式Env、执行期限；不接受shell字符串。`executor.Start(ctx, command) (Process, error)`仅在Gate短回调调用，ctx由服务操作生命周期提供，不使用socket／HTTP请求context。Process仅持当前boot真实子进程／进程组；Wait返回ProcessOutcome，Interrupt(reason)在记录取消后发送TERM、10秒后KILL并Wait回收。Linux创建独立进程组；其他平台真实执行明确unsupported。

将stdout/stderr持续并行排空，以采集器接收顺序合成固定流；增量UTF-8解码，不在读取块边界误替换完整字符。转换后最多1MiB且只在完整字符边界截断，超限继续排空。普通日志不包含命令输出；持久进程结局及result正文均加密。执行期限默认1800秒、配置范围1—86400；取消、deadline、服务关闭和实际信号都形成interrupted，即使Wait返回exit0也不能normal_exit。Wait无法得到可信状态则unknown。Docker仍可能完成已提交请求，TERM/KILL从不表示回滚。

重开不附着旧PID、不杀可能复用的PID、不重新up；保留旧进程证据、claims和workspace，进入reconciliation_required。正常网络断线则本boot监督器继续等候并保存结果，只暂停许可及新动作。所有进程／文件描述符必须在服务退出前被回收或明确留下未知证据，不能在Store关闭后异步写结果。

### 6.2 完整观察及归属

M1只读Docker能力复用固定CLI：按项目label列出全部容器（包括停止）、网络、卷，再按真实ID inspect；输出模型只提取协议容器摘要及私有归属必需字段，不把完整Env等inspect材料写日志。每次命令有30秒总查询期限和8MiB输出预算；对象在列举／inspect中消失、任一步失败、资源过多或关联矛盾都不是空集合。公开Observation按Schema生成，健康none与unknown分别表示无健康检查与无法判断。

首次部署要求项目资源为空且预期非external网络／卷名称无人占用，才能创建provisional绑定。更新比较active基线：容器／网络ID、服务及容器序号、网络／卷逻辑键，volume使用名称+driver+创建信息弱指纹；相同项目label只定位候选，不证明所有权。正常自身重建的旧→新ID在原已授权预期逻辑键下可形成候选；无已知外部竞争且采集完整才consistent。非零正常退出同样可能形成可解释的部分变更基线，但操作仍failed。保留旧孤儿容器，不隐式remove-orphans。

查询不改active基线。完整观察且归属未知／漂移、执行interrupted／unknown仍held；正常退出但观察不完整可发绑定原操作的observe进行补观察，允许更大seq但不再次up。补观察query缓存5分钟，页≤200；同query改条件拒绝、重启／过期返回cursor_expired。聚合预算不够就incomplete/budget_exceeded，不能截断后标complete。归属算法、字段、external排除及finalize发布的主定义继续在执行与恢复§3维护。

## 7. 支持、诊断与交付限制

首轮平台限定Linux、本地可靠文件系统、同机Engine Unix socket、原生Docker CLI＋Compose插件。候选解析依赖版本与现有Go依赖必须锁入go.mod/go.sum；本篇不是版本兼容承诺。CLI接受矩阵由实际记录的精确版本及§5拒绝用例确定，新版本不能因更高就自动开放。尚未拿到Linux环境时允许构建真实程序及使用明确测试替身验证命令参数／事务／崩溃路径，真实Compose矩阵仍未验证；测试替身不作为生产配置选项。

| 现象／维护入口 | 排查数据与不得破坏的规则 |
| --- | --- |
| 认领或启动失败 | identity候选／active、ledger_id、两钥和schema；不删库换身份补救 |
| 连上但不能部署 | capabilities、当前session、pending/all round及completed提交；不能因Pong把frozen设false |
| 准备拒绝 | 原payload及指纹、路径／AST位置、profile、CLI模型及pre基线；未知引用须在第一次CLI之前拒绝 |
| 有started无进程结果 | owner_boot、Gate许可、process_facts、存储故障；PID缺失不构造not_started |
| result已ACK仍占用 | 最新seq、候选归属revision、finalize关联和双方finalized；禁止直接删claims |
| 更新被判漂移 | active基线、pre/post真实ID及本操作替换映射；普通查询不能刷新active掩盖变化 |
| 修改事件／恢复 | 检查双方序号限制、扫描投影、变化ACK、旧结果迟到和最终释放；只改一端将无法收敛 |

源码交接必须包括实际二进制构建、配置说明、模块测试证据及[AR01—AR16记录](../implementation/m1-agent-runtime.md)。原生对照、两节点隔离、端点中断、O12维护入口、管理／Agent一致备份和文档独立运行按首版V01—V08继续验收；本模块不能以本地通过宣布首版完成或提供安装发布承诺。

### M1服务observe范围补充（2026-11-12）

2026-11-12 AS-R01修复前追加AR10／AR15：新增正式只读`PendingProcessOutcomeRefs(ctx,limit)([]string,error)`，limit为1—32。它在同一快照列举尚无result、未隔离／finalized、且有认证耐久process结局的operation ID，并核对原boot、started事件及双claims；损坏证据报错，不降为没有退出事实。返回只含ID，不附着旧PID、不启动命令、不改变恢复会话。服务逐项再次ReadProcessOutcome和RecordProcessOutcome，两个事务之间发生的正常结果竞争由原幂等边界收束。没有结果的已知正常退出先可形成明确“观察未完成”的结果，保留原退出码／时间／输出，再通过既有补观察完成；未知旧启动仍没有可恢复退出，继续隔离。枚举不要求在线session，因此断线及重开仍可补齐，所有变更继续由写方法验证当前boot写权。每批失败后停止，不能静默漏项或循环占用控制队列。

同日独立复验后的第二次修订（编码前）：以上可信退出准入不能只放在枚举入口。直接observe可调用ReadProcessOutcome，读后也可能发生竞争；因此枚举、公开读取和最终result事务必须共用密文／原boot／process_facts.owner_boot及exited认证。尚无结果时还核对started1与双claims；已有结果不要求已释放claims重新出现，但仍认证原进程结局与元数据一致。旧密文存在本身不足以允许首结果或补观察，损坏owner／spawn状态不能在周期恢复发现前从另一路径生成可终结结果。公开读取不改原结果；最终事务重新验证，不能依赖之前读取的内存对象授权落库。

CW04接线前细化：公开ResourceItem.stack_id可为null；未知、冲突或尚未确认的新候选不得仅凭project label回填。Compose观察使用账本认证读取的PreparationEvidence，只为具有既有归属修订且完整身份匹配可信Pre的资源关联Stack。账本仍核对真实资源ID、项目、服务和私有基线；null不取消这些验证，非null且不等本操作Stack必须拒绝。

正式服务本轮仅将绑定operation、normal_exit且原观察不完整的observe用于补观察及固定页面快照。operation_id=null或不符合补观察条件的普通查询明确返回observation_unsupported/unavailable，不采集、不新增result、不改变active归属。普通容器浏览留到M2；现行HTTP结果页读取持久结果不依赖此查询。对应AS08／AR13的验收只覆盖这一实现子集，不能将普通查询拒绝算作资源查询完成。

## 8. Agent v5 封闭快照检查器（2026-11-13实施前基线）

本节承接[维护备份§7.3](m1-maintenance-backup.md#73-v5完整备份的只读检查切片)。仅实现完整备份所需的只读内部认证，不新增节点动作、健康检查、恢复、v6、protocol2或操作者审批。调查资料只作实现依据；实际范围及稳定AI条件在本节和[实施记录](../implementation/m1-agent-runtime.md)确定。

入口为`InspectRuntimeSnapshot(ctx,path,expected Identity,key []byte,journal JournalKey)(SnapshotInspection,error)`。报告字段固定为Identity、SchemaVersion=5、ProtocolVersion=1、EncryptionKeyID、EncryptionKeyUses、TableRows（17表，零行保留）、SealedRows（execution_events/finalizations/runtime_documents）、SemanticFormat=`dm-agent-runtime-semantic-v1`及SemanticFingerprint。任何失败返回零报告，错误只含固定分类，不输出原文、SQL、驱动、路径、逐行摘要或key_check。超过资源预算为ErrInspectionLimit；取消和期限保留errors.Is。

输入仅为上层生成或清单绑定的、封闭私有单文件副本；旁文件-wal/-shm/-journal一律拒绝。共享`internal/snapshotcheck.Open`负责本地路径、私有权限、单文件、mode=ro/immutable/query_only、五分钟期限及单读事务；Finish关闭连接后再核对源字节和旁文件，Close负责中途清理。在线源仍由非immutable的Snapshot复制，不能直接传活库绕过WAL。封闭来源由调用方保证；同用户恶意修改/ABA不在保证内。检查器不调用正常Create/Open/newStore、不生成boot或更新session/profile/恢复状态；包内临时只读Store只复制认证钥匙、Validator和当前持久boot，并显式传入同一tx。

验证顺序：精确005_runtime.sql及user_version、完整integrity_check和foreign_key_check；五个singleton、身份/两钥key_check及quota；全部17表长度预检和固定主键顺序遍历；所有密文按包内唯一eventBinding/openEvent认证；逐operation核对文档/事件/claims；历史准备/候选/发布链；扫描捕获、变化及查询关联。单正文16MiB、普通行32MiB，保留协议7MiB事件/8MiB信封及1MiB进程输出的更小限额。遍历游标关闭后才嵌套读取；密文先检查长度再Scan，不积存全库原文。任何未被业务指针消费的密文仍须枚举认证并判定其合法所有者。

input0的真实明文是JSON编码的[]byte，解码出原payload，以固定合法验证信封复用prepare/FrozenPayload/Config校验，核对execution的node/stack/version/project/endpoint/policy，并重算K_ledger对原payload的HMAC。Agent没有Manager的digest钥匙，只核对input_digest绑定一致性。preparation/process/candidate采用现有类型与确定性JSON形状验证；事件始终保留原body字节。input/preparation/process仅seq0，candidate必须对应同seq非not_started结果；非started阶段不能有process_facts/process。ready需要preparation；START_COMMITTED需要started1、原permit/时间和process_facts。not_attempted/created无process结果是合法未知窗口；exited必须有认证process0，但首result可以尚未形成。started后的可信not_started2允许没有process0/candidate。结果序号连续，latest指向最后结果；后续结果只补normal_exit的不完整观察，原退出/时间/输出不变。

历史preparation不能调用比较当前profile/head的写准入validatePrepared：验证原operation/input/workspace/manifest、Pre基线与Expected，旧policy保留原profile版本。同一policy的准备tuple一致，当前policy与当前profile一致，Engine/endpoint不替换。pre.OwnershipRevision=0时managed Pre为空；>0时查同项目/Stack该已发布修订的认证Post，与原Pre比较。consistent候选按历史Pre重放逻辑资源、替换、公开容器关联及完整Post规则；unknown/drifted分支允许现有writer保存的空/部分Post，不能补造未保存的Conflicts。

项目发布从全部真实finalized+consistent候选重建revision1..P，逐修订唯一且连续，当前指针必须等于最大修订，拒绝指针与revision同时回退；not_started终结不发布。status按发布链和当前未释放操作最新drifted候选派生。初版可逐项目、逐期望修订重复有界扫描，固定临时内存、最坏O(P²)，超时不返回部分证明。finalized必须可信releaseTerminal及零claims，其余恰好原project/stack两claims。合法quarantine/blocked允许备份保留，不授予运行许可；冲突正文未保存，只验证保留的结构/引用及隔离关系。

扫描成员按捕获的phase/last/result/ACK、不可变execution字段和保留的原result字节重建原RecoveryRecord，再验证scanFingerprint；不能拿今日投影代替历史。核对active/completed两组、ordinal/排序/计数、事件前缀及finalized关系；旧完成扫描不要求包含后来新增任务。member的HMAC不含scan_id/ordinal，未保存的整组来源真实性不能补证。旧boot/过期/未完成observation_queries保留，非空result_seq必须指向该operation的真实补观察结果；每个后续结果有唯一query。recovery_changes引用真实事件，冻结之前或建扫描之前的合法变化不丢弃。

语义根使用共享NewFingerprint，以K_ledger派生检查钥，稳定身份顺序node/ledger/ledger-keyid，schema5/protocol1。固定17表顺序、列定义、二进制主键顺序、原SQL类型/NULL/整数及表尾计数参与；密文组替换为认证后的原plain及原body_fingerprint。排除KEK ID/key_check/uses和cipher/wrapped_dek，其他元数据（含旧boot/session/ACK/冲突/扫描指纹）保留。单KEK仍必须认证、uses>=现存sealed行数且不超额度，失败reservation允许uses更大。根只用于同schema、保留同K_ledger的新KEK保真比较，不作为外部签名或跨版本恢复证明。

实施后接手定位（2026-11-13）：`snapshot.go`拥有共享Reader/钥匙/报告的完整生命周期；`snapshot_rows.go`按精确DDL提取列/主键，只保留一行密文，关闭游标后认证并流式摘要；`snapshot_semantics.go`核对操作、准备、过程及结果；`snapshot_ownership.go`验证历史修订和当前头；`snapshot_scan.go`重建原扫描并核对历史query。现有正常Store只将validateSchema提取为收querier的同算法私有函数，正常writer及启动行为没有改动。新增密文列/表必须更新完整枚举和本规格；变更私有文档形状、JSON编码或扫描投影顺序须同时核对shape检查、旧捕获HMAC和语义格式，不可只修改writer。

当前证据边界：ledger.CommitPrepared只保证ManifestFingerprint为合法digest，真实workspace清单由Compose适配器产生；检查器认证原准备文档及此值的规范形状，不额外读取工作区或强制从payload重算另一份manifest来拒绝现行合法Store调用。数据库不能重算外部服务配置形成的旧profile指纹、Manager的input_digest或未保存的竞争正文/Conflicts；这些值和原文仍完整受认证及聚合摘要保护。超大历史最坏O(P²)的性能、Linux运行及关闭错误专门故障注入未由Windows普通通过替代。
