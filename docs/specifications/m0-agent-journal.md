# M0 Agent 事件、首份结果与恢复读取实施规格

日期：2026-11-07。起点：`fac4fa6`。本规格在编码前保存；实施顺序、验收结果和交付版本见[执行计划](../implementation/m0-agent-journal.md)。

## 1. 目标、来源与边界

承接已验收的[开始账本](m0-agent-ledger.md)，为后续受控任务通道提供可持久读取的started／result证据。本轮完成开始事实与started事件原子提交、首份受控结果、加密保存、重复／冲突处理、逐事件确认，以及单操作一致恢复读取。重启可读取原事件，不恢复执行权。

需求来自R04／R07／R12、Q04／Q07／Q08，主定义为[Agent协议](agent-protocol.md)第5—7节、[执行与恢复](execution-recovery.md)、[数据设计](../data-design.md)第7—11节和[Agent模块](../components/agent.md)。沿用现有Schema的started、ExecutionResult、RecoveryRecord，不创造另一份线上报文。

结果写入明确命名为Fixture：调用者提供受控执行结局，本模块校验结构及账本前提，不能证明真实进程退出、Docker观察或ownership_status。仅保存首份result；补观察的后续结果版本留待后续。当前不实现网络发送／重试调度、Manager授权与确认处理器、完整reconcile分页及完成握手、finalize、占用释放、真实执行、密钥文件管理、自动轮换或备份恢复。

## 2. 版本与兼容

继续使用现有Go1.27、CGO关闭和锁定SQLite依赖，不新增第三方库。新建v2完整DDL位于 `internal/agent/ledger/migrations/002_event_journal.sql`，这是新库定义，不是可直接对v1执行的升级脚本。

新增CreateJournal/OpenJournal入口；原Create/Open保持v1行为和原DDL不变。OpenJournal(v1)、Open(v2)均返回ErrSchema，保留原文件；不自动升级、删库、重建身份或补造历史started。CreateJournal只用于新建受控实验库，不能作为既有节点的升级或绕过旧占用的路径。v1→v2迁移及备份组恢复仍待专门模块，不能将两版账本同时接到同一真实节点。

两版共用已有安全文件打开、WAL/FULL/外键/5000、每连接读回及结构校验。v2也精确比较其版本DDL、兼容LF/CRLF，并拒绝未知结构。所有正式写入使用立即写事务和同一个sql.Tx。

## 3. 密钥、加密与数据

每个新v2账本由调用方注入独立的JournalKey：ID为1—128字节有效UTF-8，Key为32字节，必须不同于K_ledger；调用方须用安全随机数产生并专用于本账本，不跨账本共用。Store复制两种密钥，Close及局部操作结束时尽力清零副本，不承诺Go内存完全擦除。OpenJournal在空库时也验证实际KEK配对；错ID、错键或缺键不能靠新建密钥修复。

