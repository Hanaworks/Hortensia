# Docker Manager 接口设计

日期：2026-11-02。版本：0.1。状态：待审阅。本文定义逻辑交互契约；传输协议、URL、线上编码和技术组合尚未定稿，也没有已运行的接口实现。

Web 通过 Manager 管理节点、配置和操作；Agent 主动连接 Manager，取得针对本节点的任务，报告观察与执行结果。保存、Copy、Apply、Node Cross、验证和来源清理分别表达意图与结果，调用者能分辨请求受理、操作完成及业务验证。

本文承接[需求说明](requirements.md) v0.9、[功能设计](functional-design.md) v0.5、[架构设计](architecture.md) v0.3 和[数据设计](data-design.md) v0.3，面向各端开发者及测试者。接口名称和字段是本稿建议，JSON 仅用于展示完整逻辑消息，不代表已经选择 JSON RPC 或确定 HTTP 路径。Node Cross 的切换、验证清单与回退仍待设计，其接口在第九节保留明确边界。

## 一 接口参与方与范围

| 交互 | 发起方与接收方 | 交换内容与职责 |
| --- | --- | --- |
| 用户管理接口 | Web → Manager | 登录、节点与资源查询、配置保存、Copy、Apply、资源动作、任务查询及管理设置 |
| 节点接入 | Agent → Manager | 一次性注册、身份认证、协议能力、当前连接和恢复核对 |
| 任务控制 | Manager ↔ Agent，连接由 Agent 建立 | 固定任务、准备结果、开始许可、事件、结果确认与查询 |
| 文件传输 | Web ↔ Manager；Agent 主动向 Manager 取得获准内容 | 原始文件、受限执行输入和任务专用凭据，分别鉴权与限流 |
| 诊断与交互 | Web ↔ Manager ↔ Agent | 日志、统计、受限操作输出及容器终端 |
| 节点执行边界 | Agent → Docker CLI／Compose／Engine | 结构化动作映射到受支持的原生调用，遵守不构建约束 |

PVE 与 Portainer 的产品参照用于入口和操作体验，接口不向浏览器透传 Docker socket、任意 Engine API、宿主机 shell 或 Agent 管理凭据。容器终端是绑定具体容器的专用交互。

本稿采用“逻辑契约先行，传输适配随后定稿”的组织方式。可评估 HTTPS 请求配合流式连接，也可在 Agent 通道采用 WebSocket；无论选择哪种方案，身份、授权、去重和结果核对规则相同。架构中的协议候选仍保留候选状态。

## 二 通用数据与消息约定

### 类型与字段含义

| 类型或字段 | 逻辑含义及约束 |
| --- | --- |
| `*_id` | 不透明、不可复用的对象标识；显示名不作身份，示例中的 `node_a` 等仅为可读样例 |
| `*_at` | UTC 时间点，毫秒精度；示例用 `2026-11-02T03:00:00.000Z`；缺失事实为 null |
| `*_seq`、`*_version` | 对象作用域内的非负整数；示例用十进制字符串，避免展示时损失大整数精度；不可按字符串字典序比较 |
| `*_bytes`、`offset_bytes` | 非负整数字节数或从零开始的字节偏移，与字符数不同 |
| `expected_version` | 调用者读到的可变记录版本；覆盖式修改须提供，缺失时拒绝，不能隐式使用最新值 |
| `revision_id`、`input_id` | 指定不可变配置和已固定输入；执行不能把它们换成“当前最新版本” |
| `digest` | 原始内容或确定性输入的摘要，含算法及格式版本；摘要不能代替权限 |
| 字段缺省、null、空集合 | 缺省表示未提供；更新时 null 仅用于明确可清空字段；空数组表示已知为空，不能代替未采集 |
| `observation` | 目标身份、采集与接收时间、fresh／stale／unavailable，以及可空数据；过期观察不是实时结果 |

名称、路径与原文保留规则以数据设计第二、四节为准。参数中的数组是否有序由字段说明决定；Compose 文件和环境文件顺序必须保留。管理器名称长度按 Unicode 字符规则校验，内容与传输限额按字节校验，线上编码定稿时须固定同一计数算法。

请求不得带服务端拥有的字段，例如操作者、实际权限、结果状态、凭据明文返回开关、连接代次或成功部署序号。接收方拒绝未知变更字段及动作，不静默忽略影响行为的参数；查询响应可增加可选展示字段，未知枚举按“不支持”显示而非猜测为成功。

### 用户请求与返回

用户请求由 `contract_version`、`request_id`、`method`、可选 `target` 和 `params` 组成。产生副作用的提交还带 `request_scope_id` 与 `request_key`；登录、初始化等尚无普通会话的入口使用各自专门流程。身份取自认证上下文，不接受正文中的 `user_id` 充当调用者。

`target` 按动作声明 node、stack、binding 或原生资源引用；存在多级标识时逐项核对归属。更新操作使用 `expected_version`，保存配置另带 `base_revision_id`；创建对象不要求已有对象版本。空 `params` 用空对象，不能省略本方法的必填字段。

返回含原 `request_id` 和以下一种结果：

| `outcome` | 返回内容 | 调用者含义 |
| --- | --- | --- |
| `completed` | `data`，包含本方法定义的结果 | 本次同步动作已完成；Copy 保存完成不等于部署完成 |
| `accepted` | `data.operation_id`、当前操作状态及可见目标摘要 | 操作已持久受理，随后查询或订阅结果 |
| `rejected` | `error.code`、脱敏 `message`、字段问题及下一步指引 | 该请求未被受理为新操作；不替代此前可能已受理请求的结果 |

响应未到达时不能推断为 rejected。请求校验、授权、去重判断与持久提交的顺序见第四节；操作失败通过已存在 Operation 报告，不把长任务失败编码成当初请求从未被受理。

### 分页、过滤和新鲜度

