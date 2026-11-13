# 已归档：development-progress

归档日期2026-11-14；来源：docs/development-plan.md。各切片当时进度及下一依赖由M1现状替代。 本文保留历史正文，旧状态不作为当前事实。

现行替代：[docs/implementation/m1-first-version.md](../../../docs/implementation/m1-first-version.md)。原始完整文件及字节校验见[归档目录](../README.md)。

---

### 2026-11-07各切片进度（历史原文）

**保留范围与替代入口**：从下段至“整体架构”段之前，保存2026-11-07依次完成各切片时的原始进度及依赖。“当前”“下一模块”“下一依赖”“用户当前无需提供部署环境”只指对应历史切片；这些状态表述已由本节现行进展及[首版记录](../../../docs/implementation/m1-first-version.md)替代。保留原证据和未验证边界，不据后续实现回写为当时已完成。

当前完成初稿留存、M1 设计深化及 M0 的首个通信／许可门禁实验；2026-11-06 补建[切片验收标准](../m0-implementation/m0-agent-channel.md#本轮验收标准)，2026-11-07 按预先保存的补齐计划完成AC01—AC11逐项验证与审阅，代码／测试基线为`94056e0`。当前切片可供技术交接；M0 尚未整体完成，M1—M5 尚未实现。已有 Portainer 固定版本[源码对照](../../../docs/research/portainer-agent-reference.md)，其结论与本项目实测分别记录。每个增量完成时记录版本、已实现功能、适用环境、测试证据和剩余限制，更新 README 与实际使用入口。

2026-11-07继续完成[Agent开始账本切片](../m0-implementation/m0-agent-ledger.md)：开工前保存实施规格与L01—L10，源码／测试版本d572f84实现SQLite固定输入／占用、Gate持久提交、重复拒绝及三个进程终止边界；L01—L10通过，整个M0仍未完成。当时确定先落实事件／结果与恢复记录，再接入受控任务通道；不跳过Manager授权、真实准备及结果核对依赖。

同日完成[Agent事件日志切片](../m0-implementation/m0-agent-journal.md)：编码前以5c0c1f7保存规格和J01—J10，源码／测试版本8ff708b；加密started与首result、同位冲突隔离、确认和单操作恢复读取通过验收，包含实际结果事务内强杀后的重开验证。显式新v2库不升级v1；结果仍由受控夹具提供，确认不释放占用。下一模块先落实Manager持久操作／事件接收及会话／核对约束，再接入受控任务交互；网络通道继续冻结。

随后完成[Manager本地持久协调切片](../m0-implementation/m0-manager-operations.md)：基线3bf8629、源码／测试c02e1fd，M01—M10通过；独立Manager v1库保存加密输入、双占用、事务许可及首事件／结果，包含真实SQL失败与进程终止重开。身份／配置／ready仍由Fixture提供，节点重连或Open后只补事实，不任意解冻；终态保留pending和claims。下一依赖改为完整核对扫描／会话恢复、受控任务通道及双端释放，真实Compose验证前再明确联调环境需求。

2026-11-07随后完成Agent本地恢复扫描切片，按[扫描规格](../../../docs/specifications/m0-agent-scan.md)及[实施记录](../m0-implementation/m0-agent-scan.md)交接：显式新v3库持久保存冻结、扫描成员／变化集合和最近完成回执，当前boot的有界原文快照负责分页与期限。Agent完成只结束本轮扫描屏障，不删除claims、不恢复旧Gate，也不证明Manager业务核对结束。下一步先实现Manager扫描页持久接收、变化合并与业务恢复判定，再接受控网络任务及双端释放。本轮只使用本机临时数据库和辅助进程，无需用户提供真实部署环境；旧库升级、备份回退和掉电耐久均未实现或验证。

同日继续实现[Manager扫描接收与业务恢复](../../../docs/specifications/m0-manager-recovery.md)，事前规格与验收基线为`72bc30a`，本轮实现及R01—R12证据见[验收记录](../m0-implementation/m0-manager-recovery.md)。显式新v2库保存加密页面、请求链、无记录释放的本地历史依据和隔离证据，并兼容Agent同boot旧快照与较新Manager事件合并；只有完整all缺席且从未授权／观察的任务可以取消释放，普通终态仍保留pending及claims。页面收齐、业务就绪和当前completed提交分别核对，quarantined不因换轮或重开消失，未知及暂不支持的历史持续隔离。

当前下一依赖为双端finalize／finalized释放及受控任务通道接线，并继续落实认领／轮换、真实准备和Compose执行。Manager v1／v2及Agent v1／v2／v3入口各自保留，无旧库升级或备份回退恢复入口。用户当前无需提供部署环境；进入真实Docker、Compose或跨机验证前再说明所需组合及联调范围。本地库组合与进程重开实验不代表完整M0、网络恢复或掉电耐久已经完成。
