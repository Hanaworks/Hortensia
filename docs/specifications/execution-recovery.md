# M1 执行、投递与恢复规格

更新日期：2026-11-04。状态：v1 工作规格，供实现和原型验证；没有已运行的 Agent、维护工具或故障实验结果。

本文从 [Manager](../components/manager.md) 和 [Agent](../components/agent.md) 的构件及运行视图继续展开，承接 R02、R04、R05、R07、R12 与 Q03、Q04、Q07、Q08、Q10。只规定 M1 `stack.apply` 正常执行、重复投递、可证明的恢复和隔离行为。报文形状以 [Agent 协议](agent-protocol.md)及其 Schema 为准；本文负责报文背后的动作、持久化顺序和证据判定，不定义另一套消息字段。

## 1. 执行上下文及责任

一次执行的稳定上下文为 `(node_id, ledger_id, operation_id, stack_id, version_id, input_digest)`。连接的 `session_id` 可以变化，执行上下文不得变化；`project_name` 和 `execution_profile` 从冻结 payload 取得。Manager 的 `state`、Agent 的 `phase` 和 Web 的显示状态可以不同，通过协议规定的映射关联，不能直接比较枚举字符串。

| 持有者 | 必须持久保存的事实 | 不得据此推断 |
| --- | --- | --- |
| Manager | 固定输入、授权许可、取消意图、Agent 事件及最终释放确认 | 已写通道不表示 Agent 收到，更不表示执行成功 |
| Agent | 收件摘要、准备结果、开始记录、受监督的进程结局、观察、资源归属基线、释放记录 | 进程不存在不表示从未执行；资源看起来正确不表示旧请求已结束 |
| Docker | 当前资源事实 | 当前快照不能复原完整操作历史或 Manager 配置原文 |

这些规则不要求给每个私有辅助对象编写数据字典。下文只补足必须跨重启保留、决定是否允许执行和解除占用的记录。

## 2. 配置预检与原生执行

### 2.1 输入限制与阶段

