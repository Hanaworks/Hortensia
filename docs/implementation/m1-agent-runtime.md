# M1 Agent运行与Compose实施及验收

2026-11-12事前基线。尚未实施，下面全部软件验收为未验证。本计划服务[首版M1](m1-first-version.md)工作包B／E及部分G，依据[Agent运行规格](../specifications/m1-agent-runtime.md)、[执行恢复](../specifications/execution-recovery.md)和[协议](../specifications/agent-protocol.md)。根完成规格review后按拥有的文件分配实现，不在同一生产文件上并行写入。

**目标：** Agent持久身份接入正式Manager，固定输入经真实准备和受监督Compose形成结果与双端释放，重启及断线不盲目执行。

**结构：** Manager继续同一operations.Store；正式传输替换冻结探针的任务职责。Agent v5账本持有执行与归属事实，service负责串行调度，workspace／compose／executor负责实际IO和监督。

**技术：** 现有Go、coder/websocket、modernc SQLite、协议Schema及startgate；新增成熟YAML解析依赖固定版本。首轮运行Linux＋同机Unix socket；Windows本地受控验证及Linux交叉构建不等于真实部署。

## 1. 先于实现的修订与Review重点

本轮读源码确认v4仅首结果、最多seq2，缺preparing和归属，ProcessRecovery还调用拒绝v4的Fixture取消；这不是把既有测试结果抹去，而是明确它们不能证明生产接线。Agent与Manager显式新v5格式的选择、started后当前进程证明未spawn的not_started seq2、补观察多结果及outbox发送元数据先由根同步相关主规格，再编码。没有隐式旧库迁移，没有通过重建空库解除unknown。

复核最容易遗漏的输入／故障：认领响应丢失但候选仍未active；取消与开始提交竞争；同boot断线后进程结局落库；AST alias／merge藏文件引用；观察失败但已知正常退出后的新seq和旧seq迟到；已finalized后矛盾事件重新隔离。下列任务和AR验收分别覆盖。

## 2. 顺序、文件及接口产物

### A. v5执行与协调格式

文件：Agent `internal/agent/ledger/runtime_store.go`、`runtime_prepare.go`、`runtime_results.go`、`runtime_ownership.go`、`migrations/005_runtime.sql`及现有gate／scan／release格式分支；Manager `internal/manager/operations/runtime_store.go`、`delivery.go`、`migrations/005_runtime.sql`及现有event／recovery／release分支。两端可独立负责人，改动各自目录；Manager服务／运行规格由根维护。

产物接口为规格§3.2、§4.1；不得直接添加Fixture包装器。稳定输入、准备证据、进程事实、补观察新seq和候选归属都从同一个Store事务维护。

- [ ] 先写AR01／03／04／05／10／11／12／13的失败测试，断言claims、持久原body、事件位置、发送次数及基线变化。
- [ ] 实现显式新格式；旧入口拒绝新格式，v5拒绝Fixture入口；为v4保留原验收行为。
- [ ] 实现生产收件／准备／取消／进程事实／补观察及归属原子发布；Manager同步多结果判定和正式恢复取消。
- [ ] 实现持久投递取件及失败重试；业务事实提交才确认意图，当前round／session不匹配拒绝。
- [ ] 运行`go test ./internal/agent/ledger ./internal/manager/operations -count=1`，核对新增故障断言而非仅退出码。

### B. 工作区、原生适配和监督器

文件：`internal/agent/workspace/{workspace,secure_linux}.go`、`internal/agent/compose/{preflight,references,command,observation,ownership}.go`、`internal/agent/executor/{executor,process_linux,output}.go`及对应测试；非Linux提供明确unsupported执行路径。该任务可与A并行，但只消费已冻结payload和规格定义的证据类型。

接口：Materialize／Verify→Package；Prepare→PreparationEvidence；Invocation→Command；Start→Process；Wait→ProcessOutcome，详细前提见规格§5—6。不直接释放claims、不把任意成功JSON作为结果。

