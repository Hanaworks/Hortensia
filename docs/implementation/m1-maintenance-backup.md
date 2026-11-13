# M1 受控维护、备份和隔离恢复实施与验收

日期：2026-11-12。状态：**实施前基线；全部软件验收未验证**。具体行为及对象由[维护／备份规格](../specifications/m1-maintenance-backup.md)定义；本篇记录实施顺序、稳定MB条件、环境与证据，不重新复制共享协议和数据定义。对应首版G、V06、V07及O11／O12，不以文档完成替代软件交付。

用户已选择受控维护方向，接受必要时停止节点Docker及容器影响；测试方式／环境仍稍后提供，本轮不连接或改变任何现有节点。本篇的Linux支持profile为空，真实停止和解锁必须保持禁用。Windows可完成真实SQLite、加密、文件组及受控进程编排验证，不能据此填Linux屏障通过。

## 1. 版本和开工条件

设计依据：2026-11-12工作区两端v5源码（部分未提交）、`manager init/serve`及正在接线的Agent服务；不能把HEAD源码号用于证明全部v5已提交。开始实现时记录完整基线版本、工作区差异和构建hash；软件证据随最终代码重新关联。

先完成以下接口审阅，再写依赖代码：

- [ ] 两端schema6的显式新格式和v5完整离线转换，人工release_basis、维护基线来源和持久门禁写入规格主定义。
- [ ] 解决现v1无法表达unknown维护释放的事实；2026-11-12设计审阅已收束规格§6的protocol2/schema6选择，先同步主定义和Schema。v5只用1、v6只用2，不隐式迁移。Finalize判据不得扩大到unknown。
- [ ] 停止／启动配置profile、cgroup覆盖及后台Engine路径形成可验证输入；没有真实版本profile时fence拒绝，不把手填复选框当证据。
- [ ] 所有现行密文表的binding枚举与实际DDL逐项核对，恢复不能假设有keyring；新KEK逐记录重加密与独立额度reservation先写故障负例。
- [ ] 部署current格式2与两端服务锁统一；备份／身份轮换／publish的materials锁顺序统一，避免一端绕过。

首版明确保留的限制：Linux单本地rootful Engine、systemd/cgroup v2/runc及内置本地存储／网络；不支持live-restore、Swarm、插件、远程卷／FS、未知后台控制者及未验证精确版本。管理备份不包含应用数据，不提供RPO/RTO承诺、不自动删历史或备份。

## 2. 实施顺序及独立交付物

### MB工作包A：完整格式、认证和不可绕过门禁（先于屏障）

目标：有真实持久状态机，未知执行不会因一端成功而开放。文件范围为两端`ledger/operations`的v6 DDL、`maintenance.go`、正常读写／释放／恢复入口和`internal/maintenance`文件链。

- [ ] 编写失败测试：unknown输入原文不变、跨case/ledger/generation重放拒绝、缺回执不释放／不开放、正常finalization与人工链不能互相替代。
- [ ] 增加Plan／FileEnvelope／ManualResolution类型和case阶段CAS；同一步返回原始回执，不重复执行副作用。
- [ ] 实现双端Prepared→Committed→Accepted→activate链及endpoint gate；错误阶段提交失败后claims、原结果及冻结状态符合MB01—MB04。
- [ ] 实现人工基线来源并和正常candidate+finalized共同验证修订头，禁止回退／分叉；不更新最近成功版本。
- [ ] 实现protocol2全量历史投影与旧端拒绝；当前all/completed仍为开放的最后屏障，补齐MB07。

本包完成仅表示受控存储与恢复记录切片通过；没有Linux端点证明时CLI不能实机解锁。

### MB工作包B：一致快照、重加密和部署发布

目标：真实SQLite可形成和恢复完整组，任何部分组不能启动服务。新增`internal/backup`，两端包内sealed-row枚举／验证及恢复转换方法，扩展localstate和服务部署manifest读取。

