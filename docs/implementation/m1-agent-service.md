# M1 Agent服务实施与验收

2026-11-12事前基线。依据[服务规格](../specifications/m1-agent-service.md)。本机Windows只证明受控测试和编译；Linux权限、目录同步、真实Docker及原生矩阵另列，不因通过本机测试标为真实部署。

顺序：先保存本基线→失败测试→共用本地文件/锁→配置/身份命令→真实通道/调度→页面缓存→组合测试/构建→按最终代码补交接记录。

| 编号 | 验收/观察 | 初始状态 |
|---|---|---|
| AS01 | 严格配置64KiB/未知重复null类型相对路径/无env覆盖 | 未验证 |
| AS02 | init两钥及候选耐久、重开同ledger/endpoint、部分材料不覆盖、第二进程锁 | 未验证 |
| AS03 | TLS enroll/rotate提交丢回复使用同候选、401停止、不暴露秘密 | 未验证 |
| AS04 | hello真实身份/Profile、welcome冻结及扫描完成；读loop不被Compose/SQL卡住 | 未验证 |
| AS05 | prepare持久→物化→预检→ready；未许可零启动；长IO在请求前；短Gate只Start | 未验证 |
| AS06 | 断网已启动继续；取消/关闭回收且持久结果；重开不附着旧PID | 未验证 |
| AS07 | 多事件重报、ACK reply_to+op/seq关联、finalize正式事务 | 未验证 |
| AS08 | query持久绑定、一次采集固定5分钟页面、改条件/过期/重开拒绝、≤200页 | 未验证 |
| AS09 | 真实CLI Probe不能放开空验证清单；CLI/Agent构建、关联回归 | 未验证 |

完成需实际代码/命令构建、逐项证据与限制；不以测试名称或exit0替代行为断言，不把源码交接等同M1首版发布。

## 实施后AS逐项记录（2026-11-12）

