# M0 Manager 扫描接收与业务恢复规格

日期2026-11-07，起点67a8963。本篇与[实施计划](../implementation/m0-manager-recovery.md)在编码前保存；状态：实施基线，尚未验收。以下是依据现行主规格的技术落实，不表述为用户逐项确认。

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
| ReceiveRecoveryEvent | Binding及真实started/result；已知操作复用原接收规则并只在提交后ack；未知操作加密保存case后ack，持续隔离；不伪造Operation |
| ReadRecoveryState／ReadRecoveryPage／RecoveryCases／ReadRecoveryCase | 可信内部诊断、加密证据重读；不是HTTP输出或准入令牌 |

v2必须通过BindRecoverySessionFixture绑定，原BindSessionFixture只服务v1，防止漏记Agent boot。即使Agent能重放旧completed，新的Agent boot／连接也不能据此解除新的冻结。本轮Manager在换会话或重开后从首页重新扫描；不复用旧分页／complete资格。Agent自己的同boot续扫能力仍保留，Manager可按新轮从首页重新收齐未过期固定快照。

保留既有单连接immediate事务、WAL/FULL、外键、5秒锁等待与密钥管理。扫描入口使用独立recoveryMu串行化；事件接收不取此锁，以免形成嵌套等待。所有写事务重新检查active_boot与当前绑定。context约束数据库调用，未引入后台投递／硬总时限。Agent的15分钟期限为权威；收到scan_expired后保持冻结并从首页重建，不恢复墙钟期限。

## 3. 持久数据与内部协作

新增`002_recovery.sql`包含v2完整DDL，不改001。重要字段如下；缺省ID／cursor用空串，未收到total用-1。公开诊断Total可为-1，不能当空集合。

| 对象 | 关键字段、含义及来源 | 约束与生命周期 |
| --- | --- | --- |
| recovery_nodes | node_id、agent_boot、active_round；分别来自可信绑定、当前本地恢复轮次 | node外键；新绑定清活动关联并冻结；不清除历史证据 |
| recovery_scans | id、node_id、ledger_id、session_id、manager_boot、agent_boot、scope、page_limit、scan_id、status、total、seen、pages、body_bytes、next_cursor、last_operation | id为本地轮次，scan_id为Agent首页分配；status为collecting/enumerated/need_all/waiting/blocked/ready/completing/completed/abandoned/expired；进度与页同事务；双方boot禁止旧回执解冻 |
| recovery_requests | message_id、round_id、kind、page_no、cursor、raw、accepted | kind仅reconcile/reconcile_complete；raw是无秘密请求；同逻辑页只保留一条，重试不增长请求行；reply_to必须对应当前轮及当前session |
| recovery_pages | round_id、page_no、request_id、body_cipher、wrapped_dek、key_id、fingerprint | 唯一(round,page)，原body加密保存；请求及AAD绑定轮次／节点／ledger／scope／scan／limit／cursor／页号；正文从不写SQL明文 |
| recovery_members | round_id、ordinal、operation_id、page_no、record_no | 唯一(round,ordinal)及(round,operation)；只存原页位置和身份，重要正文回到解密页校验；严禁缺页／索引损坏被解释成空扫描 |
| recovery_cases | id、node_id、ledger_id、operation_id、kind、round_id、evidence_cipher、wrapped_dek、key_id、fingerprint | operation允许本库无记录；kind为稳定诊断原因；相同节点／操作／原因／原文指纹去重；正文加密；当前无通用解决／删除入口，任何case阻止该节点完成 |
| absence_releases | operation_id、round_id | 仅可信all缺席且从未授权的取消证明；引用持久完整扫描，和取消终态／释放claims同事务；不制造Agent结果或finalized |
| operations.release_status | v2新增released | 本轮只由absence_releases证明；原v1仍只有held/pending，常规终态仍需未来双端释放 |

