# M1 v5完整备份组与离线校验

2026-11-13会话客户端日期，编码前基线。依据源码95d453f、文档e291a44及[维护／备份主定义](m1-maintenance-backup.md)。本文采用对当前源码调查后明确选择的实现方式，不将调查报告或参考资料自动视为用户需求。稳定BG验收见[实施记录](../implementation/m1-backup-group.md)。同日事后补充：通用CreateGroup/VerifyGroup已实施并完成本机适用验证，实际入口及证据见下文；两端服务、CLI与真实领域组合由负责人单独集成，不能把通用回调测试当成完整组端到端交付。

## 1. 目的与边界

提供schema5／protocol1、identity format1扁平布局的在线一致备份和离线verify。完整组包含数据库、完整身份和实际两钥的可认证关联；不包含整个运行环境，不是可普通serve的恢复部署。不实现restore／stage／rekey／READY／current、schema6、protocol2或维护动作，没有网络、Docker或应用数据备份。

源服务可继续写数据库。只能承诺一个SQLite一致时间点，不承诺跨机器同时快照或RPO/RTO。Snapshot用非immutable只读连接复制活动WAL，随后两端只读Inspector只处理封闭静态副本；不得借备份调用正常业务Open改变boot／session／冻结／claims／quota。读取SHM锁元数据、创建或打开保留材料锁属于同步行为，不声称源目录每个字节绝对不变。

稳定node／ledger／manager及摘要／账本／KEK身份和实际钥不由现行正常服务替换；Agent完整identity内候选／active secret和凭据代次可由认领／轮换变动，须受材料锁保护。现布局没有部署generation，清单固定source_layout=flat-v1、generation=null，backup_id不能冒充部署代次。
## 2. 文件布局

操作者提供两个不存在的绝对本地目录 `data-out`、`secrets-out`；其各自直接父目录必须已存在且通过 private-dir 检查。程序排他创建两根，0700；全部新文件 0600，不覆盖、不自动清理、不重用旧 partial。

```text
data-out/
  sqlite/state.db             # Snapshot 独占创建 sqlite/，不预先创建它
  config.json                 # 明确白名单的配置说明，非可直接启动配置
  manifest.json               # 最终完成标志；最后原子发布
secrets-out/
  identity.json               # 原始完整字节，不重新序列化
  keys/
    digest.key                # Manager，32 原始字节
    encryption.key            # Manager，32 原始字节
    ledger.key                # Agent，32 原始字节
    journal.key               # Agent，32 原始字节
  secret-manifest.json        # 比 data manifest 先发布
```

每端只允许自身两把钥，不同时出现四把。既有 DB 的 -wal/-shm/-journal 由 Snapshot 正确读取并验证路径，但不是输出文件；独立副本验证拒绝出现这三个旁文件。成功组中不保留临时清单。未知文件/目录、链接、重解析点、硬链接或固定路径以外条目均拒绝 verify，而非忽略。

## 3. 公共接口与内部协作

