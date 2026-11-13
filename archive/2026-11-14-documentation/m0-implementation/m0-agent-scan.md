> 已归档：2026-11-14。本文是已结束M0切片的实施／验收历史，正文中的执行要求和“当前／下一步”只适用于当时，不是新的工作指令。现行实现入口：[当前模块](../../../docs/specifications/m1-agent-runtime.md)；整体状态：[M1推进](../../../docs/implementation/m1-first-version.md)。原始字节见归档原文包。

# M0 Agent 恢复扫描实施与验收

日期2026-11-07，起点2de48fd。规格及验收基线在编码前以9f6c34c保存；源码／测试版本a34a769。状态：S01—S12通过，Agent本地扫描切片可供源码交接，整个M0尚未完成。下表保留开工时的初始状态，最终结果与限制见后文。

**Goal:** 提供可分页、可重放且不会绕过开始屏障的Agent账本扫描。

**Architecture:** 显式新v3库持久保存冻结、成员／水位、变化集合及最后completed；原文快照有界保存在当前boot内存，复用既有加密日志和事务投影。Manager业务恢复与网络接线在后续增量。

**Tech Stack:** 沿用Go1.27、modernc SQLite1.61.1／SQLite3.54.1及现有依赖；单连接立即事务；标准库HMAC、单调time.Time、随机ID。

**Spec:** [模块规格](../../../docs/specifications/m0-agent-scan.md)。先保存本篇及规格，再写依赖它们的实现；沿用当前隔离工作区。

## 共同接口与分工

全部位于`internal/agent/ledger/`。不修改v1/v2 DDL，不增加依赖或线上字段。

- Task1拥有store.go、journal_store.go需要的版本分派、003_recovery_scan.sql、scan_store.go、scan_barrier.go、scan_store_test.go、scan_barrier_test.go，以及records.go/gate.go/results.go/events.go/journal_crypto.go内原子屏障与变化钩子。现有insertEvent改为Store方法。提供CreateRecovery/OpenRecovery（参数沿CreateJournal/OpenJournal，末尾加RecoveryOptions）、requireRecovery、requireRecoveryWriter(ctx,q)、requireRecoveryMutation(ctx,q,session)、requireRecoverySession(ctx,q,session)、freezeRecoveryGates()、setRecoveryLocalFrozen(bool)、noteRecoveryEvent(ctx,tx,operation,seq)、noteRecoveryConflict(ctx,tx)、BindRecoverySessionFixture、RecoveryScanState。
- RecoveryOptions{Now func()time.Time}；ScanState{Frozen,Dirty bool;SessionID,ActiveScanID,CompletedScanID string;Total uint64}。Store.recovery为*recoveryConfig，配置定义由Task1放scan_store.go：mu sync.Mutex（串行扫描API）、now func()time.Time、localFrozen bool（只用Store.mu保护）、snapshot *scanSnapshot、maxRecords int、maxBytes int。默认4096／32MiB；测试可在并发前降低私有预算。
- Task2拥有scan.go、scan_snapshot.go、scan_test.go及recovery.go内部投影提取。定义scanSnapshot并实现Reconcile(ctx,raw)([]byte,error)、CompleteReconcile(ctx,raw)([]byte,error)、readRecoveryRecord(ctx,q,id)(json.RawMessage,error)。scanSnapshot销毁方法clear()清正文；Store关闭通过recovery.mu后调用此方法。不得在事务内调用公开RecoveryRecord或任何会另开事务的方法。
- Task3拥有scan_fault_test.go、scan_restart_test.go；根据上述API独立检查SQL失败、结束回执进程终止和冻结／Gate竞争，不加入生产测试钩子。
- 共同错误由Task1加入store.go：ErrRecovery、ErrScanExpired、ErrScanCursor、ErrScanBusy、ErrScanIncomplete、ErrScanChanged。错误不含原文；有效请求的失败返回nil与Go error，线上错误信封由后续适配器负责。

DDL字段严格按规格：recovery_control的id=1，current_session等空值为''，无active时active_scope=''、active_limit=0、active_total=0；有active时scope枚举／limit1..200并frozen=1。recovery_members的ordinal从0、result_event_seq无结果为0，record_fingerprint BLOB32。recovery_changes不带scan_id，跨整个连续冻结期保留。新event／conflict钩子在frozen=1时记录；writer资格检查仍独立执行。成员只保存当前active和最近completed两组，成员生命周期清理由Task2负责。