完整事件body按敏感内容加密，不能只加密output_b64。每条事件使用crypto/rand生成独立32字节DEK；正文与包装DEK均用AES-256-GCM和Go的NewGCMWithRandomNonce，nonce由标准库放入密文。两种AAD分别为JSON数组：`["dm-event-body-v1"或"dm-event-dek-v1",node_id,ledger_id,operation_id,input_digest,kind,event_seq,key_id]`。key_check为HMAC-SHA-256(KEK, `dm-event-key-v1` + 单字节0x00 + JSON数组[node_id,ledger_id,key_id])；body_fingerprint为HMAC-SHA-256(K_ledger,原始body字节)，仅内部使用。篡改、跨行替换、错用途或错误包装必须固定报错，不能返回部分正文。[Go标准库说明](https://pkg.go.dev/crypto/cipher#NewGCMWithRandomNonce)

KEK每次包装前，先在独立短事务中将uses加1并提交，再生成DEK和加密，之后才开始事件业务事务。固定上限为1,048,576次，低于标准库2^32上限；预留失败不加密，预留成功后其他步骤失败可以浪费额度，绝不退还。不允许在已占用唯一连接的业务事务里再开预留事务。额度耗尽拒绝新加密写入，仍允许读取、重报和确认。普通重开沿用可靠本地计数；旧备份回退可能回退额度，本模块不支持将恢复的备份用于继续写入，不宣称具备回退检测。

| v2新增表 | 主要字段与约束 | 用途 |
| --- | --- | --- |
| event_keys | id=1；key_id唯一；key_check为32字节BLOB；uses为0—1048576整数 | 单一KEK配对与永久预留次数，不存密钥 |
| event_state | operation_id主键并外键引用executions；last_seq为0—安全整数上限；latest_result_seq可空、不得大于last_seq；blocked为0/1 | 与prepare夹具同事务建立，序号及结果指针原子更新；结果指针组合外键引用真实事件 |
| execution_events | `(operation_id,event_seq)`主键；kind为started/result；body_cipher、wrapped_dek、key_id、32字节body_fingerprint；ack_session可空 | 外键关联操作及key；密文和包装同事务发布；ack不删除或改写body |
| event_conflicts | `(operation_id,event_seq,body_fingerprint)`主键；created_at为Unix毫秒；operation外键 | 同位置异内容只追加不含正文的冲突指纹，并将blocked置1；保留原事件与占用 |

表采用STRICT、NOT NULL、CHECK和禁止级联删除的外键。原executions.phase继续表示ready或START_COMMITTED，保留开始许可；结果阶段由event_state及恢复视图派生，不能为显示result而清空开始事实。元数据、指纹、计数和确认session明文，配置／结果正文和密钥不进入数据库、WAL或诊断明文。

## 4. 内部接口与协作边界

```go
type JournalKey struct { ID string; Key []byte }
func CreateJournal(ctx context.Context, path, nodeID, keyID string,
    key []byte, journalKey JournalKey) (*Store, error)
func OpenJournal(ctx context.Context, path string, expected Identity,
    key []byte, journalKey JournalKey) (*Store, error)
type Event struct {
    OperationID, Digest, Kind string
    Seq uint64
    Body json.RawMessage
    AckSession string // 空为尚无确认；非空为最后确认连接
}
func (s *Store) RecordResultFixture(ctx context.Context, rawResult []byte) (Event, error)
func (s *Store) ReadEvent(ctx context.Context, operationID string, seq uint64) (Event, error)
func (s *Store) PendingEvents(ctx context.Context, operationID, sessionID string) ([]Event, error)
func (s *Store) AcknowledgeEvent(ctx context.Context, operationID, digest string,
    seq uint64, sessionID string) error
func (s *Store) RecoveryRecord(ctx context.Context, operationID string) (json.RawMessage, error)
```

以上是存储接口。AcknowledgeEvent的调用方必须先完成认证、消息方向、当前session及reply_to关联验证；本轮只以受控调用验证持久确认，不提供线上ack处理器。函数自身校验ID、摘要、事件存在及记录关联，重复同session确认幂等；确认旧事件不覆盖最新结果。PendingEvents按event_seq升序返回ack_session不等于所给session的事件，因此新连接仍可取得已在旧连接确认的证据；它不是发送队列或扫描完成判据。

ReadEvent返回解密后的原body字节副本，重报时仅更新信封；本模块没有网络封装入口。每操作至多一条started与一条首result，PendingEvents至多2条。result经现有Validator按完整报文及1MiB输出上限验证，另限制body不超过7MiB，为RecoveryRecord及后续完整报文留出空间；超预算拒绝，不默默截断并称完整。合法1.0／1e0事件序号按数学整数提取，但body原字节不重编码；同位置不同JSON字节也按冲突处理。

继续使用已有固定错误，增加ErrJournal（非v2入口）、ErrEvidence（密文／关联证据无效）、ErrKeyExhausted；错误不带正文、密钥、SQL参数或驱动细节。读取坏密文整次失败，不输出部分结果或假造空结果。

## 5. 提交、重复、冲突及重启

v2的RecordReadyFixture同事务建立event_state(last_seq=0)。NewGate以及开始事务都复核未blocked、尚无result；只有数据库预读不够。开始回调先预留加密额度并制作started密文，再在原开始事务中提交完整许可、START_COMMITTED、序号1事件及last_seq=1。started_at只生成一次，开始记录和body完全一致。任一语句或Commit失败均不Start；提交后取消／断线／到期仍保留started，但不据此认为进程已创建。

RecordResultFixture校验既有result信封／Schema、操作／摘要及两个资源占用。新写必须本boot并与prepared_session一致。ready只接受not_started，期望seq=1；START_COMMITTED接受normal_exit/interrupted/unknown，要求已有完整started、期望seq=2。not_started的当前boot夹具代表受控未启动证据；数据库事务与开始提交竞争，只能其中一方取得资格。真实执行／归属证据仍须未来执行器提供。

序号分配由事务中last_seq+1决定，传入result的event_seq必须等于该值。插入事件、更新last_seq及latest_result_seq在同事务完成。此后不接收新result版本：同一位置同body返回已有事实，不增加序号、不消费新额度；同位置异body持久记录冲突指纹并blocked，返回ErrConflict；原body、结果指针及占用不变。不同未来序号直接拒绝，不伪造补观察。并发请求可以浪费已经预留的密钥额度，但只能提交同一份事件事实。

已有事件可在新boot读取、确认及相同body重放；旧boot不能新写执行结局、重新准备或NewGate。blocked操作仍可读取原事件和确认，其恢复phase为reconciliation_required，不清冲突或释放。没有结果的旧START_COMMITTED也投影为reconciliation_required，不能凭进程已消失构造not_started或normal_exit。当前boot下相同状态保留START_COMMITTED含义。

RecoveryRecord在一个事务快照内读取操作、event_state、事件、确认及两占用。它输出现有Schema的精确字段，latest_result及result_event_seq必须指向同一原body，last_event_seq不得凭调用次数增加；有结果且未blocked则phase=result，无结果则按上述阶段映射。report_acked仅表示最新result曾获持久确认，没有result为false；当前连接的确认另由AckSession判断。claims_held必须通过两个占用完整匹配确认；缺失或不一致返回ErrEvidence。final_state和finalized_result_event_seq始终null。此读取不是完整恢复扫描，也不授权解除冻结。

Close沿用既有锁顺序并清理KEK副本；不得持Store锁等待Gate，解密／加密期间使用受锁保护复制出的密钥。Start仍必须短小，不能重入Gate或调用Store.Close，可以查询已提交事件。所有结果、ack、读取及重启路径都不释放local_claims。

## 6. 验收与完成条件

以本机Windows/amd64、Go1.27、CGO关闭、本地SQLite、受控结果与实际辅助进程为环境。J01—J10全通过、无未解决重要审阅问题、全量回归／构建／契约／文档检查通过，才能交付本模块。真实Linux、掉电、备份回退及完整M0不在本次通过结论内。

| 编号 | 输入／动作／故障点 | 可观察通过条件 |
| --- | --- | --- |
| J01 | 新v2创建重开、错键／ID／等同K_ledger；交叉v1/v2打开 | 正确身份和结构保留；错误拒绝、文件不改；v1旧测试仍通过，不隐式迁移 |
| J02 | 同内容独立加密、正文／包装篡改、跨操作／序号替换、关闭并发 | 原字节往返，密文不同；错配整次失败无秘密诊断；库/WAL无测试明文／密钥，无关闭死锁 |
| J03 | 额度预留后事件写入／Commit失败、进程终止；额度耗尽 | 预留次数不回退、未提交事件不可见；无Start；耗尽仍可读取、ack和恢复查询 |
| J04 | v2正常许可、重复许可、started插入失败、提交期间取消 | Start内可读完整started；开始与事件全有或全无，最多启动一次；取消不删已提交证据 |
| J05 | 四类首结果、错误目标／摘要／session／boot／seq／预算；not_started与开始并发 | 合法结果与指针原子提交；非法输入拒绝；竞争只允许未启动结果或开始事实其中之一 |
| J06 | 相同result重试、同位置改body、并发重复、未来版本 | 重复不增序号或新额度；冲突保留旧证据并持久blocked及指纹；不得另起任务或覆盖较新结果 |
| J07 | 正确／错摘要／错位置确认，重复确认，重开与新session | 确认只改变相应ack_session，不释放占用；新连接PendingEvents重取原body；不声称已验证网络关联 |
| J08 | ready、当前/旧boot开始无结果、四类结果、blocked／缺占用 | RecoveryRecord符合Schema且字段来自同一快照；保留许可、未知隔离、latest_result和seq一致，ack不代表释放 |
| J09 | 辅助进程在started提交后、result提交前事务中、result提交后及ack提交后强制终止 | 重开只有完整提交的事件／指针／确认；body不变、额度不回退、原占用仍在、旧操作不重启 |
| J10 | 完整测试、vet/build/mod verify、静态契约／文档及独立审阅 | 前两模块回归通过；新入口、源码版本、逐项证据和暂缺能力可查；通道仍冻结 |
