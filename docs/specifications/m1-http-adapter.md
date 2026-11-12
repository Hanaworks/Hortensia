# M1 HTTP适配器实施规格

2026-11-12编码前保存。路由、对象、权限、错误和重试语义以[HTTP主定义](http-api.md)为唯一主定义；本篇说明`internal/manager/httpapi`如何把不可信HTTP转换为[服务内核](m1-manager-service.md#101-2026-11-12编码前冻结的go接线边界)调用。状态待实施，验收见[适配器记录](../implementation/m1-http-adapter.md)。

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

## 4. 返回、错误与生命周期

每个请求生成UUID request_id；错误只输出固定category/code/message、空或明确白名单field_errors及retry_action。未知内部error统一500，不返回err.Error、SQL、配置或原始请求。ServiceError按主规格映射，Retry-After使用正整数。所有API响应包括错误均为Cache-Control: no-store，JSON Content-Type及nosniff；204无正文。ETag和Location只来自内核白名单结果，ETag已含双引号。

普通API附30秒context，客户端断开或超时取消处理但不撤销已经提交的业务事实。适配器没有后台执行或数据库生命周期，关闭由运行模块先停止HTTP再关闭Store。未来WebSocket路由单独接线，不套普通请求总时限；静态Web由运行组合处理器接线，本适配器不会把未知API回退为index.html。

排障顺序：路径和request_id → 固定输入错误 → Cookie／Origin／CSRF → 限速 → 服务错误分类 → 内核事务证据。修改路由必须同步主规格、前端和路由覆盖测试；修改请求规范化必须复核幂等摘要，不能为了同key通过而忽略秘密字段。
