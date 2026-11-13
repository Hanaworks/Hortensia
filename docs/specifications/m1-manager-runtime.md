# M1 Manager进程与本地运行规格

现行阅读提示（2026-11-14状态校正）：Manager运行模块已实现，新init为schema5；旧schema4只保留显式兼容入口，具体分派见第4节。2026-11-13已在InstaBox完成指定Linux源码回归和正式CLI／TLS／登录／Agent接入及重启子集，见[IB记录](../implementation/m1-instabox-debug.md)。完整Docker部署、跨节点业务隔离及恢复维护仍未验收，不能把本篇规格视为首版交付证明。

2026-11-12编码前基线最初依赖[服务内核](m1-manager-service.md)的显式v4以及[HTTP主定义](http-api.md)，当时尚未实现且Linux环境待安排；该时间范围的历史含义保留，不再作为当前状态。本篇负责一个Manager进程怎样从配置、身份和密钥进入服务，怎样拒绝不确定的初始化状态，以及如何关闭。

## 1. 边界和入口

`cmd/manager`只解析命令和接收退出信号，`internal/manager/service`持有配置、进程锁、Store和HTTP服务器。内核负责数据与授权，HTTP适配器负责请求边界；运行模块不执行SQL、设置节点online或构造执行成功。

命令为`manager init --config <绝对路径> --admin <username>`、`manager serve --config <绝对路径>`。init从交互终端隐藏读取口令并再次确认；受限管道输入使用明确的`--password-stdin`，只读取一行UTF-8口令，不接受命令行口令。输入的行终止符不属于口令，其他空白保留；超长输入拒绝。命令错误只输出固定分类和非秘密文件位置，不打印配置正文、口令、密钥或数据库异常。没有默认账号口令。

