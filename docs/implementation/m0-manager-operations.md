# M0 Manager 本地持久协调实施与验收

日期2026-11-07，起点1d043a7。状态：编码前基线，M01—M10均未验证。[规格](../specifications/m0-manager-operations.md)是本轮实施依据。沿用当前工作区，不改变Agent库格式或网络冻结状态。

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
