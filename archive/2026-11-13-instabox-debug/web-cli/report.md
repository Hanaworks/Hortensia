# InstaBox A：Web与正式CLI/HTTPS/WS验证报告

2026-11-13。本子任务已完成，远端与本机证据目录均已停写；不再操作A。Root可以统一暂停A。本报告仅对应本任务，不能据此宣布完整M1交付或Docker版本接受。

## 范围与输入

- 唯一操作VM：`dm-m1-a-20261113`／`m_agq5jab6pmtal66qwxntxjkvyq`。未操作B、原有VM、套餐或VM删除；没有停止全VM Docker/daemon，没有发布端口。
- 冻结源码在 `/data/docker-manager-debug/source`，清单 `/data/docker-manager-debug/source-manifest.json`。HEAD `4d46a60e38ff3092fe1fd5b5dcac55a35fb24303`，清单包含当时未提交backup-group切片；证据对应这255文件的实际字节，不单独把HEAD当成完整输入。最终逐文件重算大小/SHA256，255文件全部未变。
- 本任务只写本机 `.cache/m1-instabox-web-cli/**`、本报告及A专用测试/构建产物；不改生产源码、规格、正式docs、Git。没有使用fixture开关、能力绕过或修改接受表。
- 已先读README、Manager/Agent首次运行、两端服务规格；事前WC01—WC08基线先保存在本机与远端。run-03前又逐项复核完整HTTP API及实际公开契约并保存 `run-03-contract-audit.md`，未修改生产规格适配测试。

## 验收结果

| 项 | 结果及实际观察 |
| --- | --- |
| WC01 Linux Web | 通过。Node24.15.0/npm11.12.1；原有 `run-web-checks.sh` 只执行一次，npm ci/test/typecheck/build及overall均退出0。Vitest 3文件39测试通过（17+3+19），vue-tsc通过，Vite生成index/CSS/JS；npm ci报告0 vulnerabilities。UTC10:52:55—10:53:02。 |
| WC02 正式Manager与TLS | 通过。冻结源码 `go build ./cmd/manager`、`./cmd/agent`生成实际Linux可执行程序；正式manager init通过受控stdin读取私有随机测试口令，serve仅监听127.0.0.1。通过Python3.13严格CA验证取得构建后的index200及2个引用静态资源；系统默认不信任测试CA时证书校验明确拒绝。没有降低TLS验证或修改系统信任。 |
| WC03 真实HTTP身份/权限 | 通过。真实HTTPS登录200，Secure/HttpOnly __Host会话Cookie，GET me重读同CSRF；真实节点创建201。无node.read时配置保存404/not_found；明确赋予6项节点能力后可保存，Grant变更后原会话仍有效。使用实际一次展示token经stdin认领，未打印token/请求体。 |
| WC04 正式Agent | 通过。实际agent init/enroll/serve，身份active、代次1，使用自己的私有账本/密钥及真实/usr/bin/docker和本地Unix socket。Agent账本只读runtime_profile显示Docker29.8.2、Compose5.6.0、Engine已绑定且endpoint与HTTP一致；生产接受表仍空，实际agent_capabilities为空数组。 |
| WC05 正式WS空恢复 | 通过。正式两端通过真实TLS WebSocket通信；HTTP节点online；当前Manager持久active_round为completed、frozen=0、total=0、seen=0、pages=1，ledger匹配Agent init身份。停机重开后再次得到相同空恢复结论。 |
| WC06 多文件保存/Apply拒绝 | 通过。真实HTTP保存5文件（compose.yaml、第二Compose、插值env、应用env、附属config）、两版；原UTF8/CRLF字节及Compose顺序保留，首版不可变，同key重放返回相同创建身份。在线且已解冻时Apply返回422/unsupported，HTTP列表及只读DB计数均前后operation=0；正式重启后两版本/会话保留，重复拒绝仍0操作。 |
| WC07 退出与运行锁 | 通过。run-03两轮Manager/Agent均由本脚本Popen持有明确PID，只向自己创建的进程发SIGTERM，均正常退出0；运行中排他锁不可重取，退出后双锁可重取。最终额外检查run-01/02/03双锁均可重取、所有本任务回环监听关闭、自有CLI/监督进程0；秘密文件0600、私有叶目录0700。无ownedserver残留，无Docker daemon操作。 |
| WC08 证据层级 | 已明确。39项是Web组件/模型自动化；本次正式CLI、真实HTTPS、真实WS、真实SQLite持久状态属于实际Linux进程验证。未点击浏览器，未完成浏览器端到端操作，也未做跨VM业务部署、Compose成功部署或版本接受验证。 |

run-03 UTC11:02:23—11:02:30，18条全部成功断言，`overall.exit=0`、`status=finished`；各正式CLI init/enroll/build子命令均0，服务stderr为空。

实际对象（仅身份，无秘密）：node `9855d648-51b4-4945-b26d-ab1b8506ca33`；ledger `d9891c4e-ad9f-4399-8929-4eb412cffba2`；stack `91ad8d4d-6006-453a-95cc-a787d5d68ad4`。