冻结顺序：Task2／Bind在recovery.mu内调用freezeRecoveryGates（Store.mu仅设置localFrozen和复制列表，释放后Invalidate），然后开始事务持久freeze。不得持DB事务等Gate，不得在事件事务取recovery.mu。Task1的NewGate最终登记在Store.mu内查localFrozen，开始／prepare最终Tx调用requireRecoveryMutation。v3 Open事务重设active_boot并frozen=1，但保留active／成员／changes／completed；新实例snapshot=nil。ACK检查current_session；旧执行result仍按原prepared_session写，但检查active_boot。

## 实施顺序与验收

1. [x] 保存基线；为v3/屏障、分页、变化／完成及故障分别写失败测试，记录实际失败。
2. [x] Task1实现存储与原事务钩子；Task2实现有界快照、原文分页和完成；只在接口就绪后集成，不让各代理重复修改同一文件。
3. [x] Task3注入真实语句／Commit失败和两处进程强杀；独立审阅Gate锁序、变化集合与原文预算，先记录修订再修代码。
4. [x] 统一全量test/vet/build/mod verify、契约／文档检查；逐项回填证据、版本、范围及维护入口，保存本地Git提交。

| 编号 | 通过条件与观察 | 初始状态 |
| --- | --- | --- |
| S01 | v3创建／重开、身份密钥结构拒绝、三版互拒；旧文件和DDL不变 | 未验证 |
| S02 | prepare／Gate构造／开始提交与冻结竞争，无冻结后新执行；开始事实与claims保留，回调读取不死锁 | 未验证 |
| S03 | 同一Tx固定排序身份、total、记录原文；后续result／ack不改变已固定页面 | 未验证 |
| S04 | 数量和完整8MiB信封双预算；大记录分开、result原字节不变；总预算失败保持冻结无部分发布 | 未验证 |
| S05 | 游标篡改／跨scan、scope／limit／session错误拒绝；未枚举到末页不完成，重试不跳页 | 未验证 |
| S06 | 新事件及变化同事务，当前ACK前不完成；快照后的同位冲突令扫描失效 | 未验证 |
| S07 | 同boot新session续扫描保留截止时间，旧session拒绝，S1确认过的变化仍须S2重新确认 | 未验证 |
| S08 | pending→all成功原子替换、失败保持旧扫描与冻结、变化集合继承；旧cursor／complete拒绝 | 未验证 |
| S09 | 单调15分钟准确边界、重试不续期、缓存丢失／重启scan_expired，持久冻结保留 | 未验证 |
| S10 | completed同body幂等、跨重开重放仅回执；旧completed不关闭新扫描、不释放claims | 未验证 |
| S11 | freeze／成员发布／变化／completed语句及Commit失败不半提交；真实进程事务内／提交后强杀，重开核对冻结／回执 | 未验证 |
| S12 | KEK耗尽仍可扫描；旧版本／Manager／通道全量回归；README／组件／数据／开发／测试及接手说明同步 | 未验证 |

## 暂缺与环境

2026-11-07实施中核对（S02/S09/S11）：故障测试发现冻结事务失败后DB仍为旧frozen=0，原开始入口还须同时检查本地localFrozen，已在真实失败断言后修正。独立审阅另指出页面在等待唯一数据库连接期间可能越过deadline，因此增加取得事务后及发布前的期限复核；过期原文在下一次访问时清理，不新增后台线程。此段在期限修复前保存。

Agent的completed不是Manager业务核对完成。Manager页仓储／变化合并／历史缺失恢复及新受理解冻、finalized双端释放、任务网络入口、真实Compose、旧库迁移、备份回退和掉电耐久继续未实现或未验证。结果与session仍由受控夹具提供。

本轮使用Windows临时数据库和本机辅助进程，不需要用户联调环境。后续进入真实Docker／Compose、路径／端点或跨机网络时，先明确环境要求和测试范围。

## 实施与审阅记录

