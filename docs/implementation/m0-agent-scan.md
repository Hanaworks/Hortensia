# M0 Agent 恢复扫描实施与验收

日期2026-11-07，起点2de48fd。状态：编码前基线，S01—S12未验证。

**Goal:** 提供可分页、可重放且不会绕过开始屏障的Agent账本扫描。

**Architecture:** 显式新v3库持久保存冻结、成员／水位、变化集合及最后completed；原文快照有界保存在当前boot内存，复用既有加密日志和事务投影。Manager业务恢复与网络接线在后续增量。

**Tech Stack:** 沿用Go1.27、modernc SQLite1.61.1／SQLite3.54.1及现有依赖；单连接立即事务；标准库HMAC、单调time.Time、随机ID。

**Spec:** [模块规格](../specifications/m0-agent-scan.md)。先保存本篇及规格，再写依赖它们的实现；沿用当前隔离工作区。

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

1. [ ] 保存基线；为v3/屏障、分页、变化／完成及故障分别写失败测试，记录实际失败。
2. [ ] Task1实现存储与原事务钩子；Task2实现有界快照、原文分页和完成；只在接口就绪后集成，不让各代理重复修改同一文件。
3. [ ] Task3注入真实语句／Commit失败和两处进程强杀；独立审阅Gate锁序、变化集合与原文预算，先记录修订再修代码。
4. [ ] 统一全量test/vet/build/mod verify、契约／文档检查；逐项回填证据、版本、范围及维护入口，保存本地Git提交。

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
