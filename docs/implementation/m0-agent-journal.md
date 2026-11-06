# M0 Agent 事件日志实施计划与记录

> 按任务先写测试、观察缺失／错误行为、实现后复验；独立审阅后再交付。沿用当前隔离工作区，不新增审批或清理其他工作区。

日期：2026-11-07。起点fac4fa6。本计划与[实施规格](../specifications/m0-agent-journal.md)在编码前保存。

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

- [ ] 写v2正确／拒绝／v1兼容、密文往返／篡改、计数回滚和耗尽测试，记录初始失败。
- [ ] 实现完整v2DDL、入口、复制／清理密钥、版本结构验证、独立预留与AEAD；依赖不变。
- [ ] 运行对应测试并独立审阅数据完整性与错误路径，修正发现。

## Task 2：事件、结果与Gate接入（J04—J07）

文件：新增events.go、results.go及测试；修改records.go登记v2 event_state、gate.go加入事务条件及原子started；不改网络通道。

接口：规格中的Event、ReadEvent、PendingEvents、AcknowledgeEvent、RecordResultFixture。提供 `s.readEvent(ctx,q,operation,seq)` 供Task3在同一Tx读取；q沿用已有querier。v2状态读取helper允许Task3重用。Task1的加密在主事务外准备，主事务重新验证所有执行前提。

- [ ] 先写首次started原子性、四类首结果、重复／冲突、not_started竞争及确认测试，确认失败。
- [ ] 实现相应行为，阻止已有结果／blocked的开始，保留开始字段；持久冲突只存指纹和隔离标记。
- [ ] 真实SQL故障覆盖事件插入与Commit，检查无部分事实、无二次Start、无额度回退。
- [ ] 独立审阅，测试通过后交给恢复读取任务。

## Task 3：一致恢复读取及进程终止（J08—J09）

文件：新增recovery.go、recovery_test.go、journal_restart_test.go。

- [ ] 写各阶段RecoveryRecord的Schema、关联、ack、占用和旧boot断言；实现同一事务快照读取。
- [ ] 用同步标记和真实Process.Kill/Wait测试started后、result事务中、result后、ack后的恢复；保存原body、序号及计数证据。
- [ ] 验证旧boot不能新写结果／重新执行；无虚构not_started，辅助进程均回收。

## Task 4：整体验收与交付（J10）

- [ ] 独立审阅当前实现与J01—J10，处理重要发现。
- [ ] 全量go test/vet/build/mod verify，契约、文档和Git空白检查；只在代码修正或未决疑点需要时重跑。
- [ ] 回填逐项结果、版本、复验命令和限制；同步README、Agent、数据、协议中的report_acked定义、开发／测试计划与文档规划。
- [ ] 保存本地Git提交，保持M0整体与网络通道的真实状态。

## 验收与实施记录

开工时J01—J10均未执行。选择显式新建v2而保留v1入口，是为了本轮完成事件原子性且不未经备份验证就改写已有账本；代价是暂不提供旧节点升级路径。结果接口保留Fixture名称，不将Schema通过当成真实进程及归属证据。