基线af43699加未提交差异，Windows amd64、Go1.27.0。正式本地服务TLS及数据库，不连接用户节点。测试CLI明确夹具，生产精确版本清单为空。详细可重复入口为internal/agent/service/*_test.go、cmd/agent/main_test.go，关联锁测试承接Manager service原测试。

| 编号 | 实际结果及证据 | 未验证/限制 |
|---|---|---|
| AS01 | 本地通过：ConfigRejectsAmbiguity验证严格JSON、64KiB、重复/未知/null、相对路径；输入只来自config，不读环境覆盖 | Linux默认路径实际部署未验证 |
| AS02 | 本地通过：InitializationNeverReplacesEvidence、PartialInitializationIsNotRepaired；共用ManagerLockUsesOperatingSystemProcessLifetime验证第二进程/退出自动释放，Manager关联全包通过 | Windows mode位不证明ACL；Linux目录Sync/权限及掉电耐久未验证 |
| AS03 | 本地TLS通过：CredentialLostRepliesReuseDurableCandidate，服务端提交后断HTTP，重开同候选、同ledger、仅1次enroll/rotate、generation2；ServeAuthenticationRejectionStopsReconnect断言401一次即停 | 正式Manager双进程认领/轮换仍需组合验收 |
| AS04 | 本地TLS通过：ServeUsesDurableIdentityAndActualRecovery消费真正hello/welcome，库生成reconcile_reply/completed；网络读loop独立有界 | 长IO压测及真实网络中断未验证 |
| AS05 | 受控本地通过：SchedulerPersistsBeforeReadyAndCorrelatesEvents核对持久准备、ready/request_start与许可前up0；源码顺序保证长IO前置 | Windows真实Start unsupported并保存not_started2；Linux/Docker真实up未执行 |
| AS06 | 生产接线已完成：socket context不传给Process，监督器先写exit再观察，关闭等监督完才Close Store | 真实进程断网/关闭/TERM及强杀Agent组合未验证，不记整项通过 |
| AS07 | 受控本地通过：started/result两事件重报与reply_to关联，不认识ACK保持AckSession空，匹配ACK仍持双claims，正式finalize才释放 | 多连接重试/逐消息丢失及真实Manager全链未验证 |
| AS08 | 本地子集通过：固定snapshot页面、opaque cursor、改limit拒绝、5分钟和重开cursor_expired；ledger补观察持久关联由账本原测试承接 | 页面与补观察Store端到端组合未验证；普通query明确unsupported留M2 |
| AS09 | 本地通过：空IsVerifiedRuntime强制不报告能力、缺Docker仍恢复；Agent Windows/Linux命令构建及vet通过 | 精确版本真实矩阵为空；不能部署Compose，AR16未验证 |

最终阶段发生过一次并发编辑窗口的调度夹具缺request，随后最新源码定向通过；最终重复回归结果将在下节追加，不以早期记录替代最终证据。

### 最新重复验证（2026-11-12）

最新源码`go test ./internal/agent/service -run TestScheduler -count=3 -timeout 90s`通过（3.159s）；完整Agent/CLI TLS回归通过（service2.284s、CLI1.328s）。前述一次缺request在最新代码三次重复中未再现。随后CW04接线修订改为CommitPrepared后正式读取认证PreparationEvidence再请求许可，须随最终组合再次回归。

### 补观察Store与分页组合补验（2026-11-12）

Scheduler受控组合扩充通过（2.870s）：使用正式账本/准备接口构造明确测试进程事实normal_exit/error；service.observe正式BeginObservationQuery→ReadPreparationEvidence→CLI夹具采集2容器→RecordObservationUpdate seq3→limit1固定两页。断言原finished_at、output_b64、outcome不变；重复首请求和第二页均未增加CLI调用，query持久关联replay/seq3。AS08本地补观察Store+分页子集现已验证，跨真实TLS observe报文及真实Docker仍未验证。进程事实阶段为受控账本夹具，不能当作Windows真实Docker执行。

CW04组合的0修订反例（2026-11-12，修复前）：正式ledger允许初次绑定policy_revision=0，Compose新观察校验却要求>0，造成ready/Start可接受但Observe必报execution_profile_mismatch。AS08／AS09暂记最新组合失败；已报告根与Compose负责人按共享Schema统一，不撤销前面代码版本的历史测试记录，也不把它们作为最终通过。

### 最终组合回归（2026-11-12）

共享policy_revision=0主定义统一后，五包回归全部通过：Agent service11.663s、CLI2.448s、Compose20.723s、ledger24.898s、Manager service14.152s。前述CW04/0反例现已闭合，AS08本地Store/固定分页组合、AS09关联回归可记本地通过，真实环境限制不变。Linux amd64/Windows amd64 Agent命令均构建通过；vet及文档静态检查（59篇/731链接/194表）通过。源码/本地切片完成不等于Linux部署、M1阶段完成或发布。

## AS-R01—R03 修复验收基线（2026-11-12，实施前）

依据[AS审阅修订](../specifications/m1-agent-service.md#as-r01r03-审阅修订基线2026-11-12修复代码前)。先保存基线，再正式测试RED，最小修复后定向及service/CLI回归；不提交Git。

| 编号 | 前提、动作及预期观察 | 事前状态 |
|---|---|---|
| AS-R01a | 正式Prepare/Gate/Created/Exit已提交，尚无result；Close/Open后服务恢复产生seq2，原BootID/outcome/exit/finished/output/truncated逐字节保持；只发既有事件，无up/旧PID附着，双claims仍在 | 未验证 |
| AS-R01b | 同boot真实SQLite结果写触发器临时拒绝，退出原事实仍可读；恢复失败不造result/释放，去故障后周期恢复seq2；重复恢复不多seq、不重新执行；活监督器观察中不被提前恢复 | 未验证 |
| AS-R01c | 缺首结果的normal_exit经服务observe先恢复首result，再持久query补观察seq3与固定页面；原进程字段保持、重复query不二次采集；未知/中断仍隔离 | 未验证 |
| AS-R02a | 分别预置wal/shm/journal普通文件与目录；init拒绝，原字节/目录项保持且无两key/identity/db；与identity路径冲突在副作用前拒绝 | 未验证 |
| AS-R02b | 旁文件链接时init与openIdentity拒绝，目标原字节保持；Open前拒绝非私有/非常规journal；合法WAL恢复不因存在而被拒绝 | 未验证；链接/权限须真实OS支持 |
| AS-R03 | version非4、variant非法、大写/格式错的node-id在目录/锁/key之前ErrConfig；合法v4仍可init；加载身份遵循同约束 | 未验证 |

证据写`.cache/m1-agent-service-fix-report.md`及本篇；真实Linux、Docker精确矩阵、掉电耐久与完整双服务业务流程继续未验证。

AS-R01d（补充基线，2026-11-12，实施前）：实际Serve在没有Manager监听时先恢复耐久退出；Store打开后注入临时result写失败，失败期间无result，解除后无需新连接即自动seq2。周期遇到认证process密文损坏须返回固定错误并退出，claims保持；活collecting操作在恢复扫描中不得提前生成不完整结果，标志结束后可收敛。测试故障不得预先改变Schema，避免把OpenRuntime的正确schema拒绝误当恢复失败。

## AS-R01—R03 最终本地结果（2026-11-12，实施后）

源码基线af43699加当前未提交差异；Windows amd64／Go1.27.0、真实临时SQLite及明确测试CLI。生产代码与测试文件已冻结。下面只说明作者本地证据；根的冻结快照总验和独立复核另外记录。

| 编号 | 结果／证据 | 剩余限制 |
|---|---|---|
| AS-R01a | 通过：normal_exit及interrupted的同boot／Close-Open且Disconnect后恢复seq2；原BootID、outcome、exit17、固定finished_at、含换行/中文/NUL的output、truncated原值保持，重复恢复无seq3，双claims不释放、CLI没有up | 进程事实由明确受控Store夹具提供，真实Agent强杀/Docker未验 |
| AS-R01b | 通过：真实SQLite result INSERT触发失败，无result／无释放且原事实可读，去故障后原事实收敛；collecting活监督器不被周期抢先写不完整result | 长时持续存储故障与实际磁盘故障未验 |
| AS-R01c | 通过：重开缺首结果的observe先seq2，再正式query补观察seq3；原进程字段保持，原固定分页／重复query不二次采集回归保持 | 真实TLS observe报文与Docker未验 |
| AS-R01d | 通过：实际Serve无Manager监听也在startup恢复；Store打开后临时写失败解除，无新连接自动恢复；周期认证process密文损坏返回固定ledger诊断退出，双claims保持、无伪result | 本机进程/回环边界，不等于真实跨节点故障 |
| AS-R02a | 通过：wal/shm/journal普通文件原字节、目录内原子项保持，拒绝前未写keys/identity/db；保留路径及Windows大小写别名在目录/锁之前ErrConfig | Windows mode不证明Linux私有权限 |
| AS-R02b | 本地子集通过：Open前拒journal目录且原子项不变，既有私有非空WAL仍能打开且metadata写入值7保留 | 链接测试因当前账号不能创建symlink而SKIP；Linux权限／真实热崩溃恢复未验 |
| AS-R03 | 通过：非v4、错误variant、大写/格式错node在目录/锁/key前ErrConfig；合法v4初始化、已有凭据TLS与身份读取回归保持 | 无新增平台宣称 |

最后完整 `go test ./internal/agent/service ./cmd/agent -count=1 -timeout 120s` 通过：service15.017s、CLI1.344s；定向 `go vet ./internal/agent/service ./cmd/agent` 通过。此前完整service13.513s/CLI1.389s含详细-v，链接明确SKIP。

中间最后扩充permanent_corruption子例时曾有一次完整回归失败（service73.889s、CLI0.586s通过）：该子例在准备阶段32.75s返回resource_query_failed，尚未进入被测恢复；辅助清理又因r尚未创建而nil panic，锁未及时清理。修复测试清理guard并追加准备失败CLI调用诊断，生产代码没有变化；该子例隔离通过3.925s，随后上述最终全包15.017s通过。原准备失败的具体底层原因缺乏当时CLI明细，后续未重现，不将其表述为已确认外部环境问题，也不从最终记录隐去。

源码修复／本地回归不等于M1阶段或产品发布完成。详细交接`.cache/m1-agent-service-fix-report.md`；真实Linux、Docker精确版本矩阵、两节点、掉电耐久及完整双服务业务链限制沿原记录保留。
