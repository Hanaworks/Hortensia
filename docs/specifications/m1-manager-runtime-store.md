# M1 Manager runtime Store v5

2026-11-12 编码前基线；承接 [Agent运行§3](m1-agent-runtime.md#3-主动通道及manager接线) 及 [Manager服务](m1-manager-service.md)。本轮只完成正式Store切片，不代表Docker、跨机或M1发布。

## 范围、依赖和接口

仍由 operations.Store 唯一拥有操作、授权、双claims、事件及释放真相。显式 CreateRuntimeService(ctx,path,managerID,keys,Bootstrap) / OpenRuntimeService(ctx,path,Identity,keys) 使用schema5；旧1—4入口和DDL保留且拒绝互开，Fixture拒绝4/5。正式账号和配置接口明确接受4/5。

BindAgentHello(ctx,*AuthenticatedNode,rawHello) 返回当前 Binding 和 welcome原文；验证Schema、node/ledger、认证对象归属，生成session并调用现有认证绑定事务；welcome总是reconcile_required=true。StartRecovery等仍为当前session/round核对入口。

NextNodeDelivery(ctx,Binding,now time.Time)(*Delivery,error) 返回至多一个当前绑定的持久意图。Delivery含MessageID、OperationID、Kind、Generation、Raw。RecordDeliveryFailure(ctx,Binding,messageID,code string,now time.Time)error 只记发送失败和下次重试。取件先耐久登记，1/2/4/8/16/30秒封顶等待期后重投同一原文；实际socket写成功不改outbox状态。ready/started/result/finalized才确认业务。取消、finalize优先于prepare；恢复时仅取消、finalize及允许的补观察；prepare必须恢复完成且当前owner/prepared session一致。许可直接响应request_start。

## 数据、事务与保密

005_runtime.sql是独立完整DDL，不是迁移。operations、events、conflicts、outbox、recovery_expected、finalization序号有界至JSON安全整数。每操作started只允许seq1；result seq1仅not_started，seq2允许已授权后当前Agent报告未spawn；seq3+只允许normal_exit只读补观察，认证原历史链，进程事实不可改。最新指针和finalize意图必须指向真实最新result。

runtime_deliveries记录当前node/ledger/session/manager_boot、operation/kind、消息ID、请求原文AEAD密文及key/fingerprint、发送次数和重试时点。AAD绑定所有身份及kind；正文含payload秘密，禁止明文落SQL。取件解密后复核Schema、关联和当前业务资格；旧session数据不能投递或反馈失败。重开不恢复旧session，重连重新封装。加密额度先保留，再事务重读全部前提，避免持事务等待额度保留。

现有PrepareFinalize产生稳定请求和MAC资格，投递层不能另造finalize。最终释放仍须当前session/round真实finalized事务，不能由写成功或重试次数触发。

## 事件链、异常及恢复

结果按event_seq保留原body。迟到旧seq可补历史但不得覆盖latest；首次收到seq3+缺前seq时保留待认证事件，不让未经核对的链终结，等旧事件补齐再计算投影。normal_exit补观察只能基于原观察不完整且无drift，outcome、exit_code、finished_at、output、output_truncated完全一致。矛盾记录隔离节点与操作，释放后矛盾继续隔离。恢复页只含最新结果不是历史链证明；须由事件重报补齐链后才能完成。

维护修改须核对 events.go、runtime_events.go、recovery_evidence.go、release.go、delivery.go 与005 DDL；看操作latest/max、每个密文事件、当前round、finalize请求和双方claims，禁止直接清claims或改结果指针。

2026-11-12投递审阅、修复前补充（MV02/MV06/MV07）：同session可以替换恢复round。runtime_deliveries增加round_id，AEAD及唯一键包含当前round；取件第二事务复核round未变。completed轮次对普通业务使用空round；RecordDeliveryFailure只接受当前round的消息。否则已缓存的旧finalize可能永远重投且永远不能确认。公开Delivery/API不变。

## v5实际内部流转与维护补充（2026-11-12）

接收层先校验共享Schema和当前绑定，再在原操作事务核对输入、双claims和既存认证事件；新正文的加密额度在事务外单独预留，第二事务重新检查所有前提。允许的位置是started1、not_started1/2、其他首result2，后续只有normal_exit result3+。result3先到没有result2时存runtime_pending_events；每操作最多32个pending，超额拒绝并保持原投影。pending不推进max/latest，也不授予finalize资格。前史写入或恢复导入后按顺序晋升，复用事件接收事务，比较除event_seq/observation/ownership_status外全部字段，原密文解出body不重新编码。已有seq只允许相同kind及逐字节body；矛盾保留fingerprint并隔离，不能覆盖。

投递器的锁只串行本Store取件，不跨网络发送或等待。读取候选元数据→核对业务资格和当前round→读取或构造本绑定原文→事务外seal→第二事务重查binding/round/业务资格→保存发送尝试和next_attempt→返回Raw。normal_exit观察非complete且ownership非drifted时自动取只读observe，Generation=latest_result_seq；其他意图沿outbox generation=1。Raw以原body/固定message_id重试；业务事实推进后旧投递记录仍作历史，不再成为候选。attempts从1开始，返回前预留1/2/4/8/16/30秒等待；失败报告只传固定码并把next_attempt延后到报告时间加当前等待，不缩短既有窗口。

runtime_deliveries每行生命周期与操作同长，包含round_id且AEAD绑定node/ledger/session/Manager boot/round/message/kind/generation；与原outbox分离是因为发送尝试与业务确认是不同事实。无通用“发送成功”API。finalize原文由PrepareFinalize产生并保存原请求及确认MAC资格，不能从Delivery自行创建终结授权。正常恢复完成后round规范为空；同session换round重新生成/读取该round请求，旧失败反馈不能影响新轮。

正常退出并不解除Manager冻结；不完整观察导致的冻结仍要经当前恢复回执完成。pending、事件认证错误、未知/中断/漂移或归属未核清时不能终结。此Store只验证已认证Agent事实；未spawn的本机存活证明及进程结局由Agent5监督账本保证，本协议没有把PID作为Manager证明。

2026-11-12独立审阅修复前补充（MV06/MV07，承接双端释放F07）：已经released的真实历史出现在新all扫描时，仍须本round重新PrepareFinalize并收到匹配finalized；原confirmed outbox不是本round业务确认。投递器必须从当前扫描成员提取这一类历史确认候选，复用原真实result/MAC路径；不得因released过滤而死锁，也不得跳过恢复ready确认。

同次观察接线补充（MV03/MV05）：同session+round+operation/latest seq的query和原文保持稳定。observed页只供网络关联，不作为结果/释放依据。cursor_expired或未产生新result的观察错误，由transport调用既有RecordDeliveryFailure固定码cursor_expired/observation_failed（仅observe），持久停止该旧context的query，然后断开重连；新hello/session或新round产生新query。其他写失败仍重试原文。不给错误文本入库，不新增公开API；连接重建恢复仍完整经过当前round。

2026-11-12复核修复前补充MV03/MV05：观察context的停止状态在该message/绑定内单调保持。cursor_expired／observation_failed已经持久后，同一尝试迟到的write_failed、timeout、queue_full或connection_closed只作无副作用重报，不能覆盖终止码而复活旧query。旧session／round仍按原资格拒绝；新context必须显式产生新query。
