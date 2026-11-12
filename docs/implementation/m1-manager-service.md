# M1 Manager账号、节点与配置服务实施记录

日期2026-11-12；起点`656e355`。初始为编码前计划；2026-11-12服务内核已实现并完成下列本地验证，HTTP／运行验收由对应模块继续。本轮依赖Agent v4／Manager v3双端释放最终基线`6f92eb3`；拟新增Manager服务v4。首版总范围仍按[推进计划](m1-first-version.md)记录，首版终点已确认，两Linux节点环境尚待用户后续提供，本模块不请求节点。

## 1. 本轮目标与范围

依据[模块规格草案](../specifications/m1-manager-service.md)完成C＋D管理路径：正式初始化和TLS HTTP服务、本地账号／会话／权限、节点登记及认领／轮换、配置版本和幂等回执，以及连接同一operations权威的Apply受理／查询／取消入口。现有操作、授权、claims、事件和释放不另建副本；生产handler不调用Fixture。

正式网络任务接线、Agent工作区和Compose、Web、备份隔离恢复及O12工具不是本模块交付内容。缺少正式通道时Apply必须明确拒绝，不能设置假online或用受控执行结果制造端到端通过。HTTP返回结构采用[现行主定义](../specifications/http-api.md)，共同Config和协议结构采用[Schema](../../contracts/agent-v1.schema.json)。

## 2. 编码前审阅与依赖

已读现行HTTP、数据、Manager及运行设计，核对operations.Store、版本分派、users／nodes／grants、AcceptFixture／accept、requirePermission、AuthorizeStart、RequestCancelFixture、seal及BindRecoverySessionFixture。

已经明确的实施取舍：显式服务v4沿用同一Store和SQLite；新用户auth_rev从0起，授权后许可修订仍大于0；把acceptTx／cancelTx抽成共享事务核心而不先调用Fixture再补业务记录；请求canonical HMAC主定义写在服务规格，HTTP主文档随后补权威链接。以上是实施协调选择，仍须以最终代码和验收证明，不是用户逐项确认。

| 审阅点 | 需在编码前落实的结果 | 当前状态 |
| --- | --- | --- |
| 双端释放依赖 | 固定Manager v3／Agent v4实现及验收版本，确认v4服务保留两种释放证明、恢复屏障及原文核对 | 依赖已交付6f92eb3，v4继承同一DDL基线 |
| HTTP主定义衔接 | 链接canonical算法，确认Grant变化保持登录、最终事务查最新授权的行为，避免实现与共享会话说明各说一套 | 主文档canonical链接已同步；Grant边界已明确 |
| 密码依赖 | 锁定Argon2实现版本／许可证，记录64MiB成本和并发上限的初始实验方法 | 选定golang.org/x/crypto v0.59.0官方IDKey，BSD-3-Clause；64MiB/3轮/并行1/32字节输出/16字节盐；已下载，创建+验证约189ms本机观测；两并发拒绝测试通过 |
| v4结构／事务 | 复核新用户修订0、pending节点、Stack组合外键、操作服务关联、初始化完成标志及单连接非嵌套规则 | 004_service.sql已实现；共享Tx已抽取，具体断言见§7 |
| 运行与失败 | 决定初始化失败的可判别记录／人工处置，确认配置解析、文件权限、TLS和关闭预算的实测范围 | 待review，无可执行安装命令 |

以上review不增加逐项等待用户审批的流程。现有决定足够的技术选择由实施者落实；影响产品行为且仍无法判断的取舍，由主代理按已有约定提供具体问题，其他独立工作继续。

## 3. 实施顺序与文件职责

