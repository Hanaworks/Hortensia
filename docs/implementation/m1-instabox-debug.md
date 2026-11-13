# M1 InstaBox 调试与 Linux 验证

2026-11-13，会话客户端日期；本篇初始基线在上传、创建调试机及 Linux 执行之前保存。用户要求使用 instabox.sh 调试，并明确授权申请、在本机保存一天有效的机器读取与操作凭据，新建本项目专用调试机、上传项目源码及运行测试。官网授权已批准；环境结果分列在第5节，未执行项仍保留。

## 1. 目的与依据

本轮将现有实现放到隔离 Linux 环境验证，补充 Windows 本机不能证明的进程组、文件权限、链接和 Docker 行为证据。InstaBox 是测试载体，其模板和产品做法不构成本项目的新需求。范围承接 [M1 推进记录](m1-first-version.md)、[测试计划](../test-plan.md)、[Compose 执行规格](../specifications/m1-compose-engine.md)与[完整备份组规格](../specifications/m1-backup-group.md)。

现有源码交接为 `4d46a60`，schema5／protocol1；工作树另有尚未提交的通用备份组源码。测试前记录实际输入文件的 SHA-256，并说明相对提交的变化，不能把工作树结果表述为已提交版本结果。备份组尚未接通 service／CLI／真实双端材料适配，恢复、重加密及受控维护尚未实现；本轮不能用虚拟机快照代替这些能力。

