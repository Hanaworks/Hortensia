# M1 Manager账号、节点与配置服务实施草案

日期2026-11-12；起点`656e355`。状态：待审阅的编码前计划，尚未编码、运行测试或交付HTTP服务。本轮依赖正在推进的Agent v4／Manager v3双端释放最终基线；拟新增Manager服务v4。首版总范围仍按[推进计划](m1-first-version.md)记录，用户范围回复未在本文冒充已确认。

## 1. 本轮目标与范围

依据[模块规格草案](../specifications/m1-manager-service.md)完成C＋D管理路径：正式初始化和TLS HTTP服务、本地账号／会话／权限、节点登记及认领／轮换、配置版本和幂等回执，以及连接同一operations权威的Apply受理／查询／取消入口。现有操作、授权、claims、事件和释放不另建副本；生产handler不调用Fixture。

正式网络任务接线、Agent工作区和Compose、Web、备份隔离恢复及O12工具不是本模块交付内容。缺少正式通道时Apply必须明确拒绝，不能设置假online或用受控执行结果制造端到端通过。HTTP返回结构采用[现行主定义](../specifications/http-api.md)，共同Config和协议结构采用[Schema](../../contracts/agent-v1.schema.json)。

## 2. 编码前审阅与依赖

已读现行HTTP、数据、Manager及运行设计，核对operations.Store、版本分派、users／nodes／grants、AcceptFixture／accept、requirePermission、AuthorizeStart、RequestCancelFixture、seal及BindRecoverySessionFixture。

已经明确的实施取舍：显式服务v4沿用同一Store和SQLite；新用户auth_rev从0起，授权后许可修订仍大于0；把acceptTx／cancelTx抽成共享事务核心而不先调用Fixture再补业务记录；请求canonical HMAC主定义写在服务规格，HTTP主文档随后补权威链接。以上是实施协调选择，仍须以最终代码和验收证明，不是用户逐项确认。

| 审阅点 | 需在编码前落实的结果 | 当前状态 |
| --- | --- | --- |
| 双端释放依赖 | 固定Manager v3／Agent v4实现及验收版本，确认v4服务保留两种释放证明、恢复屏障及原文核对 | 等待依赖交付 |
| HTTP主定义衔接 | 链接canonical算法，确认Grant变化保持登录、最终事务查最新授权的行为，避免实现与共享会话说明各说一套 | 主文档canonical链接已同步；Grant边界已明确 |
| 密码依赖 | 锁定Argon2实现版本／许可证，记录64MiB成本和并发上限的初始实验方法 | 选定golang.org/x/crypto v0.59.0官方IDKey，BSD-3-Clause；64MiB/3轮/并行1/32字节输出/16字节盐；尚未下载或测成本 |
| v4结构／事务 | 复核新用户修订0、pending节点、Stack组合外键、操作服务关联、初始化完成标志及单连接非嵌套规则 | 规格草案已提出，DDL未写 |
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

现有工具链为Go1.27.0、CGO=0、modernc SQLite，具体继承版本以释放依赖交付记录复核。本模块本地开发可使用临时SQLite、TLS测试服务和辅助进程；Linux运行产物的权限、排他锁、文件／父目录同步及服务关闭须在Linux环境验证后才可支持安装说明。Argon2依赖尚未加入，不声称已经构建成功。

实现后拟运行受影响包测试、`go test ./... -count=1`、`go vet ./...`、`go build ./...`、`go mod verify`、契约及文档检查；当前没有执行记录。正式Manager进程的初始化、登录、登记／认领和保存版本需另按实际命令复验，不能仅用httptest结果冒充安装验证。真实Docker／Compose不是本模块存储及HTTP的先决条件，但首版E及V03—V07仍必须取得真实环境证据。

完成时逐项补H01—H18最终状态、代码版本、测试名称及实际断言、运行环境、失败修订与限制，再依据最终代码核对规格和主定义链接。若任一路由／事务／运行条件未实现或未验证，明确记下，不以规划已保存当完成。下一依赖是正式任务传输、真实Agent准备／执行及Web；未知执行的人工解除和备份恢复仍按首版O12／V07门槛另行实现。

2026-11-12编码前接口补充：具体Go边界及初始化失败行为已保存于规格§10.1，HTTP接线与存储实现据此并行；本条是实施前基线补充，不是测试结果。
