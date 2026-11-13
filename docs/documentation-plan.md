# Docker Manager 项目文档规划

更新日期：2026-11-14。本文维护现行文档的职责、阅读路径和持续方法；历史写作批次／原型进度已迁入[文档过程归档](../archive/2026-11-14-documentation/history/documentation-progress.md)。原稿及来源保留于[归档目录](../archive/2026-11-14-documentation/README.md)。软件现状只在[M1推进](implementation/m1-first-version.md)及模块验收记录判定，不再用早期“无实现”或“待决定”概括当前工程。

## 1. 项目依据和有效状态

| 材料 | 现行用途及边界 |
| --- | --- |
| [原始产品讨论](ChatGPT-Docker管理程序推荐-20261030-0119.md)、[早期摘要](conversation-notes.md)及[11月会话原话](../archive/2026-11-03-session-deliverables/conversation/conversation.md) | 原始输入保留；用户后续明确确认／调整优先，历史助手建议不自动构成需求 |
| [需求规格](requirements.md)、[决策与待办](decisions.md) | 产品范围、稳定需求编号、确认依据和真正未决项；参考资料不擅自扩大功能或流程 |
| [开发计划](development-plan.md)、[M1推进](implementation/m1-first-version.md) | 当前实施依赖、阶段退出条件及实际完成范围；源码切片、阶段和发布分别判定 |
| [历史归档](../archive/2026-11-14-documentation/README.md) | 追溯原方案、阶段记录和验证证据；不作为另一套现行规范 |

Go／Vue／SQLite／主动TLS通道已有工作实现，但具体技术选择不等于用户逐项批准，实测组合不外推全平台。尚未实现的正式产品能力继续作为待实施设计维护，不因还没实现而自动作废。

## 2. 统一组织与主要维护位置

需求以req42组织目标、范围、质量、路线和风险；架构以arc42组织上下文、构件、运行、部署与共同概念。模块规格按独立职责说明内部协作和数据，不机械复制整个项目骨架。

