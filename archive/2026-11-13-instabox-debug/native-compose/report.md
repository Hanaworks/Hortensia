# B机真实原生Docker/Compose对照结果

2026-11-13。事前基线见[m1-instabox-native-compose-plan.md](m1-instabox-native-compose-plan.md)，补充基线先于定向修正脚本保存。执行者仅操作B机`m_agq5jadtubvgelytmdyzbzv2q4`（dm-m1-b-20261113），未使用A、旧VM或正式业务数据，未修改任何正式Go/SQL/contracts/版本接受表。

结论：本轮完成NC01—NC12列明的原生语义子集，有真实config JSON、up和Engine资源证据。没有发现可据本轮独立判定的产品代码差异；因为没有调用产品Engine/Agent/Manager，不能把这些结果写成产品集成通过或生产精确版本接受。生产接受表继续为空。NC05第一次无image子夹具无效，已明确排除，并以NC05b合法夹具定向替代。

## 环境、输入与证据

| 项 | 实际记录 |
| --- | --- |
| 系统 | Debian GNU/Linux13.7 trixie；x86_64；kernel6.18.60-instabox；overlayfs |
| 账号与端点 | uid1000(user)，docker组；`/usr/bin/docker --host unix:///var/run/docker.sock` |
| Engine | ID `db7656a5-e664-4e16-9b49-de8ff401cfce`；Client/Server29.8.2；API1.56；containerd2.3.6/runc1.5.1 |
| Compose | `docker compose version --short`实际输出`5.6.0`；完整显示v5.6.0 |
| 官方轻量镜像 | 拉取busybox:1.37.0后，所有配置与run均固定`busybox@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec`；inspect返回Image ID `sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec`，amd64 |
| Instabox客户端 | 固定本地官方CLI0.8.268+c4f5b3e3a；未升级套餐或客户端 |
| 范围 | `/data/docker-manager-native/run-20261113-nc1`及`run-20261113-nc2`；资源前缀`dmnc_b_20261113a_`及`dmnc_b_20261113b_` |
| 运行时间 | 主轮2026-11-13约10:50:07—10:50:32 UTC；定向补充10:52:53—10:52:55 UTC；最终独立清点10:54:08 UTC |

本地证据均在`m1-instabox-native-compose/`：

- `run-native.py`、`run-addendum.py`是实际上传执行脚本；`commands.jsonl`121条与`addendum-commands.jsonl`35条保留完整argv、cwd、显式环境键、脱敏stdout/stderr、退出状态和时点。实际环境值由已保存脚本和夹具定义；随机假secret在日志/报告中统一遮盖。
- `results.json`及`addendum-results.json`逐项保存断言结果、文件SHA、规范模型与资源事实。主轮结果文件原封保留，包括原脚本对无效无image夹具的粗略PASS；审阅后的有效结论以本报告的排除说明和补充结果为准。
- `final-inventory-instabox.json`保留Instabox原始只读复核回执，`final-inventory.json`为其stdout解析；`evidence-verification.json`证明两脚本/两日志/两结果六份本地SHA与远端一致。`verify-evidence.mjs`可重复执行，已验证156条命令输出无未遮盖的假token前缀、所有记录项结束且资源为空。
- 两轮远端夹具和日志保留供复核，未清除证据。主轮输入在NC02/03前后断言SHA相同；NC08为了检验替换/orphan而有意修改配置，原初始文本由上传脚本保存、原SHA保留在results中。所有秘密仅为本任务随机假值，无真实凭据进入脚本或证据。

## 逐项判定

