# Docker Manager

自托管的多节点 Docker 管理器，面向维护多台独立节点的运维人员。节点管理与迁移体验参考 PVE，Docker 资源及 Compose 操作参考 Portainer；由用户明确决定部署位置，以 Node → Stack／Docker 资源组织管理。

`docker-manager`是暂名。当前源码交接切片为`95d453f`：Manager／Web管理、正式Agent接入、持久任务通道及Compose执行程序已有实现，本机回归与独立复核已完成适用项。[Manager运行](docs/manager-first-run.md)和[Agent接入](docs/agent-first-run.md)分别说明入口。真实Docker接受表仍为空，生产部署能力尚未开放；完整备份恢复及受控维护继续实施，M1首个完整版本未交付。范围、证据和退出条件见[M1推进记录](docs/implementation/m1-first-version.md)。

## 主要能力与边界

以下为产品范围，实际实现及验证状态以本页开头和M1推进记录为准。

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

2026-11-12按登录／节点接入 → Web保存配置 → 固定版本部署 → 查看结果推进。正式身份、HTTPS管理服务和Web已实现；[Agent服务](docs/specifications/m1-agent-service.md)、[工作区／Compose](docs/specifications/m1-compose-engine.md)和[Manager任务通道](docs/specifications/m1-manager-agenttransport.md)已有实现与本机组合证据。实际Agent可认领、连接及完成空恢复；受控启动失败任务已通过许可、结果ACK、双端释放及重连核对。受控失败验证不代表真实Docker部署成功。

复验使用`go test -p 1 ./... -count=1`，其中包含真实TLS及辅助进程；Windows本机曾有进程启动阻塞，顺序验收保留原断言和业务时限，诊断见[运行验收](docs/implementation/m1-manager-runtime.md)。`pwsh -File tools/check-contracts.ps1`和`node tools/check-docs.mjs`只检查静态契约与文档。Web在其目录运行`npm test`、`npm run typecheck`和`npm run build`。

旧M0入口保持各自格式与证据；正式运行使用显式Agent5／Manager5和protocol1，没有隐式迁移。ACK不释放占用，正常双端finalize／finalized才释放并发布归属；观察不完整只补观察，真正未知、矛盾继续隔离。已知耐久退出但尚无结果时，恢复原结局而不重跑命令。

[维护与完整备份恢复](docs/specifications/m1-maintenance-backup.md)已有事前基线，软件尚未完整实现；[SQLite一致快照](docs/specifications/m1-backup-snapshot.md)只是底层切片。未来schema6／protocol2方案与现行v5分开，不能用规划中的命令操作当前程序。Linux、真实Docker版本矩阵、两节点隔离及最终独立阅读仍未验证。用户稍后安排环境，本地独立工作继续，不连接未知节点。

每模块遵守[开工与完成流程](docs/development-plan.md#61-每轮实施的开工与完成条件)及AGENTS.md：先规格／验收，再实现，最后记录证据与限制。首版范围已确认按M1完整流程；正式名称、许可证及公开发布方式仍未决定。技术选择和参考产品行为不表述成用户逐项确认。

## 历史资料

[2026-11-04 初稿压缩包](archive/2026-11-04-initial-draft.zip)保留继续完善前的 14 篇文档原文，并附原始讨论和历史依据、逐文件校验清单；[压缩包 SHA-256](archive/2026-11-04-initial-draft.zip.sha256)用于核对包本身。解压后从 `snapshot/README.md` 开始阅读，包内旧稿不随工作目录更新。

[原始对话](docs/ChatGPT-Docker管理程序推荐-20261030-0119.md)与[早期讨论摘要](docs/conversation-notes.md)保留不变。后续产品决定的直接来源见 [11 月会话记录](archive/2026-11-03-session-deliverables/conversation/conversation.md)。

[2026-11-01 归档](archive/2026-11-01-documentation/README.md)和[2026-11-03 归档](archive/2026-11-03-session-deliverables/README.md)为历史快照，其“当前”“完成”等表述只对应当时。现行入口为本 README，归档中的设计稿不是额外的一套有效规格。
