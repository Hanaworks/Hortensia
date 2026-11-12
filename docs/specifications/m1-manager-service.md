# M1 Manager账号、节点与配置服务规格

日期2026-11-12；阅读起点`656e355`。初始状态为编码前草案；2026-11-12实现同Store服务内核，最终测试范围见实施记录§7。依赖Agent v4／Manager v3双端释放提交`6f92eb3`；其既有验收范围不扩大为真实网络或执行。本模块新增显式Manager服务v4，先完成首版路线中的C＋D管理路径，不把网络投递、真实Compose或Web一并宣称完成。技术取舍是实施方案，不是用户逐项确认。

## 1. 目的、主定义与终点

把现有受控Manager协调库连接到真实的本地账号、节点登记／凭据、配置版本和HTTP请求，使操作者可以初始化、登录、授权、登记节点、保存配置和查询受理结果。实际任务仍由唯一operations账本处理；HTTP不能建另一份“任务成功”记录或调用Fixture替代身份、配置、会话和执行事实。

共同定义引用[HTTP规格](http-api.md)、[API会话与权限](../api-design.md#62-用户会话与权限)、[数据设计](../data-design.md#71-manager-表与约束)、[Manager组件](../components/manager.md)、[运行配置](../operations.md)及[共享Schema](../../contracts/agent-v1.schema.json)。本篇维护服务的私有职责、存储、请求摘要算法和事务接入；HTTP字段、权限集合、返回对象及错误码继续以HTTP规格为准，不另取别名。验收和实施顺序见[实施草案](../implementation/m1-manager-service.md)，首版整体门槛见[首版推进](../implementation/m1-first-version.md)。

模块完成终点是：真实TLS HTTP入口使用同一SQLite权威完成账号／授权、节点认领和配置版本路径，Apply有正式的固定输入／事务受理入口，离线或尚未接入正式任务通道时明确拒绝，不伪造online或Agent结果。操作查询与取消有真实权限边界。网络适配器接线、Agent准备／执行、完整Web和O12人工恢复分别继续后续模块；仅此服务通过不代表首版可交付。

## 2. 现有实现约束与版本方案

已阅读[Store](../../internal/manager/operations/store.go)、[授权及取消](../../internal/manager/operations/control.go)、[操作受理](../../internal/manager/operations/records.go)、[加密](../../internal/manager/operations/crypto.go)、[恢复绑定](../../internal/manager/operations/recovery_store.go)及现行DDL。现有Store持有单连接、密钥副本、active_boot和恢复屏障；写事务为IMMEDIATE，密文包装额度单独预留。AcceptFixture自行开两次事务，现有users只有enabled／auth_rev，nodes由可信Fixture绑定创建；这些接口不满足HTTP幂等、待认领节点及账号认证。

本轮新增完整`004_service.sql`与`CreateService／OpenService`，承接Manager v3释放结构，旧v1／v2／v3入口与DDL保持原版本，不自动升级或把已有库换为空库。v4包含原操作、许可、事件、释放、恢复和占用的同一组表，不另设业务数据库；服务和通道共享同一Store。开发初始服务库不等于已交付历史库迁移工具。版本判定按明确支持集合分派，不用任意较大版本自动取得恢复或释放资格。

v4将users.auth_rev改为允许0以满足HTTP新用户契约。新用户无Grant；授予任何能力的变更递增修订，因此获得部署所需授权后许可记录仍可保持auth_rev大于0。v4的Fixture入口拒绝调用；旧版本测试入口保留。复用的是提取后的私有事务核心及证据检查，不是让生产handler绕过其缺失校验。

现有单KEK／摘要密钥结构暂保留，所有新密文使用同一额度权威；idempotency_records显式保存digest_key_id。此模块不交付自动密钥轮换或旧备份回退；换钥打开拒绝，旧键保留／多代密钥读写在备份及轮换模块展开，不据保存一个key_id宣称已支持轮换。

## 3. 内部结构与调用方向

| 位置／职责 | 输入、输出与边界 |
| --- | --- |
| `cmd/manager`及`internal/manager/service` | 拟承接配置、文件权限、初始化、单实例锁、TLS启动、关闭；不执行SQL或自行判定部署成功 |
| `internal/manager/httpapi` | 严格解码、路由、Origin／CSRF／Cookie及Bearer适配、8MiB限额、响应白名单与错误映射；不持有sql.DB，不把原操作行直接返回 |
| `internal/manager/operations`中的service存储及业务文件 | Store唯一拥有连接和事务；认证、授权、节点、配置、幂等、审计通过按用途划分的方法和私有Tx函数协作；目录名称不要求每个职责独立部署 |
| 既有operations核心 | 保留操作、claims、许可、回报、恢复和释放规则；正式受理使用acceptTx，正式取消使用cancelTx，二者不自行提交事务 |
| 后续Manager传输适配器 | 通过正式凭据验证、绑定／失效入口维持当前会话，发送只读持久意图；本轮不提供伪online设置接口，网络连接不能拥有第二份业务状态 |

正常HTTP请求经过结构检查 → 会话认证 → 当前可见性／权限 → 规范请求及幂等检查 → 私有事务服务 → 固定响应。Argon2计算、密文包装、网络发送不在业务事务内进行；最终事务重查会话、账号、Grant、版本和节点资格。历史查询也在一致快照内完成授权与正文读取，不能先取敏感原文再按失效权限决定输出。

内部拟用`BrowserSession`保存不可由调用者构造的会话验证值及Store来源；它只定位持久会话，不缓存永久授权。`Mutation`保存规范方法／路径、key、If-Match及已校验DTO。服务方法的每个最终事务都重新验证会话；不接受浏览器提交的user_id作为操作者。Agent侧`AuthenticatedNode`同样绑定凭据代次、节点及Store，不以裸Binding代替认证。测试可以通过真实登录／认领取得这些句柄，不允许手工设置内部可信布尔值。

## 4. 服务生命周期与初始化

拟实现命令语法为`manager init --config <绝对路径> --admin <用户名>`与`manager serve --config <绝对路径>`；这是待实现语法，不是已验证安装教程。口令从交互终端隐藏输入，自动验证使用受限输入流；不接受口令命令行参数或通用默认密码。配置字段严格采用运行篇Manager字段，未知／重复键、相对文件路径、非法origin均拒绝，不用环境变量或任意Forwarded头改变执行配置。

init取得数据目录的操作系统排他锁，确认目标库、身份和密钥没有冲突，创建服务私有目录及独立随机密钥，保存含manager_id和key_id的本地身份清单，再以一个v4创建事务建立结构、密钥校验、首个enabled管理员及初始化完成标志。首管理员auth_rev=0、system_admin=1、无节点Grant。密钥文件0600、目录0700；发布与父目录同步按Linux验证。失败材料保留，重复init不覆盖既有库、身份或密钥；明确报告需要核查的初始化状态。是否提供显式恢复初始化命令另按失败实验决定，不能在serve中隐式补造。

serve读取相同身份与密钥、核验版本和SQLite参数，打开Store使旧boot失去写资格并冻结已有节点；确认TLS证书、external_origin和监听配置可用后才接收请求。已有DB但缺身份或密钥、密钥不符、未完成初始化或未来schema立即失败。进程锁覆盖完整服务寿命；不允许第二个Manager连接同库争抢active_boot。

服务使用`net/http`和明确TLS配置；没有HTTP明文业务入口、insecure或任意CORS允许。拟定HTTP技术预算为ReadHeaderTimeout=5秒、IdleTimeout=60秒、普通API处理context=30秒、MaxHeaderBytes=32KiB及8MiB请求体上限；这些值在本模块慢请求实验后固定，不改变10秒Agent许可窗口。后续WebSocket另走连接生命周期，不受普通API总时限误杀。

收到退出信号后停止新HTTP受理，等待已开始请求在关闭预算内结束；提交之后客户端断开不撤销已持久结果，重试按原key查询。随后停传输worker、关闭当前连接、关闭Store并清理密钥副本，再释放进程锁。取消HTTP请求不是撤销Apply业务意图；取消必须使用既定cancel路由。关闭预算到期的未确定事务按重开／幂等检查，不补写“未执行”。

## 5. v4重要持久数据

下表为私有物理展开。UUID、Unix毫秒、布尔、修订范围及AEAD共同规则沿数据设计；本轮DDL必须实现NOT NULL、外键、CHECK和唯一约束。缺省NULL仅用于明确尚无的业务事实，不以零时间替代。已有操作／恢复／释放字段不重复定义。

| 表／扩充 | 新字段、来源与含义 | 约束、生命周期及维护规则 |
| --- | --- | --- |
| manager_meta | `initialization_complete` | v4仅在首管理员及结构完整提交后为1；常规Open不补管理员；原manager_id／active_boot／recovery_mode仍为唯一权威 |
| users | 原id／enabled／auth_rev扩充`login_name,password_hash,system_admin,created_at` | login_name唯一；新用户修订0，无Grant；密码为带算法／成本／盐的Argon2id编码；禁用保留身份和操作外键；禁止禁用最后一个启用管理员 |
| user_sessions | `id,token_hash,user_id,auth_rev,created_at,last_seen_at,expires_at,revoked_at,csrf_cipher,wrapped_dek,key_id,fingerprint` | id UUID唯一，token_hash为32字节主键；只存Cookie摘要；CSRF以会话id／token_hash用途绑定加密，可由GET me重读同值；失效记录可保留，不作为永久授权 |
| nodes | 原id／ledger_id／session_id／boot_id／frozen扩充`display_name,credential_generation,revoked_at,created_at,last_seen_at,agent_capabilities_json,execution_profile_json` | pending时ledger/session/boot为空串、generation=0、frozen=1；认领后ledger为有效ID且不可改；连接快照不解除frozen；本服务无整节点观察写入入口，Node.observation为null；不另建同义的节点权威表 |
| node_credentials | `node_id,generation,token_hash,enabled,created_at` | 联合主键，hash唯一；每节点仅当前代enabled；撤销／轮换与nodes代次同事务；原秘密只由Agent保管 |
| enrollments | `id,token_hash,node_id,ledger_id,expires_at,consumed_at,candidate_hash,revoked_at,created_at` | 未消费ledger/candidate为空；消费后固定绑定；token_hash唯一；每节点最多一个未撤销未消费的有效材料，由同事务检查；保留已消费记录支持响应丢失重试 |
| grants | 沿原`user_id,node_id,capability` | PUT整体替换目标用户在该节点的能力集合，单次变更只递增一次auth_rev，空数组撤销；服务不逐项调用SetGrantFixture |
| stacks | `id,node_id,project_name,display_name,current_version_id,revision,last_applied_version_id,last_operation_id,observation_operation_id,created_at,disabled_at` | `(node_id,project_name)`唯一，停用不复用身份；display初始等于project；current必属同Stack；版本／操作指针以组合外键约束；当前没有重命名或删除API |
| config_versions | `id,stack_id,parent_id,created_by,created_at,package_cipher,key_id,wrapped_dek,fingerprint` | `(stack_id,id)`唯一；parent只指同Stack；初始版本parent=NULL；原Config规范编码发布后不变，解密恢复原文件字节和数组顺序；AAD绑定版本／节点／Stack／用途 |
| idempotency_records | `user_id,idempotency_key,method,path,request_digest,digest_key_id,result_type,result_id,response_status,safe_response_json,response_etag,created_at` | `(user_id,key)`唯一，跨K路由共用空间；非空结果与业务同事务；只保存白名单非秘密回执；永久“处理中”行禁止发布；不自动回收 |
| operations | v3字段扩充`idempotency_key,digest_key_id`及服务关联 | `(user_id,key)`唯一并关联idempotency_records；node等于Stack节点、version属于Stack；原operation_id不改成HTTP自选ID；已有payload、许可、事件、claims、释放仍由同一行控制 |
| audit_events | `id,user_id,node_id,action,target_type,target_id,operation_id,created_at,outcome` | user_id/node_id恰一非NULL，分别指浏览器或节点主体；与管理变更、认领及轮换同事务；认证失败可用独立无秘密诊断，不能伪造成功审计；不含密码、token、CSRF、Config、原始执行输出 |

列表索引采用users／nodes／stacks／operations的`(created_at,id)`对应实际主键；Grant列表用用户排序键。Stack先生成版本ID，两条循环关联可用DEFERRABLE外键在同一事务插入，提交时current_version_id必须非空且匹配，不先返回空Stack再补配置。原操作表加服务外键前须重新验证v3核心兼容；旧版本DDL和旧Fixture回归仍使用自己的结构。

NodeStatus按HTTP主定义派生，不将online保存为独立永久事实：无认领为pending、撤销优先revoked、恢复隔离为reconciling，其余由当前认证会话为online／offline。最近能力可以保留，不能因为数据库里有session字符串而把重启前连接当online。当前模块缺少正式传输时，已认领节点保持offline或reconciling，Apply据真实资格拒绝。

## 6. 账号、会话、权限与秘密

密码严格按HTTP的12—128 Unicode码点校验，不trim、不归一化、不截断。采用数据设计的Argon2id参数64MiB、3轮、并行度1、32字节输出，随机盐16字节；编码包含参数，验证时仅接受本版本固定成本，避免数据库损坏令认证无界分配。2026-11-12编码前选择[Go Argon2 v0.59.0](https://pkg.go.dev/golang.org/x/crypto@v0.59.0/argon2)的IDKey，许可证BSD-3-Clause；编码前已查官方文档；实施时已下载并加入依赖，本机测试创建＋验证约189ms（单次观测），不是容量承诺。本机初始同时验证上限2，不在SQLite事务中执行哈希；未知账号使用相同成本的固定伪记录路径，不区分用户名存在性。

登录先执行限速及密码验证，生成32字节随机Cookie秘密、随机CSRF和会话UUID；CSRF包装额度在业务事务之外预留。最终事务重查账号启用状态、password_hash及auth_rev未变，再写session，提交后返回Me和Cookie。Cookie属性、空闲30分钟、绝对12小时及CSRF从共同API承接。每次请求重读session、当前用户及有效期；`now >= deadline`即失效。编码前审阅采用现行主规格的失效边界：密码修改、禁用和退出撤销会话；单独Grant变化不强制重新登录，但当前请求和最终写事务重读最新Grant及auth_rev，GET me返回当前修订。session.auth_rev仅记录创建时修订，不用其与当前不同作为登录失效条件；分页游标仍按当前auth_rev绑定，旧能力不能继续授权。此修订取代草案中严格修订匹配导致Grant变化也退出登录的提议。

所有浏览器变更精确比较Origin；登录只校验Origin与JSON，其他有效会话变更同时校验CSRF。DELETE current在无有效会话时仍按HTTP返回204并清Cookie；有会话则验证CSRF。GET me返回当前同一CSRF，不能每次读取随机换token。密码修改／禁用原子递增auth_rev并撤销该用户旧会话；没有最后管理员旁路。授权是最终事务查询，不接受前端capabilities作为输入证明。

来源地址取直接连接RemoteAddr，不信未配置代理头。登录／认领失败每来源和账号／token摘要各10次每分钟；返回429和整数Retry-After，不记录口令或token原文。限速表是有界内存技术机制，拟最多10000桶并定时丢弃到期记录；容量满不能驱逐活跃被限流桶来放行攻击，使用来源／全局限制拒绝，重启清空只影响尝试预算，不恢复认证资格。昂贵哈希并发满时短等待或429，具体等待预算在H04实验固定。

system.admin仅管账号及节点元信息。node.read缺失的资源返回404；节点可见但动作不足返回403。管理员没有node.read时Node只显示HTTP规定元信息，其能力／Agent能力为空数组，执行配置与观察为null。操作结果仅投影OperationResult白名单；输出需额外权限，不把数据库Error或Agent新字段直接发送给浏览器。

## 7. HTTP请求规范化与幂等主定义

本节拟成为服务请求摘要算法的权威定义；[HTTP规格§3](http-api.md#3-条件更新幂等与分页)已于2026-11-12链接本节。此前仅描述“规范请求HMAC”，尚无具体算法，不能把本节称为已有实现。

算法版本`dm-http-request-v1`：首先按具体路由严格校验并构造类型化DTO，拒绝未知／重复JSON键、无效UTF-8、未配对Unicode转义、尾随内容、错误null和类型。方法为路由精确大写方法；路径为匹配路由加已校验小写UUID参数形成的规范路径，不含query或fragment，不通过路径清理重定向变更请求。K路由不接受无关query。If-Match解析成合法单个强ETag，其规范值为去掉HTTP双引号后的原version_id或auth_rev十进制；没有条件头的路由用null，不能与空字符串混淆。

对合法DTO统一canonical JSON：对象键按UTF-8字节序排序；数组保留输入顺序；字符串保留解码后的Unicode，不trim／归一化，以固定JSON转义规则输出；合法安全整数输出十进制，不使用浮点舍入；布尔／null按JSON字面量。普通路由的无正文与空对象统一为`{}`，有明确默认值的可选字段在DTO中补默认值，例如replace_pending缺省等于false。其余缺省、null和空集合按契约区分。Config文件Base64须严格往返且原字节、文件顺序、compose_files、env文件顺序、profiles和环境值均参与摘要；密码和秘密相关合法输入同样参与，绝不能为减少敏感信息而从摘要中省略。

最终摘要为HMAC-SHA256，以持久digest_key_id选择摘要密钥，输入为UTF-8域前缀`dm-http-request-v1`加NUL，再加canonical JSON数组`[method,path,if_match,body]`。这是请求重试身份，区别于对冻结payload原字节计算的input_digest；operation_id等服务端新ID不加入HTTP摘要。不会把无密钥明文哈希作为秘密配置标识暴露给客户端。存量记录重试须按记录的算法版本／key_id复算；本版算法固定，可由记录或schema固定版本承接，不静默切换编码。

事务流程先保护当前身份／目标可见性，再找同用户key；存在时核对方法、路径和摘要，按HTTP的result_type当前读取规则返回原安全响应和ETag，不要求再次具备写权限；不存在才检查本次写权限和If-Match。初步重放检查在密文包装之前，避免相同重报消耗额度；加密之外仍需最终事务重复检查。两并发新请求由唯一键和事务串行裁定，败方不能留下第二版本或操作。

安全响应只允许K路由规定的非秘密字段；新建认领首次响应可以含token，但数据库冻结响应中的token始终为null，重放按当前认领状态输出pending／consumed／expired／revoked及原enrollment_id／expires_at。submissions仅返回Receipt，不提供秘密回读，也不证明404之后在途事务绝不会提交。新建Stack／保存版本响应丢失只重放原stack_id／version_id，不能以当前版本拼回新ETag。

## 8. 节点登记、认领、轮换与会话接入

CreateNode在同一事务建立pending节点、recovery_nodes初始保护行、幂等回执和审计；没有ledger、会话或执行能力，不自动授予操作者权限。创建认领材料要求管理员、节点未认领及K；有效期10分钟。已有未消费有效材料时replace_pending=false返回409，true原子撤销旧材料并保存新hash，只在首次成功响应展示原token。事务失败不返回token；响应丢失用新key显式重新签发，不尝试恢复秘密。

Enroll接受现行Agent HTTP字段，拒绝带Origin的浏览器请求，以Bearer token摘要定位。未消费且未过期／撤销时，原子固定node、ledger、候选长期凭据hash及generation=1，标记consumed并记录审计；相同token／node／ledger／candidate的已消费请求只读返回原绑定，即使首次响应丢失；任一不同401且无绑定信息。节点不会因认领直接online，须等后续真实hello／welcome及恢复。

Rotate以当前凭据认证，原子建立下一代、禁用旧代、更新node代次及冻结／废弃旧会话资格，提交后关闭旧连接。响应丢失时由Agent用已持久候选连接确认；旧凭据失效后重复轮换返回401，不能通过服务器缓存旧Bearer继续换第三个秘密。Revoke原子禁用全部凭据和有效认领材料、标记revoked、冻结节点并撤销会话资格，保留操作／claims及历史。管理端不停止或删除节点现有应用，已批准许可的副作用仍须核对。

为后续传输提供正式`AuthenticateNodeCredential`、`BindAuthenticatedNodeSession`、`RecordNodeSeen`、`DisconnectNodeSession`。绑定必须在当前凭据代次和已登记ledger相符的事务内消费有效hello，生成新session，保存Agent boot、能力及ExecutionProfile并开始恢复；不得创建不存在的节点或替换ledger。旧代、旧session的迟到disconnect／心跳不能覆盖新连接。修改requireBinding时对v4增加当前凭据／撤销资格校验，防止连接尚未物理关闭便继续发许可。此模块仅定义并验证Store端口，实际WebSocket分发在后续接线实现，不能使用BindRecoverySessionFixture代替。

## 9. 配置保存、Apply与既有业务核心

Config校验直接使用共享Schema及文件包补充检查：标准Base64解码、UTF-8原文、路径、重复文件、原字节总量和引用列表符合现行预算。保存不调用Compose，不把部署不支持的语法静默删除。统一规范编码仅用于存储完整Config对象，文件content_b64解码后的字节不改换行，数组顺序不改。预生成新Stack／Version ID，完成密文后，最终事务重查会话／授权、幂等、同名及If-Match，再插入版本、推进指针／revision、回执及审计。CAS失败整体回滚，失败可以浪费包装额度但不能发布半份配置。

Apply请求仅接受HTTP的version_id，从Stack和Version得出node、project及Config，从正式当前节点会话取得ledger与ExecutionProfile。服务生成operation_id，构造唯一FrozenPayload并调用共享Schema校验和InputDigest；从不允许Web选择socket、操作用户或输入摘要。冻结后密文原文保存一次；后续发送始终读同一payload。

现有accept函数拟重构为私有`acceptTx(ctx,tx,b,candidate,payload,sealed)`，其核心负责证据、授权、节点和两项占用、操作行及prepare意图，但不Begin／Commit。旧AcceptFixture继续用自己的外围事务调用核心；v4正式`AcceptApply`在同一最终事务内重查版本归属、节点存活资格、幂等和授权，调用acceptTx，写操作关联、幂等安全回执、Stack.last_operation_id及审计，然后提交并唤醒worker。必须始终避免“先AcceptFixture成功、后幂等表写失败”的窗口。预读／加密期间连接换代、撤权、版本变化或重开都由最终事务裁定。

当前模块没有正式任务通道时Apply返回503 node_unavailable；已连接但恢复未核清返回409 recovery_required；任一拒绝均不能留下Operation、claims或prepare意图。服务测试的成功受理通过正式认领／认证／恢复端口建立前提，并检查同一事务；不通过直接SQL清frozen证明生产链已接通。

RequestCancel正式入口在同一事务读取会话及N／O／Z权限并复用cancelTx。cancel_requested不等于已取消执行；已终态重复请求按当前快照返回。结果查询复用原加密事件／证据，投影HTTP白名单；输出页仅从已保存的统一UTF-8字节流分块。last_applied_version_id仅在既有结果事务已确认normal_exit、exit_code=0、完整观察和consistent归属时推进；新增私有Stack投影函数与结果事务同提交，不能后台异步猜测成功版本。受控原结果输入不是正式真实执行证据，端到端结论仍等执行器。

## 10. 拟实现内部API与路由接线

下表为私有职责示意，不是导出函数清单；准确公开边界以§10.1为准。公共HTTP DTO采用HTTP主定义；内部私有类型不得代替这些对象。

| Store／服务入口 | 参数与返回意图 |
| --- | --- |
| `CreateService(ctx,path,managerID,keys,bootstrap)`／`OpenService(ctx,path,expected,keys)` | Bootstrap.Password由内核校验并hash，只能创建首管理员一次；返回同一operations.Store |
| `Login(ctx,username,password)`／`AuthenticateBrowser(ctx,token)`／`Logout(ctx,session)` | 返回Me及仅首次传递的Cookie秘密／已认证会话句柄；认证失败不区分账号存在性；注销幂等 |
| `CreateUser`／`UpdateUser`／`ListUsers`／`GetUser` | 传session及Mutation／合法DTO或分页；返回User或原安全响应；更新使用目标auth_rev条件，最后管理员约束同事务 |
| `CreateNode`／`ListNodes`／`GetNode`／`ListGrants`／`ReplaceGrants`／`CreateEnrollment`／`RevokeNode` | 所有管理员／节点权限和投影在Store查询或最终事务内复查；CreateEnrollment原token只通过首次秘密结果返回 |
| `EnrollNode`／`RotateNodeCredential` | 输入严格Bearer秘密及既定DTO，事务返回非秘密绑定结果；不接受浏览器会话替代Agent认证 |
| `AuthenticateNodeCredential`／`BindAuthenticatedNodeSession`／`RecordNodeSeen`／`DisconnectNodeSession` | 给后续受信传输的正式端口，校验代次／ledger／hello／session；没有外部调用者可直接设置online的API |
| `CreateStack`／`SaveVersion`／`GetStack`／`GetVersion`／`ListStacks` | session、Mutation及Config或对象ID；统一原文校验、密文和CAS，返回指定版本／安全响应 |
| `AcceptApply`／`GetOperationView`／`ListOperationViews`／`RequestCancel`／`ReadOutput`／`ReadSubmission` | 用正式身份和对象权限进入原操作权威，输出HTTP形状；不新增第二份结果仓储 |
| 私有`requireBrowserTx`／`requireCapabilitiesTx`／`replayMutationTx`／`commitMutationTx`／`acceptTx`／`cancelTx` | 仅用调用方同一Tx，不开嵌套事务、不网络发送、不在持单连接期间调用seal |

所有[HTTP路由全集](http-api.md#5-路由全集与成功响应)均须在本模块覆盖：sessions/me、users、nodes/grants/enrollment/revoke、stacks/versions/apply、operations/cancel/output及submissions；Agent HTTP的enroll／rotate同样覆盖。请求、状态码、响应必需键／null、权限和ETag完全采用主定义。不存在的操作返回404，不用虚构状态填补缺少的执行器。`/agent/v1/connect`属于后续正式传输模块，未接线不声称Agent线上可用；Web静态产物属于后续前端模块。

严格处理重复／未知query、强ETag、路径ID及错误体。普通列表默认50、1—200，keyset游标最多1024字符，内部签名内容绑定路由／筛选／用户／当前auth_rev／排序键／15分钟到期；独立用途HMAC，不借cursor绕过本页权限。只查limit+1判断next_cursor，不提供未经授权total。输出after／limit按UTF-8字节边界处理，不能把输出切块当再次执行。

### 10.1 2026-11-12编码前冻结的Go接线边界

`service_api.go`定义`Bootstrap{Username,Password string}`、不可构造有效值的`BrowserSession`、`ServiceReply{Status int; Body json.RawMessage; ETag,Location string}`、`BrowserLogin{Session *BrowserSession; Token string; Reply ServiceReply}`及`ServiceError{Status int; Code,Category,Message,RetryAction string; RetryAfter int; FieldErrors []ServiceFieldError}`。ETag由内核返回完整带双引号值；HTTP仅序列化白名单Body和头。

准确签名为`CreateService(context.Context,string,string,Keys,Bootstrap)(*Store,error)`、`OpenService(context.Context,string,Identity,Keys)(*Store,error)`；`(*Store).Login(context.Context,string,string)(BrowserLogin,error)`、`AuthenticateBrowser(context.Context,string)(*BrowserSession,error)`、`BrowserMe(context.Context,*BrowserSession)(ServiceReply,error)`、`Logout(context.Context,*BrowserSession) error`及`DoService(context.Context,*BrowserSession,ServiceRequest)(ServiceReply,error)`。`ServiceRequest`含`Method,Path,IdempotencyKey,IfMatch string; Body json.RawMessage; Limit int; Cursor,NodeID,After string`。IfMatch传原始强ETag；Limit=0使用路由默认，After空使用0。DoService只接受主规格路由，解析为内部类型化DTO并验证语义；HTTP同时负责原始JSON编码严格检查和query重复检查，不能把session中的缓存能力作为授权依据。新增字段必须同时更新此边界及HTTP测试。

Agent引导为`EnrollNode(ctx,token string,request EnrollmentRequest)(ServiceReply,error)`、`RotateNodeCredential(ctx,token string,request RotationRequest)(ServiceReply,error)`；DTO分别含`NodeID,LedgerID,CredentialSecret`和`LedgerID,CredentialSecret`。后续受信通道通过`AuthenticateNodeCredential(ctx,secret)(*AuthenticatedNode,error)`取得不透明句柄，再`BindAuthenticatedNodeSession(ctx,handle,sessionID,agentBootID string,profile json.RawMessage,capabilities []string) error`绑定。所有端口重新复核当前代次；绑定本身保留恢复冻结，不提供直接解冻接口。

CreateService先完成密码格式／哈希（无文件副作用），再独占创建数据库，使用单次DDL＋metadata＋keys＋首管理员事务提交；初始化完成标志只能与首管理员同提交。SQL失败保留文件但没有完整标志，OpenService返回ErrSchema，运维需检查失败材料后显式处理；不得在serve中恢复到空库。已有路径返回错误且不截断。OpenService先检查显式v4、完整DDL、身份密钥、完成标志，之后才改变boot和冻结节点。

## 11. 事务失败、维护与审阅关口

存储Busy／Full按HTTP固定错误映射，无数据库错误或秘密回显；页面／适配错误不能撤销已经提交的业务事实。重报优先读取原回执；缺密钥、认证失败或数据绑定损坏拒绝返回部分正文。成功响应只在相应事务提交后生成，网络发送失败不将成功改成失败，也不能换新key重做。

维护定位按请求ID → 认证主体及当前auth_rev → 目标对象归属与权限 → key／请求摘要／安全回执 → 版本密文与指针 → 原operation／claims／outbox → 当前节点凭据／绑定／恢复顺序。普通日志仅含ID、路由模板、状态及错误码；日志不得出现token摘要可逆输入、Cookie、口令、Config或原始输出。修改身份策略查H03—H06，修改保存／幂等查H07—H12，修改受理／回报接线查H13—H16；具体编号见实施草案。

编码前必须复核：Manager v3最终释放结构及v4继承点；Grant变更不注销会话、最终事务复核当前授权及修订；新增Argon2依赖版本／成本及错误映射；规范请求算法在HTTP主定义的权威链接；服务初始化失败留下文件且初始化完成标志不存在时Open拒绝；绝不补建管理员。当前跨模块已采用的方案是同Store v4、用户修订从0、acceptTx共享核心、服务模块定义canonical算法；其余仍按草案审阅结果更新，未通过审阅不能把文字当成已实施。

本模块不提供旧库迁移、自动密钥轮换、线上任务投递、真实Compose、完整Web、备份恢复或O12解除隔离工具。它们有首版依赖及退出条件，不能因管理HTTP可运行而省略。本文交付前还须依据最终代码补实际代码入口、运行版本、异常实例及维护验证；现在保留编码前草案的真实时间。

2026-11-12下一运行切片编码前衔接：真实执行的preparing、当前存活未spawn证明、只读补观察的递增result及持久发送等待，需要显式Manager v5运行格式和`CreateRuntimeService/OpenRuntimeService`，继续同一Store；v4服务入口和验收保留，不原地扩大其seq≤2范围。准确生产接线及跨端规则先按[Agent／Manager运行规格](m1-agent-runtime.md#3-主动通道及manager接线)落实，再在本篇补实际API、字段与维护映射；此段是后续实施依据，不表示v5已实现或CLI已切换。

2026-11-12实施中补充（H13—H16，先保存再编码）：服务结果观察复用同库operation_events加密权威，Stack以`observation_operation_id`组合外键引用最近含观察的结果，不再复制一份观察密文。读取在同一权限快照验证该操作事件后仅投影Observation；没有新观察保留原引用。Node整体容器观察尚无正式采集入口，本服务返回null，后续采集模块负责，不用Stack局部结果冒充整节点观察。正式传输可调用`ReceiveReady(ctx,*AuthenticatedNode,sessionID string,raw []byte) error`，它先严格验证ready报文、当代凭据及绑定，再进入与旧Fixture共用的私有recordReadyTx；实际预检证明由Agent执行器负责，当前只实现接收边界。

2026-11-12独立审阅修订（H05/H08）：发现私有DTO解码把允许字段串用于子串判断，`PATCH user`的未知键`enabled password`可误入修改口令分支。严格键成员必须使用逐键精确集合，PATCH恰好一个`enabled`或`password`；拒绝前后口令摘要、修订、会话及审计均不变。本次应急修复先保存了回归测试和集合修复，此问题记录为紧随其后的事后补充，不回写为编码前已有规则。原HTTP主定义拒绝额外字段本身未改变。

2026-11-12审阅补充、编码前修订（H07/H16）：Agent认领和凭据轮换也必须具备同事务审计。`audit_events`以互斥的`user_id`／`node_id`外键标识浏览器或节点主体，其中恰好一个非NULL；原浏览器审计保持user_id，认领／轮换用已绑定node_id和固定action。首次认领成功及每次轮换各一条，已消费token同请求重读不追加成功审计。不得把不存在的浏览器用户伪填为系统账号。

2026-11-12存储映射补充（H08/H16，编码前）：服务内核捕获modernc SQLite基础错误码，BUSY/LOCKED映射503 storage_busy、FULL映射503 storage_full；其他数据库失败保留内部错误不透出SQL。旧协调API仍保留原ErrStorage语义。


## 12. 当前源码接手索引与实现限制（2026-11-12）

- `service_api.go`定义不透明会话／凭据句柄与HTTP边界；`service_auth.go`拥有Argon2、两并发预算、会话deadline和服务创建；`store.go`仍唯一拥有SQLite及密钥生命周期。
- `service_mutations.go`把严格JSON和合法路由转成内部DTO、计算请求HMAC，先同事务检查当前身份和已发布回执，再在事务外预备哈希／密文，最终事务复查权限和条件并提交业务、回执和审计。所有字段白名单使用精确集合；未知字段不能滑入默认更新分支。
- `service_nodes.go`维护认领／轮换和认证连接句柄。消费token重试核对原node/ledger/candidate；轮换立刻清会话和冻结，旧句柄在最终事务按最新凭据拒绝。Bind不清恢复冻结；只有既有恢复协议完成回执可打开业务。
- `service_configs.go`冻结Config并绑定版本／Stack／Node的AEAD用途；Apply读取明确版本和当代profile/capabilities，经过`records.go:acceptTx`建立原操作权威、双claims和prepare。`control.go:cancelTx`及`recordReadyTx`保留原状态规则，Fixture入口在v4拒绝。
- `service_queries.go`同快照检查当前能力并形成HTTP白名单；分页HMAC绑定路由、节点、用户、当前修订和过期，SQL只读limit+1，不统计不可见行。输出按统一已保存UTF-8字节流切片。Stack观察只通过已验证的加密结果事件指针读出。
- `events.go`的成功版本／观察指针在原结果事务内更新，失败／未知不覆盖历史成功。`release.go`仅扩展显式3/4集合，没有改释放算法；v1—v3原DDL及旧入口保留。

服务内核已有受控本地测试，不等于进程／TLS／Linux安装或真实Compose已通过。登录来源预算由HTTP适配器负责；内核只负责密码计算并发。`ReceiveReady`接收的是当前认证通道提交的合法预检事实，未实现Agent实际预检器。整节点观察、线上任务worker、真实执行和旧库迁移未在此模块实现。服务进程尚未接真实通道时，节点没有当前会话，Apply必须503，不能拿本地绑定测试冒充部署成功。完整逐项验收继续由实施文档、HTTP及运行模块共同给出。

2026-11-12审阅补充（H14，修复前记录）：cancel不存在的合法operation_id必须与GET一样返回404 not_found，不将旧核心ErrNotFound裸透到HTTP变成500；未知操作不能产生审计或回执。


2026-11-12存储映射审阅补充（H16，修复前）：包装额度、acceptTx/cancelTx共享核心也必须在服务v4保留SQLite BUSY/FULL类别，不能先折叠成ErrStorage而丢失HTTP语义；v1—v3仍返回原ErrStorage以保留已验契约。

2026-11-12恢复路径审阅修订（H13/H15/H18，修复前保存）：`ProcessRecovery`处理Agent `preparing`／`ready`、Manager尚无许可及结果时，须通过私有`recoveryCancel`事务持久取消意图，不调用v4禁用的`RequestCancelFixture`。该事务重新加载当前恢复扫描，核验active_boot、当前node／ledger／session／Agent boot和原round，以及页面已收齐且仍处于可核对状态；重新读取操作，要求属于同节点账本、仍为accepted／ready、无permit、无事件／结果且未blocked，再复用`cancelTx`。快照读取以后出现的新许可、新结果或新轮次不能被原恢复决定撤销；状态已推进时拒绝旧决定且无写入，后续按当前证据继续核对。重复处理仅保留一份cancel outbox。该路径不写终态、不制造结果／absence证明、不删除claims或解除frozen；终结释放仍由原双端协议负责。

同次生产Fixture调用审计发现`ReceiveRecoveryPage`的错误回复分支调用v4禁用的`RecordRecoveryFailureFixture`。既有错误关联、当前绑定／round、请求状态核对移入私有`recordRecoveryFailure`，合法协议接收与旧Fixture分别调用该核心；Fixture继续拒绝v4。busy仍保持原请求可重试，scan_expired／scan_changed／cursor_invalid仍将当前扫描记expired并保留冻结，随后由显式新扫描恢复；不放开测试身份入口，也不修改释放语义。


## 13. v5运行Store接手补充（2026-11-12）

本节为编码后的源码说明；保留前文v4编码前/既有验收的时间。正式运行格式现在由同一operations.Store的`CreateRuntimeService`／`OpenRuntimeService`显式创建／打开schema5，账号、认领、配置、Apply及查询仍用本篇原正式接口。`CreateService`／`OpenService`继续只接受v4，无隐式升级；全部Fixture同时拒绝4/5。具体v5字段、事务和MV验收在[v5 Store规格](m1-manager-runtime-store.md)与[实施记录](../implementation/m1-manager-runtime-store.md)维护。

新增源码职责：`runtime_api.go`验证凭据归属及Schema hello，核对node/ledger后调用原认证绑定事务，生成当前Binding及关联welcome；`delivery.go`从原outbox读取固定操作意图，每次返回前保存密文请求及等待重试窗口；`runtime_events.go`扩展有界位置及认证事件链，不另建操作库或释放权威。005 DDL是独立完整新格式，001—004保持原约束。Manager启动程序切换和网络发送适配由运行模块另行接线，本节完成不代表通道已经跑通。

Delivery公开字段为MessageID、Kind、OperationID、Generation、Raw；Raw可能含秘密，写队列使用后释放，不写日志。`NextNodeDelivery(ctx,Binding,now)`一次至多一条；第一次和重试均持久登记attempts/next_attempt，等待1/2/4/8/16/30秒封顶。`RecordDeliveryFailure(ctx,Binding,messageID,code,now)`只接write_failed／connection_closed／queue_full／timeout固定码，另仅observe接受cursor_expired／observation_failed并持久停止旧context查询，复核当前session及round并持久延后重试，不能传错误正文。socket写成功不修改原outbox；ready/started/result/finalized等既有业务事务才确认。当前round纳入请求AEAD和唯一键，同session新round也取得自己的PrepareFinalize请求。许可仍按request_start原nonce直接回复。

新结果规则包括started1后not_started2，以及normal_exit观察不完整、未drift时的只读补观察result3+。补观察保留outcome、exit_code、interrupt_reason、finished_at、not_started、顶层error、原output_b64及截断标记。高seq先到只存最多32条加密pending，不推进latest、不产生finalize资格；前史到达后逐一认证并保留原body字节。缺前史或矛盾不能通过扫描最新快照绕过，pending阻止完成和释放。只有最新、真实认证、可终结的result关联finalize及最终MAC释放；迟到旧结果仅重报历史，不覆盖新投影。held normal_exit/incomplete由投递器生成绑定本操作的observe，同latest seq使用稳定query/请求，绝不再次启动Compose。

维护从operation latest/max与双claims→认证事件（含pending）→outbox业务状态→当前boot/session/round→runtime_deliveries密文与attempts→finalize_requests及finalization MAC定位。修改序号必须同时检查Agent v5、Manager读写/恢复/释放、005 DDL与MV05/06；不能只放开数字或跳过缺失的历史。模块测试使用本机临时库、正式Manager身份API和受控Agent事实，不证明真实Docker、跨机、掉电耐久或M1发布。
