# M1 Manager任务通道实施与验收

2026-11-12事前保存，依据[模块规格](../specifications/m1-manager-agenttransport.md)。先真实TLS正式身份／拒绝测试，再恢复和投递、事件／许可、队列／关闭；最终与Agent正式服务组合，不使用Fixture替代网络身份或解冻。

| 编号 | 可观察通过条件 | 初始状态 |
| --- | --- | --- |
| MT01 | HTTPS正式认领凭据可hello/welcome；错身份／旧代／Cookie／Origin／正文／路径别名拒绝，无秘密错误 | 未验证 |
| MT02 | 空节点完成真实扫描握手后Store可受理；pending需all升级、页错误和丢回复按原请求收敛，不伪造空集合 | 未验证 |
| MT03 | 固定版本prepare由持久投递发送；ready后当前nonce许可，撤权／取消／换session拒绝；写成功不confirmed | 未验证 |
| MT04 | 真实started/result收到持久ACK，finalize/finalized后双端claims释放；丢包/重连保留原文，不第二次开始 | 未验证 |
| MT05 | observation只按已发送query关联，异常/过期停止旧上下文重连；较新result仍通过Store，旧页不能释放 | 未验证 |
| MT06 | 队列32及单配置包边界、重试next_attempt、入站不等待写／执行，ID关联预算有界 | 未验证 |
| MT07 | 同节点替换关闭旧连接，旧disconnect不清新session；Close涵盖握手中及活动WS，所有worker结束后才关Store | 未验证 |
| MT08 | 完整包测试／vet及正式Manager/Agent组合，证据区分受控本机执行和真实Linux/Docker；文档可追到事务与错误 | 未验证 |

当前没有本模块软件验证结果；用户稍后提供Linux环境操作方式，独立本机实现继续。

2026-11-12 MT06修订、实现前：控制重试表必须在入队前限制65536不同ID，已存在ID在预算满时仍按原退避重试；队列满不能留下单配置包忙标记。增加直接边界回归，正常发件与最终确认语义不变。

MT06定向结果：初版测试用rune生成键时代理区间被编码成重复替换字符，未真实达到65536；改用十进制唯一键后，修复前代码通过overlay再次确认RED，最终guard的边界用例与真实TLS恢复握手GREEN（1.027秒）。原文关联表与重试表分别有界，达到预算结束连接，不遗忘旧关联继续接受。

## 2026-11-12本机接线结果

未提交v5工作树的`handler.go`、`connection.go`和正式Manager运行入口已接通。新增8个真实TLS顶层测试；含原有测试及预算项的完整包共11个顶层测试通过（4.541秒），另含18种HTTP拒绝及2种错误hello子例。完整命令和限制见本机`.cache/m1-manager-transport-test-report.md`；最终固定版本回归及独立审阅另追加。

| MT范围 | 本机已观察结果 | 保留限制 |
| --- | --- | --- |
| MT01／MT02 | 正式登录／节点授权／认领、可信TLS、node／ledger匹配、空扫描完成；Cookie／Origin／正文／路径／错凭据等拒绝 | pending→all丢回复及真实跨机另验 |
| MT03／MT04 | 正式固定版本Apply→prepare→ready→精确nonce／reply_to／session许可→Gate一次回调→started1及ACK→受控spawn_failed／not_started2及ACK→Manager failed/pending→finalize／finalized→双方claims空；重连完成且无第二次开始。ready后撤stack.deploy返回deny_start | 启动回调是明确失败夹具，不冒充正常Docker成功；真实进程故障及所有丢包位置未穷尽 |
| MT07 | 同节点替换、旧断开不清新session、轮换后旧代401、Close等待活动及尚未hello的升级socket | 并发握手完成顺序仍在独立审阅 |
| MT08本机组合 | 实际Agent Initialize／Enroll／Serve连正式Handler／Store并经HTTPapi完成空恢复；不存在CLI时caps空，真实HTTP Apply返回422 unsupported且未产生operation | 没有修改生产接受表；真实Linux／Docker／两节点未验 |

MT05和MT06的全部观察关联、队列／退避负例继续独立验证，不因11项测试退出成功将整表标为通过。Handler不持有Compose执行器，页面回复不直接产生结果或释放。

MT07独立审阅修订（修复前）：真实TLS延迟第一个Welcome，第二连接已完成恢复后再释放旧Welcome，旧注册路径会关闭当前新连接，Store返回frozen。原替换测试未覆盖这一完成顺序。将独立探针提升正式LateWelcome回归，再按规格注册临界区重验当前Binding，保留原绑定、exact-session清理和关闭条件。

MT01／RMT02修订（2026-11-12，修复前保存）：合法当前Bearer的TLS GET缺少升级头，原实现返回426 text/plain并暴露第三方诊断，未走固定JSON错误。先提升正式RED，再限定实现upgrade_errors.go和Accept一处接线：拦截升级前非101状态／正文，错误头也隔离；不复制升级头校验，不虚构Hijacker能力；101后错误不得追加JSON，以连接期限收束。验收追加：缺升级头、恶意Version／Key、不可Hijack writer、101后Hijack失败均无原诊断／输入反射，正常正式TLS hello／welcome／恢复保留。先定向RED/GREEN，再完整transport包与目标vet；修复作者自验和后续独立复验分别记录，不扩成M1或真实Linux／Docker通过。

MT01／RMT02作者自验结果：正式四种缺失／恶意升级头先RED（顶层0.39秒，反射Connection／Version／Key），不可Hijack／101后追加错误／失败后保留升级连接三个边界也先RED（包1.642秒）。`upgrade_errors.go`隔离头和正文，库仍唯一验证升级；101后失败只收束I/O。定向GREEN含正常正式TLS恢复为2.358秒；完整transport包16个顶层／24个子例全部通过（6.225秒），目标`go vet`和本轮diff空白检查通过。本次为同一发现者转修复作者的自验，独立核验由根后续记录；不把MT01全部故障矩阵或M1标为通过。
