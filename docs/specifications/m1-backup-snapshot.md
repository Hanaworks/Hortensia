# M1 SQLite 一致快照底层切片

事前基线保存：2026-11-12 16:41 +08:00（文件创建时间）；16:44 在生产实现前补充伴随文件及同用户路径替换边界。基于当前工作区锁定的 `modernc.org/sqlite v1.61.1`。本篇细化[备份主规格 §7](m1-maintenance-backup.md#7-一致备份小程序)，只交付 SQLite 文件副本，不交付完整备份组、恢复入口或 schema6/protocol2。

## 1. 目的、依赖与调用边界

`internal/backup` 不依赖 Manager/Agent Store，不调用其 Create/Open，不修改 boot、session、恢复状态或业务表。调用方之后必须实现 materials 锁、完整身份/密钥组、精确 schema/语义/密文验证、清单和发布。此函数成功不使 MB08/MB09 整项通过，也不能产生 READY 或声称副本可恢复。

锁定驱动本地源码 `conn.go:NewBackup` 和 `backup.go` 已核对：`NewBackup(string) (*sqlite.Backup,error)`，`Step(int32) (bool,error)`，`Finish() error`；Finish 关闭目标连接，不能再 Commit 或再次 Finish。驱动 Finish 当前忽略内部目标 Close 返回值，这个上游限制必须保留在验收说明中。

## 2. 实际 API 和路径约束

```go
func Snapshot(ctx context.Context, sourceFile, targetFile string) error
```

`ctx` 不可 nil。两个输入必须是绝对本地文件路径，拒绝 URI、UNC、NUL、查询/片段字符和 Windows ADS；所有已有路径组件拒绝符号链接及 Windows reparse point。源必须是已有普通文件，硬链接数必须为 1；已有 SQLite `-wal`/`-shm`/`-journal` 伴随文件也必须是无链接普通文件。源父目录可以是现有服务目录；不因此要求它由本函数创建。路径所在目录的本机拥有者仍须可信；本函数不承诺抵御同一用户在检查与 SQLite 打开之间恶意替换目录/文件。

目标直接父目录是本次专有 staging 目录：必须不存在，由函数以排他 Mkdir(0700) 创建；其父目录必须已存在并通过 localstate 私有目录检查。目标文件用 O_CREATE|O_EXCL(0600) 预留。任何已有 staging 目录、目标或链接均拒绝，绝不覆盖。拒绝缺失/异常源发生在创建 staging 前。Windows 沿现有 localstate 能力检查 reparse，但不声称 Unix 模式能验证 Windows ACL。

导出错误分类：`ErrInput` 表示无效输入/源路径；`ErrDestination` 表示目标不能安全新建；`ErrPartial` 表示已经创建本次 staging 后失败。取消/期限仍可由 `errors.Is(err, context.Canceled/DeadlineExceeded)` 判断。目标冲突可由 `errors.Is(err, ErrDestination)` 判断。错误不输出数据库行或密钥。

## 3. 内部数据流、期限与资源生命周期

1. 建立 `context.WithTimeout(ctx, 5*time.Minute)`，先检查取消及路径。初始化错误不创建目标。
2. 排他创建 staging 和空目标，之后任何错误都保留本次文件供诊断并附 ErrPartial；禁止自动清理或覆盖重用这个名字。重试必须使用新的 staging 路径。函数不是按目标路径幂等执行的发布操作。
3. 用通用 sqlite 驱动开独立源连接：正确编码本地 file URI、`mode=ro`、`busy_timeout(0)`。不使用 immutable，不注册业务 Store 的启动 hook。源的 SQL 持久状态不变；SQLite 可能更新已有 SHM 的读锁信息，这不等同业务写入。
4. `sql.Conn.Raw` 断言驱动 NewBackup 接口，目标 URI 使用已预留文件和 `mode=rw`/`busy_timeout(0)`/`synchronous(FULL)`。执行 Step(128)；成功还存在页时继续。SQLite 自己处理并发提交导致的复制重启，成功结果是单库一致副本，时间点不承诺等于调用开始。
5. 只有 SQLite 基础错误码 BUSY=5、LOCKED=6（包括扩展码）重试，等待 50/100/200/400/500ms，此后上限 500ms；正常进展后重置为 50ms。初始化与 Step 共用总期限，等待支持取消。其他错误立即终止。
6. 成功、错误或取消都恰好 Finish 一次；Finish 失败必失败。Raw 内不调用 Conn/DB 方法，避免数据库/sql 同连接死锁。再关闭源 Conn 与 DB，检查错误；最后 Sync 目标及目录。任何步骤失败都不产生完整备份标志。

取消检查在每次 Step 前及资源完成后执行。Step、Finish、文件 Sync 等同步底层调用无法被 Go context 强行中断，5 分钟约束适用于调用之间及退避，不声称可终止失去响应的内核/磁盘。单次 Step 只允许 128 页，不用 Step(-1) 无界复制。

## 4. 状态、故障与维护规则

状态只有未创建 → staging/partial → SQLite快照完成；这个状态不写入业务库，也不是备份组状态。调用错误后，即使目标可以打开也仍是 partial，不能恢复；调用成功后也仍待整组验证。完整清单和 READY 由未来上层单独负责。

源丢失/非普通文件/链接、目标存在、并发抢占 staging 均零覆盖拒绝。busy/locked 可以同一次尝试等待，取消、坏数据库、不可写目标和 Finish/Sync 错误则结束。原有效备份从不作为目标，整个模块无删除 API。普通在线读可能接触 SQLite SHM；不能把文件字节哈希要求扩展到共享内存读锁元数据。

修改驱动版本先重审 NewBackup/Step/Finish 资源行为；修改路径必须复验链接、硬链接和已有目标；修改循环必须复验 WAL 新提交、事务一致性、取消和重试分类。完整性/外键检查在测试中用于判断副本，不替代未来上层精确语义/解密验证。

## 5. 验证与未实施内容

稳定 BS 条件和证据在[实施记录](../implementation/m1-backup-snapshot.md)。全部测试使用本机真实 SQLite/临时文件，不连接现有节点或 Docker。仅覆盖 Windows 实际运行；Linux 路径/私有权限代码需后续真实 Linux 复验。掉电耐久、满盘和实际 Finish 错误仍需专门故障环境，不能从普通进程测试推断。
