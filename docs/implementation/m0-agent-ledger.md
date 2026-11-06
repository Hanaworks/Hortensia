# M0 Agent 开始账本实施计划与记录

> 执行要求：按规格逐任务实施，先写有判别力的测试并观察失败，再实现、审阅和记录；任务执行与最终审阅分开。不清理现有托管工作区。

日期：2026-11-07。起点：`d53de6d`。本计划和[实施规格](../specifications/m0-agent-ledger.md)先于本模块代码形成，分别由`a6f0253`与开工补充`782a4e5`保存。源码／测试交付版本：`d572f84`。状态：本模块L01—L10验收通过，可供下一模块技术交接；整个M0未完成。

**Goal:** 交付可独立验证的 SQLite 开始账本及既有 Gate 适配，证明先持久提交、最多一次启动及重启冻结。

**Architecture:** 保留冻结通道，以受控 prepare 夹具登记稳定输入、两类占用；真实事务提交 START_COMMITTED 后才允许无副作用 Start 回调。每次打开形成新 boot，旧事实只读且不能恢复执行。

**Tech Stack:** Go1.27、CGO关闭、modernc.org/sqlite v1.61.1 / libc v1.79.0、SQLite3.54.1、本地文件、现有 protocol 与 startgate。

**Spec:** [M0 Agent 开始账本实施规格](../specifications/m0-agent-ledger.md)，验收编号 L01—L10。

## 共同约束与审阅重点

- 遵循开发计划的每模块开工／完成流程；技术选择及异常先写清，再实现。当前继续推进已获用户授权，不另加每步审批。
- 单库固定 node/ledger/key，双摘要，稳定六元组、项目与执行配置；原始正文不入库。
- WAL/FULL/外键/5000 每连接有效，BEGIN IMMEDIATE、sql.Tx、CAS与唯一约束；零内部执行重试。
- Gate 的nil提交仅代表此次首次持久提交，已有记录不能转换成新的启动资格。
- 关注旧boot、提交结果不确定、部分占用、准备内容微变、提交后取消／到期；不得只测正常关闭重开。
- 当前Windows证据不外推Linux、掉电、真实Compose或整个M0；无任务广播、结果及释放入口。

## Task 1：存储、身份、固定输入与占用（L01—L04）

文件：新增 `internal/agent/ledger/store.go`、`records.go`、`migrations/001_start_ledger.sql`、对应 `_test.go`；修改 go.mod/go.sum。

接口：Create/Open、Identity/Close、RecordReadyFixture/Get/Claims，以及规格定义的 Identity、Record、Claim 和固定错误。供Task2消费 Store 内部db、boot和查询逻辑。

- [x] 写Create/Open和原始输入/占用测试，确认缺失实现造成失败。
- [x] 固定依赖，安装版本1 DDL，落实每连接参数与校验、身份密钥配对、原始字节指纹和事务登记。
- [x] 补错身份、缺库、损坏、较新schema、同操作冲突、并发占用及回滚测试；运行本包对应测试。
- [x] 独立核对规格与实现，记录偏差并修正。

## Task 2：Gate 与持久开始边界（L05—L07、L09）

文件：新增 `internal/agent/ledger/gate.go`、`gate_test.go`、`fault_test.go`；必要时最小更新startgate注释，不改变既有协议行为。

接口：NewGate(ctx, startgate.Request, now, start)；内部commitStart(ctx, Permit)。沿用Task1固定数据和事务帮助，不新增网络处理。

- [x] 先写合法开始、重复、第二Gate构造、错上下文和旧boot冻结测试并确认失败。
- [x] 实现每操作单Gate、事务复核、CAS和完整许可提交；nil只用于本次首次提交。
- [x] 用提交回调屏障和真实SQLite写锁／事务错误验证取消、失效、期限及提交失败；禁止产品代码增设测试故障开关。
- [x] 独立审阅启动资格、锁顺序、关闭和错误路径，修正后复验。

## Task 3：进程终止与重开（L08）

文件：新增 `internal/agent/ledger/restart_test.go`，复用本包受控输入。

- [x] 使用测试二进制辅助进程，在三个明确阶段输出同步标记；父进程收到标记才强制终止并等待回收。
- [x] 父进程重开真实库，验证完整事实、两占用、旧boot冻结及Start累计计数；不使用正常Close替代强制终止。
- [x] 辅助进程与超时清理不得遗留；记录进程实验不能证明掉电或旧备份检测。

