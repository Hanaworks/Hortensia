# M0 Manager 本地持久协调实施与验收

日期2026-11-07，起点1d043a7。规格及验收基线在编码前以3bf8629保存；源码／测试版本c02e1fd。状态：M01—M10通过，本地协调库可供源码交接，整个M0尚未完成。[规格](../specifications/m0-manager-operations.md)是本轮实施依据。沿用当前工作区，不改变Agent库格式或网络冻结状态。第3节的初始状态保留开工时记录，最终结果见后文。

## 实施顺序

1. 保存规格与验收基线，再写失败测试。独立实现Manager v1存储／加密、控制与许可、事件接收及读取；不增加依赖。
2. 集成接口，运行局部验证，补真实SQL语句／Commit失败、并发及进程终止实验。发现基线不足先修订本页和规格再改实现。
3. 独立核对权限事务、事件恢复、密文绑定及接手说明，处理发现；运行全量测试、vet、build、契约、文档检查。
4. 按M01—M10回填版本／证据／限制，同步README、Manager组件、开发／测试及文档规划，保存本地提交。

## 内部协作接口

以下是本轮代码分工约定，不是新线上契约。包`internal/manager/operations`。

- 存储层文件store.go、crypto.go、types.go、migrations/001_operations.sql及对应测试。Identity{ManagerID,DigestKeyID,EncryptionKeyID string}；Keys{DigestID,EncryptionID string;DigestKey,EncryptionKey []byte}；Binding{NodeID,LedgerID,SessionID string}。
- Create(ctx,path,managerID,keys)与Open(ctx,path,expectedIdentity,keys)返回*Store；Store含db、boot、identity、validator及私有密钥／关闭状态。提供validID、validDigest、projectResourceID、timestamp、querier和`requireActive(ctx,q)`；errors集中定义。
- 加密内部blobBinding{OperationID,NodeID,Digest,Kind string;Seq uint64}，sealedBlob{Cipher,WrappedDEK,Fingerprint []byte;KeyID string}；seal(ctx,b,raw)、unseal(b,sealed)、fingerprint(raw)、InputDigest(raw)。seal在业务事务外调用。fingerprint返回([]byte,error)，InputDigest返回(string,error)。
- 控制层control.go、records.go及测试负责CreateUserFixture(ctx,id,enabled)、SetUserEnabledFixture(ctx,id,enabled)、SetGrantFixture(ctx,user,node,capability,granted)、BindSessionFixture(ctx,Binding)、AcceptFixture(ctx,Binding,userID,rawPrepare)(Operation,error)、RecordReadyFixture(ctx,Binding,operation,digest)error、AuthorizeStart(ctx,Binding,rawRequest)([]byte,error)、RequestCancelFixture(ctx,operation)error。
- Operation字段：OperationID,UserID,NodeID,LedgerID,StackID,VersionID,ProjectName,EndpointID,Digest,OwnerBoot,PreparedSession,State,PermitID,PermitSession,StartNonce,ReleaseStatus,CreatedAt,UpdatedAt string；PolicyRevision,Revision,AuthRev,LatestResultSeq,MaxEventSeq uint64；CancelRequested,Blocked bool。无result时LatestResultSeq=0，无permit时相关字符串空/authrev0。读层Get(ctx,id)、ReadPayload(ctx,id)、Claims(ctx,id)、Intents(ctx,id)。Claim{Kind,ResourceID string}；Intent{Kind,State,TerminalState string;Generation,ResultEventSeq uint64}。
- 共享事务helper：readOperation(ctx,q,id)(Operation,error)、readPayload(ctx,q,Operation)([]byte,error)、requireBinding(ctx,q,Binding,allowFrozen bool)error、requireClaims(ctx,q,Operation)error、readClaims(ctx,q,id)([]Claim,error)。readOperation为Store方法，其余requireBinding/requireClaims也是Store方法，readClaims为包函数。事件层直接使用同一DDL和这些helper，不重复实现关联判断。
- 事件层events.go及测试负责ReceiveEvent(ctx,Binding,raw)([]byte,error)、ReadEvent(ctx,operation,seq)(Event,error)。Event{OperationID,Digest,Kind string;Seq uint64;Body json.RawMessage}。首阶段只支持既有Agent首事件序列，不接finalized或自动恢复。

SQL约定：字段使用规格中的snake_case；operations主键operation_id，其他ID表主键id，manager_meta/keys主键singleton=1；outbox字段delivery_state，operation_events字段body_cipher。所有可选permit字段以空串／0存储，latest_result_seq无结果为0；created_at/updated_at/received_at为UTC Unix毫秒。schema_version=1，严格DDL与约束需由存储实现补齐并经独立审阅。

## M01—M10验收基线

