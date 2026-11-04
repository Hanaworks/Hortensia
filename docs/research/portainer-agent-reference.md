# Portainer 连接、交互与执行异常源码对照

阅读日期：2026-11-04。用途：为 M0 原型和后续实现提供具体代码依据，承接 D10—D13。以下区分上游源码事实、本项目取舍和待验证事项；本轮未运行 Portainer。

## 1. 固定版本与阅读边界

- Portainer 管理端：2.45.3，提交 `0fa764dc81c76bb7914d65d8e62b026d7c746899`。阅读普通 Compose 创建／更新、结果落库、启动恢复和 Agent HTTP 代理。
- Portainer Agent：2.39.7，提交 `2faa9a2b144d818de069ed31955f086d5e9ad5d5`。阅读标准 API、同步 Edge 轮询与隧道、鉴权和状态重报。两个仓库分别固定研究版本，未把它们当成已验证的配套部署组合。
- Agent 的 Chisel 依赖：v1.11.6，提交 `b9d12191f6346c0a2ecd359b21a2f5f2c51dbfdd`。阅读连接循环、握手、退避和保活。版本来源见 [Agent go.mod](https://github.com/portainer/agent/blob/2faa9a2b144d818de069ed31955f086d5e9ad5d5/go.mod#L24)。

未覆盖完整 Edge Async、Swarm／Kubernetes、Docker daemon 取消行为或真实崩溃实验。本篇不评价整个上游产品的恢复保证；结论只对应所列控制流。PVE 的交互参考仍按既定需求开展，本轮没有据此声称已核对 PVE 源码或界面。

## 2. 连接与身份

标准 Agent 的 `APIServer.Start` 提供入站 TLS HTTP API；所读同步 Edge 路径通过轮询状态，按需建立到本地 API 的反向隧道。这与本项目“Agent 主动建立 WSS，交换结构化任务”的方案不同。[API 服务](https://github.com/portainer/agent/blob/2faa9a2b144d818de069ed31955f086d5e9ad5d5/http/server.go#L68-L108)、[Edge poll/manageUpdateTunnel/createTunnel](https://github.com/portainer/agent/blob/2faa9a2b144d818de069ed31955f086d5e9ad5d5/edge/poll.go#L232-L341)

借鉴连接层与业务操作分层。本项目保留主动 WSS，身份检查在 Upgrade 与 hello 两层完成；具体任务仍检查 session、操作、摘要和许可。本轮通道原型分别验证错误凭据、节点／账本不符及旧会话替换；尚未实现认领和轮换。

上游 `DigitalSignatureVerification` 在进入 handler 前验证签名，相关 ECDSA 实现签的是固定消息或 shared secret 的摘要；不能据此认为每次请求正文和操作身份都已绑定。其模式及 FIPS 分支也有差异。本项目借鉴“业务前鉴权”的结构，继续使用自己的 TLS／Bearer 和任务许可协议。[鉴权中间件](https://github.com/portainer/agent/blob/2faa9a2b144d818de069ed31955f086d5e9ad5d5/http/security/notary.go#L21-L49)、[ECDSA 实现](https://github.com/portainer/agent/blob/2faa9a2b144d818de069ed31955f086d5e9ad5d5/crypto/ecdsa.go#L51-L111)

## 3. 建连、保活与重试不是同一个状态

Agent `CreateTunnel` 调用 Chisel `Start` 后设置 tunnelOpen；Chisel `Start` 启动后台循环，真正连接还要经过 WebSocket、SSH 和配置握手。因此这一标志表示客户端生命周期，不单独证明可执行工作。[Agent 隧道客户端](https://github.com/portainer/agent/blob/2faa9a2b144d818de069ed31955f086d5e9ad5d5/chisel/client.go#L68-L134)、[Chisel Start](https://github.com/jpillora/chisel/blob/b9d12191f6346c0a2ecd359b21a2f5f2c51dbfdd/client/client.go#L239-L263)、[connectionOnce](https://github.com/jpillora/chisel/blob/b9d12191f6346c0a2ecd359b21a2f5f2c51dbfdd/client/client_connect.go#L67-L134)

本项目区分传输握手、应用身份核对和恢复完成。原型的 welcome 始终声明需要核对，并保持冻结；连上后不提供部署能力。后续的 ready 必须满足原生预检与账本要求。

Agent 为 Chisel 配置最大 30 秒重试间隔和无限重试；Chisel 连接存活超过 5 秒才重置退避。Edge poll 另有失败后的随机错峰。这些是不同循环。上游的 keepalive 使用 SSH ping，也不是本项目的 WebSocket Ping/Pong。[隧道参数](https://github.com/portainer/agent/blob/2faa9a2b144d818de069ed31955f086d5e9ad5d5/chisel/client.go#L18-L22)、[退避循环](https://github.com/jpillora/chisel/blob/b9d12191f6346c0a2ecd359b21a2f5f2c51dbfdd/client/client_connect.go#L20-L65)、[SSH 保活](https://github.com/jpillora/chisel/blob/b9d12191f6346c0a2ecd359b21a2f5f2c51dbfdd/share/tunnel/tunnel.go#L178-L193)

采用独立阶段计时和有上限的退避思路；保留本项目现有时间参数和 401 停止重试规则。快速反复掉线不立即重置退避，列为后续重连调度实验；本轮命令仅做单次连接，尚未实现自动重连循环。

## 4. 重报与重新执行

`SetEdgeStackStatus` 对网络错误和 5xx 重发已编码状态报告；`mutateResponseForCaching` 清除缓存中的 ForceRedeploy，避免旧控制标志反复触发动作。另一个 poll 错误分支会重设 endpoint ID 并重置 stack 跟踪，属于其具体策略。[状态重报](https://github.com/portainer/agent/blob/2faa9a2b144d818de069ed31955f086d5e9ad5d5/edge/client/portainer_edge_client.go#L260-L336)、[缓存处理](https://github.com/portainer/agent/blob/2faa9a2b144d818de069ed31955f086d5e9ad5d5/edge/client/portainer_edge_client.go#L596-L615)、[poll 错误分支](https://github.com/portainer/agent/blob/2faa9a2b144d818de069ed31955f086d5e9ad5d5/edge/poll.go#L232-L251)

本项目采用“重报事实不重复副作用”的划分，依赖持久 operation_id/event_seq 与 ack/finalize；断线不删除身份和执行证据。本轮仅验证独立内存许可门禁的重复调用保护；结果重报、持久去重和恢复扫描仍待实现。

## 5. 管理端受理、执行与结果落库

管理端 `BuildAndAsyncDeploy` 先保存 Deploying，再异步执行并返回 Stack；后台从 Background 派生 15 分钟期限，不绑定浏览器请求生命周期。创建应答与最终部署结果因此是不同事实。[director.go](https://github.com/portainer/portainer/blob/0fa764dc81c76bb7914d65d8e62b026d7c746899/api/stacks/stackbuilders/director.go#L29-L53)

普通 Compose 路径使用内嵌 Compose 库的 Build/Up，不是本项目的受监督 CLI 模型。部署锁、模型加载与实际动作也并非同一取消边界。保留本项目对 interrupted/unknown 的独立定义，不借用其超时数值或错误映射作为实测结论。[Compose 调用](https://github.com/portainer/portainer/blob/0fa764dc81c76bb7914d65d8e62b026d7c746899/pkg/libstack/compose/composeplugin.go#L37-L170)、[部署锁](https://github.com/portainer/portainer/blob/0fa764dc81c76bb7914d65d8e62b026d7c746899/api/stacks/deployments/deployer.go#L61-L78)

后台执行完成后另开事务保存状态，落库失败可能保留旧 Deploying。上游将执行 error 映射为 Error、nil 映射为 Active；这不等于本项目的正常退出、完整观察及归属一致判据。[结果保存](https://github.com/portainer/portainer/blob/0fa764dc81c76bb7914d65d8e62b026d7c746899/api/stacks/stackbuilders/director.go#L106-L138)、[状态映射](https://github.com/portainer/portainer/blob/0fa764dc81c76bb7914d65d8e62b026d7c746899/api/stacks/stackutils/stack_status.go#L24-L43)

## 6. 重启与代理错误的边界

所读 `recoverStaleDeployingStacks` 将遗留 Deploying 改为 Error，并记录重启中断；该函数没有核实 Docker 执行结果。其更新 Error 栈路径可先 Undeploy 再 Deploy。本项目保持 unknown 隔离与占用，不能用这个状态转换证明安全重跑。[启动恢复](https://github.com/portainer/portainer/blob/0fa764dc81c76bb7914d65d8e62b026d7c746899/api/cmd/portainer/main.go#L713-L746)、[Error 栈更新](https://github.com/portainer/portainer/blob/0fa764dc81c76bb7914d65d8e62b026d7c746899/api/http/handler/stacks/stack_update.go#L414-L457)

Agent 代理 transport 注入签名再调用 RoundTrip；反向代理错误映射为 HTTP 502。这只能说明代理失败，不能证明远端未执行。[transport](https://github.com/portainer/portainer/blob/0fa764dc81c76bb7914d65d8e62b026d7c746899/api/http/proxy/factory/agent/transport.go#L26-L44)、[502 处理](https://github.com/portainer/portainer/blob/0fa764dc81c76bb7914d65d8e62b026d7c746899/api/http/proxy/factory/reverse_proxy.go#L43-L50)

后续故障实验应在“远端已经产生副作用但响应丢失”“执行结束而结果落库失败”“Manager 从旧状态重启”处注入故障，验证不会误判 not_started 或释放占用。这些场景并未在本轮网络握手实验中执行。

本轮实现、命令与验证证据见 [M0 实施记录](../implementation/m0-agent-channel.md)。