- [ ] 先写AR06—09／14的拒绝、字节、调用计数、信号和资源基线测试；错误引用的首个CLI调用计数必须为0。
- [ ] 锁定成熟YAML AST及离线schema／dotenv解析依赖，记录许可证；做小型夹具确认alias／merge与原生模型边界，不重写Compose合并引擎。
- [ ] 实现私有工作区原子发布、外部修改拒绝及受控运行环境。
- [ ] 实现config／up参数和受监督进程；真实Linux进程组测试与Windows替身测试分别记录。
- [ ] 实现只读全项目采集、归属比对、候选基线与补观察缓存；普通查询无active基线写入。
- [ ] 运行`go test ./internal/agent/workspace ./internal/agent/compose ./internal/agent/executor -count=1`并检查原文件字节／启动调用／释放副作用断言。

### C. 身份、通道和服务接线

文件：`internal/agentwire/`、`internal/manager/agenttransport/`、`internal/agent/service/`、`cmd/agent/`；根修改Manager service启动／关闭，将handler注入既有HTTP适配器。已有internal/channel与cmd/channelprobe继续作为原型，不替换其测试含义。

- [ ] 先写AR02／04／05／12／15测试：候选响应丢失、第二进程争锁、旧session迟到、扫描期间变化、消息关联及关闭。
- [ ] 实现init／enroll／rotate／serve，候选先耐久后联网；不输出token／原文。
- [ ] 实现共享WS读写、探活、方向／关联和队列预算；Manager绑定经正式认证hello，Agent保存当前welcome并冻结。
- [ ] 接恢复驱动、Store投递、ready/request_start、started/result/ACK、cancel、observe和finalize/finalized；每操作串行区不阻塞网络读循环。
- [ ] 组合正式服务、临时TLS和明确测试执行器跑完固定输入→释放；再重开双方查询。检查代码真实路径不调用Fixture。
- [ ] 运行`go test ./internal/agentwire ./internal/manager/agenttransport ./internal/agent/service ./cmd/agent -count=1`及`go test ./... -count=1`，构建Manager／Agent实际命令；Linux交叉构建结果单独记为构建证据。

### D. 真实环境与交接

- [ ] 等用户提供已承诺的环境操作安排；此之前不主动连接、不重复索要详情。记录实际Linux／Docker／Compose、端点及Agent/Manager版本。
- [ ] 使用专用测试项目与假秘密在第一节点执行AR16原生对照及关键崩溃边界，第二节点验证相同项目名与权限隔离；不清理未知业务资源。
- [ ] 根更新README、组件／运行主文档和M1状态；提供实际产物运行步骤，区分本地与真实环境证据。
- [ ] 独立阅读者按文档复验正常流程和一次断线／结果重报；记录口头补充点并修正文档，不能以链接检查替代首版V08。

## 3. 稳定验收AR01—AR16

状态初始均为“未验证”；实施者填写通过／失败／未验证／阻塞和代码版本／证据，不凭拟写测试名记通过。受控测试中的计数来自真实接口边界记录，不能由预设结果宣称执行已发生。

