# M0 Agent 开始账本实施规格

日期：2026-11-07。起点：`d53de6d`。本规格在本模块编码前形成，落实用户“每个模块都需要实施规格等文档流程”的要求。状态：实施基线，验收结果由[执行计划与记录](../implementation/m0-agent-ledger.md)维护。

## 1. 目标、依据与本次边界

交付对象是下一轮 Agent 实现者。本轮给已有内存许可门禁提供真实 SQLite 开始提交边界，证明固定输入、占用和开始事实不会因重复许可或普通进程重启变成第二次启动机会。可观察终点是受控 Start 回调至多一次，回调前完整开始事实已提交；重开库后旧操作只能查询，不能恢复执行。

依据：[Agent 设计](../components/agent.md)第 9 节、[数据设计](../data-design.md)第 7—11 节、[执行与恢复](execution-recovery.md)、[Agent v1 协议](agent-protocol.md)、[现有 Gate 实际行为](m0-channel-probe.md)第 5 节。已有协议和 Schema 继续作为主定义。本规格的存储布局、函数名与驱动版本属于实现者落实既有要求的技术选择，未逐项冒充用户决定。

本轮实现 `internal/agent/ledger`，使用真实文件数据库及测试辅助进程；复用 `internal/startgate`。准备阶段只提供名称明确的 `RecordReadyFixture` 受控夹具入口：它验证现有 prepare 报文并登记元数据，不物化文件、不运行预检，也不发送 ready。此入口不得用于真实部署处理器。没有 Manager 授权事务、执行事件、结果正文、ack/finalize、恢复扫描、解除隔离、网络任务处理或 Docker 调用。通道继续冻结且不增加能力广播。

选择先完成开始账本而后接网络，是因为当前缺少结果及核对记录，直接开放任务会让旧任务能否重做无法判定。SQLite 延续 P09；不另造数据库架构，不将整个 M0 缩减为本模块。

## 2. 依赖与数据库打开规则