## Task 4：整体验收、文档与交付（L10）

- [x] 独立审阅新增模块及接线，处理重要发现；核对L01—L10逐项证据。
- [x] 运行 `go test ./... -count=1 -timeout=120s`、`go vet ./...`、`go build ./...`、`go mod verify`，以及契约／文档／Git空白检查。
- [x] 更新数据、Agent、决策、测试、开发计划与README的当前状态及本轮入口；主定义只增加本次实际事实，历史记录保留。
- [x] 本表登记版本、环境、结果与未验证范围；保存本地Git提交后交接，不发布产品。

## 验收记录

开工时L01—L10均未执行；下面是按事先条件完成后的记录。测试名称仅作定位，结论来自实际断言和对应运行结果。

| 编号 | 结果 | 直接证据与结论边界 |
| --- | --- | --- |
| L01 | 通过 | [store_test.go](../../internal/agent/ledger/store_test.go)中的CreateOpenIdentityAndConnectionSettings核对身份不变、boot改变、SQLite3.54.1、WAL/FULL/外键/5000及替换物理连接；UnsafePhysicalConnection拒绝错误参数。Create不覆盖原库 |
| L02 | 通过 | OpenRejectsMissingCorruptionAndIdentityMismatch、RejectsSchemaAndInvalidCreationWithoutRepair、FailedCreationRetainsFile验证缺库／错身份与密钥／损坏／版本拒绝及不修复；MissingSchemaConstraints、UnknownSchemaObjects拒绝约束丢失和额外对象；AcrossCheckoutLineEndings验证LF/CRLF兼容 |
| L03 | 通过 | [records_test.go](../../internal/agent/ledger/records_test.go)中的PersistsRawFingerprintClaimsAndReplay、RejectsChangedFixedInputs、AcceptsMathematicalIntegerButPreservesBytes验证原字节HMAC、同输入重放、各固定值冲突；InvalidInputAndNode、DoesNotPersistConfigurationOrKey验证输入拒绝与正文／密钥不入库 |
| L04 | 通过 | ConcurrentClaimsAreAtomic使用两个真实Store竞争Stack或端点项目，只有一条完整操作和两项占用；DifferentEndpointAndFrozenBoot验证不同端点项目可独立登记；StatementFailureRollsBackBothClaims验证第二项占用失败不留半条数据 |
| L05 | 通过 | [gate_test.go](../../internal/agent/ledger/gate_test.go)中的CommitPrecedesStartAndReplayCannotStartAgain在Start内读取完整许可；变reply_to重放不再启动，变permit/auth_rev不覆盖；ExistingCommitAndConcurrentConstructionCannotGrantSecondGate拒绝已有事实和第二Gate；[fault_test.go](../../internal/agent/ledger/fault_test.go)的ConcurrentDurableCASHasOneFirstCommit验证8个并发提交仅1次返回首次成功 |
| L06 | 通过 | CancelOrInvalidateBeforeCommitKeepsReady验证提交前停止；CommitThenCancelInvalidateOrExpirePreservesFact在真实SQLite提交前／后设置回调屏障，验证Start=0而开始事实和占用保留。屏障是测试调度，不替换数据库或调用真实命令 |
| L07 | 通过 | RealWriterLockTimeoutAndContextCancellation验证第二连接真实写锁、约5秒超时及context取消；RealStatementAbortAndCommitFailure分别使用SQL中止及延迟外键在Commit时失败，验证回滚、固定诊断、门禁不确定且不能重建，移除故障或重开均不复活旧操作 |
| L08 | 通过 | [restart_test.go](../../internal/agent/ledger/restart_test.go)的ForcedProcessTermination覆盖before_commit／after_commit／after_start；父进程等待精确标记，再Process.Kill和Wait。重开核对ready或完整START_COMMITTED、两占用与旧boot冻结；只有after_start有1次落盘回调计数，重开不增加 |
| L09 | 通过 | FrozenReadyAndCommitted、WrongRequestContextAndInvalidIDs、InvalidPermitsBeforePersistence、RechecksClaimsAndPreparedContextInTransaction验证旧boot／上下文／字段／占用；CloseDuringCommitSuppressesStart及ConcurrentCloseWaitsForStartWithoutBlockingGet验证关闭后不启动、并发关闭等待和Start内查询不死锁；没有用持久UTC时间恢复期限 |
| L10 | 通过 | 最终全量五个测试包通过，663条测试及子测试通过记录，其中ledger126条，失败0、测试跳过0；vet、全包build、mod verify、契约与文档检查通过。存储专项及Gate／进程终止独立审阅均无未解决重要问题。通道代码未改，仍无任务／Docker接线 |