| 编号 | 输入、故障与动作 | 通过条件和观察方法 | 当前结果／环境 |
| --- | --- | --- | --- |
| AR01 格式与身份 | init、重开、旧格式、错钥／缺钥、部分初始化，第二实例打开同状态根 | ledger／端点身份稳定；旧库不迁移；缺材料不补造；第二进程被OS锁拒绝；原文件未覆盖 | 未验证；本机／Linux权限部分分列 |
| AR02 候选凭据 | enroll或rotate提交后丢HTTP响应，进程重开再试；服务端撤销secret | 同一候选恢复同一node/ledger；轮换仅递增一次；401停止重试；stdout／stderr无secret；两端身份及代次核对 | 未验证；TLS正式Store组合 |
| AR03 收件与准备 | 同operation相同／不同原payload；物化前终止；ready写失败 | preparing＋原文＋双claims原子；不同payload不覆盖；未成功预检不ready；没有许可不up；重启不续旧准备开始 | 未验证；数据库／工作区 |
| AR04 许可及取消 | 撤权／cancel／断线与开始提交竞争；迟到许可；START_COMMITTED后未spawn | 旧nonce不再生；最终检查后只启动一次；当前存活未spawn可写seq2 not_started；重开同记录保持unknown；claims等待真实释放 | 未验证；门禁及进程边界 |
| AR05 当前会话 | 旧hello身份、旧session事件／ACK／finalized、连接替换、方向错误、65536ID预算 | 非当前连接不能确认或授权；新绑定恢复前不受理；超限受控重连不遗忘去重；读／写队列不无限增长 | 未验证；真实回环TLS |
| AR06 工作区安全 | 穿越、链接、重解析点、额外文件、大小／摘要不符、半发布、后改字节 | 包外未读写；未完整发布不ready；既有版本不覆盖；启动前漂移拒绝；原始多文件字节及顺序保留 | 未验证；文件系统，Linux目录同步另列 |
| AR07 YAML及引用 | alias／merge藏绝对env_file；循环alias；深度／节点超预算；hooks/include/provider；插值路径；被override覆盖的不安全引用 | 首次CLI调用为0；固定诊断与源位置，无配置秘密；合法alias和多文件最终合并仍由原生config决定 | 未验证；解析库＋受控CLI |
| AR08 命令及环境 | Compose包含build、注入控制env、特殊字符项目、覆盖文件、缺image、external依赖不存在 | 无shell；实际参数up -d语义及--no-build正确；无额外删除／构建；与预检环境／文件完全一致；控制键拒绝 | 未验证；参数记录＋Linux原生对照 |
| AR09 监督与输出 | 分块UTF-8、非法字节、stdout/stderr大量输出、TERM超时、deadline及关闭；中断后exit0 | 输出≤1MiB且字符完整、排空不死锁；子进程回收；固定中断原因；中断exit0仍held；普通日志无输出 | 未验证；本机输出／Linux进程组 |
| AR10 断线和重开 | up已开始后断WS；保存正常结局前／后强杀Agent；只读观察失败 | 同boot继续保存／重报原结局；重开未知不up；已持久normal_exit允许只读补观察；真实启动次数始终1 | 未验证；辅助进程故障 |
| AR11 归属发布 | 首次同名外部项目、外部同名换ID、正常自身重建、非零退出部分变更、query更新 | 首次碰撞拒绝；外部替换不导入；合法pre/post映射保留；ACK不改active；finalize事务同时发布基线和释放双claims | 未验证；资源证据夹具／真实Docker |
| AR12 恢复投递 | Manager重开；pending缺席升级all；未授权ready取消；扫描期间新result；complete丢失 | 无Fixture调用；当前round取消可执行；等待变化ACK及双端释放才完成；旧complete不解新冻结；write成功不confirmed | 未验证；正式双Store＋TLS |
| AR13 补观察事件链 | 正常退出观察不完整，query重复／分页／过期；新result先到、旧result晚到 | 同query仅一次采集／一次新seq；原退出码／finished_at／输出不变；Manager接受新seq且旧结果不覆盖；interrupted/drift拒绝补观察收敛 | 未验证；双端v5 |
| AR14 观察完整性 | list/inspect中对象消失、查询失败、超预算、无健康检查、孤儿容器 | 失败不空成功、不假complete；健康none区分unknown；不删孤儿；无关节点／项目不进入baseline | 未验证；适配测试／原生Docker |
| AR15 释放耐久 | result、ACK、finalize、finalized逐一丢失；归属发布／释放提交前后终止；提交失败 | 保留原结果重报、无第二up；Agent回执只在事务后；Manager收到匹配回执才释放；失败无半发布；迟到矛盾重新隔离 | 未验证；双进程组合 |
| AR16 真实首条流程 | 正式Web／HTTP固定版本→Linux节点原生对照→双方重启→第二节点同名隔离 | 实际Docker资源／参数／账本／Web状态互相对应；正常、非零、断线／取消边界分别记录；不把进程重启等同掉电耐久 | 未验证；等待用户环境安排 |

