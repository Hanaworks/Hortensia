# M1 Agent服务与命令实施规格

现行阅读提示（2026-11-12，实施后追加）：事前基线保留；实际文件／内部协作见“实施后代码交接”，恢复及材料保护修订见AS-R01—R03和故障可观察性补充，结果在[AS实施记录](../implementation/m1-agent-service.md)。日常入口见[首次接入](../agent-first-run.md)，真实Docker执行尚未开放。

2026-11-12事前基线，编码前保存。范围为真实身份命令、TLS主动连接、恢复和有界调度；共同字段及事务真相引用[m1-agent-runtime](m1-agent-runtime.md)、[协议](agent-protocol.md)、[执行恢复](execution-recovery.md)、[运行配置](../operations.md)。不实现旧库迁移，不连接用户节点。

## 入口与持久材料

`internal/agent/service`提供`LoadConfig(path) (Config,error)`、`ParseConfig([]byte)`、`Initialize(ctx,Config,nodeID)`、`Enroll(ctx,Config,token)`、`RotateCredentials(ctx,Config)`、`Serve(ctx,Config)`；cmd/agent只解析参数及标准输入、捕捉信号和打印固定诊断。配置字段沿operations§10，严格64KiB JSON；相对路径、未知/重复/null/错类型拒绝，环境变量无覆盖。所有命令持有state_dir/.agent.lock；localstate提供Linux flock/目录同步和Windows受控验证，不让Agent依赖Manager进程包。

identity format1/schema5包含node、ledger、两key ID、manager origin、candidate/active/rotation_pending及generation。init检查所有材料不存在，耐久写两随机32字节钥，CreateRuntime生成ledger/endpoint，再耐久写候选identity；失败保留半集合，不覆盖。身份变更同目录临时文件Sync、Rename、目录Sync。enroll先以候选认证hello，只有明确401才提交stdin token；网络丢响应保留同一候选。rotation_pending先耐久，再优先试candidate；明确401才旧active认证及重发同candidate。serve只用active，401结束重试。身份、HTTP响应和welcome全部匹配node/ledger/代次，错误不含秘密。

## 服务结构及调用流

serve锁→加载匹配身份/钥→OpenRuntime冻结→Probe同机Engine/Compose并BindRuntimeProfile→hello使用Store BootID/endpoint→Dial→BindManagerSession(welcome,helloID)→处理reconcile及complete。精确版本未经compose验证清单允许时能力为空；Probe成功不代表验证。Profile配置指纹保存CLI/socket/执行期限与版本；Engine变化不重新绑定。无Docker时仍恢复及重报，不宣称资源为空。

一个网络读循环只把消息送入至多32个槽的控制队列；满即断连接并使Gate失效，不执行Compose或SQL。控制协调器处理恢复/ACK/许可/取消/finalize，长准备/预检/观察在单个有界工作槽执行。网络发送至多32消息，通过单写循环；事件只从PendingEventRefs逐条读取。未ACK事件按固定节流重报，当前连接messageID映射operation/digest/seq；ACK同时验证reply_to与body才持久确认。连接丢失使Gate失效，已真实启动的Process由独立生命周期继续监督。

prepare先ReceivePrepare持久原payload与claims，再ReadExecutionInput→workspace.Materialize→ReadProjectBinding→Engine.Prepare→CommitPrepared，成功才ready。请求本次许可前完成VerifyBeforeStart及Invocation；NewGate短Start回调只调用executor.Start和内存登记；回调返回后RecordProcessCreated，再Wait→RecordProcessExit→Observe→RecordProcessOutcome。Start明确失败才RecordStartFailure。进程事实不能由取消/断线推断。cancel先Store.CancelExecution，只有interrupt_required对本boot已知Process中断；重开不附着旧PID。finalize只调用Store.Finalize，由同事务发布归属及释放。

## 观察页面与关闭

