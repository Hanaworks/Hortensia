# M0 Manager 扫描接收与业务恢复实施及验收

日期2026-11-07，起点67a8963；编码前基线72bc30a，实现／测试版本00fa64e。状态：Manager本地扫描接收与业务恢复切片R01—R12已验收；整个M0、网络任务接线和双端释放尚未完成。下方初始状态保留开工时记录，最终结果另列。

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

1. [x] 保存两篇基线，复核共享接口及范围；对版本、请求链、业务状态写失败测试并观察真实失败。
2. [x] Task1完成持久版本／绑定／分页／重读；Task2完成跨字段校验、导入／case／取消释放及完成事务；生产文件各有负责人，集成后分别做规格及代码复核。
3. [x] Task3以真实Manager v2／Agent v3交换请求／页面／事件／回执；注入SQL语句／Commit失败、进程终止和重连，直接检查数据库与原文。
4. [x] 完整test/vet/build/mod verify、契约／文档检查及独立审阅；回填逐项结果、版本和限制；同步现行入口并保存本地提交。各轮检查的先后及覆盖范围见下文。

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

## 实施与审阅记录

先提交72bc30a基线，再按存储接收、业务核对、独立故障验证分工实施。新增测试首先观察到缺失类型／API的真实编译失败；集成测试交换两端库生成的信封，不直接改frozen绕过扫描握手。身份绑定及执行结果仍是受控夹具，不能将库组合写成线上任务通道已经完成。

实施中实际观察并修正：错误页未严格绑定scope／scan；诊断SQL错误被掩盖为NotFound；事件在事务外预读之后恰好发生absence释放，旧事件事务仍能覆盖canceled/released；没有许可的START_COMMITTED只进入waiting，后续空扫描可能遗漏此证据；case加密额度耗尽后节点仍可能开放。对应修复分别增加请求关联、保留存储错误、事件最终事务重读released、持久隔离，以及独立保护屏障。SQL冻结失败的测试还验证同Store本地屏障继续阻止许可，不能只检查数据库旧frozen值。

独立审阅和定向失败测试又发现两种事件顺序漏洞：START_COMMITTED页已经保存／处理后，not_started仍得到成功ack；reconciliation_required或暂不支持的历史页已保存但Process尚未运行，新成功结果仍生成成功投影。实际失败曾返回258字节ack，修复后在新事件最终事务读取相关历史原页并拒绝互斥或隔离证据。phase索引被改坏也不能绕过原文校验。上述隔离检查与已持久相同事件的幂等确认分开，不重写旧事实。

另一个修订不是增加限制，而是纠正基线推断：同Agent boot重连可以重放旧固定快照，Manager已有更晚结果并不证明Agent回退。新session及Manager重开组合曾被错误标blocked；移除仅凭expected水位产生的回退case后，保留较新Manager结果、pending finalize和占用，返回waiting。expected仍用于证明absence操作在发起该轮时已存在且水位为0。该规则的事前提议和实施中更正都保留，没有把更正写成最初就已确定。

测试也有一次断言修正：已确认prepare无需在持久cancel后改成suppressed；正确条件是prepare不再pending，并存在pending cancel，保留此前确认事实。后续review未留下阻断发现；最终增加错身份／密钥／DDL、授权缺历史、未知／错目标及不合法absence依据的明确断言。没有新增依赖、改动共享Schema、原v1 DDL或历史归档，也未加入生产故障注入开关。

## R01—R12最终结果

以下依据00fa64e的代码、测试及本轮审阅。测试文件均在`internal/manager/operations/`；“通过”仅指上述切片的条件，未实现或未验证的后续范围见末节。

