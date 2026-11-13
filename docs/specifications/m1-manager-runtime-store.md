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

## v5封闭副本检查（2026-11-13实施前基线）

本切片承接[备份§7.3](m1-maintenance-backup.md#73-v5完整备份的只读检查切片)，只检查完成且关闭的私有单文件副本，不接入服务、CLI、完整组、恢复、schema6或protocol2；补充调查不增加业务需求或审批。入口为包级`InspectRuntimeSnapshot(ctx,path,expected Identity,keys Keys)(SnapshotInspection,error)`。报告含Identity、SchemaVersion=5、ProtocolVersion=1、EncryptionKeyID、EncryptionKeyUses、TableRows（29表含零行）、SealedRows（9表含零行）、SemanticFormat=`dm-manager-runtime-semantic-v1`、SemanticFingerprint。失败返回零报告，沿现有固定错误，资源不足另为ErrInspectionLimit；不输出SQL、原始错误、原文、逐行指纹或秘密。

文件／只读事务／总5分钟和统一HMAC framing由共享snapshotcheck负责。Inspector不调用newStore、Create/Open、登录、认证、会话、恢复推进、投递或加密quota入口，不改变boot/session/flags/credentials/uses。只读证据上下文独立持有复制钥及Validator，传同一只读Tx给既有查询证据核心；不能获得正常Store的DB或活动boot。精确DDL复用schemaV5及querier校验，要求user_version/meta=5、单例、完整初始化、identity及两key check、integrity_check和foreign_key_check；未知表／列／索引／约束拒绝。

密文枚举是固定完整注册表，普通表也必须在29表白名单中。新增表或列须同步枚举和MI测试，不能仅改DDL后自动接受。以下绑定均用保存的历史字段，不能代入当前session／round；公共AAD为crypto.go中body/dek独立域、manager、OperationID、NodeID、Digest、Kind、Seq和原key_id。

| 密文表 | 正文／binding来源 | 原文验证与关联 |
| --- | --- | --- |
| operations | payload_cipher；operation/node/input_digest、payload、seq0 | 原InputDigest HMAC、prepare及完整Config Schema，固定stack/version/project/endpoint/policy |
| operation_events | body_cipher；operation/node/digest、kind+seq | 每个历史started/result Schema、permit、事件位置、连续补观察原进程事实及真实max/latest投影 |
| runtime_pending_events | body_cipher；同result+seq | 每操作至多32个，seq3+ normal_exit；缺前史或已到前史尚未晋升可恢复，不推进投影／不提供释放证据 |
| recovery_pages | recoveryPageBinding：原round/node及ledger/session/双boot/scope/scan/limit/cursor/page | 全部历史页、原request、cursor进展、members、计数及内层RecoveryRecord |
| recovery_cases | recoveryCaseBinding：case/node及ledger/operation/kind/round | 按实际case类型验证record、event、page或固定reason JSON；未知operation允许，矛盾正文是隔离证据 |
| finalization | finalizationBinding：原request原文、双boot/session/round及op ledger/digest/result seq | 真实terminal result、accepted request MAC、confirmed outbox、无原claims；和absence basis互斥 |
| user_sessions | csrf_cipher；session/user/token_hash、session-csrf、seq0 | 全部CSRF规范32字节base64url，包含过期／撤销／旧auth_rev；不更新last_seen |
| config_versions | package_cipher；version/owning node/stack、config、seq0 | 每个版本完整文件包Schema与不可变parent链，含未使用旧版本，核对head/applied/observation引用 |
| runtime_deliveries | deliveryBinding：原message/op/node及ledger/session/manager boot/round/kind/generation | 原envelope及payload／请求／观察seq；旧绑定、已确认／停投观察仍作为历史验证，不取件或重发 |

每份密文先认证双AEAD、body fingerprint及原key_id；保留事件7MiB、其他密文8MiB及协议更小预算。表行按固定PK有界遍历，取完整行前检查SQL类型和长度（普通行解码预算32MiB）；不装载整张历史表。扫描已有4096条／32MiB范围允许逐round有界读取；事件序号及配置parent采用固定内存遍历，取消检查贯穿SQL和解密。quota必须在0..1048576且不小于当前密文行数，允许已耗尽以及回滚消耗的多余reservation；读取不再计次。

逐operation验证原固定输入、accepted receipt、完整permit tuple、auth_rev不大于保留user修订、双claims、outbox、所有事件及释放。accepted/ready/authorized未started、running、unknown/interrupted、blocked、取消及恢复中均可自洽；撤权／禁用／换钥／重连不让历史失效。恢复页较旧、未知成员尚未ProcessRecovery和pending缺前史都是合法输入。不得以当前授权、binding、boot、completed门禁代替历史认证；不会自动导入或晋升事实。事件冲突事务先block+freeze，随后才可能保存quarantine/case，允许两次提交之间的副本。

正常released须有原finalization与真实accepted request MAC；absence释放须有完整原all页、expected watermark0、确实缺席且无permit/events/claims和suppressed prepare+cancel。逐个检查所有finalize_requests，包括非finalization指针引用及旧round accepted记录；accepted=1重算独立confirmation MAC，accepted=0不许有MAC。旧未接受意图按自己引用的原terminal result检查，不能因后续blocked而套用当前expectedFinalization资格拒绝。expired/abandoned原all页仍是历史证据；不能要求其是当前round。

普通元数据核对FK以外的kind／ID／generation／引用；旧凭据、pending或过期enrollment、撤销session、历史grant receipt都保留。v5没有保存所有原始HTTP HMAC输入（DoService可带未独存的IfMatch）、竞争事件正文、外部Docker事实或签名授权历史：这些只验证保存形状／引用并纳入语义根，不谎称能重算缺失证明。damaged_operation case不能豁免真实密文或本地数据损坏。

HMAC使用共享snapshotcheck.NewFingerprint，component=manager，stableIDs依次manager_id、digest_key_id。按固定表／明确列／PK序覆盖所有普通列；密文正文以认证原始plaintext替换cipher，保留原fingerprint，排除各组wrapped_dek/key_id。keys只排除encryption_key_id/encryption_key_check/uses，Digest ID/check保留。恢复撤销或schema改变前才可比较同schema新KEK副本；不公开原文或无钥低熵hash。完成证据与MI条件在[本模块实施记录](../implementation/m1-manager-runtime-store.md)维护。

2026-11-13独立自查修复前补充（MI06）：round-record类型case的原文必须逐字节来自该round已认证页面的对应成员；单有合法Schema／同node/ledger不足以证明所属。page_conflict的case.operation_id绑定**原round的scan_id**，而争议页面本身可含不同scan_id／scope／ledger，这正是保存冲突证据的用途，不能要求两份事实一致。真实ReceiveRecoveryPage的不同scan_id争议正例与认证case附错round负例均先记录RED，再修本检查器；不改变正常恢复writer。

2026-11-13独立审阅修复前补充（MI04/MI10）：pending已持久→前史已提交→晋升尚未运行是合法事务窗口，前史此刻可能完整不可修复或矛盾；检查只认证pending的Schema／HMAC／绑定、seq、计数与非released存储边界，不提前要求其符合晋升资格，不把它当最新结果。普通服务之后才能处理或隔离。任何requireClaims之前先SQL计数必须0／2，再SQL匹配预期node和stack/project ID集合，异常数量或巨大未知resource_id不得先全部加载到Go内存；正常Store方法语义不改。
