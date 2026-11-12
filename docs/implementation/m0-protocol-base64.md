# M0 Base64大正文匹配修复

2026-11-12保存修复前基线。当前状态：P01—P03及P04定向检查通过，P04统一全量待主代理验收。下表保留初始状态，实际结果见末节。规格见[实际输入约束](../specifications/m0-channel-probe.md#4-已实现的解析与输入约束)及[校验器内部说明](../specifications/m0-channel-probe.md#82-protocol从不可信字节到受约束信封)。本轮仅修复固定Base64模式的负载相关误拒绝，不改变共享Schema、消息预算、其他模式或业务状态。

## 故障证据与范围

全量日志`.cache/m0-dual-release-final-tests.jsonl`记录`TestPrepareFileBudgets/exact_total_limit`在0.25秒后收到`schema_invalid`；原失败日志没有保留底层匹配错误，不能把随后诊断冒充该次原始异常。相同未改动用例单独执行通过（0.21秒），不能据此关闭问题。

修复前临时Go overlay仅记录匹配长度、模式、耗时和不含正文的错误分类；不改共享源码或100 ms时限。相同4个1 MiB文件夹具产生7,457,863字节信封、7,457,420字节外层payload_b64。在GOMAXPROCS=1及8个短时CPU竞争goroutine下，原Base64表达式匹配443.1984 ms后明确返回`match timeout after 100ms`，Validate在460.932 ms返回`schema_invalid`。临时文件已删除，共享文件哈希未变。这直接证明相同合法数据在CPU竞争下的误拒绝机制；原次失败的具体底层异常只能据此推断。

旧实现把regexp2超时和普通不匹配都转为false，因此根Schema报错；拒绝发生在单文件／总量核对之前。表达式语法本身没有要求回溯或前瞻，允许用语义等价的线性匹配消除此负载相关判定。其余表达式继续保留ECMAScript语义和100 ms上限。严格解码、重编码一致性、UTF-8、路径与总量检查仍各自负责原有边界。

## 实施顺序与稳定验收

1. 保存本页及规格修订，再增加独立测试；记录旧实现下的实际失败。
2. 仅在`compilePattern`精确匹配当前Base64表达式时选择无状态线性实现；其余模式保留既有分支。专用匹配保留语法集合，不替代后续规范编码检查。
3. 定向运行容量、差分、坏输入和原校验器回归；由主代理运行统一全量验证，按实际证据回填状态，不把定向通过写成全量通过。

| 编号 | 验收条件与观察 | 初始状态 |
| --- | --- | --- |
| P01 | 4×1 MiB合法配置（总4 MiB、外层不足8 MiB）在受控CPU竞争时通过完整Validate；超单文件／总量仍拒绝，验证结果而非实现类型或速度阈值 | 未验证 |
| P02 | Base64坏字节、错误等号位置／数量、内部换行拒绝；模式可接受但pad bits非规范的编码由完整Validate拒绝；末尾LF／CR保持在模式层拒绝；合法规范编码仍通过 | 未验证 |
| P03 | 从共享Schema读取原模式，与regexp2.ECMAScript进行短输入语义差分，覆盖空串、所有字节、分组／填充边界及LF／CR／Unicode换行；不改变原Schema的匹配集合 | 未验证 |
| P04 | 其他表达式继续ECMAScript前瞻和100 ms限制；定向协议回归通过，主代理另运行全量test/vet/build/契约／文档检查并记录证据 | 未验证 |

测试环境为本机Go 1.27.0及go.mod锁定regexp2 v1.11.5/jsonschema v6.0.3。临时数据库、网络或Docker均非本修复依赖。CPU竞争测试只验证同一合法数据的判定稳定性，不构成吞吐或响应时限承诺。完成条件为P01—P03及P04定向检查具备证据，全量部分须由主代理明确回填。

## 实现中的语义更正（更正代码前保存）

2026-11-12：新增P03差分测试在输入`"\n"`失败，初版候选返回true而原regexp2.ECMAScript返回false。核对锁定regexp2 v1.11.5的runner.go `syntax.EndZ`：ECMAScript／RE2模式要求绝对字符串结尾，允许位于末尾LF前的是默认.NET模式。最初关于末尾单LF可匹配的文字假设错误，已先修订规格，再把候选改为保留原模式对所有末尾换行的拒绝。此记录保留真实发现顺序；不将错误假设回写为既已验证的规则。

## 定向实施结果（2026-11-12）

实现入口为[validator.go](../../internal/protocol/validator.go)的`base64SchemaExpression`、`base64Pattern.MatchString`及`compilePattern`；新测试为[base64_pattern_test.go](../../internal/protocol/base64_pattern_test.go)。Schema完整表达式精确相等才选择无状态匹配器。它先检查四字符总长，再识别最多两个末尾等号，最后单遍检查其余字符；任何换行或非ASCII字节都不能通过。大小、规范pad bits和配置业务约束仍由原后续步骤负责。

新增容量测试在修改生产代码之前实际失败：`TestBase64PrepareCapacityUnderContention`以0.43秒得到`schema_invalid`。此前临时诊断已直接证明旧引擎的100 ms超时机制。初版候选又被P03差分抓到错误的末尾LF假设，按前节记录修正后才进入以下通过结果。

| 编号 | 实际结果 | 证据与限制 |
| --- | --- | --- |
| P01 | 通过 | 新容量测试在GOMAXPROCS=1、8个CPU竞争goroutine下连续校验两次合法4 MiB文件配置；旧`TestPrepareFileBudgets`继续覆盖单文件／总量边界及各超额拒绝。测试不以耗时阈值决定成功 |
| P02 | 通过 | 16类语法／完整信封对照覆盖规范值、坏字节、填充、内部及末尾换行、两种非规范pad bits；有效语法但非规范编码仍由完整Validate拒绝 |
| P03 | 通过 | 从共享Schema取原表达式，对照regexp2.ECMAScript：19,531个短串枚举，加全部256字节插入、合法长度0—256及填充／Unicode换行边界。没有以放宽匹配集合解决超时 |
| P04 | 通过 | 定向新测试与协议整包分别1.628／2.568秒通过。主代理另从Git导出精确374016e，完整1117条测试／子测试通过、0失败／跳过，6个测试包通过；vet、build、mod verify通过。契约21有效／11无效样例及1解码payload通过，文档静态检查通过。完整批次及复验入口见[双端释放验收](m0-dual-release.md#2026-11-12最终验收与接手) |

实际运行版本为`go version go1.27.0 windows/amd64`。本修复没有新增后台线程／持久状态，CPU竞争goroutine仅存在于测试中并在该测试结束前退出。没有改Schema、其他模式编译规则、消息及配置预算；也没有把100 ms超时提高或关闭。P01—P04本地修复验收完成，不扩展为真实Docker或首版已验收。
