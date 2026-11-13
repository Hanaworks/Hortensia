# M1 受控维护、一致备份与隔离恢复规格

日期：2026-11-12。状态：**实施前基线，尚未实现、未在真实节点验证**。本文承接首版工作包G及[V06／V07](../implementation/m1-first-version.md#3-首版v01v08退出条件)、[O11／O12](../decisions.md#3-未决事项和解决时点)、[执行与恢复§6](execution-recovery.md#6-人工恢复的边界与维护要求)与[运行设计§8—9](../operations.md#8-数据库与密钥备份组)。稳定验收及实施顺序见[MB记录](../implementation/m1-maintenance-backup.md)。现有`manager`／`agent`没有本文新增的维护、备份、转换命令；下列入口是待实现接口，不能直接作为运行手册。

用户于2026-11-12选择首版提供操作者明确触发的未知执行维护，接受必要时停止节点Docker、影响容器。具体测试方式和节点环境稍后提供；本轮只设计及本地受控验证，**不得连接、配置或停止任何现有节点**。本文的版本、支持矩阵、命令分工和事务是技术选择，不表述成用户逐项确认。

## 1. 目的、方案和交付边界

交付两个有明确边界的小程序：

- `maintenance`：让真正unknown／interrupted在旧执行入口已实证终止后，以独立的人工维护事实结束占用；原执行结局继续是未知／中断，不能变成not_started、normal_exit或succeeded。
- `backup/restore`：取得SQLite一致副本、匹配身份和实际密钥，恢复到新的隔离部署代次；完整保留去重及执行事实，经当前凭据／授权复核和真实Agent全量核对后逐节点开放。

选择本机CLI及受控文件交接作为首版维护入口。它复用私有文件权限、部署锁和双端存储，不新增Web force-unlock，也不要求尚不存在的在线密钥环。在线维护RPC能减少文件搬运，但需要新增高权限API、认证和重放表；作为后续便利功能。只停Compose或只拍资源快照不能证明Docker旧请求已结束，不能作为替代方案。

首版维护仅支持经精确版本验证的Linux、本地rootful单Engine、systemd、统一cgroup v2、runc、本地可靠文件系统和内置存储／网络。备份工具可在Windows以真实SQLite及密钥验证；Linux端点屏障不能由Windows替身通过。管理备份不包含应用卷、bind数据、镜像层或Docker数据目录，维护屏障也不证明应用事务成功、回滚或掉电耐久。

## 2. 现有实现事实及必须先解决的接线

检查基于2026-11-12工作区，含未提交v5源码；不能用HEAD代表这些改动已整体交付。

| 权威代码／数据 | 当前实际边界 | 本模块须接入的变化 |
| --- | --- | --- |
| Agent `ledger/migrations/005_runtime.sql`、`runtime_store.go` | 完整v5；execution／events／runtime_documents／project_bindings及local_claims；身份不可替换；Open冻结新boot | 显式v6，增加维护和恢复事实；不能往严格v5库偷偷加表 |
| Manager `operations/migrations/005_runtime.sql`、`store.go` | 完整v5；账号、凭据、配置、操作、恢复和resource_claims；精确DDL验证 | 显式v6及离线v5→v6完整转换；修改受理、许可、恢复、释放的共同门禁 |
| Agent `journal_crypto.go` | 单`event_keys` KEK，最多1,048,576次；K_ledger作指纹；body及DEK两层AAD均含key_id | 旧备份不能继续用回退计数封装；新KEK必须逐记录重加密，不能只换wrapped_dek |
| Manager `crypto.go` | 单`keys`行，HMAC和单KEK；相同额度，body及wrap AAD也含key_id | 保留HMAC及digest_key_id；全量重加密并更新单KEK元数据 |
| Agent `release.go`、`runtime_ownership.go` | Finalize只接受可信not_started或normal_exit+完整consistent；active基线按candidate+finalized连续头认证 | 新增独立维护发布证明，验证正常发布及人工发布共同形成无分叉修订头；不得绕过原finalized认证 |
| Manager `release.go`、`records.go` | released由finalization或无许可all缺席证据认证；当前CHECK不允许未知结果released | 新增release_basis=maintenance分支及独立证明；原normal/absence分支保持原谓词 |
| `contracts/agent-v1.schema.json` RecoveryRecord、Agent `recovery.go`、Manager `recovery_evidence.go` | 非finalized必须claims_held=true；finalized必须有可正常终结的结果；v1不能诚实表示人工释放的unknown | 必须显式版本化恢复记录，见§6；不能伪造finalized或省略历史 |
| Manager `service/identity.go`、Agent `service/identity.go` | 单份identity及固定密钥文件，服务持有`.manager.lock`／`.agent.lock` | 新增受控部署代次发布；旧布局只可作转换源，不能宣称现有identity已支持代次或keyring |

既有共享对象、输入digest、事件序号、资源归属及正常双端释放定义继续链接[运行账本](m1-agent-runtime.md)、[Manager v5](m1-manager-runtime-store.md)、[共享Schema](../../contracts/agent-v1.schema.json)和[执行与恢复§3—5](execution-recovery.md#3-资源归属与合法替换)。本模块只增加独立维护和恢复对象，不重新定义ExecutionResult。

## 3. 程序组成、输入输出和入口

实现位置建议：新增`internal/maintenance`（case／文件链及屏障编排）、`internal/backup`（快照／清单／部署发布）、两端Store各自新增`maintenance.go`和`restore.go`（所属表认证及事务）。Linux副作用实现单列`barrier_linux.go`，Windows只提供可注入替身；禁止在跨平台实现内拼shell脚本。CLI在现有`cmd/manager`和`cmd/agent`扩展，调用绝对程序及参数数组。

新增接口如下；均为**规划名称**，实施后再生成help和已执行示例：

| 入口 | 输入／输出与副作用 |
| --- | --- |
| `manager maintenance plan --config … --request … --out …` | Manager服务已停且独占部署锁；request列endpoint、全部未知操作、维护原因、负责人和环境profile。冻结节点、停投prepare/authorize，保存case，输出脱敏plan及独立0600交接secret；不操作Docker |
| `agent maintenance inspect --config … --plan … --secret-file … --out …` | Agent服务已停；验证同node/ledger/endpoint及输入、完整执行历史和所有受影响claims，输出实际停止范围、拒绝条件、资源影响及plan摘要；只读，不自动执行维护 |
| `agent maintenance fence --config … --plan … --secret-file … --approve-plan-digest … --out …` | 明确摘要批准后持久进入维护，再执行受支持的端点屏障；取得屏障及新daemon完整观察，输出证据。只停止／重启列出的服务和任务，不删除Docker资源 |
| `agent maintenance resolve --config … --case … --decision … --out …` | decision逐资源列保留的管理对象、external依赖、未知对象及原因。校验证据未变；事务写独立resolution／基线／本地释放，输出AgentPrepared回执；端点仍维护冻结 |
| `manager maintenance accept-agent --config … --case … --receipt … --out …` | 校验本case及全部操作/claims、Agent回执，事务保存resolution与ManagerCommitted、释放对应claims；输出提交证书。仍冻结，不允许部署 |
| `agent maintenance accept-manager --config … --certificate … --out …` | 校验同一prepared及Manager提交证书，保存AgentAccepted，输出确认；仍禁止普通执行，只允许恢复通道 |
| `manager maintenance activate --config … --accepted …` | 保存双端已接受状态，允许此节点以新session进入全量恢复；不是直接解除冻结；当前all/completed通过后才开放 |
| `manager/agent backup --config … --data-out … --secrets-out …` | 原生一致快照＋匹配材料；在线可读，不调用Store.Open改boot；不向普通HTTP导出秘密 |
| `manager/agent backup verify --manifest … --secrets …` | 只读校验清单／身份／完整DDL／全部密文及交叉引用，输出对象计数和受控错误；不会启动服务 |
| `manager/agent restore stage --manifest … --secrets … --target …` | 空的私有目标代次，源只读；验证、离线转换／重加密，生成恢复报告及新恢复备份组；不覆盖当前环境，不连接任何节点 |
| `manager/agent restore publish --staged … --expected-current …` | 服务已停且独占部署锁；按§8原子发布同代次DB、身份、密钥，启动仍是隔离模式 |

每个变更入口同时检查OS私有目录权限、无链接／重解析点、case身份、预期阶段和部署代次。停止动作只能由`fence`执行；`inspect`不因为获得root权限就自动停止。维护operator是可信本机管理员身份，记录有效UID及其提供的责任人／工单；不得伪装成原Apply受理者。交接secret应由操作者通过已有可信文件途径搬运，本程序不自行SSH或上传。该secret只认证文件链及误交接，**不声称抵御持root和secret的恶意操作者伪造证据**。

交接secret丢失时两端保持当前冻结及阶段；若某一端仍有可认证的case私有副本，只能在本机独占锁下导出同一secret并重放原文件链，不生成新secret冒充原回执。双方均无可认证secret副本时不得导入无MAC文件、改case_id、删gate或删除claims；保留原case及原材料，作为无法完成的恢复事件继续隔离。首版不提供绕过secret的解锁入口。

## 4. 维护case、持久对象和事务

### 4.1 重要对象

全部ID沿既有UUID规范，摘要是SHA-256字节的base64url；时间用UTC，时限同时用本次boot的单调时间。敏感观察、进程参数、完整回执正文用所属Store的加密层，不写普通日志。

| 对象／字段 | 含义、来源、约束及生命周期 |
| --- | --- |
| MaintenancePlan | `format=dm-maintenance-v1`、`case_id`、`manager_id`、`node_id`、`ledger_id`、`endpoint_id`、`engine_id`、两端部署generation、profile摘要、原因／operator、`created_at`、`expires_at`、`operations[]`、`plan_digest`；operations固定全部受影响operation/input_digest/last_event_seq/result_seq/许可及claims摘要。批准后不能增删目标；阶段开始有效期30分钟，超时只能重做只读计划，不能解除已有冻结 |
| FileEnvelope | `case_id`、`step`、`plan_digest`、`previous_receipt_digest`、`body_digest`、正文及HMAC；不同step用域`dm-maintenance-file-v1/<step>`。交接secret为随机32字节，Manager密文保存；Agent首次接收后保存在case私有材料中。原字节入库；同case同step不同摘要即冲突，冻结并保留两份证据 |
| EndpointFence | `fence_id`、boot_id、machine-id指纹、socket规范路径及inode观察、Engine ID、精确二进制hash/版本、unit配置hash、前后daemon启动身份、控制组覆盖清单、socket及进程关闭事实、残留检查、开始／完成时间、重新启动和观察证据；证明旧动作不能继续，不证明原操作成败 |
| ManualResolution | 每operation唯一，`resolution_id`、case/fence、原input/seq/结果原文摘要、`disposition=execution_unknown_closed`、责任人与时间、完整资源观察摘要、旧归属修订和`accepted_baseline_revision`、逐资源判定及理由、AgentPrepared摘要、ManagerCommitted摘要；结果永远单独引用，不修改原事件。首版不自动清理此历史 |
| `maintenance_cases`（两端v6） | case_id主键；endpoint索引、计划摘要、阶段、不可变body密文、secret密文／私有引用、前序／本序回执摘要、创建与更新时间。每endpoint最多一个未完成case；更新阶段采用CAS，旧阶段重放返回原回执 |
| `maintenance_resolutions`（两端v6） | operation_id唯一FK原execution/operation（Manager缺失原操作时关联recovery_case，不伪造受理记录）；case、输入digest、seq、resolution摘要及正文密文；Agent存本地Prepared／Accepted，Manager存对应Committed／Accepted证据 |
| `maintenance_endpoint_gate`（两端v6） | endpoint_id唯一、case_id、阶段、fence摘要、active/deferred；在普通受理、许可、Gate、投递、恢复完成共同检查。不能仅凭nodes.frozen或内存布尔值控制 |
| `restore_runs`（两端v6） | restore_id、source backup_id与清单摘要、目标generation、新KEK ID、阶段、源／目标行数及语义摘要、当前授权复核摘要、各节点all round及结果；保留恢复来源与未决项，不删旧记录 |

case阶段固定为`planned → fencing → fenced → agent_prepared → manager_committed → agent_accepted → activated → completed`；两端只保存自己已经验证的前缀。失败另存`last_error`及`failed_step`，不倒退阶段或自动解冻；尚未进入fencing的旧计划才可标`superseded`。不同机器同一阶段存在传播延迟，真实已提交回执决定事实。gate在completed前只开放本case导入／导出与激活后的认证恢复读取；最终completed在当前all完成事务中形成，不由离线文件单独触发。

首版正常维护要求原身份、冻结输入、事件原文、claims和旧基线均能认证；坏cipher、缺钥、DDL损坏或无法确定属于哪个原任务的事实，不能由人工决策覆盖。先找回可认证原材料或保持隔离。既有全局quarantined不能无条件清零：v6把隔离原因及所影响operation/case持久关联，只有本case完整覆盖并持久解决的原因可标resolved；其他原因继续阻塞。恢复读取允许读取本case证据，不等于普通Gate可以进入；两端requireBinding／requireReleaseHealthy／scan完成均须接入此区分。

v6只增加明确DDL并调整Manager释放CHECK：`release_basis`为normal／absence／maintenance，默认normal；maintenance released仍保持`state=reconciliation_required`，通过独立resolution读取显示“原执行未知，维护已结束占用”。它不更新Stack最近成功版本。`blocked`保留执行证据存在冲突的含义，不能简单置零消除历史；新受理按当前case及已认证释放判定。缺回执、错输入、较新矛盾事件时released无效且重新隔离。

### 4.2 两端更新次序

1. Manager plan事务固定所有受影响claims和操作证据，写endpoint gate，抑制旧prepare/cancel/observe/finalize投递，撤销旧执行会话；服务停机和DB冻结共同保证没有在内存中发出的新许可。已经发出许可的历史仍保留。
2. Agent fence在任何停止动作前先保存case+gate及原材料备份；关闭Gate、禁止新session启动，停止旧Agent／Compose控制组。端点终止屏障不成功则停在fencing；不得进入resolve。
3. Agent resolve在同一事务核对固定输入、最新事件位置、完整屏障、观察、旧基线头及claims；写独立resolution和人工接受的基线修订，删除**仅该operation**的local_claims，保存AgentPrepared原回执。释放由该有证明的事务承担，不能提供独立删除claims入口。
4. Manager accept-agent在同一事务验证本地plan、全部操作或明确recovery_case、AgentPrepared及原事件链，写ManagerCommitted、resolution及release_basis=maintenance，删除相应resource_claims。没有Agent持久Prepared绝不释放。case仍阻塞此endpoint普通任务。
5. Agent accept-manager持久验证并存ManagerCommitted，生成AgentAccepted；Manager activate持久验证AgentAccepted。缺任一回执，都只能继续原case，不可开新case或换身份绕过。Agent单端Prepared以及Manager单端Committed都不意味着端点已开放。
6. 双端可在维护门禁下启动正式服务，只接受当前认证新session、all扫描及证据消息；当前all包括原未知历史和resolution投影。Manager校验没有新增矛盾、没有漏case、当前凭据已复核；原`reconcile_complete/completed`仍是最后屏障。通过后在本地事务把gate标为历史完成，才能开放新许可。通信失败／重启重新all，不因持有离线activate文件跳过。

文件交接可任意重复，不能跨step、case、ledger、部署generation、operation或input_digest重放。回执文件写入失败不撤销已经提交的数据库事实：同入口可从库中重新导出相同回执。事务提交结果不确定时先查询原case，不重新执行Docker屏障或创建第二个resolution。

只有Manager备份缺失、Agent真实all提供的旧任务保存在recovery_case时，也可按同case维护；不能补造版本正文／发起者／历史许可。此分支release的是恢复目标占用与case阻塞，独立resolution保留原Agent执行事实；原业务operation不存在仍保持不存在。

## 5. Linux端点终止屏障

### 5.1 官方资料支持的事实及边界

Docker默认关闭daemon会关闭容器，但live-restore使容器继续运行，Swarm也有独立行为。因此`systemctl stop docker`不是通用执行终止证明。[Docker live restore](https://docs.docker.com/engine/daemon/live-restore/)

containerd的shim拥有运行任务，容器生命周期独立于containerd进程；停止dockerd和containerd、或看不到原Compose PID，都不能单独证明旧运行时动作已终止。[containerd Runtime v2](https://containerd.io/docs/main/runtime-v2/)、[containerd生命周期设计](https://github.com/containerd/containerd/blob/main/docs/historical/design/lifecycle.md)

systemd的`KillMode=control-group`覆盖该unit控制组，`SendSIGKILL`控制宽限后终止；它不保证容器／shim在同一unit中。cgroup v2的`cgroup.events populated=0`反映该子树没有存活进程，不能把一个unit的空组等同全端点空组。[systemd.kill](https://freedesktop.org/software/systemd/man/latest/systemd.kill.html)、[Linux cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)

Docker插件可在其他进程甚至远程主机执行；local卷也可配置远程挂载。首版屏障不覆盖外部执行者，故必须拒绝这些组合，不能靠声明“停止Docker会停一切”推导。[Docker Plugin API](https://docs.docker.com/engine/extend/plugin_api/)、[Docker卷](https://docs.docker.com/engine/storage/volumes/)

以下是据此作出的**工程推导和待实测支持profile**，不是官方给本项目的全局屏障保证。版本profile未建立前，真实`fence`必须拒绝，不能预置`verified=true`。

### 5.2 首版最小支持矩阵

profile必须固定：内核／systemd、Docker／containerd／runc／Compose精确版本和二进制hash；Docker root、exec root、socket、containerd地址／namespace、systemd unit和drop-in、cgroup树映射、存储驱动与插件。只允许一个明确的rootful本地端点；rootless、Desktop、远程TCP／SSH、Swarm active、第三方runtime／hook、独立网络／卷／授权插件、legacy插件发现文件、非本地volume options、远程FS／FUSE、共享containerd namespace或无法归属的shim均拒绝。

操作者须控制同一端点所有变更入口：原Agent、外部Compose／CLI、容器挂载Docker或containerd socket、其他控制服务和用户会话。屏障profile记录这些入口如何停止与保持阻断；发现未知监听、现存控制连接、可写socket暴露或不在控制范围的执行者则拒绝。本工具不承诺抵御维护窗口内持root的外部管理员同时修改。

`live-restore=true`即拒绝首版自动屏障；不现场改成false冒充原状态已安全。即使false，仍须检查全部运行时任务和控制组，不把配置当证据。支持profile须包含正常容器、restart policy、健康检查、容器create/start/pull和删除中断各类实测；旧请求可能恢复的持久工作队列必须按该版本源码和实验确认已排空或终止。无法确认某条后台路径的版本不列入支持矩阵。这里只承诺经验证的Engine操作组合，不承诺所有Docker扩展。

### 5.3 屏障算法与证据

1. fence前用固定只读检查采集全部Engine对象、配置和进程／控制组映射，包括停止容器、moby任务、shim、运行时helper、daemon及socket activation，不只检查未知Stack。范围／配置不支持则零停止动作退出，case保持planned/frozen。
2. 保存Manager与Agent材料及工作区清单；持续持有本机维护锁。持久记录将阻断的unit／socket、原enable/mask配置及恢复方案。临时mask和停止必须先阻断socket activation及其他已知重启入口，再停止原Agent／Compose、dockerd及所属containerd。容器停止／TERM宽限及KILL只对计划覆盖的控制组执行，禁止按名字全机pkill。
3. 先按unit正常停止；Agent／Compose宽限10秒，daemon／runtime总停止期限120秒。超时可对计划内控制组升级SIGKILL，但**必须等其真正空组**；D状态、挂起I/O、未知shim或控制连接残留均fence_failed。没有因“已经等够120秒”而自动通过的路径。
4. 验证units inactive、socket无listener且不能自动激活，原Agent／Compose／daemon／containerd／shim及全部容器runtime/helper的覆盖子树`populated=0`，并检查整机进程视图没有该profile之外同端点执行者。记录boot+进程starttime+cgroup，不按可复用PID判断；两次只读核对间隔1秒用于发现意外重启，间隔本身不构成证明。不存在的已回收控制组须由systemd生命周期和上次映射解释。
5. 有界检查该版本已知持久后台工作路径；停止失败或无法确定旧请求是否能被新daemon恢复，保持停止／隔离并报告拒绝原因。禁止为了让检查通过删除Docker元数据、容器或队列。
6. 仅在空组及外部入口持续阻断后按原配置启动新的containerd/dockerd，保持Agent普通执行禁用。记录新启动身份、相同Engine ID和数据根；读取全部目标容器／网络／卷及关联。daemon恢复过程中不将资源数暂时为零视为空项目；等待Engine就绪后按完整观察算法采集，失败则保持case。restart policy可重新启动工作负载，属于新daemon按已有策略恢复，记录这一副作用；它不能继续旧Compose或旧API控制进程，且不得含可重新写Docker的容器。
7. 观察有效期5分钟，期间端点写入口保持冻结。新事件、profile/Engine/boot变化、外部修改迹象或到期都使观察无效，重新采集并复核；不重新把已失效屏障当成当前事实。屏障证明对应本case、boot及受控启动代次，不能拷给另一个节点。

当前未掌握用户节点的实际unit、cgroup布局和Engine版本，**可发布支持矩阵为空**。实现可先完成Linux采集器、拒绝分支和受控故障测试；MB06真实证据到位前，不开放真实解锁。这一限制必须出现在help和运行状态，不能仅藏在测试记录里。

新daemon的自动启动副作用须在inspect中逐容器列出，而不是承诺全部容器恢复运行或保持停止；restart policy按原配置执行，不为让维护通过静默改策略。[Docker restart policy](https://docs.docker.com/engine/containers/start-containers-automatically/)

### 5.4 资源接受与结果区别

屏障成功只证明旧动作不会继续；之后的完整观察用于确定接下来管理哪一组资源。decision必须逐项列出原可信pre基线、当前真实ID、逻辑服务／容器序号、旧→新映射、external排除和不明确对象。labels只定位候选，不能单独授予管理权。

操作者可以明确接受本次未知操作留下的当前候选作为新的**人工接受基线**，同时承担其归属判断；这不是把观察推算成原执行成功，也不是M2通用导入任意已有项目。接受范围只能是原operation固定项目和预期对象；不能把其他Stack或未知额外同名资源自动纳入。解释不清、资源被外部替换无法分离或无完整观察时保持隔离；清理、删除／回滚资源不包含在该命令内。

新基线来源是resolution_id+fence+人工决策，与正常candidate/finalized来源并列认证。Agent必须验证同一项目修订链连续、无分叉、无回退；Manager保留相同摘要。普通观察不能发布基线，原未知操作不更新最近成功版本，Web必须能同时显示原unknown及维护已释放的事实。

## 6. 恢复记录与兼容性增量

v1无法表达人工释放后的unknown，不允许在v1中谎报finalized。**M1的维护版本使用显式`protocol=2`、`agent-v2.schema.json`与两端schema6**；20类消息的目的、Finalize正文及其normal/not_started判据不变，仅恢复历史增加独立维护投影。2026-11-12设计审阅后由本轮实施负责人将此技术选择收束，不记作用户逐项确认。实施前须同步协议主定义、Validator和精确兼容矩阵；本文件不直接修改现有通道。

兼容矩阵固定为v5运行库只用protocol1，v6运行库只用protocol2；v6没有人工历史也不回落v1。不在serve/Open中隐式迁移，v5只能先停服务、完整离线转换为v6及格式2部署manifest，再启动维护版本。旧v1—v4受控夹具继续保留原格式和原协议边界，不据此宣称它们获得v6维护能力。

RecoveryRecord新增必填可空`maintenance_resolution`；旧历史值null，正常finalized仍按原判据。人工历史为`phase=maintenance_resolved`、`claims_held=false`、`final_state=null`、`finalized_result_event_seq=null`；latest_result和原seq原字节保留（START_COMMITTED无result仍为null），maintenance_resolution为`{case_id,resolution_id,resolution_digest,agent_prepared_digest,manager_commit_digest,agent_accepted_digest}`。此对象只引用已经双方持久保存的离线链；不能仅凭对端发来的几个摘要接受释放。

Manager必须已有且能认证匹配的完整文件链，才接受此扫描投影；缺链（例如恢复了较旧Manager备份）建立recovery_case，离线从真实Agent导出同一case全链核验并保存，再重新all。Agent全量导出包含所有维护历史，pending可以不含已人工解决项，all绝不能遗漏。两端任何迟到新seq或同seq不同原文都建立新的冲突case；旧resolution仍留存，不覆盖。

有人工历史的Agent绝不能协商退回v1；旧Manager或Agent收到不支持的协议版本明确拒绝且保持冻结。不得用未知字段容忍、删除claims_held或把released的原unknown藏出all扫描来避免版本工作。MB07包括恢复到旧备份后再发现人工历史的真实Store测试。

## 7. 一致备份小程序

### 7.1 输入、锁和输出组

输入为现有受控配置与两个**新建**绝对私有目录。`data-out`保存DB、脱敏配置／版本、清单；`secrets-out`保存相同backup_id的完整identity和实际32字节密钥。Agent identity含当前／候选认领secret，属于秘密材料。Manager TLS私钥按部署密钥另行保管并在清单声明恢复需求，不把一份旧TLS私钥自动装回公开入口。密钥目录0700、文件0600；Windows执行权限检查沿现有localstate能力，不能假称POSIX模式足够。

稳定node／ledger／manager、摘要／账本及KEK身份和实际钥不由现行正常服务替换；Agent完整identity中的候选／active secret和凭据代次可随认领／轮换改变。新增`.materials.lock`供backup与这些发布及未来KEK／部署切换互斥；服务init、认领／轮换发布身份必须接入。在线备份不取得服务独占运行锁，不调用Create/Open，也不修改active_boot、session或recovery_mode。Agent在线备份遇到认领／轮换先等待同锁；数据库及身份不存在天然跨文件事务，不能只读一次identity就假定与DB一致。现有v5没有部署generation；其具体扁平布局／清单／锁顺序及发布失败状态按[完整组规格](m1-backup-group.md)落实，格式2代次发布仍属后续恢复设计。

BackupManifest定义：`format=dm-backup-v1`、backup_id、component、manager_id或node_id/ledger_id、generation、schema/protocol/application/build版本、SQLite版本、开始／完成时间、DB及每文件大小+SHA-256、身份摘要、引用key_id集合、全部持久加密表的行数、语义验证摘要、secret清单摘要和恢复所需的外部材料。清单不包含secret正文。引用关系必须无环：先固定DB和完整identity字节及摘要；secret-manifest只引用backup_id、component／身份ID、DB摘要、identity摘要及其实际私有文件清单，不引用data-manifest；然后data-manifest引用secret-manifest完整原字节的SHA-256。SHA-256提供损坏／错组检测，不能声称可抵御同时篡改数据库和所有清单的攻击者。

### 7.2 SQLite快照和验证

实现选择当前锁定`modernc.org/sqlite v1.61.1`已有的Online Backup API：`sql.Conn.Raw`内断言`interface{ NewBackup(string) (*sqlite.Backup,error) }`，`Step(128)`直到返回`more=false`，必须成功`Finish()`并关闭源／目标连接。该版本Finish会关闭目标连接；不同时调用Commit/Finish以免错误重复使用底层对象。用专用只读源连接，不注册两端带启动副作用的Store；源不能用immutable模式忽略活动WAL。[modernc Backup接口](https://pkg.go.dev/modernc.org/sqlite@v1.61.1#Backup)、[SQLite Online Backup](https://sqlite.org/backup.html)

私有staging目录由程序独占创建，目标由程序生成本地路径，不接受URI／共享路径。SQLITE_BUSY/LOCKED采用50ms起、最大500ms退避，总快照期限5分钟，可取消；完成前的任何错误都标partial，不发布有效清单。备份期间源仍可能写入；只声称一个SQLite一致快照，不声称和另一机器同一时刻或零RPO。不能复制在线DB主文件或依次复制DB/WAL/SHM来冒充快照；WAL可能包含主文件尚未合入的提交。[SQLite WAL](https://sqlite.org/wal.html)

从**副本**读取身份与key_id、检查完整性、外键、精确schema及所有密文；和持锁的身份／密钥材料逐项核对，校验整个版本包原字节、冻结输入、事件链、正常或人工释放依据、claims及归属头。验证不能只抽一个配置或运行quick_check就成功。错钥、未知表／密文列、缺引用、越过额度、身份不同或部分decrypt失败都拒绝该备份组。逐表解密枚举沿§8.1，任何新增加密表必须更新此清单和MB08。

文件逐个Sync，关闭副本，目录Sync，先原子发布secret-manifest，再发布引用它的data-manifest作为最终完成标志。校验顺序固定为：data-manifest中所列DB／普通文件摘要→所引用secret-manifest原字节摘要→secret-manifest的backup_id、component、身份ID及DB／identity摘要与data组相等→全部secret文件字节及数据库密文认证。secret-manifest不反向引用data-manifest摘要，避免循环。两个目录不能跨目录原子提交；上述关联全部成立才算完整组，secret-manifest独自存在仍为partial。失败不覆盖上一组，partial保留供诊断并明确不可恢复。保留策略首版不自动删除，不虚构已满足RPO/RTO；记录实际耗时及大小供O11后续决定。

### 7.3 v5完整备份的只读检查切片

2026-11-13会话客户端日期，实施前追加；源码依据`95d453f`、文档依据`e291a44`。本轮仅继续已定M1的一致备份，不把补充调查或参考产品做法变成新增业务需求、审批流程或发布条件。先实现两端快照检查，再接完整组、材料锁和CLI；恢复、维护及schema6／protocol2仍不在本切片实现范围。

**输入是封闭副本，不能是活库。** 上层先完成Snapshot并关闭其全部连接，或按已发布清单找到私有、静止的单文件DB；检查前后都要求`-wal`／`-shm`／`-journal`不存在，并核对主文件SHA-256相同且与上层清单预期相符。不删除旁文件来制造合格输入。文件必须是本地绝对路径、无链接／重解析点、单硬链接的私有普通文件。缺少旁文件不能证明没有其他writer，“已封闭”由调用方持有、生成及清单绑定的来源保证；不承诺抵抗同用户恶意替换／ABA。

因此只读Inspector使用独立通用SQLite连接、`mode=ro&immutable=1`、query_only及单连接只读事务；不调用业务Create／Open／登录／恢复入口，不生成或更新boot、session、profile、凭据、quota。在线源的Snapshot继续使用**非immutable**方式读取真实WAL；不得把副本检查的连接参数套到在线复制。WAL格式的数据库头本身不构成拒绝条件，完整提交须已在副本主文件内。

两端所属包增加以下入口，具体表与状态验证由[Agent运行](m1-agent-runtime.md)及[Manager运行存储](m1-manager-runtime-store.md)维护：

```go
// internal/agent/ledger
func InspectRuntimeSnapshot(ctx context.Context, path string,
    expected Identity, key []byte, journal JournalKey) (SnapshotInspection, error)

// internal/manager/operations
func InspectRuntimeSnapshot(ctx context.Context, path string,
    expected Identity, keys Keys) (SnapshotInspection, error)
```

两包各自定义SnapshotInspection，Identity沿所属主定义，其余共有字段为：SchemaVersion=5、ProtocolVersion=1、EncryptionKeyID、EncryptionKeyUses、TableRows、SealedRows、SemanticFormat、SemanticFingerprint。计数map为固定表白名单到uint64，零行显式存在；Agent覆盖17表／3密文表，Manager覆盖29表／9密文表。报告不返回原文、逐行指纹、密钥校验值或秘密。所有认证、事务及关闭成功后才返回完整报告；失败返回零值，不产生部分成功摘要。沿既有固定错误分类，另以ErrInspectionLimit区分资源预算不足；取消／期限可用errors.Is判断，不附带SQL、驱动或正文原始错误。

完整检查包括精确v5 DDL、全部singleton、integrity_check、foreign_key_check、身份及两钥、quota保留量、每个历史密文及其唯一AAD、输入／事件／许可／claims／归属／释放／恢复引用。检查存储自洽性，不要求全部任务成功，也不按当前授权、当前会话或今日资源头拒绝合法历史。过期／撤销、合法未知、隔离、扫描中及已保存退出尚无result都要按原事实保留。资料未保存的竞争正文、外部Docker事实、原始HTTP HMAC输入等不能凭检查器补证；具体证据边界在各端说明，不以“完整检查”声称恢复出不存在的事实。

摘要用于后续**同schema**新KEK重加密前后的保真比较，不是外部真实性签名或任意跨版本摘要。统一framing为`类型字节 || uint64大端长度 || 原始字节`；NULL、int64（8字节大端）、TEXT、BLOB、控制标记有不同类型。专用域、component、稳定身份和Digest／Ledger key ID先以原HMAC钥派生检查钥，再以独立域流式HMAC；SemanticFormat分别为`dm-agent-runtime-semantic-v1`及`dm-manager-runtime-semantic-v1`，结果为规范base64url。按固定表序、明确列序、二进制主键序编码列定义、各行及表尾计数，覆盖全部普通表、历史和隔离资料。密文组以认证后的**原始明文字节**、原body fingerprint及绑定／业务元数据代替cipher、wrapped_dek、key_id；不能重新marshal原文再摘要。单KEK的ID、key_check、uses排除于可比根之外，但均须先认证并在报告中保留ID／uses；Digest／Ledger ID及check保留。路径、检查时间不入根。跨schema转换及恢复撤销／门禁变化另行定义，不借此忽略其他元数据变化。

默认总期限5分钟，在SQL／行／解密边界检查取消。按有界主键批次遍历，关闭行游标后再作嵌套读取，不把整表／全部历史装入内存。解密前先检查长度；单份正文上限16MiB、普通单行解码预算32MiB，超出返回ErrInspectionLimit，不冒称所有现行v5超限资料均损坏。保留既有协议／文件／输出的更小限制。历史归属可采用固定内存重复扫描，最坏O(P²)；超时失败，不输出部分证明，不新增规模承诺。

本轮扁平身份格式1尚无部署generation。后续v5备份清单明确`source_layout="flat-v1"`、`generation=null`，以实际身份／key ID及字节摘要绑定组，不生成虚构部署代次。未来格式2的真实generation另行版本化。完整组和CLI在两个Inspector通过后继续实施，本节保存不改变MB08／MB09当前未完成状态。

### 7.4 两端共享的只读资源与摘要辅助模块

2026-11-13客户端日期、编码前保存。`internal/snapshotcheck`只管理副本读取资源和上述framing，不知道Agent／Manager表、密文AAD或业务状态，不输出检查通过／备份完成结论。

`Open(ctx,path)(*Reader,error)`：先检查上下文、本地绝对无链接路径、私有父目录／普通单硬链接主文件及旁文件缺失，流式计算SHA-256；再用独立通用SQLite连接建立ro/immutable/query_only、单连接只读事务。连接实际IsReadOnly(main)须为true；SQLite LIMIT_LENGTH设32MiB作为整行后备，领域检查仍先按16MiB正文等更细预算检查长度。`Reader.Context()`返回至多5分钟的派生context；`Reader.Tx()`供同一个快照中的领域读取。

`Reader.Finish()`在领域全部验证后关闭只读事务、Conn和DB，再核对文件身份／hash及无旁文件，成功才允许领域返回报告。`Reader.Close()`用于中途失败清理或Finish后的重复清理，不意味着领域检查成功。Reader由单一调用者顺序持有，领域代码须关闭每个rows后再嵌套读取或Finish，不把Reader交给正常服务。错误分类为ErrInput、ErrStorage、ErrChanged、ErrLimit；`StorageError(ctx,err)`将SQLite过大及取消映射为固定错误，不回显底层错误。关闭失败不返回成功。Go期限检查不能强行中止失去响应的内核／磁盘。

`NewFingerprint(master,component,format,stableIDs,schema,protocol)`返回仅在内存的Fingerprint；component只允许agent／manager，master32字节，稳定ID顺序为Agent(node,ledger,ledger-key-id)、Manager(manager,digest-key-id)。Marker(string)编码控制标记；Value(any)只接受nil、int64、可用int64表示的uint64、string、[]byte，不支持浮点或隐式字符串转换；Sum()返回当前HMAC的规范base64url，Close清除自持临时钥并使摘要不可继续使用。不承诺Go运行时全部内部副本的物理抹除。

精确类型tag：NULL=0、int64=1、TEXT=2、BLOB=3、Marker=4。每项均为tag、8字节大端无符号长度、原字节；NULL长度0，整数正文为8字节二补码。派生HMAC依次编码Marker(`dm-snapshot-check-key-v1`)、component、稳定ID数量及各ID；以派生钥建立根HMAC，依次编码Marker(`dm-runtime-snapshot-semantic-v1`)、format、component、schema、protocol、稳定ID数量及各ID，再接领域的表／行framing。子模块不重复实现文件／SQL生命周期或上述算法；它不替代各端自己的唯一AAD生成和认证函数。

2026-11-13独立审阅后、修复前补充SC02／SC04：Windows可把`state.db.`或尾空格路径解释为`state.db`，而字符串追加的旁文件路径变成另一名字；不能据别名路径未发现WAL就使用immutable。输入路径必须与打开句柄的规范长路径相同（仅忽略本平台大小写），拒绝尾点／空格、8.3短名等别名；主文件、hash、旁文件及SQLite必须针对同一文件路径。不是通过删除WAL或忽略真实提交修复。其他平台仍按其实际文件名语义，不机械套用Windows限制。

## 8. 隔离恢复、单KEK转换和发布

### 8.1 不能仅重包装的实际原因与枚举

两端现行body AAD含key_id。换KEK及key_id后，即使DEK本身不变，原body cipher也无法用新AAD验证。因此首版选择**完整逐记录解密、验证、使用新随机DEK和新KEK重加密**；保留HMAC／K_ledger，所有input_digest、指纹、幂等比较、Agent事实原文及event_seq不变。不实现在线keyring，不把旧库uses重置后继续同KEK。

转换器在包内复用原binding生成器，不能在外部脚本猜AAD：

| 端 | 必须覆盖的现行v5密文表／binding |
| --- | --- |
| Manager | `operations` payload；`operation_events`及`runtime_pending_events`按事件type+seq；`recovery_pages`用原round／page／cursor上下文；`recovery_cases`用原case上下文；`finalization`用原finalize request／boots／session／round；`user_sessions` CSRF按原session/user/token hash（即使将撤销也验证保存）；`config_versions`按version/node/stack；`runtime_deliveries`按原delivery、boot／round上下文 |
| Agent | `execution_events`按operation/digest/kind/seq；`finalizations`按原finalized绑定；`runtime_documents`按`runtime-<kind>`及seq；K_ledger身份校验和local_fingerprint保持原值 |
| v6新增 | maintenance／restore对象按独立域、case／resolution、双方身份、generation及step绑定；加入统一sealed-row枚举。元数据没有cipher但参与binding的字段也必须保留和验证 |

FK引用单KEK时，转换目标先建完整新DDL、插入新keys/event_keys，随后逐表插入原有行及新密文，延迟FK或按依赖序处理。不能原地改旧keys主键导致旧行无法读取。目标是包含原所有表、行及业务关系的完整逻辑副本，**不是创建空服务库再重新登记节点或操作**。

### 8.2 stage与额度

1. 原备份、清单、identity、旧钥全程只读。取得target私有目录，禁止已有文件和链接；生成restore_id和新的generation。验证§7全部材料后才生成新KEK ID及32字节随机key，HMAC或K_ledger原ID原key保留。
2. 建立独立staging元数据并保存本次新钥；新KEK仅用于本次目标。每次实际封装前独立提交额度reservation，不因后续事务失败退款；额度上限仍1,048,576。预先计数需转换密文加恢复控制记录，若超额度拒绝`rekey_capacity_exceeded`，不假装支持多KEK。不得为了装下记录删执行历史。
3. 小批逐记录open旧cipher、认证指纹／原Schema及业务关联，然后用相同原文字节及新key_id重新seal，保存目标行。原结果原文字节摘要及完整语义计数必须相等；new uses包括所有尝试及预留，不能只填成功行数。恢复需要隔离标记、session撤销等新增写入，须在新KEK下执行。
4. 中断后首版不继续任何结果不确定的新钥封装。staging标abandoned、原样保留，并用**全新KEK/新generation**重做；不重用那把新钥并重置uses。已发布stage不得回头续写。没有把操作系统进程崩溃等同于reservation必已耐久的假设。
5. 全量验证新DB、身份、密钥、所有密文及业务关系，生成转换报告；再次生成并验证这份新恢复组的备份，将new backup_id写入READY。READY前任何错误均不能publish。原备份仍保留，不能由新钥覆盖旧key文件。

converter不是正常Store writer：源用独立只读解密上下文，目标用仅stage可用的写上下文，独立记录新钥reservation；不能调用普通Open去改源active_boot，也不能以暂时关闭认证／外键校验的通用Store供正常服务使用。逐记录事务失败留下的reservation与目标写入计数分开记录；stage完成时把其已预留总数写入目标单KEK的uses，并校验不小于真实所有尝试。转换上下文不能被serve引用。

初次升级v5→v6也走完整离线转换，普通重启不自动升级。v1—v4历史夹具格式不在本轮自动转换范围，明确拒绝；版本迁移、协议v2和新身份格式须一起出兼容矩阵。

### 8.3 DB／密钥／身份作为一组发布

当前分散路径不能靠“先替换DB再替换key文件”实现原子发布。新增部署manifest格式2，根目录的`current.json`指向不可变`generations/<id>/`；其中含DB路径、identity、密钥路径和摘要、schema/protocol、restore_id及generation。服务启动先取得原部署独占锁，再一次读current，按全部摘要与DB身份核对；不能混读旧路径和新generation。旧配置路径只用于定位部署根和一次性转换，不偷偷自动寻找其他钥匙。

stage的所有文件及目录Sync后标READY。publish持有服务运行锁和materials锁，核对`expected-current`，在同一文件系统写`current.json.tmp`、Sync、原子替换current、Sync父目录。崩溃结果只能是旧完整组或新完整组；任何摘要／代次矛盾拒绝启动。Windows对文件替换和锁单独验证，不能把Linux rename语义直接套用。原数据目录和旧current留存审计；回退旧组属于**再次隔离恢复**，不能恢复旧KEK使用计数后直接serve。

### 8.4 Manager恢复门禁、授权和凭据

恢复目标必须标`recovery_mode=1`、所有节点冻结、已有浏览器session全部revoked（包括备份时尚有效者）、旧执行会话及投递关联失效；原历史请求／回执保留只作证据，旧prepare和许可不自动重发。先只绑定本机维护监听，禁止公开Web变更；恢复管理员通过本机入口设置新口令和新会话，原账号ID／历史受理关系保留。

旧备份可能遗漏新增账号、撤权及凭据撤销，恢复软件不能从旧副本推知当前授权。首版安全默认：所有节点grants视为待复核、普通账号禁用待逐一重新启用；保留原表和审计，由当前负责人提供逐项授权清单，产生新的auth_rev／审计，不直接用旧grants放行。无法给出当前授权时继续维护模式，而不是把“管理员勾选备份可信”作为权限核验。

全部旧node_credentials及未消费enrollment先标撤销，取消旧enrollment，不因旧token可哈希匹配就放行。按同一node_id/ledger_id执行本机受控重新凭据绑定：操作者先从真实Agent本机读取当前active/candidate generation和身份摘要，与恢复Manager已知最大代次取max后加1；到达安全整数上限拒绝。生成新32字节secret，Manager存候选hash，操作者传入真实Agent，Agent耐久保存候选，Manager提交active后Agent确认；丢响应按同一候选重试。**不新建身份、不清账本、不降credential generation**。必须改造既有轮换入口以支持“旧凭据已不可信”的离线恢复凭据流程；旧普通RotateNodeCredential不足以承担此事。

真实Agent通过新凭据、TLS和原node/ledger身份连接后，强制scope=all，含已finalized、已ACK和人工维护历史。Manager发现副本后新增操作／缺许可／missing input时保存在recovery_case，保留全部原事实，不假造受理版本。原unknown继续占用或走§4维护；正常已释放历史按已有双端证明核对。逐节点完成当前all/completed及所有恢复case后，持久记录当前授权复核摘要与round，才开放该节点。不能用导入的空账本或备份里的all报告替代真实Agent本次全量核对。

### 8.5 Agent备份恢复及账本回退

Agent恢复保持原node_id/ledger_id、K_ledger和全部历史，但标`ledger_rollback_suspected`，不允许凭数据库能打开就普通连接。旧Agent服务／Compose和可能已提交Docker请求必须先按§5建立端点屏障。恢复前可读的当前账本、identity、钥匙和工作区保留为原材料，并在新恢复case中引用；能认证的新增历史只追加导入，不由旧副本覆盖。

Manager与Agent双端按all比较已知操作：Manager已授权而恢复Agent缺记录的操作是未知，不能归为“从未执行”；双端都无备份后执行记录时也不能用空集合证明未执行，必须有端点屏障与受控资源接受事实。原数据损毁且无法重建原输入／目标关联时持续隔离，不改ledger_id逃避。处理过程保留rollback case，即使最后人工结束占用，原记录缺失的事实仍显示。

## 9. 失败、时限、取消和恢复表

| 失败位置／刺激 | 持久与外部行为 | 继续入口 |
| --- | --- | --- |
| plan过期、身份／输入／profile不符 | 无停止；冻结及原证据保留 | 重新inspect及新计划，旧case明确superseded且只能未开始屏障时替代 |
| fence超时／取消／停止失败 | 已发生的停止不回滚；保存执行到哪一步及残留；不发布充分屏障、不释放 | 原case重新inspect，明确计划后继续；不能自动重启未知服务掩盖失败 |
| fence后观察不完整／归属不明 | 屏障证据保留，claims不变；原unknown不变 | 只读重观察或保留隔离 |
| Agent Prepared后丢文件／Manager停机 | Agent resolution已存，本地claims已按事务释放；endpoint gate仍阻塞新动作 | 重导出相同回执并accept-agent |
| Manager Committed后Agent未accept | 双端仍maintenance，禁止新受理／许可 | 重导出同证书，Agent accept及Manager activate |
| 任一步数据库提交不确定 | 无成功回执；不猜事务失败／成功 | 以case+step查询已提交原字节；不存在则重试同一步 |
| snapshot busy／满盘／取消／Finish失败 | partial组不生效，上份不动 | 新backup_id重试；源服务不进入恢复或换boot |
| 缺钥／错钥／损坏／未知密文表 | 不输出秘密、拒绝stage及publish | 修复原材料；不生成替代旧钥尝试解密 |
| rekey中断／额度不足 | 源只读，stage原样保留abandoned；未发布 | 新generation、新KEK重做；超额度要新的格式能力，禁止删历史 |
| publish前后崩溃／旧current并发改变 | 全部材料无READY或expected不符即拒绝；可重读current判定 | 原publish幂等检查，不跨代次拼接 |
| all扫描出现新事件／case／旧人工链缺失 | 重新冻结，保留两份证据；不先开放“其他步骤已成功”的目标 | 导入真实证据、重all，必要时新维护case |

文件操作／普通本地DB阶段默认总期限5分钟；文件交接不设自动解锁时限。用户取消只停止未来步骤，不撤销已经发生的Docker停止、凭据撤销或DB提交。任何时间过去、进程消失、操作者备注或只读快照都不单独构成释放证明。

## 10. 验证及维护者必须保持的规则

测试必须同时断言原结果与seq未变、claims的事务变化、endpoint gate、实际底层调用数、当前恢复屏障和下一次冲突操作的允许／拒绝。代码入口沿§2：新增密文表时检查sealed-row枚举、备份清单、rekey以及AAD负例；修改归属时检查正常／人工两种修订链；修改协议时检查all原文及人工证据引用；修改服务初始化时检查旧路径及current混组拒绝。

MB记录分别报告本机真实SQLite验证、受控执行器／假systemd验证、Linux真实Engine验证。真实停止尚未执行之前O12和V06保持未验证；没有真实Agent、当前授权／凭据及旧备份演练之前V07保持未验证。规格保存只表示可以据此实施和审阅，不表示模块、阶段或产品已完成。
