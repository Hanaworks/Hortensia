> 已归档：2026-11-14。本文是已结束M0切片的实施／验收历史，正文中的执行要求和“当前／下一步”只适用于当时，不是新的工作指令。现行实现入口：[当前模块](../../../docs/implementation/m1-first-version.md)；整体状态：[M1推进](../../../docs/implementation/m1-first-version.md)。原始字节见归档原文包。

# M0 原型验收补齐计划

日期：2026-11-07。起点：`3f0dcc1`。本计划先于本轮测试和修正编写，承接既有[实施与验收 AC01—AC11](m0-agent-channel.md#本轮验收标准)；不改变上一轮规格事后补录的事实。

**Goal:** 补齐当前通信原型与独立 Gate 的验收证据，修正不符合既定规格的行为，给出逐项可追溯的交付结论。

**Architecture:** 沿用当前共享 Schema、TLS 通道、命令行与独立内存 Gate。先以规格构造正常及拒绝输入；已有行为正确则保留代码，发现失败则记录原因、最小修正并复验。原型不接入持久账本、任务执行或 Docker。

**Tech Stack:** go.mod 固定的 Go 1.27.0 及依赖；Windows/amd64、本机回环 TLS、临时测试材料、可控时钟。

**Spec:** [实际实现规格](../../../docs/specifications/m0-channel-probe.md)第 2—5 节及目标协议。测试证明方法与完成条件以 AC01—AC11 为准。

## 共同约束与审阅重点

- TLS 最低 1.2；hello/welcome 10 秒；8 MiB 重组文本预算；禁用压缩。
- 指纹包含 hello，最多 1024 项；满后再收到消息即关闭，包括重复消息。
- 冻结诊断精确匹配规格；Agent 不接受握手后的应用消息；能力为空。
- Ping 15 秒、最后 Pong 后 45 秒，最多 4 个在途；Gate 窗口固定 10 秒，不因重试延长。
- 每个拒绝测试须确保其他前提有效，防止因无关错误而假通过；断言返回值、输出、状态或调用次数，不能只看测试名称。
- 先写预期再运行；缺陷修正必须先复现失败。补验已有正确行为可直接通过，如需验证测试敏感性只在隔离副本做定向变异，保留失败证据。
- 不增加仅用于测试的公共配置或放宽标准；必要的内部可测性调整须说明理由。真实套接字、可控时钟及静态结构审阅分别记录，不互相替代。

## Task 1：通道的期限、身份与消息边界（AC03—AC07）

文件：`internal/channel/acceptance_test.go`（新增），必要修正位于 `channel.go`；沿用 `channel_test.go` 夹具。接口为 `Server.ServeHTTP`、`Connect`、`Peer.Run`，不改变公开签名。

- [x] 增加 Upgrade 边界：错误路径 404；非 TLS／非 GET／空 query／替代凭据 401；响应 no-store；重定向目标调用数 0；压缩不协商。
- [x] 增加握手期限及关联：真实连接等待 hello 或 welcome 达到 10 秒关闭；更早父上下文有效；错误 node、reply_to、信封／body session 或 reconcile_required=false 拒绝，正确 welcome 可接受。
- [x] 逐类核对支持矩阵：Manager 冻结应答完整信封／body，Agent 对合法应用消息关闭 1008/unexpected_message，hello 能力为空。
- [x] 指纹表测试发 hello 加 1023 个唯一消息；边界前同内容重试不占新项；满后新消息和同内容重试均关闭；同 ID 改内容关闭。补关闭 reason 和 Agent 的非法／分片超限接收断言。
- [x] 运行 `go test ./internal/channel -count=1 -timeout=90s`；错误行为先记录失败再修正，结果返回主执行者。

## Task 2：命令行与材料（AC02）

文件：`cmd/channelprobe/acceptance_test.go`（新增）、必要时 `main.go`；复用现有进程和证书夹具。接口为 `run(context.Context, []string, io.Writer, io.Writer) int` 及现有 CLI。

- [x] 参数、材料和模式分别构造有效前提，只改变一个输入；检查规定的 0／1／2 返回值和固定诊断，确保 stdout／stderr 不含测试秘密和秘密文件内容。
- [x] 覆盖 token 的 43 字符规范编码和允许的单个行尾，拒绝空格、多行、错长度／非规范编码；覆盖 UUID、URL、CA、证书／私钥及监听地址边界。
- [x] 验证 JSON 输出字段、运行到期及连接期间到期的区别、401、断线、监听冲突、输出失败；故障使用受控本机资源或 writer，避免在产品中增设测试故障开关。
- [x] 对正常运行无法独立触发的防御分支，记录调用链／不可达条件与静态检查依据，不宣称已做故障实验。
- [x] 运行 `go test ./cmd/channelprobe -count=1 -timeout=90s`，记录断言、修正与剩余限制。

## Task 3：解码输入预算（AC07）

文件：`internal/protocol/acceptance_test.go`（新增）、必要时 `validator.go`；接口 `Validate([]byte) (Envelope,error)`。

- [x] 对 8 MiB 原始消息、1 MiB 输出、单文件 1 MiB、总文件 4 MiB、128 文件、240 UTF-8 字节路径做边界内／等于／超出检查；保持外层 8 MiB、目标及引用等其他前提合法。
- [x] 覆盖重复／缺失引用、内外目标不符、编码规范性与解码后 UTF-8；检查所报错误及无秘密回显。现有数字、日期与严格 JSON 用例纳入证据索引。
- [x] 运行 `go test ./internal/protocol -count=1 -timeout=90s`；不从结构验证外推 Compose、HMAC 真实性或真实文件系统保证。

## Task 4：整体验收与交付记录（AC01、AC08—AC11）

文件：现有实施与验收、实际规格、测试计划、开发计划、文档规划和 README；必要时补 `gate_test.go`／`heartbeat_test.go` 的实质遗漏。

- [x] 逐条核对 Gate 绑定、期限、重放、Deny、提交竞争、失败及8个状态，和探活期限／预算／清理。已有直接断言充分时只引用证据。
- [x] 对 Tasks 1—3 做独立规格与代码审阅，修正重要问题；核对代码未开放任务、未调用 Docker、Gate 未接入网络。
- [x] 全量运行 `go test ./... -count=1 -timeout=90s`、`go vet ./...`、构建、`go mod verify`、契约与文档检查；仅在新修改／失败／审阅发现需要时重复测试。
- [x] AC01—AC11 逐项登记方法、环境、结果和限制；完成条件满足才更新当前切片结论，整个 M0／M1 状态按实际保留。原失败与形成时点保留。
- [x] 保存本地 Git 提交，提供当前交付入口、证据与后续依赖；不自动发布产品。

## 计划自查

AC02—AC07 已逐项分配到 Tasks 1—3；AC08—AC10 由 Task 4 核对既有断言；AC01／AC11 由整体验证与交接处理。接口沿用现有定义，不创建第二套报文。未决产品能力均不属于此次补齐范围；无需新的产品取舍才能执行以上工作。

## 执行记录

Task 4 局部核对：既有 Gate 用例覆盖身份／关联、10秒窗口、重复、取消／失效、提交竞争、Deny 与已开始事实；探活用例以可控时钟核对15／45秒、迟到Pong、4个在途预算及清理。本轮在 `internal/startgate/acceptance_test.go` 补到期Deny、提交失败时保留取消／失效、未观察到期前的取消、最大安全授权版本四组行为，`go test ./internal/startgate -count=1 -timeout=30s` 通过；无需修改 Gate 代码。Tasks 1—3 已完成并分别通过本包测试。最终全量复验四包通过、537条测试及子测试通过记录、失败0、跳过0；构建、静态检查和模块校验通过。未改运行代码。独立审阅发现旧会话测试未等待清理结束，已补清理屏障及新连接完整应答，定向20次通过后再跑全量；CLI主机名用例明确限定为失败映射，名称验证由显式ServerName用例提供。最终审阅无未解决问题。代码／测试及本计划已保存为94056e0，交付文档绑定该版本并逐项记录AC01—AC11结果；后续文档提交保存最终交付结论。