## 4. 证据、完成条件和交接

每次验证记录代码SHA或未提交差异、Go与依赖版本、系统及CLI组合、用例输入摘要、故障位置、双方提交顺序、实际命令启动计数、资源ID与claims／event／baseline的观察。不记录真实秘密全文；假秘密用来核验日志／DB明文泄漏边界。对跨进程故障保留可再次执行的测试入口，普通测试成功不能代表磁盘掉电耐久。

本地源码切片完成需A—C实际程序可构建、AR01—15适用本地项有证据且回归通过，剩余Linux部分明确未验证。整个Agent真实运行模块完成还需AR16及Linux相关AR项实际通过。首版交付仍依赖Manager/Web、备份／隔离恢复、O12和独立阅读验收；不把本篇完成写成M1完成。

文档核对命令为`node tools/check-docs.mjs`和`pwsh -File tools/check-contracts.ps1`；它们只检查引用／契约。本计划没有执行这些软件验收或连接用户节点，后续实施记录应追加真实日期，保留本段事前基线身份。

## 5. Agent v5账本本地实施记录（2026-11-12，实施后补充）

本节只记录任务A的Agent侧，保留本文开头事前基线身份。源码基线`af43699a947d569aff9a2b581cd2436c6a4b51ca`加未提交账本差异；Windows amd64、Go 1.27.0、CGO_ENABLED=0、bundled SQLite 3.54.1。没有连接用户节点，没有运行真实Docker。`runtime_test.go`的19个顶层用例及子用例从正式CreateRuntime/BindManagerSession/ReceivePrepare/Gate接口建立链路；没有调用Fixture后改表冒充生产链。SQL触发器只作明确事务失败注入。

| AR范围 | Agent侧本地观察与证据 | 当前判定及剩余依赖 |
| --- | --- | --- |
| AR01 | RuntimeFormatKeysProfileAndFixturesRemainSeparated／FormatDoesNotAcceptFixtures：v5显式入口、v4双向拒绝、两钥错配拒绝、v5全部Fixture拒绝、端点Engine变更拒绝 | 账本本地通过；身份文件、第二实例OS锁与Linux权限由服务模块验证 |
| AR03 | PreparingIsAtomicAndInputCannotChange／PrepareAndReadyFailuresLeaveNoHalfState：原payload密文及双claims、不同字节隔离、收件／ready写入触发失败无半状态，额度不退还；ReadyReplayDoesNotSpendMoreEncryptionBudget：重复原字节及证据在额度耗尽后仍可读取 | 账本本地通过；真实工作区IO由B验证 |
| AR04 | OnlyLiveStartFailureCanProduceNotStartedSequenceTwo／ExpiredAfterCommitUsesLiveNoSpawnEvidence／UncertainCommitCannotCreateNoSpawnResult：started1后本boot未spawn可写not_started2；重开not_attempted仍unknown；最终10秒失效真实Start计数0；提交失败及随后取消不造未启动证据 | 账本本地通过；网络许可竞争／真实Cmd.Start由组合验证 |
| AR10 | DisconnectKeepsProcessTruthAndObservationNewSequence／RestartSavedExitCanReportButUnspawnedCommitStaysUnknown：断线不阻止同boot退出事实及结果；已保存退出可重开只读观察；无退出不附着旧PID；启动回调计数1 | 账本本地通过；强杀真实Agent和Docker端点故障未验证 |
| AR11 | FinalizeBaselineAndClaimsAreOneTransaction／FirstProjectCollisionCannotBeAdopted／OwnershipReplacementAndExternalDrift：首次碰撞拒绝，明确替换映射才纳入候选，正常非零退出可形成基线；ACK不发布；finalize删除claims失败回滚baseline及receipt；成功后revision递增、重开保留，未解释替换held | 账本本地通过；资源来自明确夹具，真实Docker采集未验证 |
| AR12 | ObservationAdmissionAndScanChanges：扫描期间新增seq3，当前会话未ACK时拒绝complete；ACK后才完成；RestartCannotResumePreparingButCanCancel：重开不续准备，当前恢复取消可保存seq1未启动 | Agent账本本地子集通过；正式Manager v5／TLS投递与双方恢复未验证 |
| AR13 | 补观察元数据在采集前持久登记，同query不重复采集／seq、同条件重放、改变limit拒绝、每操作一个进行中查询、5分钟过期；原退出码／finished_at／输出逐字节保持；interrupted及drift不接受自动补观察 | 元数据及事件链本地通过；页面快照／分页接口未实现，整项未完成；Manager迟到旧seq验收另记 |
| AR15 | 事务故障无半发布、回执重放、重开读取、释放后矛盾进程事实持久隔离；Intermediate interrupted exit0保持双claims | Agent账本本地子集通过；真实双进程逐消息丢失／崩溃和Manager释放仍待组合验证 |