- [ ] 编写失败测试：活动WAL中新提交、备份期间identity轮换、缺钥／错组／坏cipher、忽略新密文表、在线备份意外改boot、旧KEK计数回退等负例。
- [ ] 用锁定modernc Backup接口从独立只读连接取得副本；Step/Finish、busy、取消及partial发布按规格执行；副本作全量语义验证。
- [ ] 保存双目录清单及所有实际身份／密钥材料；不将secret混入普通输出或日志。
- [ ] 实现v5→v6及旧备份stage：源只读、原HMAC/K_ledger和所有事实保留、新KEK逐记录重加密及先预留额度；中断弃用本次新钥，重新stage用更新代次／新钥。
- [ ] 实现格式2完整generation和current原子发布；验证expected-current与运行锁；发布后必为隔离恢复状态。
- [ ] 用真实Manager Store和Agent Store验证恢复：旧session撤销、普通账号／授权待复核、所有旧节点凭据撤销及离线候选重新绑定、all发现备份缺失历史。

本包验收不要求真实Docker但必须用真实两端Store和真实SQLite，不能以固定JSON恢复报告替代。

### MB工作包C：Linux可证终止屏障和操作者CLI

目标：从只读inspect到明确批准计划，再到可实证屏障；只有profile通过才产生可解锁证据。新增Linux barrier适配器；Windows编排测试注入unit/cgroup/Engine视图替身，不执行真实停止。

- [ ] 先写零副作用拒绝负例：live-restore、Swarm、插件、远程卷、未知shim／控制入口、错误boot／profile、未批准摘要。
- [ ] 实现inspect：完整unit、activation、daemon/containerd/shim/runtime和控制组映射，输出具体影响与拒绝原因。
- [ ] 实现fence阶段持久化、原材料备份、停止／120秒期限／残留检查、受控重启和完整资源观察。任何失败无充分屏障，无自动释放。
- [ ] 实现显式逐资源decision、受控人工基线接受及各阶段CLI导入／导出。执行原文字节、标准输出和help的状态必须真实。
- [ ] 在用户提供的专用Linux环境记录精确profile、后台Engine操作源码／实验依据、调用及资源变化，才能从空支持矩阵新增一行。

### MB工作包D：首版真实演练及交接

- [ ] 主节点用真实unknown故障注入，经端点屏障、双端文件链和all恢复后才接受下一次冲突部署；底层旧up不能继续／重复。
- [ ] 第二节点同名项目和账号权限不受维护目标串扰；未知控制入口发现时保持拒绝。
- [ ] 实际一致备份及旧备份隔离恢复，发现备份后新增操作、已ACK终结历史、人工维护历史及撤权变化；旧secret拒绝、真实Agent全量核对。
- [ ] 更新README、运行手册、首次运行说明、协议／数据主定义和M1状态；独立读者完成正常备份和失败恢复，不依赖作者口头补充。

本轮设计子任务只拥有本篇与规格篇；其他文件的同步由实施负责人完成，不能在此基线假称已同步。

## 3. 稳定MB验收条件

编号不重用；发现新故障先追加修订基线和受影响项，再改实现。每项记录通过／失败／未验证／阻塞，并链接最终版本证据，不能只记录测试命令成功。

