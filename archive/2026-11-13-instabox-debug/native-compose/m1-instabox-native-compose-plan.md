# B机原生Compose对照事前清单

形成于2026-11-13，先于脚本与远端实验。依据README、docs/specifications/m1-compose-engine.md §3—4、docs/specifications/execution-recovery.md §2—3、docs/implementation/m1-compose-engine.md CE05/08/09/15、docs/test-plan.md T04/T17及§7，承接.cache/m1-instabox-validation-plan.md阶段C。

本子任务只验证原生Docker/Compose语义与适配器应消费的真实输出，不调用正式Manager/Agent，不授予生产能力；生产接受表继续为空。B机ID m_agq5jadtubvgelytmdyzbzv2q4，已知Debian13/kernel6.18.60/Docker29.8.2/Composev5.6.0/UID1000，以实际记录为准。仅上传本任务测试脚本，根目录/data/docker-manager-native，项目/资源前缀dmnc_b_20261113。不得操作A、既有VM、业务资源、系统daemon或公网端口。

| 编号 | 动作与预期观察 | 范围与完成判据 |
| --- | --- | --- |
| NC01 | 记录系统/Engine/CLI版本、端点和UID；拉取官方busybox小镜像后固定digest/ID | 保存实际命令/退出状态/digest，无版本推定 |
| NC02 | 原字节多文件及子目录override，正反文件顺序；有序env-file、默认.env/显式empty.env，显式映射优先；profile开关 | config --format json语义断言；输入前后SHA相同；引用基准为显式project-directory；不依赖排版 |
| NC03 | 同一受控目录/环境/项目/文件/profile做config及up --detach --no-build，readonly包内文件、env_file、命名卷 | 实际容器Env/只读挂载/运行状态与模型一致；网络和卷只含Compose原生项，无额外注入 |
| NC04 | 实际list/inspect容器、网络、卷；停止容器、健康无字段和受profile控制服务；记录labels和created | CLI输出满足64hex容器/网络ID及name/driver/CreatedAt卷弱指纹；不从label推断本地StackID/null的产品结论 |
| NC05 | image+build指向带标记的本地Dockerfile后仅up --no-build | 实际使用固定image ID；无构建产物/构建标记；无image的独立案例非零，记录资源副作用 |
| NC06 | 外部网络/卷缺失、显式创建后可用 | config不证明依赖存在；up缺依赖非零且不造external；已有external无managed项目label，结束保留直到显式清理 |
| NC07 | 显式container_name撞上本任务创建的无Compose标签容器 | up非零、原容器身份/状态不变，记录可能已创建网络等部分副作用；产品ready前拒绝仍需产品集成 |
| NC08 | 同一服务配置变化触发替换；随后去除服务制造orphan | 相同逻辑服务换真实ID；原生不带remove-orphans时保留孤儿；绝不自动接管 |
| NC09 | 依赖顺序确保好服务先启动，坏服务非法可执行文件导致非零 | 必须观察非零下的实际已启动/created资源；非零不意味着回滚 |
| NC10 | 条件等待中的原生up进程组TERM取消 | 记录信号/退出/仍在的真实资源；只证明该取消点，不声称Agent监督/任意Docker请求中断等价 |
| NC11 | 只清理此脚本创建的容器/网络/卷，再按项目label及名字清点 | 保留完整操作日志及零残留证据；保留日志和拉取镜像便于复核，不删除VM |

实施顺序NC01→NC02→NC03/04→NC05—10→NC11。脚本每项独立记录通过/失败/未执行；单项失败继续其他安全独立项，最后finally清理。30秒只读预算、每个变更最长60秒（取消实验主动TERM/KILL且最多35秒）。使用假秘密且输出统一遮盖，不输出凭据。所有测试脚本及证据仅.cache/m1-instabox-native-compose/**；摘要.cache/m1-instabox-native-compose-report.md。

完整日志包含argv、受控环境键、脱敏stdout/stderr、退出状态、时点与断言。config/inspect仅在该隔离夹具上采集；输出里的假secret统一替换，报告不印正文。结束交接NC逐项结果、发现/精确复现、环境/镜像身份、未覆盖产品项，不把Probe或原生脚本通过计为生产版本矩阵全部通过。

## 首轮证据核对后的定向补充基线（先于补充脚本）

主脚本11项显示PASS，但NC05无image子夹具写了YAML `command: [true]`，schema先拒绝，因此该子项不能算no-build缺image证据。保留首轮原文，定向NC05b改成合法字符串命令，先config成功，再up --no-build预期缺本地镜像非零、无构建产物，复核实际错误原因。NC06首个external缺失错误发生在网络检查，不能声称卷缺失路径已单独通过；NC06b先仅创建外部网络后up，预期外部卷缺失非零且不创建卷，再创建卷后正常up。

新增NC12是原生label_file能力及包内configs/secrets文件行为核对：用合法alias引入env_file，label_file提供标签且inline标签按原生优先级覆盖，configs/secrets绑定假文件；检查规范模型和实际只读挂载/内容。该项只记录该版本原生支持，不打开产品AllowLabelFile，不对静态安全检查、脱敏、归属或账本作集成结论。补充脚本使用新命名空间dmnc_b_20261113b，仍finally清理并保存逐名零残留证据。已通过且输入无变的原NC02—04/07—10不重复执行。