最后验证：`go test ./internal/agent/ledger -count=1`（包含保留v1—v4验收及19个v5顶层用例）通过；`go vet ./internal/agent/ledger`通过。具体输出与审阅暂缺项见`.cache/agent-runtime-ledger-report.md`。没有运行race验证（本环境CGO关闭），没有把进程重开当作掉电耐久。接手者仍须复核Store跨端事件链及真实service调度，不能由本节宣布A—C或首版完成。

## 6. 第一轮账本审阅修复基线（2026-11-12，修复前）

2026-11-12 AS-R01对应追加基线（新增API编码前）：AR10／AR15增加PendingProcessOutcomeRefs有界枚举；覆盖同boot及重开已保存退出但无result、已有result排除、缺退出事实排除、错误limit拒绝、process密文／owner boot／started或claims不符时报错。服务组合另验证临时结果写失败重试及重开自动补结果、不重复up；原AR10仅账本直接调用通过不能证明服务有恢复入口。此新增项尚未验证，后续追加真实证据。

同日账本API结果：三个正式顶层用例及五种损坏子例先在空返回stub上RED；实现后同boot／重开均能枚举原退出，补写result后不再枚举，无退出不虚构；非法limit及密文、owner_boot、spawn_state、claims、started损坏拒绝。定向组GREEN（1.092秒）；中途首次实现引用不存在的校验helper导致编译失败，随后复用现有requireStartedEvent纠正，未把该失败省略或算通过。服务自动入口及完整账本回归另记。

二次独立复验发现原枚举准入可由直接observe绕过：修改process_facts的owner_boot或spawn_state后，枚举拒绝，但公开ReadProcessOutcome／RecordProcessOutcome仍可生成result。AR10／AR15追加负例与修订，先确认读取及最终事务两个入口RED，再统一可信退出读取；不把前轮28.146秒整包通过当作此边界已覆盖。当前未修复项保持失败，证据在`.cache/m1-agent-service-rereview-negative-evidence.md`。

同日二次修复后：枚举、公开读取、首结果／补观察最终事务、结果重放及历史认证共用同一快照的readProcessOutcome。EveryOutcomePath两个元数据反例正式先RED后GREEN；重开、断线、正常归属终结等定向组通过1.839秒，完整Agent账本包通过28.206秒。原独立observe旁路两个反例及无退出反例GREEN（2.748秒），实际服务恢复组合GREEN（13.730秒）；独立报告关闭AS-R01—04。无result仍要求started1和双claims，有result／已终结不要求claims重新出现；原事实、输出和序号不变。更早“未修复”是事前记录，由此段修复结果更新当前状态。