```go
// internal/backup；不导入 agent/service、manager/service 或两端 Store 包。
type Component string // "agent" | "manager"
type MaterialRole string // "identity" | "ledger" | "journal" | "digest" | "encryption"

type MaterialSource struct {
    Role MaterialRole
    Path string
}
type Source struct {
    Component Component
    DatabaseFile string
    MaterialsLockFile string
    Materials []MaterialSource // 必须恰好 identity + 该端两把钥
    ConfigJSON []byte          // service 生成的白名单说明
    Creator BuildInfo
    ExternalMaterials []ExternalMaterial
}
type GroupRequest struct {
    Source Source
    DataOut, SecretsOut string
}
type Material struct { Role MaterialRole; Raw []byte }
type MaterialSet struct { Files []Material; ConfigJSON []byte }
type BuildInfo struct {
    ApplicationVersion string
    BuildRevision *string
    BuildDirty *bool
}
type ExternalMaterial struct {
    Kind string
    Required bool
    Policy string
}

type IdentityRef struct {
    ManagerID string // Manager 必填，Agent 必须空
    NodeID, LedgerID string // Agent 必填，Manager 必须空
}
type KeyRef struct { Role MaterialRole; KeyID string }
type RowCount struct { Table string; Rows uint64 }
type DomainReport struct {
    Component Component
    Identity IdentityRef
    SchemaVersion, ProtocolVersion int
    SQLiteVersion string        // 本次检查工具的 SQLite runtime，不是源文件写入版本
    Keys []KeyRef               // 包含两把实际钥的 ID；角色排序
    ReferencedEncryptionKeyIDs []string // 所有密文实际引用的 key_id，排序、去重
    TableRows []RowCount        // 全部应用表计数，表名排序；零行显式存在
    SealedRows []RowCount       // 全部密文表计数，表名排序；零行显式存在
    SemanticFormat string      // 例如 dm-agent-runtime-semantic-v1
    SemanticFingerprint string      // 领域负责定义、完整复算的HMAC，不能用表行数冒充
}
type DomainInspector func(context.Context, string, MaterialSet) (DomainReport, error)

type GroupState string // "partial" | "publication_uncertain" | "complete"
type GroupResult struct {
    BackupID string
    DataManifest, SecretManifest string
    State GroupState
    Report DomainReport
}
func CreateGroup(context.Context, GroupRequest, DomainInspector) (GroupResult, error)
func VerifyGroup(ctx context.Context, dataManifestFile, secretManifestFile string,
    inspect DomainInspector) (GroupResult, error)
```

上述输入类型不是外部 HTTP DTO。Raw 只在调用栈内使用，结果/清单/普通日志永远不含 Raw。CreateGroup 自己读 MaterialSource，自己持有材料锁，不能把是否持锁完全托付给任意回调。材料捕获上限：identity 64 KiB、每把 key 恰好 32 字节；config 和每份清单各最大 256 KiB。组件纯解析仍可沿用其现行更小有效内容限制，捕获上限不放宽身份业务格式。回调只读取受控副本和捕获的材料，不可留存或修改原字节、不作网络/Engine/进程操作。

两端 service 新增 `Backup(ctx,c,dataOut,secretsOut)` 和 `VerifyBackup(ctx,manifest,secrets)` 薄入口：前者先复用配置验证，构造固定 Source，闭包调用纯材料解析与领域 InspectRuntimeSnapshot；后者从备份 config.json/identity 纯解析，不读取活部署配置，不申请源服务锁。引用方向为 `service -> backup`、`service -> ledger/operations`，不会出现 `backup -> service` 循环。领域检查 API 返回值由 service 适配为 DomainReport；其精确枚举/语义算法以所属模块主定义为准。

CLI 仅增加：

```text
manager|agent backup --config <abs> --data-out <new-abs-dir> --secrets-out <new-abs-dir>
manager|agent backup verify --manifest <abs-manifest.json> --secrets <abs-secret-manifest.json>
```

verify 不需要 `--config`，也不接受源服务参数；命令专属 flag 严格检查，未知/串用/多余位置参数拒绝。输出只含 backup_id、component、两份清单路径、计数、验证状态和固定错误分类；不输出 identity/配置原文/SQL 密文/secret。没有 restore/READY 提示。

## 4. 材料锁、源材料和身份一致性

锁路径为 Manager `filepath.Join(filepath.Dir(DatabaseFile),".materials.lock")`，Agent `filepath.Join(StateDir,".materials.lock")`。沿 localstate 操作系统排他文件锁；锁文件持久保留，绝不通过删除/替换锁文件解锁。路径检查和单硬链接限制与材料相同。旧部署首次运行本轮工具可能创建这个保留锁文件；不能要求重新 init 才能备份。

全局顺序：需要两把锁者始终 **运行锁 → 材料锁 → SQLite**；逆序释放。backup 只拿材料锁，从不尝试运行锁；领域验证只打开副本。普通业务数据库写者不拿材料锁。此图没有“backup 持材料锁再等待运行锁”的回路。