| 编号 | 通过条件与观察方法 | 初始状态 |
| --- | --- | --- |
| M01 | 实际SQLite新建／重开；错误身份、两类密钥、版本、DDL拒绝；每连接参数与Close密钥副本验证 | 未验证 |
| M02 | 同事务受理、双claims、prepare；相同输入幂等，异输入拒绝；两任务争同Stack／项目只有一方持久成功；摘要按原字节 | 未验证 |
| M03 | 持久用户／Grant与许可串行裁决；撤权前后及并发测试直接检查permit、auth_rev；停用及缺任一N/C/D拒绝 | 未验证 |
| M04 | 同nonce同permit；换nonce、取消前置、旧会话拒绝；许可后撤权不否认已提交许可；不产生authorize outbox | 未验证 |
| M05 | started及首result转换覆盖正常0／非0、不完整、漂移、中断、未知、not_started、取消后正常完成；ack引用本次消息 | 未验证 |
| M06 | 原文重复不增加revision／事件／意图／顺序加密额度；异文持久隔离，首次证据不变、节点冻结、finalize抑制，跨重开保留 | 未验证 |
| M07 | result先到再started无回退；数学整数和转义空白原文保留；错误seq、类型、permit、节点／ledger／session／摘要无成功ack | 未验证 |
| M08 | 真实SQL语句与Commit失败无半操作／半事件／成功ack；真实进程强杀分别覆盖事务内和提交后，重开查事实及claims／意图 | 未验证 |
| M09 | Open／替换session冻结新受理与许可，旧Store同样不能写；新会话可补报同body；没有随意解冻入口 | 未验证 |
| M10 | 终态只pending且finalize原子、claims保留；正文／DEK／绑定破坏拒绝；库/WAL无测试秘密；额度回滚不退且耗尽可读；全量回归及接手说明 | 未验证 |

环境基线沿用Windows/amd64、Go1.27.0、CGO=0、modernc SQLite1.61.1／SQLite3.54.1；版本以最终运行记录复核。测试不把函数名称当证据，直接检查持久行、返回报文和故障后的再打开结果。

## 明确暂缺与下一依赖

2026-11-07实施中修订（M05/M10）：独立审阅按execution-recovery §4.1指出，接收匹配result还必须在同一事务将已有cancel意图置confirmed。先记录此遗漏，再补取消后成功测试对意图的断言并修正实现；没有改变公共协议或取消结局规则。

真实认证与认领、配置版本仓储、HTTP权限与幂等、outbox投递、网络任务处理、完整扫描／解冻、补观察、finalized释放、密钥轮换／备份回退和真实Compose均未实现。没有线上超时调度；许可10秒窗口仍由Agent Gate负责。本轮context和5秒SQLite锁等待不等于任务超时。完成本切片不等于完成M0或产品交付。真实环境需求在进入Docker／跨机验证前单独告知用户。

## 实施与审阅记录

先保存新模块测试，实际观察到Store／Types和方法未实现的编译失败，再实现独立存储、控制及事件层。存储与控制分别局部验证后集成。独立审阅覆盖事件的原字节／乱序／确认、权限与受理事务边界、加密和故障测试；主执行者交叉核对Store生命周期、控制读取和进程夹具。

审阅修正留下实际红绿证据：

- cancel后的正常成功result已记录，但cancel意图仍pending；增加意图断言实际失败，补同事务确认后通过。
- 未授权not_started之后出现seq2执行结果，原先只拒绝无许可而没有隔离旧终态；实际断言仍failed/pending，调整矛盾证据优先级后变blocked/held并抑制finalize。
- 将已有result的投影改回ready，四个控制／受理入口原先未发现隐藏结果；统一验证持久事件后拒绝。删除prepare／cancel意图后，Get和Intents原先未诊断；补完整性验证后通过。
- ReceiveEvent在缺失prepare意图时原先仍返回ack；先记录失败，再复用完整证据核对，最终回归通过。

最终事件修复经只读独立复核，未发现递归、嵌套事务或持锁等待问题；末次建议把重复事件的只读事务Commit结果检查补齐，由主执行者实施并重新验证完整Manager包及vet。不是在第一次测试成功后直接认定交付。未增加依赖、未改变共享Schema、Agent库格式、网络任务入口或历史归档。

## M01—M10最终结果

下表依据c02e1fd的实现与实际断言；测试文件都在`internal/manager/operations/`。这些编号仅表示本切片完成，不代表完整Manager进程或部署链完成。

