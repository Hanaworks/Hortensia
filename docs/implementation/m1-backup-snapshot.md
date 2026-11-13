# M1 SQLite 快照底层实施与验收

现行阅读提示（2026-11-13）：第1—4节保留2026-11-12底层快照切片的事前条件、Windows过程与当时暂缺，不再用其中“仅本机”“Linux尚未验证”“上层未实现”表述当前整个`internal/backup`状态。现行逐项结果见[第5节](#5-2026-11-13-linux关联验证与当前边界)；Linux完整回归见[InstaBox记录§6](m1-instabox-debug.md#6-第一轮-linux-源码回归)，后续通用组见[BG记录](m1-backup-group.md)。底层快照、通用组、正式两端备份命令和恢复交付继续分别判定。

事前基线保存：2026-11-12 16:41 +08:00（文件创建时间；16:43 编写测试/API占位，正式RED后16:46实施生产代码）。实施规格见[底层快照规格](../specifications/m1-backup-snapshot.md)及[主规格 §7](../specifications/m1-maintenance-backup.md#7-一致备份小程序)。仅实现 `internal/backup`，不接入服务/CLI/身份发布，不变更现有 schema 和依赖。当前工作区包含多人未提交变更；交付时附本切片文件与本机版本，不用 HEAD 冒充整个工作区版本。

## 1. 顺序与完成边界

保存本基线 → 编写用户结果/故障边界测试 → 运行并保存正式 RED → 最小实现 → targeted GREEN → 独立复核和结果记录。其他包正在并行修改，本子任务按负责人要求只运行 `go test ./internal/backup -count=1`；全仓验证由负责人整合后执行。无 Git 写入，无真实节点/停止动作。

## 2. 稳定验收条件（实施前）

| 编号 | 输入/动作与用户关心的断言 | 实施前结果 |
| --- | --- | --- |
| BS01 活动 WAL | 保持源连接和 WAL，checkpoint 后再提交新值；Snapshot 副本读到新值，integrity_check=ok；源主文件/WAL字节及 boot/session 哨兵不变 | 未验证 |
| BS02 并发事务 | 复制多页源期间真实 writer 原子更新版本包、事件、claim的相同版本；副本中三个版本一致、外键/完整性通过，writer 实际可提交 | 未验证 |
| BS03 新建/零覆盖 | 目标 staging/文件已有、源=目标、URI/相对路径、源缺失/目录/链接、目标父路径链接/共享路径均拒绝；已有文件和原备份原字节不变 | 未验证 |
| BS04 取消/partial | 已取消调用不创建目标；源被真实 SQLite 独占锁阻塞时短期限返回 DeadlineExceeded 和 ErrPartial，保留新目标，原源/备份不变，释放锁后可以新名字重试 | 未验证 |
| BS05 BUSY 与期限 | 真实独占锁短暂持有，解除后同调用成功；取消退避能及时停止；重试只限 BUSY/LOCKED，其他错误不无限等待 | 未验证 |
| BS06 资源/损坏 | 真实坏库失败并保留 partial；成功后目标可正常重开/移动，无 READY/manifest 生成；源业务哨兵无启动副作用 | 未验证 |
| BS07 发布边界 | 无 Store.Open/服务/CLI/schema/协议/清单实现；BS成功不代表 MB08/09完成，密钥、语义验证、跨文件一致锁和完整备份发布保持未实现 | 审阅待完成 |

## 3. 环境、证据与暂缺

环境为本机 Windows、锁定 modernc.org/sqlite v1.61.1；精确 Go/SQLite/OS、正式RED、最终命令和逐项结果存 `.cache/m1-backup-snapshot-report.md`。普通测试不声称掉电耐久。实际满盘/Finish/Sync故障和 Linux 权限/目录耐久尚未验证，不可据 BS通过改写完整 MB08/MB09结论。上层材料锁、密文/schema/业务验证、secret清单、rekey、READY、恢复发布与 schema6/protocol2均由后续实施。

## 4. 2026-11-12 16:56 事后结果

交付为当前未提交的 `internal/backup` 五个文件及两篇BS文档；生产代码入口为 `snapshot.go:Snapshot`，Raw/连接生命周期在 `copyDatabase`，Step/Finish生命周期在 `copyPages`，平台普通文件/硬链接检查在 `files_windows.go`/`files_linux.go`。本子任务未修改既有业务包、README、go.mod/go.sum或Git；负责人整合后更新现行总状态。

正式RED使用固定返回“snapshot not implemented”的API占位，真实WAL/取消/partial等测试按预期失败；增加错误分类断言后11个路径子项均正式失败。生产实现后最终执行 `go test ./internal/backup -count=1 -v`：退出0，64.882s，12个顶层测试中11通过/1跳过；另有2个符号链接子测试跳过。测试报告及本切片SHA-256见 `.cache/m1-backup-snapshot-report.md`。

| 编号 | 2026-11-12结论（历史） | 证据与当时限制 |
| --- | --- | --- |
| BS01 | 通过（真实SQLite） | checkpoint后提交version=9仍在WAL；副本读到9、integrity/FK通过，源主文件/WAL SHA-256和boot/session哨兵未变；无Store初始化 |
| BS02 | 通过（真实SQLite） | 8192个4KiB大字段行，复制期间writer真实提交；版本包/事件/claim三表版本一致，行数/完整性/外键通过 |
| BS03 | 部分通过／部分未验证 | 已有文件/目录、源=目标、坏输入、硬链接及WAL硬链接真实拒绝并保留旧字节；junction较早一轮真实通过，最终复验创建权限拒绝而SKIP；符号链接权限不可用而SKIP，不能据此声称本轮链接全矩阵通过 |
| BS04 | 通过（真实SQLite/文件） | 预取消零创建；真实独占锁下180ms期限返回DeadlineExceeded+ErrPartial并保留目标；旧partial拒绝重用，新staging重试成功 |
| BS05 | 本机边界通过；实际LOCKED暂缺 | 真实BUSY解除锁后同次成功；受控raw adapter验证扩展LOCKED重试、IOERR立即失败、固定128页及取消后Finish；5分钟配置经代码审阅，未等待完整5分钟；无响应内核不可强制中断 |
| BS06 | 通过（已列真实刺激）；真实磁盘故障暂缺 | 坏库不重试、partial保留；成功目标在验证器打开前仅一个DB，可立即rename；Finish失败通过受控raw adapter，未实际制造满盘/Finish/Sync失败 |
| BS07 | 源码边界复核通过 | 无业务Store/服务/CLI/schema/协议调用，无清单/READY；MB08/09整项仍未完成，后续必须接materials锁及全量语义/密文/身份/secret组验证 |

复验历史保留：首个GREEN曾为11.065s（junction真实通过、两个symlink子测SKIP）；补故障边界后的复验96.674s失败于“创建junction: Access is denied”，其余实现断言通过。随后只将明确环境权限拒绝标为SKIP，并将symlink能力探测改为每package一次，得到上述最终GREEN。其他cmd错误仍使测试失败，存在链接后的生产拒绝断言未放宽。

源码可交接用于上层组合；链接权限、真实故障、Linux平台和完整备份组验证保持明确暂缺。这里不称整个备份模块、M1阶段或产品发布完成。

## 5. 2026-11-13 Linux关联验证与当前边界

本轮冻结工作树输入在A机通过完整Go回归：18测试包、1991条测试／子测试通过、0失败、2跳过，另2包无测试；两个跳过仅为两端Windows大小写专用用例。Go1.27.1／linux/amd64／CGO1、Debian13.7、Linux6.18.60-instabox，vet／build／mod verify均通过。255份源输入已逐字节核对；完整JSON及取回证据见[InstaBox记录§6](m1-instabox-debug.md#6-第一轮-linux-源码回归)，不把该工作树结果表述为已提交版本结果。

| 编号 | 当前结果及其证据范围 | 仍未覆盖 |
| --- | --- | --- |
| BS01／BS02 | 原Windows真实WAL新提交、并发事务一致性证据保留；Linux完整包对应断言亦通过 | 不证明跨机器同时快照、应用数据备份或零数据损失 |
| BS03 | Linux真实WAL硬链接、源symlink及目录symlink拒绝断言实际通过，已有目标与零覆盖包断言通过；通用组原生no-replace发布另见[BG记录](m1-backup-group.md) | Linux通过不改写11-12 Windows权限跳过的历史；目录正常同步及不覆盖不证明掉电耐久 |
| BS04／BS05 | Linux包内取消、partial、真实SQLite BUSY及受控LOCKED／Finish边界回归通过 | 无响应内核仍不能由context强行中断；受控LOCKED不是实际驱动LOCKED故障 |
| BS06 | Linux包内坏库、partial保留、完成后重开／移动及受控错误断言通过 | 真实满盘、Close／Finish／Sync失败及掉电仍未验证；驱动Finish忽略内部Close返回值的上游限制仍有效 |
| BS07 | Snapshot仍只取得SQLite副本，不调用业务Store或服务启动；上层通用CreateGroup／VerifyGroup已在工作树实现并有本机及Linux包证据 | service／CLI／真实两端领域组合未接通；隔离恢复、重加密、受控维护未实现，MB08／09及M1均未整体完成 |

以上补齐指定Linux路径、文件属性与包回归证据；保留原事前基线及Windows失败／跳过的真实过程，不由普通运行或受控I/O错误推出真实设备故障已经验收。