| 入口 | 材料锁持有范围与行为 |
| --- | --- |
| Manager/Agent init | 完成配置/口令等无副作用验证、检查/创建私有目录，取得原运行锁，再取得材料锁；锁内先做完整存在/别名冲突检查，再生成/写 identity/钥/DB；直到 Store.Close 与目录同步完成。任意失败仍按原 init 保留材料，先关 DB 再释放锁 |
| Agent enroll/rotate | 运行锁成功后、loadIdentity 之前取得材料锁；保留整个 authenticate/request/candidate 落盘/promote 和 Store.Close 期间的锁；响应丢失原候选保留规则不变。备份要么看到先前状态，要么看到此次退出时的完整状态，不会交错复制 |
| Agent Serve 启动 | 运行锁→材料锁→读取 identity/钥、检查源 DB/旁文件、OpenRuntime；Open 成功后立刻释放材料锁，继续持运行锁直到服务关闭。任何启动失败先关 Store/清钥，再释放两锁 |
| Manager Start | 运行锁→材料锁→loadIdentity/检查 DB 旁文件/OpenService 或 OpenRuntimeService；Open 成功后释放材料锁，不带进 Runtime 生命周期；backup 只接受 schema5，现行 schema4 启动兼容仍保持 |
| Backup | 取得材料锁后读完整 identity 与两钥，直到最后清单发布/同步和辅助重读结束才释放。全程不调用业务 Create/Open |
| Verify | 离线备份输入，无活部署锁，无网络，无写入 |

材料锁获取策略：backup 对 `ErrLocked` 以 50/100/200/400/500ms、随后 500ms 等待，受本次总 5 分钟期限和取消约束；等锁期间不创建输出根。其他既有入口使用 fail-fast 材料锁，忙时返回明确 `materials_busy` 而不是误称已有服务运行。这样不悄悄增加 init/启动/凭据命令的长时间挂起；发布 identity 的动作可以重试，但不能绕锁。

Agent 的 `openIdentity` 应返回独立的运行锁与材料锁资源，所有三个调用者（Serve、Enroll、RotateCredentials）必须显式决定释放点；不要在共同 helper 内提前释放后再让 enroll/rotate 无保护地 saveIdentity。`saveIdentity/promote` 为只允许已经持锁路径调用的私有 helper。两端 init 直接写材料的入口同样接线。未来任何 KEK/身份/部署切换都必须沿相同顺序；本轮不实现这些切换。

### 4.1 三层核对

1. **材料解析**：严格 JSON、未知/重复字段拒绝、格式1/schema5、身份 UUID/key ID 不同等原条件；Agent credential_state 的 candidate/active/rotation_pending 与 secret/generation 组合完整校验，Manager只接受当前两钥。Create 额外核对 identity.ManagerOrigin 与 supplied Config；verify 和备份 config.json 中记载的 origin 核对。
2. **DB 对材料**：只在副本上检查 Agent agent_meta 的 node_id/ledger_id/ledger_key_id 及 key_check、event_keys 的 journal key ID/key_check/uses；Manager manager_id、两 key ID/check/uses 和初始化完成标志。两端精确 v5 DDL和完整业务/全部密文验证由领域 Inspector 承担。不能只对比身份文件，也不能调用会换 boot 的普通 Open。
3. **发布前重读**：仍持材料锁，重新安全读源 identity/两钥，和开始捕获的完整原字节比较；不等或路径属性改变则 `materials_changed`、保持 partial。此检查辅助发现违反协作锁的外部修改，不宣称能抵御同用户恶意替换/ABA。

Agent 本地 DB 没有 Manager 当前凭据是否撤销的权威；本次只认证本地身份/钥与该 DB 匹配，不访问 Manager 来声称 credential_generation 在远端仍有效。离线 verify 同样不能证明当前授权或对端凭据状态；这些仍是未来隔离恢复的门槛。

