# Docker Manager

自托管的多节点 Docker 管理器，面向维护多台独立节点的运维人员。节点管理与迁移体验参考 PVE，Docker 资源及 Compose 操作参考 Portainer；由用户明确决定部署位置，以 Node → Stack／Docker 资源组织管理。

`docker-manager`是暂名。管理／接入运行交接基线为`95d453f`：Manager／Web管理、正式Agent接入、持久任务通道及Compose执行程序已有实现，本机回归与独立复核已完成适用项。后续两端备份只读检查器已有实现，证据见[M1检查器记录](docs/implementation/m1-first-version.md#8-两端备份只读检查切片)。[Manager运行](docs/manager-first-run.md)和[Agent接入](docs/agent-first-run.md)分别说明入口。真实Docker接受表仍为空，生产部署能力尚未开放；完整备份组／恢复及受控维护继续实施，M1首个完整版本未交付。

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

| 阅读任务 | 现行入口与职责 |
| --- | --- |
| 判断现在做到哪里 | [M1推进与验收](docs/implementation/m1-first-version.md)：工作包状态、V01—V08、已验证子集和剩余依赖 |
| 看产品目标和操作行为 | [需求](docs/requirements.md)、[功能与交互](docs/functional-design.md)：稳定范围、场景、质量与后续能力 |
| 理解整体和组件 | [架构](docs/architecture.md) → [Manager](docs/components/manager.md)、[Agent](docs/components/agent.md)、[Web](docs/components/web.md) |
| 启动和接入 | [Manager首次运行](docs/manager-first-run.md)、[Agent接入](docs/agent-first-run.md)；实际入口和限制分别说明 |
| 接手模块内部实现 | [模块阅读图](docs/documentation-plan.md#7-模块阅读与维护入口)：正式通道、服务、账本、存储、执行、Web和备份各自的规格 |
| 查通信和请求字段 | [HTTP规格](docs/specifications/http-api.md)、[Agent协议](docs/specifications/agent-protocol.md)、[共享Schema](contracts/agent-v1.schema.json) |
| 查数据与持久化 | [数据设计](docs/data-design.md)、[Manager v5 Store](docs/specifications/m1-manager-runtime-store.md)、[Agent v5](docs/specifications/m1-agent-runtime.md)：逻辑定义、实际字段／事务和DDL入口 |
| 查执行与恢复规则 | [执行恢复](docs/specifications/execution-recovery.md)、[工作区／Compose／监督](docs/specifications/m1-compose-engine.md)：开始、副作用、观察、结果与释放 |
| 查备份实现及缺口 | [完整备份组规格](docs/specifications/m1-backup-group.md)、[BG验收](docs/implementation/m1-backup-group.md)、[维护恢复设计](docs/specifications/m1-maintenance-backup.md) |
| 核对真实调试证据 | [InstaBox记录](docs/implementation/m1-instabox-debug.md)：Linux、Web、原生CLI与正式接入的不同证明范围 |
| 继续实施与验证 | [开发计划](docs/development-plan.md)、[测试计划](docs/test-plan.md)、[运行准备](docs/operations.md) |
| 查文档组织与教训 | [文档规划](docs/documentation-plan.md)、[全过程复盘](docs/implementation-lessons.md) |
| 查确认及未决项 | [决策与待办](docs/decisions.md)：用户确认、技术选择及剩余取舍 |

已结束的M0验收与被替代方案统一从[2026-11-14归档目录](archive/2026-11-14-documentation/README.md)查阅，不作为当前实施清单；仍被v5明确引用的底层／旧格式规格见模块阅读图。

文档以 req42／arc42 为统一骨架，模块、数据、接口、测试和运行继续承接需求与设计。2026-11-04 的继续落实覆盖首条流程的 HTTP 返回对象、20 类 Agent 消息、共享字段约束、执行结局和归属、双端释放及恢复核对；上一轮内容中的相关规则同步更新。现行稿用于按同一约定开展实现与原型，不代表产品或故障恢复已经验证；迁移策略和后续能力按阶段展开。文档／契约检查与软件测试分别报告。

## 继续开发

2026-11-12按登录／节点接入 → Web保存配置 → 固定版本部署 → 查看结果推进。正式身份、HTTPS管理服务和Web已实现；[Agent服务](docs/specifications/m1-agent-service.md)、[工作区／Compose](docs/specifications/m1-compose-engine.md)和[Manager任务通道](docs/specifications/m1-manager-agenttransport.md)已有实现与本机组合证据。实际Agent可认领、连接及完成空恢复；受控启动失败任务已通过许可、结果ACK、双端释放及重连核对。受控失败验证不代表真实Docker部署成功。

复验使用`go test -p 1 ./... -count=1`，其中包含真实TLS及辅助进程；Windows本机曾有进程启动阻塞，顺序验收保留原断言和业务时限，诊断见[运行验收](docs/implementation/m1-manager-runtime.md)。`pwsh -File tools/check-contracts.ps1`和`node tools/check-docs.mjs`只检查静态契约与文档。Web在其目录运行`npm test`、`npm run typecheck`和`npm run build`。

旧M0入口保持各自格式与证据；正式运行使用显式Agent5／Manager5和protocol1，没有隐式迁移。ACK不释放占用，正常双端finalize／finalized才释放并发布归属；观察不完整只补观察，真正未知、矛盾继续隔离。已知耐久退出但尚无结果时，恢复原结局而不重跑命令。

[维护与完整备份恢复](docs/specifications/m1-maintenance-backup.md)已有事前基线，软件尚未完整实现；[SQLite一致快照](docs/specifications/m1-backup-snapshot.md)只是底层切片。[完整备份组](docs/implementation/m1-backup-group.md)的通用库已提交`7362441`，正式两端service／CLI及真实材料组合继续接线。未来schema6／protocol2方案与现行v5分开，不能用规划中的命令操作当前程序。2026-11-13用户已指定InstaBox并批准调试访问，已创建两台专用Linux机，首轮Linux源码回归1991条通过、0失败、2项Windows专用跳过，见[IB验证记录](docs/implementation/m1-instabox-debug.md)。Docker原生对照、产品版本矩阵、两节点隔离及最终独立阅读分别验收，不由Linux源码回归推定通过。

每模块遵守[开工与完成流程](docs/development-plan.md#61-每轮实施的开工与完成条件)及AGENTS.md：先规格／验收，再实现，最后记录证据与限制。首版范围已确认按M1完整流程；正式名称、许可证及公开发布方式仍未决定。技术选择和参考产品行为不表述成用户逐项确认。

## 历史资料

[2026-11-04 初稿压缩包](archive/2026-11-04-initial-draft.zip)保留继续完善前的 14 篇文档原文，并附原始讨论和历史依据、逐文件校验清单；[压缩包 SHA-256](archive/2026-11-04-initial-draft.zip.sha256)用于核对包本身。解压后从 `snapshot/README.md` 开始阅读，包内旧稿不随工作目录更新。

[原始对话](docs/ChatGPT-Docker管理程序推荐-20261030-0119.md)与[早期讨论摘要](docs/conversation-notes.md)保留不变。后续产品决定的直接来源见 [11 月会话记录](archive/2026-11-03-session-deliverables/conversation/conversation.md)。

[2026-11-01 归档](archive/2026-11-01-documentation/README.md)和[2026-11-03 归档](archive/2026-11-03-session-deliverables/README.md)为历史快照，其“当前”“完成”等表述只对应当时。现行入口为本 README，归档中的设计稿不是额外的一套有效规格。