Go 1.27.0、CGO_ENABLED=0；固定 `modernc.org/sqlite v1.61.1` 和其要求的 `modernc.org/libc v1.79.0`。驱动最低 Go 1.26，内嵌 SQLite 3.54.1；运行时必须查询并验证版本。驱动 BSD-3-Clause，SQLite public domain，其他传递依赖随 go.mod/go.sum 固定；正式发布前仍需复核全部许可及上游支持状态。[固定版本说明](https://pkg.go.dev/modernc.org/sqlite@v1.61.1)、[go.mod](https://raw.githubusercontent.com/modernc-org/sqlite/v1.61.1/go.mod)、[许可证](https://raw.githubusercontent.com/modernc-org/sqlite/v1.61.1/LICENSE)。选用纯 Go 驱动以适配当前无 CGO 的环境；mattn 驱动需要 CGO/GCC，未采用。[安装要求](https://github.com/mattn/go-sqlite3#installation)

数据库只接受本地文件路径；代码以 `net/url` 生成 file URI，拒绝内存库及调用者注入 DSN。Create 以排他新建创建文件，权限请求 0600，父目录由调用者准备；Windows 实际访问控制由宿主目录 ACL 决定，不以 Unix mode 宣称已隔离。失败保留文件，不自动删库重建。Open 使用 mode=rw，不存在即失败；调用者必须提供预期 node_id、ledger_id、key_id 与恰好 32 字节 K_ledger。

每个物理连接使用 WAL、synchronous=FULL、foreign_keys=ON、busy_timeout=5000，并读回校验；连接池最大打开／空闲连接均为 1。写事务统一 `sql.Tx`，以驱动 `_txlock=immediate` 在开始时取得写锁；不自行跨池拼 BEGIN/COMMIT。超时与事务失败不自动重试，不再次执行 Start。[驱动参数](https://pkg.go.dev/modernc.org/sqlite@v1.61.1#Driver.Open)、[SQLite 事务](https://sqlite.org/lang_transaction.html)、[WAL](https://sqlite.org/wal.html)

首次 schema_version=1，DDL 内嵌在包内 `migrations/001_start_ledger.sql`。Create 在一事务中安装表和元数据；Open 只接受版本1、匹配元数据及与内嵌迁移一致的表／索引定义。比较前统一CRLF为LF并移除外围空白和末分号，避免Git换行转换使同版库无法打开；不宽泛重写SQL。忽略SQLite内部对象，其他额外表、索引、视图或触发器均视为未识别schema；不能仅凭版本号和列名接受缺失约束的库。缺失、损坏或较新版本均拒绝，不升级、不降级、不补空身份。当前没有历史schema升级，也没有备份恢复工具。

## 3. 固定数据与存储约束

稳定执行上下文为 `(node_id, ledger_id, operation_id, stack_id, version_id, input_digest)`；node/ledger 由单库身份确定。session 只绑定本次准备和开始，不能代替稳定身份。原始 payload_bytes 由现有 Validator 校验后解码，从同一原文提取 project_name、docker_endpoint_id 和 policy_revision；不重新序列化后计算指纹。已通过Schema的数学整数写法1、1.0、1e0必须提取为相同整数；其原始字节仍不同，同操作改写会触发本地指纹冲突。时间物理列采用Unix毫秒INTEGER，Record查询结果再转UTC文本。

| 表 | 本轮字段与约束 | 事务语义 |
| --- | --- | --- |
| agent_meta | id=1 单行主键；node_id、ledger_id、schema_version=1、ledger_key_id、key_check | Create 生成随机 ledger_id；调用者提供有效小写 UUIDv4 node_id 和非空且至多128字节 key_id |
| executions | operation_id 主键；stack_id、version_id；input_digest、local_fingerprint 为32字节BLOB；project_name、docker_endpoint_id、policy_revision；prepared_session、owner_boot_id；phase；session_id、start_nonce、permit_id、auth_rev、reply_to；created_at、started_at | ready 与 START_COMMITTED 两种 phase；开始字段含 started_at 要么全空，要么全齐，且仅 START_COMMITTED 可非空；revision 为0至9007199254740991 |
| local_claims | `(resource_kind,resource_id)` 主键；operation_id 外键指向 executions，禁止级联删除 | kind 为 stack 或 project；stack 键为 stack_id；project 键为 JSON 数组 `[docker_endpoint_id,project_name]` 的紧凑编码；与执行记录同事务插入 |

必要字段 NOT NULL，BLOB 长度、阶段、成组许可、数字范围、主键和外键由 DDL 约束；UUID 与 prepare 的完整内容沿用应用层 Schema 验证。开始字段第一次提交后没有覆盖或清空方法。project 不只用显示名作键，避免不同端点混淆。登记第二个占用失败时整个新操作回滚，不能留下半条记录。

key_check = HMAC-SHA-256(K_ledger, `docker-manager/ledger-key/v1` + 单字节0x00 + JSON编码的 `[node_id,ledger_id,key_id]`)；Open 用恒定时间比较，不能仅凭 key_id 相同认定密钥正确。local_fingerprint = HMAC-SHA-256(K_ledger, 原始 payload_bytes)。Manager input_digest 只作为32字节不透明值保存，不验证 Manager HMAC。既有 operation_id 必须同时匹配稳定上下文、两个摘要、项目／配置、准备 session；冲突拒绝且不覆盖。报文 message_id 变化不改变准备事实。

不存配置原文、payload_b64、输出或密钥，因此本切片不需要把完整正文加密机制一并实现。身份、项目名、时间等元数据仍明文，指纹不得当作公开接口字段输出。K_ledger 的生成、秘密文件权限、备份与轮换仍由后续身份／密钥模块落实；本 API 复制调用方密钥，关闭时尽力清零，不宣称 Go 内存可完全擦除。

## 4. 包接口与失败约定

以下均为内部 Go 接口，不是新网络协议：

```go
type Identity struct { NodeID, LedgerID, KeyID string }
func Create(ctx context.Context, path, nodeID, keyID string, key []byte) (*Store, error)
func Open(ctx context.Context, path string, expected Identity, key []byte) (*Store, error)
func (s *Store) Identity() Identity
func (s *Store) Close() error
func (s *Store) RecordReadyFixture(ctx context.Context, rawPrepare []byte) (Record, error)
func (s *Store) Get(ctx context.Context, operationID string) (Record, error)
func (s *Store) Claims(ctx context.Context, operationID string) ([]Claim, error)
func (s *Store) NewGate(ctx context.Context, request startgate.Request,
    now func() time.Time, start func(startgate.Permit)) (*startgate.Gate, error)
```

Record 暴露本轮表的元数据：Identity、OperationID、StackID、VersionID、Digest（规范Base64url）、LocalFingerprint（内部规范Base64url）、ProjectName、EndpointID、PolicyRevision、PreparedSession、BootID、Phase、CreatedAt、StartedAt、Permit（未开始为 nil）。Claim 为 Kind、ResourceID。Record 不含原始输入。时间记录 UTC RFC3339 毫秒；持久时间不用于重建10秒窗口。

错误使用可供 errors.Is 判断的固定诊断：ErrInput、ErrIdentity、ErrKey、ErrSchema、ErrStorage、ErrNotFound、ErrConflict、ErrClaimed、ErrFrozen、ErrGateExists、ErrAlreadyCommitted。失败不拼接配置、密钥、SQL参数或原始驱动报错到对外诊断。Gate 自身的关联、期限与状态错误保留。存储错误保守处理，不表示执行一定未发生。没有可自动释放占用的错误类别。

## 5. 开始、重复、失败与重启

每次 Create/Open 生成新的不可由调用者指定的随机 boot_id；登记时保存 owner_boot_id。重开后 Get/Claims 可查询旧记录；RecordReadyFixture 和 NewGate 对旧 boot 记录拒绝为 ErrFrozen，不更改 phase、不补许可、不续期限。本轮没有解除冻结入口。独立的 Store 可以创建不同的新夹具操作，数据库占用约束仍有效；此机制不是生产 Agent 的进程单实例锁，也不检测旧备份回退。

同一 Store 每操作最多创建一个 Gate；constructor 校验身份、prepare session、摘要与 ready，成功后保留登记，取消、拒绝或失败后也不允许再 NewGate。NewGate及提交适配器负责按既有Id/Digest/Revision规则校验请求与许可字段，包括nonce、message_id、permit_id和reply_to；不能依赖内存Gate的非空检查替代类型约束。再次使用既有 Gate 的 RegisterRequest/Authorize 遵守原规格。新 Gate 不能绕过已经开始的记录。

Close先禁止新登记／新Gate并使所有现有Gate失效，等已进入Start的短回调退出后关闭数据库；不得持有Store登记锁调用Invalidate，避免Start内Get的锁反转。Close返回后旧Gate不能再次调用Start；并发／重复Close均应等待同一次关闭完成。数据库调用的context取消只结束对应等待／事务，不自动等于业务Cancel或连接失效；后者由调用者明确调用Gate方法。另一个Open不会撤销仍存活的第一个Store，这不是进程接管机制。

CommitStart 回调在立即写事务内重读记录、检查本 boot、固定上下文、摘要、准备 session 与两个占用，CAS `ready → START_COMMITTED` 并保存全部许可字段。**只有本次首次成功提交才能返回 nil**；匹配的已有开始事实返回 ErrAlreadyCommitted，不同许可返回 ErrConflict，均不能再调用 Start。预读不是执行资格；CAS和事务检查才是提交边界。

提交结束后仍由 Gate 在同一互斥内复查取消、连接失效及10秒期限，再调用 Start。Start 必须短小、不可重入 Gate，也不可调用会等待本 Gate 的 Store.Close；可用Get读取提交事实。回调返回不代表业务成功。开始事实提交后若取消／失效／到期，Start 为0，但已持久事实及占用保留，不能报告 not_started 或取消完成。没有事件表，故不产生 started 报文。

Begin、写入、Commit 失败均不调用 Start，Gate 进入不确定状态或保留已经发生的取消／失效；原 Store 禁止重建 Gate，重开由新 boot 冻结。即使读回 ready，也不能据此重试旧门禁。已经开始的操作收到重复许可不再写入、不再次 Start。所有终态显示都不足以释放本轮占用；没有 Delete/Release/Finalize 接口。

| 故障点 | 重开后的事实 | 必须行为 |
| --- | --- | --- |
| ready 已提交，开始提交前进程被终止 | ready + 原占用 | 旧boot冻结，不自动恢复许可 |
| START_COMMITTED 已提交，Start 尚未调用 | 开始许可完整 + 原占用 | 视为可能有副作用，不自动重做 |
| Start 已调用后进程被终止 | 同一开始事实 + 原占用 | 不凭进程退出或回调计数判断结果，不再调用Start |
| 登记／开始事务中止或提交失败 | 事务前事实，或无法确定 | 不覆盖旧证据，不释放，不在当前流程内部重试 |
| 缺库、错身份、错密钥、未知schema | 打开失败 | 不自动新建替代账本 |

## 6. 验收与交付条件

执行环境为本机 Windows/amd64、Go1.27、CGO关闭、临时本地 SQLite 文件及无副作用回调。L01—L10 全部具备直接断言且通过、独立审阅无未解决重要问题、全量回归与文档检查通过，才能称本模块切片完成。不是完整 Agent／M0 或可安装产品。

| 编号 | 输入、动作／故障点 | 可观察通过条件 |
| --- | --- | --- |
| L01 | Create、关闭、正确Open；重新建立物理连接 | 身份不变、boot改变；SQLite3.54.1、WAL/FULL/外键/5000读回正确；原文件不能被Create覆盖 |
| L02 | 缺文件、错误node/ledger/keyID/key、非32字节密钥、损坏库、较新schema | 固定错误，不能生成替代库或改写执行证据 |
| L03 | 合法prepare夹具及1/1.0/1e0整数形式、相同重放、变摘要／原始字节／目标／项目／配置 | 同输入一条记录；任一固定内容冲突拒绝，原文不入库；原bytes而非重编码参与HMAC |
| L04 | 同Stack或同端点项目的并发新操作，另一端点不同Stack | 冲突组仅一个操作与两占用完整提交；非冲突组可登记；没有半条记录 |
| L05 | 合法许可及重复、不同reply_to重放、变permit/auth_rev；重复Gate构造 | Start内读到完整已提交事实；总调用数1；不同许可不覆盖；新Gate不得再次获得资格 |
| L06 | 提交前取消／失效；提交过程中取消／失效；提交后窗口到期 | Start=0；提交过的开始事实和两占用仍在；无虚构取消完成或释放 |
| L07 | 真实第二连接写锁等待超时／context取消、语句中止、延迟外键导致Commit失败 | Start=0；原事实与占用保留；原Gate不得再提交，新Gate拒绝；错误无秘密回显 |
| L08 | 辅助进程分别停在开始提交前、提交后Start前、Start后，再由父进程强制终止 | 重开数据与故障点一致；每组旧操作NewGate/重新登记均冻结；累计Start不增加 |
| L09 | 关闭重开旧ready/committed；改Gate摘要/session/关联、非法UUID/超范围许可、破坏占用夹具；提交中及Start中Close | 旧boot及不完整执行资格不能启动；时间窗口不由数据库UTC时间恢复；Close无死锁且返回后无新Start |
| L10 | 全量测试、vet、构建、模块校验、契约和文档检查；静态接线审阅 | 原通信切片回归通过；仍无网络任务入口与Docker调用；全部本轮结果、版本和限制可查 |

L07 的语句／提交故障证明代码拒绝继续，不冒充真实磁盘耗尽或掉电实验。L08 是进程终止恢复，不证明硬件掉电耐久。真实Linux、磁盘满／损坏期间写入、备份回退、迁移、完整加密、事件重报、两端核对与释放均待后续验证。
