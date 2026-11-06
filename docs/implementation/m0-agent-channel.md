# M0 Agent 通道原型实施与验收

更新日期：2026-11-06。适用对象：当前通信与独立许可门禁原型切片。代码基线为 `98d9c13`，不是整个 M0 阶段。以下实施约定和验收表于本日补建，不能追认为编码前已完整确定；原实施过程与测试记录继续保留。

**Goal:** 用现有 v1 报文验证真实 TLS WebSocket 连接及独立的开始许可门禁，为后续持久化和 Docker 实现提供可重复证据。

**Architecture:** 管理端与 Agent 的协议测试程序共享本仓库 Schema 验证器；连接只完成认证、hello/welcome、探活和会话替换。开始许可在独立受控测试中验证，不将内存状态伪装成持久账本。

**Tech Stack:** 本机 Go 1.27.0；coder/websocket v1.8.15（ISC）、santhosh-tekuri/jsonschema/v6 v6.0.3（Apache-2.0）、regexp2 v1.11.5（MIT）；间接依赖 x/text v0.14.0。版本与校验和保存在 go.mod/go.sum。采用 Schema 库官方的 ECMAScript 引擎接入方式，正则匹配超时 100 ms 后拒绝，不手写一套 Path 正则翻译。这是本轮 Windows/amd64 验证组合，其他工具链和操作系统尚未验证。

**Spec:** 本切片受[通信规格](../specifications/agent-protocol.md)、[执行与恢复](../specifications/execution-recovery.md)和[消息 Schema](../../contracts/agent-v1.schema.json)约束，只实施下表列明的子集；基线 `98d9c13` 的最终接口、支持消息、状态与异常行为见 [M0 实际实现规格](../specifications/m0-channel-probe.md)。本篇维护实施约定、验收条件和执行记录。

## 本轮实施约定

本轮要交出的结果是：可从源码构建的 `channelprobe`，在本机以两个进程验证受信 TLS 与应用握手；共享验证器拒绝不合契约的输入；独立 Gate 在受控回调下验证一次许可的开始条件和竞争处理。使用者是继续开发与复验的人员。当前运行组合为 Windows/amd64、Go 1.27.0，不承诺可部署的 Docker 管理服务。

