# M0 双端释放实施及验收

2026-11-12编码前基线，起点656e355。状态：待实施，F01—F10未验证。[模块规格](../specifications/m0-dual-release.md)是本轮依据，原共享协议不改字段。本模块完成后继续M1服务与真实执行，不将库切片视为用户要求的第一个版本。

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
