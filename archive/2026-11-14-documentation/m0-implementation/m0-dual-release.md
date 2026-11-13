> 已归档：2026-11-14。本文是已结束M0切片的实施／验收历史，正文中的执行要求和“当前／下一步”只适用于当时，不是新的工作指令。现行实现入口：[当前模块](../../../docs/specifications/m1-manager-runtime-store.md)；整体状态：[M1推进](../../../docs/implementation/m1-first-version.md)。原始字节见归档原文包。

# M0 双端释放实施及验收

2026-11-12编码前基线，起点656e355。状态：F01—F10已完成本地库切片验收，源码6f92eb3；统一回归包含协议修复374016e。[模块规格](../../../docs/specifications/m0-dual-release.md)是本轮依据，原共享协议不改字段。下方初始表保留事前未验证状态，最终结果另列。本模块完成后继续M1服务与真实执行，不将库切片视为用户要求的第一个版本。

## 顺序与分工

1. 保存规格与本页基线。Agent与Manager分别写行为失败测试，记录实际失败后实现；原版本测试保持原入口。
2. Agent任务独占internal/agent/ledger：新增004_release.sql、release.go及测试，修改Store格式选择、事件／claims读取和扫描投影／过滤，保留v1—v3行为。
3. Manager任务独占internal/manager/operations：新增003_release.sql、release.go及测试，修改v3格式选择、事件／释放证据读取、恢复消费与完成判定，保留v1—v2行为。
4. 主代理写跨端集成及故障／进程终止测试，不与两端任务改同一文件；独立review后记录修正，统一test/vet/build、契约和文档检查。补内部数据及维护入口，逐项验收并更新当前状态。

共同API按规格第2节；CreateRelease/OpenRelease参数与本端CreateRecovery/OpenRecovery完全一致。Agent Finalize(ctx,raw)返回完整finalized；Manager ReceiveFinalized(ctx,b,raw)返回error。两端ReadFinalized(ctx,id)返回原body json.RawMessage。内部命名各端负责，重要字段和事务变化先更新规格。测试中的可信身份、准备和结果仍用Fixture；真实执行后另验证归属发布。

| 编号 | 验收条件与观察 | 初始状态 |
| --- | --- | --- |
| F01 | 新版本显式创建／打开、旧版互拒、身份密钥DDL错误拒绝，旧库回归；新建／重开冻结 | 未验证 |
| F02 | not_started取消／失败、normal_exit零／非零结果计算相同终态；unknown／中断／不完整观察／归属不一致不能释放 | 未验证 |
| F03 | Agent保存可认证回执与删claims同事务；重复同事实重放不增额度、不重执行，错误输入／版本／终态不覆盖 | 未验证 |
| F04 | Manager必须有当前持久请求，旧session／boot／错reply_to拒绝；匹配回执与released／confirmed／删claims同事务 | 未验证 |
| F05 | Agent回执丢失Manager仍占用，重试收敛；Manager提交后重复回执不改变终态或误删新任务claims | 未验证 |
| F06 | pending仅省略已释放且全部事件确认的操作，all保留；原文／回执损坏不能藏在过滤外；固定旧页不变 | 未验证 |
| F07 | 扫描finalized历史能经重发finalize核对，较旧页不回退已释放事实；未知／矛盾／absence后有记录仍隔离；有记录的正常恢复可完成握手 | 未验证 |
| F08 | 释放后原事件重报可确认；normal_exit seq2已释放后首次迟到且兼容的真实started seq1可补齐并ack；异文不覆盖历史并隔离；准入仍尊重本地／持久隔离与恢复冻结 | 未验证 |
| F09 | 两端释放语句及Commit失败保留原claims，无成功响应；真实进程终止前／后重开保留对应证据及冻结 | 未验证 |
| F10 | 额度耗尽可读／重放旧回执，证据篡改拒绝；全量回归、代码审阅、规格与首次接手说明一致 | 未验证 |

## 运行与完成条件

本机Go／SQLite组合沿原模块，实际版本在验收时重读。命令：go test ./... -count=1、go vet ./...、go build ./...、go mod verify、pwsh -File tools/check-contracts.ps1、node tools/check-docs.mjs。故障测试断言原文、行和claims，不只检查error或退出码。所有F项有实际证据才标本地模块完成；暂缺环境或范围单列。

重点复核：释放与迟到事件竞争、旧快照跨session、pending过滤隐藏损坏证据、seal后最终事务重验、重复释放误删后续任务claims。当前不需要真实Docker环境；原生执行与首版交付阶段需要，届时明确要求。Kill不是掉电耐久，受控normal_exit不是Docker执行或归属已经验证。

## 2026-11-12最终验收与接手

规格先以02d15c7保存；迟到started规则以e4bd42d补足。实施期间先记录再修正：结果回调与扫描锁循环、本地提前PrepareFinalize不等于历史矛盾、当前轮确认需要独立认证MAC，以及pending预算只计算认证并过滤后的保留项。独立审阅指出后两项，失败用例分别观察到伪造accepted可开放恢复、被省略历史仍耗尽快照预算，修复后定向验证通过；二次独立静态审阅未发现直接阻塞项。