### 4.2 路径与所有权

所有相关路径使用 BS 的绝对、本地、非 URI/UNC/NUL/query/fragment/Windows ADS 规则；拒绝所有现有组件的 symlink/reparse。源 DB/已有旁文件、identity、钥与 `.materials.lock` 要是单硬链接普通文件，先检查、打开后核对类型/身份、再读取；源 DB/材料的父目录及两输出的父目录要通过 private-dir。Linux owner 是当前有效 UID，目录0700、已有可读文件不得对 group/other 或 execute 开放；新写0600。Windows沿现有 reparse 检查，**mode 不是 ACL 证据**。Linux真机 mode/owner/rename/目录耐久仍须独立验证。

先计算规范路径集合再取得任何会创建文件的锁：DB、三旁文件、identity、两钥、原运行锁、新材料锁，不得别名。Windows规范路径比较必须不分大小写；已有文件须以句柄规范长路径认证，拒绝尾点/尾空格/8.3别名；硬链接/链接也单独拒绝。Agent config 原路径唯一检查新增材料锁；Manager的对应检查提取为 init/start/backup 共用，不能只在 init 写钥前才检查。任何钥或 DB 不可被配置成锁路径。

data-out 与 secrets-out 规范路径必须不同且互不包含，不得碰撞任一源保留文件；新目录不能已存在。允许在可信私有父目录下选址，不递归扫描源目录、不将 source state_dir/keys_dir 全部打包。现行 localstate.RejectLinks/CheckPrivateDir 不承诺抵御同一有效用户恶意竞争替换祖先；保留 BS 既有边界。

## 5. 清单字段、摘要方向与验证算法

所有 SHA-256 是32字节的无填充 base64url规范编码。清单采用固定 struct 编码、UTF-8、无BOM、末尾一个 LF；对实际落盘原字节求摘要，不对再次序列化的 JSON 求摘要。读端 strict JSON，拒绝重复/未知字段、null/省略不合规则、负计数、重复 role/key/table/path 和非规范摘要。

### 5.1 Data manifest：`format="dm-backup-v1"`

| 字段 | 定义 |
| --- | --- |
| backup_id/component | 新随机 UUID；agent或manager |
| source_layout/generation | 本轮固定 flat-v1/null，不能伪造部署代次 |
| identity | IdentityRef，端别字段组合严格 |
| schema_version/protocol_version | 固定5/1；不自动接受1—4或未来版本 |
| creator | `{application_version,build_revision,build_dirty}`；取本次备份工具实际 build info，不可取得时 revision/dirty 明确 null，不能冒充运行服务的源码版本 |
| sqlite_version | 备份工具使用的真实 SQLite runtime 版本；不等于历史写库版本 |
| started_at/completed_at | UTC RFC3339Nano；completed_at 为内容与领域验证结束、写最终清单之前；耗时另由单调计时输出，不推断RPO/RTO |
| database | `{path:"sqlite/state.db",bytes,sha256}`，来自关闭后的副本 |
| files | 仅 `{path:"config.json",bytes,sha256}`；固定允许列表 |
| identity_sha256 | 完整秘密 identity 原字节摘要；不包含正文 |
| keys/referenced_encryption_key_ids | DomainReport 的角色+key_id以及所有密文实际引用的KEK ID集合 |
| inspection | `{semantic_format,semantic_fingerprint,table_rows,sealed_rows}`；来自完整副本验证，两组计数均含零行 |
| secret_manifest | `{bytes,sha256}`，secret-manifest完整最终原字节；不写操作者秘密目录的绝对路径 |
| external_materials | 下面定义的明确未捕获材料/再供应要求 |

### 5.2 Secret manifest：`format="dm-backup-secrets-v1"`

