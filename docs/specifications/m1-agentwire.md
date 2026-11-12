# M1 Agent通信适配内部规格

2026-11-12事前基线，待实现。共同消息、方向及10秒握手／15秒Ping／45秒Pong以[Agent协议](agent-protocol.md)为主；本模块是[运行规格§3](m1-agent-runtime.md#3-主动通道及manager接线)的传输实现，不保存业务状态，不负责授权许可、执行、结果确认或恢复判定。

## 1. API及职责

位置`internal/agentwire`。`ClientConfig{URL string; Token string; RootCAs *x509.CertPool; Hello []byte}`；`Dial(ctx,config)(*Peer,[]byte,error)`发送调用者真实hello，返回已校验welcome原文和当前会话Peer。仅wss固定connect路径、无用户信息/query/fragment；禁止重定向，TLS≥1.2并校验名称，Cookie不作为认证。网络错误与401分别固定分类，长期凭据不进入错误文本。

Manager先通过Store认证Bearer再升级，`Accept(w,r)(*Handshake,error)`仅接受无Origin请求；Handshake持socket与校验器，`ReadHello(ctx)([]byte,error)`在10秒内接收首条合法hello。认证身份与hello node/ledger的核对在Manager Store完成；只有Store产生welcome后`Welcome(ctx,raw)(*Peer,error)`发送对应欢迎并转入会话。Handshake错误／退出必须Close；未取得Peer的连接不能处理应用消息。禁止构造临时ledger或随机执行端点。

`(*Peer).Read(ctx)([]byte,protocol.Envelope,error)`只返回经校验的完整原消息；`Write(ctx,raw) error`只发送符合当前会话及方向的信封；`Close()`关闭连接并等待本模块探活退出。每Peer一个调用方读循环；写入串行，调用方有界队列由service管理。查询、事件和finalize关联由调用方的持久状态机核验，不能因为协议结构有效就ACK或开放执行。

实现接手API：`SessionID()`、`Done()`用于绑定和退出协调；`LastPongAt()`只返回真实收到Pong的本机时间，初始探活起点不冒充收到帧，供Manager适配器更新last_seen。此读取不代替Store当前凭据／session重验。

## 2. 数据与流转

每个Peer持side、session_id、共享不可变Validator、关闭context、写锁、heartbeat协调及`message_id→SHA256(type/session/reply_to/body原字节)`的有界去重表。两种方向各自核验；hello/welcome仅握手期间允许。相同ID同信封原文可交给业务幂等处理；异文拒绝，最多65536个ID后关闭并重新核对，不遗忘旧ID继续接收。映射仅本连接内存，不与业务幂等回执混用。

接收顺序：WebSocket完整文本（压缩关闭、8MiB总限）→UTF8／严格JSON／Schema→当前session／方向→ID指纹→返回原文。二进制、坏编码、错误方向、旧session或异文均不交付业务。发送也检查相同会话和方向，写锁只保护一次写，不持有SQL事务。包级错误为固定类别，绝不附原文或token。

握手和单次写附有限context；普通已建立读循环不使用HTTP30秒总期限。Ping按15秒节拍发起，Ping调用收到真实Pong后才更新最近Pong；45秒未Pong关闭，最多4个Ping在途，超限单独作为预算故障。单调时间用于期限，wall clock只用于诊断；关闭使读、写及Ping退出。没有任何网络失败会自动修改操作终态。

## 3. 验证与维护

2026-11-12独立审阅修复前补充AW02／AW04：握手也是同连接去重历史。转入Peer时，Manager将已接收hello、Agent将已接收welcome及原文指纹带入接收表并计入65536预算；后续应用消息复用其ID必须拒绝。不能只记录转入应用循环之后的消息。本轮运行模式固定每个连接都恢复扫描，因此welcome的reconcile_required必须为true；这是正式v5运行模式的收窄，不宣称覆盖协议允许的所有可选运行模式。

见[实施与AW验收](../implementation/m1-agentwire.md)。定位顺序为TLS信任／HTTP401→hello/welcome身份关联→session及方向→消息大小／Schema→去重预算→Pong→业务Store；重连不恢复旧permit。修改消息支持集合须同步Schema主定义，修改探活须保留迟到Pong刷新及独立在途预算。受控TLS测试不是跨机或Docker验证。
