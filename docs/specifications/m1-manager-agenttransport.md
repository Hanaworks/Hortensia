# M1 Manager节点任务通道实施规格

现行阅读提示（2026-11-12，实施后追加）：下文保留编码前基线及后续修订。正式通道已本机接线，确切验证及限制见[MT记录](../implementation/m1-manager-agenttransport.md)；真实Linux／Docker和完整M1未验收。不能把历史“待实施”当作当前源码状态，也不能把本机接线当作真实部署通过。

2026-11-12编码前基线，待实施。位置`internal/manager/agenttransport`；它连接[通信适配](m1-agentwire.md)、[Manager v5 Store](m1-manager-runtime-store.md)和[Agent运行](m1-agent-runtime.md#3-主动通道及manager接线)。不另建账号、节点、操作、发送意图或恢复权威。

## 1. 入口与生命周期

`New(*operations.Store)(*Handler,error)`，Handler实现http.Handler，只处理GET `/agent/v1/connect`。Manager运行模块在普通HTTP适配器之前分派这个精确端点，因此不受普通API30秒context约束；仍受TLS与请求头预算。只接受TLS、无Origin/Cookie/query/路径别名及正文、唯一规范Bearer秘密；异常正文拒绝时关闭连接，不排空慢正文。固定JSON错误沿HTTP主规格，不回显凭据或内部错误。

2026-11-12 MT01独立审阅后、RMT02修复前追加：认证通过仍可能因WebSocket升级头失败。升级校验继续唯一委托agentwire／WebSocket库，Manager以响应包装拦截升级前的非101状态，返回固定JSON（输入错误400 invalid_field，服务端能力错误500 internal_error），含request_id、no-store、nosniff及Connection close；第三方错误正文及其头不透传，不反射Version／Key等输入。包装只通过Unwrap暴露原ResponseWriter的实际能力，不无条件声明Hijacker。101正常转发后即不可改写HTTP响应；若随后Hijack失败，丢弃库的后续错误写入，以立即读写deadline收束尚未移交的HTTP连接。该包装仅活于Accept调用，不持有Store或修改任何节点／操作事实。

接手定位为`upgrade_errors.go`：`header`隔离暂存库待发头，只有101会复制到真实writer；`committed`保证只选择一次HTTP输出，`upgraded`区分已发101而不能再返回JSON的路径。它们只表达Accept响应状态，不代表hello已绑定或恢复完成。成功升级之后的帧仍由Peer直接写socket，包装不截获业务帧；原writer不具备Hijacker时，库在101之前拒绝，包装固定返回500。

先AuthenticateNodeCredential，再agentwire.Accept／ReadHello，再Store.BindAgentHello核对身份和生成当前session，最后Welcome。每节点登记唯一当前连接，新连接替换旧连接；关闭旧连接不清除新session。所有业务入站仍经Store最终事务检查当前身份，注册表不是授权依据。握手未完成或发送welcome失败也要按确切session清理绑定。

2026-11-12独立审阅后、MT07修复前追加：并发握手的Welcome写入完成顺序不等于Store会话绑定顺序。注册临界区必须在同一注册表锁内先通过ReadRuntimeConnectionState再次核对本Binding仍为当前，然后才替换并关闭旧连接；失败只关闭自身。锁覆盖核对到注册，阻止较早绑定但较晚Welcome的处理器关闭已注册的新会话。Store不借用注册表锁回调，绑定事务在进入此锁前已结束；核对不含网络写或Compose，不能持SQLite事务等socket。

Handler持有Store借用引用、连接注册表与关闭标志，不关闭Store。`Close(ctx)`先拒绝新连接，取消并关闭已有Peer，等待读／写／恢复worker退出；运行模块在Store关闭前调用。WebSocket已升级连接不由http.Server.Close管理，必须由本模块主动收束。握手中socket也登记并可关闭，不能只管已完成Peer。

## 2. 连接内部协作

每连接一个读循环、一个写循环和一个恢复／投递驱动。写队列最多32项，一次只有一个配置包排队／发送；小消息不堆积无限内存。队满停止当前连接并为有持久Delivery的消息记录queue_full；业务事实仍在Store，重连按恢复处理。读循环只做有限Store调用及入队，不等待Compose或socket写。

| 内部对象 | 数据来源与含义 | 生命周期 |
| --- | --- | --- |
| nodeConnection | 认证句柄、Binding、Peer、context、队列、关闭／worker等待 | 从绑定成功到exact-session断开；Store收束后不再异步写 |
| outbound | raw原文、kind、message_id、可选Delivery身份 | 来自Store或Store已提交的回应；发送失败不改原操作结果 |
| sentRequests | 本连接已实际发送的请求ID、kind、query_id／operation_id；只留必要关联 | 最多65536项，耗尽关闭重连；不持第二份配置正文，不作为释放证据 |
| controlRetry | reconcile／complete请求的原ID、下次尝试及1/2/4/8/16/30秒退避 | 每连接最多65536项；与sentRequests分别有界，耗尽在入队前拒绝新ID并关闭。请求原文和关联权威仍由Store持久恢复请求维护 |

写循环调用Peer.Write成功后登记发送关联，失败由RecordDeliveryFailure记录固定码并关闭连接；成功不是业务确认。为避免极快回复先于写返回，关联可先登记为in-flight，但只有写调用已开始的请求有回应资格，失败即结束该连接。raw使用后不进入普通日志。

读到有效当前帧后RecordNodeSeen；真实Pong通过Peer.LastPongAt变化唤醒／低频触达，不因循环tick凭空刷新。记录时仍复核当前凭据和session。Node.online来自正式绑定，Docker可用性与观察分别呈现。

## 3. 入站与恢复流转

| 入站 | Store处理与输出义务 |
| --- | --- |
| ready | ReceiveReady成功后唤醒驱动；不能在适配器直接设置state |
| request_start | AuthorizeStart返回authorize_start／deny_start原文入写队列；不创建第二个许可或放入普通outbox |
| started／result | ReceiveEvent持久成功后才将其返回ACK入队；高seq及历史链规则由Store负责 |
| reconcile_reply | ReceiveRecoveryPage处理合法页及固定协议错误；不得把error页当空账本 |
| reconcile_completed | ReceiveRecoveryCompleted提交后才认为本轮Store完成；不直接开放节点 |
| finalized | ReceiveFinalized核对持久请求／当前round／MAC并释放；适配器不删除claims |
| observation | 核对reply_to及query_id属于本连接已发送observe，只记录当前帧；页面不当作result／归属或释放证据。cursor_expired／observation_failed仅observe可RecordDeliveryFailure并重连，以新round新query恢复 |
| protocol_error | 固定错误关闭或按明确关联处理，不返回另一个protocol_error造成循环 |

新绑定StartRecovery(pending,200)，持久请求入队。collecting状态用NextRecoveryRequest；enumerated／waiting等可处理状态调用ProcessRecovery，need_all创建新all轮，ready走PrepareRecoveryComplete，completing稳定重发原请求直至completed。blocked保持隔离，不猜测成功。waiting与正常期都可从NextNodeDelivery取允许的持久工作；正常prepare只在Store认为已完成恢复时出现。released历史的新all轮也须投递本轮finalize，不能因outbox曾confirmed而跳过。

接线前明确只读Store入口`ReadRuntimeConnectionState(ctx,Binding)(RuntimeConnectionState,error)`，返回经同一事务当前绑定核验的Frozen和Recovery状态。只有旧round已completed而当前节点确实重新冻结时才发起新恢复，不能每次正常结果都重新扫描并取消其他尚未开始的操作。此入口不返回SQL连接，不授权变更。

入站提交后非阻塞通知驱动，低频tick补丢失通知并遵守持久next_attempt。普通结果若令节点冻结，即使随后补观察可终结，也不能自行清frozen；重新启动当前恢复round或关闭重连，经新complete回执开放。ErrRecoveryIncomplete等表示等候更多证据，保留当前连接；存储／身份／关联失败固定关闭并保留权威事实。

## 4. 验证与维护

见[MT实施验收](../implementation/m1-manager-agenttransport.md)。定位顺序：TLS／Bearer→hello身份→当前Binding／注册表→持久恢复请求→Store入站提交→发送关联／队列→当前round完成／finalized。修改队列或重试不得改变幂等和确认标准；关闭测试必须覆盖握手中和已升级连接，不能仅以HTTP Shutdown退出证明没有worker继续使用Store。