来源`.cache/m1-runtime-ledgers-review.md`的P2-2／P2-3；范围只修Agent归属发布读认证与取消校验顺序，详见运行规格§4.4，不扩大生产API或Schema。

- AR11／AR15：将独立未finalize候选指针反例提升为正式回归；增加active无指针、provisional带指针、revision／seq／跨候选指针、回执破坏及claims仍持有的负例。原正常发布、失败回滚、重放和重开必须仍通过。
- AR04／AR05：将错误digest cancel不改变Gate的反例提升正式回归，并验证随后合法许可仍只启动一次；旧session／缺operation／读取失败无副作用；真实cancel与开始竞争、Gate等待期间可读Store、后续写失败不伪造结果或释放，重试可收敛。
- 先确认上述回归RED，再修生产代码，定向及整个ledger包复验；独立复核由原review负责，作者通过不替代独立结论。证据写`.cache/agent-runtime-ledger-fix-report.md`，不提交Git、不连接真实节点。

### 6.1 作者修复结果（2026-11-12，实施后补充）

原两个独立失败探针已提升为`runtime_review_test.go`中的正式回归，先观察到同样RED：未finalize consistent候选被读成active、错误digest cancel将Gate改成canceled。修复读路径后，扩展负例还确认仅认证单个旧receipt无法防指针＋revision回退，也不能认证真实drift状态；按§4.4追加基线后，改为同项目连续、无分叉的认证candidate/finalized修订头，以及未释放操作的完整事件链／drift候选派生status。所有历史与当前指针、seq、revision、目标、finalized和claims都来自同一快照。没有新增Schema或线上字段。

取消现在通过recovery串行锁保护当前会话，先只读事务验证session／operation／digest／事件与claims，关闭事务后等待Gate，再于正式事务重新核对。被拒绝的请求没有Gate副作用；真实取消若后续写失败，保持Gate停止和claims，重试生成同一持久未启动事实。禁止Gate.Start回调重入CancelExecution，避免等待自身；Get／Claims等只读操作不受取消持有SQLite阻塞。

| 验收／正式测试 | 本地观察与结果 |
| --- | --- |
| AR11／AR15：ReviewUnfinalizedCandidateCannotBecomeActive、ReviewBindingReferentsAndStatusAreAuthenticated、ReviewPublishedPointerCannotRollBackToOlderCandidate、ReviewKnownDriftCannotBeHiddenByStatusMetadata | 未finalize、active无指针、provisional带指针、错误revision／seq／Stack、缺失／损坏finalized、恢复claims、凭空漂移状态、旧合法发布回退、隐藏真实drift均拒绝；原FinalizeBaselineAndClaimsAreOneTransaction与OwnershipReplacementAndExternalDrift仍通过 |
| AR04／AR05：ReviewWrongDigestCancelMustNotChangeGate、ReviewRejectedCancelHasNoGateSideEffects | 错误digest、旧session、缺operation／claim及数据库读取失败均不改变waiting Gate；错误digest后同一合法许可重放只启动一次，事件未被错误取消污染 |
| AR04／AR15：ReviewCancelWriteFailureKeepsStoppedClaimsAndRetries | 取消结果写入故障不制造事件／释放，Gate保持canceled、claims为2；存储恢复后重试收敛seq1并返回同一body |
| AR04：ReviewCancelDoesNotHoldTransactionWhileWaitingForStart、ReviewCancelRacesStartWithoutDuplicateExecution | 等待已进入Start时回调仍可读取Store；8组真实并发开始／取消各自只有未启动或一次启动＋interrupted两类结果，无第二次启动，保留双claims；提交成功标记短暂未到达时允许保守ErrEvidence并在原调用完成后重试 |