1. [ ] 完成审阅及必要主定义链接，保存规格和H01—H18验收基线，固定释放依赖版本。先写能够观察真实失败的版本、认证、事务和HTTP契约测试。
2. [ ] 在operations中新增v4完整DDL、CreateService／OpenService、正式认证／管理服务方法及内部Tx核心；旧DDL和入口保留。复核所有v4写入经过active_boot，受理和许可同时保留冻结及当前凭据屏障。
3. [ ] 实现`internal/manager/service`与`cmd/manager`的配置、文件、初始化、锁、TLS和关闭；实现`internal/manager/httpapi`的严格协议适配及错误白名单。handler不取得sql.DB。
4. [ ] 实现账号／会话／Grant、节点登记／认领／轮换／撤销；用正式入口形成测试身份，验证不同账号／节点和凭据响应丢失路径。
5. [ ] 实现版本密文、条件保存、canonical请求及安全回执，接入共享acceptTx／cancelTx及操作查询；所有业务＋回执＋审计保持一个最终事务。
6. [ ] 覆盖每条HTTP路由、跨权限拒绝、数据泄漏、并发同key／If-Match、存储语句／Commit失败、重开和真实TLS进程测试。对抽出的旧业务核心执行关联回归。
7. [ ] 独立审阅规格／代码及运行说明，逐项回填H01—H18结果、代码版本、证据及限制；同步README、组件、数据及首版路线状态。继续网络／执行器／Web依赖，不把此模块单独当首版交付。

建议按职责使用service_store、service_auth、service_nodes、service_configs、service_mutations、service_queries等文件，但文件名不是验收条件。共享Store／DDL和现有records／control／events改动指定单一负责人；HTTP适配与独立故障测试可在接口稳定后并行。数据库初始化脚本和公开DTO一处维护，不为并行开发复制契约。

## 4. H01—H18验收基线

每项均须断言实际数据库／HTTP／进程结果，不以函数名或退出码替代。下面为编码前初始状态；完成后另列最终结果，保留真实形成时间。路由权限缩写及精确响应均以HTTP主规格为准。

