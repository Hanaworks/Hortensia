# M0 Manager 扫描接收与业务恢复实施及验收

日期2026-11-07，起点67a8963。状态：编码前基线，R01—R12未验证。

**Goal:** 持久接收Agent扫描并按现有恢复矩阵核对，只有完整业务依据和当前completed才能开放节点。

**Architecture:** Manager显式v2复用原协调库，页面加密收件箱与幂等业务消费分开；最终完成请求和回执都重查持久业务。v1、Agent v3和线上Schema不变。

**Tech Stack:** Go1.27.0、CGO=0、modernc SQLite1.61.1／SQLite3.54.1、既有标准库密码与Schema校验；当前外部管理的隔离工作区。

**Spec:** [模块规格](../specifications/m0-manager-recovery.md)。先保存并提交基线，再实施；普通技术选择由实施者记录推进，不新增逐轮审批流程。

## 文件与共同接口

全部生产代码在`internal/manager/operations`。不改001 DDL、不增加依赖。Task1负责store.go版本参数、recovery_store.go、recovery_pages.go、recovery_page_read.go、migrations/002_recovery.sql及存储／分页测试；control.go只增加原BindSessionFixture拒绝v2，完整新绑定由recovery_store.go负责。Task2负责recovery_business.go、recovery_complete.go、recovery_evidence.go、recovery_business_test.go，以及records.go/events.go的released证据分支。Task3只写recovery_integration_test.go、recovery_fault_test.go、recovery_restart_test.go。

共享约定：Store.version int（原版=1）、Store.recoveryMu sync.Mutex；v2检查`requireRecovery() error`。错误增加ErrRecovery、ErrRecoveryIncomplete、ErrRecoveryBusy。RecoveryState{RoundID,ScanID,Scope,Status,AgentBootID,NextCursor string;Limit,Pages,Seen,Total int}。私有recoveryScan嵌入RecoveryState，另有NodeID,LedgerID,SessionID,ManagerBoot,LastOperation string及Bytes int。Task1定义上述类型。

Task1提供：CreateRecovery/OpenRecovery参数沿原Create/Open；BindRecoverySessionFixture(ctx,Binding,agentBootID)error；StartRecovery(ctx,Binding,scope string,limit int)([]byte,error)；NextRecoveryRequest(ctx,Binding)([]byte,error)；ReceiveRecoveryPage(ctx,Binding,raw []byte)(RecoveryState,error)；ReadRecoveryState(ctx,nodeID string)(RecoveryState,error)；ReadRecoveryPage(ctx,roundID string,pageNo int)(json.RawMessage,error)。页号从0开始。

2026-11-07接口审阅补充：Task1同时提供RecordRecoveryFailureFixture(ctx,Binding,requestID,code string)(RecoveryState,error)，检查当前轮持久请求关联；scan_expired/scan_changed/cursor_invalid标expired保持冻结，busy不改变进度，同scope Start随后可新建轮次。覆盖complete已准备后过期，不让completing永久卡住（R08/R09）。

内部共享：`loadRecoveryScan(ctx,q,Binding)(recoveryScan,error)`读取当前轮并验证绑定及双方boot；`readRecoveryScanByID(ctx,q,id)(recoveryScan,error)`只读历史；`readRecoveryPages(ctx,q,recoveryScan)([]json.RawMessage,error)`在调用方同一事务中重读校验全部已存页／请求／members；返回每页原body，调用者负责clear。`recoveryPageBinding(scan,pageNo,cursor) blobBinding`生成独立用途AAD上下文。所有helper不自行开始／提交事务。

Task2提供：`parseRecoveryRecord(raw json.RawMessage)(recoveryRecord,error)`，私有record字段OperationID,Digest,StackID,VersionID,ProjectName,Phase,FinalState string；LastSeq,ResultSeq,FinalizedSeq uint64；ClaimsHeld,ReportAcked bool；Result,Raw json.RawMessage。Task1共同Schema后调用它做跨字段核对。支持数学整数表示，原文不重编码。Task2提供ProcessRecovery、PrepareRecoveryComplete、ReceiveRecoveryCompleted、ReceiveRecoveryEvent、RecoveryCases、ReadRecoveryCase，公开签名按规格。完成方法沿Task1的requests表保存kind=reconcile_complete，page_no=-1。

ProcessRecovery及ReceiveRecoveryCompleted返回(RecoveryState,error)，PrepareRecoveryComplete与ReceiveRecoveryEvent返回([]byte,error)；RecoveryCases(ctx,nodeID)返回([]RecoveryCase,error)，RecoveryCase字段ID,NodeID,LedgerID,OperationID,Kind,RoundID string；ReadRecoveryCase(ctx,id)返回(json.RawMessage,error)。2026-11-07业务编码前补充R05/R06：phase=reconciliation_required的原可终结result仍必须隔离并抑制finalize，不将其作为正常成功页消费；其正文留在加密页及case。

DDL列名按规格表固定；recovery_scans的page_limit、body_bytes与公开Limit/Bytes映射。requests.raw为BLOB，accepted为0/1；pages只保存加密body和请求关联，其余从校验后的原body读；members记录page_no/record_no。cases的kind和fingerprint用于去重，round_id可空串（真实事件无活动轮）；absence_releases只存operation_id与round_id。Task1在v2 operations的release_status CHECK允许released，但只有state=canceled、cancel_requested=1、无permit/事件且blocked=0的无记录例外；Task2必须从证明校验，而非仅信字符串。

