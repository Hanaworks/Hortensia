# M1 固定工作区、Compose适配与进程监督

现行阅读提示（更新至2026-11-13，实施后追加）：下文2026-11-12事前设计和随后修订按其时间保留。三个源码模块已有实现与独立复核，指定Linux进程组／链接／锁断言已在InstaBox实际通过，见[IB平台结果](../implementation/m1-instabox-debug.md#6-第一轮-linux-源码回归)和[CE记录](../implementation/m1-compose-engine.md)。原生Docker／Compose对照与产品集成分别验收；精确生产接受表仍为空，不能据Probe或原生CLI脚本直接开放能力。

2026-11-12事前基线，先于本模块实现保存。范围承接[Agent运行§5—6](m1-agent-runtime.md#5-安全工作区与原生预检小程序)、[执行恢复§2—3](execution-recovery.md#2-配置预检与原生执行)。本模块没有账本事务、网络发送、event_seq分配、claims释放或active归属发布权限。真实Linux/Docker矩阵待环境安排。

## 1. 调用、数据与边界

`workspace.Materialize(ctx, stateDir string, input workspace.Input) (workspace.Package,error)`：Input含已持久收件的OperationID、InputFingerprint和原Payload；调用者必须先完成ReceivePrepare。Package含绝对Dir、OperationDir、相对WorkspaceID、ManifestFingerprint、原输入Payload（结构化只读副本）和文件清单；内部保存原字节及摘要。Payload字段形状以[共享Schema](../../contracts/agent-v1.schema.json)的FrozenPayload/Config为准。InputFingerprint来自ledger.Record.LocalFingerprint，不能用SHA冒充该HMAC。Package只属于原operation；重开重新从账本ReadExecutionInput物化/核对，不能从“最新配置”恢复。

`workspace.Verify(ctx, pkg) error`重新检查完整文件/目录集合、普通文件类型、长度及原字节，并比较创建时冻结的Payload、manifest指纹及包外empty.env/空HOME/空DOCKER_CONFIG；`workspace.ReadFile(ctx,pkg,path)`只返回已核对清单成员。manifest是有序path/size/SHA256的JSON，指纹为SHA256的Base64url；只存在operations目录，不能污染package。已有包差异workspace_drift，不覆盖。取消中止准备，已发布文件保留；M1不自动清理可能被挂载的工作区。

`compose.Profile`保存DockerCLI绝对路径、DockerSocket同机unix端点、ledger.RuntimeProfile、ExecutionTimeout（1—86400秒，默认1800）及AllowLabelFile（只有原生对照后开放）。`compose.New(profile) (*Engine,error)`固定该上下文，无自定义结果提供器。`Engine.Prepare(ctx,pkg,binding ledger.ProjectBinding) (Prepared,error)`返回PreparationEvidence与仅内存Normalized模型；binding不存在由调用方传零值。`Engine.VerifyBeforeStart(ctx,prepared) error`重新检查包、外部依赖及全项目pre基线；`Engine.Invocation(pkg) (executor.Command,error)`只构造up参数。VerifyBeforeStart必须在Gate前完成长IO，Gate短回调仅Start。

`Engine.Observe(ctx, prepared) (ledger.ObservationEvidence,error)`从实际Docker列举+inspect产生公开Observation及私有post/replacements/conflicts；失败仍返回含error/incomplete的Observation，不把失败变空complete。补观察query登记与缓存由ledger/service负责，本模块一次调用就是一次真实采集，不隐式重复。`Engine.Probe(ctx)`读取Engine ID、Docker及Compose版本，返回用于RuntimeProfile绑定的证据；CLI版本适配开放仍受真实环境验收控制。

`executor.Command`含Program绝对路径、Args数组、Dir、Env显式集合及Timeout。`executor.Start(ctx,command) (*Process,error)`只支持Linux受监督变更执行；Process.PID仅诊断，Interrupt(reason)幂等，Wait()返回ledger.ProcessOutcome（BootID由服务补齐）。中断原因输出采用共享协议的canceled/deadline/agent_stopping/signal，调用别名cancel/shutdown分别规范化为前述对应值。Start后即启动独立Wait和排空，不要求调用方及时Wait才监督。RunReadOnly只执行受控结构化只读调用，分开stdout/stderr，30秒/8MiB stdout与1MiB stderr预算，超限继续排空并返回固定错误，不能用于开始up。

## 2. 工作区小程序

stateDir/workspaces/stack/version/package保存完全原字节；operations/operation保存manifest.json、empty.env、home和docker-config。临时目录同盘、独占写0600、目录0700、Sync后rename并同步父目录。Linux所有相对操作以已打开目录句柄和O_NOFOLLOW进行，每级目录都不跟随链接；state根必须由当前账号持有且无组/其他写权限。Windows路径检查仅供受控本地测试，不能声称Linux句柄/ACL保证。拒绝路径穿越、反斜线、冒号、空字节、重复路径、前缀冲突、额外文件/目录以及重解析点。文件最多128个、单个1MiB、总计4MiB。

## 3. 解析与原生模型小程序

每个compose文件先经YAML AST节点/深度预算、重复/非字符串键、标准tag、alias循环检查，再展开alias及merge。展开节点上限100000、深度64；扫描全部原文件包括覆盖掉/未启用定义。离线Compose schema拒绝未知非x属性；x内容被alias引入真实字段后照常检查。不调用compose-go loader。读文件入口及拒绝项以执行恢复§2表为主；插值env只使用dotenv.ParseWithLookup消费包字节，lookup也检查控制键，绝不读取宿主环境。label_file默认关闭。

所有包内引用在首个CLI调用前验证。build仅接受字面量本地context和普通dockerfile路径，拒绝additional_contexts、ssh、secrets、远程context及dockerfile_inline等未经覆盖读入口；不读取宿主构建材料。原文不改写。挂载静态记录显式绝对来源，含变量/波浪号拒绝；最终以原生json规范化bind/volume/tmpfs模型验证，包内bind只能只读普通文件。

固定命令前缀为`--host socket compose --ansi never --project-name project --project-directory package`，随后有序env、-f、profiles。显式环境仅固定PATH、受控HOME/DOCKER_CONFIG、locale和通过检查的插值变量。未选env时显式选择包内.env，否则operations/empty.env。config用`config --format json`，up只用`up --detach --no-build`，相同上下文、无shell。config输出只在内存，预算8MiB，超时30秒；不写日志或包。不能由预设JSON证明Compose已运行。

## 4. 监督与观察小程序

Linux Start设独立进程组。ctx取消或Timeout触发TERM；10秒后对整个组KILL，始终Wait。实际信号退出或任何Interrupt即使exit0仍为interrupted，无法得到可信状态为unknown。stdout/stderr并发排空，经单采集锁按接收顺序组合；每个流独立保留UTF8尾字节，不把分块误当非法字符，转换后1MiB完整字符截断，超限持续排空。结果由Wait缓存，重复Wait不消费另一个结果。

进程已中断且父进程先退出时仍KILL该进程组，避免已关闭管道的后代继续运行；PID/组只用于本boot持有的子进程，不跨重开使用。启动Probe使用新建私有临时HOME/DOCKER_CONFIG，不读取服务账号默认Docker配置。Probe成功只证明只读版本可查询，服务层另以精确版本验收清单决定是否发布compose.apply.v1能力。

观察总窗口30秒，容器包含停止状态。分别按项目label列container/network/volume，再逐对象inspect且核对真实ID、项目label、逻辑键及列举数量，资源预算200。对象消失、重复逻辑键、失败和超限不能complete。健康无Health字段为none，未知状态为unknown。ResourceRecord只保留共享字段；inspect Env/Secrets不进入持久材料。卷ref是name+driver+created弱指纹。external网络/卷独立按名称只读存在性检查，排除managed。首次项目非空或未知同名资源拒绝；更新pre与active按资源身份/关键关联比较。正常post允许预期服务实例/网络/卷和已有孤儿，替换映射只对同逻辑旧→新生成；无法解释者进入Conflicts。发布归属最终由ledger重新计算，观察本身不写基线。

卷ResourceRef的可诊断编码为`name|driver|CreatedAt`，名称/driver采用Docker原值；CreatedAt缺失不伪造，账本保持unknown。container/network ref必须为64位小写hex。已知关联矛盾的最小实际ResourceRecord写入Conflicts；普通查询错误没有确凿冲突则保留unknown及补观察可能。补观察重开后可以只用原PreparationEvidence和重新核对Package构造Prepared，不重新Prepare/config/up。

## 5. 维护与限制

workspace负责磁盘字节，compose负责模型和Docker证据，executor负责真实进程事实；错误只返回固定code与源文件/字段/位置，不带配置值和输出。修改Compose依赖时同时复核schema新增读入口、静态拒绝表、dotenv控制键和原生命令矩阵。恢复不重附着PID、不重复up。Linux句柄、信号组、真实Docker资源行为只有对应环境实测后可声明；Windows夹具及交叉构建另记。

2026-11-12实现后核对：源码入口分别为workspace.go/secure_linux.go、compose/references.go→preflight.go→command.go→observation.go、executor/executor.go→output.go/process_linux.go。已固定go.yaml.in/yaml/v3 v3.0.6（Go1.16，MIT/Apache双许可证文件）和compose-go/v2 v2.14.0（Go1.24，Apache2）；实际项目Go1.27编译通过。只使用schema及dotenv两个包，不导入loader。dotenv lookup对任何键返回空且已存在以关闭依赖默认缺变量告警，值不进入执行变量；所有键仍检查，LD_*/DYLD_*/LC_*及GODEBUG等控制键一并拒绝。该段为事后实现补充，未回写事前状态。

## 6. 2026-11-12独立复核后的修订基线（先于修复）

CW01/CW02探针指出前述实现检查不足：导出的OperationDir未绑定创建上下文；仅靠dotenv lookup遍历不能覆盖短路的嵌套分支。原始失败证据保留在`.cache/m1-compose-wire-review.md`和对应probes.log，不以原测试通过覆盖独立发现。

修复后的Package必须将OperationID、InputFingerprint、OperationDir、Dir、WorkspaceID、ManifestFingerprint和规范Payload/清单绑定为同一份私有冻结上下文。Verify先比较所有公开执行字段，再从冻结root/原operation派生同一操作目录读取材料；不允许切换到同版本另一操作的有效目录。CLI使用的HOME/DOCKER_CONFIG/empty.env必须与已核对目录一致。此规则不要求调用方能访问HMAC密钥：指纹从收件传入后按原值冻结，不能在后续Package内改写。

dotenv增加与求值无关的词法安全检查：按dotenv声明、行内注释、单/双引号及跨行边界识别会插值的值；跳过单引号字面量及转义美元，遍历`$KEY`和`${KEY...}`的所有嵌套操作数（默认`-`/`:-`、替代`+`/`:+`、必填`?`/`:?`），与当前lookup值或条件无关。检查只提取潜在控制键，不产生变量值、不覆盖原文件。声明/escape/插值合法性仍由固定版本dotenv.ParseWithLookup决定；scanner遇到预算或无法安全定位的边界时拒绝，不自创变量求值器。保留原lookup键防护和赋值键检查，合成lookup仅用于无秘密语法检查，不能作为分支覆盖证明。

新增`compose.IsVerifiedRuntime(dockerVersion, composeVersion string) bool`供service决定是否发布compose.apply.v1。生产精确组合清单初始为空；仅精确字符串命中已完成真实Linux/Docker/Compose矩阵且有证据的源码条目才true。无版本区间、自动去前缀/trim/归一化、Probe即通过或运行时开关。Probe/只读恢复本身不受此API阻止；生产service在开始新变更前调用该门禁。测试的私有fixture版本表不写进生产清单，也不给生产Engine添加成功结果或绕过选项。真实版本录入须先补矩阵证据与范围再改清单。

修复后实现核对（晚于本节基线）：workspace.Package的私有executionContext保存所有执行字段及清单JSON，Verify先比较，再从冻结operationID派生已核对操作目录。compose/env.go使用声明/注释/引用边界扫描以及线性美元标记扫描，不复制Compose变量求值；双引号中仅按固定依赖支持的escape追踪美元是否变为活动字符，单引号和注释不扫描，unquoted反斜线不被误当作美元转义。后续成熟ParseWithLookup使用固定非空占位值防止缺值告警/required的假错误；该值和解析产物始终丢弃，仅赋值键与lookup键用于控制键检查。实际变量插值仍仅发生于原生config/up读取原文件。runtime_versions.go已实现公开精确检查API，源码清单保持空。新增维护测试见env_test.go、runtime_versions_test.go及完整Package上下文回归。

## 7. CW04公开资源关联修订基线（2026-11-12，先于修复）

基础`collect`只负责当前Docker事实，ResourceItem.stack_id默认null；请求Package内StackID及相同project/service标签均不构成本地管理登记。Observe可以把未知资源放入私有Conflicts，但不能同时在公开摘要冒认其属于请求Stack。顶层操作仍使用原Stack，不受摘要空关联影响。

只有可信本地登记与本操作Pre基线、冻结Package相互绑定，且当前资源身份及关键关联与该已登记Pre条目完全相同，才允许回填StackID。至少核对原operation/input/workspace/manifest/profile/endpoint/Engine及Stack/project对应关系；精确容器身份包括kind/ResourceRef/LogicalKey/managed分类/project/service/container_number/created_at/driver。存在冲突的资源、仅符合Expected的新候选、旧逻辑键换新ID的替换候选、无已认证登记来源或关联字段不匹配，一律null。未经finalize的新/替换候选保持null是本轮采用的保守行为；归属发布仍只由ledger执行。

可信来源只能来自ledger既有认证准备/登记读取路径，不接收单独trusted布尔或把本轮Docker快照当登记。补充具体传参方式前与根协调Store当前验证：共享Schema允许ResourceItem.stack_id=null，ledger不能为凑显示字段而要求未登记候选非null。本模块不改ledger。此基线覆盖公开摘要标注，不改变ResourceRecord/Expected/Replacement及已有终结发布事务。

本轮选定较小传参方案（仍先于修复）：不增加Prepared.Binding。调用方必须先CommitPrepared成功，再以Store.ReadPreparationEvidence取得认证Evidence交给Observe；重开同样读取，不能继续使用尚未入账的内存Prepare结果。该证据的operation/input指纹绑定已冻结Stack/project/endpoint输入。Engine先核对Evidence的operation/input/workspace/manifest/profile/Engine/版本与Package及固定Runtime，且Package的endpoint/policy匹配；完整身份缺失或不一致返回execution_profile_mismatch，不声称观察成功。仅OwnershipRevision>0的可信Pre中完整相同managed容器可回填Stack；此数值不是调用者任意给出的信任开关，其来源必须是上述认证Store记录。

## 8. CE15观察端点复核基线（先于实现）

2026-11-12服务组合修正前记录：PolicyRevision沿共享Revision定义，0是合法首次策略修订；观察必须逐值匹配Package／Profile，不得额外要求大于0。OwnershipRevision>0仍只用于公开Stack关联的已有登记条件，两种修订不能混用。

同一Unix socket可能在Agent存活或正常结局后补观察前指向另一个Engine，不能用新Engine的空集合使旧操作收敛。Observe在任何list/inspect前实际读取info及Compose版本，使用已核对Package关联的受控HOME/DOCKER_CONFIG/显式环境；不能只看启动时缓存的Profile。实际Engine ID、Docker/Compose版本、固定profile指纹和endpoint/policy与原准备证据必须相同。Probe失败、字段缺失或变化时返回明确Observation.error（相应probe错误或execution_profile_mismatch），Post不冒充新端点基线，且list/inspect调用数0。准备前静态拒绝的首CLI计数0规则仍保留；此额外Probe位于既有准备成功后的观察。VerifyBeforeStart已有同类实际Probe复核，本轮共享相同受控命令实现。原子跨Docker观察锁仍不存在，复核与观察间的外部竞争不被声称全部隔离。

实施后核对：command.go的probePackage/probeCommand共享实际info/Compose版本读命令；启动Probe仍使用独立私有临时目录，VerifyBeforeStart和Observe使用已验证操作目录。observationContextMatches在CLI前核对Package/Profile/Evidence，observationFailure生成error状态及空items而非空complete。完整采集后以全ResourceRecord比较和冲突ref排除回填既有Stack；基础collect、错误/部分采集、新候选与替换不会回填。回填所用Evidence的认证来源由调用方Store读取职责保证，Engine不接收trusted布尔、不生成新的归属登记。