`config.bundle.v1` 对应 [Agent 支持矩阵](../components/agent.md#92-配置包和外部依赖支持矩阵)。本轮将“未知引用拒绝”落实为以下确定边界：Compose 主动读取文件的路径必须为可直接确定的字面量；其中含 `$`（包括 `$$`）、波浪号展开或远程 URL 时，M1 部署返回 `unsupported_file_reference`，指出文件及 YAML 路径。原文仍可保存。普通 image、environment、command 等值继续交给原生 Compose 插值，不因为这个文件边界而禁止一般环境变量。

该边界是 M1 的实现选择，不是 Compose 本身不支持路径插值。YAML anchors/aliases 的展开与变量插值、跨文件合并不是同一阶段；原生 Compose 承担最终插值与多文件合并。[Compose fragments](https://docs.docker.com/reference/compose-file/fragments/)、[插值规则](https://docs.docker.com/reference/compose-file/interpolation/)

| 阶段 | 输入与具体动作 | 可持久发布的结果／失败处理 |
| --- | --- | --- |
| A 收件 | 检查身份、Schema、尺寸、摘要、路径唯一性；事务内插入 preparing 与本地 Stack／项目名占用 | 只有事务提交后才能物化；同 ID 异输入为冲突，原记录不变 |
| B 物化 | 在本操作 staging 写原始字节；逐层检查目录和文件不为链接；同步文件和父目录；原子发布版本目录 | 目录已存在则逐文件核对原字节；差异 `workspace_drift`，不得覆盖 |
| C 无副作用结构检查 | 解析所有提交的 Compose 文件，展开标准 YAML alias／mapping merge，仅检查结构及下列文件读取位置 | 此前不调用 Compose；保留源位置，不生成替代 YAML |
| D 原生模型 | 使用固定 Docker 路径、端点、package 工作目录、有序 `-f`／`--env-file`／profiles，运行 `config --format json` | 得到仅在内存使用的规范化模型；不写普通日志；错误为准备失败 |
| E 模型及资源检查 | 检查 image、Build 限制、挂载、外部资源、项目归属；保存实际端点、CLI 版本和资源基线 | 通过后写 ready；未通过报告 `not_started`；不因预检通过而释放占用 |
| F 启动前复核 | 请求本次开始许可前完成版本字节、执行配置和目标基线复核；许可核验与开始记录提交后，再检查连接／单调期限／取消，短回调直接启动`up --detach --no-build` | 2026-11-12运行接线细化：许可最后检查之后不再做SQLite／文件扫描或Docker查询等长IO。当前存活流程确认未spawn时才报告未开始；崩溃后不得补造 |
| G 结局与观察 | 监督进程退出，记录 outcome；另行采集全部目标容器及资源基线，保存结果事件 | 按第 5 节判定；停止进程、命令退出、观察完整分别记录 |

`config` 负责规范化短写法、变量和文件合并；Agent 不手工拼出最终 Compose 模型。[config 命令](https://docs.docker.com/reference/cli/docker/compose/config/) 所选 CLI 版本必须通过本文边界用例才能报告 `compose.apply.v1` 能力，未验证的新版本或新增读文件特性不得仅凭版本号更高而开放。

### 2.2 阶段 C 的解析与引用算法

1. 只读取 `compose_files` 指定的包内原始文件；其他文件按用途单独处理。解析单个 YAML 文档，拒绝重复映射键、非字符串键、循环 alias、未知自定义 tag，以及超出节点预算的展开。M1 预算为每份文件展开后最多 100000 个节点、深度 64；超限返回 `config_complexity_exceeded`，不部分接受。标准 alias 和 `<<` mapping merge 按 YAML 规则展开；Compose 特有 `!reset`／`!override` 本版部署明确不支持。
2. 在展开后的每份文件分别检查已知文件读取入口。检查包含未启用 profile 和最终可能被后续文件覆盖的定义，避免解析前阶段先读了随后才被覆盖的文件。跨文件合并和插值仍由阶段 D 完成。
3. 以 package 根作为显式 `--project-directory` 和进程工作目录。包内引用解析后必须仍在该根内，并且对应清单中的普通文件；目录逐组件检查，禁止符号链接和外部路径。override 文件位于子目录不改变引用基准；在原生对照中验证显式项目目录与多文件的基准一致。[多文件路径规则](https://docs.docker.com/compose/how-tos/multiple-compose-files/merge/)
4. 在调用 CLI 前核对所有引用字节与清单；初始包内 `.env` 也必须显式选作 `--env-file`，否则使用包外受控空文件。不允许通过 `.env` 隐式切换 Compose 文件、Docker 端点或工作目录。插值变量映射及插值 env 文件中的赋值键，禁止 `DOCKER_*`、`COMPOSE_*`、PATH、HOME 等执行控制键；容器自己的 `services.*.env_file` 是容器环境，不按宿主执行环境处理。
5. 读文件入口清单与原生 Compose Schema／适配版本绑定。未知非 `x-` 属性先拒绝，不能默认是无副作用扩展。`x-` 内容可保留，alias 展开后进入有效服务或顶层对象的内容仍须检查。原生模型阶段再核对实际产生的路径、挂载及服务能力，不以静态扫描替代最终校验。

| 文件或能力入口 | v1 前置处理 |
| --- | --- |
| 有序插值 env 文件／默认 `.env` | 包清单中的字面量普通文件；读取赋值键检查执行控制键，值的 Compose 插值语义由原生工具处理 |
| `services.*.env_file` | 支持字符串及字符串列表；映射扩展形式本版部署拒绝；所有引用必须在包中 |
| `services.*.label_file` | 支持字面量字符串及列表，文件必须在包中；能力仅在所选 Compose 版本对照通过后开启 |
| 顶层 `configs.*.file`、`secrets.*.file` | 只读包内字面量普通文件；environment／content 来源及 external configs／secrets 本版明确不支持 |
| 顶层 include、服务 extends、credential_spec、provider、develop、pre_start／post_start／pre_stop、顶层 models | 本版部署拒绝；避免未经覆盖的文件读取、外部程序或生命周期动作 |
| `build` | 保留原文；不得实际构建。阶段 D 不使用解析镜像 digest 等额外选项；image 缺失或 `pull_policy: build` 时部署拒绝。未验证会在 config 阶段读取构建外部材料的组合拒绝 |
| 未知文件读取属性或远程引用 | `unsupported_file_reference`，不尝试调用 CLI 后再检查 |

`label_file`、`credential_spec` 和 `provider` 不能被“只检查 env_file”覆盖；其语义依据 [Compose services](https://docs.docker.com/reference/compose-file/services/)。本表只规定管理器当前接受的输入，不删除用户保存的原文。

### 2.3 阶段 E 的挂载及目标检查

挂载判定以原生规范化模型为准，预检不自行用冒号拆分短语法。`type=bind` 时，保存原始来源是否相对的事实，再与规范化绝对 source 对应：包内来源必须是已提交普通文件且只读，目录／可写挂载拒绝；包外来源必须由原文明确给出绝对宿主机路径，作为外部依赖，Agent 不读取、复制或创建其内容。为避免相对变量在展开后伪装绝对路径，挂载 source 和短语法完整挂载字符串含插值时，本版也返回 `unsupported_file_reference`。`type=volume` 交给原生命名卷语义，external 目标须已存在；`tmpfs` 按原生模型接受；其他挂载类型待适配后开放。

Docker 本身可能根据绑定选项创建缺失宿主路径；预检不宣称可以隔离该行为，也不静默替用户添加 `create_host_path:false`。对包内只读文件预先要求存在，避免被当作待创建目录。来源、读写和路径类型规则见 [Compose volumes](https://docs.docker.com/reference/compose-file/services/#volumes)。

检查配置、目标项目基线、external 网络／卷以及显式 container_name 冲突后才返回 ready。调用 `up` 前再检查一次；两次检查之间并没有跨 Docker 的原子锁。外部管理员在同一项目上并发写入时，管理器不保证识别全部竞争，发现漂移则转核对；受管理项目的常规变更应通过 Manager 发起。

## 3. 资源归属与合法替换

### 3.1 登记记录

Agent 维护节点侧 `project_bindings`，键为 `(docker_endpoint_id, project_name)`，唯一对应一个 `stack_id`。其持久字段是 `ownership_revision`（非负递增整数）、`last_operation_id`（初始可空）、`baseline_collected_at`、`baseline`（加密的资源记录集合）、`status`（provisional／active／drifted）。Manager 不复制一套独立判定逻辑；其操作证据保留 Agent 归属修订和观察关联。此记录属于 Agent 私有持久化模型，不作为 Web 的全量数据字典。

baseline 条目包含 `kind`（container／network／volume）、`resource_ref`、`logical_key`、`classification`（managed／external）、`created_at`（无法取得则空）和关联证据。容器、网络使用 Docker 实际 ID；volume 使用名称、driver 和创建信息组成观察指纹，不能把它宣称为与容器 ID 同等强度的不可复用身份。M1 不执行卷删除，卷身份不充分时只报告依赖与漂移。

`logical_key` 对容器为 `(service, container_number)`，从 Compose 规范化服务集合及实际 labels 对照；网络／卷为配置中的逻辑键。labels 用来定位候选，不单独证明管理权。无 service 关联、重复逻辑键或无法解释的资源属于待核对证据，不能自动纳入删除集合。

### 3.2 首次部署与更新算法

1. 首次部署先查询完整项目资源及同名目标。项目已有资源、默认网络／卷名称被未知对象占用时拒绝；明确声明的 external 网络／卷作为依赖排除在所有权外。空项目可创建 provisional 绑定，仍保留操作占用。
2. 更新时读取已登记 baseline，再完整查询实际资源。已登记 ID 缺失、同名不同 ID、未知额外同项目资源或关键关联变化，均报告 `resource_drift`。停止、健康变化、容器同 ID 重启属于运行状态变化，不单独改变所有权。
3. 准备完成时保存本操作的 pre-baseline 和归属修订；启动前重新比较。比较失败且证明未执行时返回未开始；不自动导入外部变化。
4. 自己受监督的 `up` 正常结束后，采集 post-baseline。既有 ID 保留；新 ID 只有在对应预期服务／资源逻辑键、属于本次已授权项目变更且没有已知外部竞争时，才登记为本次变更产生或替换的候选对象。记录旧 ID→新 ID 及 operation_id，待 finalize 与结果一起提交为新 active 基线；不能在每次普通查询时自动刷新基线掩盖外部替换。
5. 正常非零退出可能已有部分替换；完整、可解释的 post-baseline 可以登记这些事实，同时操作仍为 failed。服务被移出配置但本次未移除的旧容器保留为已有托管资源，不自动调用 remove-orphans。
6. post查询缺页或执行中断时，不发布新的active基线，ownership_status为unknown并保持占用；已知进程outcome保持原值。原绑定仍保留active／provisional，不能把采集不足当作已证实漂移。只有发现对象关联矛盾、已知外部竞争等确凿不一致才将绑定设drifted。2026-11-12运行账本实施中发现并细化此区分：正常退出但观察不完整需要允许后续只读补观察收敛，不能先写不可自动消除的drifted将其堵死。新候选资源留在证据中，不丢失也不冒认所有权；unknown／interrupted不能因普通补观察被改成normal_exit。

该模型解决常规自身重建与部署前已存在外部替换的区别，不提供对恶意伪造 labels 或同一时窗外部 Docker 写入的完整检测保证。发现外部修改后重新接管属于 O10，M1 不自动采用“当前看到的全部资源”。

## 4. Manager outbox 生命周期

### 4.1 记录与发送

outbox 只存业务发送意图及固定输入引用，不存秘密正文。每行除现有主键和 operation_id 外，保存 `message_kind`、`message_generation`、`delivery_state`、`attempt_count`、`next_attempt_at`、`last_session_id`、`last_message_id`、`last_error_code`。代次在意图语义改变时递增，普通重发不递增；例如先 prepare 后 cancel 是两类意图，不能靠更新同一 payload 把旧任务改成取消。

`delivery_state` 为 pending／waiting／confirmed／suppressed。每节点只有一个发送循环，进程崩溃后 waiting 可重新处理；不为单 Manager 引入分布式 worker 租约。磁盘记录是事实，内存队列仅作唤醒。

| 当前状态与刺激 | 同一数据库事务内的动作 | 提交后的动作 |
| --- | --- | --- |
| Apply 受理 | 建操作、占用及 prepare/pending，generation=1 | 唤醒该节点 worker |
| pending 到时 | 复核操作、身份、会话、核对状态、队列容量；更新 waiting、尝试次数和发送标识 | 向当前连接发送；事务内不等待网络 |
| 写通道成功 | 保持 waiting | 等待对应业务证据，不能标 confirmed |
| 写失败／等待证据超时 | 可重发的意图回 pending；延迟依次 1、2、4、8、16、30 秒封顶，并加入 0—20% 抖动 | 同操作及冻结输入重发；不生成新 operation_id |
| prepare 收到匹配 ready、started 或 result | 持久保存证据后 prepare→confirmed | 不再投递 prepare |
| cancel 收到匹配 result 或恢复记录内的同一持久结果 | cancel→confirmed | 根据事实处理结果，不能推断回滚 |
| 对应意图已无效 | 转 suppressed，保存原因 | 不发送；保留审计 |
| 连接改变／Manager 重启 | 本节点发送暂停，保留 pending/waiting | 先按第 5 节核对；通过后仅恢复仍有效的意图 |

默认等待 prepare 业务证据 30 秒，超时只影响投递重试，不改变业务结果。原型需以慢连接核验这些初始参数。prepare 可重发，Agent preparing 时继续原准备、暂不返回虚假的 ready，不重复建立执行任务；ready／结果按账本重报。取消一经持久请求，可以在恢复后继续发送。

### 4.2 许可和释放的特殊处理

`authorize_start` 不进入通用 outbox。许可仅回答当前 session_id、同 start_nonce 的请求；提交许可后发送失败或 Manager 崩溃，进入核对，不换 nonce 再发第二次执行许可。

`finalize` 可以作为可重发的 outbox 意图，固定绑定输入摘要、result_event_seq 与 terminal_state。收到匹配 finalized 才 confirmed。ack 表示证据已存储，不释放资源；finalize／finalized 才处理双方占用。出错不能把 finalize 降级为普通备注解锁。

## 5. 恢复判定与占用释放

### 5.1 正常重连和扫描

认证、node_id、ledger_id、输入摘要先匹配；不匹配时保留现有记录和占用，节点隔离。正常重连扫描 pending；备份恢复扫描 all，包括已 finalized 的历史。分页与 `reconcile_complete`／`reconcile_completed` 的格式和冻结规则见 [Agent 协议](agent-protocol.md)。扫描结束本身不开放任务：Manager 必须已处理所有返回记录、缺失记录及扫描期间新增事件，终态释放握手完成后才能确认核对结束。

| Manager 已有事实 | Agent 已有事实 | 恢复动作 |
| --- | --- | --- |
| 无已提交许可，操作 accepted／preparing／ready | preparing／ready，或 scope=all 完整可信扫描证明无记录 | M1 重连不继续旧任务自动开始；Manager 持久取消，存在 Agent 记录时发送 cancel 并取得未开始结果；全记录扫描证明无记录时记录管理端未授权取消及其核对依据 |
| authorized／running／reconciliation_required | preparing／ready 或无记录 | 已发许可可能有迟到／丢失历史，不能仅凭当前无记录解除占用；Agent 有完整账本并能持久证明未开始时报告 not_started，否则隔离 |
| 任意非终态 | START_COMMITTED 或 reconciliation_required，没有可靠进程结局 | 保持未知及占用，不重跑、不生成新许可；需要第 6 节维护处理 |
| 任意兼容状态 | outcome=not_started 的持久 result | 按原因 failed／canceled；保存该结果后进入释放握手 |
| 任意兼容状态 | outcome=normal_exit、observation.status=complete、ownership_status=consistent | exit_code=0→succeeded，其他→failed；保存后进入释放握手 |
| 任意兼容状态 | outcome=normal_exit、观察不完整，且没有已确认归属漂移 | 只补观察并重新判定归属，仍保留占用；不重执行 Compose |
| 任意状态 | ownership_status=drifted，或观察已完整但 ownership_status=unknown | 保持 reconciliation_required，不发 finalize；只读采集完整不能代替归属证据 |
| 任意状态 | outcome=interrupted／unknown，或相同 event_seq 内容不同 | 保持 reconciliation_required；错误或正常退出码本身均不足以解除 |
| 已存终态、释放 pending | Agent 仍有对应 result 或已 finalized | 重发同一 finalize；不得重新 prepare |
| 已存终态、释放 released | Agent 与之完全匹配的 finalized | 无副作用完成对账；结果、输入或释放位置不同则重新隔离 |
| 本库没有 operation | Agent 返回历史记录 | 建 recovery_case，保留原目标和证据；不伪造受理者、版本正文或受理时间；该节点不能仅因扫描结束而解除隔离 |

“无记录”必须来自当前 scope=all 完整可信扫描，不等于 pending 集合中缺席，也不等于账本丢失后的空库。普通重连 pending 扫描中缺失某个待核对任务时，以 scan_id=null、cursor=null、scope=all 发起新扫描，原子替代旧 pending 扫描后再判断；新扫描使用新 scan_id，冻结连续保持，旧游标和旧 reconcile_complete 失效，不能在原 scan_id 内改变 scope。新扫描重新固定全量身份及记录，并保留尚未在当前连接确认的变化事件，直至按协议取得 ack。Manager 自己备份缺失许可、Agent 自己备份缺失执行记录时都必须按隔离恢复处理，不能套用正常重连的未授权取消路径。

`ownership_status` 随 result 的同一 event_seq 持久保存：not_checked 表示未启动而无需归属收敛检查；consistent 表示 pre/post 基线按第 3 节可解释；drifted 表示已经发现对象替换、未知资源或关联冲突；unknown 表示证据不足。Manager 必须检查该字段，不可先仅凭退出码与 complete 写 succeeded，再依赖 Agent 拒绝 finalize 来补救。正常退出的 not_checked 结果违反协议，不能作终结证据。

取消必须先在 Manager 事务中阻止新的许可并停投旧 prepare，再发送 cancel。Agent 已有准备记录时，将取消与开始放在同一串行执行区：未提交开始时持久化 not_started/canceled_before_start 结果；已提交开始时只按实际执行事实判定，无法证明未启动则保持中断／未知。Agent 无该 operation_id 时返回 operation_not_found，不制造缺少目标、版本的执行记录；Manager 只能在 scope=all 完整有效扫描冻结新 prepare 并证明无记录、且自己从未提交许可时走管理端单独释放例外。已有许可不适用该例外，取消响应本身也不是 finalized。

### 5.2 已知正常退出后的补观察

Manager 发送带原 operation_id 的只读 observe，并绑定该任务的 Stack。Agent 先识别已有 query_id 的重放或分页请求，按相同查询条件返回既有快照；只有新 query_id 的首请求才要求 cursor=null，并检查原记录 outcome=normal_exit、退出码已持久化、观察尚未完整、输入匹配且 ownership_status 不是 drifted，否则拒绝作为自动恢复使用。unknown 只有源于不完整采集而尚未判定归属时，才允许通过补齐观察再与原 pre-baseline 比较；已确认的漂移不因再拍一张快照消失。

Agent 使用原操作目标重新采集，保留原 outcome、exit_code、finished_at 和执行输出，更新带新 collected_at 的观察及据此重新计算的 ownership_status。新结果以新的 event_seq 在事务中保存后发送 result；没有新进程、没有新 Apply。Manager 保留旧失败观察证据，仅在 complete 且 consistent 时收敛。若发现外部变化导致归属不能成立，仍保持核对，不以“查询终于成功”掩盖漂移。

同一操作同时最多一个补观察采集；重复 query_id 返回同一采集结果，后续页按原查询条件及 next_cursor 读取固定快照，不再次采集或新增 result event_seq。首次补观察已经使原结果完整，也不阻止同一未过期查询的重放或翻页；缓存过期按协议返回 cursor_expired，不把旧查询当作新采集。新的 query_id 才按首请求前提发起新的采集。读查询失败只返回 unavailable 并继续占用。普通资源查询的 operation_id 为空，不能被拿来替代某个任务的恢复证据。

### 5.3 双端释放握手

1. Manager 校验并保存 result；按前述判据写终态和 `release_status=pending`，仍持有 claims。同一事务建立 finalize 意图，绑定 result_event_seq、输入和 terminal_state。ack 在证据提交后发送。
2. Agent 收到 finalize 时，要求匹配自己当前可终结结果，且不存在更晚的矛盾证据。normal_exit 结果必须同时满足 complete 和 ownership_status=consistent；not_started 按未启动判据检查。在单事务写入最终判定、释放 local_claims、保存 finalized 回执；若更新归属基线，也与此结果关联保存。事务失败不回复成功。
3. Manager 收到 finalized，核对其输入和结果位置，在单事务释放自己的 resource_claims、将 release_status 改为 released、完成 outbox。随后才允许新冲突操作。
4. Agent 回执丢失时，Manager 保留占用并重发 finalize；Agent 返回原 finalized，不再次执行。Manager 提交 released 后崩溃，重启按已持久结果恢复。Agent 虽已释放本地占用，但 Manager 尚未确认时仍不产生下一任务，因唯一授权入口在 Manager。
5. scope=all 完整扫描证明无 Agent 记录且 Manager 能可靠证明从未提交许可的取消，可只在 Manager 释放；仍记录本次可信对账依据。已发 prepare 但未取得可信全记录扫描时不能走此例外，pending 缺席不满足条件。

已完成操作以后再次发现证据冲突，旧业务结果保留，同时创建 recovery_case 并重新隔离目标；不能通过覆盖历史终态隐藏冲突。

## 6. 人工恢复的边界与维护要求

M1 网络协议没有无条件 resolve／force-unlock。对 START_COMMITTED 后丢失执行事实、账本回退、未知外部替换等情形，自动恢复明确停止在 reconciliation_required。

维护人员应先冻结该 Docker 端点的所有变更入口，保留 Manager、Agent 账本和工作区副本，停止旧 Agent 执行服务并确认其受监督进程已经退出，再核查实际 Docker 端点。若无法排除旧 Docker 请求仍在处理，必须在独立安排的维护窗口建立能够终止旧请求的端点屏障，并核对其影响；这可能涉及 Docker 服务或宿主机维护，不能由管理器悄悄执行，也不能把进程 PID 消失当作屏障证明。

维护记录至少关联 node_id、ledger_id、operation_id、input_digest、端点、相关服务停止事实、屏障依据、资源完整观察、归属差异、操作者和时间。管理员声明是人工证据，不能伪装成 Agent 自动测得的正常退出码。无法建立充分依据时继续隔离。

本轮不交付可解除这类隔离的人工恢复工具，也不把一段说明等同于已经具备该工具。维护负责人对人工证据与窗口负责，Manager／Agent 实现者负责提供不能绕过身份、输入及占用核对的入口。其安全入口、证据验证和双端更新必须在可交付安装包前完成设计与恢复原型验证，才能开放；在此之前，M1 对真正 unknown 的支持是保存证据和隔离，不承诺恢复后继续修改该目标。此限制不妨碍正常部署、可靠结果重报或已知正常退出后的补观察，但阻止将“所有故障都能自动恢复”作为 M1 验收结论。

## 7. 实现断言与参考使用

| 场景 | 必须观察到的断言 |
| --- | --- |
| anchor 隐藏包外 env_file、变量路径、label_file 缺失 | 首次 CLI 调用计数为 0；错误指向源文件和字段 |
| 两个 Compose 文件覆盖同一服务的 env／挂载 | 原始字节不变；原生模型负责合并；所有前置引用均已检查 |
| 更新正常重建容器 | pre/post ID 不同，但操作、服务键和归属修订相连；下一次可部署 |
| 外部同名替换后 Apply | 预检 resource_drift，CLI up 调用计数为 0；不刷新基线掩盖变化 |
| 正常退出、观察完整但归属漂移 | result.ownership_status=drifted；Manager 保持核对，不先标 succeeded，也不发 finalize |
| outbox 写通道成功后断线 | 仍未 confirmed；相同操作恢复，不产生第二次底层执行 |
| 正常退出后观察失败 | 同 operation_id 补观察、新 event_seq；up 调用总数不增加 |
| finalize／finalized 任一丢失或提交边界崩溃 | Agent 和 Manager 通过重报收敛；在 Manager released 前不接受冲突操作 |
| START_COMMITTED 后丢失结果 | 状态保持未知；完整当前资源快照不能直接触发 finalize |

正文组织采用 [arc42 构件视图](https://docs.arc42.org/section-5/)和[运行视图](https://docs.arc42.org/section-6/)，将职责落在既有构件上，只展开关键场景。工程表达对照了 Helichrysum [技术设计第 4、7、8 节](D:/Project/Helichrysum/docs/technical-design.md)和[切片 2 计划](D:/Project/Helichrysum/docs/slice-2-plan.md)：借鉴输入、处理、输出、依赖和可观察断言的衔接，没有采用其平台实现、参数或测试完成标记。Docker 原生行为来源在相应规则旁引用；v1 接受范围、投递参数与恢复限制均为本项目工作规格，待原型验证。