| 编号 | 判定与实际证据 | 关联范围 |
| --- | --- | --- |
| NC01 | 通过：实际记录版本/Engine身份/端点/账号，pull后固定digest和ID | 运行组合事实，不是生产验证清单 |
| NC02 | 通过：主文件+子目录override按顺序使MERGE_KEY=override；反序=base。有序env-file后者胜，显式map胜env-file；显式empty.env使默认.env无效，显式选.env才用其值；无profile时dormant不在模型，hidden启用后进入模型。两个相对bind均以显式project-directory为根，原字节SHA不变 | CE01/05、T04/T17原生对照子集 |
| NC03 | 通过：与config同上下文的`up --detach --no-build`创建app/dormant；模型断言的8个env值在实际容器中匹配；包内只读文件可读取，实际bind的RW=false；仅1个原生default网络+1个配置data卷，镜像ID固定，输入SHA不变 | 无额外注入的原生基线；未证明产品无注入 |
| NC04 | 通过：container/network完整64hex ID；Compose project/service/container-number、network/volume逻辑label可读；卷name/local/CreatedAt可组成弱指纹。无健康检查时inspect State不含Health；停止容器state=exited且`container ls --all`仍包含；随后恢复运行 | CE08原生资源输出子集；StackID/null产品行为未执行 |
| NC05 | 正向通过：image+build原文保留，固定镜像被使用，构建marker镜像前后均不存在。首次无image子项因`command:[true]`被schema拒绝，此子项无效，不算no-build证据 | CE05及F02正向子集 |
| NC05b | 修正后通过：合法`command:['true']`、本地build context先config成功且模型无image；随后`up --detach --no-build`exit1，明确`No such image: dmnc_b_20261113b_noimage-app:latest`；无marker镜像/容器，但创建了default网络 | 真正缺本地构建镜像、不构建及非零部分副作用 |
| NC06 | 通过：config成功不代表external存在；缺外部网络up exit1，未创建网络/卷；显式创建后成功。external网络和卷无Compose项目label，按项目label查询均不归入managed | CE09外部依赖子集 |
| NC06b | 通过：仅先建外部网络时up明确`external volume ... not found`exit1且卷仍缺失；创建外部卷后正常up，managed查询不包含二者 | 单独证明卷缺失路径；不从网络错误外推 |
| NC07 | 通过：与本任务无Compose标签容器的显式container_name碰撞导致exit1；原容器完整ID和running状态不变，未被接管；已创建项目default网络 | CE09碰撞原生事实，产品ready前拒绝未执行 |
| NC08 | 通过：服务command配置变化引起app真实容器ID替换，逻辑app/1不变；移除dormant定义后再次原生up，不带remove-orphans，原dormant ID保留且出现orphan警告 | CE09替换/orphan原生事实，active/finalize未执行 |
| NC09 | 通过：good先started，bad使用不存在的可执行入口；Compose exit1时good=running、bad=created且default网络存在 | CE09非零部分变更；不能把非零解释成无副作用 |
| NC10 | 通过：slow=running、Compose等待service_healthy时向独立进程组TERM，原生Compose exit130；slow仍running、after=created、default网络仍存在 | 仅此原生等待点；不是产品executor/Agent取消验收 |
| NC11/11b | 通过：逐名清理各轮唯一命名空间资源；最终独立只读清点两个前缀的container/network/volume均空 | 只清理自有资源；镜像和证据保留，VM未删除 |
| NC12 | 通过：YAML alias引入env_file正常；两个label_file按顺序后者胜，inline标签最终覆盖；configs/secrets文件绝对路径规范化，实际为只读bind，容器读取字节等于原假文件；输入SHA不变 | 原生文件能力事实；产品AllowLabelFile未开放 |

## 可直接复核的故障证据

以下是现行规格预期的原生副作用边界，没有将它们标成产品缺陷：

1. 碰撞：`run-native.py:collision`。对已有无Compose标签的同名容器执行固定前缀+`up --detach --no-build`。`commands.jsonl`第62条记录exit1和真实Conflict；项目容器为0，但已有default网络。对应CE09、Compose规格§4“首次碰撞/不接管”和test-plan V06。
2. 非零部分变更：`partial_failure`的bad依赖good `condition:service_started`，bad entrypoint不存在。`commands.jsonl`第83条up非零，第85/86条inspect保留created/running事实；结果文件NC09记录状态。对应CE09与test-plan V07的“命令结束不证明回滚”。
3. 取消：`cancel_waiting_up`在slow进入running后对Compose进程组SIGTERM。`commands.jsonl`第92条记录injected_signal、exit130，后续inspect保存slow running/after created。对应CE07原生背景与test-plan V07；未执行产品10秒KILL监督规则。
4. 缺image但配置合法：补充`no_image`，`addendum-commands.jsonl`第4条config成功、第5条up exit1/No such image。与原无效布尔夹具严格分开。对应F02及CE05。

## 未覆盖与交接边界

- 本轮没有导入/调用项目Go模块，没有Prepare/VerifyBeforeStart/Observe与真实Engine组合，没有正式产品Invocation参数捕获、账本CommitPrepared认证、资源StackID/null、归属发布、替换/冲突分类的产品断言。
- 未执行Manager/Agent真实正向Apply、双端ACK/finalize/reconnect、两VM业务资源/权限隔离。没有据原生成功打开`compose.apply.v1`或写生产接受表。
- CE02链接/半发布、CE03/04静态拒绝、CE06输出上限、CE07产品监督、CE08采集中途消失/200预算、CE11/12执行冻结/控制env、CE14公开Stack关联、CE15端点更换均没有在本脚本中验收。A机Linux测试的事实由Root独立汇总，不能混算到本报告。
- 无image和external原生拒绝不代表产品ready前拒绝已验证；原生label_file成功也不等于其安全扫描/Schema/产品能力已全部通过。
- 没有进行掉电/磁盘满/设备Sync失败/恢复，未修改daemon配置、系统服务、用户组或公网端口，未执行privileged容器。

最终清理：主轮9容器/6网络/2卷，补充2容器/3网络/1卷均已删除；期间被Compose替换删除的旧app容器不计作最终清理对象。唯一保留的Docker工件为本轮固定busybox镜像缓存。可将该报告作为正式真实环境矩阵的原生对照证据入口，但必须另行完成相应产品集成用例后再决定精确接受项。