| 阅读任务 | 现行入口 |
| --- | --- |
| 产品目标、行为、范围 | [需求](requirements.md)、[功能与交互](functional-design.md)、[决定](decisions.md) |
| 全局组成与模块关系 | [架构](architecture.md)、[Manager](components/manager.md)、[Agent](components/agent.md)、[Web](components/web.md) |
| 逻辑数据与实际存储 | [数据设计](data-design.md)提供逻辑实体和存储主定义导航；私有字段、DDL、事务及生命周期在所属M1模块规格 |
| 跨边界操作和编码 | [接口总览](api-design.md)、[HTTP规格](specifications/http-api.md)、[Agent协议](specifications/agent-protocol.md)、[Schema](../contracts/agent-v1.schema.json) |
| 做到哪里、按什么验收 | [开发计划](development-plan.md)、[M1推进](implementation/m1-first-version.md)、[测试计划](test-plan.md)及各模块实施记录 |
| 实际启停、接入、故障 | [Manager首次运行](manager-first-run.md)、[Agent接入](agent-first-run.md)、[运行准备](operations.md)；未实现工具不写成可执行命令 |
| 当前教训与持续约束 | [全过程复盘](implementation-lessons.md)、[开工与完成流程](development-plan.md#61-每轮实施的开工与完成条件) |

## 3. 正文的内容深度与边界

README给当前状态及任务导向入口，不把已结束M0验收混入当前工作清单。需求、功能设计及组件保留完整产品目标，同时区分M1范围和后续能力；不能把设计清单当实现清单。

每篇实施规格说明目的／边界／依赖、输入输出、实际接口和内部结构、关键调用及数据流、正常异常／状态／事务、初始化／关闭／恢复、故障定位和维护规则。重要内部对象定义其含义、来源、约束和生命周期；公共字段引用主定义，不再造冲突字典，也不逐行翻译代码。

实施记录说明范围、顺序、稳定验收编号、观察方法、环境、完成条件和限制；最终逐项写通过／失败／未验证／阻塞及版本、证据。模块设计与实现形成时间保留，事后补充不追认为事前已有。

## 4. 参考资料的使用

req42、arc42、C4用于组织和表达；[Portainer固定源码对照](research/portainer-agent-reference.md)及用户提出的PVE体验用于补足理解。它们不接管本项目需求，不自动产生额外审批、限制或范围。技术选型记录所读版本、调用链和采用依据；具体依赖版本以锁文件及相应实施规格核对。

原稿中的详细写作参考及来源表保存在原文归档，仍可追溯；其外部产品行为不作为本项目运行证据。需要变更当前行为时先对应现行需求和影响范围，不从参考差异推演必须由用户裁定的新问题。

## 5. 编写、实施与交接

遵循[开发计划§6.1](development-plan.md#61-每轮实施的开工与完成条件)及[小型程序尺度](development-plan.md#62-模块按小型程序组织规格)：先保存规格和验收，再实现；不足先修订基线，交付前按最终代码检查。普通技术取舍写明依据后推进，已授权工作不机械重复请求批准。

从接手者视角核对是否可追踪入口到落库／返回、理解保存原因及异常判断、找到修改入口和相应验证。实际执行首条流程与一个失败场景才可关闭独立阅读验收；作者解释、链接检查或文件数量不能代替该验证。

## 6. 过时内容、归档与检查

整篇不再有效的方案／已结束实施记录移入archive，注明日期、原因、来源、替代入口；旧路径必要时仅留兼容跳转。现行文档局部失效则抽出旧正文并替换当前内容，不把失效算法和当前规则并列维护。原始需求、参考原件、失败证据保留其输入／历史性质，不因未采用而作废。

归档前保留原文和摘要，调整可读副本导航时记录原始与迁移后的字节校验。更新目录、链接和必要锚点；检查归档可读副本，而不修改已封存的旧证据包。只有仍被现行实现引用的底层规则／旧格式API继续保留在规格目录，并说明作用域。

`node tools/check-docs.mjs`检查现行本地链接、锚点、表格和围栏；归档另查本地引用和原文摘要，Git差异检查排除误改源码。软件测试是否需要重跑由实际代码／行为变化决定，不把文档整理计作新增软件通过。

## 7. 模块阅读与维护入口

| 相对独立职责 | 现行定义及维护重点 |
| --- | --- |
| 输入与协议校验 | [实际输入约束](specifications/m1-protocol-input.md)、[共享Schema](../contracts/agent-v1.schema.json)；不可信字节如何成为受约束对象 |
| 正式通道／探活 | [Agent wire](specifications/m1-agentwire.md)；握手、方向／会话、重复、读写、期限和关闭 |
| Manager投递与恢复接线 | [任务通道](specifications/m1-manager-agenttransport.md)、[Store v5](specifications/m1-manager-runtime-store.md)；绑定、持久意图、重试、恢复及事务权威 |
| Agent进程／凭据／任务协作 | [Agent服务](specifications/m1-agent-service.md)、[Agent v5](specifications/m1-agent-runtime.md)；收件、调度、准备、开始、退出、结果、恢复及释放 |
| 工作区／Compose／监督器 | [执行模块](specifications/m1-compose-engine.md)；冻结原字节、预检、实际副作用、输出预算与保守观察 |
| Manager管理内核／HTTP／运行 | [服务](specifications/m1-manager-service.md)、[HTTP适配](specifications/m1-http-adapter.md)、[进程](specifications/m1-manager-runtime.md)；权限／数据、请求边界、文件和资源生命周期 |
| Web | [Web规格](specifications/m1-web.md)；节点上下文、草稿／版本、提交回执、失权和迟到响应 |
| 备份与检查 | [快照](specifications/m1-backup-snapshot.md)、[完整组](specifications/m1-backup-group.md)、[两端检查及恢复设计](specifications/m1-maintenance-backup.md)；库完成与工具／恢复待实现分开 |

M0的[开始账本](specifications/m0-agent-ledger.md)、[事件日志](specifications/m0-agent-journal.md)、[扫描](specifications/m0-agent-scan.md)、[Manager协调](specifications/m0-manager-operations.md)、[Manager恢复](specifications/m0-manager-recovery.md)、[双端释放](specifications/m0-dual-release.md)及[通信探针](specifications/m0-channel-probe.md)继续作为被明确引用的底层／旧格式定义；它们的已结束验收记录已[归档](../archive/2026-11-14-documentation/README.md)，不再列作当前计划。

<a id="3-文档安排与内容深度"></a>
<a id="31-项目入口-readmemd"></a>
<a id="32-需求规格-requirementsmd"></a>
<a id="33-功能与交互设计-functional-designmd"></a>
<a id="34-架构设计-architecturemd"></a>
<a id="35-manager-模块设计-componentsmanagermd"></a>
<a id="36-agent-模块设计-componentsagentmd"></a>
<a id="37-web-模块与交互状态设计-componentswebmd"></a>
<a id="38-数据设计-data-designmd"></a>
<a id="39-接口规格-api-designmd"></a>
<a id="310-测试计划-test-planmd"></a>
<a id="311-开发实施计划-development-planmd"></a>
<a id="312-部署与运行准备-operationsmd"></a>
<a id="313-决策与待办-decisionsmd"></a>
<a id="4-按篇使用的参考资料"></a>
<a id="5-编写顺序和方案复核"></a>
<a id="6-检查交付与后续展开"></a>
<a id="本轮检查记录"></a>
<a id="7-2026-11-04-深化安排"></a>
<a id="71-深化交付与检查记录"></a>
<a id="8-2026-11-04-继续落实通信与执行规格"></a>
<a id="81-交付与检查记录"></a>
<a id="9-portainer-对照与首个-m0-原型"></a>
<a id="10-按模块先定规格再实施"></a>
<a id="11-agent-事件日志的规格实现与验收"></a>
<a id="12-模块规格面向接手与维护"></a>
<a id="13-按独立模块检查说明完整性"></a>
<a id="131-模块阅读与补全范围"></a>
<a id="132-本轮完成核对"></a>
<a id="14-manager-本地持久协调实施"></a>
<a id="15-agent-本地恢复扫描实施"></a>
<a id="16-manager-扫描接收与业务恢复实施"></a>

历史标题定位：原写作／阶段／物理方案正文已[归档](../archive/2026-11-14-documentation/history/documentation-progress.md)；以上旧定位仅承接历史书签，不恢复旧规则。