先保存模块规格和本页，再建立测试并实际观察到缺失API／类型的编译失败。实现按存储屏障、固定快照／分页、独立故障验证分工；独立契约测试直接解析报文和检查SQLite行，不仅检查函数是否返回成功。

实施期间留下以下失败及修正证据：冻结SQL失败后本地状态已冻结，原开始提交仍可能绕过持久旧状态，补最终事务的localFrozen检查；关闭中的v3辅助入口仍允许继续检查，补closing拒绝；页面等待唯一数据库连接跨过期限后曾返回815字节成功响应，补取得事务后及发布前的期限检查；观察到期时旧缓存原文仍在，补访问时清除。对期限修复增加的测试曾因直接访问已清空缓存而空指针失败，修正断言为允许nil或已清空缓存，没有据此恢复过期正文。

存储重构审阅还发现失败创建／打开的未发布Store持有密钥副本，已补回滚后关闭数据库并清理自有副本。最终独立只读审阅覆盖全部扫描生产代码、DDL、原写入口钩子、测试和规格，没有新阻断发现；指出“一个Store最多32MiB”的表述遗漏替换时新旧共存，已在规格第3节明确单份32MiB、两份原投影合计最多64MiB及额外临时开销，没有把缓存预算写成进程内存峰值。

未增加依赖、未修改共享Schema、v1／v2 DDL和历史归档。网络任务仍未接线；本轮成功信封可按共同Schema校验，失败仍是Go错误，线上错误页及protocol_error由后续适配器承担。没有生产故障注入开关。

## S01—S12最终结果

以下依据a34a769及实际运行断言。相关文件均位于`internal/agent/ledger/`，测试中的身份、session、结果及Manager完成请求由受控夹具提供。

| 编号 | 结果 | 证据、观察及限制 |
| --- | --- | --- |
| S01 | 通过 | [scan_store_test.go](../../../internal/agent/ledger/scan_store_test.go)检查v3版本／初始冻结、Open保留session且撤销旧写者、错误ledger身份／账本密钥／KEK拒绝、版本入口互拒、DDL及约束异常拒绝；旧v1／v2测试完整回归，DDL无差异，无升级工具 |
| S02 | 通过 | [scan_barrier_test.go](../../../internal/agent/ledger/scan_barrier_test.go)直接检查冻结后prepare、Gate登记、开始事务被拒及事实保留；[scan_fault_test.go](../../../internal/agent/ledger/scan_fault_test.go)用真实Gate控制两处竞争：已进入Start时扫描等待且回调Get／Claims不死锁；START_COMMITTED后未进入回调时失效Gate，持久started保留而Start不执行 |
| S03 | 通过 | [scan_protocol_test.go](../../../internal/agent/ledger/scan_protocol_test.go)的FixedRecordsOrderingAndPaging检查按operation_id排序、固定total、分页串联及重读原字节；扫描后result／ACK不改原页面；[scan_test.go](../../../internal/agent/ledger/scan_test.go)另检查持久成员指纹破坏时不能完成 |
| S04 | 通过 | BytePaginationPreservesLargeOriginalResults使用两条各超过4MiB、含大量小于号的结果，实际分成完整信封小于等于8MiB的页面，抽取latest_result与原字节相同；记录／总字节预算失败无部分发布。此为受控容量夹具，不是生产负载或峰值内存测量 |
| S05 | 通过 | RejectsCursorAndRequestBindingErrors检查未知／跨扫描游标、scope／limit／session错误；未到尾页Complete被拒；分页重试保持原成员、游标及total；所有成功响应通过共同Schema校验 |
| S06 | 通过 | ChangesSurviveUpgradeAndReconnect检查新事件原子进入变化集合、未确认时不完成；ConflictInvalidatesFrozenView检查同位异文置dirty、旧页面和Complete拒绝、新扫描显示隔离事实。语句／Commit故障另核对事件、变化索引及dirty一起回滚 |
| S07 | 通过 | 同boot换session保留scan和deadline、旧会话请求拒绝；S1确认过的变化在S2仍阻止完成，只有S2再次ACK才可完成；相同session绑定不会重新冻结已完成状态 |
| S08 | 通过 | UpgradeRejectsOldCursorAndDowngrade检查pending→all换ID、旧cursor／complete拒绝、all→pending拒绝；SnapshotBudgetFailureDoesNotReplaceActive检查升级失败保留旧ID／成员／页面和冻结；变化集合跨升级保留 |
| S09 | 通过 | DeadlineCannotBeRenewed检查受控单调时钟15分钟边界，重试／重连不延期；DeadlineWhileWaitingForConnection实际占用唯一连接越过期限，等待后的页面无成功响应／末页资格；LostMemoryCannotResumeFromPersistentMembers检查缓存丢失不依持久成员恢复，重开保持冻结 |
| S10 | 通过 | EmptyCompletionAndHistoricalReceipt及CompletedReplayDoesNotThawNewScan检查空扫描、完成同body回执、重开后重放、旧回执不解除新扫描冻结；始终保留已有两项claims；回执只保留最近成功完成的一轮 |
| S11 | 通过 | [scan_fault_test.go](../../../internal/agent/ledger/scan_fault_test.go)对冻结、成员替换、事件／变化、完成及dirty五类事务分别注入真实SQL语句失败和延迟外键Commit失败；[scan_restart_test.go](../../../internal/agent/ledger/scan_restart_test.go)在完成事务内及提交后回执前强杀辅助进程，重开核对旧／新回执、冻结、原文、ACK与claims，无半提交 |
| S12 | 通过 | ExhaustedKeyStillReadsAcksAndCompletes将包装额度置上限后验证读事件、旧页、当前ACK、完成／重放及新建含结果的扫描，不增加／退还额度、不释放claims；CloseClearsCachedBodies检查缓存引用清空和正文零化。全量回归、维护说明及各现行入口同步，命令与结果见下节 |

