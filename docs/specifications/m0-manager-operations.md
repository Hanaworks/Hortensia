# M0 Manager 本地持久协调模块规格

日期：2026-11-07。起点1d043a7。本篇与[实施计划](../implementation/m0-manager-operations.md)在本模块编码前保存。状态：实施基线，尚未验收。本轮技术选择由实施者依据既有设计落实，不表示用户逐项确认。

## 1. 目的、边界与依赖

模块`internal/manager/operations`是独立SQLite协调库：把固定任务输入转换成操作、资源占用和发送意图，依据持久用户／Grant及当前会话决定开始许可，把Agent的started／首result转换成可重读证据和操作状态。它不执行Docker，不提供HTTP或网络任务处理器。

共享主定义引用[数据设计](../data-design.md#7-m1-存储布局与物理类型提案)、[HTTP规格](http-api.md)、[Agent协议](agent-protocol.md)、[执行与恢复](execution-recovery.md)及[Schema](../../contracts/agent-v1.schema.json)。输入使用FrozenPayload／prepare；事件使用started／ExecutionResult；不在本篇另造线上字段。

本轮入口带Fixture的部分代表可信测试调用者提供身份、节点登记、已冻结配置和ready事实；它们不是认证、节点认领、配置仓储、真实预检或扫描完成接口。用户enabled/auth_rev和逐项Grant真实落库，许可事务自行复核，不能由调用者提供allowed布尔值替代。尚无密码登录、HTTP幂等键／回执、配置版本外键、投递工作线程、完整恢复扫描、finalized释放和部署程序。

完成边界为终态`release_status=pending`及固定finalize意图，或`reconciliation_required/held`。本模块没有删除claims的方法。网络原型继续冻结。真实Docker、跨机联调、备份回退、掉电耐久另行验证；需要环境前向用户说明。

## 2. 对外入口与生命周期

| 入口 | 输入与输出 | 前提／副作用 |
| --- | --- | --- |
| Create／Open | 文件路径、Manager身份、两份32字节密钥；返回Store | Create排他建新v1库；Open只接受完整既有v1结构、身份及密钥配对，进入恢复冻结；失败不重建文件 |
| Identity／Close | 持久身份；关闭错误 | Store不可复制；关闭后不再提供新写入，关闭数据库后清除持有密钥副本；调用者提供的密钥不修改 |
| CreateUserFixture | user_id、enabled | 新用户auth_rev=1；同身份重复创建拒绝；没有密码／角色功能 |
| BindSessionFixture | node_id、ledger_id、session_id | 初次新库的新节点可测试受理；不同ledger拒绝；替换session或Open后永久冻结新受理／许可；相同绑定重复不改变状态 |
| SetGrantFixture／SetUserEnabledFixture | 用户／节点／能力／授权或启用标记 | 变化与auth_rev递增同事务；无变化不递增；无解冻能力 |
| InputDigest | payload原字节 | 专用摘要密钥HMAC-SHA256，base64url无填充；调用者构造prepare时使用 |
| AcceptFixture | 当前Binding、user_id、完整prepare信封 | 校验Schema、内外目标、摘要、N/C/D授权；原子写操作、双占用、prepare意图；同operation同用户／固定输入幂等，异输入拒绝；不是HTTP幂等 |
| RecordReadyFixture | Binding、operation_id、input_digest | 代表已完成预检／prepare关联；accepted→ready，prepare意图confirmed；重复无修订增长 |
| AuthorizeStart | Binding、完整request_start | 事务内裁决许可；提交后返回合法authorize_start信封；不写通用outbox |
| RequestCancelFixture | operation_id | 可信本地取消意图，不实现HTTP取消权限；标记取消并写cancel意图，不擅自生成执行结局 |
| ReceiveEvent | 当前Binding、完整started／result | 校验、加密、原子提交后返回ack；未提交不返回成功ack；未知操作／旧会话／错误身份拒绝 |
| Get／Claims／Intents／ReadPayload／ReadEvent | operation_id及事件序号 | 可信内部读取，不承担HTTP可见性；正文解密后复核Schema及关联，损坏报错，不返回空证据冒充成功 |

Binding来自未来连接协调器，在本轮由夹具提供；信封session必须匹配Binding，且业务事务内再查节点当前session、ledger及当前Store的boot。Open写入新的active_boot并冻结所有节点，旧Store的写事务同样被拒；读取旧事实不等于恢复可执行资格。本轮不提供MarkReconciled。新session可补报历史body，但不得恢复旧许可；真正恢复解冻留给完整扫描模块。

Create要求本地普通文件路径，拒绝内存库、调用者DSN、UNC、问号及NUL。调用者创建父目录。每个物理连接核验SQLite3.54.1、WAL、synchronous=FULL、foreign_keys=ON、busy_timeout=5000；单Store最多一个连接，写事务使用immediate。同库可有多个读取句柄，但Open会撤销旧boot的写入资格。context取消／SQL失败／Commit失败统一不给成功结果；调用者只能查持久事实后决定重试。

## 3. 内部结构和本地数据

存储及密钥层负责文件／结构／密钥生命期；控制层负责用户Grant、会话、受理、ready、许可及取消；事件层负责原文字节、幂等、结果投影和ack。各层共享同一Store和事务，不经内存队列传递未提交事实。

| 表／对象 | 关键字段、来源及含义 | 约束与生命周期 |
| --- | --- | --- |
| manager_meta | singleton、schema_version=1、manager_id、active_boot、recovery_mode | Manager ID跨普通重启不变；boot每次实例新建；Open将recovery_mode置1，当前无清零入口 |
| keys | digest_key_id、encryption_key_id、两份key_check、uses | ID及配对校验持久；原密钥不落库；uses为KEK已预留包装次数，0—1048576，只增不减 |
| users／grants | id、enabled、auth_rev；user_id/node_id/capability | 用户与节点外键；能力只接受HTTP规格七项枚举；授权变化提升用户revision，与许可串行竞争 |
| nodes | id、ledger_id、session_id、boot_id、frozen | 当前连接绑定与历史操作授权会话分离；替换或重开后frozen=1 |
| operations固定部分 | operation_id、user_id、node_id、ledger_id、stack_id、version_id、project_name、endpoint_id、policy_revision、input_digest、owner_boot、prepared_session | 来自验证后的prepare与解码原payload；Manager重算摘要；本轮版本归属来自可信夹具，未建立配置仓储 |
| operations正文 | payload_cipher、wrapped_dek、key_id、fingerprint | 保存解码后的原payload字节；字段含义和加密见第5节；不存明文配置 |
| operations可变部分 | state、revision、cancel_requested、permit_id、permit_session、start_nonce、auth_rev、latest_result_seq、max_event_seq、release_status、blocked、created_at、updated_at | revision初值1，每个新事实／取消／隔离变化递增；重复无增长；result指针与最大事件序号分离，缺seq1时也可保存seq2 |
| resource_claims | node_id、resource_kind、resource_id、operation_id | 唯一(node,kind,id)；stack用stack_id；project用JSON字符串数组[endpoint_id,project_name]，避免拼接歧义；始终保留 |
| outbox | operation_id、kind、generation、delivery_state、result_event_seq、terminal_state | 唯一(op,kind,generation)，本轮generation=1；prepare/cancel/finalize意图，仅pending/confirmed/suppressed；没有重试调度或已发送承诺 |
| operation_events | operation_id、source=agent、event_seq、kind、body_cipher、wrapped_dek、key_id、fingerprint、received_at | 唯一(op,source,seq)；保存Validator返回的body原字节，按序读取不要求到达连续；不以message_id去重 |
| event_conflicts | operation_id、event_seq、kind、fingerprint、created_at | 持久保存异文指纹，重复指纹不增行；不保存第二份原文，不能从此表复原冲突正文；首次原文仍可读 |

本轮冲突表仅存带密钥指纹，沿用Agent当前切片的最小证据边界，避免未授权正文无限堆积；维护可确认相同冲突是否重现，无法离线比较第二份正文。若后续需要完整冲突审计，先增加保留／容量规则和验收，再升级结构。

## 4. 事务与行为

### 4.1 受理到许可

受理先验证完整prepare、解码payload并计算摘要。对已存在operation，在事务内核对固定字段和原字节，完全相同只返回旧操作；新输入先独立预留加密额度，再加密，最后在新事务中重查身份、当前会话、未冻结、用户enabled及N/C/D三项Grant、双方目标和资源占用。操作、两项claims和prepare意图必须一起提交，任何冲突或故障全部回滚。

受理入口的重复调用仍须当前有效绑定、节点未冻结、用户enabled及N/C/D权限；撤权／重开后用可信Get读取已有事实，不再调用AcceptFixture重新受理。它与HTTP回执的读取授权规则不同，也不采用已提交许可的历史重发规则。2026-11-07代码对照时明确此调用前提。

RecordReadyFixture只把accepted变成ready并确认prepare意图；本轮不假装已经实现线上reply_to登记。AuthorizeStart仍自行校验完整request_start。新许可事务重查当前boot/session、节点未冻结、操作ready、用户enabled、N/C/D、两项claims、无取消／blocked／result，固定permit_id、nonce、auth_rev和当前会话，state变authorized，Commit后才返回。nonce不同的再次请求拒绝。

同operation/session/nonce且当前boot仍有效时，返回同一permit_id/auth_rev及本次reply_to，不重做授权、不延长Agent的10秒窗口。已提交许可之后的撤权／取消不能撤销过去事实；没有许可时，先提交的撤权／取消阻止许可。返回已有许可前仍核验当前连接、占用及没有矛盾证据，终态／隔离后不重发。许可不放进可跨会话自动重试的outbox。

取消只记录意图：新取消增加revision并写cancel/pending，重复不增；收到真实结局后按实际证据判定，不能因cancel_requested=true就把正常成功改成canceled。终态不新增取消意图。

2026-11-07实施中补充：接收匹配result的事务同时确认已有cancel意图（承接执行与恢复§4.1）；started仅确认prepare，不能确认cancel。正常成功与cancel确认可以同时成立：确认表示已有结局证据，不代表取消成功。

这里确认的是新result提交之前已存在的取消意图。核对中的未知结果之后才请求取消时，cancel仍pending；重复更早的同body回报保持幂等，不将过去证据当作新取消的确认，后续需要补观察／恢复模块处理。

### 4.2 事件保存、乱序与冲突

ReceiveEvent先验证完整Schema、body≤7MiB、方向及当前session，再进入事务核对节点／ledger／操作／摘要、claims和事件位置。合法范围为seq1=started或not_started result，seq2=非not_started首result；后者必须已有许可，started必须引用原permit。允许seq2先到，然后seq1补历史；补历史增加revision但不回退结果／释放状态。超出本切片的补观察版本拒绝，不冒充未来功能。

相同(op,agent,seq)且kind和body逐字节相同：返回本次ack，不重新加密、不增加revision／事件／finalize。不同正文或与已存序列矛盾：保留旧证据，记录冲突指纹、blocked=1、state=reconciliation_required、release_status=held、抑制finalize意图，提交后返回ErrConflict且无ack。节点冻结，阻止同节点其他新任务／许可；重复已保存的原文仍可确认。首次冲突指纹只保存一次，不无限增长相同记录。

新事件先独立预留并加密，业务事务再次核对全部条件；写事件、max_event_seq、latest_result_seq、状态、revision及必要finalize意图一起提交。并发败方可能消耗已预留额度，但只保留一份事件。Commit失败不构造成功ack。ack的body只有operation_id/event_seq，session与本次连接一致，reply_to指本次message_id；ack不释放资源。

| 首result证据 | 操作状态 | 释放／意图 |
| --- | --- | --- |
| normal_exit，观察complete，ownership consistent，exit_code=0 | succeeded | pending；固定result序号和terminal_state的finalize/pending |
| 上述条件成立但退出非0 | failed | pending；finalize/pending |
| not_started，error.code=canceled_before_start | canceled | pending；finalize/pending |
| 其他合法not_started | failed | pending；finalize/pending |
| interrupted／unknown／观察不完整／归属drift或unknown | reconciliation_required | held；不生成finalize，节点冻结 |

started表示开始资格已持久提交，不等于进程实际调用。未经真实执行器证明的测试result仍只作为夹具事实验证转换。本轮不实现Stack.last_applied_version_id更新；未来配置仓储接入须在结果事务中增加并验收。

## 5. 加密、重读和恢复边界

Keys包含不同ID且字节不同的DigestKey和EncryptionKey，各32字节；与Agent密钥独立。密钥配对HMAC绑定Manager身份、用途和ID，即使空库也能拒绝错密钥。input_digest只对原payload字节做HMAC-SHA256，不加域、不重编码。内部fingerprint采用独立用途前缀，不作公共摘要。

每条payload／event使用随机32字节DEK；正文与包装DEK均使用标准库AES-256-GCM随机nonce封装。AAD用JSON数组绑定用途版本、manager_id、operation_id、node_id、input_digest、kind、seq和key_id；payload的kind=payload/seq=0，event用实际kind/seq。正文与DEK包装使用不同用途域。先用独立写事务把uses加1并Commit，再产生密文；额度达到1048576拒绝新包装，业务回滚／并发败方／崩溃不退额度；已存在事实可重读及幂等确认。

重读既验证两层AEAD和指纹，也验证原Schema与行关联。组装校验信封时只编码信封头、拼接原body，防止json.Marshal对原文再次转义或压缩。ReadPayload返回独立副本及重算摘要核对；ReadEvent保持原文数学整数表示和空白。敏感正文、原密钥不得出现在错误／明文表／日志中。

普通Open做结构与quick_check、身份及密钥配对，再用事务设置新active_boot和恢复模式、冻结节点；事实／claims／意图不丢失。当前会话重新绑定后只允许补报历史事件。未知库、结构变化、损坏或密钥不符均报错；不自动修复、迁移或删库。备份回退可能回退KEK计数，当前不支持作为可写恢复；没有反回退机制，不得直接恢复旧副本继续补写。进程终止实验仅证明已测提交边界，不证明掉电耐久。

## 6. 验证与维护

稳定验收M01—M10见[实施记录](../implementation/m0-manager-operations.md)。修改字段先查Schema／数据主定义，再查本篇本地约束和SQL；修改许可查撤权排序、重复许可、会话替换；修改事件查乱序、冲突、原字节、提交失败及保留占用；修改加密查额度与绑定破坏。操作状态不等于释放状态，不能在UI或下一模块把pending解释成可重用资源。

故障定位顺序：确认Manager身份／库版本／密钥配对 → active_boot与节点session/frozen → 操作固定目标与两项claims → 原许可及auth_rev → 事件原文／序号／result指针 → finalize状态。ErrInput为不合法结构，ErrIdentity为身份不符，ErrPermission为持久权限不足，ErrFrozen为恢复／会话屏障，ErrConflict为固定输入或证据冲突，ErrClaimed为占用竞争，ErrEvidence为持久证据不一致，ErrKeyExhausted为包装额度用尽，ErrStorage为数据库／关闭失败；错误不包含正文。

交接时补入实际源码定位与逐项证据，保留本次编码前基线和事后补充时间。

## 7. 实现中的补充与接手路径

2026-11-07，基线3bf8629保存后补充：SQLite中input_digest是32字节BLOB，公开Operation.Digest为base64url无填充字符串；不是两份摘要。JSON中的1.0／1e0等合法整数经共享Validator确认后转为精确安全整数，原body仍按原字节保存。字段物理约束以[Manager v1 DDL](../../internal/manager/operations/migrations/001_operations.sql)为准；不是Agent v1/v2的升级。

| 维护入口 | 内部协作与不变量 |
| --- | --- |
| [store.go](../../internal/manager/operations/store.go)／[types.go](../../internal/manager/operations/types.go) | Create先验证路径／密钥再排他建文件并提交结构／身份；Open核验全DDL与密钥再更新active_boot；requireActive在写事务内查持久boot，Close锁外关闭DB后清理持有副本 |
| [crypto.go](../../internal/manager/operations/crypto.go) | keyCopies隔离Close与在途密码操作；reserveKeyUse独立事务并复核active_boot；seal先预留后包装；unseal没有DB调用，可用于唯一连接上的事务内读取 |
| [records.go](../../internal/manager/operations/records.go) | prepare→原payload→本地字段／摘要→加密输入→操作及两占用／prepare意图；先查重复再加密，业务事务重查防止预检和提交之间条件改变 |
| [control.go](../../internal/manager/operations/control.go) | Fixture登记事实，Grant／enabled变化提升auth_rev；许可与撤权共享SQLite写事务顺序；旧permit只是历史决定，当前session／boot屏障仍须通过 |
| [events.go](../../internal/manager/operations/events.go) | ReceiveEvent先做无新包装的重复检查，再seal并二次事务复核；readEvent解密并验证Schema；validateOperationEvents在同一快照比较事件与状态／finalize，防止坏投影伪装成无结果 |

例如任务A已授权，Agent先回报seq2成功result：Manager在一个事务内存原body、把A改成succeeded/pending、记录finalize(2,succeeded)，仍保留两项claims。随后seq1 started只补原permit证据并增加revision。若相同seq2换成退出码1，首次result不被覆盖；持久冲突、节点冻结、A转核对并抑制finalize。若Manager在任一Commit后丢应答，当前会话重报同body只取得新ack；重启后需重新绑定当前会话，仍只能补事实，不能再发旧许可。故障处理者应查实际事件及意图，不按最后收到的消息猜状态。

当前Get校验固定输入、claims与事件投影；可信只读接口返回的正文可能含秘密，不能直接用作未来HTTP输出。未来接线仍须完成认证、方向／reply_to关联、恢复扫描以及权限投影。新增完整恢复时必须修改当前“Open后不解冻”限制并新增验收，不能绕过节点frozen字段。