字段固定为 `backup_id,component,source_layout,generation,identity,database_sha256,database_bytes,identity_sha256,files`。files 中 identity项为 `{role:"identity",key_id:null,path:"identity.json",bytes,sha256}`；两钥项包含各自 role、key_id、固定 `keys/<role>.key`、bytes=32和摘要。identity摘要必须同时等于本清单identity项和data清单identity_sha256。

无环关系：

```text
DB bytes ──hash──> data manifest.database
     └────hash──> secret manifest.database_sha256
identity/key bytes ──hash──> secret manifest.files
identity bytes ──hash──> both manifests.identity_sha256
secret manifest final bytes ──hash──> data manifest.secret_manifest
```

Secret manifest绝不引用data-manifest摘要。两个清单的 backup_id/component/身份/layout/generation/DB大小与摘要必须相等；钥 role/ID还须和身份及DB共同核对。SHA只检测损坏/错组，不认证恶意重写全部材料的攻击者。

### 5.3 配置说明与外部材料

config.json 是独立白名单说明，必填 `format="dm-backup-config-v1"`、component 和布尔 `supplied_config=true`，再包含下列组件对应的非秘密运行参数；不复制未知字段、配置原始文件或任意目录。

* Manager：listen、external_origin、log_level。TLS cert/key、WebDir、源DB/identity/key绝对路径不落普通备份；用 external_materials 槽位说明需要再供应的服务TLS证书/私钥及匹配应用构建的Web资产。TLS材料策略为 `provision_current`，不能暗示旧私钥被自动装回公开入口。
* Agent：manager_origin、docker_socket、execution_timeout_seconds、log_level、trust_roots_mode（system/custom）。external_materials 列当前受支持 Docker/Compose CLI、Engine端点、系统/自定义信任根；不读取私钥或浏览器/机器环境中的秘密。
* 两端明确排除镜像层、Docker data-root、卷/bind数据、外部数据库。Agent额外列出 workspaces/operations 文件树：本组检查DB冻结payload原字节，但没有备份宿主机工作区原文件及实际挂载依赖；未来恢复必须保留/核对这些材料，不能据DB组宣称全部运行文件得到保护。

`ExternalMaterial` 固定 `{kind,required,policy}`；kind/policy只使用下表组合，无任意说明里混入secret或源绝对路径。`required` 是未来恢复需要明确处理的依赖元数据：**Create/Verify 不检查这些外部路径、网络或 Docker 是否存在，不新增备份放行或审批关口**。Agent 工作区/操作文件的保留、重建和核对依据实际历史；空库或未部署节点可以没有这些文件，不要求操作者补造本不存在的材料。

| 适用组件 | kind | required | policy |
| --- | --- | --- | --- |
| Manager | tls_server_certificate | true | provision_current |
| Manager | tls_server_private_key | true | provision_current |
| Manager | web_assets | true | matching_application_build |
| Agent | docker_compose_cli | true | provision_supported_version |
| Agent | engine_endpoint | true | revalidate_endpoint |
| Agent | tls_trust_roots | true | provision_current |
| Agent | agent_workspaces | true | preserve_and_reconcile |
| Agent | agent_operations | true | preserve_and_reconcile |
| 两端 | docker_images | false | excluded |
| 两端 | docker_data_root | false | excluded |
| 两端 | application_volumes_and_binds | false | excluded |
| 两端 | external_databases | false | excluded |

两端适配器提供其完整适用集合，kind 不重复，清单按 kind 排序；通用层只验证元数据组合及集合，不查询外部对象。config.json 不是 serve 可直接读入的Config。备份所用 supplied config 也不宣称等于运行进程此刻持有的配置；DB与材料身份才是组绑定依据。

### 5.4 Verify固定顺序

