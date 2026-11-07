# M0 Manager 扫描接收与业务恢复规格

日期2026-11-07，起点67a8963。本篇与[实施计划](../implementation/m0-manager-recovery.md)在编码前保存并提交为72bc30a；实现／测试版本00fa64e，R01—R12已完成本地库切片验收。以下是依据现行主规格的技术落实，不表述为用户逐项确认，也不代表整个M0完成。

## 1. 目的、依据与本轮终点

Manager此前只有固定操作、许可、首事件／结果及finalize意图，重连后始终冻结。本模块在同一operations包增加持久扫描收件箱、恢复判定、缺失历史隔离、可信无记录取消及完成回执屏障。依赖[原协调库](m0-manager-operations.md)、[Agent扫描](m0-agent-scan.md)、[协议§7.2](agent-protocol.md#72-恢复扫描)、[恢复矩阵](execution-recovery.md#51-正常重连和扫描)、[数据设计](../data-design.md#71-manager-表与约束)和[共享Schema](../../contracts/agent-v1.schema.json)。共享RecoveryRecord／ExecutionResult不重新定义。

必须分别记录：页面收齐、业务可结束、已收到并持久保存Agent完成回执。页面complete和report_acked不能直接开放受理。正常空账本可以完成；可信all扫描证明无记录且本库从未授权的旧任务，可以按既有例外取消并释放Manager占用；已授权缺记录、额外未知历史、矛盾或不支持的证据保持隔离。已知首结果照常合并，仍待finalize的终态保留pending及claims，不能提前结束恢复。

本轮不实现Agent finalize、在线finalized处理、补观察版本、真实Compose、HTTP／网络任务处理器、备份回退及旧库迁移。合法finalized历史和超出首结果序列的证据加密保留为case并阻止完成，留待双端释放模块继续处理。并非声称已支持所有协议历史。正常重开假设数据库没有回退；没有反回退证明，不提供可写备份恢复入口或强制解锁。

## 2. 方案、生命周期与接口

使用显式新Manager v2库，保留v1 DDL和Create/Open行为。原地修改v1会让旧验收与存量格式失真；独立新服务又会重复既有事务／密码逻辑，因此选择同包显式版本入口。v2 Create/Open及新节点均冻结；Open更新active_boot并废弃旧活动轮次的完成资格。只改变核对成功节点的frozen，不把全局recovery_mode清零来开放其他节点。

页面先完整加密落库，再由幂等业务核对消费。这样能在崩溃后按持久页继续处理，而不在持有唯一连接的页面事务中调用会自行开事务的ReceiveEvent。收件箱提交不是业务通过；最终完成请求和完成回执事务均重新核查当前全部操作、claims及未解决case。

| 入口 | 输入输出与责任 |
| --- | --- |
| CreateRecovery／OpenRecovery | 参数沿原Create/Open；返回v2 Store，版本互拒，无自动升级 |
| BindRecoverySessionFixture | Binding及可信hello的agentBootID；事务保存当前绑定，换session／任一boot后废弃旧轮完成资格并冻结 |
| StartRecovery | Binding、scope、limit；保存轮次和首页请求后返回完整reconcile；同轮同参数重复重放待发请求；pending→all或expired可新建，其他有效轮次不随意替换 |
| NextRecoveryRequest | Binding；按已提交next_cursor创建或重放唯一待发页请求；不接受调用者自报offset／cursor |
| ReceiveRecoveryPage | Binding及完整reconcile_reply；原子保存原文页、身份索引、数量与后续游标，返回RecoveryState；不返回事件ack |
| ProcessRecovery | Binding；幂等合并持久页面与实时事件、产生取消／隔离／无记录释放，再返回RecoveryState；未收齐拒绝处理为完成 |
| PrepareRecoveryComplete | Binding；重新检查业务已可结束，持久保存reconcile_complete请求后返回信封 |
| ReceiveRecoveryCompleted | Binding及完整回执；检查当前请求、轮次、双方boot与session及业务，原子保存completed并解冻当前节点 |
| RecordRecoveryFailureFixture | Binding、当前requestID、可信适配器错误码；scan_expired/scan_changed/cursor_invalid持久标expired并允许重扫，busy保持原请求重试；绝不解冻 |
| ReceiveRecoveryEvent | Binding及真实started/result；已知操作复用原接收规则并只在提交后ack；未知操作加密保存case后ack，持续隔离；不伪造Operation |
| ReadRecoveryState／ReadRecoveryPage／RecoveryCases／ReadRecoveryCase | 可信内部诊断、加密证据重读；不是HTTP输出或准入令牌 |

v2必须通过BindRecoverySessionFixture绑定，原BindSessionFixture只服务v1，防止漏记Agent boot。即使Agent能重放旧completed，新的Agent boot／连接也不能据此解除新的冻结。本轮Manager在换会话或重开后从首页重新扫描；不复用旧分页／complete资格。Agent自己的同boot续扫能力仍保留，Manager可按新轮从首页重新收齐未过期固定快照。

保留既有单连接immediate事务、WAL/FULL、外键、5秒锁等待与密钥管理。扫描入口使用独立recoveryMu串行化；事件接收不取此锁，以免形成嵌套等待。所有写事务重新检查active_boot与当前绑定。context约束数据库调用，未引入后台投递／硬总时限。Agent的15分钟期限为权威；收到scan_expired后保持冻结并从首页重建，不恢复墙钟期限。

2026-11-07业务编码前审阅补充（R08/R09）：扫描已收齐甚至已准备complete后仍可能到期或出现冲突，可信本地适配器用RecordRecoveryFailureFixture记录当前请求的明确失败；也适用于本轮两端库组合中的等价Go错误。旧请求、旧session及未知错误不能废弃当前轮；仅响应丢失不记为失败，继续重发持久请求。线上protocol_error接线仍在后续模块。

## 3. 持久数据与内部协作

新增`002_recovery.sql`包含v2完整DDL，不改001。重要字段如下；缺省ID／cursor用空串，未收到total用-1。公开诊断Total可为-1，不能当空集合。

| 对象 | 关键字段、含义及来源 | 约束与生命周期 |
| --- | --- | --- |
| recovery_nodes | node_id、agent_boot、active_round、quarantined；分别来自可信绑定、当前轮次及已观察隔离条件 | node外键；新绑定清活动关联并冻结，但不清quarantined或历史证据 |
| recovery_scans | id、node_id、ledger_id、session_id、manager_boot、agent_boot、scope、page_limit、scan_id、status、total、seen、pages、body_bytes、next_cursor、last_operation | id为本地轮次，scan_id为Agent首页分配；status为collecting/enumerated/need_all/waiting/blocked/ready/completing/completed/abandoned/expired；进度与页同事务；双方boot禁止旧回执解冻 |
| recovery_requests | message_id、round_id、kind、page_no、cursor、raw、accepted | kind仅reconcile/reconcile_complete；raw是无秘密请求；同逻辑页只保留一条，重试不增长请求行；reply_to必须对应当前轮及当前session |
| recovery_pages | round_id、page_no、request_id、body_cipher、wrapped_dek、key_id、fingerprint | 唯一(round,page)，原body加密保存；请求及AAD绑定轮次／节点／ledger／scope／scan／limit／cursor／页号；正文从不写SQL明文 |
| recovery_members | round_id、ordinal、operation_id、page_no、record_no、phase | 唯一(round,ordinal)及(round,operation)；保存原页位置、身份及阶段索引，阶段必须与解密原页一致；严禁缺页／索引损坏被解释成空扫描 |
| recovery_expected | round_id、operation_id、last_event_seq | 在Start冻结事务固定该节点本库各操作及已知水位，用于absence证明该操作当时已存在且无事件；复合主键及round/op外键，不单凭它认定Agent历史回退 |
| recovery_cases | id、node_id、ledger_id、operation_id、kind、round_id、evidence_cipher、wrapped_dek、key_id、fingerprint | operation允许本库无记录；kind为稳定诊断原因；相同节点／操作／原因／原文指纹去重；正文加密；当前无通用解决／删除入口，任何case阻止该节点完成 |
| absence_releases | operation_id、round_id | 仅可信all缺席且从未授权的取消证明；引用持久完整扫描，和取消终态／释放claims同事务；不制造Agent结果或finalized |
| operations.release_status | v2新增released | 本轮只由absence_releases证明；原v1仍只有held/pending，常规终态仍需未来双端释放 |

每轮最多4096条记录、32MiB原始页面body、4096个数据页及一个完成请求；单页完整信封仍不超过8MiB且不超过请求limit1—200。非空页必须推进，空账本允许单一空末页。重试不重复保存或消耗包装额度；新页失败可能消耗已提交额度，不退还。各轮和case是审计证据，当前不自动清理已保存历史，磁盘随不同恢复轮次增长；这不是长期保留策略或容量已验证的产品。未来清理必须保留absence_releases引用依据及未解决case。

页／case复用既有随机DEK和KEK额度机制，但采用独立Kind用途和完整上下文绑定；内部blobBinding的OperationID位置对这些用途保存roundID／caseID，Digest位置保存规范JSON上下文而非公共input_digest。原payload／event的AAD字节规则不改。页与case重读均检查AEAD、指纹及共同Schema／本地关联。临时原文尽力clear，不承诺Go内存彻底擦除。

存储层负责版本与boot；收件箱层负责请求链、分页及解密重读；业务层消费完整页并对照固定操作、历史许可、事件及claims；完成层把业务校验与当前回执联系起来。普通事件仍经既有事件库处理。页面内latest_result可调用同一内部接收核心，但不制造网络result信封或ack；真实事件重报才有自己的message_id及当前连接ack。

## 4. 页面接收、正常与异常流转

1. Start在事务内冻结节点，保存本地round与首页reconcile；重试返回原请求。页响应先共同校验，再查持久reply_to、当前session／双方boot与ledger。
2. 按固定scope／scan／total及请求页号接受；operation_id全局严格递增、唯一；页内数量不超limit，累计不超total，complete时累计必须等于total且next_cursor=null。游标不得回环，不可跳过尚未提交页；错误页不计完成。
3. 新页先做无新包装重复预查，事务外seal，最终事务重新检查全部关联，再同时写密文页、成员和进度。重放同body不增长计数／额度；同页异文保留原页，另外持久case再返回冲突，重试原页及重开不能消除隔离。无成功进度绕过存储失败。
4. readRecoveryPages重读时解密校验全部页面、请求关联、身份索引与计数，不能只信可修改的计数列。Process在收齐之后处理；中途失败保留收件箱和已提交事实，可幂等重做。
5. pending缺任何未释放的本地任务，状态need_all；Start(all)生成新的本地轮次与null/null请求，旧轮标abandoned，冻结连续，旧响应／complete不再有效。Agent总数／结构超预算返回拒绝并冻结，不截断继续。

RecoveryRecord的目标／摘要／result身份及序号必须相互一致；last_event_seq不得低于其嵌入result序号，无result时report_acked=false。phase、claims和final字段按Schema及恢复矩阵校验。固定页可以落后于已接收的新事件，不能把较旧页面覆盖较新持久结果；同一位置异文必须隔离，不能因实时事件较新而忽略矛盾。

2026-11-07业务实现前补充（R05/R09）：Start同事务固定recovery_expected水位。若页面低于扫描启动前Manager已经知道的水位，保存watermark_regression case；只有启动之后新增事件才允许领先于旧页，防止把Agent丢失历史误判为正常页后变化。

2026-11-07整轮审阅更正上述水位判定（修复前保存）：Agent同boot对null/null可能重放旧快照，Manager新轮开始并不证明Agent刚建立快照，故前段“低于expected即永久case”的方案废止。保留expected作为无记录释放的本地历史依据；较旧页只保持已有Manager事件／终态及占用、等待后续核对，不回退也不凭水位制造冲突。真正同位异文、started与not_started互斥仍隔离。当前非空Agent扫描都不能走自动释放；若后续all中缺失，已有permit或事件仍阻止absence例外。

## 5. 业务判定与完成屏障

| 证据组合 | 持久动作与下一步 |
| --- | --- |
| 已知目标／摘要不符，或本库证据损坏 | 保存隔离case，保留原操作／claims，不自动修复 |
| 本库没有该操作 | 保存含原RecoveryRecord的case，不补造用户、端点、配置正文或受理时间 |
| 无许可且Agent preparing／ready | RequestCancel规则持久取消并停投旧prepare；等待not_started，不能再授权旧任务 |
| 有许可且ready／START_COMMITTED，无可靠结果 | 等待持久未开始或真实结果，保持冻结与占用；不根据扫描臆造started时间或permit ID |
| 有合法首结果 | 按原结果投影幂等导入；同位异文隔离；终态pending保留finalize意图／claims，报告waiting，不发complete |
| unknown／interrupted／漂移等未知结果 | 原规则保持核对及claims；不得用扫描结束自动解锁 |
| all完整证明未出现、本库无许可／无事件／未blocked | 同事务取消、抑制prepare与cancel、写absence_releases、删该操作两项claims，state=canceled/release_status=released；引用完整扫描作为依据，不写虚假result |
| all缺已授权或已有事件操作 | 保存缺失历史case，保留许可／原事件及claims；不会走未授权取消例外 |
| finalized历史或超出当前首事件序列 | 加密保存unsupported_history case，保持隔离，后续双端释放／补观察模块处理 |

从页面导入首结果与实时ReceiveRecoveryEvent都沿相同事件规则；页面没有ack，report_acked不代替当前变化确认。当前真实事件指向未知操作时，先保存case再返回指向该事件的ack；这样保留证据但不开放节点。已absence释放的操作后来出现任何Agent事件，也保存case并冻结，不能回写历史取消／重造claims掩盖矛盾。

2026-11-07编码开始后的基线审阅补充（业务实现前保存，R05/R06）：Agent的phase=reconciliation_required优先于嵌入结果，即使latest_result原文看起来可终结，也可能来自同位异文冲突后的原证据。先保存case、隔离同节点未释放操作并抑制已有finalize；原页加密保留结果，不按该页导入“成功”覆盖隔离。对于已释放历史只建case并冻结节点，不覆盖原取消终态。

同次审阅补充R03/R06/R07/R09：absence释放后任何后续扫描记录（包括无事件的ready）也构成矛盾case；释放最终事务重查该节点无case，不能遗漏期间已入隔离表的新证据。真实合法但超出首事件序列的消息先保存unsupported_event case后ack，不送入只支持seq1/2的旧核心；已知操作的目标、摘要或permit矛盾先保存case且无成功ack。任何case都保持节点冻结，不用简单错误返回代替持久隔离。

2026-11-07实现中审阅修订（修复前记录，R06/R09/R10）：case可能在节点已开放时发现，而加密额度耗尽或SQL失败会阻止保存正文。因此先设本地节点屏障，再独立事务持久frozen=1/quarantined=1，随后才预留加密与保存case；后两步失败无ack但不消除隔离。quarantined跨新轮／重开保留，当前无清除入口；同Store本地屏障也由受理／许可最终检查，解决持久冻结本身失败的窗口。此保守屏障是单独提交的保护事实，不将它称为case事务半提交。另在原事件写事务再次检查released，防止事务外预读之后恰好发生absence释放，必须转隔离而非覆盖历史取消。

同次修复复核补充R05/R09：已存扫描中的START_COMMITTED是开始事实证据，不能只在Process时检查。members的phase索引用于定位原页；接收not_started的最终事件事务也必须解密核验该操作任一已存开始页，存在时转冲突隔离，不能在Process返回waiting后再写出互斥终态。它不伪造started消息、时间或permit ID。absence证明还须引用包含该操作expected水位0的原轮次，不能拿受理之前的空扫描作为依据。

该事务检查也覆盖已存页的reconciliation_required及本轮不支持的历史形状；即使新的成功result与页内原结果相同，仍不能在Process之前生成成功投影／finalize。已持久事件同原文重报仍可ack，不重写旧事实，后续业务核对负责抑制与隔离。

PrepareComplete与ReceiveCompleted各在最终事务内检查：当前轮已完整、当前绑定未变、无case、所有本地操作都具有有效释放证明且无claims、无待解决业务。旧操作的owner_boot／prepared_session／permit不被改成当前值。正常空库也必须走两次握手；未保存回执前节点始终冻结。completed重放只在同一仍有效轮次幂等；新轮、换boot/session后旧响应不得改变冻结。

业务状态不是授权布尔参数，外部不能直接标记“已核对”。单节点完成不影响其他节点。请求等待／错误页／context取消不产生取消业务事实或释放资源。无需真实环境即可用本地两端库及受控结果验证；这不等于网络消息已接入。

## 6. 维护、验证与边界

诊断按node/ledger/双方boot/session → 当前round及请求关联 → 页与members一致性 → case → 操作原许可／结果／claims → absence依据 → complete请求／回执顺序查看。修改分页须查原字节与双预算；修改解冻须同时检查重开、旧回执、并发新事件及其他节点；修改释放必须保留独立证据分支，不能仅允许released字符串。

验收稳定编号R01—R12及逐项证据见[实施记录](../implementation/m0-manager-recovery.md)。覆盖真实SQLite失败／Commit失败、重开与辅助进程终止，及Manager v2与Agent v3的本地组合。运行环境沿既有Go／SQLite组合；需要Docker／Compose或跨机验证前再告知用户，当前不需要联调环境。第7节是实现后的源码入口及完整实例，事前基线和实施中的更正记录均保留。

## 7. 实施后的接手路径与完整实例

本节于2026-11-07实现后补充。事前基线为72bc30a，前文保留实施中规则补充和水位判定更正，不将其回写为最初已经完备。

| 代码入口 | 内部协作及修改时必须保持的规则 |
| --- | --- |
| [store.go](../../internal/manager/operations/store.go)、[002_recovery.sql](../../internal/manager/operations/migrations/002_recovery.sql) | 按明确version选择完整结构；原v1入口不接受v2。新Store持有独立密钥副本，Open撤销旧boot并冻结节点，未发布Store失败时清理自有副本 |
| [recovery_store.go](../../internal/manager/operations/recovery_store.go) | 保存受信Agent boot、当前绑定、轮次及无秘密请求；Start同时冻结和固定expected；失败重扫必须关联当前请求；重开／换绑定丢资格而不删审计证据 |
| [recovery_pages.go](../../internal/manager/operations/recovery_pages.go) | 先识别相同已存页再做新页预算，避免重复消耗额度；新页在seal外部事务后重查请求并一起写页／members／进度；同页异文另行隔离 |
| [recovery_page_read.go](../../internal/manager/operations/recovery_page_read.go) | 从持久请求和密文原页重建链、排序、total、bytes及members，检查phase索引；关闭游标再读后续数据，不在唯一连接上等待自己 |
| [recovery_business.go](../../internal/manager/operations/recovery_business.go) | Process串行消费完整收件箱，复用既有事件事务导入首结果、调用取消或保存case；只对可信all缺席进行受控本地释放，verifyAbsenceRelease在后续Get／Claims／Intents中重读其依据 |
| [recovery_evidence.go](../../internal/manager/operations/recovery_evidence.go) | parseRecoveryRecord补共同Schema无法表达的关系；case先本地／持久隔离再加密保存；事件最终事务核验与该操作相关的历史原页，两个到达顺序都必须保持互斥证据不能转终态 |
| [events.go](../../internal/manager/operations/events.go)、[records.go](../../internal/manager/operations/records.go)、[control.go](../../internal/manager/operations/control.go) | v2 ReceiveEvent统一走恢复接收，原事实重复仍可确认；released需独立证明，不仅放开字符串枚举；准入最终检查本地与持久冻结；v1语义不变 |
| [recovery_complete.go](../../internal/manager/operations/recovery_complete.go) | 准备complete和收completed各有独立最终业务检查；保存请求accepted=1、轮次completed、单节点解冻同事务，历史重报只读 |

完成事实使用已校验的原complete请求（内含唯一scan_id／ledger_id）和accepted=1持久表示；completed没有额外业务字段，不重复存第二份相同语义的body。Open把旧轮标abandoned并清活动关联，但保留原请求及accepted，因此能分辨“结束已提交”和“新boot当前是否允许执行”。没有把历史status字符串当作当前授权。

例一：Manager受理A但尚未投递，断线后pending扫描为空。Process只返回need_all，保留A的两项claims。新的all扫描仍为空；Process复查本轮expected中A的水位为0、本库无许可／事件／case后，同事务保存absence依据、A=canceled/released、抑制prepare／cancel并删A的claims。此时节点仍冻结。PrepareComplete保存请求，Agent接受后Manager提交匹配回执才开放节点；若此处崩溃，重开仍先冻结，旧accepted只作历史，新轮重新握手。

例二：Agent先固定包含START_COMMITTED的扫描页，随后保存成功result(seq2)。Manager接收真实事件，提交原文与succeeded/pending及finalize意图后才ack；Process重读旧页时不回退结果、不伪造缺少的started(seq1)，保持waiting及两项claims。同Agent boot换连接或Manager重开，Agent仍可能重放这份旧页；Manager保留自己的较新结果，不据新轮expected水位制造回退case。当前还没有双端finalize／finalized，故此节点不能仅凭页收齐而开放。

例三：已收到START_COMMITTED页后又收到not_started(seq1)，无论Process是否运行，新事件的最终事务都解密核验历史页并拒绝互斥投影；退出事件事务后先冻结／隔离，再保存case，不返回成功ack、不新增终态。若加密额度已经耗尽，case正文可能尚未保存，但quarantined及本地屏障仍阻止授权；不能通过开始新扫描或重开清除。排障时应同时查看case和quarantined，后者为1且无case可能表示证据保存失败，而非空历史。

当前读取历史页的次数随该操作相关恢复轮次增长；32MiB是单轮原body容量，不是整个进程内存／长期磁盘上限。未验证4096页吞吐、历史保留规模或真实节点负载。临时页和解析记录尽力清理，返回诊断原文的调用者负责其使用与清理；不应直接送到未做权限裁剪的界面。