2026-11-07业务实施前补充R05/R09：Task1增加recovery_expected(round_id,operation_id,last_event_seq)，主键(round,op)，Start的新轮冻结事务用本节点operations的max_event_seq固定水位；Task2据此区分合法页后新事件和扫描前历史倒退，后者入case。先记录此基线变化再依赖实现。

## 实施顺序

1. [ ] 保存两篇基线，复核共享接口及范围；对版本、请求链、业务状态写失败测试并观察真实失败。
2. [ ] Task1完成持久版本／绑定／分页／重读；Task2完成跨字段校验、导入／case／取消释放及完成事务；生产文件各有负责人，集成后分别做规格及代码复核。
3. [ ] Task3以真实Manager v2／Agent v3交换请求／页面／事件／回执；注入SQL语句／Commit失败、进程终止和重连，直接检查数据库与原文。
4. [ ] 完整test/vet/build/mod verify、契约／文档检查及独立审阅；回填逐项结果、版本和限制；同步现行入口并提交。

## R01—R12验收基线

| 编号 | 通过条件与观察 | 初始状态 |
| --- | --- | --- |
| R01 | 显式v2、v1互拒，错身份／密钥／DDL拒绝；新建／重开／新节点冻结，旧Store及旧Agent boot无写入／完成资格 | 未验证 |
| R02 | 持久请求关联、跨页严格排序／唯一、固定scope/scan/total，拒绝跳页／游标环／错session/ledger/reply_to及伪造complete | 未验证 |
| R03 | 页原文字节加密往返、同页幂等不增额度、异文拒绝；8MiB/limit/4096/32MiB预算及损坏页／members不成为空扫描 | 未验证 |
| R04 | pending缺未释放本地操作只产生need_all，升级换本地轮次、连续冻结，旧页／完成失效 | 未验证 |
| R05 | 未授权ready持久取消停投；合法首结果导入沿原投影，旧快照不回退新事件，无扫描页ack；待finalize时保持pending/claims与冻结 | 未验证 |
| R06 | 额外未知操作、目标／摘要／同位冲突、超出支持历史保存case并阻止完成，不虚构Operation；真实未知事件先持久后ack | 未验证 |
| R07 | 可信all缺席且无许可／无事件才原子取消释放并保存依据；已授权缺席保留claims；重开后证明可核验、损坏证明不能读作已释放 | 未验证 |
| R08 | 只有业务就绪才准备complete；当前请求回执提交后解冻该节点，旧回执／新轮／重启／换session不解冻，其他节点保持冻结 | 未验证 |
| R09 | 扫描期间新事件与原页合并，不丢结果、不伪造started；新case／冲突在complete准备后到达仍阻止回执解冻 | 未验证 |
| R10 | 页面／case／无记录释放／completed语句和Commit失败不半提交、不成功应答；额度可消耗但不退还 | 未验证 |
| R11 | 至少完成回执事务内／提交后两处真实进程强杀，重开检查证据／claims／冻结；本地双端组合可重试，无重复执行 | 未验证 |
| R12 | 密文／绑定破坏拒绝，KEK耗尽已存页／事件可重读及幂等处理；v1、Agent、通道全量回归，规格／接手／入口同步 | 未验证 |

## 审阅重点与暂缺

额外关注：页提交后业务消费失败重试、固定页比实时事件旧、请求构造成功但提交失败、旧completed跨boot重放、absence释放后出现矛盾事件。分别由R03/R05/R08/R09/R10断言；不得借测试夹具清frozen绕过真实握手。

2026-11-07审阅修订（相关业务实现前保存）：同页异文必须持久case；已absence释放后出现ready等无事件记录同样隔离；释放事务复查该节点case；合法超范围真实事件先加密保存case再ack，目标／摘要／permit矛盾保存case但不ack。以上分别扩充R03/R06/R07/R09，不改变未知必须隔离的主规则。

2026-11-07实现中先记录再修复（R06/R09/R10）：recovery_nodes增加quarantined 0/1；Store增加由Store.mu保护的recoveryBlocked map[string]bool。case先本地屏障，再独立事务持久隔离，之后才seal及case事务，失败不ack且保持隔离。最终准入与完成／absence释放也检查此屏障；新绑定／Start不清。事件事务重读发现released必须转case，堵住事务外Get后发生释放的竞争。故障测试区分保护屏障先提交与case正文事务原子性。

后续定向复核（修复前）：members增加phase并校验其与加密页一致，索引(operation_id,phase,round_id)。Task2在not_started事件事务查询并核验历史START_COMMITTED页，覆盖Process先跑、结果后到的反序及并发；absence证明补expected水位0关联检查（R05/R07/R09）。

整轮审阅更正（修改前记录，R05/R09）：Agent同boot可能在新Manager轮次重放旧snapshot，单凭expected水位不能证明回退，取消watermark_regression自动case。保留较新Manager事实／claims且非空仍waiting，expected仅承接absence历史依据；补带页后结果的换session／Manager重开组合回归，要求没有虚假case。

合法finalized及补观察历史目前保存隔离证据，不会当作已完成释放；普通非空任务可能停在waiting，这是缺少后续双端释放的明确依赖，不是页面接收失败。历史审计目前不自动清理；未验证长期磁盘容量、真实Docker／网络、备份回退、旧库升级及掉电耐久。本轮使用本机数据库与辅助进程，不需要联调环境；进入真实环境前另说明要求。
