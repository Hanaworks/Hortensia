# 2026-11-14 文档归档与替代目录

状态：历史归档，不是另一套现行规格。归档日期2026-11-14；依据用户要求，将已结束阶段的记录、被具体规格替代的旧方案和过时进度移出现行正文。原始需求／会话／参考原件不因未采用而作废。

## 1. 本次处理基线

变更前仓库基线为d0fa613。先保存原目录结构的Markdown原文快照、逐文件SHA-256和压缩包，再迁移或改写现行文档。原文包只用于历史复核，解压目录中的相对链接应结合该Git版本的仓库上下文阅读。

| 类别 | 处理方式 | 现行替代入口 |
| --- | --- | --- |
| 已结束的M0切片实施／验收记录 | 迁入m0-implementation，保留正文、版本、失败及限制；旧位置仅留兼容跳转 | [M1推进](../../docs/implementation/m1-first-version.md)及各M1模块实施记录 |
| 旧数据物理布局、事务／密钥环／迁移提案 | 迁入superseded-design，保留历史论证；现行数据设计只保留逻辑定义及真实存储主定义入口 | [数据设计](../../docs/data-design.md)、[Agent v5](../../docs/specifications/m1-agent-runtime.md)、[Manager v5](../../docs/specifications/m1-manager-runtime-store.md) |
| 已被替代的五步备份算法 | 迁入superseded-design，旧算法作废，不作为操作步骤 | [完整备份组](../../docs/specifications/m1-backup-group.md)及[维护／恢复设计](../../docs/specifications/m1-maintenance-backup.md) |
| 文档编写、开发计划中的历史进度流水 | 迁入history；现行规划只保留持续方法、当前阅读路径与有效依赖 | [文档规划](../../docs/documentation-plan.md)、[开发计划](../../docs/development-plan.md) |
| 仍由当前代码／规格引用的底层规则、原始资料、未来正式产品设计 | 保留其有效用途，区分旧格式参考／当前实现／待实现；不按日期一律作废 | [现行README](../../README.md) |

归档正文中的“当前”“待实施”“下一步”只指记录形成时。可读副本为保持导航而更新相对链接并增加归档提示，原始字节另存原文包；不得用归档中的接口、旧DDL或旧完成状态覆盖现行定义。

## 2. 整理顺序与核对条件

本表在迁移前保存，随后记录结果；本次不改变Go、SQL、协议Schema或软件能力。

| 编号 | 核对条件 |
| --- | --- |
| DA01 原文保全 | 原文包与逐文件摘要先保存；迁移可读副本时只改导航／归档提示，历史正文和失败记录不删除 |
| DA02 现行唯一入口 | README不将M0旧验收列为当前待办；被替代算法及旧物理模型从现行正文迁出；实现状态仍明确M1未交付 |
| DA03 引用可达 | 更新现行目录和交叉引用；兼容旧路径可找到归档，必要历史锚点可追溯；归档可读副本的本地链接也检查 |
| DA04 有效材料保留 | 原始会话和参考输入保持字节不变；仍被v5引用的底层规格保留，并解释其适用范围 |
| DA05 变更边界 | 文档／归档校验通过；Git差异仅含文档及归档材料；没有把文档整理声称为新增软件测试通过 |

## 3. 归档清单与结果

9篇整篇记录已迁移；5组局部旧正文已抽出。来源及校验见[manifest.json](manifest.json)，原始69份Markdown见[原文包](original-documents.tar.gz)。现行入口始终为项目README，兼容跳转页不承担旧正文的现行维护。

| 历史实施记录 | 原路径 | 现行承接 |
| --- | --- | --- |
| [m0-acceptance-completion.md](m0-implementation/m0-acceptance-completion.md) | docs/implementation/m0-acceptance-completion.md | [现行入口](../../docs/implementation/m1-first-version.md) |
| [m0-agent-channel.md](m0-implementation/m0-agent-channel.md) | docs/implementation/m0-agent-channel.md | [现行入口](../../docs/implementation/m1-first-version.md) |
| [m0-agent-journal.md](m0-implementation/m0-agent-journal.md) | docs/implementation/m0-agent-journal.md | [现行入口](../../docs/specifications/m1-agent-runtime.md) |
| [m0-agent-ledger.md](m0-implementation/m0-agent-ledger.md) | docs/implementation/m0-agent-ledger.md | [现行入口](../../docs/specifications/m1-agent-runtime.md) |
| [m0-agent-scan.md](m0-implementation/m0-agent-scan.md) | docs/implementation/m0-agent-scan.md | [现行入口](../../docs/specifications/m1-agent-runtime.md) |
| [m0-dual-release.md](m0-implementation/m0-dual-release.md) | docs/implementation/m0-dual-release.md | [现行入口](../../docs/specifications/m1-manager-runtime-store.md) |
| [m0-manager-operations.md](m0-implementation/m0-manager-operations.md) | docs/implementation/m0-manager-operations.md | [现行入口](../../docs/specifications/m1-manager-runtime-store.md) |
| [m0-manager-recovery.md](m0-implementation/m0-manager-recovery.md) | docs/implementation/m0-manager-recovery.md | [现行入口](../../docs/specifications/m1-manager-runtime-store.md) |
| [m0-protocol-base64.md](m0-implementation/m0-protocol-base64.md) | docs/implementation/m0-protocol-base64.md | [现行入口](../../docs/implementation/m1-protocol-input.md) |

局部归档包括[旧数据物理方案](superseded-design/data-physical-proposal.md)、[旧备份生成算法](superseded-design/backup-generation-proposal.md)、[文档编写过程](history/documentation-progress.md)、[开发进度流水](history/development-progress.md)和[早期验证摘要](history/early-validation-summary.md)。其中已被替代的规则不再出现在现行操作正文；历史结论本身保留。

## 4. 整理结果

2026-11-14完成本轮文档整理；没有改变产品需求、程序、SQL、协议或软件验收结论。

| 条件 | 实际结果 |
| --- | --- |
| DA01 | 原文包解出69份Markdown，长度和SHA全部与变更前清单一致；9篇整篇及5组局部可读副本去除导航差异后正文一致 |
| DA02 | README已切换到M1任务导航；旧物理方案和备份算法从现行正文移出；文档规划只维护持续方法与当前模块入口 |
| DA03 | 现行67篇（含9个兼容页）846个本地链接／203表检查通过；本归档15篇216个链接／31表检查通过。9个旧位置保留70个标题定位，3篇局部整理文档另保留35个旧定位，均明确指向归档而不恢复旧正文 |
| DA04 | 两份原始会话／摘要字节未变；7篇底层／旧格式规格因仍有有效代码或v5引用而保留并明确用途；其余48份原文除了链接目标没有改写正文 |
| DA05 | Git差异未涉及Go／SQL／契约／Web源码；本轮仅做文档及归档检查，不重报软件测试通过 |

后续新增或修订继续按[现行文档规划](../../docs/documentation-plan.md#6-过时内容归档与检查)归档失效内容，不把正文只加“过时”标签后仍留作现行规则。兼容页只解决原始资料／书签的路径与定位，不在当前阅读目录重复维护旧正文。
