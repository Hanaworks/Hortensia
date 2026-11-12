# M1 HTTP适配器实施与验收

2026-11-12事前基线保留如下；同日已实现适配器并记录后文实际验收。依据[适配器规格](../specifications/m1-http-adapter.md)及[HTTP主定义](../specifications/http-api.md)，先验证边界拒绝，再接真实服务内核。测试替身只证明编码／分派，不替代真实账号、权限及事务验证。

| 编号 | 入口与通过条件 | 编码前状态 |
| --- | --- | --- |
| HA01 | 全部主路由按模板分派；错误方法／未知路径／路径别名不调用内核，非法UUID、重复或未知query返回固定错误 | 未验证 |
| HA02 | 重复键／坏UTF8／未配对代理项／尾随／未知DTO字段／错误Content-Type／超8MiB均拒绝；不截断或透出秘密 | 未验证 |
| HA03 | 真实登录返回安全Cookie，GET me返回同CSRF和当前修订；错Origin/CSRF及重复头无变更；有效与无效会话退出分别符合约定 | 未验证 |
| HA04 | K及If-Match头、两种limit预算、输出after和操作node_id严格解析，准确传入内核；原key幂等与CAS结果通过真实Store核对 | 未验证 |
| HA05 | Agent两路由拒绝浏览器Origin、错Bearer及额外字段；实际认领同候选重试／轮换符合内核结果且秘密不回显 | 未验证 |
| HA06 | 按直接来源与账号／token摘要的第11次失败被429限制，Retry-After准确；伪造代理头不能换来源；过期及10000桶容量受控 | 未验证 |
| HA07 | 所有成功／错误均no-store且白名单，204无正文；内部错误固定500，无SQL／请求正文；超时／断开不会补写业务失败 | 未验证 |
| HA08 | TLS真实Store集成覆盖登录→账号与授权→节点→保存／读取版本→离线Apply拒绝及回执；撤权后无配置／结果泄漏，独立审阅与主规格一致 | 未验证 |

完成时回填实际测试、版本和限制；正式任务通道／Docker及Web端到端验收继续归首版V01—V08。

## 2026-11-12实施后验收

Windows amd64、Go 1.27.0，本机临时SQLite v4／TLS。首轮结构测试在实现缺失时失败；实现后依次修复尾斜线误识别为非法ID、标准Cookie解析忽略非法重复项、慢正文超时后清除deadline导致继续排空正文。后两项均保留专门回归测试。所用生产文件为`internal/manager/httpapi/{handler,routes,limiter,errors}.go`；HTTP不访问SQL、不缓存授权、不实现业务事务。

交接时HEAD为`2d13d73`，实现尚在共享工作区，正式提交号由根任务合并验收后记录；不能用该HEAD表示包含本模块全部实现。

| 编号 | 本次结果 | 观察证据与限制 |
| --- | --- | --- |
| HA01 | 通过 | 27个主路由均在真实TLS集成取得成功响应；23个需会话路由逐一断言无会话401及错误query400，登录／退出／两Agent路由另测来源与认证；未知路由、方法、路径别名无Backend调用 |
| HA02 | 通过 | `TestRejectsStructureBeforeBackend`覆盖坏编码、重复键、安全整数、尾随、BOM、错误媒体类型、额外登录字段和8MiB超限；Agent精确字段与Service内部严格DTO继续各自验证；超限不截断受理 |
| HA03 | 通过 | TLS真实登录断言Cookie完整属性、GET me同CSRF/ETag；错来源／CSRF及重复CSRF无节点创建；非法／重复Cookie401；有效退出后旧Cookie401，再退出204清Cookie |
| HA04 | 通过 | K／强ETag缺失、重复、弱值、通配值拒绝；两类limit及after精确传入；真实版本保存CAS412、原key重放同身份／同正文、GET submissions取得原回执 |
| HA05 | 通过 | 两Agent路由来源／重复Bearer／Cookie冒充／额外字段拒绝；真实认领相同候选重试同绑定、同key认领token不再展示、轮换旧秘密401且响应不含秘密 |
| HA06 | 通过 | 来源IP忽略伪造转发头；跨IP相同账号／Agent token第11次失败429；Retry-After与窗口过期断言；小容量等价测试证明容量满不驱逐、在途并发不超额；正式构造容量10000，未做负载基准 |
| HA07 | 已测边界通过 | 全TLS响应检查no-store/JSON/nosniff，204无正文；未知错误及恶意内部Message不泄密；30秒context和断开取消到达Backend；100ms真实慢正文及时停止且不调用Backend，keepalive下一请求不继承旧deadline；提交后断网丢响应未另做网络故障注入 |
| HA08 | 本机集成通过，审阅待根任务汇总 | 真实TLS覆盖账号授权／认领轮换／保存读取版本／离线Apply503无排队／回执；真实Agent账本受控hello、认证绑定、扫描完成后Apply202、GET Operation／列表、取消保持accepted、输出空页、撤权403/404。独立审阅与整仓最终验收由根任务汇总 |

最后模块核验：`go test ./internal/manager/httpapi -count=1 -timeout=60s`通过（3.469s）；`go vet ./internal/manager/httpapi`通过。该模块测试需允许本机回环连接；初次受执行环境限制出现socket access forbidden，获准本地TLS测试后正常执行，未放宽TLS验证。

曾执行`go test ./... -count=1`，除`cmd/channelprobe`外各包通过；`TestManagerAndAgentProcessesUseVerifiedTLS`在process_test.go:228失败，原输出为`untrusted TLS was not rejected with a fixed diagnostic`（33.96s）。单独定向复验该项通过（2.29s），没有修改channelprobe文件；失败根因尚未确定，不能用单项复验销掉整仓失败，完整最终验收留根任务统一执行。HTTP模块后续慢正文补充后的全量模块检查已通过。

范围边界：本次是HTTP源码和本机TLS／持久业务组合交接，不是M1发布。Agent身份／恢复消息仍由测试组合调用，没有真实WebSocket运行时或Docker执行；output测试覆盖受理中空页，非空最终输出UTF8切块依赖核心自己的验证。未验证跨机部署、真实浏览器完整流程、代理转发部署、掉电耐久、旧库迁移、持续高并发或资源耗尽压测。后续依赖为根任务运行入口／独立审阅／整仓验收，随后真实Agent执行与Web首条完整流程。

根最终汇总：源码已纳入af43699；独立审阅另发现classify／parseRequest早拒绝未消费正文的问题，先记录规格并实际RED后，对所有正文读取前拒绝加Connection: close，真实TLS复验通过。静态入口对应问题由运行模块修复。当前四类运行边界缺陷均经独立复核关闭；精确af43699的1347条Go测试／子测试、vet、build、mod verify及Linux交叉构建结果见[运行验收](m1-manager-runtime.md#2026-11-12本机实现与验收)。HA08中的Agent仍为受控库组合，不据此改为真实部署通过。
