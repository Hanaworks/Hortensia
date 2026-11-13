# M1 v5完整备份组实施与验收

2026-11-13会话客户端日期，编码前保存；同日追加通用层实际结果见§5。依据[完整组规格](../specifications/m1-backup-group.md)和[MB主定义](../specifications/m1-maintenance-backup.md#73-v5完整备份的只读检查切片)，不实现恢复／维护／新格式，不扩业务需求。通用源码已实施，服务/CLI和真实领域组合由负责人独立集成，不在通用回调结果中冒报。

## 1. 开工基线与依赖

已交接运行源码95d453f／文档e291a44；本轮未提交的shared／Agent／Manager Inspector先经独立审阅与对应包验证，Root正在全仓回归。完整组不得将Inspector占位或固定JSON当领域通过。新实现前先保存本篇及规格，按BG编号写真实行为RED。

2026-11-13客户端日期、Group编码前补充：统一捕获上限为identity 64KiB、config/各manifest 256KiB、钥各32字节；身份组件纯解析的既有更小有效限制不因此放宽。MaterialSet传ConfigJSON，DomainReport同时保留TableRows/SealedRows与keyed SemanticFingerprint；Create/Verify在领域回调前后核对副本身份/hash及无旁文件。BuildInfo及ExternalMaterial闭集已在规格保存，外部材料required只表示未来恢复需处理的依赖，不检查外部对象存在，不因空库无工作区而拒绝备份，也不新增审批。影响BG01/05/07/08/11，本处仍无Group测试结果。

## 2. 顺序与完成条件

1. Generic backup的固定清单／组构造／verify、无环摘要、新建及partial／uncertain／cleanup边界；先类型与文件流程，受控回调测试仅算通用层，不假称真实解密。
2. 两端service纯材料解析和Inspector适配，运行锁／材料锁短期及命名空间校验；真实源保持运行，复制和检查不改业务控制事实。
3. 严格CLI backup／backup verify接线及旧命令兼容；verify不读活配置、不启动服务。
4. 真实两端Store／身份／多文件配置／历史事件形成组并离线验证；故障、并发材料变更、错组及秘密边界。
5. 独立复核、全仓关联验证及Linux编译；平台权限／目录同步等分别记未验证。更新README和运行说明后才能把本地适用子集记为实现，不把MB／整个M1标完成。
## 3. 稳定BG01—BG12验收条件

以下不是测试已通过；负责人保存到实施记录后再写/运行实现。已有BS01—BS07继续承接，不重跑无关检查代替新增结果。

| 编号 | 输入/动作与必须观察的结果 |
| --- | --- |
| BG01 真实组与秘密边界 | 用两端正式v5 API建立含多文件秘密版本、事件/输出、非空claims和已finalized历史的真实SQLite；Create+Verify返回相同身份/钥集合/精确密文计数及语义摘要；逐个备份原文字节可认证；普通目录/标准输出不含已知identity secret/key正文 |
| BG02 在线无启动 | 源Store/服务保持运行并真正提交新版本/事件/claim；调用backup无需运行锁，快照包内部一致；active_boot/session/recovery_mode/原uses不因backup增加，服务在同boot继续处理；源主/WAL非写入区间字节不变，SHM读锁不作不变断言 |
| BG03 材料锁等待 | 用真实第二进程占.materials.lock，backup尚不创建输出；释放后继续成功，等锁取消返回Context错误；持.agent/.manager.lock的运行服务不阻塞backup |
| BG04 所有写入口互斥 | 在init关键阶段、Enroll候选/远端成功未promote、Rotate rotation_pending/promote暂停时检查锁；backup只能等待或取得原/新完整状态，不能把旧identity+新钥混组；backup持锁时init/enroll/rotate/start的材料阶段明确materials_busy且不写identity/钥/DB；普通服务不持材料锁整个生命周期 |
| BG05 身份/钥失败零发布 | 交换两真实组的identity/某一钥、损坏32字节、缺钥、短钥、Manager schema4、未来schema、Agent非法secret状态；领域Inspector拒绝，所有最终data manifest均不存在，原材料不被补造/替换 |
| BG06 全量领域认证 | 采用领域方案的每个密文表/引用/语义故障样本，包含非首行/尾行损坏、未知密文列/DDL、claims/归属链坏、额度越界；Create和Verify都拒绝。不能只测quick_check或一份config解密 |
| BG07 组摘要无环与错配 | 错组secret manifest、相同ID但不同DB、identity摘要不等、文件大小/字节损坏、缺key、未知JSON/重复key、路径穿越/绝对path、重复role/table均拒绝；验证读取secret manifest精确原字节，而非重新marshal后比较 |
| BG08 新建/路径与权限 | 已有输出/两输出相同或互相嵌套/大小写别名/DB或钥配置成锁名/所有源和输出祖先链接reparse/材料与锁硬链接均拒绝，已有DB/备份原字节保持；Linux owner/mode与Windows reparse/ACL限制分列 |
| BG09 partial生命周期 | 注入Snapshot/Finish、复制/Close/Sync、领域验证、secret发布、data发布前失败与取消；无最终data清单、原有效组不变；第二根冲突保留第一根；旧partial不重用，新名字可重试 |
| BG10 最终发布边界 | 按实际平台或受控文件操作故障点区分data rename前失败、rename后目录Sync失败、完成后锁Close失败；分别断言partial/uncertain/complete+cleanup，不能把已出现完成标志的情况伪写成无发布；并发占位不被rename覆盖 |
| BG11 离线verify无副作用 | 源部署不可访问/运行锁已持有仍能仅用两清单verify；前后整个备份树文件/摘要相同且无WAL/SHM/journal生成；无网络/Engine/Create/Open/新boot/READY/current写入 |
| BG12 CLI与交接 | 两端实际命令严格flag与错误码/退出码；成功输出计数和清单路径、失败只受控分类；由未参与实现者依规格说明DB→材料→验密→manifest关联及异常定位；更新实际help/README/MB状态，声明restore/rekey/v6和真实Linux未完成 |

环境分层：Windows真实SQLite/文件/辅助进程、受控Sync/发布故障、真实Linux锁/owner/mode/no-replace/目录同步分别留证据。没有真实Linux实验就保持该项未验证；普通进程中止不等于掉电/满盘。完整备份组完成只能支持MB08对应v5子集，不能把MB09恢复或整个M1标完成。

## 4. 结果及交接

本基线保存时BG01—BG12均未验证。后续逐项记通过／失败／未验证／阻塞、源码版本与原始证据、真实与受控文件操作边界；不以工具退出0代替用户结果。五分钟期限、专门Close／Sync／磁盘故障与Linux实机证据不足时如实保留。源码切片完成不代表可发布恢复工具；当前生产命令尚无本篇接口。

2026-11-13编码中补充：通用层首轮固定失败API的正式RED记录于`.cache/m1-backup-group-generic-red.txt`，真实SQLite/秘密原字节绑定与离线verify的首轮GREEN记录于`.cache/m1-backup-group-generic-first-green.txt`。后续两个故障断言实际复现“同字节源材料替换仍发布”和“密文计数超过普通表计数仍接受”，修复前RED在`.cache/m1-backup-group-generic-revision-red.txt`；先补规格，再实施源文件身份比较及计数结构约束。此处只记录通用受控回调，不作为两端真实解密组合证据。

## 5. 2026-11-13通用层实施结果

基于负责人已交接Inspector checkpoint `4d46a60`，通用层为本轮工作区未提交的`internal/backup/group*.go`及本文/对应规格。未改原有Snapshot、localstate、snapshotcheck、领域Store、依赖、服务或CLI；未执行Git和全仓命令。环境：Go1.27.0 windows/amd64、固定modernc.org/sqlite v1.61.1，真实本机SQLite/文件/辅助进程锁；领域回调是明确受控检查，不认证两端真实密文。

| 验证批次 | 实际结果和证据 |
| --- | --- |
| 首轮正式RED→GREEN | 固定失败API令真实组与取消断言失败，随后Create/Verify等通过；见§4列出的原始文件 |
| 两项修订RED→GREEN | 同字节源材料换实体、密文计数超过应用行数两项实际失败再修复，GREEN 0.874s；`.cache/m1-backup-group-generic-revision-green.txt` |
| Verify扩展故障→GREEN | 首轮发现共享协议安全整数上限拒绝9007199254740993；仅通用元数据改精确整数解码，完整Verify矩阵10.579s通过；`.cache/m1-backup-group-generic-verify.txt`保留失败、`...-verify-green.txt`为修复后结果 |
| 独立只读复核→修复 | 发现Inspector返回前取消导致ctx错误链被替换；真实RED→GREEN，修复后0.626s；`.cache/m1-backup-group-generic-review-red.txt`和`...-review-green.txt`。复核者未运行测试，修订后未报告其他可具体触发问题 |
| backup整包含原BS | `go test ./internal/backup -count=1 -v`退出0，84.497s，165 test/subtest通过、3跳过、0失败（37个顶层通过）；`.cache/m1-backup-group-generic-tests.txt`。本次Junction及两项symlink创建权限拒绝而SKIP，不覆盖此前其他批次实际通过的证据 |
| 最新通用矩阵 | 取消修复及WAL/零覆盖断言补齐后`go test ./internal/backup -run 'Test(CreateGroup\|VerifyGroup\|Group)' -count=1 -v`退出0，15.237s，145 test/subtest通过、0失败/跳过（29顶层）；`.cache/m1-backup-group-generic-final-tests.txt`。Windows尾点/尾空格/8.3材料别名实际拒绝通过 |
| WAL断言加强 | 最新测试增加直接从组副本读取version=17的断言，定向测试退出0、0.787s；`.cache/m1-backup-group-generic-wal-final.txt`；不把受控领域报告当WAL结果 |
| 零覆盖追加断言 | complete/partial目录重用拒绝且原树不变，新路径重试成功；第二根被并发占位时保留首根/冲突目录，退出0、1.521s；`.cache/m1-backup-group-generic-zero-overwrite-final.txt` |
| 静态与Linux编译 | `go vet ./internal/backup`、`GOOS=linux GOARCH=amd64 go build ./internal/backup`均退出0；对应`.cache/...-vet.txt`及`...-linux-build.txt`。只编译，不作为Linux原生锁/权限/no-replace/目录耐久验证 |

完整包批次在取消修复前执行；该批次没有失败，修复后的最终通用矩阵重新覆盖全部通用行为及追加的WAL/零覆盖断言，BS源码未变；不虚构一次运行覆盖所有最终BS和通用测试。源码校验值及详细命令/边界由`.cache/m1-backup-group-generic-report.md`和`...-sha256.txt`承接。

| BG项 | 通用层当前状态 | 剩余依赖或限制 |
| --- | --- | --- |
| BG01 | 部分通过：真实SQLite副本、完整identity/key字节、新建组、无环清单与普通文件秘密边界 | 两端正式v5真实密文/历史组合及最终标准输出由Root集成，不能用受控回调代替 |
| BG02 | 部分通过：源保持WAL写入，新提交17进入副本、源boot/session哨兵不变；BS原事务一致性回归通过 | 正式服务同boot/session/quota及持续业务写入依赖服务组合 |
| BG03 | 通用层通过：真实第二进程占材料锁、等锁零输出、释放继续、期限取消 | 服务运行锁不阻塞backup的正式进程组合由Root承接 |
| BG04 | 部分通过：材料锁贯穿流程，源字节/文件实体变化拒绝 | init/enroll/rotate/start的全部写入口互斥由服务实现和测试承接 |
| BG05 | 部分通过：材料缺失/短钥及callback错误零完成发布 | 真实身份/错钥/schema/Agent secret状态语义由两端适配器及领域检查承接 |
| BG06 | 未验证真实领域故障：通用层仅验证报告结构和失败传播 | 每密文表尾行/AAD/业务引用等真实故障使用已交接Inspector及Root组合测试 |
| BG07 | 通用层通过：完整摘要/错组/严格字段/null/整数/路径/两计数/fingerprint矩阵 | 完整业务真实性仍依赖真实领域回调 |
| BG08 | 部分通过：新建/碰撞/互嵌、Windows文件及父目录别名、8.3、材料/锁硬链接拒绝 | 本批Junction/symlink权限不足；Linux owner/mode/原生发布待真实环境 |
| BG09 | 部分通过：真实取消/坏材料和受控Snapshot/写/manifest阶段失败保留partial，原生no-replace不覆盖 | 真实满盘/文件Close/磁盘Sync/掉电仍未验证；受控错误不当真实设备故障 |
| BG10 | 通用层通过：data发布前partial、最终目录Sync不确定、完成后cleanup分别断言，真实锁可再取得 | I/O失败为受控注入；平台真实耐久实验暂缺 |
| BG11 | 通用层通过：源材料移除且运行锁占用后只凭两份清单verify，备份树字节/文件不变，无旁文件；callback改DB拒绝 | 正式两端领域副本读取由各自证据及Root集成承接 |
| BG12 | 通用源码阅读复核完成 | CLI/help/README/正式用户输出与总体交接由Root统一；M1/MB08整项/MB09恢复不标完成 |

下一步：通用层停写并交负责人做真实两端service/CLI组合、关联回归与最终版本；集成发现明确缺陷时按事前规格/故障断言修订，不由现有通用结果推断产品发布或恢复完成。

## 6. 独立复核P2取消分类修订（修复前追加）

2026-11-13：负责人提供的`.cache/m1-backup-group-independent-review.md`及`probe-result-complete.txt`实际复现Create的Inspector返回非nil领域错误分支遗漏ctx.Err：取消和期限两反例丢失errors.Is分类，partial/未发布边界仍正确；Verify及最终data rename后取消控制例通过。本轮先保存受影响BG09/12的基线细化，再在正式测试加入两个只读回调反例观察RED，最小修复该分支并定向GREEN/vet。未扩需求、API、共享代码或领域Store；不重复无关BS整包，Linux原生最终回归由负责人承接。

随后正式`TestCreateGroupInspectorErrorPreservesCancellation`与`...PreservesDeadline`实际RED：分别0.21s/3.04s，状态partial且错误链缺少context分类；原始`.cache/m1-backup-group-generic-p2-red.txt`保留。`group.go`只在inspect错误分支增加ctx.Err优先检查，未改API或提交边界。

修复后定向组合（两新反例、成功Inspector后取消、7个发布失败状态、离线Verify控制）退出0，6.418s，12个test/subtest通过（5顶层）、0失败/跳过，见`.cache/m1-backup-group-generic-p2-green.txt`；`go vet ./internal/backup`退出0，见`...-p2-vet.txt`。此前§5全generic/整包批次属于修复前历史，本次不冒称重新全包通过；最终源码hash更新在`...-generic-sha256.txt`，交负责人冻结新包及Linux原生最终回归。

独立overlay原五probe另作定向复验，结果保留于`.cache/m1-backup-group-generic-p2-independent-check.txt`：3控制通过、2 Create probe失败、退出1，3.407s。两失败源于probe在检查context前还要求保留旧domainFailure及ErrVerification；本轮按既有Verify和修订基线优先返回ctx.Err，因此该额外旧行为guard不成立，实际输出已为context canceled/deadline exceeded，不能把此批报告为全绿，也不为满足旧guard改变已定规则。正式两反例明确验证context、ErrPartial、partial及无最终发布并通过；独立取消后data rename仍complete、Verify取消与uint64/Unicode控制继续通过。

## 7. 负责人关联回归与独立关闭（2026-11-13）

原独立审阅者按已保存的ctx.Err优先契约修订了仅属probe的旧domain guard，未改生产源码／正式测试：两Create取消／期限反例以及Verify、发布后取消、uint64／Unicode三个控制实际退出0，5项通过、0失败／跳过，3.334秒。旧RED及旧guard批次EXIT1继续保留；最终group.go摘要与作者冻结版本相等。独立报告为`.cache/m1-backup-group-independent-review.md`，此P2关闭。

负责人随后将完整受验输入冻结并在InstaBox A执行Linux全仓回归：18个测试包通过，1991条测试／子测试通过、0失败、2项Windows平台专用跳过；vet／build／mod verify全部退出0。精确255份输入、工具组合、校验值与完整日志见[IB记录第6节](m1-instabox-debug.md#6-第一轮-linux-源码回归)。该批包括本次Create错误分支及全部最终group／BS测试，不能与前述Windows历史批次混称一次执行。

BG03、BG08—BG11的既有通用层断言已在Linux实际运行，覆盖第二进程材料锁、硬链接／symlink拒绝、原生不覆盖发布、零复用、partial／uncertain／cleanup和离线树不变。跨UID权限否定、真实满盘／Close／Sync／掉电仍无专门刺激；I/O注入仍为受控故障。BG01／02／04／05／06／12的正式service／CLI／真实领域组合依赖不变，本轮不将MB08整体、恢复或M1标完成。
