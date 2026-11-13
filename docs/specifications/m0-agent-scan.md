> 现行用途（2026-11-14归档整理）：本篇保留仍有代码入口的旧格式API及被M1明确引用的基础规则，不是当前阶段的实施计划。正式schema5／protocol1的完整运行与差异见[Agent v5](m1-agent-runtime.md)、[Manager v5](m1-manager-runtime-store.md)和[正式通道](m1-agentwire.md)。本篇已结束的实施／验收历史从[归档目录](../../archive/2026-11-14-documentation/README.md)查阅，不能把旧格式参数或当时状态套用于正式服务。

# M0 Agent 恢复扫描实施规格

日期2026-11-07，起点2de48fd。本篇与[实施计划](../../archive/2026-11-14-documentation/m0-implementation/m0-agent-scan.md)在编码前以9f6c34c保存，源码／测试版本a34a769。状态：S01—S12通过，Agent本地扫描切片可供源码交接，整个M0尚未完成。实施中修订及实施后接手说明保留各自时点；技术细节为本轮实现选择，不表述为用户逐项确认。

## 1. 目的与完成边界

本轮开工前Agent只能读取一条RecoveryRecord，Manager不能据此确认已经取得整本账本。本模块在`internal/agent/ledger`增加完整身份枚举、固定记录快照、分页、变化事件确认屏障及持久扫描结束回执。共享字段引用[Agent协议§7.2](agent-protocol.md#72-恢复扫描)、[Schema](../../contracts/agent-v1.schema.json)、[数据设计§7.2](../data-design.md#72-agent-表与边界)；开始／事件主定义继续由[账本](m0-agent-ledger.md)、[日志](m0-agent-journal.md)维护。

必须区分三个完成：页面complete仅为枚举结束；reconcile_completed证明Agent完成本轮扫描检查并关闭扫描冻结；Manager还须核对旧许可、占用、变化结果、缺失历史和必要释放，持久保存completed后才能开放新受理。当前交付只覆盖前两项。可信夹具提供已认证session和Manager完成请求，不提供HTTP或网络认证，也不代替Manager业务判断。

本轮不实现finalize、finalized历史、归属基线、补观察、Manager页仓储／恢复判定、在线任务和真实Compose。当前全部executions均未finalized，因此pending与all的实际集合相同；二者仍保留独立scope及替换规则，不声称已验证历史过滤。扫描完成绝不删除资源claims，不复活旧Gate、nonce或旧boot任务。

## 2. 方案与版本

采用显式Agent v3新库：保留v1／v2完整DDL及入口，新增`003_recovery_scan.sql`、CreateRecovery／OpenRecovery。复用既有文件、pragma、密钥及精确结构校验；内部版本选择统一，不复制两套存储框架。三版入口互拒，失败不删库／重建身份；没有v2→v3迁移，不把新建实验库当作真实节点升级方式。

固定身份集合、序号引用、数量、冻结及最后完成记录持久保存；含latest_result的完整记录仅保存在有界内存快照，正文仍由既有加密事件提供。相比额外加密整份临时快照，这样不消耗KEK包装额度，并符合重启／缓存丢失必须scan_expired的协议边界。若只保存内存身份集合会违反数据设计的持久要求；若直接持久明文RecoveryRecord会泄漏结果，因此两者均不采用。

v3 Create／Open均建立新的active_boot、frozen=1；Open保留变化集合、成员及最后completed，旧活动扫描因无本boot缓存失效。v3所有写入口复核持久active_boot，旧Store不能写入或预留新加密额度；旧版生命周期保持原规则。Open撤销新写入资格，不证明已经进入的旧执行或Docker请求停止。

## 3. 接口、责任与内部数据

| 入口 | 输入／输出 | 调用方及副作用 |
| --- | --- | --- |
| CreateRecovery／OpenRecovery | 原Journal参数加RecoveryOptions{Now}；返回Store | Now为空使用time.Now；自定义时钟只供受控测试，必须单调；新v3初始冻结 |
| BindRecoverySessionFixture | 当前session_id | 调用者已认证；不同session失效现有Gate并持久冻结，保留同boot扫描缓存；相同session不重复冻结已完成状态 |
| RecoveryScanState | Frozen、Dirty、SessionID、ActiveScanID、CompletedScanID、Total | 可信诊断读取；不是准入许可令牌 |
| Reconcile | 合法完整reconcile信封；返回reconcile_reply | 事务复核当前session／boot；新扫描固定成员与记录；返回当前message_id/reply_to关联，不复用旧信封 |
| CompleteReconcile | 合法reconcile_complete；返回reconcile_completed | 复核ledger、当前会话、活动缓存及末页、dirty和变化确认，再同事务保存completed／解除扫描冻结 |
| 既有prepare／Gate／result／ack | 原接口 | v3在原事务加入屏障或变化跟踪；v1／v2不改变行为 |

成功响应直接使用现行Schema，不增加线上字段。输入非法、错误session、存储失败等返回Go错误且无成功报文；ErrScanExpired映射scan_expired，ErrScanCursor映射cursor_invalid，ErrScanBusy映射busy，ErrScanIncomplete／ErrScanChanged表示不能完成。实际线上error页／protocol_error映射和发送由未来通道适配器承担；本轮函数不是完整网络处理器。

同一连接message_id不可对应不同body的通用约束仍由通道适配器检查，本地扫描模块不另建无限请求ID缓存。RecoveryScanState.Frozen是持久冻结值；冻结事务失败后localFrozen可能仍为true，实际准入更严格，不能用诊断读取代替写事务检查。

| 持久对象 | 本地定义与来源 | 生命周期／约束 |
| --- | --- | --- |
| recovery_control单行id=1 | active_boot、current_session、frozen、active_scan_id、active_scope、active_limit、active_total、dirty、completed_scan_id、completed_scope、completed_total | 空ID／scope用空串；未有total为0；frozen/dirty为0/1；有效active必须frozen；completed保留到下一次成功完成替换 |
| recovery_members | scan_id、ordinal、operation_id、last_event_seq、result_event_seq、phase、report_acked、record_fingerprint | 固定按operation_id升序；ordinal从0；唯一(scan,ordinal)及(scan,operation)，operation外键；无result序号0；HMAC绑定原投影。保存当前活动及最近已完成集合，替换旧活动或下一次完成才清理旧集合 |
| recovery_changes | operation_id、event_seq | 联合主键及事件外键；整个连续冻结期的新事件集合；重连／升级scope／过期／重开均保留，ACK不删除；成功完成才清除 |
| 内存recoveryConfig | 独立scan互斥锁、Now、localFrozen、一个scanSnapshot | localFrozen仅由Store.mu保护，用于新Gate登记；scan锁串行页面／会话切换／完成，不在事件写事务取此锁 |
| 内存scanSnapshot | scanID、scope、limit、deadline、records原字节、对应members、已签发游标／页、已枚举末页标记 | 一个Store最多一份已发布快照，每份最多4096记录及32MiB原投影；单条受既有7MiB事件预算及完整8MiB信封约束；过期／替换／完成／Close清理正文 |

fingerprint为HMAC-SHA256(K_ledger,`dm-scan-record-v1`+0字节+原RecoveryRecord字节)，不作公共input_digest。正文不写入SQL、WAL、错误或日志；缓存生命周期终止尽力clear，不承诺Go内存彻底擦除。持久成员可用于核对扫描身份，不允许重开时据此复活单调计时或分页资格。

2026-11-07最终源码审阅补充容量说明：pending→all或dirty重建时，在新快照提交前保留旧快照；失败保留旧活动身份及缓存，只有原扫描未dirty且仍有效时才可继续使用。此时两份原投影合计最多64MiB，另外还有单条解密、JSON校验、完整页面、成员／游标及分配器的临时开销；32MiB不是整个进程的内存上限，未做真实节点峰值内存测量。替换成功清旧缓存，失败清未发布的新缓存。

## 4. 冻结、快照与分页

### 4.1 冻结先于发布

1. 在Store.mu下设置本地冻结并复制Gate列表，随后释放锁，逐个Invalidate，等待已进入的短Start回调返回。新Gate最终登记必须再查localFrozen。
2. 单独事务验证active_boot／session并持久frozen=1。发生错误仍保持本地拒绝；Open默认冻结，不能通过重开绕过。
3. 在另一个立即事务中读取有界、排序身份集合，关闭游标，再用内部`readRecoveryRecord(ctx,q,id)`读取同一事务快照。构造全部独立RawMessage副本，计算成员元数据及指纹。
4. 在该事务保存新scanID／scope／limit／total与成员，清dirty，成功后才替换内存缓存；pending→all失败保留旧活动身份及缓存，冻结持续。超条数／字节预算或坏证据不能发布部分集合。

新prepare在其最终事务查冻结；NewGate在预读及最终登记查屏障；CommitStart在原开始事务重新查。扫描不得持Store.mu、数据库事务或唯一连接调用Gate.Invalidate；否则Start回调的Get／Claims可能反向等待。Start必须短且不能调用会话绑定、扫描／完成、Gate注册或Close；扫描不停止已经运行的实际任务。

固定快照捕获之前已赢得竞争的START_COMMITTED事实必须保留。Gate在冻结过程中已经提交但还未调用Start时，被Invalidate后不再Start；不能据此把持久开始事实改成not_started。扫描关闭不恢复已失效Gate。

### 4.2 首请求、游标和大小

首请求scan_id/cursor都为null；scope pending/all，limit数学整数1—200。活动有效扫描遇到同scope、同limit的null/null重试，返回原首页面、不延长deadline；改变limit拒绝。唯一允许主动替换的有效活动请求是pending→all；不允许all→pending降级。dirty、过期或缓存丢失后null/null可以新建扫描，持续冻结。

已知scan_id且cursor=null表示重读首页面；后续只接受该扫描签发的不透明随机游标。scope／limit必须与创建时一致；未知scan、篡改游标、跨扫描游标及旧session拒绝。游标指向固定页起点，不能由调用者构造offset；只有请求前页后签发的游标才可请求后页，重读已发页面不增加枚举计数。旧scope升级的ID／cursor／complete均失效。

页面按请求limit与完整8MiB编码信封双重切分，包含所有元数据、逗号及next_cursor。只编码信封与页头，拼接已验证记录原字节，绝不再次json.Marshal记录／latest_result。非空集合不能返回空末页；无法容纳单条记录时报错并保持冻结。成功末页next_cursor=null、complete=true，总数固定；只有确实生成／提交末页资格后才允许Complete。网络送达／Manager完整性仍由接收方判断，不以函数返回证明对端收齐。

单调deadline为成功建立快照时所用起点加15分钟；重复、重连、ACK及翻页不延期，到达边界即失效。无跨重启计时恢复。到期只有新null/null才能重新扫描，不因到期解冻。

到期缓存采用访问时清理：下一次扫描／完成调用观察到过期时清除内存原文；没有后台计时线程。期限在取得数据库事务后以及发布页面／完成前复核，数据库锁等待不能把已过期扫描变成有效末页。内存占用即使无人再访问也受单快照预算限制。

## 5. 扫描期间变化与完成

旧执行可以继续写result，未开始取消可以写not_started；开始阶段仍禁止新START_COMMITTED。新事件与recovery_changes在同一事务写入。RecordResultFixture保持原PreparedSession本地执行关联；新连接发送及ACK则用当前绑定session，不能把此夹具接口当网络接收器。

v3 AcknowledgeEvent在原确认事务检查active_boot及current_session；旧session不能覆盖当前确认。变化索引不会因S1已ack而删除，S2续扫仍须重新ack这些事件。pending→all、扫描到期或重开不丢连续冻结期间的索引；固定旧页面的report_acked不随确认变化而改写。

同位异文冲突只改变blocked、没有新event_seq：同一冲突事务将控制dirty=1。已有快照的翻页／Complete拒绝，必须null/null重新固定包含隔离事实的快照，旧结果ACK不能代替新冲突已被观察。新扫描包含blocked记录不代表Manager可以解冻未知目标；该业务裁决仍属于Manager。

Complete对当前活动扫描检查：缓存完整且未到期、scan／ledger／session／boot一致、已枚举末页、持久成员及total与缓存一致、dirty=0，且recovery_changes全部存在并在当前session得到ack。检查与completed写入、旧完成成员清理、清活动状态／变化集合、frozen=0同一事务；Commit成功后才解除本地屏障、返回回执。写入失败无成功响应、无部分解冻。

若请求scan_id正是持久最近completed，返回相同body的新信封，不改变当前active、frozen或变化集合。即使Agent已经重启或开始另一扫描，也只重放历史回执，不用旧完成记录结束新的冻结。下一次成功完成才替换最近completed；更老回执不保证重放。

## 6. 异常、维护与验证

创建／关闭复用既有SQLite WAL/FULL／外键／5秒锁等待及密钥清理；context只约束数据库工作，Gate失效等待依赖短回调约定，不能称所有步骤硬超时。Close先禁止新调用／失效Gate，释放数据库后清扫描缓存，锁顺序不跨DB持Store.mu；扫描本身不预留KEK，额度耗尽仍可读、分页、确认及完成。

排障先查ledger与版本、active_boot/current_session/frozen，再查活动scan／成员数量／dirty、变化事件的ack_session，最后查completed。不能按墙钟日期推测缓存仍有效，也不能因一条complete=true就删除占用。改分页查原文字节和完整信封预算；改事件／冲突查同事务变化登记；改会话查新连接重ACK；改关闭／Gate查反向等待；改v3结构先定升级及兼容验收。

稳定验收S01—S12见[实施记录](../../archive/2026-11-14-documentation/m0-implementation/m0-agent-scan.md)。交付前核对最终源码、运行证据及维护说明，保留本次事前基线和实施修订时间。真实Docker、Manager业务恢复、线上联调、升级、备份回退和掉电耐久均不能由本切片代替。

## 7. 代码接手与完整实例

本节于2026-11-07实施后补充，事前规则仍以9f6c34c及本篇标明的修订为依据。

| 入口／文件 | 处理与维护规则 |
| --- | --- |
| [scan_store.go](../../internal/agent/ledger/scan_store.go)、[store.go](../../internal/agent/ledger/store.go) | 明确选择v3格式；校验文件／密钥／DDL后注册新boot并冻结；失败清理未发布Store持有的密钥副本，Close不持Store.mu等待DB或扫描锁 |
| [scan_barrier.go](../../internal/agent/ledger/scan_barrier.go) | 会话绑定串行化；freezeRecoveryGates先设localFrozen再锁外Invalidate；原写事务检查active_boot/current_session/frozen及本地屏障，故障后保持保守拒绝 |
| [gate.go](../../internal/agent/ledger/gate.go)、[records.go](../../internal/agent/ledger/records.go) | 构造Gate的预读不授权；最终登记看本地状态，开始／prepare提交看事务状态。完成扫描只允许未来流程，不恢复已失效Gate |
| [results.go](../../internal/agent/ledger/results.go)、[events.go](../../internal/agent/ledger/events.go) | insertEvent同事务增加变化索引；同位冲突置dirty；ACK核当前session但不删索引。不得把事件确认、扫描完成及claims释放合并 |
| [recovery.go](../../internal/agent/ledger/recovery.go) | readRecoveryRecord接受caller的querier，内部读取同一事务中的完整事件／占用与许可；公开RecoveryRecord继续提供单操作事务包装 |
| [scan_snapshot.go](../../internal/agent/ledger/scan_snapshot.go) | 独立提交冻结后构建完整快照；先关闭身份游标，再逐项解密；持久members与活动控制原子替换；scanWire只拼接原文，verifyScanMembers检查数据库水位／HMAC与内存固定成员一致 |
| [scan.go](../../internal/agent/ledger/scan.go) | Reconcile处理首请求／续页／升级；页面只在事务和期限检查成功后签发下一游标／末页资格；Complete检查变化当前确认并原子写回执，旧回执重放不改控制状态 |

例：S1先完成空扫描并准备任务A，随后开始扫描X。固定记录仍显示A为ready。A在冻结期间收到取消并保存not_started/seq1，X的页面不改，但recovery_changes新增(A,1)，Complete被拒。S1确认后，若换成S2，原变化条目继续存在，旧ack_session=S1仍不足；S2重新确认后，可信Manager若已经完成自己的业务核对才发送Complete。Agent提交completed(X)、清变化索引并解除扫描准入，A的两项claims仍保留，等待另外的finalize流程。

若完成回执丢失后Agent重启，Open将frozen置1且丢失活动缓存。Manager重发Complete(X)只取得同一持久完成body；新冻结不会因此解除。要重新开放扫描准入仍须从新首页完成另一轮。若X页面生成后出现异文冲突，则dirty优先阻止X翻页和结束；重新固定快照会显示reconciliation_required，不能靠旧结果ACK掩盖冲突。

正文快照、成员指纹及变化集合各有不同作用：正文保证翻页不读到新状态，持久成员证明枚举过哪些身份／水位，变化集合补充固定快照之后的事件。任何一项缺失、过期或错关联都不应被解释为空账本。恢复的业务决策及资源释放尚在Manager／finalize模块；未来接线须继续走独立实施规格与验收流程。