| 编号 | 入口、动作或故障 | 通过条件与观察 | 初始状态 |
| --- | --- | --- | --- |
| H01 版本与初始化 | init／serve，v1—v4交叉打开、错身份／密钥／DDL、重复init、初始化中止 | 只有显式v4服务入口接受完整服务库；首管理员一次创建且auth_rev=0／无Grant；失败不覆写已有材料、不自动补空库；重开冻结及旧boot失权保留 | 未验证 |
| H02 服务生命周期 | 非法配置、相对路径、坏TLS／Origin、第二实例、请求中退出及重启 | 严格配置失败可解释；真实TLS可登录；单实例锁覆盖服务寿命；提交后的断连不丢回执；关闭资源和密钥副本，未验证平台明确列出 | 未验证 |
| H03 会话与CSRF | POST sessions、GET me、DELETE current；错口令、时间边界、Origin／CSRF缺失／错误 | Cookie属性完整，数据库无原Cookie；同会话me返回同一CSRF；空闲30分钟／绝对12小时到点失效；无效来源和CSRF无变更；重复退出204 | 未验证 |
| H04 密码与预算 | Unicode口令边界、异常编码成本、未知账号、超过尝试／并发预算 | 不trim／归一化；Argon2参数和盐可核验；认证结果不泄漏账号存在性；429含Retry-After，限速容器有界；成本测量不被表述成容量承诺 | 未验证 |
| H05 账号及Grant | POST／GET／PATCH users，GET／PUT grants；同key重放、错ETag、禁用最后管理员、撤权与许可竞争 | 新用户0修订无Grant；一次更新一次修订；原安全回执／ETag不随当前对象变；最后管理员保护；密码修改或禁用失效旧会话；Grant变化保留登录但最终事务拒绝已撤权限；管理员不自动部署 | 未验证 |
| H06 节点登记与秘密 | POST／GET nodes、enrollment-tokens、revoke；token过期、重复签发、replace_pending、同key重报 | pending无执行身份；token仅首次展示，重放null及当前状态；认领材料不进安全回执或日志；撤销保持任务／claims且旧凭据无权继续 | 未验证 |
| H07 Agent认领／轮换 | enroll及rotate在提交后丢响应、不同候选重试、旧代连接迟到 | 同token／node／ledger／candidate只形成一个绑定；不同请求401无泄漏；轮换新代唯一、旧代失效；候选可用于正式认证确认；不新建账本身份掩盖失败 | 未验证 |
| H08 HTTP结构和错误 | 对全部路由注入重复键、未知键、非法UTF-8、错类型／null、错误ID、未知query和超过8MiB | 使用固定HTTP错误形状、code、request_id与field_errors；不回显秘密或SQL；所有API no-store；成功及错误响应均符合主定义且无多余内部字段 | 未验证 |
| H09 canonical与幂等 | 方法／路径／If-Match／正文、对象键序、合法转义／默认值；同key密码或配置变化、并发提交及重开 | canonical正反例稳定；秘密输入参与HMAC而不持久明文；同用户跨K路由共用空间；相同请求原回执，不同输入409；已成功重放在密钥额度耗尽时不新增包装 | 未验证 |
| H10 版本仓储及CAS | 新建Stack、GET Stack／Version、POST versions；两用户同基准、同名、跨Stack版本、加密行替换 | 原文件字节和数组顺序往返不变；一个成功一个412，无半份Stack；同名409；组合外键及AAD拒绝换行；保存不调用CLI、不自动修正Compose内容 | 未验证 |
| H11 读权限与分页 | users／nodes／grants／stacks／operations列表；节点／动作不足、签名／范围／用户／auth_rev／时限篡改 | 404与403正确；管理员元信息裁剪；每页重查权限，无total泄漏；游标15分钟到期410；页边界不漏重复已有排序项，不把普通列表称固定恢复快照 | 未验证 |
| H12 回执安全读取 | GET submissions、已成功K路由重放；变更目标已更新／撤权／失去写但保留读权限 | 只读当前用户key；使用result_type规定的当前读取权限；原状态／body／ETag不从当前对象重建；已失写但有读可读原回执；token／口令／Config／CSRF不进入安全响应 | 未验证 |
| H13 正式Apply受理 | POST apply；固定V2后保存V3、离线／冻结、换session、撤权、并发占用、key响应丢失 | 正式身份／版本／会话校验，同一事务建立操作、两项claims、prepare、幂等、审计和last_operation_id；V2原输入保持；拒绝没有残留；服务路径不调用Fixture | 未验证 |
| H14 查询、取消与输出 | GET operation/list/output、POST cancel；缺读取／输出／取消权、UTF-8字节边界、重复取消 | HTTP只返回白名单；无结果为null；终态pending仍可见；取消不是即时无副作用结论；输出至多既有1MiB流，不分裂字符、不重执行；同操作取消幂等 | 未验证 |
| H15 当前绑定及业务钩子 | 正式凭据验证／绑定／断线端口；旧generation或session迟到；原结果事务与Stack投影 | 只绑定已认领且ledger相符节点；旧断线不清新session；当前模块无真实通道时不能online／受理；成功版本指针与可信结果同事务，失败／未知不覆盖历史成功 | 未验证 |
| H16 原子失败与竞争 | 账号／Grant、认领、版本／回执、Apply、取消、结果指针事务的语句失败及延迟外键Commit失败 | 无成功应答、无半份业务／回执／审计、无丢失claims；最终事务能拒绝计算期间发生的撤权／会话改变；包装额度可消耗但不退回，单连接无嵌套死锁 | 未验证 |
| H17 重开及进程边界 | 正式HTTP服务在提交前／后、响应前终止并重启；服务旧Store尝试写入 | 已提交用户／节点／版本／回执可读，未提交不存在；旧session／凭据按持久规则处理，节点先恢复；同key不产生第二份受理；进程实验不表述为掉电耐久 | 未验证 |
| H18 回归与交接 | 原v1—v3协调／释放／恢复及Agent相关回归，契约检查、独立代码与文档阅读 | 新v4未破坏旧入口或共有证据规则；文档能追到请求／Tx／数据／返回及故障判断；记录依赖版本、准确测试批次和未接线范围，不将HTTP模块通过扩为M1通过 | 未验证 |

## 5. 路由覆盖与验证组织

