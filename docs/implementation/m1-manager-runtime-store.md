# M1 Manager runtime Store v5 实施与验收

2026-11-12编码前保存。依据 [v5规格](../specifications/m1-manager-runtime-store.md)。环境：Windows本机Go/SQLite，受控Agent事件；真实Docker、Linux掉电及跨机未验证。

顺序：保存本基线→行为失败用例→显式格式及正式API→事件链与恢复释放→持久加密投递→模块回归与交接核对。不得以测试名或退出码代替业务断言。

| 编号 | 验收结果和观察方法 | 初始状态 |
| --- | --- | --- |
| MV01 | 新v5创建/重开，v4/v5互开拒绝；所有Fixture拒绝5；正式登录配置仍可用 | 未验证 |
| MV02 | hello身份匹配、welcome关联、替换session冻结；旧session不可投递/确认 | 未验证 |
| MV03 | prepare原payload只在恢复完成后投递；写成功不确认；退避后原文重试；SQLite/WAL无假秘密明文 | 未验证 |
| MV04 | started1+not_started2被保存，等待finalized才释放双claims | 未验证 |
| MV05 | normal_exit不完整→seq3完整，原进程事实不变；新先旧后最终latest正确，矛盾隔离 | 未验证 |
| MV06 | 多事件链经重开和恢复核验，latest/finalization/outbox真实关联；不能SQL伪造链或MAC | 未验证 |
| MV07 | 恢复未授权ready走正式取消；恢复期间不prepare，取消/finalize可投递；当前round屏障 | 未验证 |
| MV08 | 保留v1—v4全套operations回归；文档入口和限制清楚 | 未验证 |

完成条件：逐项追加真实通过/失败/未验证/阻塞，记录命令、版本及限制。本文件只验收Store源码切片。

## 2026-11-12本地实施结果（编码后追加）

代码基线为af43699之后本工作树未提交差异；Go1.27.0 windows/amd64，modernc SQLite3.54.1，临时本地SQLite WAL/FULL。此前基线段保留为编码前记录。身份/配置/Apply均走正式Manager5入口；测试Agent4仅提供受控扫描和恢复页面，进程结果正文为明确测试事实，不能作为Agent5真实执行证据。

| 编号 | 当前结果 | 可再次执行的证据／实际断言 |
| --- | --- | --- |
| MV01 | 通过（本地） | ExplicitFormatAndFixtureBoundary：显式5创建/重开、正式登录、v4打开v5拒绝；MV02另逐个检查9类Fixture拒绝 |
| MV02 | 通过（本地） | HelloIdentityAndAllFixtureRejection及DeliveryWaitIsNotBusinessConfirmation：异node hello不替换当前会话；welcome关联；旧session不可取件/写失败事实 |
| MV03 | 通过（本地） | DeliveryWaitIsNotBusinessConfirmation、EncryptedDeliveryAndTamperRefusal、RetryBackoffAndConcurrentTake：ready前意图仍pending；1/2/4/8/16/30等待、4并发仅1次取件、原文稳定；DB/WAL无payload_b64正文；改密文拒发 |
| MV04 | 通过（本地） | StartedThenNotSpawnedWaitsForFinalized：started1/not_started2→canceled/pending仍2claims；匹配finalized后恰好0claims |
| MV05 | 通过（本地） | OutOfOrderObservationChainAndImmutableProcess、ProcessRewriteAndForbiddenRepairAreIsolated、PendingPromotionPreservesExactBody：seq3先到不推进latest，2后晋升3且重开不丢；输出/exit/时间改写及interrupted/drift修复拒绝；原body空白不被重编码 |
| MV06 | 通过（本地） | FinalizationAuthenticatesLatestAndLateHistory、NewRoundReplacesDeliveryWithinSameSession：finalize关联seq3；MAC后迟到旧结果/started不造claims；改finalization seq拒读；同session新round换请求，旧round失败反馈拒绝 |
| MV07 | 通过（本地） | RecoveryCancellationAndDeliveryFreeze：恢复中不prepare；正式ProcessRecovery持久cancel且可投递；没有结果/回执不能完成，不伪造结果或释放claims |
| MV08 | 通过（本地） | 原1—4 operations全套回归；001—004 DDL无差异；文档检查55篇、716本地链接，只证明链接/锚点/表格/围栏 |

新增12个TestRuntime顶层用例，含5个禁止修复子用例。实际行为失败曾观察到：显式5入口未定义；同session换round仍取旧请求/无新请求。修复后新增组通过，最近`go test ./internal/manager/operations -run TestRuntime -count=1`为5.560秒。完整回归最近一次41.796秒通过，最终完整复验另见交接报告。一次新增测试把SQL字符串Scan到json.RawMessage导致测试失败，改用string后通过；不是持久数据格式迁移。

