# M0 Agent 事件日志实施计划与记录

> 按任务先写测试、观察缺失／错误行为、实现后复验；独立审阅后再交付。沿用当前隔离工作区，不新增审批或清理其他工作区。

日期：2026-11-07。起点fac4fa6。本计划与[实施规格](../specifications/m0-agent-journal.md)在编码前以5c0c1f7保存；源码／测试交付版本8ff708b。状态：J01—J10通过，本模块可供技术交接；完整M0和真实任务通道尚未完成。

**Goal:** 提供持久started／首份result、加密原文重读、确认和单操作恢复读取，为下一轮任务通道提供事实基础。

**Architecture:** 新建v2账本同库原子保存开始及事件；密钥使用次数先独立事务预留；原开始阶段与结果指针分离。v1入口保留，不隐式迁移。

**Tech Stack:** 已锁定Go1.27和modernc SQLite；Go标准库AES-GCM／HMAC；Windows临时文件和辅助测试进程。

**Spec:** [事件日志规格](../specifications/m0-agent-journal.md)，J01—J10为完成条件。

## 共同约束与审阅重点

- 事件body来自现有Schema，原字节持久重读；body≤7MiB、输出≤1MiB，序号由事务复核。
- KEK独立于K_ledger，每事件独立DEK；每次包装前持久预留，最多1048576，回滚不退。
- v2完整DDL与v1分开；旧文件不升级或重建。新库入口不是既有节点升级工具。
- not_started与开始竞争、同位异文冲突、旧boot结果伪造、Close与密钥副本、唯一连接下的嵌套事务是重点故障类。
- ack不释放，恢复读取不解除冻结，首份结果之后的补观察另有模块；任何范围变化先记入规格及本页。

## Task 1：v2入口、结构与事件加密（J01—J03）

文件：新增 `journal_store.go`、`journal_crypto.go`、对应测试及 `migrations/002_event_journal.sql`；改store.go的共用初始化／版本选择及Close，不改v1DDL。均在 `internal/agent/ledger/`。

接口：CreateJournal/OpenJournal、JournalKey；Store新增journal配置。内部约定：eventBinding含OperationID/Digest/Kind字符串与Seq uint64；sealedEvent含Cipher/WrappedDEK/Fingerprint字节和KeyID字符串；`s.sealEvent(ctx,binding,body)`先预留再加密，`s.openEvent(binding,sealed)`只解密校验不访问数据库。错误和固定额度在本任务定义。不得在业务Tx内调用sealEvent。

- [x] 写v2正确／拒绝／v1兼容、密文往返／篡改、计数回滚和耗尽测试，记录初始失败。
- [x] 实现完整v2DDL、入口、复制／清理密钥、版本结构验证、独立预留与AEAD；依赖不变。
- [x] 运行对应测试并独立审阅数据完整性与错误路径，修正发现。

## Task 2：事件、结果与Gate接入（J04—J07）

文件：新增events.go、results.go及测试；修改records.go登记v2 event_state、gate.go加入事务条件及原子started；不改网络通道。

接口：规格中的Event、ReadEvent、PendingEvents、AcknowledgeEvent、RecordResultFixture。提供 `s.readEvent(ctx,q,operation,seq)` 供Task3在同一Tx读取；q沿用已有querier。v2状态读取helper允许Task3重用。Task1的加密在主事务外准备，主事务重新验证所有执行前提。

- [x] 先写首次started原子性、四类首结果、重复／冲突、not_started竞争及确认测试，确认失败。
- [x] 实现相应行为，阻止已有结果／blocked的开始，保留开始字段；持久冲突只存指纹和隔离标记。
- [x] 真实SQL故障覆盖事件插入与Commit，检查无部分事实、无二次Start、无额度回退。
- [x] 独立审阅，测试通过后交给恢复读取任务。

## Task 3：一致恢复读取及进程终止（J08—J09）

文件：新增recovery.go、recovery_test.go、journal_restart_test.go。

- [x] 写各阶段RecoveryRecord的Schema、关联、ack、占用和旧boot断言；实现同一事务快照读取。
- [x] 用同步标记和真实Process.Kill/Wait测试started后、result事务中、result后、ack后的恢复；保存原body、序号及计数证据。
- [x] 验证旧boot不能新写结果／重新执行；无虚构not_started，辅助进程均回收。

## Task 4：整体验收与交付（J10）