| 编号 | 用户结果与故障边界／观察方法 | 环境／当前结果 |
| --- | --- | --- |
| MB01 入口与范围 | inspect遇错node/ledger/endpoint/input/profile、过期计划、未批准摘要或不支持runtime时，停止调用计数为0；原claims／事件／身份不变，错误无秘密 | Windows受控；未验证 |
| MB02 原执行事实保留 | unknown/interrupted及START_COMMITTED无result各一例，维护结束后原outcome／exit／seq／result原字节／许可不变；独立resolution可查，不显示成功、不改最近成功版本、不调用up、不制造not_started | 真实双端Store；未验证 |
| MB03 双端释放原子性 | 在Agent Prepared、Manager Committed、Agent Accepted事务各提交前后崩溃／满盘；claims只和对应持久回执原子变化，无回执文件可重导出；所有不完整阶段下一冲突请求拒绝 | 真实SQLite／两个进程；未验证 |
| MB04 门禁及重放 | 重放每步返回相同字节；跨case／旧generation／旧输入／同step不同内容拒绝；secret丢失保持冻结，只能从已认证本机副本恢复同一secret，不能无MAC解锁；单端成功、activate文件本身、Pong或session替换均不开放，当前all/completed后才开放 | 真实Store及通道；未验证 |
| MB05 归属修订 | 原pre→人工接受post有真实ID逐项说明和resolution来源；普通snapshot不能发基线；指针+revision回退、修改status、换finalized为resolution、分叉和extra未知资源拒绝；正常发布仍能继续下一修订 | 真实Store／受控观察；未验证 |
| MB06 真正端点屏障 | 专用Linux中注入Compose死但dockerd请求仍在处理、daemon停但shim残留、后台pull/create/start/delete及restart policy。屏障前不释放；所有计划控制组空／旧控制入口停止、旧请求不再继续后才出证据；live-restore/插件/远程卷/残留任务拒绝。记录实际unit、cgroup、进程starttime、Engine及资源，不用PID消失替代 | 用户环境尚未提供；未验证，不允许实机解锁 |
| MB07 维护全量历史 | protocol2 all包含原unknown原文及独立maintenance引用；无完整本地链、旧Manager／Agent、较新矛盾seq均隔离；v5只用1／v6只用2，交叉配对与隐式迁移拒绝；Manager恢复到维护前备份后，从真实Agent导入完整链再all可收敛；pending缺席不冒充all | 双端真实Store／正式通道；未验证，主定义待同步 |
| MB08 在线SQLite一致快照 | 在活动WAL和并发写入时备份，副本含一致版本包／事件／claims，全部密文可读；源active_boot/session不改变且任务不重复；逐表计数与源副本语义验证覆盖全部当前加密表，新增表遗漏必须失败 | Windows真实SQLite；未验证 |
| MB09 备份组完整性 | 缺数据／secret清单、错backup_id、错identity、错钥、坏cipher、删FK／约束、缺表、目录链接、busy／满盘／取消／Finish失败均拒绝；上份有效组不变、partial不能恢复；普通日志和stdout无秘密 | Windows真实文件／SQLite；未验证 |
| MB10 单KEK回退安全 | 旧备份uses落后于真实后续使用；恢复用全新KEK及ID、原HMAC/K_ledger不换，所有表原文明文／fingerprint／input_digest／seq相等；AAD换key_id仍可读；仅重包装的错误实现失败；新uses包含所有预留，未退款 | 双端真实Store；未验证 |
| MB11 rekey失败恢复 | 任一记录解密／封装／事务／Sync失败或进程强杀，源组hash不变、stage未READY不可publish；重试生成不同新KEK，旧尝试保留abandoned；超1,048,576拒绝且未删历史 | Windows真实SQLite／进程；未验证 |
| MB12 完整组发布 | 在key写完、DB写完、READY、current替换前后终止进程，启动只认旧完整组或新完整组，混组／过旧expected／服务持锁明确拒绝；旧组回退必须再次恢复、新KEK，不直接serve旧计数 | Windows实际文件＋Linux待验证；未验证 |
| MB13 授权与凭据 | 恢复旧Manager后旧浏览器会话、旧node token和enrollment均拒绝；备份后撤权不由旧grants复活；普通账号待复核，当前管理员逐项授权；新候选掉响应耐久重试、node/ledger不变、generation不降 | 真实Manager／Agent Store；未验证 |
| MB14 旧备份与真实all | 含秘密的多文件版本原字节可读；备份后新增／已ACK任务、备份缺失许可、未知和人工历史均从真实Agent all被发现并保存；旧许可不重发、up次数不增；未知仍held或经过完整维护链 | 真实Store先测，Linux演练待环境；未验证 |
| MB15 Agent账本回退 | 恢复旧Agent保留当前可读原材料；Manager已授权而Agent缺记录保持未知。双端都回退也不能以空all释放；必须屏障和维护；禁止改身份／空库重认领／删claims。正常finalized历史与回执不丢 | 双端Store及Linux屏障；未验证 |
| MB16 操作与文档交接 | 帮助及手册只列真实命令、精确支持版本、停止影响和失败继续入口；独立维护者仅凭材料完成一次备份验证及中断case恢复，并能解释原unknown和人工释放区别。修正文档后复验卡点 | 最终产物／独立读者；未验证 |