每轮最多4096条记录、32MiB原始页面body、4096个数据页及一个完成请求；单页完整信封仍不超过8MiB且不超过请求limit1—200。非空页必须推进，空账本允许单一空末页。重试不重复保存或消耗包装额度；新页失败可能消耗已提交额度，不退还。各轮和case是审计证据，当前不自动清理已保存历史，磁盘随不同恢复轮次增长；这不是长期保留策略或容量已验证的产品。未来清理必须保留absence_releases引用依据及未解决case。

页／case复用既有随机DEK和KEK额度机制，但采用独立Kind用途和完整上下文绑定；内部blobBinding的OperationID位置对这些用途保存roundID／caseID，Digest位置保存规范JSON上下文而非公共input_digest。原payload／event的AAD字节规则不改。页与case重读均检查AEAD、指纹及共同Schema／本地关联。临时原文尽力clear，不承诺Go内存彻底擦除。

存储层负责版本与boot；收件箱层负责请求链、分页及解密重读；业务层消费完整页并对照固定操作、历史许可、事件及claims；完成层把业务校验与当前回执联系起来。普通事件仍经既有事件库处理。页面内latest_result可调用同一内部接收核心，但不制造网络result信封或ack；真实事件重报才有自己的message_id及当前连接ack。

## 4. 页面接收、正常与异常流转

1. Start在事务内冻结节点，保存本地round与首页reconcile；重试返回原请求。页响应先共同校验，再查持久reply_to、当前session／双方boot与ledger。
2. 按固定scope／scan／total及请求页号接受；operation_id全局严格递增、唯一；页内数量不超limit，累计不超total，complete时累计必须等于total且next_cursor=null。游标不得回环，不可跳过尚未提交页；错误页不计完成。
3. 新页先做无新包装重复预查，事务外seal，最终事务重新检查全部关联，再同时写密文页、成员和进度。重放同body不增长计数／额度；同页异文保留原页并返回冲突、冻结不得解除。无成功进度绕过存储失败。
4. readRecoveryPages重读时解密校验全部页面、请求关联、身份索引与计数，不能只信可修改的计数列。Process在收齐之后处理；中途失败保留收件箱和已提交事实，可幂等重做。
5. pending缺任何未释放的本地任务，状态need_all；Start(all)生成新的本地轮次与null/null请求，旧轮标abandoned，冻结连续，旧响应／complete不再有效。Agent总数／结构超预算返回拒绝并冻结，不截断继续。

RecoveryRecord的目标／摘要／result身份及序号必须相互一致；last_event_seq不得低于其嵌入result序号，无result时report_acked=false。phase、claims和final字段按Schema及恢复矩阵校验。固定页可以落后于已接收的新事件，不能把较旧页面覆盖较新持久结果；同一位置异文必须隔离，不能因实时事件较新而忽略矛盾。

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

PrepareComplete与ReceiveCompleted各在最终事务内检查：当前轮已完整、当前绑定未变、无case、所有本地操作都具有有效释放证明且无claims、无待解决业务。旧操作的owner_boot／prepared_session／permit不被改成当前值。正常空库也必须走两次握手；未保存回执前节点始终冻结。completed重放只在同一仍有效轮次幂等；新轮、换boot/session后旧响应不得改变冻结。

业务状态不是授权布尔参数，外部不能直接标记“已核对”。单节点完成不影响其他节点。请求等待／错误页／context取消不产生取消业务事实或释放资源。无需真实环境即可用本地两端库及受控结果验证；这不等于网络消息已接入。

## 6. 维护、验证与边界

诊断按node/ledger/双方boot/session → 当前round及请求关联 → 页与members一致性 → case → 操作原许可／结果／claims → absence依据 → complete请求／回执顺序查看。修改分页须查原字节与双预算；修改解冻须同时检查重开、旧回执、并发新事件及其他节点；修改释放必须保留独立证据分支，不能仅允许released字符串。

验收稳定编号R01—R12见[实施记录](../implementation/m0-manager-recovery.md)。覆盖真实SQLite失败／Commit失败、重开与辅助进程终止，及Manager v2与Agent v3的本地组合。运行环境沿既有Go／SQLite组合；需要Docker／Compose或跨机验证前再告知用户，当前不需要联调环境。实现后补源码入口及完整实例，保留事前基线和事后补充时点。