- [x] 独立审阅当前实现与J01—J10，处理重要发现。
- [x] 全量go test/vet/build/mod verify，契约、文档和Git空白检查；只在代码修正或未决疑点需要时重跑。
- [x] 回填逐项结果、版本、复验命令和限制；同步README、Agent、数据、协议中的report_acked定义、开发／测试计划与文档规划。
- [x] 保存本地Git提交，保持M0整体与网络通道的真实状态。

## 验收与实施记录

开工时J01—J10均未执行。选择显式新建v2而保留v1入口，是为了本轮完成事件原子性且不未经备份验证就改写已有账本；代价是暂不提供旧节点升级路径。结果接口保留Fixture名称，不将Schema通过当成真实进程及归属证据。

开工审阅明确J06：顺序重试已存在事件不再预留额度；并发重试允许浪费先行预留，但业务序号和事件只有一份。两类测试分别断言，不将密钥计数误当成已提交事件数。

编码顺序：先保存测试并观察缺少JournalKey、事件及恢复API的编译失败，再实现。集成初期曾在事件状态登记尚未落地时运行恢复测试，得到ErrEvidence；接入完成后通过，未放宽条件。进程夹具随后改进失败诊断：先判断是否收到边界标记，避免对已经失败退出的子进程Kill时，Windows的Access denied遮蔽真正原因。

两项实质修正均保留了可运行失败证据：恢复旧NewGate持锁查询行为时，事务内解密测试失败为“Gate lock blocked transactional decryption”；恢复无锁数据库读取后通过。含大量字面`<`的合法结果约2MiB，在旧校验信封重编码后超过8MiB而读取失败；改为仅编码信封头并附原body。RecoveryRecord也补原字节回归，旧实现改变结果字节而失败，修正为元数据头加已经验证的原body后通过。

审阅范围：入口、结构、加密与计数由独立审阅通过，另独立检查事件、恢复、事务与进程终止实验，指出恢复正文重编码问题并未发现其他重要问题。该问题的最后修正及最终全量复验由主执行者完成；不将此写成修正后又获得了一次独立审批。当前无已知未解决的重要审阅问题。

## J01—J10逐项结果

下列测试均位于 `internal/agent/ledger/`；名称作定位，结论以断言和实际运行结果为依据。

| 编号 | 结果 | 直接证据与限制 |
| --- | --- | --- |
| J01 | 通过 | [journal_store_test.go](../../internal/agent/ledger/journal_store_test.go)的CreateOpenAndKeyPairing、InvalidCreationDoesNotCreateFile、RejectsAlteredSchemaAndSupportsLineEndings验证两版互拒、空库密钥配对、错键／同键／错误结构与LF/CRLF；旧Store／账本回归通过，未迁移v1 |
| J02 | 通过 | [journal_crypto_test.go](../../internal/agent/ledger/journal_crypto_test.go)独立解包核对AAD/HMAC与随机密文、篡改／跨绑定拒绝、真实库和活动WAL无测试正文或密钥、密钥副本／Close清理；ReadRejectsAuthenticatedInvalidOrMismatchedBody补校验解密后结构与关联 |
| J03 | 通过 | ReservationSurvivesRollbackAndExhaustion、ReservationFailureDoesNotProduceCiphertext、ConcurrentReservationsCannotExceedCap验证先行计数、真实语句／延迟外键Commit失败和额度竞争；主事务失败及强杀后计数不退，耗尽仍能读取、重放、确认和Open |
| J04 | 通过 | [events_test.go](../../internal/agent/ledger/events_test.go)的StartedVisibleInsideStartAndAckPreservesEvidence在Start内读取同一started；[journal_fault_test.go](../../internal/agent/ledger/journal_fault_test.go)核对started插入／Commit失败与取消边界，Start最多一次，开始与事件原子提交 |
| J05 | 通过 | [results_test.go](../../internal/agent/ledger/results_test.go)的FirstResultsAndDuplicateConflict覆盖四种结局；RejectsWrongBindingAndState、ExactBytesNumericSequenceAndBudgets覆盖目标／摘要／连接／boot／序号、1.0/1e0、7MiB正文和1MiB输出边界；NotStartedRacesStart只允许一种提交资格，结果仍是夹具证据 |
| J06 | 通过 | 相同body顺序重试不增加额度或序号；8个并发重试只提交一份结果。异body保留原事件并持久冲突指纹及blocked，未来版本拒绝；GateRechecksBlockedAndResultAfterConstruction验证构造后变化也在开始事务重新检查 |
| J07 | 通过 | StartedVisibleInsideStartAndAckPreservesEvidence与CanceledResultAndAckPreserveCommittedState验证精确确认、错误关联、重复／取消与持久结果不变；新session重新取得同一body且占用保持。仅可信存储API，未验证线上reply_to与认证 |
| J08 | 通过 | [recovery_test.go](../../internal/agent/ledger/recovery_test.go)覆盖ready、当前／旧boot开始、四类结果、冲突与损坏，实际投影嵌入reconcile_reply经原Schema验证；序号、结果指针、许可及两占用一致；PreservesEmbeddedResultBytes保留原正文不膨胀，未实现完整扫描 |
| J09 | 通过 | [journal_restart_test.go](../../internal/agent/ledger/journal_restart_test.go)四组真实子进程强杀，分别停在started后、result事务内、result后、ack后。事务内使用同SQLite驱动／参数加测试专用TEMP触发器，在实际RecordResultFixture的事件和指针更新后、Commit前暂停；重开只见完整提交，计数分别为1或2，body逐字相同，旧任务不重启 |
| J10 | 通过 | 全量5个测试包通过，750条测试及子测试通过记录，其中ledger213条；失败0、测试跳过0。vet、全包build、mod verify、20类消息／21正例／11反例／1解码输入的契约检查及文档检查通过；通道运行代码未改，仍冻结且不执行Docker |