observe先BeginObservationQuery持久绑定operation/query/limit，然后只读采集；允许的正常退出补观察经RecordObservationUpdate产生新seq。同query固定5分钟快照、页≤200；opaque cursor绑定query/位置，同query改limit/operation拒绝；过期/重开cursor_expired，不能重新采集同query。页面只含Schema允许字段，失败为明确incomplete；未知/中断不自动解除隔离。缓存有界，预算不足明确budget_exceeded。

关闭先停止连接收件并使Gate失效，再取消服务工作上下文/中断已知进程，等待监督器持久退出及结果（至少30秒预算），最后Close Store、清钥和释放锁。存储失败保持claims及unknown，不合成normal_exit；普通网络断线不用进程context。维护时按身份→runtime profile→session/scan→preparation→process/events→finalized定位，不能直接SQL修复。

## M1普通查询范围（2026-11-12接线前细化）

本轮自动observe仅接受绑定operation的normal_exit且原观察不完整的补观察。ledger.BeginObservationQuery与RecordObservationUpdate为这一修复链路提供持久关联，不泛化成普通查询日志。operation_id=null、正常完整结果、interrupted/unknown等普通查询返回Schema规定的明确observation_unsupported/unavailable，不调用CLI、不追加结果、不改变归属；普通资源浏览作为M2后续范围。HTTP/Web继续读取持久结果。该范围适用于AS08及AR13，不能将拒绝查询写成观察完成。

## 实施后代码交接（2026-11-12）

实际文件为config.go（严格配置）、identity.go（材料顺序与匹配）、credentials.go（HTTPS/候选恢复）、runtime.go（连接/有界调度）、observation.go（只读补观察快照）。公开Initialize返回Identity供嵌入调用者核对；cmd/agent不打印该对象或secret。openIdentity先检查数据库/sidecar元数据，不将数据库整体加载到内存，然后OpenRuntime核对两钥及schema。

`session`持当前id/context、32槽入站和出站、小型ACK引用及重发节流；每连接65536引用预算达到则重连。prepare入站最多同时1个配置包；>64KiB出站同时只保留1条，事件逐条从Store读取。32槽work保存不含payload的闭包，原payload只在worker从密文账本按需读取。`operation`在同boot内保存已验证Prepared/Command、Gate、真实Process和监督注册状态，finalize提交后移除该内存入口；崩溃后不会按旧PID重建Process。

锁序：在r.mu下只快照Gate指针；释放r.mu后Invalidate，避免Gate短Start回调登记Process时反向等待。Store事务只通过公开API进入；网络读循环不执行这些事务。Gate Authorize返回之后监督器RecordProcessCreated，再等待Process，随后独立写进程结局及观察结果。Callback内连接已结束时不Start，明确失败使用账本要求的spawn_failed；许可已提交但尚未进入Start的证明只由正式RecordStartFailure判定。

未能Probe/绑定真实Engine时仍能hello、扫描、重报，但r.readable=false，补观察明确execution_profile_unavailable，不能拿另一Engine的空资源解释旧操作。所有生产变更能力同时要求Linux、有效绑定和IsVerifiedRuntime精确清单；清单当前为空，所以提供了真实程序与受控调度验证，尚不能部署用户Compose。

固定补观察缓存最多8个query、合计16MiB、单query8MiB。页面及opaque随机cursor由第一次采集固定，首个query持久CreatedAt确定5分钟期限；缓存不逐页重新采集。缓存不存在但Store已登记query时返回cursor_expired，不重采集；普通查询仍按上节M1范围拒绝。

本地启动例（路径需要实际部署者配置，尚无Docker接受组合）：`go build -o agent ./cmd/agent`，`agent init --config /etc/docker-manager-agent/config.json --node-id <UUID>`，`agent enroll --config /etc/docker-manager-agent/config.json --token-stdin`，`agent serve --config /etc/docker-manager-agent/config.json`。轮换为`agent rotate-credentials --config /etc/docker-manager-agent/config.json`。认领token从stdin第一行读取，最多45字节（含CRLF），不要求等待stdin关闭。help/version不打开身份或密钥。真实Linux、文件权限/目录耐久、Docker矩阵及安装服务安排仍需后续环境验收。

