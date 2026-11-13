# M1 v5完整备份组实施与验收

2026-11-13会话客户端日期，编码前保存。当前未实现；依据[完整组规格](../specifications/m1-backup-group.md)和[MB主定义](../specifications/m1-maintenance-backup.md#73-v5完整备份的只读检查切片)，不实现恢复／维护／新格式，不扩业务需求。

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