| 编号 | 结果 | 证据、观察及限制 |
| --- | --- | --- |
| R01 | 通过 | [recovery_store_test.go](../../internal/manager/operations/recovery_store_test.go)直接检查显式版本互拒、新节点／重开冻结、旧Store失去写资格、换Agent boot废弃活动轮；[recovery_business_test.go](../../internal/manager/operations/recovery_business_test.go)的V2RejectsWrongIdentityKeyAndDDL分别拒绝错误身份、摘要密钥和结构；完成组合测试覆盖旧boot回执无效，原v1用例仍通过 |
| R02 | 通过 | [recovery_pages_test.go](../../internal/manager/operations/recovery_pages_test.go)检查分页排序／重复、total变动、游标重复、未知reply_to、空末页及错误页scope／scan关联；[recovery_pages.go](../../internal/manager/operations/recovery_pages.go)最终请求／页面检查与共同Schema共同约束session、ledger、scope、页号和完成条件；两端组合实际交换多页信封，不从调用者接收自报进度 |
| R03 | 通过 | 页body重读与原字节相同；同页重复不增额度；异文保存case。实际构造超过8MiB完整信封、4097条total、超过limit及累计32MiB的页面并拒绝；密文、请求、member位置／缺行、计数、phase被替换均不能读成合法空页。预算测试不等于4096页吞吐或峰值内存测试 |
| R04 | 通过 | [recovery_integration_test.go](../../internal/manager/operations/recovery_integration_test.go)的PendingToAllAbsenceRelease用真实Agent空扫描证明pending遗漏只得到need_all并保留两项claims；升级产生新本地round及Agent scan，保持冻结至最终握手。分页过期重扫测试拒绝旧请求进入新轮；当前轮／旧请求约束同时用于升级 |
| R05 | 通过 | ReadyCancelResultStillWaitsForFinalize检查持久cancel、停止prepare待投递及结果合并后仍保留双方两项claims；LateEventsBeatFixedPageWithoutFabricatingStarted、MultiplePagesKeepSnapshotAndMergeLiveResult检查原结果和较新事实不被旧页回退。ReconnectKeepsNewResultWithOldAgentSnapshot覆盖新session／Manager重开复用旧Agent快照，不产生虚假回退case。扫描页接收无事件ack |
| R06 | 通过 | UnknownAndMismatchedRecordsNeverFabricateOperations、UnsupportedHistoryShapesAreQuarantined、AgentBlockedResultCannotCreateFinalize、ReleasedWrongDigestNeverAcknowledges等直接检查case、原操作数量、原终态及占用；[recovery_fault_test.go](../../internal/manager/operations/recovery_fault_test.go)核对未知事件仅在证据持久后ack，重报不增行／额度；合法超范围历史不会冒充已支持释放 |
| R07 | 通过 | PendingToAllAbsenceRelease及故障测试核对absence依据、取消／released、停止投递和两项claims删除的同事务结果；AuthorizedMissingHistoryKeepsClaims保留permit／两项claims并阻止完成；AbsenceProofMustPostdateAcceptance拒绝引用受理前的空扫描，ReleasedEventFinalTransactionCannotOverwriteAbsence及AbsenceThenReadyRecordPreservesHistoricalOutcome保持原取消事实并隔离新矛盾 |
| R08 | 通过 | EmptyHandshakeAndStaleCompleted覆盖当前空扫描完成、同轮重报、新session／Agent boot／Manager boot旧回执拒绝，另一个节点始终冻结；等待finalize的任务不能准备complete。ExpiredCompletionCanRestartFromDurableFailure用Agent单调时钟推进到15分钟，准备后的请求也可记录过期并新建扫描，过期不解冻 |
| R09 | 通过 | 多页及实时事件组合保留新结果且不制造started；StartedPageConflictsWithLateNotStarted、NotStartedAfterProcessedStartPageIsRejected及IsolatedPageBlocksResultBeforeProcess覆盖前后两种到达顺序；LateUnknownEventBlocksPreparedComplete在complete已准备后加入case，最终回执不能解冻。已持久相同事件保持幂等，不生成新事实 |
| R10 | 通过 | [recovery_fault_test.go](../../internal/manager/operations/recovery_fault_test.go)对页面／case／absence释放／completed四类事务各注入SQL语句失败及延迟外键Commit失败，共8个子项，核对页／索引／进度、原文、操作／claims、accepted及冻结，无部分成功应答；额度消耗不退还。case正文失败时独立保护隔离可以已提交，不能误报为同一事务半提交 |
| R11 | 通过 | [recovery_restart_test.go](../../internal/manager/operations/recovery_restart_test.go)实际启动辅助进程，在完成事务内及提交后返回前分别Kill／Wait；重开核对accepted=0／1的不同历史、请求原文、absence依据与claims，均先冻结后重新握手。库组合检查受控开始次数；没有运行Compose，进程终止不等于掉电耐久 |
| R12 | 通过 | 页密文／绑定／索引破坏被拒；包装额度耗尽后旧页及已存事件仍可重读／幂等确认，case新写失败保持隔离。v1、Agent、通道回归与最后Manager整包均通过；[规格第7节](../specifications/m0-manager-recovery.md#7-实施后的接手路径与完整实例)补代码职责、完整流程和诊断规则，README／组件／数据／开发／测试／规划同步。检查批次详见下节 |

## 运行环境、检查与复验

环境：Windows/amd64、Go1.27.0、CGO_ENABLED=0、modernc.org/sqlite v1.61.1／SQLite3.54.1、PowerShell7.6.9；沿用当前隔离工作区及依赖。测试使用临时数据库、本机回环通信和辅助进程，不需要用户提供联调环境。

- 全量`go test ./... -count=1 -json`通过6个测试包，共971条测试／子测试pass，失败0、测试skip0；Manager18.330秒、Agent20.757秒、通道15.801秒，仅记录本次耗时。父测试及辅助入口包含在统计内，不等于971个独立场景；contracts嵌入包的package skip不算测试跳过。
- 上述全量运行之后，仅Manager继续修正隔离页优先于新结果并补充验收断言；最终`go test ./internal/manager/operations -count=1 -json`完整通过184条测试／子测试、失败0、skip0，包耗时15.562秒，对应00fa64e。其余包代码未再变化；不把后增测试混入971条全量统计，也不声称全量批次已经含最终修正。
- `go vet ./...`、`go build ./...`、`go mod verify`通过。契约检查通过20类消息、21正例、11反例及1个解码输入；文档链接／锚点、表格列数、围栏和Git空白检查通过。静态检查不替代语义审阅、代码测试或外部环境验证。
- 基线72bc30a、实现／测试00fa64e及随后交付文档提交分别保留开工、实现、验收时点。本地忽略目录中的`.cache/m0-manager-recovery-final-tests.jsonl`及`.cache/m0-manager-recovery-delivery-package.jsonl`保存对应原始运行记录，不作为版本化交付物；下面命令可重新生成结果。

在仓库根运行`go test ./internal/manager/operations -count=1`复验Manager库；完整回归运行`go test ./... -count=1`，须允许本机回环连接及辅助进程。工程检查为`go vet ./...`、`go build ./...`、`go mod verify`、`pwsh -File tools/check-contracts.ps1`、`node tools/check-docs.mjs`和`git diff --check`。查看故障／终止实验可定向运行`go test ./internal/manager/operations -run 'TestRecovery(Fault|Restart)' -count=1`。

## 完成边界与下一依赖

本轮交付源码、可重复的本地组合验证及维护说明。能够完成的是空账本恢复，或可信all证明从未授权／观察的任务缺席后取消释放，再经当前Agent回执开放该节点；存在Agent操作记录时，结果已保存也仍可能waiting，因为双端finalize／finalized尚未实现。页面收齐、业务就绪、Agent接受complete和Manager提交completed是不同事实。

下一模块先定义并实现双端finalize／finalized释放，包括持久回执、重复投递、双方占用解除和恢复扫描中的终结历史，再接受控网络任务通道。仍未实现／未验证：真实Compose执行和跨机部署、线上身份认领与轮换接线、补观察结果版本、case人工解决／隔离解除、旧库升级、备份回退、掉电耐久、长期审计保留与生产容量。本轮没有失败或阻塞的R01—R12项；这些后续项不因此视为通过，也不能据本切片宣称M0整体完成或产品可发布。进入需要真实Docker／Compose或跨机网络的验证前，再告知用户具体环境要求。
