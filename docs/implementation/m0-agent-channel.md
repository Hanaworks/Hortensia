# M0 Agent 通道原型实施记录

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 用现有 v1 报文验证真实 TLS WebSocket 连接及独立的开始许可门禁，为后续持久化和 Docker 实现提供可重复证据。

**Architecture:** 管理端与 Agent 的协议测试程序共享本仓库 Schema 验证器；连接只完成认证、hello/welcome、探活和会话替换。开始许可在独立受控测试中验证，不将内存状态伪装成持久账本。

**Tech Stack:** 本机 Go 1.27.0；coder/websocket v1.8.15（ISC）、santhosh-tekuri/jsonschema/v6 v6.0.3（Apache-2.0）、regexp2 v1.11.5（MIT）；间接依赖 x/text v0.14.0。版本与校验和保存在 go.mod/go.sum。采用 Schema 库官方的 ECMAScript 引擎接入方式，正则匹配超时 100 ms 后拒绝，不手写一套 Path 正则翻译。这是本轮 Windows/amd64 验证组合，其他工具链和操作系统尚未验证。

**Spec:** [通信规格](../specifications/agent-protocol.md)、[执行与恢复](../specifications/execution-recovery.md)、[消息 Schema](../../contracts/agent-v1.schema.json)。用户已要求继续推进现有方案，本轮沿既定 M0 范围实现。

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

文件：`contracts/embed.go`、`internal/protocol/validator.go`、`internal/protocol/validator_test.go`。

接口：`NewValidator() (*Validator,error)`，`Validate([]byte) (Envelope,error)`，`NewMessage(type,session,reply string,body any) ([]byte,error)`；Envelope 的 body 保留原始 JSON。

- [x] 先运行缺少实现的测试，确认现有21个正例、11个反例及重复键、非法 UTF-8、尾随 JSON 的断言能阻止错误输入。
- [x] 实现本地嵌入 Schema，禁止校验时访问外部 URL；所有消息复用同一主定义。
- [x] `go test ./internal/protocol` 通过。

## Task 2: TLS 通道和许可门禁

文件：`internal/channel/`、`internal/startgate/`、`cmd/channelprobe/`。

通道接口：`NewServer(Config) (*Server,error)` 返回 HTTP Handler；`Connect(context.Context,ClientConfig) (*Peer,error)` 完成应用握手；`Peer.Run(context.Context)` 维持读循环与探活。门禁接口由其局部实现确定，必须覆盖本计划 Review Focus。

- [x] 先编写并运行失败测试：TLS 信任、认证来源、hello 身份、会话替换及重连冻结、非法消息关闭。
- [x] 使用真实回环 TLS WebSocket 实现握手和受限读循环；命令行仅用于原型，两端配置使用文件提供秘密。
- [x] 先编写并运行失败测试：到期／错会话／错摘要／错关联／重复／取消，以及持久回调期间失效；再实现许可门禁。
- [x] 使用测试子进程完成 Manager／Agent 握手；不以进程内模拟代替该项证据。
- [x] `go test ./...`、`go vet ./...` 和原有静态检查通过；独立审阅后修正重要问题。

## 执行记录

2026-11-04：原仓库现有契约和文档检查通过。原生工具创建的隔离目录不能正常执行 Git status，且项目外参考依赖原路径；本轮回到原仓库实施，保持既有提交基线。仅实际运行并查看结果后勾选任务。

已完成固定版本 [Portainer 源码对照](../research/portainer-agent-reference.md)。据此保留连接／应用握手／任务结果的分层，补充同 nonce 的固定许可和关联规则；没有照搬上游的身份重置或重启后简单改 Error 的处理。

测试观察过缺失行为的失败，再实现并复验。独立审阅发现并修正：探活未按最后 Pong 独立计时、hello 未参与去重、JSON 格式变化被误判为新内容、尚未完成 hello 的连接未随 Server.Close 关闭，以及极端数字触发依赖库 panic。上述问题均补有回归；方向测试也修正为合法形状的反向报文，分片超限在双向读写中核对 1009。

数字解析现采用每 token 128 字节、指数绝对值≤128 的预算，再精确判定安全整数；避免浮点舍入和任意精度解析的资源消耗。具体值已同步通信规格。探活使用独立 15 秒发送、最后 Pong 后 45 秒期限；原型最多保留 4 个未完成 Ping，耗尽时报独立 transport budget 错误，不冒充“45 秒没有 Pong”。原型每连接最多记录 1024 条握手后的诊断消息，达到预算关闭；该限制不是产品吞吐量指标。

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

`go test ./...` 的四个测试包、`go vet ./...`、原型构建和 `go mod verify` 已通过；contracts 为纯嵌入资源包。21 个原正例与 11 个原反例同时由 PowerShell 和 Go 检查。两个独立 OS 进程完成真实 TLS 握手；探活边界使用可控时钟验证，未宣称真实等待 45 秒的全网络丢包实验。

当前 CGO 关闭，race 检查无法运行。已有并发行为测试不等于竞态检测。未验证 Linux／FreeBSD 部署、真实网络代理与 NAT、认领／轮换、自动重连、数据库、任务完整报文流程、进程崩溃恢复、Docker 副作用或人工解锁；独立 Gate 未接入网络，CommitStart 只是受控回调，不能证明耐久性或持久 not_started。

下一步先把持久操作账本与固定许可／事件记录接入受控任务通道，再做断线和重启实验；在这些依据成立前，通道保持冻结，不增加真实部署能力。