环境：Windows/amd64、Go1.27.0、CGO_ENABLED=0、SQLite3.54.1、PowerShell7.6.9。通过记录包含父组及无故障模式的辅助测试入口，不等于750个独立场景；contracts纯嵌入包的package级skip不计作跳过测试。最终ledger包12.139秒、TLS通道14.517秒，仅为本次运行记录，不是性能承诺。未增加依赖或更改go.mod/go.sum。

最终文档检查覆盖26篇、326个本地链接及103张表，通过链接、锚点、表格列数与围栏检查；Git空白检查通过。该脚本不验证外部来源可访问性或代替语义审阅。

复验命令：`go test ./internal/agent/ledger -run TestJournal -count=1 -timeout=90s`；完整回归 `go test ./... -count=1 -timeout=120s`、`go vet ./...`、`go build ./...`、`go mod verify`、`pwsh -File tools/check-contracts.ps1`、`node tools/check-docs.mjs`。本机TLS测试需要允许回环套接字；不将运行环境拒绝连接当成实现通过或产品错误。

交付入口：[v2创建与打开](../../internal/agent/ledger/journal_store.go)、[加密与额度](../../internal/agent/ledger/journal_crypto.go)、[事件读取与确认](../../internal/agent/ledger/events.go)、[首结果](../../internal/agent/ledger/results.go)、[恢复读取](../../internal/agent/ledger/recovery.go)、[v2DDL](../../internal/agent/ledger/migrations/002_event_journal.sql)。开始适配沿用gate.go。调用者提供独立密钥，只能以受控结果夹具验证本模块；没有新增可部署的CLI或产品入口。

未交付／未验证：v1→v2升级、密钥文件／轮换／备份恢复及回退检测、掉电／真实磁盘故障、Linux运行、CGO race、真实进程结果与归属、补观察结果版本、Manager持久授权／事件接收、线上发送重试／ack关联、完整恢复扫描及双端释放。结果和确认都不解除占用。下一模块先补Manager持久操作／事件接收及会话／核对约束，再接入受控任务通道；每个模块仍先定具体规格与验收条件。

## 维护交接说明补充

2026-11-07用户补充规格也要面向其他开发者和维护者。本次根据8ff708b源码及DDL补充[规格第7—10节](../specifications/m0-agent-journal.md#7-内部结构与职责)：文件和协作职责、两阶段结果检查流程、开始／确认／恢复提交点、三类状态的区别、全部新增持久字段的来源和生命周期、错误定位及修改时必须维持的约束。同步开始账本说明及后续模块的交付要求。

本次仅完善文档，不改变源码、Schema或J01—J10判据；原软件测试结果仍绑定8ff708b。本次检查针对说明与源码是否一致、本地链接／锚点及Markdown结构，不将历史软件测试记为再次执行。初次规格基线、后续实现修订和本次交付后补充的时间分别保留。

独立核对说明后修正三处措辞：5000毫秒只约束SQLite锁等待、整体期限由调用context控制；not_started输给started竞争仅返回错误，不写异文冲突行；started从许可生成seq=1而result从body提取序号。最后文档检查结果记入文档规划第12节，通信规格同时补充维护入口及独立ledger尚未接入网络的界限。
