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