列表请求使用有界 `limit`、允许的过滤字段及可空 `cursor`，返回 `items`、`next_cursor`、采集信息及必要的部分失败说明。游标绑定当前用户、授权版本、过滤条件和稳定排序；不可借游标改变访问范围。过期或失效返回 `CURSOR_INVALID`，客户端重新分页；实时变化列表不承诺跨页快照。

汇总逐节点返回状态和时间，一个节点失败不使全部结果归零。可见计数从授权集合计算；无权访问的对象及操作引用不出现在冲突信息、筛选选项或订阅事件中。对不可见对象统一返回 `NOT_FOUND_OR_FORBIDDEN`。

### 限额与能力

`capabilities.get` 返回当前契约版本、启用的功能、可查询的动作目录和限额名称；返回能力不代表授予权限。Manager 的配置须提供请求大小、单文件及文件包大小、文件数、名称长度、分页上限、转换输入大小、并发上传与订阅数、输出缓冲预算、任务受理时限等数值；Agent 协商可接受消息和片段大小、并发及保留能力。

有效上限取各端配置与协商能力中较严格者，超限在接收或解析过程中尽早拒绝并给出允许上限；不能先读入无限内容。心跳、空闲超时和重连退避参数也由连接协商返回，使用单调时钟判断间隔，不用远端墙钟决定任务是否已完成。当前不预填未经容量验证的数值；部署前须有明确默认值及最小最大范围。

## 三 身份与权限目录

### 用户身份

`system.initialize` 只在未初始化状态接受首位管理员信息，管理员创建与初始化状态在同一事务提交。初始化入口还需部署侧一次性引导凭据或等效本地受控入口，具体交付方式在运维设计确定；不能仅因系统未初始化就允许任意网络访问者抢先创建管理员。响应丢失后查询初始化状态或正常登录，不再次开放初始化。

`session.create` 输入登录名与密码，返回会话状态、到期时间、当前用户信息及服务端生成的请求作用域。认证失败返回统一错误；登录限流不输出密码。`session.get` 查询当前身份与作用域，`session.close` 撤销会话，`account.password.change` 修改本人密码并明确其他会话处理结果。找回密码走管理员重置或运维恢复，不匿名清空初始化状态。

若采用浏览器 Cookie 会话，须启用受保护 Cookie、跨站请求校验及实时连接 Origin 检查；具体 Cookie 名称、SameSite 策略、CSRF 载体和部署域名随传输适配定稿。Agent 凭据和用户会话不能互换，长期秘密不进入 URL、普通审计或追踪日志。

### 建议动作码

下表的花括号表示目录缩写，实现时每个动作独立存在，例如 `container.start` 与 `container.stop` 分别授权。便捷角色模板展开为明确授权；系统管理资格不自动绕过节点动作检查。

| 动作码 | 范围及允许内容 | 组合要求 |
| --- | --- | --- |
| `node.read` | 节点概览和非敏感资源摘要 | 只返回获准节点；完整配置、日志和凭据另行授权 |
| `config.read` | Stack 原文、附属文件、完整配置查看与导出 | 操作输入中的配置同样受此权限保护 |
| `config.{create,update,adopt,delete}` | 管理器内配置创建、保存、接管和删除 | 编辑既有配置还需 read；不改变 Docker 运行资源 |
| `stack.deploy` | Apply、部署、更新及涉及节点依赖的校验 | 目标配置可用；使用秘密还需相应凭据使用授权 |
| `stack.{start,stop,restart,remove}` | 项目运行控制或移除运行资源 | remove 不隐含卷删除，具体范围固定在输入中 |
| `container.{create,start,stop,restart,delete}` | 对应容器动作 | 不借容器入口绕过所属 Stack 的冲突控制 |
| `diagnostics.{logs,stats}` | 应用日志、受限统计与诊断 | 目标明确，日志可能包含敏感内容 |
| `container.exec` | 指定容器的交互执行 | 单独高信任授权，不授予宿主机通用 shell |
| `image.{pull,tag,delete,prune}` | 对应镜像操作 | 拉取认证仓库需凭据使用权；prune 明确选择及影响范围 |
| `network.{create,delete,prune}` | 对应网络操作 | 重新核对引用，不由缓存判断绝对安全 |
| `volume.{create,delete,prune}` | 对应卷操作 | delete／prune 明示持久数据影响并选择具体范围 |
| `credential.use` | 某凭据、节点及用途 | 与对应节点动作同时满足；不返回可读密码 |
| `system.{users,grants,nodes,classification,credentials,backup,recovery,audit}` | 对应管理功能 | 由系统管理资格控制；需要分别记录实际动作 |

Copy 组合来源 `config.read` 与目标 `config.create`；Apply 组合目标 `stack.deploy` 和所需凭据授权，不额外要求来源运行控制。Node Cross 的每个阶段按实际动作检查两端权限，不能用一个 migrate 标志授予任意来源删除权。

操作查询、事件与输出按当前访问权限过滤：对象概览可显示脱敏结果，完整配置需要 `config.read`，运行输出按诊断或配置内容分别授权。原提交者身份不保证撤权后仍可读输出。权限不足时从结果中省略不可见关联，不能先泄露另一节点或操作者详情再返回拒绝。

## 四 并发、重发与错误

### 去重与重试

变更请求的作用域绑定用户、认证会话与 Manager `recovery_id`；客户端从服务端取得，不自行选择。请求键只在该作用域内使用一次。Manager 检查当前身份和权限后，在事务内比较同键请求的语义输入并返回原结果，或记录新操作；同键不同输入返回 `REQUEST_KEY_REUSED`。

已有去重记录的比较先于把重发当新操作进行版本或资源冲突检查：首次保存已提升版本时，同键同输入的重发仍返回原保存结果。首次受理则执行正常版本、目标与冲突检查。去重不授予内容读取权限，返回旧结果仍需当前授权。