| 必须定清的内容 | 本轮约定及主定义 |
| --- | --- |
| 实施范围 | `contracts` 的嵌入与 Go 验证器、`internal/channel`、`internal/startgate`、`cmd/channelprobe` 及对应测试；通道与 Gate 分别验证，当前不相连 |
| 对外表现 | 命令参数、必需材料、JSON 输出、固定诊断及退出码按[实际规格第 2 节](../specifications/m0-channel-probe.md#2-命令行输入与可观察输出)；消息、时序、限制与关闭行为按第 3 节 |
| 内部实施边界 | 解析入口、解码与预算按实际规格第 4 节；Gate 的输入、8 个状态、回调顺序及错误优先级按第 5 节；网络和持久化的调用方责任仍未实现 |
| 必须保持的不变量 | 无效身份不能建立应用会话；旧会话不能取代新会话；未恢复时不开放任务；无有效许可、过期、取消、失效或提交失败时不调用 Start；同一 Gate 的许可重放不产生第二次 Start |
| 暂不实施 | 认领／轮换、自动重连、网络到 Gate 的任务适配、SQLite／持久账本、恢复扫描、真实进程或 Docker 执行、Web、多节点容量与平台兼容；这些继续留在原路线图 |
| 来源与变更 | 继承的目标规则、实施时技术选择和事后补录分别见[实际规格第 7 节](../specifications/m0-channel-probe.md#7-规则来源与实施选择)。约定变更须写明原因、受影响的验收项及验证安排，不能仅改代码或删掉失败标准 |

目标规格是约束来源；实际规格是本切片当前行为的核对基线；下表规定接受这些行为所需的证据。发现两者冲突时先登记偏差并处理，不能自动以“代码目前如此”决定要求。后续工作的开工与变更要求统一见[开发实施计划第 6.1 节](../development-plan.md#61-每轮实施的开工与完成条件)。

## 本轮验收标准

本表从上述范围和行为约定推导验收条件。已有用例是证据入口，不是标准的来源；用例通过只能证明其实际断言。每项结论应记录代码版本、环境、方法、预期与实际结果及剩余偏差。下表是 2026-11-06 对 `98d9c13` 的覆盖核对，历史执行结果见后文；“有用例”不等于本日已执行或整行已通过。

| 编号 | 必须达到的可观察结果 | 现有证据入口与待补项 |
| --- | --- | --- |
| AC01 构建与复验 | 固定依赖可校验，构建得到 `channelprobe`；全量测试、静态检查、契约和文档检查无失败；运行条件可复现 | 后文复验命令及 go.mod/go.sum；结果必须绑定具体代码版本，环境失败和跳过单独记录 |
| AC02 CLI 与材料 | 按实际规格接受参数和身份材料；正确运行输出规定 JSON；错误返回约定诊断与退出码；输出不泄露 token、私钥或原始错误输入 | `main_test.go`、`process_test.go` 已覆盖参数拒绝、令牌错误、双进程输出等；全部诊断分支及 1／2 退出码的精确对应尚待逐项核对 |
| AC03 传输与认证 | 两个 OS 进程经验证 CA／名称的 TLS 完成握手；拒绝 TLS 1.1、不信任证书、错误身份／令牌及 Cookie／Origin／query／重复 Authorization；路径、方法、重定向和压缩按规格处理 | `TestManagerAndAgentProcessesUseVerifiedTLS`、`TestTLSAndIdentityRejection`、`TestUpgradeRejectsAlternateCredentials` 有部分直接断言；路径／方法、空 query、不跟随重定向、禁用压缩及 no-store 的针对性验证待补 |
| AC04 握手与会话 | hello/welcome 在各自 10 秒期限内核对身份及关联；错误 welcome 被拒绝；替换连接后旧连接关闭且不清除新 session；等待 hello 的连接也能随服务器关闭 | `TestSessionReplacementClosesOldConnection`、`TestServerCloseIncludesPendingHello` 等；10 秒截止及错误 welcome 的专门用例待补 |
| AC05 冻结与能力 | capabilities 为空、welcome 要求核对；Manager 按支持矩阵返回完整 `prototype_frozen` 诊断，不推进任务；Agent 收到握手后应用消息按规格关闭 | `TestTrustedHandshakeRemainsFrozen`、`TestFrozenSessionReturnsErrorWithoutStarting` 覆盖部分字段；诊断完整 body／session 和 Agent 应用消息拒绝待补；Gate 未接入须结合代码结构审阅 |
| AC06 消息与去重 | 错方向／旧 session／重复 hello／内容冲突关闭；等价 JSON 重试保持同一含义；指纹包含 hello，最多 1024 项，满表后下一条消息关闭且不再增长 | `TestPostHandshakeInvalidMessagesClose`、`TestHelloMessageIDCannotBeReused`、`TestEquivalentJSONRetryKeepsSameMeaning`；1024 项精确边界、满表后重复消息、业务消息同 ID 改内容及关闭 reason 待补 |
| AC07 解析与输入 | 21 正例接受、11 反例拒绝；非法 UTF-8、重复键、尾随 JSON、超限分片、非法编码／路径／冻结包被拒绝；极端数字不 panic 或舍入成合法整数 | `validator_test.go` 与 `TestFragmentedMessageBudget`；全部大小／数量边界及 Agent 接收路径仍须逐项核对，不能由文件级覆盖推定通过；不包含真实文件系统或 Compose 行为 |
| AC08 探活与清理 | 15 秒发 Ping 不受前一 Ping 等待阻塞；Pong 更新 45 秒存活期限；无 Pong 到期关闭；4 个在途预算单独报错；退出回收等待者 | `heartbeat_test.go` 使用可控时钟验证边界；本项允许此方法，不能据此报告真实网络丢包实验完成 |
| AC09 许可与重放 | session／operation／digest／nonce／reply_to／许可身份全部匹配才允许开始；10 秒到期拒绝，重发不延时；已登记请求关联均有效；重复许可只产生一次提交和一次 Start | `gate_test.go` 的关联、窗口、重复和许可冲突用例；统计的是受控回调次数，不是 Docker 执行次数 |
| AC10 提交竞争与失败 | 提交前后均复核；提交期间 Cancel／Invalidate 不等回调返回；取消、失效、到期或提交失败后不 Start；提交失败不重试；断线保留已开始事实并拒绝旧许可重放；waiting 且未过期时匹配 Deny 阻止后续开始，无关 Deny 不改变状态，提交中或 started 后的 Deny 不覆盖已有事实 | `TestCommitFailureNeverStartsOrRetries`、`TestExpiryDuringCommitPreventsStart`、`TestInvalidationAndCancellationDoNotWaitForCommit`、`TestConcurrentDuplicateCannotEnterCommitTwice`；另有 `TestDenyBeforeAuthorizationPreventsStart`、`TestUnrelatedDenialCannotCancelPendingRequest`、`TestDenialDuringCommitCannotOverwriteCommitment`、`TestLateDenyAndCancelCannotOverwriteStartedFact`；当前 Invalidate 由测试调用，不能证明网络已正确驱动 Gate |
| AC11 可交接资料 | 接手者可定位相同版本的源码、依赖、规格、验收结果与运行命令；知道每个未通过／未验证项以及能否继续依赖它开发；文档与当前代码一致 | 本篇、实际规格、README 与测试计划必须同步；交接状态不得笼统写为“测试通过，M0 完成” |

### 完成条件与当前结论

只有 AC01—AC11 所列条件逐项有相符证据、偏差已处理、复验结果已记录，才可将本切片标为“验收完成”。直接影响身份、冻结、许可、重复执行或错误结论的失败阻塞完成；其他未验证项也不能默认通过。若要改变本轮范围，须说明原标准、调整理由、影响与承接位置后修订基线，不能为了勾选完成而删项。

当前结论为：**源码和既有测试资料可供接手复验；按本日补建的验收基线，切片验收仍未完成。** AC02—AC07 已列出待补的分支、时限和边界证据，其余项也须按具体断言记录结论，不能从测试文件存在推定全部满足。本次补建规格和完成判据，尚未补齐这些待验收项。

允许的交接用途是继续开发与技术复验，交付内容为对应版本的源码、锁定依赖、可构建探针、测试、规格和运行说明；它不包含产品安装包或 Docker 管理能力。CGO race 未运行、Linux／FreeBSD 未验证、真实网络与持久任务未验证继续显式保留。AC08／AC09 采用受控测试的边界从本表定义，不把它们扩称真实节点证明。

整个 M0 仍须补技术组合、身份生命周期、持久化与恢复、文件／Compose 原生对照等证据，O02／O03／O04／O07 不因本切片验收而关闭。O12 是后续解除隔离与产品交付的恢复条件，不新增为当前冻结原型的验收要求；O05／O06 仍按原迁移／存储阶段推进。下一轮持久任务工作必须另有具体实施约定和验收表，先解决本表中会影响它的缺口，再实现依赖行为；不能直接把“接入账本”一句话当作开工规格。

## Global Constraints

- TLS 最低 1.2，验证 CA 与服务器名称；只接收 Bearer，不接收 Cookie、URL token、浏览器 Origin。
- hello/welcome 期限 10 秒；文本消息上限 8 MiB；禁用压缩；拒绝未知字段、重复 JSON 键及非法 UTF-8。
- 同节点只有一个有效 session；重连必须核对，未实现恢复扫描时维持冻结。
- 开始许可窗口 10 秒，绑定 session、operation、input_digest、nonce 和请求关联；同 nonce 重发不延长期限。
- 通道程序报告空能力并拒绝部署报文；不连接 Docker，不实现认领、SQLite 账本、完整恢复或产品 Web。

## Review Focus

- 无效凭据和跨节点 hello 在任何业务消息之前被拒绝。
- 旧连接退出不能误删新的有效 session；重连不能被当作新节点开放任务。
- 畸形 JSON、错误方向、旧 session、二进制帧及超预算消息被明确拒绝。
- 同 nonce 的多个 request message_id 均可正确关联；重复许可不重复调用执行器。
- 持久开始回调前后期限、取消或连接失效时，执行器不能被调用；持久提交后不能把不确定性当作允许重试。

## Task 1: 复用契约的 Go 校验器

以下勾选保留当时的实施任务执行记录；任务执行不等于上方 AC01—AC11 已完成验收。

文件：`contracts/embed.go`、`internal/protocol/validator.go`、`internal/protocol/validator_test.go`。

接口：`NewValidator() (*Validator,error)`，`Validate([]byte) (Envelope,error)`，`NewMessage(type,session,reply string,body any) ([]byte,error)`；Envelope 的 body 保留原始 JSON。

- [x] 先运行缺少实现的测试，确认现有21个正例、11个反例及重复键、非法 UTF-8、尾随 JSON 的断言能阻止错误输入。
- [x] 实现本地嵌入 Schema，禁止校验时访问外部 URL；所有消息复用同一主定义。
- [x] `go test ./internal/protocol` 通过。

## Task 2: TLS 通道和许可门禁

文件：`internal/channel/`、`internal/startgate/`、`cmd/channelprobe/`。

最终通道接口：`NewServer(Config) (*Server,error)` 返回 HTTP Handler；`Connect(context.Context,ClientConfig) (*Peer,error)` 完成应用握手；`Peer.Run(context.Context) error` 维持读循环与探活，拒绝后续应用消息。门禁最终接口为 `New(Request,Hooks)`、`RegisterRequest`、`Authorize`、`Deny`、`Cancel`、`Invalidate`、`State`，输入、返回及状态转换在实际实现规格第 5 节维护。

- [x] 先编写并运行失败测试：TLS 信任、认证来源、hello 身份、会话替换及重连冻结、非法消息关闭。
- [x] 使用真实回环 TLS WebSocket 实现握手和受限读循环；命令行仅用于原型，两端配置使用文件提供秘密。
- [x] 先编写并运行失败测试：到期／错会话／错摘要／错关联／重复／取消，以及持久回调期间失效；再实现许可门禁。
- [x] 使用测试子进程完成 Manager／Agent 握手；不以进程内模拟代替该项证据。
- [x] `go test ./...`、`go vet ./...` 和原有静态检查通过；独立审阅后修正重要问题。

## 执行记录

2026-11-04：原仓库现有契约和文档检查通过。原生工具创建的隔离目录不能正常执行 Git status，且项目外参考依赖原路径；本轮回到原仓库实施，保持既有提交基线。仅实际运行并查看结果后勾选任务。

已完成固定版本 [Portainer 源码对照](../research/portainer-agent-reference.md)。据此保留连接／应用握手／任务结果的分层，补充同 nonce 的固定许可和关联规则；没有照搬上游的身份重置或重启后简单改 Error 的处理。

测试观察过缺失行为的失败，再实现并复验。独立审阅发现并修正：探活未按最后 Pong 独立计时、hello 未参与去重、JSON 格式变化被误判为新内容、尚未完成 hello 的连接未随 Server.Close 关闭，以及极端数字触发依赖库 panic。上述问题均补有回归；方向测试也修正为合法形状的反向报文，分片超限在双向读写中核对 1009。

数字解析现采用每 token 128 字节、指数绝对值≤128 的预算，再精确判定安全整数；避免浮点舍入和任意精度解析的资源消耗。具体值已同步通信规格。探活使用独立 15 秒发送、最后 Pong 后 45 秒期限；原型最多保留 4 个未完成 Ping，耗尽时报独立 transport budget 错误，不冒充“45 秒没有 Pong”。每连接指纹表最多 1024 项，包含 hello；满后再收到任何握手后消息均关闭，重复 ID 在表未满时不增加项数。该预算不是产品吞吐量指标。

## 复验与手工运行

仓库根目录执行：

```powershell
go test ./... -count=1
go vet ./...
go build -o bin/channelprobe.exe ./cmd/channelprobe
go mod verify
pwsh -NoProfile -File tools/check-contracts.ps1
node tools/check-docs.mjs
```

两进程测试自动生成临时证书与测试身份，不需要真实节点或 Docker。沙箱需允许回环套接字；网络权限错误不是用例通过。依赖获取曾遇官方模块代理超时，本轮使用模块镜像下载同一固定版本，保留 Go 校验和验证；未修改全局 Go 配置。

手工运行需要自备包含 `127.0.0.1` IP SAN 的证书、对应私钥、两端相同的 UUIDv4 node/ledger 标识，以及编码为 43 字符 Base64url 的 32 字节随机 node.token。证书、私钥与令牌不进入仓库。以下命令中的标识需替换为实际值：

```powershell
go run ./cmd/channelprobe -mode manager -listen 127.0.0.1:8443 -cert cert.pem -key key.pem -token-file node.token -node-id <node-uuid> -ledger-id <ledger-uuid> -duration 30s
go run ./cmd/channelprobe -mode agent -url wss://127.0.0.1:8443/agent/v1/connect -ca cert.pem -token-file node.token -node-id <node-uuid> -ledger-id <ledger-uuid> -duration 20s
```

Manager 输出 listening；Agent 完成应用握手后输出 handshake、session_id 和 `reconcile_required=true`，不打印秘密。命令只连一次；认证失败返回 unauthorized，断线退出，尚无自动重连。Manager 仅允许字面量回环地址。

## 当前证据与限制

2026-11-06 复验：代码仍为 `98d9c13`，本次仅改文档；Windows/amd64、Go 1.27.0、CGO=0。`go test ./... -count=1 -timeout=60s` 四个测试包全部通过，`go vet ./...`、`go build -o bin/channelprobe.exe ./cmd/channelprobe`、`go mod verify` 通过。首次复验因官方模块代理连接超时而有三个包未能进入测试，独立 startgate 包通过；随后只在当前进程设置 `GOPROXY=https://goproxy.cn` 获取锁定依赖并完成全量复验，未修改全局配置。契约检查为 20 类消息、21 正例、11 反例、1 个解码输入通过；文档检查为 20 篇、235 个本地链接、93 个表格通过。该结果只刷新已有用例证据，未关闭上方 AC02—AC07 的待补验证，切片验收结论仍为未完成。

`go test ./...` 的四个测试包、`go vet ./...`、原型构建和 `go mod verify` 已通过；contracts 为纯嵌入资源包。21 个原正例与 11 个原反例同时由 PowerShell 和 Go 检查。两个独立 OS 进程完成真实 TLS 握手；探活边界使用可控时钟验证，未宣称真实等待 45 秒的全网络丢包实验。

当前 CGO 关闭，race 检查无法运行。已有并发行为测试不等于竞态检测。未验证 Linux／FreeBSD 部署、真实网络代理与 NAT、认领／轮换、自动重连、数据库、任务完整报文流程、进程崩溃恢复、Docker 副作用或人工解锁；独立 Gate 未接入网络，CommitStart 只是受控回调，不能证明耐久性或持久 not_started。

下一步先把持久操作账本与固定许可／事件记录接入受控任务通道，再做断线和重启实验；在这些依据成立前，通道保持冻结，不增加真实部署能力。
