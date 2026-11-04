# 项目资料归档

归档日期：2026-11-01。来源：`D:\Project\docker-manager`。

按用户的最新要求，原位置仅保留两份对话资料，其余 19 个文件均已归档。`snapshot/` 保存首次整理前全部 21 个文件的完整快照，其中包括上一批已经归档的 11 个文件；`additional/` 保存随后追加归档的 README、HANDOFF、需求说明和待定事项四个文件在第一次整理后的版本。

本次整理不改变历史中已确认或未确认的结论，不将旧建议重新视为定案。归档中的“当前”“下一篇”等说法属于当时的记录，后续工作以用户的新指示为准。

## 查阅入口

- [追加归档的项目入口](additional/README.md)
- [追加归档的交接说明](additional/HANDOFF.md)
- [追加归档的需求说明](additional/docs/requirements.md)
- [追加归档的待定事项](additional/docs/open-questions.md)
- [整理前项目入口](snapshot/README.md)
- [整理前交接说明](snapshot/HANDOFF.md)
- [文档编写方案](snapshot/docs/documentation-plan.md)
- [需求说明](snapshot/docs/requirements.md)
- [功能设计](snapshot/docs/functional-design.md)
- [架构设计](snapshot/docs/architecture.md)
- [数据设计](snapshot/docs/data-design.md)
- [待定事项](snapshot/docs/open-questions.md)
- [上一批暂不采用的初稿](snapshot/docs/archive/2026-10-30-unadopted-drafts/README.md)
- [文件清单与 SHA-256 校验值](manifest.json)

## 原文件的最终位置

以下文件来自最初准备材料及原始对话；分类依据为交接记录、来源说明和文件创建记录，而非最后修改日期。

| 原路径 | 本次处理 |
| --- | --- |
| `README.md` | 追加归档到 `additional/README.md` |
| `HANDOFF.md` | 追加归档到 `additional/HANDOFF.md` |
| `docs/requirements.md` | 追加归档到 `additional/docs/requirements.md` |
| `docs/open-questions.md` | 追加归档到 `additional/docs/open-questions.md` |
| `docs/conversation-notes.md` | 原样保留 |
| `docs/ChatGPT-Docker管理程序推荐-20261030-0119.md` | 原样保留 |

这些文件在此前讨论中可能已经修订，本次不推测或还原早期内容。两份仍在原位置的对话资料未作修改；四个追加文件只重定位相对链接，正文保持当时记录，其中“保留 6 个文件”等文字属于第一次整理时的历史状态。

## 归档方式

首次整理将后续新增文件移入 `snapshot/` 下相同的相对路径，最初文件复制一份进入快照。追加归档将上述四个文件移入 `additional/`，保留原有相对目录结构；原快照不覆盖。目录旁的同名 ZIP 包含本目录的归档说明、清单、完整快照和追加文件。

清单的 `files` 对应首次整理前文件及快照，`keep_original` 表示当前是否仍保留原路径；`additional_files` 同时记录追加文件移动前与调整链接后的校验值。需要恢复时按清单从对应目录取回所需版本，并先核对目标位置的现有内容。