### CW04组合修订（2026-11-12，接线修订前记录）

来源为根与Compose/ledger负责人对公开stack归属的组合复核：service在CommitPrepared成功后、请求许可前，正式ReadPreparationEvidence并以认证落库值替换Prepared.Evidence；重开已走同一读取入口。Compose按OwnershipRevision>0及Pre完整身份精确匹配才附stack_id，新资源/替换候选为null，ledger可接受保守null但拒绝错误非null。Observe在采集前重新Probe核对Engine/策略/版本；失败使用明确不完整观察，保留原进程outcome/exit/output，不把新端点当成原端点资源。受影响AS05／AS08／AS09及AR11／14；这属于本轮实现选择及复核修订，不表述为用户逐字段确认。

开始请求每秒以新message_id重报，但沿同一Gate/nonce原10秒窗口RegisterRequest，不延长许可窗口、不创建第二Gate。等待窗口耗尽由活进程准备失败入口记录start_window_expired并保留claims。该定时处理属于控制循环，不做Compose；相同nonce收到任一已登记reply_to可关联，started后不会再重发请求。

单prepare/start槽覆盖准备与其许可窗口：worker发送request后等待本操作Start已尝试/拒绝/取消/10秒耗尽或连接失效，再处理下一任务；不会在另一个项目准备时并发进入前一项目Start。此等待不占网络控制循环、不等待已启动进程完成。

监督结局及结果耐久后服务清掉Prepared/Command/Process中的原文和子进程对象，保留小型Gate/nonce关联承接同连接重复许可；只读补观察从Store重新读取认证准备和原payload。finalize后删除小内存入口。Gate表自身沿ledger本boot防重策略，不能通过服务清内存产生第二次执行机会。

## AS-R01—R03 审阅修订基线（2026-11-12，修复代码前）

范围只修 `.cache/m1-agent-service-review.md` 的三项：可信退出与首结果之间的恢复缺口、初始化遗漏SQLite旁文件、UUID版本/variant过宽。受影响AS02／AS06／AS07／AS08；共享进程字段和事务仍由[m1-agent-runtime](m1-agent-runtime.md)及ledger定义，不在服务直接SQL。

**AS-R01**：Store有界 `PendingProcessOutcomeRefs(ctx,limit)` 枚举（每批≤32）后，服务逐项 `ReadProcessOutcome` 认证原事实，再以原 BootID/outcome/exit/finished/output/truncated/error 写 `RecordProcessOutcome`。恢复首结果使用明确不完整的资源观察，不执行旧命令、不附着旧PID、不从当前boot重造进程事实；normal_exit仍为normal_exit。后续正式observe可走持久query补采集。恢复在首次hello前和服务独立worker每秒尝试，断网仍可收敛；结果写失败保持双claims和同一事实，下次继续，不合成未知或释放。每批失败不阻止同批其他项。网络控制loop不运行Compose观察。已在活监督器观察中的操作由内存标志排除，避免抢先生成不完整结果；监督器最终写结果失败后结束该标志，交给恢复。恢复读取和最终写入均由Store幂等认证，已有结果不增加seq。显式observe也先恢复该操作缺失的首结果，然后按原查询规则采集。

**AS-R02**：init在生成或写任何key前统一检查identity、ledger.key、journal.key、agent.db、agent.db-wal、agent.db-shm、agent.db-journal，任何既有普通文件/目录/链接均拒绝且不交给SQLite清理。配置将所有数据库保留路径纳入互异校验，identity不能占用旁文件名。openIdentity在OpenRuntime前对所有三个既有旁文件检查路径无链接及私有普通文件；不存在允许，合法热恢复WAL仍交由SQLite。初始化失败不清理/改写遗留原字节及目录项；锁文件属于命令执行的既有同步设施，不当作数据库材料。