| 编号 | 最终结果 | 实际断言与定位 |
| --- | --- | --- |
| F01 | 通过 | 两端ExplicitVersions／VersionsAndStaleSession及OpenRejectsWrongIdentityKeyAndDDL；新旧入口互拒，身份／密钥／约束受损拒绝，重开及新建保持冻结 |
| F02 | 通过 | 两端TerminalMatrix：取消前未开始、其他未开始、零／非零退出按事实判定；unknown、中断、观察不完整和归属不一致保留占用 |
| F03 | 通过 | Agent AtomicReplayAndReopen、WrongFactsPreserveEvidence：回执可认证，两个claims与回执一起发布；同事实重放保留原文且不多耗包装额度；错事实隔离而不覆盖 |
| F04 | 通过 | Manager RejectsWrongReplyWithoutRelease、OldBindingDoesNotQuarantineCurrent、CurrentRoundRequiresAuthenticatedConfirmation、ConfirmationCannotMoveBetweenRequests：当前绑定／请求／轮次逐项核验，伪造accepted或跨请求复制MAC不能完成恢复 |
| F05 | 通过 | 跨端OldSnapshotAndLostResponseConverge及KeepsManagerConflictUntilReceiptAndNeverDeletesNewClaims：Agent释放但丢回复时Manager仍持两项claims；重试收敛；后续新任务占用不被旧回复删除 |
| F06 | 通过 | Agent PendingAllAckAndSnapshot、PendingBudgetCountsRetainedRecordsOnly、PendingAuthenticatesAcrossIdentityBatches：pending必须全部事件确认才省略，all保留；跨128条批次仍认证末批，省略历史损坏拒绝；旧页内容保持不变 |
| F07 | 通过 | Manager SupportsFinalizedPageWithoutFabricatingReceipt、MissingHistoryDependsOnScope及跨端ReconnectRequiresCurrentRoundConfirmation：旧页不能造新回执，重连／重开需要本轮确认；all缺历史隔离、pending合法缺席保留证据 |
| F08 | 通过 | 跨端ResultBeforeStartedCanFinishThenReceiveRealStarted与两端冲突测试：真实迟到started补齐后仍保持终态／released；异文、错误许可及持久隔离不放行；StartCallbackReportsResultDuringScan验证真实回调可在扫描期间落结果，无反向取锁死锁 |
| F09 | 通过 | 两端语句及Commit故障测试检查原文、行与claims原子性；Agent ForcedTermination及Manager跨端ForcedTermination在释放事务内／提交后真实终止辅助进程，重开分别保持未释放占用／已释放凭据，并先冻结 |
| F10 | 通过（本模块范围） | 额度耗尽历史重放／重确认、密文替换及MAC篡改测试通过；独立审阅关闭问题；完整回归和构建见下。未覆盖的真实执行与Linux环境明确列于限制 |

测试入口：[Agent释放行为](../../../internal/agent/ledger/release_test.go)、[Agent进程实验](../../../internal/agent/ledger/release_restart_test.go)、[Manager释放行为](../../../internal/manager/operations/release_test.go)、[当前轮确认](../../../internal/manager/operations/release_confirmation_test.go)、[跨端流转](../../../internal/manager/operations/dual_release_integration_test.go)及[Manager进程实验](../../../internal/manager/operations/dual_release_restart_test.go)。这些测试仍用可信身份／结果夹具，不能按名称推导Docker已运行。

本机环境Go1.27.0 windows/amd64、CGO=0、SQLite3.54.1及go.mod锁定依赖。两端单独回归560条测试／子测试通过、0失败、0跳过。首次全量出现协议exact_total_limit失败；经诊断证实合法大Base64在CPU竞争下可触发旧正则100ms超时，按[P01—P04修复记录](m0-protocol-base64.md)保存基线并修复，没有放宽输入限制。

最终从Git导出精确374016e源码到忽略的`.cache/release-baseline`运行，避免与正在开发的M1文件混合：`go test ./... -count=1 -json`为1117条测试／子测试通过、0失败、0跳过，6个测试包均通过；`go vet ./...`、`go build ./...`及`go mod verify`通过。JSON逐项日志在本机`.cache/m0-dual-release-final-verified.jsonl`，不纳入源码；接手者按上述版本与命令可复验。契约检查21个有效／11个无效样例及1个解码payload通过；文档链接／锚点／表格检查通过，这两项仅证明静态一致性。

交付范围：源码、受控本地释放与恢复协议及维护说明。暂缺：正式身份／网络任务接线、真实Compose及归属基线发布、后续结果补观察、旧库迁移、备份回退、人工解除隔离和掉电耐久。进程Kill不是断电。normal_exit来自Fixture，真实执行模块须把候选归属基线接入同一释放事务后才能宣称真实部署通过。下一步继续M1账号／配置／HTTP、Agent执行与Web；用户已说明测试环境及操作方式稍后安排。