Manager构建SHA256：`0b1244b1e090dbcdcb484073c4bccfd2d49c6009671465a57ea33cdb134cef3c`；Agent：`b2a38ab66ad55cfe26933188c3dea85d1a127c855816d98d4705510c6612e963`。

## 原始失败及夹具勘误

1. **run-01失败，保留原记录。** 私有测试CA采用OpenSSL默认扩展，没有keyUsage；A Python3.13.5严格验证返回 `SSLCertVerificationError`、verify_code92、`CA cert does not include key usage extension`。默认客户端和显式无代理直连相同，环境无代理。正式Manager init/serve均已成功，诊断重开与首次关闭均正常0。修正仅在下一独立run中新生成测试CA时加入critical `CA:TRUE`和`keyCertSign,cRLSign`；没有降低TLS校验。
2. **run-02失败，保留原记录。** 已通过正式页面、信任边界、登录和节点创建；额外负例错误地期待无node.read配置写返回403。当前HTTP规格/requireCaps规定不可见对象404/not_found，实际服务符合规格。修正测试断言，不是产品故障。原“Grant变更需重登录”的脚本注释也不准确，run-03改为直接证明原会话仍有效。
3. **run-03前保存完整契约审计后执行通过。** 审计覆盖剩余每条HTTP路径、权限、ETag、状态、原文保存、幂等、Apply前置条件/空能力、持久读取和退出；所有修订均为测试夹具/断言勘误。三次输入源码哈希最终仍完全一致。

原失败不计作产品缺陷或验证通过；run-03通过不抹去前两次夹具错误。没有把测试夹具错误写入产品规格。

## 只读观察与秘密边界

正式CLI没有暴露其内部Store。公开 `ReadRuntimeConnectionState` 需要正在运行的Store；调用另一个 `OpenRuntimeService` 会生成boot并撤销原writer，因此本任务没有这样做。通过实际HTTP online和Python SQLite `mode=ro`加`PRAGMA query_only=ON`只读取当前恢复标量、身份、版本/操作计数佐证；不写SQL、不改会话或恢复屏障。Agent版本证据同样只读runtime_profile。HTTP ExecutionProfile本身只含endpoint/revision，不把它误报成Docker版本信息。

口令、CA/服务私钥、Manager/Agent密钥、Agent身份中的长期秘密和运行数据库全部留在A专用私有目录；未下载、打印或入库版本化。本脚本不记录HTTP请求/响应体或headers，只记录断言、身份、状态与计数。下载采用明确白名单，仅Web完整日志、无秘密烟测日志/脚本/基线及最终摘要。

远端对下载前118文件、77,096字节作46个已知秘密表征扫描（生成的测试口令、Agent active/candidate秘密、私钥raw/base64/hex），0命中；另扫私钥PEM头/实际session赋值，0命中。Cookie及认领token只在进程内存或标准输入，从未写普通日志；扫描结论不替代这项脚本边界审阅。最终新增的扫描摘要自身也只含计数。

最终下载119文件、77,630字节；每文件SHA256/大小在下载时及本机独立复算各核对一次，全通过。远端读取无truncated/timedOut，完整证据不是尾部节选。

## 交接目录与停写状态

本机（Root可筛选归档；这里没有秘密运行材料）：

- `.cache/m1-instabox-web-cli-report.md`：本报告。
- `.cache/m1-instabox-web-cli/baseline.md`、`run-02-baseline-amendment.md`、`run-03-baseline-amendment.md`、`run-03-contract-audit.md`：事前依据和真实修订。
- `.cache/m1-instabox-web-cli/received/evidence/web-first/`：完整Web环境、stdout/stderr、逐项exit、时间/status。
- `.cache/m1-instabox-web-cli/received/smoke/run-01/evidence/`、`run-02/evidence/`、`run-03/evidence/`：三次正式CLI构建/服务日志/断言/exit及时间。
- `.cache/m1-instabox-web-cli/received/smoke/`：实际执行的三份脚本/编排输出/远端基线，以及 `final-verification.json`、`secret-boundary.json`。
- `.cache/m1-instabox-web-cli/evidence-manifest.json`：119文件相对路径、字节数、SHA256；`local-verification.json`：本机再次核验结论。
- `.cache/m1-instabox-web-cli/smoke.py`：最终run-03脚本；`diagnose-tls.py`与`run-01-tls-diagnosis.json`：CA问题窄诊断；`final-verify.py`、`check-secret-boundary.py`、`save-evidence.mjs`：最终检查/白名单提取工具。

远端安全证据：`/data/docker-manager-debug/evidence/web-first/`、`/data/docker-manager-debug/smoke/run-{01,02,03}/evidence/`及 `/smoke/` 下上述脚本/基线/摘要。**不要归档整个 `/smoke/run-*`**：其中仍保留测试身份、数据库、口令与私钥，仅`evidence/`属于此次证据白名单。

本机和远端均已停写。A最后读取完成，已通知Root可以暂停；本子任务不再操作VM。未提交Git、未改正式文档，后续由Root统一接收筛选证据与更新现行文档。真实Docker接受表、跨VM业务链、浏览器实际点击和整个M1交付继续按各自验收推进。
