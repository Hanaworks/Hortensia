# M1 HTTP适配器实施规格

2026-11-12编码前保存，随后依据最终实现补充内部维护说明。路由、对象、权限、错误和重试语义以[HTTP主定义](http-api.md)为唯一主定义；本篇说明`internal/manager/httpapi`如何把不可信HTTP转换为[服务内核](m1-manager-service.md#101-2026-11-12编码前冻结的go接线边界)调用。当前适配器已实现并完成本机TLS与真实Store测试；运行时通道／Docker不属本模块已验证范围，验收见[适配器记录](../implementation/m1-http-adapter.md)。

## 1. 职责与调用边界

`New(Config{Store Backend, ExternalOrigin string}) (http.Handler,error)`返回可供TLS服务器使用的处理器。Backend精确承接Store的Login、AuthenticateBrowser、BrowserMe、Logout、DoService、EnrollNode及RotateNodeCredential；适配器不访问SQL，不缓存用户能力，不自行判断执行成功。接口允许单元测试隔离HTTP编码；正式集成须使用真实v4 Store，不能用替身结果宣称服务已交付。

请求经过路径／方法分类、头与query检查、受限原始正文读取、严格JSON解析、当前会话认证、Origin／CSRF检查、内核调用，最后仅输出内核ServiceReply白名单。账号与Grant最终事务复核仍在内核，HTTP检查不能代替它。登录及Agent引导的失败预算先于昂贵密码验证。读请求不要求Origin；全部浏览器变更精确匹配配置origin，Agent引导拒绝任何Origin头。

## 2. 编码与传输规则

不用会自动清理路径并重定向变更的路由器；只匹配主规格的路径模板，UUID必须小写v4。重复斜线、尾斜线、编码后的路径别名、未知路由或错误方法不调用后端；未知路由／方法使用既定404错误，不增加未约定的405响应。已匹配模板中的非法ID返回400。query只允许该路由定义的字段，重复／未知键及非法百分号编码拒绝。

原始正文最多8MiB，超限413且不截断受理；非空JSON正文必须为application/json（可带charset=utf-8），不接受其他参数、编码或BOM。严格JSON使用protocol.StrictJSON，拒绝重复键、非法UTF-8、未配对代理项、尾随内容及不安全数字。业务字段白名单和语义在内核再次检查；登录／Agent DTO在适配器精确解码后转为类型化参数。空正文路由仅接受缺省或空对象，读请求拒绝额外正文。

普通分页limit为1—200、缺省0交内核采用50；output的limit为1—262144、缺省采用65536，after保持合法非负十进制字节偏移。operations列表必须有node_id。游标签名、权限及过期由内核负责。K路由必须有唯一合法Idempotency-Key；条件路由If-Match必须唯一且为主规格规定的强ETag，缺少返回428。其余路由不把无关头纳入规范请求。

Cookie只读取唯一`__Host-dm_session`，重复Cookie或非法秘密不能取得会话。登录返回的Cookie使用Path=/、Secure、HttpOnly、SameSite=Strict和12小时绝对寿命；服务内核仍执行30分钟空闲和12小时绝对有效期。其他有效会话变更通过BrowserSession.CheckCSRF作常量时间比较；重复CSRF头拒绝。退出无有效会话时仍须合法Origin，返回204并清Cookie；有有效会话必须通过CSRF后注销。

## 3. Agent引导与限速

enroll和rotate只接受唯一Authorization头、Bearer方案及规范43字符SecretToken，拒绝Cookie作为身份和任何Origin头；请求对象沿主规格。原秘密仅作为本次参数，不进入日志或响应。Store认证失败映射401；不在错误中暴露绑定身份。

失败预算按直接连接RemoteAddr规范IP和账号／Bearer摘要分别计数，每个一分钟固定窗口最多10次认证失败；不信Forwarded/X-Forwarded-For。成功不重置已有失败预算。检查时任一预算已耗尽返回429与整数秒Retry-After；只有认证失败增加计数，结构错误不消耗昂贵验证。桶键使用固定长度SHA256摘要、仅存计数和到期时刻，最多10000桶；清理过期项后容量仍满则拒绝新桶，不驱逐活跃限制来放行。此为进程内限速，重启清空不恢复凭据或授权。内核另限制Argon2并发，不能以HTTP桶代替内存成本控制。

2026-11-12实现前细化（HA06）：在途认证临时占用失败预算，完成后释放；仅401计入失败。这样并发请求不能同时通过第十次失败前的检查，成功不会永久消耗预算。窗口从该桶首次请求起算60秒，仍有在途认证的过期桶暂不移除，完成后才能清理；这不延长已经提交的会话或认领材料寿命。容量检查和两键预留共用一把短锁，不持锁调用Store。错误输出只采用已知code对应的固定文案／状态／分类，未知组合统一500；field_errors暂返回空数组，避免内核动态诊断意外泄密。

## 4. 返回、错误与生命周期

2026-11-12独立审阅、修复前补充HA01／HA07：不仅readBody失败，路径分类、query及条件头等正文读取前的拒绝也必须标记Connection: close，防止处理器清理deadline后net/http无时限排空未读正文。不能仅给“慢正文格式错误”一条分支设关闭。正常已消费正文继续允许keepalive，不继承上一请求的读deadline。

每个请求生成UUID request_id；错误只输出固定category/code/message、空或明确白名单field_errors及retry_action。未知内部error统一500，不返回err.Error、SQL、配置或原始请求。ServiceError按主规格映射，Retry-After使用正整数。所有API响应包括错误均为Cache-Control: no-store，JSON Content-Type及nosniff；204无正文。ETag和Location只来自内核白名单结果，ETag已含双引号。

普通API附30秒context，客户端断开或超时取消处理但不撤销已经提交的业务事实。适配器没有后台执行或数据库生命周期，关闭由运行模块先停止HTTP再关闭Store。未来WebSocket路由单独接线，不套普通请求总时限；静态Web由运行组合处理器接线，本适配器不会把未知API回退为index.html。

2026-11-12慢正文边界补足（HA07，实施前）：context本身不能打断阻塞的网络Body.Read；普通HTTP请求同时使用ResponseController将读deadline设为当前请求context的deadline（上层更短期限优先），返回时清除连接读deadline，避免影响下一次keepalive请求。真实TLS慢正文测试通过上层100毫秒期限控制实验时长，必须在正文未发送完时停止读取且不进入Backend。正文失败响应关闭连接，防止net/http在清除deadline后继续排空未读正文；普通成功请求保留keepalive。该处理仅属于本适配器，不放入未来WebSocket接线。

排障顺序：路径和request_id → 固定输入错误 → Cookie／Origin／CSRF → 限速 → 服务错误分类 → 内核事务证据。修改路由必须同步主规格、前端和路由覆盖测试；修改请求规范化必须复核幂等摘要，不能为了同key通过而忽略秘密字段。

## 5. 最终内部结构与维护入口

本节于2026-11-12按实际代码事后补充，不回写为开工前已实现。模块入口是[handler.go](../../internal/manager/httpapi/handler.go)，无数据库句柄、后台goroutine或关闭方法；Backend必须由运行模块提供，ExternalOrigin初始化时要求无路径／query／userinfo的HTTPS origin。构造阶段不发网络请求，不建立会话。返回的handler可并发使用；唯一共享可变数据是失败预算，由一把短时互斥锁保护。

| 内部职责／数据 | 来源、含义与约束 | 维护定位 |
| --- | --- | --- |
| route | 固定方法／模板及空正文、分页、K、条件更新标记；每请求只匹配一条，不清理路径或重定向 | [routes.go](../../internal/manager/httpapi/routes.go)，新增路由需同时补成功、未认证与错误query测试 |
| ServiceRequest | 原方法／规范路径／严格校验的原JSON、K／强ETag及已解析query；Limit=0和After空保留内核缺省语义，不提前生成幂等摘要 | `parseRequest` → `readBody` → Store.DoService；跨边界字段主定义在服务规格§10.1 |
| BrowserSession | 从唯一合法Cookie逐请求取得的不透明句柄；不保存能力快照，CSRF由句柄常量时间验证，业务授权仍在内核事务复核 | `sessionCookie`、ServeHTTP、Store.AuthenticateBrowser；原始Cookie先计数再解析，防止标准库丢弃非法重复项造成身份歧义 |
| 登录／Agent DTO | StrictJSON得到对象后精确检查字段集合与类型，满足Username／Password／Id／SecretToken再传类型化参数；原秘密仅在该请求内存生命周期存在 | `exactStrings`、`login`、`agent`；Agent Cookie不形成身份，任何Origin头均拒绝 |
| failureBucket | SHA256固定长度键 → failures/pending/expires；失败完成计数、在途预留、60秒窗口；没有密码、令牌或原IP正文，重启清空 | [limiter.go](../../internal/manager/httpapi/limiter.go)；两键预留原子执行，满容量不驱逐活跃记录 |
| 错误定义与响应 | 已知code对应固定状态／分类／文案／retry_action；request_id每请求生成；错误字段数组为空；未知服务错误或非法成功状态固定500 | [errors.go](../../internal/manager/httpapi/errors.go)、`writeReply`；禁止改成直接返回err.Error或动态Message |

失败读取／超预算请求关闭连接；完整正文成功读取后的业务错误仍可复用连接。读deadline只限制本请求，结束后清除；30秒context传递到认证和业务事务，已经提交的业务事实不因网络写失败重写。HTTP只写ServiceReply的Body／ETag／Location，不在响应写失败后再调用业务或创建新key。

测试入口分为[结构和预算测试](../../internal/manager/httpapi/httpapi_test.go)、[头／权限入口／时限测试](../../internal/manager/httpapi/boundaries_test.go)及[真实TLS集成](../../internal/manager/httpapi/integration_test.go)。前两者的boundaryBackend只观察是否分派和固定错误，不能作为业务成功证据；集成使用真实v4 Manager、实际密码校验和真实Agent账本。Agent尚无公开hello boot读取API，测试只读其active_boot作为受控hello身份，经正式认领、认证绑定和扫描完成打开恢复屏障，不改frozen。生产HTTP文件不导入SQL；该受控接线不等于真实Agent运行时已交付。
