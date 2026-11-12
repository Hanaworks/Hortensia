# Docker Manager

自托管的多节点 Docker 管理器，面向维护多台独立节点的运维人员。节点管理与迁移体验参考 PVE，Docker 资源及 Compose 操作参考 Portainer；由用户明确决定部署位置，以 Node → Stack／Docker 资源组织管理。

`docker-manager`是暂名。当前从M0技术原型继续推进M1首个完整流程：已完成本机TLS通信、许可门禁、两端持久任务与恢复扫描，并完成[双端终结释放的本地验收](docs/implementation/m0-dual-release.md)。账号／配置服务、HTTP入口、启动流程和Web正在实施，真实Agent执行及整套联调仍未完成，尚无可安装验收的Docker管理产品。首版范围、依赖和退出条件见[M1推进记录](docs/implementation/m1-first-version.md)。

## 主要能力与边界

- 在 Web 中创建、查看、编辑 Compose、环境及附属运行配置，保存和部署分别反馈，无需先 SSH 到服务器建文件。
- 管理内外网独立节点、Stack、容器、镜像、网络和卷，提供分组标签与多用户的节点／操作权限。
- Copy 建立来源不变的独立配置，Apply 部署明确版本；Node Cross 表示部署位置迁移，执行后验证，通过后由用户选择是否删除来源。
- 提供 docker run 转 Compose 的文本转换工具，结果可编辑并带入 Stack 流程，转换本身不执行命令或部署。

管理器遵循原生 Docker／Compose 行为，不擅自注入额外网络或卷；不提供镜像构建、Dockerfile 和构建上下文管理。节点独立，不做自动调度。通常约 20 节点作为设计和验证基线，不设置固定 20 节点上限；实际容量未验证。

希望迁移按需携带存储，前期允许手动搬运；具体存储机制仍暂缓。迁移切换、中断和回退策略也需要继续确定，不能把配置复制说成完整迁移。UI 布局与视觉随功能实现细化。

## 组成与技术状态