**AS-R03**：node输入及加载身份中的node/ledger UUID必须是共享Schema `Id` 的小写UUIDv4（version=4、variant=8/9/a/b），输入校验在创建目录/锁/随机钥前执行；错误返回ErrConfig。缺材料与错误身份仍不补造。此约束引用共享Schema，不拓宽ledger接受域。

事前边界：本地Windows、真实SQLite与明确测试CLI；符号链接需运行账号能力，不能创建时记未验证；不增加生产接受矩阵，不声明Linux进程组/目录Sync/掉电耐久或真实Docker部署通过。

### AS-R01 故障可观察性补充（2026-11-12，补充实现前）

与AR有界枚举的认证规则统一：每批遇到首个读/写错误即停止，不跳过损坏项继续宣称扫描成功；临时 `ErrStorage` 保持事实/claims，按秒重试。`ErrEvidence`、`ErrConflict`、`ErrFrozen` 等非临时认证/身份/隔离错误终止Serve并返回固定库诊断；周期发现时取消网络连接和服务生命周期，不吞错无限重连。首次hello前只执行一批有界恢复，其余由周期继续；不宣称单次覆盖所有历史。正常监督器从注册至完成观察/结果写入期间标为collecting，恢复入口只跳过活监督器；监督器返回（包含写结果失败）后再可恢复。该标志不授权执行、无持久真相地位；真正事实只能由Store认证。

AS-R02配置别名补充（2026-11-12，补充实现前）：Windows文件系统路径互异检查按大小写不敏感的清理路径比较，不能让IdentityFile用AGENT.DB-WAL等别名占用保留SQLite旁文件；Linux保留原大小写语义。受影响AS-R02a，拒绝仍在目录/锁/key之前。

## AS-R01—R03 最终交接核对（2026-11-12，实施后）

本篇“AS-R01 故障可观察性补充”**覆盖**最初审阅修订中“每批失败不阻止同批其他项”：最终代码为首错即停；ErrStorage按秒原项重试，其他认证/身份/隔离错误终止Serve并返回固定库诊断。保留前述事前基线与补充的真实先后，不将后续选择倒写为最初已确定。

恢复代码位于 `internal/agent/service/recovery.go`：Store先有界认证候选，再逐项读取原ProcessOutcome；原normal_exit暂存明确error观察以形成首result，原interrupted仍unknown归属/claims保留。`runtime.go`在第一轮hello前执行一批，并启动独立每秒恢复循环；临时写错误不会终止循环，非临时错误经受锁保护的recoveryError取消Serve连接，最终由Serve返回。主动observe在正式BeginObservationQuery前调用同一缺首结果恢复入口，之后沿原持久query/只读采集/RecordObservationUpdate/固定页面链；不重放旧Start。

`operation.collecting`仅代表当前boot仍有监督器负责Wait/Observe/首result，从afterStart注册到监督器defer清除；此标志避免周期恢复抢过正常观察。它不会替代Store事实或授权新进程。监督器写result失败并返回后，下一次周期读取独立保存的进程事实完成恢复。`forgetCompleted`继续清除Prepared/Command/Process原文，不删除ledger和claims；finalize仍是唯一正式释放入口。

初始化在UUID及互异路径校验后持锁检查全部主材料/旁文件，未通过则不生成/写钥、不调用SQLite；SQLite打开前既有三个旁文件均要求无链接且为私有普通文件，合法WAL存在不构成拒绝原因。Windows路径别名按本机大小写语义比较；shared Schema Id仍是node/ledger UUID约束主定义。关键测试在review_test.go，测试CLI屏障只在测试二进制内读取其私有旁文件，生产程序没有绕过配置/能力的开关。