每条[HTTP主路由](../specifications/http-api.md#5-路由全集与成功响应)均须有成功、结构失败和权限失败覆盖；条件／幂等路由另验证错误ETag、同key重放及冲突。测试表登记方法与模板，不只以“账号测试通过”代替全部路由。Agent的两个引导路由单独校验拒绝Origin及Bearer行为；后续connect端点不混入已交付列表。

存储测试直接检查业务行、关联、密文原文和幂等／审计同事务；HTTP测试通过真实登录／认领取得身份，用TLS测试服务发送原始请求，检查返回对象及Cookie。成功Apply的受控集成需要正式认证／绑定及恢复端口，不允许手工改frozen或用Fixture跳过前提。无真实任务通道的服务进程只证明管理HTTP可运行，其Apply拒绝也是应验边界。

故障实验选择真实SQLite语句和Commit故障，而非生产代码故障开关；进程边界使用辅助进程标记点后终止并重开，保存具体提交事实。口令和Config均用假值，测试还检查普通日志／审计／回执没有秘密。Argon2成本、网络时限、分页与多文件容量记录实际测量，不凭常量称生产性能已通过。

## 6. 环境、复验与交付限制

现有工具链为Go1.27.0、CGO=0、modernc SQLite，具体继承版本以释放依赖交付记录复核。本模块本地开发可使用临时SQLite、TLS测试服务和辅助进程；Linux运行产物的权限、排他锁、文件／父目录同步及服务关闭须在Linux环境验证后才可支持安装说明。Argon2依赖已加入，x/crypto v0.59.0带入x/sys v0.50.0和x/text v0.44.0；构建与回归见下记录。

实现后拟运行受影响包测试、`go test ./... -count=1`、`go vet ./...`、`go build ./...`、`go mod verify`、契约及文档检查；当前没有执行记录。正式Manager进程的初始化、登录、登记／认领和保存版本需另按实际命令复验，不能仅用httptest结果冒充安装验证。真实Docker／Compose不是本模块存储及HTTP的先决条件，但首版E及V03—V07仍必须取得真实环境证据。

完成时逐项补H01—H18最终状态、代码版本、测试名称及实际断言、运行环境、失败修订与限制，再依据最终代码核对规格和主定义链接。若任一路由／事务／运行条件未实现或未验证，明确记下，不以规划已保存当完成。下一依赖是正式任务传输、真实Agent准备／执行及Web；未知执行的人工解除和备份恢复仍按首版O12／V07门槛另行实现。

2026-11-12编码前接口补充：具体Go边界及初始化失败行为已保存于规格§10.1，HTTP接线与存储实现据此并行；本条是实施前基线补充，不是测试结果。


## 7. 2026-11-12服务内核实现与核验

事前规格及H01—H18先保存于本轮工作树，依赖释放基线`6f92eb3`；服务源码尚未独立提交，本表记录本轮真实工作树测试，不伪造提交号。内部职责、实际DDL和接手索引见[服务规格§12](../specifications/m1-manager-service.md#12-当前源码接手索引与实现限制2026-11-12)。HTTP／运行模块分别使用独立实施记录，整条H标准的进程、TLS和平台部分不由本表代验。

Windows amd64、Go1.27.0、CGO=0、modernc SQLite3.54.1；临时本地SQLite以及一个真实Agent v4库参与恢复消息交换。Manager身份、登录、Grant、认领、凭据认证、绑定和恢复完成均走正式入口，不写SQL解冻。Agent测试绑定及后续ready／执行result属于受控证据；测试证明服务受理／投影规则，不能称实际Compose部署或真实传输通过。

| 编号 | 内核结果与证据 | 仍需的模块／证据 |
| --- | --- | --- |
| H01 | 内核通过：首管理员、v3/v4互拒、重复init不覆盖、Open使旧boot无权且既有会话可重验；`TestServiceAuthenticationAndVersionBoundary`、`TestServiceMalformedCanonicalAndInitializationPreservation` | 文件初始化中止与完整命令由运行模块验证，尚不能称整项通过 |
| H02 | 未验证（此切片不实现HTTP进程） | 运行模块TLS/锁/退出及Linux权限 |
| H03 | 内核通过：真实登录、固定CSRF、错误秘密拒绝、空闲和绝对到点失效、密码撤销旧会话；SQL读取失败不伪装401 | HTTP Cookie/Origin/CSRF完整路由由适配器验证 |
| H04 | 内核通过：固定Argon2id成本、Unicode原文、两并发预算；创建＋验证观测约189—349ms，波动含同时测试负载，不是容量承诺 | 来源和用户名有界限速／Retry-After由HTTP验证 |
| H05 | 内核通过：新用户0修订、Grant一次修订且不退出、撤权实时生效、最后管理员保护、原响应ETag不变；恶意未知键不可改密码或撤销会话 | HTTP逐路由及全场景更新竞争继续联测 |
| H06 | 内核通过：pending、一次展示token、同key秘密null、同请求重报不新建；认领／撤销持久实现 | 所有过期／replace_pending／撤销HTTP组合继续联测 |
| H07 | 内核通过：正式认领原请求重试固定绑定、不同候选拒绝、轮换新代可认证且旧代／旧句柄失效、认领审计失败回滚全部事实 | 真实Agent候选秘密耐久保存与网络响应丢失未验证 |
| H08 | 内核通过：严格共享JSON、精确字段集合、未知cancel404、固定storage_busy/full；`TestServiceMalformedCanonicalAndInitializationPreservation`及审阅回归 | 完整HTTP原始编码、体积、头和错误白名单由适配器验证 |
| H09 | 内核通过：Unicode等价转义及默认false同摘要、异输入冲突、8并发同key一版本、耗尽包装额度仍可重放 | 全HTTP请求canonical组合继续联测 |
| H10 | 内核通过：Config原文件字节往返、两并发请求条件保存一个201一个412、无半份Stack、跨版本密文替换AAD拒绝 | 未把Compose语法或部署能力检查混成保存行为 |
| H11 | 内核通过：limit+1分页无重复、游标换范围拒绝、当前修订变化拒绝、管理员元信息裁剪 | 所有分页路由/到期/极限组合仍需HTTP覆盖，整项不标完全通过 |
| H12 | 内核通过：撤写保留读仍可重放旧回执，撤node.read后404；同用户key隔离由SQL查询约束 | 全result_type的HTTP路径联测 |
| H13 | 内核通过：正式认证与双库恢复屏障后Apply；未恢复409，固定版本、两claims、prepare、回执、审计和last_operation_id同提交；FULL时无半个operation/claims | 无正式网络worker时不形成部署链；真实执行器仍后续依赖 |
| H14 | 内核通过：正式ready/许可/取消，取消不伪造终态；受控结果只返回白名单；UTF8字节切块不拆字符；不存在cancel404 | HTTP权限／实际输出采集由对应模块续验 |
| H15 | 内核通过：旧代句柄拒绝，绑定保留冻结，原结果事务推进成功版本／观察指针，终态pending仍持claims | 节点整体观察采集、正式传输worker、真实成功证据未实施 |
| H16 | 内核通过：真实SQLite语句审计失败、延迟外键Commit失败、外部写锁BUSY、保存／Apply FULL，全都不发布半份业务/回执/审计/claims | 全部具体操作的故障交叉矩阵尚非穷尽验证 |
| H17 | 部分通过：服务库重开及旧boot拒绝已测 | 独立服务进程提交前/后终止实验由运行模块完成，未声称掉电耐久 |
| H18 | 旧v1—v3协调／恢复／释放与新增服务包回归通过；独立审阅探针已提升为`service_review_test.go`，其余服务断言在`service_test.go` | 全仓构建、文档／契约检查与HTTP/运行集成由根代理汇总；不扩为M1完成 |

审阅已修复：PATCH字段子串白名单、Agent主体审计缺口、未知取消的错误映射、共享Tx中SQLite错误类别丢失、会话SQL错误判断顺序。PATCH紧急修复的规格补记为事后记录，其他增量先记录行为和受影响项再改。独立审阅来源和补充实验保存在工作树缓存；关键测试已进入源码测试文件，后续可直接重复执行。

最后复验命令为`go test ./internal/manager/operations -count=1 -json`、`go vet ./internal/manager/operations`及`go mod verify`；完整JSON证据在`.cache/m1-service-core-tests.jsonl`。精确通过数量与最终耗时由本节末追加最终批次结果，不以此前小批次替代最后代码检查。代码／规范已可交给HTTP和运行模块联编；整个服务模块H01—H18仍需这些模块共同闭环。

最终内核批次：2026-11-12，272个test/subtest通过、0失败、0跳过，operations包耗时29.566s；其中17个Service/ReviewM1顶层测试。go vet通过，go mod verify返回all modules verified；文档检查46篇/671本地链接/168表通过（仅静态链接/锚点/表形/fence）。此记录仍不代表真实TLS或Linux部署通过。

### 7.1 恢复生产入口修订基线（2026-11-12，修复前保存）

受影响H13/H15/H18：已有272项通过仍未覆盖服务v4已受理任务出现在真实Agent库ready扫描的分支；`ProcessRecovery`调用v4禁用的`RequestCancelFixture`而返回ErrSchema。生产扫描错误回复同样误调`RecordRecoveryFailureFixture`。本次先补真实Manager v4／Agent v4 ready扫描失败回归，再抽取规格§12约定的私有恢复取消／错误核心；旧Fixture限制和v1—v3行为不变。

| 对应验收 | 本次观察及通过条件 | 修订时状态 |
| --- | --- | --- |
| H13 | 正式认领、凭据认证、恢复完成、Apply和Agent持久ready之后，真实Agent扫描回复由ProcessRecovery处理为waiting；恰一份pending cancel，prepare停止待投递；无伪终态／结果，双claims保留 | 待验证 |
| H15 | 取消最终Tx重查当前绑定、active_boot、原round及操作尚无许可／结果；迟到轮次、替换会话、旧Store、新许可／结果均不能写入取消；重复处理幂等；扫描失败仅影响当前关联请求，expired可新扫描且保持冻结 | 待验证 |
| H18 | 检查全部生产Fixture调用；v4 Fixture仍拒绝；定向回归及operations包含旧v1—v3全部回归通过，记录本次实际命令／结果与未覆盖范围 | 待验证 |

实现范围限operations恢复路径、服务测试及本两篇文档；不更改release算法，不扩展成真实任务传输／Compose验证。完成后追加实际结果，保留本事前基线。

### 7.2 恢复修复结果（2026-11-12，修复后追加）

本轮原始失败已由真实Manager v4／Agent v4库复现：ready扫描、四种错误回复和Agent实际过期后的下一页均因生产调用Fixture返回`manager schema unsupported`。修复仅替换这两处生产调用并增加私有事务核心，未修改release算法。`service_recovery_test.go`保存6个顶层测试／19个test与subtest，最终定向批次4.726s全通过。

| 对应验收 | 本次结果与可重复观察 | 范围限制 |
| --- | --- | --- |
| H13 | 通过：真实Agent持久ready扫描后ProcessRecovery返回waiting；重复处理只有一个pending cancel，prepare变suppressed；操作仍accepted／held、无事件与结果、双claims保留且节点冻结；控制报文preparing／ready配合Manager ready状态也通过 | Agent绑定／ready仍是受控Fixture事实，未证明实际预检或网络cancel处理 |
| H15 | 通过：private recoveryCancel拒绝原round被替换、session被替换、旧Manager boot、新permit、新result、collecting及completed；每项操作快照与意图均无变化。cancel outbox真实SQLite触发器失败回滚flag、revision及prepare状态，去故障可重试。busy保持同请求原文；expired／changed／cursor_invalid允许新round，旧错误回复不能影响新round；密钥预算耗尽仍能保存取消意图 | 四类错误wire是明确受控传输映射；其中scan_expired另由真实Agent时钟过期产生原始ErrScanExpired并验证下一页／新scan；未新增真实传输或容量承诺 |
| H18 | 通过：生产Go文件中Fixture检索仅剩定义；v4两个Fixture仍ErrSchema。完整operations回归291项通过、0失败、0跳过，28.766s，含原v1—v3恢复／释放；go vet通过。最终文档静态检查48篇／696链接／180表通过 | 全仓联编及提交由根代理继续；此修复不把H01—H18整体或M1标为完成 |

本次完整命令：`go test ./internal/manager/operations -count=1 -json`、`go vet ./internal/manager/operations`、`node tools/check-docs.mjs`。JSON证据保存在工作树`.cache/m1-recovery-cancel-package.jsonl`，故障、实现与交接报告为`.cache/m1-recovery-cancel-fix.md`。本次源码尚未独立提交，沿用上述工作树与工具链，不能将前一批272项证据代替本次291项结果。


## 2026-11-12 v5运行Store衔接

v4服务原H验收记录保留。本轮在同一operations.Store内补显式schema5、认证hello、持久加密投递和多结果链；独立事前基线、MV01—MV08及证据见[v5运行Store实施](m1-manager-runtime-store.md)。v4入口/DDL未迁移，正式账号/配置API接受明确4/5，Fixture两者均拒绝。源码切片范围不包括真实Docker、跨机或完整M1发布；网络服务接线与总体交付仍由Agent运行、Manager运行及首版记录汇总。