1. 检查两份显式清单路径及父目录，限制大小、拒绝链接/硬链接；读data严格解析并确认 v1/flat-v1/null/schema5/protocol1。
2. 只按固定白名单检查data组DB/config的类型、大小、SHA；独立DB的三个旁文件不得存在。拒绝未知条目，不因清单给了路径就允许遍历。
3. 读取显式secret-manifest路径的原字节，先比对data记载的大小/SHA，再严格解析，核对全部跨组字段和DB/identity摘要。
4. 核对每个secret固定文件的字节、大小、摘要；严格解析identity并校验凭据状态，确认两钥32字节与角色/key ID。
5. 只对独立副本执行 DomainInspector，按[共享只读检查主定义](m1-maintenance-backup.md#73-v5完整备份的只读检查切片)，`mode=ro&immutable=1` 仅用于确认没有旁文件且已封闭的完整副本；源 Snapshot 仍禁止 immutable。整个 verify 不创建SQLite旁文件、不改backup文件。精确schema、FK、integrity、全部密文、冻结包、链、claims、归属/释放依赖本步完成。
6. 逐字段比较重算的component/identity/schema/protocol/key集合/TableRows全部表计数/SealedRows密文表计数/semantic_format和semantic_fingerprint；sqlite_version是创建者来源信息，不要求未来验证器的SQLite runtime必须相同。返回本次验证工具版本另作结果元数据。回调前后都核对清单绑定副本的文件身份、SHA-256及无旁文件；只信任私有静态目录，不承诺抵御同用户恶意ABA。

verify不能将无最终data manifest的secret-only/partial组升级成完成组；不能补文件、修密文、改钥、写READY或启动服务。

## 6. Create完整流程、状态和失败

CreateGroup 建立总5分钟有界context，覆盖材料锁等待、Snapshot、检查与发布；Snapshot仍有自己5分钟上限，父期限取更早者。同步Step/Finish/Sync及内核调用不能被Go强行打断，期限在调用之间/行边界检查。验证在密文行/包之间检查context，不把一个全库循环做成不可取消区。

1. ctx/input/端别/路径和输出不存在等检查；构造backup_id。先验证源路径及候选材料，取得材料锁后重新检查，再捕获完整材料原字节。未有输出时的错误不称输出partial。
2. 排他创建data-out、secrets-out和secrets/keys；一个根成功、另一个失败也保留已创建根，返回partial及已知路径，不覆盖冲突目录。sync新根及其父目录。
3. Snapshot(sourceDB,data/sqlite/state.db)；任何错误保留partial且不产生最终清单。Snapshot拥有新sqlite目录，外层不能先Mkdir它。
4. 独占写配置说明、identity、两钥，逐文件Sync/Close；在Inspect前后核对封闭副本的文件身份、SHA-256及无旁文件，Inspect对该副本和捕获材料做完整认证。拿到全部DomainReport后才生成清单；重读源材料，确认没变。
5. 对关闭的副本和普通/秘密文件求实际摘要；再次核对输出固定目录树。生成secret-manifest原字节，写同目录唯一临时文件，Sync/Close；原子不替换rename到secret-manifest.json，Sync秘密目录及keys目录。
6. 生成引用secret-manifest最终原字节的data-manifest原字节，写唯一临时文件、Sync/Close；**最后一次检查ctx之后**，原子不替换rename到manifest.json。此后进入提交收尾，不因新取消再把已提交结果伪装成未发布；完成data目录Sync，关闭材料锁，清理内存秘密buffer，返回。

原子发布须使用平台 no-replace rename；不要复用会覆盖目标的 localstate.Replace。本轮 Linux `renameat2(RENAME_NOREPLACE)`、Windows `MoveFileEx`不设REPLACE_EXISTING，同目录且只对程序新建目录中的文件；不支持所需语义就失败保留partial，不退回覆盖rename。清单临时文件随机名/O_EXCL/0600；只移动本次名字，不删除既有对象。

| 位置/状态 | 返回语义与重试 |
| --- | --- |
| 预取消/输入/源材料错误/等锁取消，尚无输出根 | 无输出，固定错误分类；可能已有/新建保留锁文件；修复后以新backup_id重试 |
| 创建任何输出后，最终data rename之前错误 | `partial`；可能只有secret-manifest；不恢复、不verify为完整、不覆写上份；以全新两个目录重试 |
| 最终data rename失败 | partial；旧目标若意外出现保持原字节，不覆盖 |
| 最终data rename成功，data目录Sync失败 | `publication_uncertain`，返回专门ErrPublicationUncertain及路径；完整verify能核对当前字节，却不能追认此前目录耐久。保留所有文件，重做新组；不尝试删除完成标志来制造“肯定partial” |
| 最终目录Sync成功，仅材料锁Close/内存收尾错误 | 结果State=complete并返回ErrCleanup；备份内容完成与源同步资源清理失败分别报告，不能说本组不完整 |
| 完成 | complete；只有此时Create返回正常成功 |

错误类型沿Snapshot既有ErrInput/ErrDestination/ErrPartial并增加 `ErrMaterialsBusy,ErrMaterialsChanged,ErrVerification,ErrPublicationUncertain,ErrCleanup`。Context错误可errors.Is查询；领域错schema/identity/key/cipher/semantic使用受控细分code，不把明文或SQL行塞进error。调用方必须同时看GroupResult.State和error，CLI对uncertain/cleanup都非零退出但分别报告状态。

2026-11-13独立复核后、P2修复前明确BG09/12：最终data清单发布前，Inspector因取消/期限返回固定领域错误时，即使该错误本身未包装context错误，Create也必须优先保留实际ctx.Err，供errors.Is查询，和Verify同一错误分支一致。部分组仍保留、最终清单不得出现；这不改变最终data rename成功之后完成提交收尾的规则，也不改回调API或领域错误定义。

5分钟是本轮操作上限和保守容量边界，不是完成SLO。超限大组保持partial；如实际容量要求更大，再先修改上限/验收基线。没有自动保留删除、上传、加密压缩包或RPO/RTO保证。

## 7. 接口细化及维护规则

MaterialSet.ConfigJSON为复制的配置说明原字节，由generic Create／Verify传入同一Inspector回调。两端service适配器严格解析原identity及两钥，Agent核对身份manager_origin与描述一致，再调用所属包InspectRuntimeSnapshot。generic backup不导入service或领域Store，不自行推断业务身份／密文语义。捕获的所有Raw由本次调用持有，在落盘／检查／摘要后清理；回调不得把秘密留在普通报告或异步持有。

DomainReport.SemanticFingerprint为主定义规定的规范base64url keyed HMAC，SemanticFormat使用dm-agent-runtime-semantic-v1／dm-manager-runtime-semantic-v1；它不是未加钥的SHA摘要。TableRows补足全部17／29应用表；inspection对象包含table_rows、sealed_rows、semantic_format、semantic_fingerprint。原方案中的semantic_sha256名称不采用。两端Inspector报告已有独立主定义，此处只负责转换为排序数组并逐字段核对，不复制AAD或更改其含义。当前唯一KEK的实际引用集合在sealed计数为0时为空，否则只能包含对应EncryptionKeyID；两把实际钥的角色／ID始终保留。

所有清单、config.json最多256KiB；identity捕获最多64KiB，两份钥必须各32字节。JSON严格UTF-8／无重复及未知字段；数字按声明整数类型处理，时间UTC RFC3339Nano，不以墙钟先后推导执行结局。IdentityRef的无关端别字段为空字符串；当前generation以及不可取得的creator revision／dirty为显式null。其余必填字段缺失或null拒绝。SHA-256固定32字节规范无填充base64url；程序落盘JSON用固定struct及末尾LF，摘要核对实际原字节。

BuildInfo字段为application_version、可空build_revision、可空build_dirty，来自本次工具，不是假称数据库历史写入版本。SQLiteVersion来自真实工具所用驱动。ConfigJSON的组件及白名单字段必须正确；Manager只保存listen／external_origin／log_level，Agent保存manager_origin／docker_socket／execution_timeout_seconds／log_level／trust_roots_mode，均加format与supplied_config标志；它不直接作为服务Config加载。ExternalMaterial只使用上述明确枚举与策略，不能混入任意源路径或秘密。Manager service还须在创建输出前拒绝两个输出落在web_dir内；generic包不知道部署Web根，不能猜测。

已存在路径比较须使用实际文件身份及本平台规范路径；非现存目标使用已认证私有父目录和预定叶名。两输出不得相同或互相嵌套，包括Windows大小写和目录别名；不能通过重命名覆盖、清理partial或删除锁文件规避。受控文件操作测试适配器只在包内验证I/O故障，不作为生产配置或用户放行选项。

2026-11-13编码中、修复前补充BG04/07/08：源材料前后核对同时保存打开文件的身份及摘要，同路径同字节但文件实体被替换也应返回materials_changed；不能只比较再次读取的字节。通用报告结构还须保证每个SealedRows表都存在于TableRows，其密文行数不超过该表总行数；精确领域表集合与AAD仍由领域Inspector认证，不在通用层另建一份业务表定义。

修改密文表／身份字段时同时更新领域Inspector及清单／计数映射；修改材料发布或启动必须保持运行锁→材料锁→SQLite顺序；修改清单格式／摘要framing须显式版本化，不能让普通重构静默破坏旧备份验证。备份成功不能证明当前远端凭据未撤销、现在的用户授权或应用卷可恢复；这些是后续隔离恢复必须另行处理的事实。

## 8. 通用层实际入口与维护定位（实施后补充）

`internal/backup/group_types.go`保存公共契约和受控错误；`group.go:CreateGroup/createGroup`按§6组织本次context、真实材料锁及输出生命周期，私有groupOperations只作为同包受控I/O故障测试边界，每次调用独立，不是生产配置或用户绕过接口。锁在全部发布和关闭路径释放，source材料只捕获本组件三份固定文件。

`group_manifest.go`负责固定清单DTO、配置/外部元数据白名单、字段存在/null/重复检查及报告归一化。元数据数字保留json.Number后按声明uint64/int64解码；不能直接用共享protocol.StrictJSON的JavaScript安全整数上限限制完整计数。该模块验证通用报告结构，两端精确17/29表及密文AAD仍只由领域Inspector认证。

`group_files.go`负责私有普通单链接文件读取、流式hash/身份、固定目录树、源/目标保留路径及锁等待；平台group_files_linux/windows.go负责句柄/硬链接/规范长路径和不替换rename。原有Snapshot及其平台helper保持原样；在线源没有切换到immutable。`group_verify.go:VerifyGroup`只从显式清单固定目录树捕获材料，保持副本前后hash/身份不变，完整比较两组计数和语义报告。

Create在没有创建任何输出根时，GroupResult.State为空；返回的backup_id/计划清单路径只标识本次尝试，不证明文件存在。partial不返回领域成功报告；最终data清单出现后uncertain/complete携带完整报告，结合error区分持久化和清理状态。Verify失败返回零报告，成功只证明当前组字节和领域自洽性，不能追认过去的目录耐久或对端授权事实。

排障从受控错误分类→材料锁→源材料和输出命名空间→Snapshot→副本Inspector→源实体重读→secret清单→最终data发布/Sync→锁清理依次定位；失败资料保留，不能删除锁、覆写partial、替换钥或补空DB绕过。维护验证入口为`internal/backup/group*_test.go`及既有BS测试；真实领域、服务锁和CLI证据仍要由对应集成验收承接。

同日P2修复后补充：Create中Inspector返回非nil错误时，先检查本次ctx.Err，再处理普通领域拒绝；现在与Verify一致。正式`TestCreateGroupInspectorErrorPreservesCancellation/Deadline`覆盖“纯读取后取消/超时，再返回不包装context的固定领域错误”，断言可查询的Context分类、ErrPartial、partial状态及无最终清单；成功报告后取消及其他发布状态的已有断言保留。