运行环境：Windows/amd64，Go1.27.0、CGO_ENABLED=0，本地临时文件SQLite3.54.1，PowerShell7.6.9。contracts为纯嵌入包，没有独立测试，其package级skip不计作跳过测试。测试及子测试计数包含父组与辅助进程测试入口，不等于663个独立场景。最终ledger包耗时8.051秒、TLS通道15.365秒，只记录本次运行，不作为性能承诺。

复验：在仓库根运行 `go test ./internal/agent/ledger -count=1 -timeout=90s`；完整复验为 `go test ./... -count=1 -timeout=120s`、`go vet ./...`、`go build ./...`、`go mod verify`、`pwsh -File tools/check-contracts.ps1` 和 `node tools/check-docs.mjs`。本轮20类消息、21正例、11反例和1个解码输入的契约检查通过。受限执行环境曾阻止本机TLS套接字连接，已在允许本机连接的运行环境完成最终全量复验；该环境失败没有被标成产品行为缺陷。依赖初次获取遇官方代理超时，随后只对当前进程使用goproxy.cn，未修改全局Go设置，模块完整性校验通过。

交付包含实际[包源码](../../internal/agent/ledger/store.go)、[固定输入和占用](../../internal/agent/ledger/records.go)、[Gate适配](../../internal/agent/ledger/gate.go)、[版本1DDL](../../internal/agent/ledger/migrations/001_start_ledger.sql)、测试及文档，没有新增面向部署的CLI。当前使用方式是复验及供下一模块引用内部包；Create/Open须由调用者提供身份和密钥，RecordReadyFixture明确只用于受控准备夹具。

未验证／未交付：实际Linux、CGO race、掉电或真实磁盘耗尽、旧备份回退与迁移、生产单实例、秘密文件／密钥备份轮换、真实工作区准备、Manager授权事务、事件及结果重报、任务网络处理、完整核对扫描、finalize双端释放及Docker执行。本轮没有释放方法，也没有把unknown改成成功或取消完成。下一模块先补事件／结果与恢复记录的实施规格和验收条件，再开放依赖这些事实的受控任务交互。

## 实施裁定与进度

开工裁定：本轮仅完成开始账本与Gate受控接入，暂不开放网络任务。理由是结果事件与核对链尚不完整；代价是当前只能从测试驱动验收，需要下一模块继续完成结果及恢复记录。

开工审阅补充（进入相关代码前）：L03接受Schema合法数学整数写法但按原字节指纹去重；L09明确开始字段类型校验、Close失效及等待语义；物理时间用Unix毫秒，密钥校验域分隔为真实0x00。由独立审阅发现并同步规格，不改变既有线上协议。

Task1审阅修正：仅检查schema版本、列名和quick_check不足以拒绝被重建而失去约束的表。先以真实ALTER/CREATE AS复现Open错误接受，再补规格：要求非SQLite内部对象与内嵌版本1的表／索引定义一致，额外触发器等对象拒绝。测试故障注入对象只存在于已打开的测试库，不作为可重新打开的正式schema。

Task1复审修正：本仓库启用Git自动换行，精确SQL比较应统一CRLF/LF，否则普通检出重新构建会误拒同版旧库。补回归后按相同定义的换行规范化比较，不放宽表约束。

实施顺序证据：最初先写存储、门禁及重启测试，观察尚无Store/NewGate/commitStart导致的编译失败；随后实现。两项结构审阅缺陷另有可运行的行为失败回归，修正后通过。首次Gate测试中两个变摘要夹具误用了原有全零摘要，已修正测试输入；未据此改动正确实现或放宽条件。

最终交接：规格与计划基线a6f0253／782a4e5，源码／测试d572f84，后续文档提交保存本页验收结论及入口同步。用户“每个模块走文档流程”的要求同时落实到开发计划与根目录AGENTS.md。交付文档复核纠正了开发计划页首仍只列通信原型的遗漏；最终文档检查24篇、293个本地链接、100个表格通过，Git空白检查通过，AGENTS.md的流程及引用另行核对。归档和需求正文未改。
