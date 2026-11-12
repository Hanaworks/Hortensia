# M1 Compose执行适配实施记录

2026-11-12事前基线，先于本模块实现保存。[规格](../specifications/m1-compose-engine.md)。本切片承接AR06—11/14，范围是可调用IO与真实进程监督，不包含归属发布事务或M1完整交付。

## 顺序与环境

先保存本规格和CE验收，再锁定YAML/schema/dotenv依赖并核验API/许可证/Go兼容；先写拒绝与字节测试，再实现workspace；再静态检查及原生命令；再监督器与list/inspect观察；最后按最终代码复核规格并执行本地测试、Linux交叉构建。当前Windows/Go1.27；Linux/Docker环境尚未提供，不连接节点。测试CLI用实际辅助可执行程序，记录argv/次数，不在生产options注入成功结果。

| 编号 | 输入/动作与观察 | 通过条件 | 结果 |
| --- | --- | --- | --- |
| CE01 | 原始多文件/嵌套/已有版本复用/后改字节/额外条目 | 原字节和顺序相同；漂移拒绝且不覆盖；manifest包外 | 本地通过，含包外empty.env及内存上下文漂移 |
| CE02 | 穿越/重复/前缀冲突/链接/半发布 | 根外无读写；ready前拒绝；Linux句柄另验 | 穿越/前缀本地通过；Windows链接测试因权限跳过，Linux句柄/半发布故障未验证 |
| CE03 | alias/merge隐藏env_file、循环、重复键、深度/节点预算 | 首次CLI计数0、固定错误及位置 | 本地通过，真实辅助CLI调用计数0 |
| CE04 | 未知读入口、控制env含export/多行、覆盖掉/未启用服务 | 全部原文件在CLI前拒绝；合法alias原字节不改 | 本地通过；dotenv不读宿主、不打印缺变量告警 |
| CE05 | 辅助CLI记录config及up argv/env、模型bind/image/build | 两者上下文相同，无shell/up删除构建选项；不安全模型拒绝 | 本地参数/环境/模型断言通过；真实Compose归一化未验证 |
| CE06 | UTF8跨块/非法字节/stdout+stderr超限 | 字符完整且≤1MiB、持续排空、无输出日志 | 本地通过，实际辅助进程双流各4MiB排空且返回预算错误 |
| CE07 | Linux真实子进程组TERM/10秒KILL/exit0/deadline | Wait回收全部；中断exit0仍interrupted | 未验证；待Linux |
| CE08 | list/inspect失败/消失/超预算/健康none/orphan | 无假空complete、全项目含停止容器；无删除 | 本地list/inspect、关联错误、预算、停止/none通过；真实Docker未验证 |
| CE09 | 首次碰撞/更新替换/external/非零部分变更 | 具体身份及逻辑证据；不接管外部/不修改active | 本地替换/碰撞/external夹具通过；非零实际部分变更与finalize跨模块验证 |
| CE10 | 包测试、Linux交叉构建、最终规格复核 | 构建真实生产路径；区分受控夹具和真实Compose | 三包本地测试/vet与Linux测试二进制交叉构建通过；真实Linux运行未验证 |

完成要求是适用本地CE项有结果/证据、故障断言覆盖字节与真实调用，未验证Linux项明确保留；不能据本表宣称模块真实部署或M1完成。交付证据汇总到`.cache/m1-compose-engine-report.md`，不含真实秘密。

## 2026-11-12实现与验证补充

未提交源码基于af43699叠加本次独占目录；Go1.27.0/windows/amd64。三包普通测试、go vet及linux/amd64三包`go test -c`成功；完整用例输出见`.cache/m1-compose-engine-tests.txt`，测试二进制`.cache/m1-{workspace,compose,executor}-linux.test`。文档检查55篇/713链接通过，只证明链接/表格/围栏。Windows符号链接因当前账号权限无法创建，已明确SKIP，不能写成安全验证通过。

Linux已写真实进程测试：TERM后exit0仍interrupted，忽略TERM的父子进程组deadline+10秒KILL且后代心跳停止；只完成交叉构建，没有运行。首次包发布与目录Sync使用Linux句柄实现；掉电、故障注入、真实Compose多文件/alias规范化及Docker竞态依旧待实机。服务层精确版本清单初始为空，不因Probe成功自动开放能力。没有连接节点、清理Docker资源、直接SQL、释放claims或Git提交。

## 2026-11-12复核修复事前基线

CW01/CW02使CE01/CE04前述本地通过范围不足，新增下列稳定条件；先保存本段，正式回归RED后再修实现。精确版本门禁为本次明确要求，不能从旧Probe推断已存在。

| 编号 | 输入/动作与观察 | 通过条件 | 当前结果 |
| --- | --- | --- | --- |
| CE11 | OperationDir改任意目录或另一有效操作目录；OperationID/指纹/Dir/WorkspaceID/manifest/Payload/清单分别改变 | Verify/Invocation拒绝；CLI计数0；核对目录与命令实际路径一致 | 正式RED已复现；修复后全部字段及有效其他操作目录回归通过 |
| CE12 | dotenv控制键置于所有默认/替代/必填嵌套分支，外层已存在/缺失/空值；单引号/双引号/转义/注释/跨行/相邻表达式 | 任意潜在控制键在首次CLI前拒绝且不泄漏值；纯字面量不误判；成熟dotenv继续负责语法和真实值 | 正式RED实际CLI各5次；修复后前检计数0，全运算符及literal/escape语料通过 |
| CE13 | 空组合/fixture组合/高版本/前缀空白变体/任意组合 | 生产IsVerifiedRuntime初始全false；测试私有表只精确匹配、不进入生产；Probe不授予能力 | API缺失RED保存；生产空表及私有fixture精确匹配回归通过 |

