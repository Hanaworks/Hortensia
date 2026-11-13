# 2026-11-13 InstaBox 调试证据归档

状态：历史执行归档，2026-11-13形成；保存已执行输入和原始结果以便追溯，不作为现行规格或支持版本清单。现行入口为[IB调试记录](../../docs/implementation/m1-instabox-debug.md)、[M1推进](../../docs/implementation/m1-first-version.md)及各模块规格；后续追加结果不改写原始失败或旧输入。

## Linux 第一轮

[源码包](linux-source.tar.gz)仅包含255份编译／测试输入，不含`.git`、本机凭据、运行数据库或node_modules；[逐文件清单](linux-source-manifest.json)保存当时HEAD4d46a60及未提交通用备份组的实际原字节。该组后提交为7362441，测试输入与提交时工作树相等；原清单不回填历史HEAD。[完整证据包](linux-first-evidence.tar.gz)保留Go JSON、stderr、环境及各步exit，[汇总](linux-first-summary.json)由完整日志计算。18测试包、1991条测试／子测试通过，2项Windows专用跳过；vet／build／mod verify退出0。

[实际执行脚本](run-linux-checks.sh)使用专用Linux目录；复现应在空白隔离VM中按脚本路径解包相同输入并核对清单。脚本和输入只是该批历史材料，不能取代后续源码或直接在已有业务目录运行。目录Sync正常不证明掉电耐久。

## 原生 Docker／Compose

[执行说明](native-compose/report.md)和[事前范围](native-compose/m1-instabox-native-compose-plan.md)解释NC01—NC12。两轮脚本、156条脱敏命令、结果与清理清点保留于本目录；`native-compose/verify-evidence.mjs`可用Node运行，核对本地文件与已取回远端摘要及自有资源清理证据。

**明确作废的子断言**：主轮results.json中NC05无image子夹具使用了YAML布尔command，失败原因是schema，不是no-build；原粗略PASS仅作为历史原文保留，不作为有效结论。有效替代为补充轮NC05b：合法config成功，再因缺本地构建镜像失败且未构建。主轮NC05的image+build正向部分另有有效证据，不能与作废子项混同。

这些是原生CLI与Engine对照，不是产品Prepare／Observe／正式Apply或两节点隔离验收；生产接受表仍为空。脚本使用固定路径和资源名，复现需新建空白专用环境，并保持只清理该脚本创建的对象。归档保留两轮输入、无效尝试、替代证据和清理结果，不自动更新成后续运行结果。

## Web 与正式 CLI

[执行报告](web-cli/report.md)、[119份白名单清单](web-cli/evidence-manifest.json)及[本机校验结果](web-cli/local-verification.json)说明Web39项、类型／构建及正式CLI／HTTPS／WS子集。[Web脚本](run-web-checks.sh)与三轮CLI脚本、最初基线及run-03前的契约核对均保存在`web-cli/received/`，未保存身份、密钥、口令、数据库或Cookie／token正文。

run-01 CA缺签发KeyUsage、run-02误将隐藏资源404预期为403，均为失败的测试夹具，原始退出1保留；两轮不作为产品故障。run-03修正测试材料／预期后退出0，保持严格TLS及原有权限规则，实际完成启动、登录、认领、空恢复、五文件两版保存／幂等、重启及关闭。原基线中的错误预期已由run-02／03勘误和契约核对明确替代，不能把旧403或会话撤销假设当现行API规则。

这不是浏览器点击或正向Docker部署；实际能力为空时422拒绝且零操作才是本轮预期。测试进程已关闭、锁可重新取得，两台专用VM均暂停，环境保留供后续调试。

受验输入与原始证据文件摘要另见本目录生成的`manifest.json`；它只校验列出的归档内容，不是产品备份manifest。本目录Git属性保留原字节，原始received材料按原始数据保存，不做换行或尾空行格式化，以免破坏已核对的SHA。虚拟机ID是测试证据来源，不是生产配置。
