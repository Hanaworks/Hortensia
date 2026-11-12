# Agent首次接入与当前限制

2026-11-12。本文对应正在验证的M1 v5程序；`cmd/agent`和正式Manager任务通道已存在，整套M1尚未交付。实际部署位置、测试方式和两台Linux节点由用户另行安排，本页不要求现在操作现有业务主机。

**当前能完成接入与恢复核对，尚未开放生产Compose执行。** Docker／Compose精确接受表为空；即使探测到版本，Agent也不会上报部署能力。原生对照和故障验证后才能增加实际支持组合。真实Docker部署、未知任务受控维护及完整备份恢复仍待继续验收。

## 1. 准备程序和配置

先按[Manager首次运行](manager-first-run.md)准备可信HTTPS入口。在Manager页面登记节点，记录其UUID；另给操作账号明确授权。每节点使用独立Agent身份与账本，不能复制另一节点目录接入。

源码根目录运行`go build -o bin/agent ./cmd/agent`；Windows本地构建用`bin/agent.exe`。当前Go版本为1.27.0。Linux交叉构建不代表Linux安装验收。Agent不提供公开监听端口，由节点主动连接Manager。

Linux配置示例（具体路径须按部署环境调整）：

```json
{
  "manager_origin": "https://manager.example:8443",
  "ca_file": "/etc/docker-manager-agent/manager-ca.crt",
  "state_dir": "/var/lib/docker-manager-agent",
  "identity_file": "/etc/docker-manager-agent/identity.json",
  "keys_dir": "/etc/docker-manager-agent/keys",
  "docker_cli": "/usr/bin/docker",
  "docker_socket": "unix:///var/run/docker.sock",
  "execution_timeout_seconds": 1800,
  "log_level": "info"
}
```

origin必须可达且证书名称匹配。公开CA受系统信任时可省略`ca_file`；私有CA提供真实PEM文件，没有跳过校验选项。JSON及所有文件路径严格校验，不展开环境变量或相对路径。Linux状态／密钥叶目录要求0700，身份／密钥和数据库由服务账号私有持有；Windows权限与Linux验收分别记录。

## 2. 初始化、认领与启动

1. 在目标节点运行`agent init --config <配置绝对路径> --node-id <Manager登记的UUID>`。程序创建两份独立密钥、唯一ledger和候选身份；此时不连接Manager，不部署Docker。
2. 在Manager页面生成该节点的一次性认领token。通过受控标准输入运行`agent enroll --config <配置绝对路径> --token-stdin`。token不作为命令行参数，不写入脚本或普通日志；只需一行，不必关闭标准输入。命令本身不隐藏终端回显，可由服务账号私有临时文件或已有秘密输入工具提供标准输入。
3. 认领成功后运行`agent serve --config <配置绝对路径>`。连接先核对node／ledger和当前凭据，再扫描账本并完成握手。Manager保存完成回执后才结束核对；连接、核对完成、具备执行能力是不同状态。

认领响应丢失时，对同一目录重试原流程；程序优先核验已耐久保存的候选，不创建第二个ledger或第二套凭据。网络错误不等于token失效。不要重新init、换node-id或清账本解决认领失败。

`agent version`或`agent --version`只显示开发版本，不读取身份／密钥。当前各子命令的`--help`共用参数列表；`--node-id`供init指定节点，`--token-stdin`供enroll读取token，其他命令不应传这两个参数。本文不列尚未实现的维护命令。

## 3. 停止、重连和轮换

正常停止使用SIGINT／SIGTERM，留出监督器保存退出结局的时间。网络断开使尚未开始的许可失效；已启动任务由独立监督器继续记录，重连不重跑。重启不附着历史PID，也不把PID不存在解释成未执行。

轮换前先停止持有本地锁的Agent服务，运行`agent rotate-credentials --config <配置绝对路径>`，完成后重启serve。候选先耐久保存，丢响应可对同一材料重试。明确401会结束serve的自动重连，须核对当前凭据；不手工删除候选或降低代次。

所有命令争用同一`.agent.lock`；文件存在不代表程序存活，操作系统锁才是依据。不要删除锁文件启动第二实例。

## 4. 保留材料与定位故障

| 观察 | 检查入口及处理边界 |
| --- | --- |
| 初始化拒绝已有材料 | identity、两钥、agent.db及-wal／-shm／-journal；半集合也保留，不让SQLite或重复init清理 |
| 身份／钥／格式不匹配 | 核对同一组node、ledger、两key ID和schema；不补空库、换钥或静默迁移 |
| TLS或认领失败 | 核对origin、主机名、证书／CA、Manager节点身份和token；不关闭证书验证 |
| 连接后不能部署 | 先看恢复是否完成，再看能力；当前空接受表明确unsupported，不是以后自动执行 |
| started后无结果 | 保留进程事实和账本；已耐久退出会恢复原结局，不能证明退出则继续核对，不续旧命令 |
| 已ACK仍占用 | ACK只证明收到；检查最新result、归属和finalize／finalized，不删除claims |
| 观察失败 | 不改原退出码、时间或输出；允许的正常退出只补观察，真正未知继续隔离 |

数据流、对象和事务见[Agent服务](specifications/m1-agent-service.md)、[运行账本](specifications/m1-agent-runtime.md)、[Compose执行](specifications/m1-compose-engine.md)和[Manager任务通道](specifications/m1-manager-agenttransport.md)。维护及备份恢复目前是[实施前设计](specifications/m1-maintenance-backup.md)，不能按其中规划名称直接运行命令；管理备份不包含容器卷或应用数据。