逻辑上由 Web、Manager 和节点 Agent 组成，Agent 主动连接 Manager 已经确认。Go Manager／Agent、Vue 3／TypeScript Web、两端独立 SQLite、Agent TLS WebSocket 已作为本轮技术工作选择继续落实；配置正文加密存库，Web 同源访问，首期轮询操作结果。v1 通信字段与关键行为已有工作规格，依赖版本及实际运行组合仍需原型验证，详见[方案取舍](docs/decisions.md#21-2026-11-04-的具体技术建议)。

当前优先考虑 Unix-like 下具备可用 Docker CLI／Compose 时的一致操作行为；Linux、FreeBSD 等环境意向不代表已经完成平台兼容验证。实际 Agent 环境、Docker 端点和文件路径组合会在实现前验证。

## 开始阅读

| 阅读任务 | 入口与主要内容 |
| --- | --- |
| 看这套文档如何安排 | [项目文档规划](docs/documentation-plan.md)：各篇职责、主骨架、参考、展开深度及检查记录 |
| 回看从文档到交付的教训 | [项目文档编写到实施交付的全过程复盘](docs/implementation-lessons.md)：资料阅读、规划与深度、阶段判断、实施验收及后续执行要求 |
| 审阅产品目标与范围 | [需求规格](docs/requirements.md)：R01—R17、Q01—Q11、术语、使用场景和边界 |
| 核对日常操作方式 | [功能与交互设计](docs/functional-design.md)：编辑部署、跨节点、权限和转换流程 |
| 理解整体及模块 | [架构设计](docs/architecture.md) → [Manager](docs/components/manager.md)、[Agent](docs/components/agent.md)、[Web](docs/components/web.md) |
| 查精确定义 | [数据设计](docs/data-design.md)、[接口规格](docs/api-design.md)：逻辑实体、操作、状态和失败边界 |
| 对接首条流程 | [HTTP 规格](docs/specifications/http-api.md)、[Agent 协议](docs/specifications/agent-protocol.md)、[协议 Schema](contracts/agent-v1.schema.json)：消息和返回对象、字段约束、关联及错误 |
| 核对已实现的具体行为 | [M0 实际实现规格](docs/specifications/m0-channel-probe.md)：实际报文、支持矩阵、参数、状态、错误及代码／测试对应 |
| 判断本轮做什么、怎样算完成 | [M0 实施与验收](docs/implementation/m0-agent-channel.md#本轮验收标准)：AC01—AC11 的通过条件、逐项结果与交接范围；当前通信及独立门禁切片已验收，整个M0仍未完成 |
| 跟进持久开始账本 | [账本实施规格](docs/specifications/m0-agent-ledger.md)与[实施计划／验收记录](docs/implementation/m0-agent-ledger.md)：编码前定义范围、存储、接口、故障行为与L01—L10 |
| 跟进事件与结果持久化 | [事件日志规格](docs/specifications/m0-agent-journal.md)与[实施计划／验收记录](docs/implementation/m0-agent-journal.md)：started、首结果、加密、确认和恢复读取，J01—J10已验收 |
| 跟进Manager持久协调 | [Manager实施规格](docs/specifications/m0-manager-operations.md)与[实施计划／验收记录](docs/implementation/m0-manager-operations.md)：固定输入、双占用、事务许可、回报幂等和乱序、结果／释放意图；M01—M10已验收，未接网络任务 |
| 跟进Agent完整账本扫描 | [扫描实施规格](docs/specifications/m0-agent-scan.md)与[实施计划／验收记录](docs/implementation/m0-agent-scan.md)：本地v3扫描切片已完成，覆盖持久冻结、固定成员／原文快照、分页、当前连接确认和完成回执；Agent扫描完成与Manager业务恢复分开 |
| 跟进Manager扫描接收与业务恢复 | [恢复实施规格](docs/specifications/m0-manager-recovery.md)与[实施计划／验收记录](docs/implementation/m0-manager-recovery.md)：显式新v2库、加密页面与case、无记录释放的历史依据、旧快照与新事件合并和当前完成回执屏障；R01—R12结果与限制在验收页维护 |
| 跟进双端终结释放 | [双端释放规格](docs/specifications/m0-dual-release.md)与[F01—F10验收](docs/implementation/m0-dual-release.md)：Agent v4／Manager v3真实持久回执、原子释放、迟到事件、当前轮认证及跨端进程恢复；执行结果仍为受控夹具 |
| 接手账本与事件模块维护 | [开始账本内部说明](docs/specifications/m0-agent-ledger.md)、[事件日志内部结构与流转](docs/specifications/m0-agent-journal.md#7-内部结构与职责)：职责、调用流程、字段来源和生命周期、异常定位及修改约束 |
| 按独立模块接手阅读 | [模块阅读与补全范围](docs/documentation-plan.md#131-模块阅读与补全范围)：通信各内部单元、账本／事件及Manager／Agent／Web的说明入口；共同定义、本地定义和转换处理分别落实 |
| 查看实际原型与源码参考 | [M0 实施记录](docs/implementation/m0-agent-channel.md)：运行方式、测试和限制；[Portainer 源码对照](docs/research/portainer-agent-reference.md)：固定版本、调用链与采用理由 |
| 实现关键执行机制 | [执行与恢复](docs/specifications/execution-recovery.md)：预检、资源归属、投递、结果确认和恢复边界 |
| 安排实现与验证 | [开发实施计划](docs/development-plan.md)、[测试计划](docs/test-plan.md)：切片依赖、工程接入和可观察断言 |
| 准备安装和维护 | [部署与运行准备](docs/operations.md)：网络与权限、备份恢复、升级与故障，尚非可执行安装教程 |
| 查决定与剩余问题 | [决策与待办](docs/decisions.md)：用户确认、设计提案、阻塞范围及解决时点 |

文档以 req42／arc42 为统一骨架，模块、数据、接口、测试和运行继续承接需求与设计。2026-11-04 的继续落实覆盖首条流程的 HTTP 返回对象、20 类 Agent 消息、共享字段约束、执行结局和归属、双端释放及恢复核对；上一轮内容中的相关规则同步更新。现行稿用于按同一约定开展实现与原型，不代表产品或故障恢复已经验证；迁移策略和后续能力按阶段展开。文档／契约检查与软件测试分别报告。

## 继续开发

2026-11-12继续推进首个可试用版本，当前按M1的登录／节点接入 → Web保存配置 → 固定版本部署 → 查看结果组织工作；具体范围、模块依赖、V01—V08退出条件和文档独立阅读验证见[首版推进记录](docs/implementation/m1-first-version.md)。当前正在实现[双端释放](docs/specifications/m0-dual-release.md)，随后继续正式服务、真实执行、界面和运行交付；完成这一库模块不等于首版完成。

可在仓库根运行 `go test ./... -count=1` 复验当前原型，其中包含真正两个进程的回环 TLS 握手；`pwsh -File tools/check-contracts.ps1` 和 `node tools/check-docs.mjs` 继续检查静态契约与文档。Go 版本、手工运行命令和范围见 M0 实施记录。

开始账本、事件日志、Manager本地协调和Agent本地恢复扫描已有独立验收；本轮实现Manager扫描页持久接收、变化合并、业务核对及完成回执屏障，范围和证据见[恢复规格](docs/specifications/m0-manager-recovery.md)及[验收记录](docs/implementation/m0-manager-recovery.md)。只有可信all扫描证明无记录且本库从未授权／观察的任务，才走取消释放例外；有Agent记录的终态仍保留pending和占用，未知、矛盾及暂不支持的历史保持隔离。下一依赖是双端finalize／finalized释放和受控任务通道接线，网络探针仍冻结；认领／轮换、原生Compose及节点接入 → Web保存配置 → 部署 → 查看结果仍需后续落实。

Agent保留v1开始账本／v2日志，扫描使用显式新v3库；Manager保留v1协调入口，恢复使用显式新v2库，均无旧库升级工具。身份、会话和执行结果仍有受控夹具边界；页面收齐、Agent扫描完成、Manager业务就绪与当前回执提交是不同事实，不能据此宣称整个节点恢复或产品可发布。本轮使用本机临时数据库与辅助进程，无需用户提供真实部署环境；进入真实Docker或跨机验证前会先明确联调环境需求。首个增量交接见[实施计划](docs/development-plan.md#4-m1-工程落实与任务依赖)。每个模块执行[开工与完成流程](docs/development-plan.md#61-每轮实施的开工与完成条件)，仓库工作约定同时保存在AGENTS.md。

新决定记录于 decisions，再同步需求、设计、接口和测试。已有历史材料中的助手建议不会因为再次引用而变成用户已确认决定。正式名称、许可证、发布方式及首版完整范围仍需确定。

## 历史资料

[2026-11-04 初稿压缩包](archive/2026-11-04-initial-draft.zip)保留继续完善前的 14 篇文档原文，并附原始讨论和历史依据、逐文件校验清单；[压缩包 SHA-256](archive/2026-11-04-initial-draft.zip.sha256)用于核对包本身。解压后从 `snapshot/README.md` 开始阅读，包内旧稿不随工作目录更新。

[原始对话](docs/ChatGPT-Docker管理程序推荐-20261030-0119.md)与[早期讨论摘要](docs/conversation-notes.md)保留不变。后续产品决定的直接来源见 [11 月会话记录](archive/2026-11-03-session-deliverables/conversation/conversation.md)。

[2026-11-01 归档](archive/2026-11-01-documentation/README.md)和[2026-11-03 归档](archive/2026-11-03-session-deliverables/README.md)为历史快照，其“当前”“完成”等表述只对应当时。现行入口为本 README，归档中的设计稿不是额外的一套有效规格。
