# M0 Agent 开始账本实施计划与记录

> 执行要求：按规格逐任务实施，先写有判别力的测试并观察失败，再实现、审阅和记录；任务执行与最终审阅分开。不清理现有托管工作区。

日期：2026-11-07。起点：`d53de6d`。本计划和[实施规格](../specifications/m0-agent-ledger.md)先于本模块代码形成。

**Goal:** 交付可独立验证的 SQLite 开始账本及既有 Gate 适配，证明先持久提交、最多一次启动及重启冻结。

**Architecture:** 保留冻结通道，以受控 prepare 夹具登记稳定输入、两类占用；真实事务提交 START_COMMITTED 后才允许无副作用 Start 回调。每次打开形成新 boot，旧事实只读且不能恢复执行。

**Tech Stack:** Go1.27、CGO关闭、modernc.org/sqlite v1.61.1 / libc v1.79.0、SQLite3.54.1、本地文件、现有 protocol 与 startgate。

**Spec:** [M0 Agent 开始账本实施规格](../specifications/m0-agent-ledger.md)，验收编号 L01—L10。

## 共同约束与审阅重点

- 遵循开发计划的每模块开工／完成流程；技术选择及异常先写清，再实现。当前继续推进已获用户授权，不另加每步审批。
- 单库固定 node/ledger/key，双摘要，稳定六元组、项目与执行配置；原始正文不入库。
- WAL/FULL/外键/5000 每连接有效，BEGIN IMMEDIATE、sql.Tx、CAS与唯一约束；零内部执行重试。
- Gate 的nil提交仅代表此次首次持久提交，已有记录不能转换成新的启动资格。
- 关注旧boot、提交结果不确定、部分占用、准备内容微变、提交后取消／到期；不得只测正常关闭重开。
- 当前Windows证据不外推Linux、掉电、真实Compose或整个M0；无任务广播、结果及释放入口。

## Task 1：存储、身份、固定输入与占用（L01—L04）

文件：新增 `internal/agent/ledger/store.go`、`records.go`、`migrations/001_start_ledger.sql`、对应 `_test.go`；修改 go.mod/go.sum。

接口：Create/Open、Identity/Close、RecordReadyFixture/Get/Claims，以及规格定义的 Identity、Record、Claim 和固定错误。供Task2消费 Store 内部db、boot和查询逻辑。

- [ ] 写Create/Open和原始输入/占用测试，确认缺失实现造成失败。
- [ ] 固定依赖，安装版本1 DDL，落实每连接参数与校验、身份密钥配对、原始字节指纹和事务登记。
- [ ] 补错身份、缺库、损坏、较新schema、同操作冲突、并发占用及回滚测试；运行本包对应测试。
- [ ] 独立核对规格与实现，记录偏差并修正。

## Task 2：Gate 与持久开始边界（L05—L07、L09）

文件：新增 `internal/agent/ledger/gate.go`、`gate_test.go`；必要时最小更新 startgate 注释，不改变既有协议行为。

接口：NewGate(ctx, startgate.Request, now, start)；内部commitStart(ctx, Permit)。沿用Task1固定数据和事务帮助，不新增网络处理。

- [ ] 先写合法开始、重复、第二Gate构造、错上下文和旧boot冻结测试并确认失败。
- [ ] 实现每操作单Gate、事务复核、CAS和完整许可提交；nil只用于本次首次提交。
- [ ] 用提交回调屏障和真实SQLite写锁／事务错误验证取消、失效、期限及提交失败；禁止产品代码增设测试故障开关。
- [ ] 独立审阅启动资格、锁顺序、关闭和错误路径，修正后复验。

## Task 3：进程终止与重开（L08）

文件：新增 `internal/agent/ledger/restart_test.go`，复用本包受控输入。

- [ ] 使用测试二进制辅助进程，在三个明确阶段输出同步标记；父进程收到标记才强制终止并等待回收。
- [ ] 父进程重开真实库，验证完整事实、两占用、旧boot冻结及Start累计计数；不使用正常Close替代强制终止。
- [ ] 辅助进程与超时清理不得遗留；记录进程实验不能证明掉电或旧备份检测。

## Task 4：整体验收、文档与交付（L10）

- [ ] 独立审阅新增模块及接线，处理重要发现；核对L01—L10逐项证据。
- [ ] 运行 `go test ./... -count=1 -timeout=120s`、`go vet ./...`、`go build ./...`、`go mod verify`，以及契约／文档／Git空白检查。
- [ ] 更新数据、Agent、决策、测试、开发计划与README的当前状态及本轮入口；主定义只增加本次实际事实，历史记录保留。
- [ ] 本表登记版本、环境、结果与未验证范围；保存本地Git提交后交接，不发布产品。

## 验收记录

开工时 L01—L10 均未执行。通过条件以预先保存的规格为准，后续逐项填写测试证据和实际结论。

## 实施裁定与进度

开工裁定：本轮仅完成开始账本与Gate受控接入，暂不开放网络任务。理由是结果事件与核对链尚不完整；代价是当前只能从测试驱动验收，需要下一模块继续完成结果及恢复记录。

开工审阅补充（进入相关代码前）：L03接受Schema合法数学整数写法但按原字节指纹去重；L09明确开始字段类型校验、Close失效及等待语义；物理时间用Unix毫秒，密钥校验域分隔为真实0x00。由独立审阅发现并同步规格，不改变既有线上协议。