| 编号 | 结果 | 证据、观察及限制 |
| --- | --- | --- |
| M01 | 通过 | [store_test.go](../../internal/manager/operations/store_test.go)验证排他新建、Open身份／密钥配对、错版本／DDL、路径、节点冻结、旧写句柄撤销、Close副本清理；每连接由verifyConnection核验运行组合，未实现旧库升级 |
| M02 | 通过 | [records_test.go](../../internal/manager/operations/records_test.go)检查操作／两claims／prepare同事务、原payload与HMAC、相同输入不增额度／修订、异输入拒绝、Stack及端点项目分别竞争；固定版本归属仍为Fixture输入 |
| M03 | 通过 | [control_test.go](../../internal/manager/operations/control_test.go)检查持久enabled／三项Grant、auth_rev变化、撤权与许可的两种顺序及并发竞争；直接读取许可与用户revision验证决定，没有真实登录／HTTP权限 |
| M04 | 通过 | PersistentPermissionAndReplay、SessionReplacementAndOpenFreeze检查同nonce返回原permit/auth_rev，换nonce和前置取消拒绝、后置撤权／取消不推翻已提交许可；无authorize outbox，网络请求关联尚未接线 |
| M05 | 通过 | [events_test.go](../../internal/manager/operations/events_test.go)逐项检查八类结果映射、有效ack/reply_to、取消后正常成功及cancel confirmed；所有结果仍来自受控夹具 |
| M06 | 通过 | OutOfOrderAndExactReplay、ConcurrentResultReplay检查同原文不增revision／事件／意图／顺序额度；EventConflictPersistsAndSuppressesFinalize检查原结果保留、节点冻结、冲突指纹去重及重开后仍可确认原文 |
| M07 | 通过 | seq2先于started仍保持succeeded/pending；body的空白与2.0原样重读，payload合法整数表示和元数据错配另测；错误session／ledger／node／digest／permit／future seq无ack，not_started与执行矛盾持久隔离 |
| M08 | 通过 | [manager_fault_test.go](../../internal/manager/operations/manager_fault_test.go)用真实语句失败和延迟外键Commit失败核对受理／结果全回滚、无ack且额度不退；[manager_restart_test.go](../../internal/manager/operations/manager_restart_test.go)在finalize插入后Commit前及result提交后分别强杀辅助进程，查原文、完整claims、投影及意图，无半记录 |
| M09 | 通过 | Open更新active_boot、冻结旧节点；旧Store不能写／预留包装额度；新session可重报历史body但不能恢复许可；没有核对完成解冻入口 |
| M10 | 通过 | 所有终态仍pending、claims不删；finalize与结果原子。密码测试覆盖正文／DEK／AAD／key ID／指纹篡改、原文独立解包、库与活动WAL秘密扫描、额度回滚重开不退／并发耗尽仍可读；Get／控制／接收拒绝损坏或缺失证据；完整检查与维护入口已同步 |

进程实验的具体额度为payload、started、result尝试各预留一次，共3次。事务内强杀后result不存在，重报新写后变4；提交后强杀重报保持3。两种情况都不释放claims或重发许可。进程Kill/Wait不等于掉电或存储设备失效测试。

## 运行环境、检查与复验

环境：Windows/amd64、Go1.27.0、CGO_ENABLED=0、modernc.org/sqlite v1.61.1、SQLite3.54.1、PowerShell7.6.9。工具及依赖未更新。

- 全量`go test ./... -count=1 -json`通过6个测试包，855条测试／子测试pass记录，其中Manager105条，失败0、测试skip0。父测试和无故障模式辅助入口也包含在统计内，不等于855个独立场景；contracts为纯嵌入包，其package skip不计作测试跳过。
- 全量时Manager包7.362秒、Agent账本14.081秒、通道16.171秒，仅记录本次运行。最后补重复事件的事务结束检查后，受影响Manager整包再次通过（5.149秒、105条pass），并重新通过该包vet；其余包未改。
- 全量vet、build、mod verify通过；契约检查通过20类消息、21正例、11反例、1解码输入。文档检查和Git空白检查通过；文档脚本只检查本地链接／锚点、表格列数和围栏，不代替语义或外部来源验证。
- 初步完整测试曾因运行环境限制不能连接回环端口，并有一次既有协议exact_total_limit测试报告schema_invalid；没有改这些模块。停止并发测试、按回环测试所需权限统一复验后上述场景全部通过，不把初次失败写成通过，也不据单次波动断言根因。

复验入口：在仓库根运行`go test ./internal/manager/operations -count=1`；完整回归用`go test ./... -count=1`，需允许本机回环通信和辅助进程。另运行`go vet ./...`、`go build ./...`、`go mod verify`、`pwsh -File tools/check-contracts.ps1`及`node tools/check-docs.mjs`。本轮原始运行输出保留在本地忽略目录`.cache/`，不是版本化交付物；可按上述命令重新生成。

下一步先落实可信完整核对扫描和会话恢复屏障，再接受控任务通道、双端终结释放，最后进入真实Compose执行与路径／端点验证。当前无需用户提供联调环境；进入真实Docker或跨机试验前，先说明所需系统、Docker／Compose组合、连接方式和具体观察项。