本节是作者本地证据，不替代原review的独立复验。历史认证读成本随同一项目已发布操作数／事件数增长；当前选择完整历史核对，未做规模性能验收。没有扩普通query、Fixture或自动接管，也没有连接真实Docker／Linux节点。最终命令输出、限制和交接见`.cache/agent-runtime-ledger-fix-report.md`。
## 7. Agent只读快照检查器实施基线（2026-11-13，编码前追加）

范围与算法见[运行规格§8](../specifications/m1-agent-runtime.md#8-agent-v5-封闭快照检查器2026-11-13实施前基线)及MB§7.3。先保存本表，再编写RED测试和实现；本轮只使用本机真实SQLite，不连接节点、不执行Docker，不声明完整备份组或恢复完成。

| 编号 | 条件、刺激与可观察结果 | 初始状态 |
| --- | --- | --- |
| AI01 | 空/有历史v5副本正确身份两钥通过；错身份/钥、旧版本、变更DDL/多表/trigger拒绝，失败报告为零 | 待实施 |
| AI02 | 成功/失败/取消前后主文件hash、boot/session/profile/uses不变，无旁文件；输入仅封闭私有单文件 | 待实施 |
| AI03 | 三密文表全部kind、历史ACK/finalized/旧候选及尾行逐一认证；坏cipher/wrap/key_id/fingerprint和孤立文档拒绝，计数含零行 | 待实施 |
| AI04 | 原payload/本地HMAC/metadata、私有JSON和准备绑定损坏拒绝，原payload字节和文件顺序不改 | 待实施 |
| AI05 | preparing/ready、started未登记spawn、created未知、durable exit无首结果、not_started2、unknown/interrupted空Post及合法隔离均通过且不执行 | 待实施 |
| AI06 | phase/process owner/spawn/pid/cancel/started/事件缺口/过程与历史结果不一致拒绝；首退出无result仍通过 | 待实施 |
| AI07 | 多发布修订和profile更新保留历史；缺修订/分叉/旧合法指针回退/伪active或drifted拒绝 | 待实施 |
| AI08 | ACK仍held；finalized为可信terminal且零claims；非法未知释放/缺claim拒绝，合法quarantine不被解除 | 待实施 |
| AI09 | 原扫描捕获在ACK/追加结果/finalize/换boot后仍认证；改成员字段/指纹/组计数拒绝；旧completed容许后来任务 | 待实施 |
| AI10 | 历史/过期查询和变化集全部保留；孤儿/错误event/query关联及无query后续结果拒绝 | 待实施 |
| AI11 | 受控同schema全行新KEK重封装计数/语义根相同；业务元数据或原文改变则根变化；quota满仍可读，低于sealed数拒绝 | 待实施 |
| AI12 | 16MiB正文/32MiB行超限为ErrInspectionLimit，取消/期限及SQL故障不泄secret/SQL/路径、不给部分根 | 待实施 |

实施顺序：共享reader可用前先验证本包可编译stub的行为RED（不将缺helper编译失败记RED）；随后schema/身份/枚举及fingerprint、逐操作、归属、扫描/查询、定向回归。Windows本机适用项由实际输出记录；Linux、真实节点、掉电和完整恢复继续未验证。报告保存`.cache/m1-agent-snapshot-inspection-report.md`，逐项报告通过/失败/未验证，不能由命令成功代替上述条件。

### 7.1 作者本机结果（2026-11-13，实施后追加）

源码为本轮未提交工作区；工具为Go1.27.0 windows/amd64、modernc.org/sqlite v1.61.1/SQLite3.54.1。先保存§8及AI表，再以可编译stub观察到空库/错身份两钥/取消行为RED；后续控制行和profile矛盾用例又真实暴露三个缺失验证并修复。一次新增测试缺少import导致的编译失败不计RED；历史扫描空subtest生成含`#`的临时路径曾被正确拒绝，改用合法测试名后验证历史算法，不放宽路径规则。

最终`go test -p 1 ./internal/agent/ledger -count=1 -json`通过：157个顶层、289个子例，0失败/0跳过，29.620s；新增检查器13个顶层、61个子例。`go vet ./internal/agent/ledger`退出0。完整原始记录保存`.cache/m1-agent-snapshot-inspection-tests.jsonl`，详细交接和限制保存`.cache/m1-agent-snapshot-inspection-report.md`。没有运行全仓Go测试、Git、真实节点或Docker。

| AI | 当前结果与具体证据 | 限制 |
| --- | --- | --- |
| AI01 | 通过本机适用项：EmptyAndNoMutation/EmptyUnboundProfile、WrongKeysAndCanceled、RejectsLegacyAndSidecar、额外表/trigger拒绝；精确DDL沿既有共用验证器 | v4作为旧格式代表，不宣称支持旧格式转换 |
| AI02 | 通过：重复检查主文件字节摘要不变、无旁文件，旧boot重开后仍不更新事实；封闭/别名/hash由共享Reader承接 | 不承诺同用户ABA；Linux权限仍待环境 |
| AI03 | 通过：三密文表每kind及两条发布历史的首/尾损坏拒绝；旧seq2坏密文、孤立文档、wrap/fingerprint/quota损坏拒绝 | 无新增密文表被自动忽略 |
| AI04 | 通过既有存储契约：原input metadata/HMAC、私有未知键/孤立prep、prepare完整协议校验；原文参与根 | 不把未保存或writer未保证的外部manifest关系补成新要求 |
| AI05 | 通过：LegalRuntimeHistory的13种窗口和空未绑定profile；含durable exit无result、not_started2、unknown/interrupted空Post、rev0失败终结及finalized后隔离 | 本机监督事实夹具，不是Docker证据 |
| AI06 | 通过：owner/spawn/pid/cancel/started删除/local指纹损坏及原事件链检查，原有账本包过程恢复回归全部通过 | 历史PID只作为数据，未附着或操作进程 |
| AI07 | 通过：两修订发布后profile升级仍认证旧prep，指针+revision回退和错误revision拒绝；正常归属回归未变 | 无大规模P²性能承诺 |
| AI08 | 通过：已finalized恢复claims拒绝、未finalized缺claim拒绝、合法quarantine不被解除；正常释放谓词继续共用 | 不新增人工释放能力 |
| AI09 | 通过：旧扫描在ACK/补观察/finalize/换boot后认证；成员指纹/ordinal/组/count/phase/ACK损坏分别拒绝 | 历史整组来源MAC原模型未保存，不能补证 |
| AI10 | 通过：过期NULL查询、已完成查询及冻结变化保留；错query seq、缺query拒绝，FK及事件全量认证 | 不运行旧query，不用当前期限否定历史 |
| AI11 | 通过：所有三类密文用测试专用新KEK/DEK封装，计数和根相等；created_at变化根不同；新uses满额可读，低于sealed数拒绝 | 测试夹具不等于恢复/rekey产品实现 |
| AI12 | 部分通过：正文16MiB+1、普通TEXT32MiB+1为ErrInspectionLimit；已取消/到期保留错误分类，错误不含注入秘密；共享Reader真实关闭/hash检查 | 未专门注入Agent层Close失败、内核卡死或大历史期限；不伪报这些故障通过 |

接手复核已按最终代码检查入口→单只读事务→全部表/密文→业务引用→Finish→报告流程；五个控制/身份singleton、历史claims/归属/扫描及quota均在同一快照。独立静态审查指出的同policy无prep遮蔽、32MiB SQL错误映射、同位冲突指纹、Finish内部ctx取消误分类及未冻结空session/receipt均已修正。当前为源码交接及Windows适用验证，AI12所列专门故障与Linux待验证，不宣布完整备份组、MB08/09、M1或发布完成。