验证先定向CE11/12失败用例保存RED，再分别修复并GREEN，加入CE13编译/行为测试后跑三包、vet与Linux构建；原overlay可由独立审阅者原样复验。结果汇总`.cache/m1-compose-engine-fix-report.md`。本次不扩大真实环境验收或修改账本/service。

### 修复结果补充（晚于上述基线）

正式RED见`.cache/m1-compose-engine-fix-red.log`，对应Package单字段及另一有效操作组合变更、原两个嵌套dotenv探针；新增API的RED为`.cache/m1-compose-engine-version-red.log`。修复后定向GREEN分别见context-green、env-green、lexical-version-green日志。最终三包本地回归见`.cache/m1-compose-engine-fix-tests.log`：workspace通过（链接仍因权限SKIP），compose通过，executor通过。

实现位置：workspace的私有executionContext持有完整执行关联和清单JSON，Verify在文件IO前逐字段拒绝漂移；compose/env.go只识别dotenv边界/插值键及影响美元含义的双引号escape，所有条件操作数均扫描，再用固定成熟dotenv做语法解析。词法检查不选择条件分支；非空合成lookup结果从不进入进程环境。双引号八进制可产生有效美元符号，已纳入语料并用原依赖实际lookup作oracle；单引号字面量、合法$$/双引号反斜线美元及注释不误判。runtime_versions.go的生产表为空，私有fixture只存在测试中。原overlay定向独立复验由根安排，本记录不冒称已经复验。

最后检查：三包go vet成功；compose/workspace的linux/amd64测试二进制交叉构建成功（`.cache/m1-{compose,workspace}-fix-linux.test`）；文档结构检查59篇/731链接/191表通过。Linux二进制尚未执行。修复源码和证据已交接，后续仅因独立复验新发现继续变更。

## CW04修复事前基线

2026-11-12独立原反例在空Pre/Expected中实际采到容器及网络；私有Conflicts非空，公开item仍写请求Stack。先保存本段，升级正式RED，再修`collect`默认null及由可信Pre/登记认证层精确回填。具体可信传参由根与service/ledger协调；本负责人不改账本。

| 编号 | 输入/动作与观察 | 通过条件 | 当前结果 |
| --- | --- | --- | --- |
| CE14 | 空Pre同project未知资源；Expected新候选；同逻辑新ID替换；不同Stack/project/endpoint/登记/关联；合法已登记同ID容器 | unknown/conflict/new/replacement的公开stack_id均null；仅可信登记+完整Pre身份完全匹配的既有资源填写Stack；私有Conflicts/Replacement不丢失 | 正式RED已保存；精确既有正例及unknown/new/replacement/revision0/external/project/service/created负例GREEN |
| CE15 | Observe前同socket更换Engine、Docker/Compose版本变化、策略/profile/Evidence不匹配及Probe失败 | 返回明确error证据；不list/inspect新端点、不把空集合当原操作complete；合法端点只读观察继续 | 正式RED为complete空集合且无Probe；修复后实际Probe与明确error/listinspect0，证据不匹配则所有CLI0 |

正反例均使用真实辅助CLI采集边界；定向及三包回归后追加`.cache/m1-compose-engine-fix-report.md`，原review overlay由独立审阅者定向复验。本轮不把此修复等同Linux/Docker实测。

根已选择不增加Prepared.Binding；service在CommitPrepared后及重开时统一ReadPreparationEvidence提供认证身份上下文。Engine只核对字段一致性并按非零revision+完整Pre身份保守回填；ledger允许null且验证错误非null由根负责，不在本轮目录越权修改。Observe实际Probe补入CE15，是本轮明确新增的原端点不变量修复；先保存规格/本表，再写正式RED。空Evidence旧probe需补齐认证字段才进入未知资源采集场景，不能因身份错误的安全拒绝误判CW04已验证。

### CW04/CE15实施后核对

`collect`不再访问请求Stack生成关联。Observe先Verify固定工作区，再核对认证Evidence与原冻结输入/profile的所有跨字段关联，接着用相同受控HOME/DOCKER_CONFIG环境实际Probe。任何身份错误均生成可持久Observation.error，未查询资源。采集完成后，只有非零归属revision且managed容器完整等于可信Pre、又无该资源冲突，才回填public stack_id；只满足Expected的新资源或替换ID保持null。私有替换/冲突证据继续保留，同ID但关键关联变化明确记冲突。

正式RED：`.cache/m1-compose-engine-cw04-red.log`、`.cache/m1-compose-engine-cw04-ce15-red.log`；定向GREEN：`.cache/m1-compose-engine-cw04-ce15-green.log`。三包回归日志`.cache/m1-compose-engine-cw04-ce15-tests.log`全部成功，workspace链接测试仍权限SKIP。首次较长子测试名引起Windows工作目录长度限制，早期临时日志不作CE15结论；已缩短测试名并加实际info调用/精确错误码断言，最终RED明确证明未Probe却complete空集合，GREEN证明实际Probe且不list/inspect。真实Linux/Docker限制未改变。认证Store与service接线由各自负责人验证，本模块没有修改ledger或service。
