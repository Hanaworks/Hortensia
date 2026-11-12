# M1通信适配实施与验收

2026-11-12编码前保存；[模块规格](../specifications/m1-agentwire.md)为依据。先TLS握手与拒绝行为测试，再会话／方向／原文，再探活／关闭。保留既有channelprobe，不把此包替身消息当生产部署。

| 编号 | 通过条件与观察 | 初始状态 |
| --- | --- | --- |
| AW01 | 真实TLS使用明确CA可握手，错误CA／主机名拒绝；401与网络错误固定分类，无token；不重定向转发凭据 | 未验证 |
| AW02 | hello/welcome原文与关联一致；错误node/ledger由Store层拒绝，Peer仍核验协议/关联/session；Origin、首包错误拒绝 | 未验证 |
| AW03 | 双向支持集合、旧session、二进制、坏JSON／UTF8／超8MiB不交付；应用原文保持，不重复构造body | 未验证 |
| AW04 | 同ID同文可重报，异文拒绝；达到有界ID预算关闭，重连不得借此绕过业务幂等 | 未验证 |
| AW05 | 并发写串行完整、取消写可结束；调用方读循环持续接收，不等待执行器 | 未验证 |
| AW06 | 真实Pong刷新45秒期限，迟到Pong正确延长；4在途独立预算；没有虚假Pong更新 | 未验证 |
| AW07 | Close／父context取消使读写与探活退出，无关闭后后台业务写入；重复关闭幂等 | 未验证 |
| AW08 | 包测试／vet／构建与规格核对，接正式Manager/Agent另行验证；跨机环境明确未验 | 未验证 |

初始尚无实现及测试结果。时间缩短的测试只验证相同期限算法，不宣称网络容量或硬实时保证。

## 2026-11-12本机实现与复核

已实现`internal/agentwire`，当前仍为未提交v5工作树。AW01—AW07适用本机项经过真实TLS、双向原文、大小／方向／session／ID重用、串行写取消、真实Pong和Close验证；独立审阅发现握手ID未带入应用去重表，先由正式hello／welcome ID重用用例复现，再修复并独立复验。原`channelprobe`保持冻结，不以新模块替代旧证据。

最终本机`go test ./internal/agentwire -count=1 -json`通过52条测试／子测试，0失败／跳过，包耗时1.083秒；原始记录`.cache/m1-agentwire-final-tests.jsonl`。LastPongAt仅由真实Pong更新的当前实现包含在此回归。AW08的正式两端服务组合、跨机及真实Docker分别由MT／AS及首版环境验收承接；这些结果不是整项跨环境完成。