## 4. 证据格式和运行环境

每次证据目录记录：代码版本及dirty manifest、构建hash、OS/内核/systemd/cgroup、Go／SQLite／Docker／containerd／runc／Compose版本、身份和profile摘要、测试输入摘要、注入点、前后两端表和解密后的安全语义摘要、底层调用计数、资源实际ID、预期／实际／结论、限制。秘密只用假材料；不把token／完整inspect Env／密钥写进普通验收日志。

本机可执行的未来验证命令入口为`go test ./internal/maintenance ./internal/backup ./internal/agent/ledger ./internal/manager/operations ./internal/localstate -count=1`及全仓`go test ./... -count=1`，但前两个包尚不存在，当前不得列作已运行。链接及契约检查用现有`node tools/check-docs.mjs`、`pwsh -File tools/check-contracts.ps1`；它们只验证静态文档／契约，不使MB软件条目通过。

真实验证前用户提供测试安排后，维护负责人先填实际节点、unit／socket／cgroup、容器停止范围、服务恢复顺序、专用项目、假秘密、备份目录及故障方式。没有环境时继续独立开发，不催要或猜测现有节点地址。进程重启不是掉电实验，假Docker不是Linux终止屏障，库能重开不是隔离恢复成功。

## 5. 完成条件、限制与下一依赖

源码切片完成：对应MB本机项有最终版本证据，真实副作用入口仍按空profile拒绝；明确哪些包／入口实际存在。模块完成：MB01—MB16全部适用项通过，包括Linux屏障和旧备份演练，主定义／运行资料已同步。首版G完成还需实际服务产物和安装／运行资料，V01—V08全部完成才谈M1阶段完成；内部试用不等于公开发布。

本篇保存时：**MB01—MB16均未验证；无新软件完成结论**。下一依赖是协议恢复投影和schema6／部署manifest审阅，再实施真实存储／备份切片；Linux profile与最终演练等用户后续提供的环境安排。O12方向已定，技术屏障及证据尚未交付，O11的长期保留／容量／RPO/RTO仍未解决。

## 6. v5完整备份后续切片（2026-11-13实施前基线）