网络超时后首先用作用域和请求键调用 `requests.lookup`；查到操作则继续跟踪，未查到且原会话仍有效时可重发完全相同请求。查询缺失不是在途请求绝未受理的证明，同键重发由去重事务协调。重新登录或恢复后旧作用域失效，不能把同一旧请求自动换新键；通过对象操作历史找回结果，用户明确重试才新建请求并关联原操作。

详细结果已清理但键仍有效时返回 `REQUEST_RESULT_EXPIRED` 和可授权访问的操作引用；保留墓碑，绝不把该键当新请求。重复请求只影响交付次数，不代表可以重复 Docker 副作用。

### 版本和冲突

保存配置比较 Stack 的 `expected_version` 及 `base_revision_id`；冲突返回当前版本引用与授权范围内的摘要，不返回对方完整配置。客户端保留本地编辑，重新读取或合并后以新请求键提交，不能自动替换版本号覆盖。

同一项目或资源的冲突动作返回 `OPERATION_CONFLICT`，可附获准查看的既有操作引用。离线节点的新变更返回 `NODE_UNAVAILABLE`，不形成恢复后突然执行的长期队列。已受理但尚未获许可的任务超过受理时限时，先确认未开始；无法确认则核对，不能按超时释放占用。

### 统一错误目录

| 错误码 | 含义及调用者下一步 |
| --- | --- |
| `UNAUTHENTICATED`、`SESSION_EXPIRED` | 登录或重新登录；恢复原操作查询，不能自动用新作用域重复变更 |
| `NOT_FOUND_OR_FORBIDDEN`、`ACTION_FORBIDDEN` | 不暴露不可见对象；已可见对象的动作不获准时提示所需权限 |
| `INVALID_ARGUMENT`、`PRECONDITION_REQUIRED` | 返回字段路径与脱敏原因；修正必填项或重新取得版本 |
| `VERSION_CONFLICT`、`TARGET_CHANGED` | 配置版本或实际 Engine／绑定不匹配，重新读取并核对 |
| `REQUEST_KEY_REUSED`、`REQUEST_SCOPE_EXPIRED`、`REQUEST_RESULT_EXPIRED` | 分别表示键输入冲突、作用域失效、结果已清理；遵守前述重试规则 |
| `OPERATION_CONFLICT`、`RESULT_UNKNOWN` | 先跟踪或核对原操作，不启动冲突副作用 |
| `NODE_UNAVAILABLE`、`DEPENDENCY_UNRESOLVED` | 节点不可用或目标依赖未满足；不把离线当空环境 |
| `UNSUPPORTED_ACTION`、`CAPABILITY_MISMATCH`、`BUILD_UNSUPPORTED` | 调用不支持的能力，保留原输入并说明范围；不静默改写为其他行为 |
| `LIMIT_EXCEEDED`、`RATE_LIMITED` | 返回适用限额或等待提示；重试变更仍使用原有效键 |
| `CANNOT_CANCEL`、`RECONCILIATION_REQUIRED` | 无法证明未开始，或需要人工证据；不报告已安全取消 |
| `PROTOCOL_INCOMPATIBLE`、`IDENTITY_REVOKED`、`RECOVERY_REQUIRED` | 禁止新执行，升级、重新授权或进入恢复核对 |
| `INTERNAL_ERROR` | 脱敏说明与追踪标识；副作用状态不明时给操作引用或提示按请求键查询 |