完成故障实验覆盖两类不同持久结果：事务内强杀没有新的completed，重启继续冻结；提交后强杀可重放已提交body，但重启产生的新冻结或新扫描仍不能被旧回执解除。两类都检查任务结果原文、确认关联与claims仍在。Kill/Wait只是进程终止，不等于掉电或存储设备失效验证。

## 运行环境、检查与复验

环境：Windows/amd64、Go1.27.0、CGO_ENABLED=0、modernc.org/sqlite v1.61.1／SQLite3.54.1、PowerShell7.6.9。沿用既有工具链及依赖。

- 全量`go test ./... -count=1 -json`通过6个测试包，902条测试／子测试pass记录，其中Agent账本260条，失败0、测试skip0。父测试及辅助入口也包含在统计内，不等于902个独立场景；contracts纯嵌入包的package skip不算测试跳过。
- 全量时Agent账本14.574秒、Manager5.431秒、通道15.355秒，仅记录本次运行。之后只增补测试中的额度耗尽后新建扫描、Close缓存清理、错误身份／密钥断言，受影响的`TestScanRoot`与`TestRecoveryStore`共8个测试再次通过（1.041秒），生产代码没有再变；不把后增测试混算进902条全量记录。
- 全量`go vet ./...`、`go build ./...`、`go mod verify`通过。静态契约通过20类消息、21正例、11反例、1解码输入。文档链接／锚点、表格列数、围栏检查和Git空白检查通过；这些静态检查不代替语义审阅或外部引用核实。
- 保存的基线9f6c34c、实现／测试a34a769和本页交付记录分别保留开工、实现、验收时点。原始全量日志在本地忽略目录`.cache/m0-agent-scan-tests.jsonl`，不作为版本化交付物；可用下列命令重新生成。

在仓库根运行`go test ./internal/agent/ledger -count=1`可复验本地账本和扫描；完整回归使用`go test ./... -count=1`，环境须允许本机回环连接及辅助进程。另运行`go vet ./...`、`go build ./...`、`go mod verify`、`pwsh -File tools/check-contracts.ps1`、`node tools/check-docs.mjs`及`git diff --check`。

下一步先按同样文档流程实现Manager扫描页的持久接收、连续冻结期间变化合并、完整性检查及业务恢复屏障，再接受控任务通道和双端终结释放。当前未验证真实节点／网络任务、finalized历史过滤、备份回退、旧库升级、掉电耐久或真实Compose；这些未验证项不影响本轮受控Agent库切片验收，也不因此成为完整M0或可安装产品。
