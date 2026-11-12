# Manager与Web首次运行

2026-11-12。Manager／Web支持初始化、登录、账号与节点授权、节点登记、多文件配置保存和查询。`af43699`是已验证管理切片的历史源码基线；后续v5实现已接通正式Agent与持久任务通道，正在收束固定版本回归，实际边界见[M1推进记录](implementation/m1-first-version.md)。[Agent接入说明](agent-first-run.md)列真实命令。Docker接受表仍为空，不能实际部署Compose；完整备份恢复和未知任务维护继续实施。Linux布局尚待真实验证，不把交叉构建当作安装验收。

## 1. 准备程序、页面和证书

当前构建组合为Go1.27.0、Node24.15.0、npm11.12.1。先在`web`目录运行`npm ci`和`npm run build`，得到`web/dist`；在仓库根运行`go build -o bin/manager ./cmd/manager`，Windows构建可将输出名写为`bin/manager.exe`。这两部分共同构成当前管理切片，Manager不会在线下载页面。

准备由浏览器和Agent信任的TLS证书，证书名称必须覆盖实际访问的主机名。Manager不生成替代证书，没有忽略证书检查的运行选项。本机验收使用了专门测试证书与明确CA信任，没有修改系统信任。

## 2. 配置

配置文件使用绝对路径。以下是Linux布局示例，路径需要与实际部署一致：

```json
{
  "listen": "127.0.0.1:8443",
  "external_origin": "https://manager.example:8443",
  "tls_cert_file": "/etc/docker-manager/server.crt",
  "tls_key_file": "/etc/docker-manager/server.key",
  "database_file": "/var/lib/docker-manager/manager.db",
  "keys_dir": "/etc/docker-manager/keys",
  "web_dir": "/opt/docker-manager/web",
  "log_level": "info"
}
```

将构建后的`dist`内容放到web_dir，必须能读取其中的index.html。默认监听本机回环；需要从别处访问时，由部署者明确修改listen。external_origin填写浏览器真正访问的HTTPS origin，不带结尾斜线、路径、默认443或端口前导零；IPv6采用浏览器显示的压缩形式。部署者提供的TLS私钥仅服务账号可读，Linux模式0600。全部字段含义见[运行配置](operations.md#10-m0m1-运行配置工作定义)。

## 3. 初始化与启动

在Manager主机运行`manager init --config <配置绝对路径> --admin <账号>`，按提示隐藏输入并确认口令。账号为3—64个小写ASCII字母／数字及点、下划线、连字符；口令12—128个Unicode码点，前后空白保留。需要受控管道输入时显式增加`--password-stdin`，口令不作为命令行参数。

当前源码初始化创建schema5数据库、identity.json和两份独立密钥，并建立首个管理员。旧schema4管理库只按原入口打开，不迁移成v5。重复init不会覆盖，孤立WAL／SHM／journal及其他半集合也会拒绝；中止材料不会被清理重建。出现initialization_materials_exist时先保留并检查；确需重新开始的未联网实验可使用另一组空目录配置。已有运行数据不能用此方式绕过核对。

运行`manager serve --config <配置绝对路径>`，进程保持运行时在该origin打开页面确认入口。关闭使用SIGINT／SIGTERM，Manager先停止新请求并等待在途请求，再关闭数据库和释放进程锁。本机测试已验证实际可执行程序提供构建页面、证书校验后的登录、会话重读及节点列表。

## 4. 首次使用

1. 用初始化账号登录，进入“账号与节点管理”，创建需要使用的账号并登记节点。
2. 明确授予目标账号该节点的动作。管理员本身不自动拥有配置或部署能力。编辑配置至少需要node.read、config.read和config.write；部署另需stack.deploy；查询结果和输出分别需要operation.read及operation.output.read。
3. 选择节点，新建Stack并编辑包内文件，填写Compose文件和环境文件顺序，保存版本。保存和部署分别执行；保存成功后重开可读取同一原始内容。
4. 保存过程中仍可继续编辑；保存返回后，后续编辑保留为未保存草稿。遇到冲突先比较最新版本；遇到提交结果未知先核对原提交，页面不会用新key静默覆盖。

按[Agent接入步骤](agent-first-run.md#2-初始化认领与启动)认领并启动后，节点完成正式扫描才结束核对。当前未有实际Docker接受组合，Agent能力为空，部署明确unsupported；离线／未恢复完成时则按实际前提拒绝。拒绝不积压成未来自动执行的任务。真实部署步骤须随原生验证补齐，不能用受控测试结果替代。

## 5. 定位失败和交接

configuration_invalid先查严格JSON、绝对路径和origin；private_files_invalid查私有目录、文件和链接；already_running查是否已有同数据库实例，不删锁文件；identity_mismatch／key_mismatch／database_format_invalid应核对原身份、密钥及格式，不补造空库。tls_invalid检查名称、有效期和证书／私钥配对。固定错误不包含口令、密钥或原始配置。

浏览器草稿、口令、认领token和输出仅在内存，刷新会丢弃未保存草稿；待核对提交只保留非秘密身份。数据库、身份和密钥必须配套保留。完整一致备份和隔离恢复工具尚未交付，当前不能以复制在线数据库主文件代替备份。详细维护入口见[运行规格](specifications/m1-manager-runtime.md)、[服务规格](specifications/m1-manager-service.md)及各实施验收页。
