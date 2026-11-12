# M0 双端终结释放规格

2026-11-12编码前基线，代码起点656e355；实现及本地F01—F10验收已完成，源码6f92eb3，最终回归374016e，证据见[验收记录](../implementation/m0-dual-release.md)。本模块承接[恢复矩阵及双端释放](execution-recovery.md#53-双端释放握手)、[Agent协议第6—7节](agent-protocol.md)、[共享Schema](../../contracts/agent-v1.schema.json)、[Manager恢复](m0-manager-recovery.md)及[Agent扫描](m0-agent-scan.md)。它是推进M1首个完整版本的基础，不单独称为首个版本。

## 1. 目的与选择

解决已有可靠结果仍占住目标、恢复扫描永远waiting的问题。Manager先持久结果及finalize意图，Agent核验同一结果后持久释放并回复，Manager持久回执后释放自己的占用。任何ack、超时、断线或扫描末页均不能代替此链。

使用显式新Agent v4及Manager v3入口CreateRelease/OpenRelease；原版本入口及DDL保留、版本互拒，无自动迁移。本轮沿用首结果seq1/2，不增加线上字段或补观察版本。独立回执表保存finalized事实；finalized不是started/result事件，不占event_seq。新版本复用已有恢复机制，不建立第二套扫描实现。

真实Docker执行器及归属候选基线尚未存在；本轮normal_exit结果来自受控夹具，只验证终结判据及占用协议。不能声称已经发布真实资源active基线。真实执行模块必须将候选基线验证／发布接入同一Agent释放事务，之后才能验收真实normal_exit部署。

## 2. 接口与边界

| 端 | 入口 | 行为 |
| --- | --- | --- |
| Agent | CreateRelease/OpenRelease | 参数与CreateRecovery/OpenRecovery相同，返回v4 Store，新建／重开仍冻结 |
| Agent | Finalize(ctx, raw) ([]byte,error) | 收完整finalize信封；按当前可信session和active_boot核验，提交回执及释放后返回finalized；冻结期间允许处理旧任务终结 |
| Agent | ReadFinalized(ctx, operationID) (json.RawMessage,error) | 读取并认证持久finalized body，诊断／重放不消耗新加密额度 |
| Manager | CreateRelease/OpenRelease | 参数与原CreateRecovery/OpenRecovery相同，返回v3 Store |
| Manager | PrepareFinalize(ctx, Binding, operationID) ([]byte,error) | 重读原结果和pending finalize，核对隔离及身份，保存当前绑定下稳定请求后返回；允许恢复冻结时处理 |
| Manager | ReceiveFinalized(ctx, Binding, raw) error | 必须对应已保存请求、当前session及双方boot，核对完整body，事务保存回执、released和完成outbox并删除该操作claims |
| Manager | ReadFinalized(ctx, operationID) (json.RawMessage,error) | 认证已存释放body及结果绑定，不将released字符串当证据 |

只有新版本支持上述释放入口。共享四字段operation_id/input_digest/result_event_seq/terminal_state引用Schema；最终状态必须从本端持久结果重算：not_started按canceled_before_start判canceled，否则failed；normal_exit仅complete且consistent才可按exit_code判succeeded/failed；unknown/interrupted、缺结果、同位冲突、版本或终态不符均拒绝，保留原事实并隔离矛盾，不返回成功。旧会话／错误reply_to拒绝但不污染当前业务事实。

2026-11-12实现审阅修订、修复前记录：调用方过早调用Manager.PrepareFinalize（尚无结果或结果尚不可终结）只是本地前提不满足，返回ErrRecoveryIncomplete；已有隔离返回ErrFrozen，不因此新增case。当前持久请求收到不同finalized事实或原页出现真实互斥证据才持久隔离。不能把一次无副作用的提前调用当作Agent历史矛盾。

## 3. 内部协作、数据与事务

Agent以原执行记录、加密事件和event_state为权威；新finalization表以operation_id唯一，记录result_event_seq、terminal_state、body_cipher/wrapped_dek/key_id/fingerprint。AEAD用途finalized并绑定节点／ledger／操作／摘要／结果序号，序号不是新的事件位置。正文由可信结果投影产生，不保存调用者未经核验的终态。保存回执和删除该操作两项local_claims为同一事务；不删除执行／事件历史。已释放后Claims返回空，事件及扫描仍可认证读取；缺回执且claims丢失是损坏，不能当已释放。

Manager新finalize_requests保存operation、node/ledger/session/双方boot、message_id及raw请求，当前相同绑定重试返回原请求；新连接生成当前请求，旧请求保留审计但无释放资格。finalization表保存经验证的加密回执及关联请求／结果。成功事务同时写回执、release_status=released、finalize intent=confirmed、删除resource_claims；终态state与原结果不变。不改变归属或其他操作占用。absence_releases继续作为无Agent记录例外，必须与有结果的finalized证明区分。

密钥包装预留沿既有机制独立提交，失败不退额度；已有同事实重报先读后验，不重新加密。seal不在持有唯一连接的业务事务内执行，最终事务重新验证全部条件。不得在事务内调用会自行开事务的公开方法。Agent遵循recovery.mu→数据库／Store.mu既有锁序；Manager沿recoveryMu串行恢复／终结，事件最终事务仍需重查释放事实，防止晚到事件覆盖终态。

2026-11-12实现补测后、修正前明确：Agent结果接收不得取得recovery.mu。Start回调可能写结果，而扫描会持recovery.mu等待Gate失效；若回调反取此锁会死锁。结果事务只保存持久隔离及Store.mu本地准入屏障，不同步等待Gate；Gate最终提交检查该屏障。Finalize／扫描保留原串行锁序，结果与释放通过SQLite最终事务重新核验状态。

完成事实可跨重启读取；回应信封按当前请求message_id和session构造，因此重连不重放旧信封。不同输入／结果／终态不能改写原释放；已释放操作的相同started/result原文重复可确认而不重写，不同原文保存冲突并冻结节点，历史终态及原回执保留，不重建已释放claims。不得因为released直接把所有重报视为矛盾。

编码前审阅补充：现有Schema不允许reconciliation_required且claims_held=false，故Agent已释放后矛盾不能伪造重新占用的RecoveryRecord。v4须保存独立、跨新扫描／重开不清除的节点隔离事实（release_control.quarantined），拒绝新的变更及成功扫描完成；不能通过新scan清dirty消除隔离。历史回执可诊断读取但不得对矛盾状态返回成功finalize。隔离入口保存失败时仍保留本地拒绝屏障，且无成功响应；与既有Gate冻结锁序一致。后续人工解决模块负责清除，本轮无解锁入口。

2026-11-12实施前补足F08的乱序边界：Manager可以先收到seq2结果并完成释放，随后首次收到真正started(seq1)。若已释放的是对应normal_exit(seq2)、输入／许可及历史原页一致，则持久补齐该原始started并ack，保持终态／released／原回执，不重建claims。它不是制造started，而是接收真实迟到事实。已有seq1异文、not_started之后的started、错误许可或互斥原页仍隔离。不能把所有未存位置统称为矛盾，导致合法重排永久冻结。

## 4. 扫描与业务恢复

Agent v4的RecoveryRecord经认证回执投影为phase=finalized、claims_held=false、真实final_state/finalized_result_event_seq，原latest_result字节不变。pending仅省略已finalized且全部持久事件已确认的操作；all含全部历史。过滤前必须认证被省略操作的回执／结果，不能只信表存在。固定扫描的成员及原文在终结后保持原样；当前变化事件仍需当前连接ack。Finalize不是新事件，不伪造ack或修改已有result确认。

Manager v3接受支持范围内的finalized扫描记录：先核对本地已知操作、目标、原结果，再准备当前finalize并取得真实回执；不从扫描页制造reply_to或跳过持久请求。有已持久匹配释放的历史只做一致性核对。尚未持久finalized时，扫描中Agent已释放也不能单凭page释放Manager claims；通过重发相同事实finalize收敛。

编码前审阅补充：Agent-present操作即使本地已经released，也须取得本轮、当前连接的finalized确认才可业务ready。finalize_requests额外关联round_id（正常非扫描为空串）及accepted；相同操作／绑定／round重发原请求，新轮生成新请求。已确认outbox仍允许为恢复构造当前核对请求，不再次释放；ReceiveFinalized原子标该请求accepted。不能仅删除旧scan.Seen=0限制后让旧页直接完成。pending缺席的已释放历史可依本地证据保留；all扫描缺少此前有真实finalized的操作视为missing_history并隔离，absence例外仍按原规则。

2026-11-12独立审阅修复前补充F04/F07/F10：首次加密回执只认证首次请求，不能证明后续新轮accepted。每条请求增加confirmation_mac，以独立用途的HMAC认证完整请求原文、请求ID、双方boot/session/round及操作／结果／终态；收到真实匹配回执后与accepted同事务保存。读取当前轮资格必须核验MAC，单改accepted无效。重确认不预留新的KEK包装额度；历史首次释放回执继续保留原文。

同次审阅补充F06：4096条／32MiB预算针对过滤后实际保留的固定快照。pending扫描要以有界批次读取并认证所有候选历史，认证后省略已释放且全部确认的记录，再累计快照预算。历史总数／被省略正文不能占用pending预算；被省略证据损坏仍须拒绝。不能因累计已完成历史超过预算而让空pending永久无法恢复；扫描历史认证仍受context约束，不称长期吞吐已验证。

旧固定页可早于本端释放，只要身份、结果及互斥事实一致便保留较新本端事实；不能因旧页仍claims_held=true或phase=result而错误隔离。absence已释放的操作出现任何Agent记录仍隔离。完整业务ready改为所有本地操作有有效释放证据且无claims/case/quarantine；不再一概要求scan.Seen=0。未知操作、真实矛盾和不支持的结果序列仍隔离。准备complete和接收completed都重新核对这些条件。

## 5. 正常与异常实例

正常：结果持久→Manager ack并保留pending/两项claims→PrepareFinalize保存请求→Agent保存回执并删两项claims→Manager收到匹配回执，保存并删自己的claims→若在恢复中，再走complete/completed解除节点冻结。仅释放占用不自动解除恢复冻结。

Agent提交后回执丢失：Manager仍pending并持有claims；重投finalize得到原事实，Agent不再次执行或删除别的操作claims。Manager提交后进程终止：重开节点仍冻结，历史释放可验证，新的恢复轮次完成前不允许新任务。两端事务内失败均无成功响应且无半释放。

错误判定或密文损坏：不生成有效释放证明，不清占用；诊断区分缺记录、输入冲突、证据损坏和存储失败。没有force-unlock。context取消只停止当前调用，不伪造业务取消；未获响应按既有1/2/4/8/16/30秒退避由未来投递器重试，本库没有后台线程或重试计时器。

## 6. 验证与维护

验收F01—F10见[实施记录](../implementation/m0-dual-release.md)。排障顺序：当前绑定→原结果／终态判据→finalize intent及请求→Agent回执／claims→Manager回执／claims→扫描历史→最终completed。修改释放判据须同时核对两端实现与共享主定义；修改扫描过滤须验证所有事件确认与被省略证据认证；修改事件重报须覆盖释放前后两种顺序。真实执行归属、补观察、旧库升级、备份回退及人工解除隔离仍由后续模块负责。

## 7. 实施中的接手补充

以下于2026-11-12实现过程中补充，初始基线02d15c7，迟到started规则另在e4bd42d保存；最终代码与验收版本见实施记录。两端释放入口不会创建后台投递线程。Open仍按原机制换boot并冻结；Close只关闭持有资源并清理密钥副本，不自动终结操作、清除隔离或删除claims。恢复时先绑定当前会话，再从原持久事实核对，不续用旧实例写资格。

| 内部数据 | 实际来源与约束 | 生命周期及维护用途 |
| --- | --- | --- |
| Agent finalizations | operation_id唯一；result_event_seq指向本地真实结果，terminal_state由结果重算；AEAD包保存body及body_fingerprint，原事件继续保留 | 与本地claims删除同时提交；不因ACK、重连或扫描完成删除。读取时核对原事件、输入和终态，不能仅凭行存在判已释放 |
| Agent release_control | singleton id=1，quarantined是持久隔离事实；Store另有本地拒绝屏障 | 新建为0，发现矛盾后为1；新扫描、重开不清。正文／SQL失败时仍不得开放准入，排障同时查本地错误及持久隔离 |
| Manager finalize_requests | message_id唯一，operation/node/ledger/session/双方boot/round关联原请求raw、accepted及confirmation_mac；当前逻辑键重试复用 | 认证MAC与accepted同事务提交，单个布尔字段不作为证据；历史请求不代表新轮完成资格；无活动扫描的round为空，扫描期间绑定当前round |
| Manager finalization | operation_id唯一，关联首次释放request_id、result_event_seq、terminal_state及加密回执body | 与本地released、confirmed及claims删除同事务；后续新轮只确认新请求，不改最初释放证明。不混用absence_releases |

读入口允许核验历史事实，写入口必须检查当前boot/session。新finalize先检查本地操作与原始结果，再用独立事务预留加密额度，最后事务重查并释放。回执已存的重复请求绕过新加密，但仍不能绕过身份及隔离检查。Manager恢复中Process可能返回waiting，此时调用者应投递持久finalize并把真实finalized送回，再次Process；库不会隐式发送网络消息。全链以[跨端测试](../../internal/manager/operations/dual_release_integration_test.go)为可执行示例，身份／结果夹具边界仍保留。