会话客户端日期记录；依据已交接源码95d453f／文档e291a44。本轮先做无需schema6、维护屏障或用户节点的v5备份检查，随后接组／材料锁／CLI，不修改业务范围。具体入口、报告、framing、静态副本和错误预算见[主规格§7.3](../specifications/m1-maintenance-backup.md#73-v5完整备份的只读检查切片)。这修订前文“先等schema6才可开始所有备份”的实施依赖，不表示恢复或维护也可跳过其版本工作。

1. Agent／Manager各自在已有模块规格追加精确表、绑定、历史状态和证据边界，在实施记录保存AI／MI稳定条件；保存后才加正式RED和实现。
2. 先证明检查不改变主库／文件集合／boot／session／quota，错身份／钥／DDL／历史密文／claims及引用拒绝，合法未完成和旧历史接受；每个密文表覆盖非首行／历史行。
3. 验证摘要确定性、普通或明文内容变化必变、仅同schema正确更换KEK不改变可比根；新KEK实作若尚未实现，用明确测试构造验证，不假称恢复工具已完成。
4. 路径、旁文件拒绝、私有文件、大小和取消／超时分别验证；Windows不能执行的权限项仍未验证，Linux构建不当作运行。
5. 检查器独立复核后再接完整组、无环清单、材料锁、命令及BG01—BG12；到那时先补组的精确实施基线，不提前填通过。

完整组精确实施基线现已保存于[BG记录](m1-backup-group.md)，实际接口、材料锁及发布／verify流程见[组规格](../specifications/m1-backup-group.md)。它承接本节v5范围，不包含恢复或维护功能；BG结果须另行验证。

当前只有既有BS SQLite复制底层已完成本机适用项。两端Inspector、完整组、恢复及维护仍未实现；AI／MI结果随后在所属记录维护，本篇不重复另一份共享字段定义。

### 6.1 共享副本辅助的事前条件 SC01—SC06

| 编号 | 必须观察的结果 | 当前 |
| --- | --- | --- |
| SC01 | 完成副本ro/immutable读取真实SQLite；UPDATE被拒绝，控制值、主文件及文件集合前后不变 | 未验证 |
| SC02 | 活WAL／SHM／journal、链接／多硬链接、非私有或非本地路径拒绝；不清理或补造文件 | 未验证 |
| SC03 | 已完成Snapshot包含活动WAL提交，即使头为WAL格式也可只读；无新旁文件／boot／session | 未验证 |
| SC04 | 检查期间主文件字节／身份改变或旁文件出现，Finish拒绝；取消可观察且所有资源释放，重复Close安全 | 未验证 |
| SC05 | 精确framing可与独立HMAC计算对照；类型、顺序、component、身份及内容差异影响摘要，边界拼接不碰撞 | 未验证 |
| SC06 | 非法master／类型／溢出不产生有效结果；关闭后不能继续摘要；目标包测试／vet／Linux编译分别记录 | 未验证 |

先保存以上基线，再用API stub观察行为RED；依赖尚未实现导致的编译错误不当作行为反例。SC通过仍不等于任何AI／MI／BG或完整MB条目通过。

### 6.2 共享辅助本机实现记录（2026-11-13）

SC01—SC06已有共享源码，仍为未提交工作树。8个首批正式顶层用例先在可编译stub上观察行为RED，再实现Reader与Fingerprint并GREEN（0.746秒）；随后增加真实SQLite长度预算及原始查询错误隐藏的验证用例，未将此事后补验声称为事前RED。最终目标包9个顶层／5个子用例通过（14条PASS、0失败／跳过，0.641秒），JSON在`.cache/m1-snapshotcheck-tests.jsonl`。vet通过；Linux amd64测试二进制交叉编译通过，未在Linux执行。

本机实际断言为只读拒绝UPDATE、主库和目录项不变、三个旁文件／多硬链接拒绝、真实活动WAL经Snapshot形成完整单文件后可检查、主文件变化或旁文件新增时Finish拒绝、取消保留且资源收束、独立HMAC对照及类型／边界／身份隔离、非法类型／溢出和关闭后复用拒绝，以及SQLite真实32MiB后备预算／原始错误不外泄。链接／Linux私有权限／真实存储关闭故障和五分钟失去响应的内核不由这些用例证明。

两端AI／MI、完整组及MB条目仍分别实施，不能用本节替代。

SC02／SC04独立审阅重新打开：Windows尾点别名可让Open／Finish成功却忽略真实WAL提交、返回旧boot。原测试只覆盖精确同名旁文件，未覆盖文件系统别名。先保存本修订与正式反例，再修规范路径认证；原SC通过记录不能覆盖此缺口。根因证据与后续复验分开保存在`.cache/m1-snapshotcheck-review.md`。

同日修复及复验：尾点／尾空格正式反例均复现旧值，真实8.3短文件名反例也先RED；改用Windows句柄规范长路径与输入规范路径比较（仅忽略大小写），别名在immutable打开前拒绝。最终共享包11个顶层／9个子例共20条通过、0失败／跳过（0.754秒）；独立原尾点／真实短名反例分别通过0.932／0.701秒，正式包独立通过0.790秒。旧Snapshot非immutable复制另行实测正确读取真实WAL，无该缺陷，未扩展修改其行为。保留首次RED、修订及最终SHA证据，SC02／SC04本机此边界关闭，Linux实际运行与关闭故障等限制不变。