配置字段、默认值和含义引用[运行配置](../operations.md#10-m0m1-运行配置工作定义)。本轮不新增环境变量覆盖。JSON最多64KiB；拒绝未知／重复键、错类型、null、非法UTF-8、未配对Unicode转义及尾随内容。省略才用默认值。文件配置路径及文件引用必须为本平台绝对路径。listen必须host:port且端口1—65535；external_origin必须规范HTTPS origin，无用户信息、路径、query、fragment，显式默认端口443不接受。TLS证书和密钥由部署者提供；serve核验密钥匹配和证书对origin主机名有效，没有insecure模式。

## 2. 本地文件与持久身份

数据目录取database_file父目录，密钥目录取keys_dir。程序仅创建指定叶目录及缺少的父目录，不改变已有父目录权限；数据／密钥叶目录须由当前服务身份私有持有。Linux目录0700、秘密和数据库文件0600；Windows使用操作系统权限环境并单独记录验证限制，不把Unix mode位当作Windows ACL证明。配置路径中的符号链接／重解析点和最终身份、锁、密钥、数据库的链接拒绝；要求本地可靠文件系统。

| 文件 | 内容与来源 | 生命周期与异常规则 |
| --- | --- | --- |
| 数据目录`identity.json` | 格式版本1、manager_id、schema_version=4、digest_key_id、encryption_key_id；随机UUID和独立key ID | 明文非秘密但身份关键；先于数据库发布并同步。serve只读，缺失不补造 |
| keys_dir下`digest.key`、`encryption.key` | 各32字节独立随机密钥，文件不含额外换行 | O_EXCL创建、写满、Sync、Close，再同步目录；读长度必须恰好32，复制进入Store后清理运行层副本 |
| `database_file`及SQLite旁文件 | 内核CreateService产生v4结构、密钥验证、首管理员与initialization_complete | 原子提交边界由内核主定义维护；存在时init不覆盖，serve不迁移或创建空库 |
| 数据目录`.manager.lock` | 无业务状态的操作系统锁文件 | init与serve都持有排他文件锁；文件存在不代表运行中。进程退出内核释放锁；不会删除锁文件导致并发锁在不同inode上 |

锁覆盖该数据目录的完整操作寿命。仅凭PID或文件存在判断单实例不足；采用Linux flock／Windows LockFileEx，其他未支持平台拒绝构建或运行，不退回不可靠的“锁文件”。配置两个实例指向同一数据库但不同密钥目录仍争用同一锁。

## 3. 初始化与失败流转

2026-11-12独立Agent审阅后、Manager对应修复前补充RT02：初始化前置材料集合必须包含`database_file`、其`-wal`、`-shm`、`-journal`旁文件、identity及两份密钥。即使主库不存在，旁文件仍属于不完整材料的证据；在产生任一密钥或打开SQLite前拒绝已有普通文件、目录或链接，保留原字节和目录项。不能让SQLite自动清理旁文件后再判断是否可初始化。本模块不使用ATTACH或超级日志；增加数据库用法时须同步复核材料集合。

同轮RT03／RT08修复前补充：serve在取得部署锁后、调用任何Store打开方法之前，检查三个旁文件的完整无链接路径；不存在正常，存在则必须是本服务身份私有的普通文件。允许SQLite对有效WAL／journal作正常恢复，不因旁文件存在而一律拒绝；拒绝链接、目录、非私有材料时不触发SQLite清理。Linux实际权限／链接实验单列，不能用Windowsmode结果替代。

对应RT02补充：Windows保留材料路径比较采用不区分大小写的规范路径，数据库若与identity／两钥／旁文件发生别名冲突，必须在发布钥之前返回配置错误。比较只覆盖本平台规范路径及大小写，不声称可接受短文件名等任意别名；链接路径继续拒绝。

顺序为严格解析配置和口令 → 检查并创建私有目录 → 取得排他锁 → 确认数据库、身份及两份密钥均不存在 → 生成随机身份／密钥 → 逐个独占持久写入密钥和identity → CreateService原子建库与首管理员 → 关闭Store → 同步相关目录 → 释放锁。写入失败、磁盘故障或进程中止不删除已产生材料，也不自动重试覆盖。

数据库的初始化完成标志是唯一完成依据；identity不另存一份可不同步的“完成”布尔值。文件已写但数据库不存在／不完整时serve失败，重复init也失败，提示保留材料并检查。由于初始化尚不含任何节点或操作，明确需要重新开始时可由操作者选择全新空目录配置；程序不自动清理不确定目录。成功init不启动HTTP，不授予首管理员任何节点动作能力。

## 4. 启动、资源所有权与关闭

2026-11-12正式Agent接线前基线：新init使用Manager v5及CreateRuntimeService；identity的schema_version明确保存5，serve按4／5显式分派打开，旧v4不迁移且只保留管理HTTP路径。v5在普通HTTP适配前将精确connect路径交给[任务通道](m1-manager-agenttransport.md)，浏览器变更完成后仅发非阻塞唤醒提示，Store仍是唯一受理权威。关闭先停止并等待该通道的握手／已升级连接／worker，再HTTP关闭，最后Store与锁；不能依赖HTTP Shutdown处理已劫持socket。

2026-11-12静态产物接线前选择：采用配置web_dir指向独立构建的静态目录，默认`/usr/share/docker-manager/web`，已同步运行配置主定义；使Go构建与Node构建各自可复验，交付包同时包含两者。serve必须能读取该目录的index.html，绝不下载资源或返回伪造登录结果。只向GET／HEAD公开根index和真实静态文件，禁止目录列表、符号链接、路径逃逸；未知API送HTTP适配器的固定错误，其他未知路径404。index不缓存，带内容hash资产可缓存；同源默认CSP限制资源来源，表单及连接只到自身。发布说明须记录Go和Web的同一交付版本，不能将旧dist与新API任意拼装。

serve先完成配置、私有文件、身份及TLS检查，取得同一排他锁，再OpenService核验数据库并取得当前boot；打开会冻结旧节点。绑定监听失败时关闭Store并释放锁，不继续后台运行。TLS最低1.2，无HTTP明文业务端口。HTTP限制与[服务规格§4](m1-manager-service.md#4-服务生命周期与初始化)一致；普通API30秒context，ReadHeaderTimeout 5秒、IdleTimeout 60秒、MaxHeaderBytes 32KiB，请求体8MiB。WebSocket后续由专用生命周期管理。

程序接受SIGINT／SIGTERM后停止新请求，给在途HTTP最多15秒关闭时间；到期关闭连接并等待处理器退场，再关闭Store和锁。关闭HTTP context不撤销已提交Apply，实际取消必须经过业务路由。返回已持久状态依赖幂等读取；不能因响应没有发完补写“未受理”。后续任务通道接入时，其worker必须在Store关闭前退出，运行模块的关闭顺序随接线先修订再实现。

进程持有TLS材料和Store；普通日志只包含生命周期阶段、非秘密地址及固定错误分类。就绪仅表示HTTPS监听和Store可用，不代表节点或Docker就绪。没有通道时已认领节点仍离线／核对中，Apply明确拒绝。

## 5. 验证与维护

2026-11-12独立审阅、修复前补充RT01／RT05／RT06：external_origin的非默认端口必须为无前导零的十进制；`0443`和`08443`分别会被浏览器规范化成默认443和8443，若原字符串直接保存会导致全部Origin比较失败，启动须拒绝。静态入口不消费业务正文；带非空Content-Length或分块正文的请求必须立即拒绝并关闭连接，不能在返回后无时限排空正文。此边界与API早拒绝共同覆盖所有对外请求。

同轮审阅追加、修复前记录RT01／RT07：IP origin必须采用浏览器会发送的规范形式；IPv6使用最左最长零段压缩、小写十六进制、无前导零，IPv4映射尾部也用十六进制，非规范展开形式拒绝；IPv4仅接受规范四段十进制，不接受浏览器会另行归一化的简写／旧式数字地址。初始化等候首遍／确认口令时也要观察取消；取消先恢复终端模式再返回固定canceled诊断，不能因为接管信号而永久卡在读口令。命令取消后立即退出，不复用同一stdin执行后续工作；某些平台底层控制台读不能被普通context中断，读协程在进程退出时终止，已经读得但未交付的口令缓冲必须清理。

规范IP和数字末段判定参考[WHATWG URL主机解析与序列化](https://url.spec.whatwg.org/#host-parsing)；2026-11-12读取官方现行文本，并以Node URL.origin核对审阅中的IPv6与端口案例。Go的netip.String对映射地址的点分尾部不能直接作为浏览器origin，运行模块按8个16位段生成用于比较的规范形式，不修改用户配置后默默采用另一个origin。

验收见[运行实施记录](../implementation/m1-manager-runtime.md)。定位顺序：配置解析 → 目录／文件权限与链接 → 排他锁 → identity及两份密钥 → Store的schema和完整标志 → TLS证书／origin → 监听及关闭。不要通过删除身份、换钥、清库解决打开失败。修改文件布局需同时更新备份组；修改锁须验证真实第二进程；修改关闭须验证已提交请求在重启后可查询。