维护入口为[v5规格](../specifications/m1-manager-runtime-store.md)内部流转补充与[服务§13](../specifications/m1-manager-service.md#13-v5运行store接手补充2026-11-12)。尚未验证：真实Agent5监督结局、真实Docker、Linux权限/进程组、跨机、掉电耐久、M1整套发布；根接线负责人负责真实TLS正式组合及总体验收。README由根统一更新，避免将本切片写成首版完成。

2026-11-12独立审阅与接线补充：MV06/MV07首次审阅发现已released历史在新all扫描中的当前轮确认死锁，新增ReleasedHistoryGetsCurrentRoundFinalize先复现waiting且Delivery=nil，再补当前all成员历史finalize候选，仅在本round尚无真实accepted请求时复用PrepareFinalize发送；双Store回归完成ready/complete，不放宽MAC。MV03新增ExpiredObservationStopsOldQueryUntilNewContext先因cursor_expired拒绝而失败，随后固定失败码/旧context停投/新round新query通过。现在14个TestRuntime顶层用例，新增组7.387秒通过；审阅修复前最终完整回归33.132秒通过，修复后最终回归另追加。结构检查55文档/721链接/186表通过。真实双v5正式路径另由独立审阅探针复验，不与本篇受控Agent4事实混写。

最终完整复验：`go test ./internal/manager/operations -count=1`修复后44.258秒通过；`node tools/check-docs.mjs`55文档/721链接/186表通过；本模块`git diff --check`通过（仅环境CRLF提示）。独立双v5审阅复验结果由审阅者另报，生产源码现冻结。

## Manager封闭副本检查：2026-11-13实施前基线

先保存本节及[模块详细规格](../specifications/m1-manager-runtime-store.md#v5封闭副本检查2026-11-13实施前基线)，再行为RED、实现及定向回归。本轮仅operations及这两篇模块文档，不接服务、backup、依赖或任何节点。共享snapshotcheck由根负责；缺helper导致的编译失败不当行为RED。此前源码95d453f／文档e291a44；当前并行工作树的精确文件、环境、命令、RED／最终证据存`.cache/m1-manager-snapshot-inspection-report.md`。

| 编号 | 预先确定的结果与观察 | 初始结果 |
| --- | --- | --- |
| MI01 | 真实Manager5封闭副本成功，29/9计数及规范根确定；主文件hash、文件集合及boot/session/flags/quota/last_seen不变，原服务仍可写 | 未验证 |
| MI02 | 错identity／钥／check／schema版本／DDL／约束／外键／单例／未知表拒绝，零报告及固定错误；用尽quota仍可读 | 未验证 |
| MI03 | 九密文表逐表破坏非首行或历史行cipher/wrap/fingerprint/AAD均拒绝；所有旧config/session/delivery/round/pending纳入检查 | 未验证 |
| MI04 | accepted、authorized无started、unknown、blocked、pending和扫描中合法；撤权／禁用／撤销／过期／旧session不被当前资格拒绝；缺permit、错投影／claims／outbox拒绝 | 未验证 |
| MI05 | 原文InputDigest／Schema、完整历史事件链／固定进程事实／原版本包关联验证；坏seq/原文/版本引用拒绝，不补造started/result | 未验证 |
| MI06 | 所有历史扫描／成员／请求／case验证；允许半页前缀、expired、abandoned、未知成员待处理和quarantine先提交；伪造水位／cursor／成员拒绝 | 未验证 |
| MI07 | 真实normal与absence释放验证，全部accepted finalize request MAC含非最新旧round；缺回执／假accepted／错claims拒绝；历史未接受且后续blocked意图可读 | 未验证 |
| MI08 | 所有config链／head／receipt／credential与session普通元数据验证；缺失HTTP preimage不假称补证；普通列或潜伏行改变根 | 未验证 |
| MI09 | 同schema测试构造新KEK／ID／DEK／uses仅重加密根相同，原HMAC钥、明文和其他metadata变化被拒绝或根不同；不声称已有恢复工具 | 未验证 |
| MI10 | 旁文件／非私有路径／链接由shared helper拒绝；取消／长度／行解码预算中止无部分报告，无明文或原始SQL错误，资源全部关闭 | 未验证 |

完成只表示这组本机Inspector切片适用项有证据，不代表MB08/09整体、完整组、恢复、Linux权限、真实Docker或M1完成。根负责共享helper检查及完整组后续基线。回归仅本模块定向测试和operations全包，不执行全仓./...；需要环境升级的Go测试按当前权限机制运行。

2026-11-13修复前MI06补充：自查已正式观察认证且Schema合法的unknown_operation/page_conflict附错round被错误接受（1.146s）。补充case→原页成员原文关系；page_conflict则核对case的原scan_id而不是要求争议页内容一致，并补真实不同scan_id冲突正例。原正常Store写入语义不变，修复后须重跑Inspector组及operations全包。

2026-11-13独立审阅修复前MI04/MI10补充：原审阅probe实证seq3 pending→seq2 complete已提交未晋升被误拒；1024额外64KiB claims在拒绝前Go累计分配约97.5MB。先保存模块修订和正式复现，再移除pending晋升门槛、在读取claims前做固定结果SQL计数／预期ID集合验证；原probe修后重跑，内存probe仅打印数值不单独作内存契约PASS。正式测试另断言对照差值有界，SQL驱动缓存开销不假称硬进程堆上限。

## Manager副本检查本地实施结果（2026-11-13编码后）

入口已实现；调用独立snapshotcheck.Reader，精确v5校验、29表普通数据与9表全部历史密文、原binding、业务图与完整keyed语义根均在一个只读Tx内完成，再由Finish检查关闭／文件不变。旧Store启动逻辑没有skipStartup或Inspector绕过分支；原schema wrapper保留，查询核心改为querier。全部错误返回零报告，未注册正常业务DB或生成boot。

| 编号 | 本地结果 | 最终可观察依据 |
| --- | --- | --- |
| MI01 | 通过 | 真实SQLite复制、29/9计数、两次根一致、主文件原字节不变且无旁文件；原服务仍可RecordNodeSeen |
| MI02 | 通过 | 错身份／两钥／keycheck／版本／DDL／索引／FK／quota-low拒绝；quota满仍可读 |
| MI03 | 通过 | 九类真实保留密文各cipher/wrap/fingerprint/key_id共36负例拒绝，含非head Config和撤销session |
| MI04 | 通过 | accepted／authorized／unknown／blocked／pending历史接受；两种前史已提交尚未晋升窗口；正式禁用实际owner及撤销实际node后仍可读 |
| MI05 | 通过 | 多result原事实链／原文输入HMAC／Config关联，认证成功但Schema或固定输入错误仍拒绝 |
| MI06 | 通过 | collecting、未知成员未处理／case已处理、abandoned及真实不同scan_id争议页可读；case附错round拒绝 |
| MI07 | 通过 | 真实normal和历史all-absence释放，非指针accepted MAC逐条验证，后续blocked的旧未接受意图仍可读 |
| MI08 | 通过（已有事实范围） | 普通metadata根变化、130+行与末行损坏、Config／receipt／credential／session图；缺失HTTP preimage或历史竞争正文不补证 |
| MI09 | 通过（受控构造） | 九类密文新KEK/ID/DEK/nonce及增加uses后完整根／计数相等，不表示已交付转换工具 |
| MI10 | 本机适用项通过；专门故障未验证 | 取消、33MiB行预算和异常1024×64KiB claims先SQL拒绝；专门关闭故障、实际大历史超时、Linux权限仍未验证 |

Go1.27.0 windows/amd64、modernc v1.61.1／SQLite3.54.1；真实Manager5+受控Agent4扫描夹具，无真实Docker或节点连接。最终生产冻结版本`go test -json ./internal/manager/operations -count=1`退出0，48.680s，154顶层／227子例通过，无失败或跳过；其中Inspector16顶层／52子例。之后只补正式禁用owner／撤销node的第17个Inspector测试，独立通过（包1.101s），生产未变。`go vet ./internal/manager/operations`通过；文档检查64篇／779链接／213表通过，仅静态格式证据。

独立review发现的pending事务窗口与claims异常集合分配均先RED再修，原独立overlay复验通过；reviewer另复验9.345s，claims约17.86MB（原97.54MB），两项CLOSED。正式内存负例断言同受控输入累计分配小于32MiB，不声称普遍堆上限。精确命令、行为失败／夹具错误区别、逐项结果、最终生产SHA及JSON证据见`.cache/m1-manager-snapshot-inspection-report.md`和`.cache/m1-manager-snapshot-operations-test.jsonl`；独立报告为`.cache/m1-manager-snapshot-inspection-review.md`。

本地Inspector源码切片可交接；完整备份组、materials锁／清单／CLI、隔离恢复／正式rekey、schema6／protocol2及MB08/09整体仍未完成。没有运行全仓./...、Git或现有环境操作。