官方资料只用于连接与环境操作：[快速开始](https://instabox.sh/docs/quickstart)、[账号授权](https://instabox.sh/docs/security/agent-sign-up)、[机器权限](https://instabox.sh/docs/security/api-keys)、[模板与限制](https://instabox.sh/docs/account/limits)。网页列出的预装版本不是实测版本，必须在机器中重新记录。

## 2. 环境、输入与过程边界

先使用一台新建的本项目专用 Linux VM。读取账号配额后选择可用模板，不购买或升级套餐；若账号只允许一台，先做单机验证，双节点验收保持未验证。机器命名、ID、模板版本、创建及停机时间写入本机缓存证据。2026-11-13用户随后明确允许使用原有两台暂停机器；本轮先用已新建的两台，其他机器作为按需资源，不把已有机器无业务的陈述外推成任意其他账号机器可用。

创建时关闭 Vault 自动导入并显式不选择组织 Secrets。只上传本仓库编译及测试所需的源码、SQL、契约、Web 源码和依赖锁文件；排除 `.git`、`.cache`、`node_modules`、实际数据库、身份文件、私钥与本机凭据。测试账号、证书及秘密均在隔离目录中新建，只含测试数据。源文件清单和传输包摘要保存在本机，解包后逐文件核对，避免只证明传输命令退出成功。

Go 至少满足 go.mod 的 1.27.0；编译工具、依赖和容器镜像从其正式来源取得并记录实际版本。保留原业务期限和断言，不能为了让虚拟机通过而放宽期限、跳过失败或关闭保护机制。InstaBox exec 有单次时限与输出截断边界，长测试写远端日志并作为后台工作执行，最终取回完整文件，不能只据截断终端判定。

初轮测试使用 VM 内部回环地址，无需公开 Manager 或 Docker 端口。后续浏览器或跨机连接先明确 TLS、来源和通道；不能用平台默认公开端口或更换身份验证语义掩盖服务错误。Docker socket 不发布到网络。

## 3. 稳定验收编号

| 编号 | 执行与观察 | 当前状态 |
| --- | --- | --- |
| IB01 环境事实 | 记录 VM ID／模板、内核／发行版／架构／PID1、用户／权限、CPU／内存／磁盘、Go／Node／Docker／Compose 版本；Docker 端点与 Engine ID 分别记录；失败不伪填版本 | 已记录第5节环境及A完整清单；不作为支持矩阵通过 |
| IB02 输入对应 | 固定本轮源码文件清单／摘要，包上传后核对实际解包的每份输入；隔离材料与日志不进入源码归档 | A已逐文件核对255份输入，第6节 |
| IB03 Linux 回归 | 在该输入上运行 `go test -p 1 ./... -count=1 -json`，完整保留退出码、包／测试／跳过／失败及 stderr；另行记录 vet、build、Web 测试及构建 | Go／vet／build／mod verify通过；Web39项／类型／构建亦通过，第8节 |
| IB04 平台边界 | 核对 executor 的真实进程组 TERM／10 秒 KILL／中断 exit0，workspace no-follow／symlink、两端私有材料链接、backup 与 snapshotcheck 的 Linux 文件属性测试实际执行；辅助 CLI 仍是夹具，不称真实 Docker | 指定Linux断言已实际通过，第6节；不含真实设备故障／掉电 |
| IB05 原生 Docker 对照 | 固定实际 Engine／Compose 对及测试镜像，对照相同文件／环境／顺序／目录／项目参数的 config 与 up 行为、资源归属及无 build 边界；逐项对应 CE／T04，不以 Probe 成功代替支持矩阵 | 原生子集已执行，第7节；产品Engine组合未验，生产精确接受表仍为空 |
| IB06 正式服务 | 新建测试身份，实际 Manager／Agent 命令完成启动、TLS、认领、连接和恢复；记录 boot／session／持久材料及关闭。空能力接入与成功部署分开报告 | 指定回环CLI／HTTP／WS子集通过，第8节；正向Docker部署未执行 |
| IB07 两节点隔离 | 两个独立 VM／Engine 上同名对象，执行与权限只影响已指定目标；保留双方资源前后证据。一个 VM 内的进程夹具不关闭此项 | 未执行；依赖账号容量及双节点连接 |
| IB08 首版判断 | 关联 M1 V01—V08，分别记录真实流程、故障注入及独立阅读。未实现的恢复／维护保持未完成，Linux 回归不自动构成首版交付 | 未执行 |

IB03 中若已有测试覆盖 IB04，不为凑证据重复运行；从完整日志确认对应断言实际执行即可。出现具体故障再定向复现、定位、先修订受影响规格和断言，再修改实现并回归。新增 Docker 接受条目前必须保存精确组合的实际兼容证据和支持边界，没有配置或环境变量绕过。

## 4. 结果、关闭与交接

本篇保存时所有 IB 项均未执行。后续记录真实命令、源码清单摘要、工具版本、日志位置、观察结果、已修复问题和仍未验证的边界。单独列出平台权限、进程终止与真实存储故障，不把进程重启或 VM 暂停称作掉电耐久。

本轮机器的进程和临时服务在退出前关闭；机器按实际权限暂停或停止并核对状态，保留后续调试材料和 ID，不未经授权删除。账号密钥／claim token 不写项目文档、普通日志或会话；公开报告不包含测试秘密正文。

## 5. 实际环境记录

2026-11-13 18:41—18:43（Asia/Shanghai），官网授权已生效。账号 API 实测 `maxMachines=5`，已有两台 suspended，创建本轮两台后共4台；不能以通用免费套餐网页覆盖账号真实配额。所用CLI固定为 `0.8.268+c4f5b3e3a`，未在测试中自动升级。

| 用途 | 名称／ID | 创建后的事实 |
| --- | --- | --- |
| A：Linux回归与正式服务 | dm-m1-a-20261113／m_agq5jab6pmtal66qwxntxjkvyq | running／runtime.ready；4vCPU、8GiB、20GiB data；vault=false；free=true |
| B：原生Compose及后续节点隔离 | dm-m1-b-20261113／m_agq5jadtubvgelytmdyzbzv2q4 | 同规格、独立VM；running／runtime.ready；vault=false；free=true |

两机模板固定 `agent-browser@2026-11-01.3-android.1-cli.1`，digest为 `sha256:808062679e901d9cd95cf734e7fd5def01e00e18bbd6d9e78d00e56413ffb710`；创建显式 `--no-vault --secrets none`，没有导入账号秘密。实际系统均为 Debian13.7、Linux6.18.60-instabox、amd64、systemd；执行UID1000，具备docker组权限。实测Go1.27.1、Node22.23.3、npm10.9.9、Docker29.8.2、Composev5.6.0。Web package.json要求Node `^24.15.0 || >=26.0.0`，模板现有Node不满足，Web执行前须取得匹配工具而不能忽略engines告警。

创建响应、状态和版本原文保存于本机`.cache/instabox-debug/`；本节环境读取与第6节软件结果分开，不把已创建机器记成联调完成。

## 6. 第一轮 Linux 源码回归

2026-11-13 18:47:31—18:50:47（Asia/Shanghai），A机实际执行。输入为4d46a60加尚未提交的通用组及其取消分类修复，共255份Go／SQL／契约／Web锁定源码；这不是原4d46a60的原样复验。上传包SHA-256为 `58a25ab5886bb77eecad523d45dadb70901960e01a97a7bd112774134cee0ea6`，清单SHA-256为 `662ccc93185d5541fa0ec409e76ad7edc0bbd73eda569e5bd9d94d4a8574483b`，远端逐文件长度和摘要均相等后才启动测试。

验后通用组源码保存为`7362441`，提交前再次核对上述255份输入与工作树无差异；原始清单保留形成时HEAD，不把验后提交号伪写成执行时版本。[历史输入与证据归档](../../archive/2026-11-13-instabox-debug/README.md)保留相同字节，后续维护以现行源码为准。

| 验证 | 真实结果 |
| --- | --- |
| `go test -p 1 ./... -count=1 -json` | 退出0；18个测试包通过，另2包无测试；1991条测试／子测试通过，0失败、2跳过 |
| 两项跳过 | Agent和Manager的Windows大小写路径别名专用测试，在Linux按平台分流；不是Linux链接或进程断言被跳过 |
| `go vet ./...`／`go build ./...`／`go mod verify` | 均退出0，modules verified |
| 运行组合 | Go1.27.1、linux/amd64、CGO_ENABLED=1；与此前Windows Go1.27.0／CGO0证据分别报告 |
| 输入／证据 | 远端输入核对退出0；完整JSON、stderr及各步exit已取回本机，证据包SHA-256为 `75ce55818f6d19636e1318b1669ae3e159b65bc647094ce5a474e583887619b2` |

完整日志确认以下断言确实执行且通过：`TestInterruptedExitZeroRemainsInterrupted`、`TestDeadlineKillsIgnoringProcessGroup`、`TestSymlinkRejected`、`TestInitAndOpenIdentityRejectSidecarLinks`的六个旁文件组合、`TestPrivateFilesRejectLinks`、`TestManagerLockUsesOperatingSystemProcessLifetime`、`TestSnapshotRejectsLinks`的WAL硬链接／源symlink／目录symlink，以及`TestSchedulerPersistsBeforeReadyAndCorrelatesEvents`。因此补上指定Linux进程组、真实链接拒绝、操作系统锁和受监督辅助CLI路径证据；最后一项仍是测试CLI，不是Docker执行。

本机证据位置为`.cache/instabox-debug/linux-first/`和`linux-first-summary.json`，来源清单在`.cache/instabox-debug-v2/source-manifest.json`。正式测试源码随仓库保存，命令可在相同输入／工具环境重新执行。目录Sync成功不证明掉电耐久，受控I/O故障不证明真实磁盘故障；通用组用受控回调通过也不关闭真实两端材料适配、完整备份命令或恢复验收。

## 7. B机原生 Docker／Compose 子集

2026-11-13 18:50—18:54（Asia/Shanghai），在B机独立执行NC01—NC12原生对照。两机Engine ID不同：A为`b6acafc8-57c0-4e3e-b092-496e8f703d80`，B为`db7656a5-e664-4e16-9b49-de8ff401cfce`；这是环境事实，不是产品节点选择正确性的证明。B固定Docker29.8.2、Compose短版本`5.6.0`和busybox不可变镜像digest，完整内容见[原生说明及证据](../../archive/2026-11-13-instabox-debug/native-compose/report.md)。

真实观察包括多文件顺序、env优先级、profile、包内只读bind、原生default网络和命名卷、资源labels／停止容器／health none、external网络／卷缺失、同名碰撞、替换／orphan、非零部分变更、取消等待，以及label_file／configs／secrets的原生输出。156条脱敏完整命令和实际资源inspect均已取回；六份脚本／日志／结果的本地SHA与远端一致，负责人再次执行证据校验退出0。

其中主轮NC05无image子夹具错误使用YAML布尔command，原非零来自schema；该子断言明确作废，历史结果原文保留且不计入no-build证据。NC05b先用合法输入通过config，再实际因缺构建镜像失败且无构建；external卷缺失也单独刺激，而不由网络失败外推。原生up失败／取消仍留下network、created或running容器，不能将退出状态当回滚或未执行证明。

本轮没有调用产品Prepare／VerifyBeforeStart／Observe、正式正向Apply或两节点授权隔离；label_file原生成功不开放产品能力，也未添加生产接受条目。自有容器／网络／卷最终清点均空，仅镜像缓存和证据保留；B于18:59暂停，状态实测suspended，后续可继续调试。旧两台机器仍未启用。

## 8. A机 Web 与正式 CLI／HTTP／WS

2026-11-13，使用官方Node24.15.0 Linux包且SHA-256校验通过，实际npm11.12.1；不使用模板的Node22忽略项目engines。`npm ci`、`npm test`、`npm run typecheck`、`npm run build`均退出0，3个测试文件共39项通过，生成生产dist；这是组件自动化与构建，不是浏览器点击流程。

实际构建并启动Manager／Agent命令，使用新私有测试目录、回环监听和显式CA信任。前两轮夹具失败保留：run-01的测试CA缺少签发KeyUsage，严格客户端拒绝；run-02将无node.read时隐藏资源的404错断言为403。修订仅在测试材料／预期中，未降低TLS校验或改变产品权限；第三轮前将剩余预期逐项核对现行HTTP／Agent／存储规格并保存契约勘误。

run-03于19:02:23—19:02:30（Asia/Shanghai）完成，`overall.exit=0`，负责人已读取完整assertions并复核以下结果：

| 行为 | 实际观察 |
| --- | --- |
| 页面／传输／会话 | 正式Manager提供index和2份构建资源；显式CA访问成功、默认不信任该CA时拒绝；实际登录获得Secure／HttpOnly会话，重读保留 |
| 显式授权 | 缺node.read时保存隐藏404/not_found；增加6项真实节点能力后同一会话继续有效，不把grant更新误作撤销登录会话 |
| 正式Agent接入 | CLI init／enroll／serve，长期凭据active／generation1；正式WS恢复round completed、total0／seen0／pages1，节点online且unfrozen |
| 实际运行探测 | Agent只读runtime_profile与HTTP端点一致，记录真实Docker29.8.2／Compose5.6.0；生产接受表为空，实际capabilities数量0 |
| 多文件与版本 | 5份文件、两版原文字节／UTF8／CRLF／Compose顺序保留；同键同输入重放不新增Stack／版本，旧版不可变 |
| 拒绝副作用 | 已online／unfrozen前提下Apply返回422 unsupported；前后操作计数均0，未产生待执行任务，不把离线或恢复中的拒绝混作版本门禁 |
| 正常重启 | 两端退出0后用原材料重新启动；原会话、node／ledger和两版内容保留，再次完成空恢复及422零操作断言 |
| 关闭与材料 | 所有自有CLI／监督进程0、专用监听关闭；两端操作系统锁可重新取得。没有停止Docker daemon或使用恢复CLI |

运行中空恢复的持久事实通过SQLite `mode=ro`／query_only读取；不调用会更换boot／恢复状态的业务Store.Open来伪造旁观检查。只取回白名单证据、脚本和摘要，不下载测试身份、密钥、口令、数据库、Cookie或token。原始失败／有效结果及输入对应由[历史归档](../../archive/2026-11-13-instabox-debug/README.md)保存，当前生产版本仍为schema5／protocol1。

A于19:05暂停并实测suspended，B已于18:59暂停；测试材料保留供继续联调。IB07双VM业务隔离、正向Docker部署、故障恢复／受控维护和真实浏览器独立阅读仍未验证，IB08不标完成。已允许使用的原两台暂停机本轮未启用。