错误携带 `recovery` 指引，如 `refresh`、`lookup_request`、`follow_operation`、`reconcile`、`fix_input` 或 `none`。不能仅用一个 `retryable=true` 让客户端盲目重发变更。若选用 HTTP，状态码按 [HTTP 语义](https://www.rfc-editor.org/rfc/rfc9110.html)映射：受理与完成、认证失败、版本冲突分别表达；响应码本身不代替操作状态。

## 五 用户接口目录

目录中的动作名称是逻辑方法，CRUD 缩写在物理契约中需展开。所有修改继承第四节的请求键和适用版本检查；所有查询继承当前授权与分页规则。

| 逻辑方法 | 主要输入 | 返回与前提 |
| --- | --- | --- |
| `capabilities.get`、`session.*` | 版本或登录／会话信息 | 能力与会话信息，遵守第三节 |
| `users.*`、`grants.replace` | 账号字段；用户、节点、明确动作列表、expected_version | 管理记录新版本；禁止并发移除最后可用管理员 |
| `nodes.{list,get,create,update,detach}` | 分类过滤或节点字段；更新版本；解除时管理配置保留选择 | 节点记录和观察；detach 不删除运行资源，未知任务先核对 |
| `node.enrollment.issue`、`node.identity.revoke` | node、当前版本、到期策略或撤销原因 | 一次性注册材料或撤销结果；只向获准管理员交付秘密 |
| `groups.*`、`tags.*`、`node.classification.update` | 分类字段、node、group 或 tags、expected_version | 组织关系与新版本；不会附带授权继承 |
| `stacks.{list,get,create}` | node、名称、初始配置清单或查询过滤 | 创建返回 Stack／revision，默认不部署 |
| `config.{get,save,export}` | Stack、revision；保存含基础版本、完整文件清单和部署输入 | 原始文件或新 revision；保存不改节点运行目录 |
| `config.check` | 固定 revision、目标、检查模式 | 带时间和目标的检查结果、未检查项；本地结构检查与节点依赖检查分别授权 |
| `stack.apply` | Stack、固定 revision、预期 binding／Engine、执行选项、目标凭据引用 | accepted Operation；沿用部署规则，不处理来源 |
| `stack.control`、`stack.remove_runtime` | 固定对象、start／stop／restart 或明确删除范围 | accepted Operation；数据删除需额外明确范围与权限 |
| `stack.delete_config` | Stack、expected_version、历史保留选择 | 管理配置删除结果；Docker 资源保持原有状态 |
| `stack.copy` | 来源 Stack／revision、目标 node／名称、调整后的目标输入 | 独立目标配置与复制 Operation；不自动 Apply |
| `resources.{list,get}` | node、kind、原生标识、摘要或受限详情视图 | 带观察时间的数据，详情分别授权 |
| `container.create`、`resource.action` | 固定 node／Engine、资源类型和原生引用、动作专用参数 | accepted Operation；参数仅在方法允许清单内，不接受任意 CLI 文本 |
| `projects.discover`、`stack.adopt` | node；已有项目引用、原文件、目录和归属核对依据 | 发现信息或管理绑定；接管不自动部署，缺失配置保持明确 |
| `credentials.*` | 用途、名称、授权范围；设置时的秘密值 | 元数据、不可变值版本及可用性；普通读取不返回密码 |
| `run.convert` | 命令文本、目标转换规则版本、用户提供的附属文件 | 草稿、已映射项、差异、缺失项和阻塞项；不访问节点或执行输入 |
| `operations.{list,get,cancel,reconcile}` | 操作过滤／标识；取消版本；核对证据 | 状态、取消结果或核对记录，见第七节 |
| `requests.lookup` | 当前有效作用域和请求键 | 原受理结果、墓碑或尚未找到；仍鉴权 |
| `subscriptions.{open,close}` | 目标、流类型、参数及恢复游标 | 订阅身份、流能力和缺口，见第十节 |
| `backups.{create,get,download}`、`recovery.inspect` | 管理备份范围或已上传备份引用 | 备份 Operation／清单或只读恢复检查；实际替换流程见第十一节 |
| `audit.list` | 获准的目标、操作者和时间过滤 | 脱敏审计，分权过滤；不返回配置及密码正文 |

`resource.action` 的参数按类型严格区分：容器创建含镜像、命令参数数组、环境、挂载、网络等；镜像拉取含引用和凭据版本；网络创建含驱动和支持参数；卷创建含驱动参数，删除含对象清单和数据确认。移除 Stack 的命名卷选项、清理筛选条件、强制删除等影响范围必须显式传递，缺失不能默认扩大。具体原生参数支持清单随兼容版本验证补齐，当前不声称覆盖 Docker 的任意选项。

配置读取、运行检查、Copy、导出及转换各自返回必要的差异与未解决依赖。转换状态建议为 `complete`、`needs_input` 或 `unsupported`，其中 complete 仅表示支持范围内完成转换，不代表可在任意节点部署。`run.convert` 不产生 Docker Operation，用户主动保存后才进入配置记录。

## 六 配置、文件与固定输入

### 上传与保存

`uploads.begin` 指定目的、获准 node／Stack、逻辑文件名及预期字节数，返回限时上传标识和适用限额。未有 Stack 的新建上传绑定目标 node、用户与会话；已有 Stack 上传还绑定对象。上传标识不能被另一用户或目标复用；退出登录后不自动将未发布内容公开给新会话。

上传内容按原字节接收。片段包含上传标识、偏移和字节内容，重复偏移仅在内容相同时确认；越界、重叠不同内容或超限拒绝。`uploads.finish` 检查完整长度和服务端计算的摘要，返回就绪文件引用。具体分片和线上字节编码随传输选型确定；客户端不能自称上传已完成或指定服务端存储路径。

配置保存提交完整文件清单，每项为逻辑 `path`、`upload_id` 或当前获准 revision 中的文件引用、用途及受支持文件属性；缺少条目表示从新版本清单移除该文件。清单外业务目录不进入删除范围，旧 revision 仍不可变。相对路径不得逃出声明的文件包，Compose 原文中的外部路径作为依赖保留。

保存还提交有序 Compose 文件列表、项目目录、项目名输入、profiles、插值变量及环境文件来源。Manager 校验文件就绪和授权后原子发布新版本；冲突不发布半份配置。文件下载必须从获准 revision 或操作输入定位，不提供按全局 blob 摘要任意取内容的接口。

### 校验与执行输入

`config.check` 的 `structure` 模式只处理用户已提供、已获准的文件，不读取节点文件；不能声称等价于原生 Compose 完整解析。`target` 模式调用目标 Agent 的原生校验能力，要求目标部署及实际凭据使用权限，完整解析预览另需配置读取。检查结果固定目标、revision、输入摘要、工具版本和观察时间，并列出未验证项目。

Apply 提交后 Manager 形成不可变 ExecutionInput，包含动作、目标 Node／Engine、Stack／binding、revision 与文件清单、原生对象引用、实际项目名、稳定工作目录、文件顺序、profiles、环境输入来源、全部执行选项和凭据版本引用。未确认的绑定要先核对；输入改变必须新建操作。

建议文件摘要采用 SHA-256 原字节摘要；复合输入使用带格式版本的确定性编码，若采用 JSON 可评估 [JCS](https://www.rfc-editor.org/rfc/rfc8785.html)。物理契约定稿时必须列全参与摘要的字段、null／缺省规则和共享测试向量；各端不能自行拼接字符串。文件正文和密码不进入普通结果摘要，但不能漏掉会改变执行含义的秘密版本引用。

Agent 只能使用输入中的明确目录、项目和受支持执行参数，不接受浏览器传来的最终 shell 命令。部署禁用构建，构建依赖无法满足时返回 `BUILD_UNSUPPORTED` 或依赖错误，保留原配置而非删掉字段继续执行。镜像标签与外部依赖可能改变，观察结果单独保存，不回写原文。

## 七 Operation 与用户可见结果

`operations.get` 返回操作标识、动作、执行位置、获准的目标摘要、固定输入引用、状态、阶段、创建／开始／结束时间、状态版本、结果摘要、已知副作用、错误及是否需要核对。完整输入和输出另行授权；`phase` 描述进展，不替代状态。

| 状态值 | 与功能设计的对应 | 允许的后续处理 |
| --- | --- | --- |
| `accepted` | 已持久受理，未批准开始 | 前置失败、已证实未执行的取消，或批准开始 |
| `running` | Manager 已批准开始，Agent 正在或可能已经执行 | 接收成功、失败或未知结果；阶段可显示许可已发但尚无启动反馈 |
| `succeeded` | 取得该动作成功依据 | 查询实时状态，不能由此推断业务健康或迁移验证通过 |
| `failed` | 取得失败依据，可能部分改变资源 | 阅读结果并核对，用户可显式创建新的修复操作 |
| `unknown` | 无法确认执行或完成结果 | 查询、重连核对、人工记录；不能自动重做 |
| `cancelled` | 已证实未进入执行范围 | 保留取消记录；当前不保证撤销已开始 Docker 操作 |

取消请求引用 Operation 的 `expected_version`。尚未派发时可事务取消；已派发准备时，Manager 必须取得 Agent 持久确认未获得有效开始许可且未修改运行文件，才能完成取消。无法确认时返回待核对信息而非 cancelled；授权开始与取消在 Manager 中不能同时成功。

人工核对提交目标操作、当前状态版本、Agent 观察引用、执行者处置证据、资源及受管理文件观察、核对结论与说明。Manager 核查证据归属及新鲜度；纯文本“已停止”或 `force=true` 不能直接释放占用。断线旧 Agent 无法证明停止时，需通过运维流程停止或隔离执行者，再记录依据。核对权限由系统恢复管理与相应节点读取／控制授权共同决定；不能借恢复入口获得任意节点操作权。

解除占用只改变管理协调状态，保留原操作结果及人工依据；不伪造命令成功。已知可靠终态补报后，按数据设计的绑定与部署序号条件更新历史，旧结果不能倒退最近成功部署，也不能解除后来任务的占用。

## 八 Agent 主动连接与任务消息

### 注册和连接

Agent 验证 Manager 的服务端身份后，用限定节点的一次性注册凭据提交 `agent.enroll`，含注册标识、安装实例说明及协议能力。Manager 原子消费凭据、建立身份并返回节点身份与专用连接材料；它们不进入浏览器节点详情或审计正文。

注册响应丢失时，已消费的凭据不重新签发秘密，也不凭 Node ID 恢复身份。管理员核对是否产生孤立身份，撤销后重新签发注册凭据。后续可设计持有证明的注册恢复，但不能用匿名重试替代这一条件。

每次 `agent.connect` 均认证专用凭据，交换 installation、当前 recovery、身份、协议版本、能力和实际 Engine 观察；Manager 分配 `connection_id`、递增 `connection_generation` 和有效限额。连接代次由服务端控制。重复身份、已撤销身份、Engine 变化或恢复不一致进入受限核对，不能直接获得开始许可。

任务消息共有 `message_id`、`type`、当前连接上下文和 `payload`。当前连接上下文包含 installation、当前 recovery、node、Agent identity、connection 及代次；与旧任务的原始键分别保存。相同 `message_id` 不是跨重连的任务去重主键。

### 准备与授权开始

| 逻辑消息 | 方向与必需载荷 | 接收后的保证 |
| --- | --- | --- |
| `task.prepare` | Manager → Agent：task_key、固定输入与摘要、支持的 action、内容获取授权、准备范围 | 验证目标和能力，持久建立或找回同一任务；不同摘要拒绝；仅暂存及获准检查 |
| `task.prepared` | Agent → Manager：task_key、输入摘要、事件流、准备结果、实际身份、检查及未满足项 | Manager 记录结果；准备成功不代表获准修改运行目录或 Docker |
| `task.start_request` | Agent → Manager：task_key、输入摘要、准备依据、当前连接 | Manager 重查用户、节点、动作、凭据、冲突和准备有效性 |
| `task.start_permit` | Manager → Agent：permit_id、task_key、输入摘要、恢复标识、身份、连接代次、授权版本、适用部署序号、有效性信息 | 许可与 running 状态持久提交后发送；Agent 验证并持久记录，再进入执行路径 |
| `task.start_denied` | Manager → Agent：task_key、原因及处理要求 | 不启动；已证实未执行时结束，否则进入核对 |
| `task.query`／`task.report` | 双向：原 task_key、输入摘要、持久阶段、许可接受情况、执行者与结果依据 | 查询不执行任务；“未找到”须带记录完整性与恢复情况，不能直接当未执行证明 |
| `task.cancel_prepare`／`task.cancel_report` | 双向：task_key、取消请求及持久未开始证据 | Agent 本地协调取消与许可接受；无证据不能确认安全取消 |

task_key 为 installation、操作创建时 recovery、node、operation 的组合，跨重连不变。准备失败、磁盘记录失败或能力不足时，不进入执行。准备阶段不写实际运行目录；原生解析若需节点文件或远端内容，限制在该任务获准的范围，不能经预检执行任意来源内容。

开始许可只在当前有效连接上接受；尚未接受许可就失联，不启动。已经有效接受并持久记录的许可属于该任务执行范围，可在随后断线时继续并报告。重发许可不产生第二次执行；Agent 进程重启后先核对任务及执行者，不因有旧许可就再次启动 CLI。

许可发送或接受是否发生无法确定时保持核对状态。新连接不得直接复用旧许可；只有可信证据证明未开始、旧执行者已被排除且输入授权仍有效，才能重新协调尚未开始阶段。对执行可能已发生的情况，绝不自动给任务换标识再启动。

任务专用凭据通过独立受保护的获取流程交付，绑定 task_key、输入中的明确秘密版本、节点及用途；每次发放重新授权，不携带到其他任务。普通任务摘要只含引用。Agent 使用隔离的任务认证环境，按生命周期清理；准备阶段确需访问凭据时也必须取得对应授权，不能提前取得所有仓库密码。

### 事件与结果确认

`task.event` 包含原 task_key、固定 `event_stream_id`、递增 `event_seq`、输入摘要、事件类别、Agent 发生时间及载荷或受限输出引用。生产者身份由连接认证校验。Agent 在同一任务流中持久保存序号，重启和重发保留原值；Manager 自身事件使用该操作下另一条流。

Manager 按 operation、event_stream、seq 三元组去重；相同键相同载荷可重复确认，同键不同载荷返回冲突并核对。乱序事件可暂存，确认返回该流已持久接收的连续序号与缺口；不能只确认最大序号而吞掉中间事件。关键事件不得静默丢弃，输出丢弃需明确 gap 标记。

完成事件包含该动作的终态、固定输入摘要、实际执行依据、退出信息、已知部分副作用、错误摘要及输出引用。Manager 校验状态转换并持久提交结果后，才返回 `task.result_ack`，明确已记录的终态事件键。独立且充分的终态证据可用于取得结果，但事件缺口仍保留并请求补齐；不能伪造缺失日志。Agent 未收到确认时重传结果，不重做动作；本地清理仍须满足保留策略。

旧连接不能申请新许可。历史结果可以经当前认证连接的核对消息携带原 task_key 提交；身份替换后由管理员关联原身份与证据，不让新 Agent 冒认旧任务。Manager 恢复后的新 recovery 用于新连接，旧任务始终保留创建时 recovery。

### 连接、背压和版本

心跳仅说明通道活性，不能证明 Docker 可用或任务仍在运行。断线重连采用带抖动的有界退避；握手后先核对旧任务，再开放对应变更。慢输出不能阻塞开始拒绝、撤权、心跳或结果确认，控制与批量内容分别有队列预算；关键记录空间不足时停止接受新变更。

版本协商选择双方支持的契约主版本及功能集，必需能力不匹配就拒绝相应任务；不能默默删字段执行。可选查询字段可扩展，动作语义、必填项或摘要编码改变须升级格式版本。若采用 WebSocket，其帧、关闭和 Ping／Pong 按 [RFC 6455](https://www.rfc-editor.org/rfc/rfc6455.html)处理；应用层仍需本节任务确认与恢复机制。

## 九 Copy Apply 与 Node Cross

Copy 的输入固定来源 Stack／revision、目标节点、目标名称及经用户调整的文件和部署输入。来源文件、目标第一版、CopyOrigin、内部操作结果和请求去重在一次发布事务中建立；来源版本后来变化不让此次输入悄悄跟随最新值。复制过程中撤权则在提交前拒绝，目标已经合法发布后按目标权限管理。

Copy 返回目标 Stack、revision、复制操作及依赖提示；目标尚未部署。Apply 引用指定目标版本，产生独立目标执行操作。目标失败不停止或清理来源。完整来源配置已保存时，来源离线可用于 Copy；来源离线不能用于证明迁移切换已经发生。

Node Cross 独立于上述方法。以下为待细化的逻辑阶段，不构成可立即实现的执行接口清单：

| 阶段意图 | 必须固定或取得的信息 | 对外完成条件 |
| --- | --- | --- |
| 准备迁移 | 来源和目标部署、配置与数据范围、依赖、冲突及切换方案 | 准备结果明确，不能因此宣称已迁移 |
| 执行迁移 | 已确定的两端阶段操作、授权、切换与恢复依据 | 目标达到约定状态、来源完成切换，分别有执行结果 |
| 迁移后验证 | 本次迁移、两端对象和输入、检查批次、自动证据、必要业务确认 | 明确通过、未通过、未知或已失效；区别于预检及目标部署成功 |
| 选择来源处置 | 有效验证批次、用户选择、具体来源对象及持久数据范围 | 保留不自动重启；删除还需当前权限、状态核对及独立清理结果 |

验证通过不自动触发删除，用户未选择或验证未通过时不进入来源清理。删除请求必须绑定仍有效的验证和对象范围；来源复用、重新启动或相关状态改变后重新核对，不能凭旧 success 标志删除。持久数据删除分别选择和授权；清理失败单独报告。

迁移期间普通 Stack、资源控制和清理入口也要遵守相同冲突约束，不能通过通用删除接口绕过迁移来源保护。具体跨节点占用的取得和解除、Stack 身份延续、切换顺序、验证项目、失败回退及存储搬运未定稿前，不能发布可执行 Node Cross 的契约或将该能力标为可用。其他接口可继续细化，不用 Copy 成功标准填补这一缺口。

## 十 日志 统计与容器终端

`subscriptions.open` 指定目标、流类型、允许的参数、期望输出视图及可选恢复游标。Manager 校验当前权限后返回 `subscription_id`、流类型、是否可恢复、限制及初始观察信息。每条流事件绑定订阅，包含有作用域的序号或游标、时间、类型、载荷长度及数据；流内容按不可信输出展示，不执行其中的 HTML 或终端外链命令。

| 流类型 | 主要参数和恢复行为 |
| --- | --- |
| 操作事件 | operation、事件游标；持久记录范围内可补读，过期返回缺口并重新取操作快照 |
| 操作输出／容器日志 | 目标、允许的时间或条数范围、follow、stdout／stderr；可被截断，必须报告来源与缺口，重连是否重叠由游标说明 |
| 统计 | 明确目标及采样要求；慢消费者可丢弃旧样本，显示时间和丢样，不补造连续历史 |
| 容器终端 | node、Engine、container、参数数组、受支持的用户／工作目录、TTY 尺寸；绑定单个交互会话，断线不重放输入 |

终端打开需要当前 `container.exec`；Agent 对照 Engine 身份及容器原生标识执行，不能按重名容器替换目标。输入、resize 和关闭只能进入已授权会话；每次输入仍检查权限有效性。环境变量等敏感参数不进入普通审计。

终端输入序号只用于当前会话内检测重复或乱序，不支持跨会话自动重发按键、命令或粘贴内容。字节丢失无法恢复时关闭并说明中断；重新打开是新会话。关闭流不保证已启动容器内进程退出，也不表示停止容器。

退出、停用、撤权或恢复代次变化立即关闭对应受限订阅，恢复访问须重新认证、授权并新建订阅。旧订阅标识及游标不是访问凭据，重连不自动恢复终端。输出缓冲耗尽时使用明确的截断／gap／关闭原因，不能影响任务终态保存。

## 十一 备份 恢复与管理接口

创建管理备份返回 Operation，完成后取得可用 BackupManifest 和受限下载。生成中、失败与可用分开；只有数据库及其引用内容、必要密钥材料和清单一致验证后才可标为可用。下载需要系统备份权限，短期下载能力绑定本次会话及具体对象，不公开原始存储路径。

`recovery.inspect` 只检查上传备份的格式、版本、完整性及恢复缺口，不替换运行中的数据。真正恢复需要进入隔离的运维恢复模式，停止旧 Manager 的执行权并生成新 recovery，旧会话、注册材料和开始许可失效，Agent 重新核对。这不是普通在线配置修改，也不能自动重新派发备份中的任务。

恢复开始入口、本地恢复凭据、替换流程及失败重启步骤由运维文档给出；在该流程定稿前不开放普通 Web 的“一键覆盖并继续执行”接口。恢复完成的管理状态不等于应用数据或节点运行状态已恢复。

审计与核对记录保留操作者、目标、动作、结果及证据引用。删除配置、节点或账号时按数据生命周期保留必要历史，不把数据库级联删除当作资源清理；查询历史仍按当前权限过滤。

## 十二 关键交互示例

以下为完整的单条逻辑请求和对应返回，字段取值是示例。它们展示保存成功、重发、版本冲突和 Apply 受理；线上认证由会话上下文提供，传输头与 HTTP 路径未定。文件已通过第六节上传完成，样例 upload 引用只代表该用户在该目标的就绪内容。

### 保存后重发

用户编辑 `stack_web` 的 `rev_3`，Stack 版本为 7，提交一个 Compose 文件；环境数组为空表示本次没有额外输入，不让 Manager 自动补入自身环境。

```json
{
  "contract_version": "draft-1",
  "request_id": "rq_save_1",
  "method": "config.save",
  "request_scope_id": "scope_session_a",
  "request_key": "save_key_1",
  "target": {"node_id": "node_a", "stack_id": "stack_web"},
  "params": {
    "expected_version": "7",
    "base_revision_id": "rev_3",
    "files": [{"path": "compose.yaml", "upload_id": "upload_ready_1", "purpose": "compose"}],
    "deployment_input": {
      "compose_files": ["compose.yaml"],
      "project_directory": ".",
      "project_name": "web",
      "profiles": [],
      "environment_files": [],
      "interpolation_values": {}
    }
  }
}
```

```json
{
  "request_id": "rq_save_1",
  "outcome": "completed",
  "data": {"stack_id": "stack_web", "revision_id": "rev_4", "row_version": "8"}
}
```

如果返回丢失，保持原作用域、键和语义输入重发，可更换仅用于跟踪的 request_id；仍返回 rev_4 和版本 8，不再生成 rev_5。更换了文件内容却沿用 save_key_1 时拒绝为 REQUEST_KEY_REUSED。

### 另一名编辑者版本冲突

另一名有权编辑的用户用自己的有效作用域和新键，提交基于版本 7／rev_3 的保存。返回如下；完整对方内容不随错误公开。

```json
{
  "request_id": "rq_save_other",
  "outcome": "rejected",
  "error": {
    "code": "VERSION_CONFLICT",
    "message": "配置已有新版本，请读取后处理本地修改。",
    "recovery": "refresh",
    "details": {"current_version": "8", "current_revision_id": "rev_4"}
  }
}
```

### Apply 与结果未知

用户明确选择已保存的 rev_4 部署。binding 和 Engine 已核对，`remove_orphans=false` 表示不请求清理本次配置未定义的项目容器；选择 true 时需展示范围并检查相应资源删除权限。不含 Build 或来源清理选项。凭据数组为空表示仅使用不需受管秘密的依赖。

```json
{
  "contract_version": "draft-1",
  "request_id": "rq_apply_1",
  "method": "stack.apply",
  "request_scope_id": "scope_session_a",
  "request_key": "apply_key_1",
  "target": {"node_id": "node_a", "stack_id": "stack_web"},
  "params": {
    "revision_id": "rev_4",
    "expected_binding_id": "binding_web_a",
    "expected_engine_id": "engine_a",
    "options": {"remove_orphans": false},
    "credential_uses": []
  }
}
```

```json
{
  "request_id": "rq_apply_1",
  "outcome": "accepted",
  "data": {
    "operation_id": "op_apply_1",
    "status": "accepted",
    "target": {"node_id": "node_a", "stack_id": "stack_web"},
    "input_id": "input_apply_1"
  }
}
```

Manager 固定输入后发送 prepare；Agent 持久准备、请求开始；Manager 重查授权并持久签发许可，Agent 持久接受后执行。许可后失联时查询可返回 unknown，而非 failed。用户不能因页面超时重新创建同一部署；当前会话可按原键取得 op_apply_1，重连后以原 task_key 核对结果。

例如同一 Agent 的两个操作分别有 `(op_apply_1, stream_a, 1)` 和 `(op_apply_2, stream_b, 1)`，它们不是重复事件。op_apply_1 的 seq 3 先于 seq 2 到达时，不把连续确认推进到 3；终态独立持久确认也保留缺口。op_apply_1 的迟到成功不覆盖已成功的更高部署序号。

### Agent 结果与持久确认

下例假设同一任务的事件 1、2 已持久接收，Agent 补报事件 3。`digest.value` 是用于展示同值核对的样例，不是实际计算结果或摘要测试向量；实际格式在物理契约中定稿。当前连接上下文与原 task_key 分列，即使重连也不能改变后者。

```json
{
  "message_id": "msg_result_1",
  "type": "task.event",
  "context": {
    "installation_id": "installation_1",
    "recovery_id": "recovery_1",
    "node_id": "node_a",
    "identity_id": "agent_a",
    "connection_id": "connection_a_4",
    "connection_generation": "4"
  },
  "payload": {
    "task_key": {
      "installation_id": "installation_1",
      "operation_recovery_id": "recovery_1",
      "node_id": "node_a",
      "operation_id": "op_apply_1"
    },
    "input_digest": {
      "algorithm": "sha256",
      "format_version": "example-v1",
      "value": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    },
    "event_stream_id": "stream_a",
    "event_seq": "3",
    "occurred_at": "2026-11-02T03:00:00.000Z",
    "kind": "finished",
    "result": {
      "status": "succeeded",
      "evidence": {"action_result": "compose_exit", "exit_code": 0, "materialized_manifest_id": "manifest_1"},
      "known_effects": ["managed_files_updated", "compose_up_completed"],
      "error": null,
      "output_ref": "output_1"
    }
  }
}
```

Manager 确认输入、目标及结果依据并持久提交后，返回以下确认。只表示该次动作结果已记录；不表示业务健康、迁移验证通过或来源可删除。

```json
{
  "message_id": "msg_ack_1",
  "type": "task.result_ack",
  "context": {
    "installation_id": "installation_1",
    "recovery_id": "recovery_1",
    "node_id": "node_a",
    "identity_id": "agent_a",
    "connection_id": "connection_a_4",
    "connection_generation": "4"
  },
  "payload": {
    "task_key": {
      "installation_id": "installation_1",
      "operation_recovery_id": "recovery_1",
      "node_id": "node_a",
      "operation_id": "op_apply_1"
    },
    "event_stream_id": "stream_a",
    "result_event_seq": "3",
    "persisted_through_seq": "3",
    "recorded_status": "succeeded"
  }
}
```

## 十三 场景检查与后续定稿

| 场景 | 应满足的接口结果 | 需求 |
| --- | --- | --- |
| 初始化、登录、停用与最后管理员竞争 | 初始化不重复开放，失效会话拒绝请求，最后管理员约束原子执行 | R01、R11；Q03、Q07 |
| 内外网 Agent 注册及身份变化 | 主动接入，逐次认证，Engine 变化进入核对，不要求节点间直连 | R02；Q01、Q04 |
| 查看约 20 节点、筛选分类及部分离线 | 分页与过滤限于授权集合，离线保留时间，20 不作接入上限 | R03、R14；Q01、Q09 |
| 两人保存与响应丢失 | 版本冲突不覆盖，同键重发返回原版本，旧作用域不能悄悄变新提交 | R04；Q02、Q05 |
| 多文件、原文、外部路径与 Build 依赖 | 保留输入顺序，文件引用授权，节点预检不越权，构建明确拒绝 | R06；Q06、Q07 |
| Apply、删除配置与删除资源 | 动作范围及记录分开，保存不执行，数据删除单独选择 | R05、R10；Q01、Q02 |
| Node Cross 执行后验证，再选择来源处置 | 迁移、验证、选择与清理分开；旧验证及通用接口不能绕过保护 | R07、R11；Q03、Q10；完整切换契约待补全 |
| 手动准备或后续搬运数据 | 用户准备说明不冒充验证证据，配置复制不冒充完整迁移 | R15；Q10；搬运机制暂缓 |
| Copy 后 Apply，来源离线或后来删除 | 复制产生独立目标，Apply 不改变来源；权限在每阶段检查 | R17；Q02、Q03、Q04 |
| 容器、镜像、网络、卷操作与凭据 | 动作专用参数、目标身份、实时前置检查和秘密用途绑定 | R08、R09、R10；Q01、Q03 |
| 已有 Compose 项目接管 | 缺失配置可见，接管不自动执行部署，不隐式覆盖同名项目 | R12；Q02、Q06 |
| 撤权、日志背压、终端重连 | 关闭受限订阅，控制消息不被淹没，终端输入不重放 | R08、R11；Q03、Q04、Q07 |
| Agent 重启、许可在途、重复或乱序结果 | 固定任务去重，未知先核对，三元组事件去重，持久后确认 | R13；Q04、Q08 |
| 管理备份与恢复 | 备份完成需清单完整，恢复新代次不自动重做节点任务 | R13；Q08 |
| run 转 Compose 含秘密、缺失环境文件或不支持语法 | 只解析文本，输出差异与阻塞项，不执行或自动部署 | R16；Q07、Q11 |

进入实现前须补齐：传输与认证载体、物理路径和编码、完整动作参数 schema、数值限额与超时默认值、摘要编码及测试向量、Agent 兼容矩阵、恢复操作入口。选定后以机器可读契约维护字段和示例，并建立跨端校验，不能让正文和代码各自定义一套协议。

Node Cross 的切换、验证清单、Stack 身份和回退属于单独的实质设计缺口，不能由接口实现者自行猜定；存储搬运细节继续暂缓。其余建议按本文逻辑契约进入后续测试设计。本稿场景表是检查与验证目标，尚未执行网络、Docker、权限或故障注入测试。

结构参考 [NASA Interface Design Description](https://swehb.nasa.gov/spaces/SWEHBVD/pages/102695656/5.02%2B-%2BIDD%2B-%2BInterface%2BDesign%2BDescription)中的参与方、数据元素、交互、兼容与追溯内容；协议候选按本文引用的官方 RFC 查阅。查阅日期：2026-11-02。未套用 NASA 的项目管理流程。
