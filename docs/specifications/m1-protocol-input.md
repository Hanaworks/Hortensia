# M1共享JSON与Config输入校验规格

2026-11-12编码前基线，起点374016e。当前共享输入切片已通过本地定向验收，消费方集成与整仓验收由主代理继续。消费方为[HTTP适配器](m1-http-adapter.md)、[Manager运行模块](m1-manager-runtime.md)及[服务内核](m1-manager-service.md)；本模块在`internal/protocol`提供共同输入校验，不处理网络、身份、权限、存储或执行。共享Config结构唯一引用[Schema的Config定义](../../contracts/agent-v1.schema.json)，原消息流程承接[protocol内部说明](m0-channel-probe.md#82-protocol从不可信字节到受约束信封)。验收见[实施记录](../implementation/m1-protocol-input.md)。

## 1. 入口、输入输出与错误

| 入口 | 输入与输出 | 调用方义务 |
| --- | --- | --- |
| `protocol.StrictJSON(raw []byte) (any,error)` | 最多8 MiB完整JSON字节；成功返回独立的map[string]any／[]any／string／json.Number／bool／nil树。数字为精确规范化的安全整数；失败返回nil和固定错误码，不返回部分树 | 这是JSON结构／编码校验，尚非特定业务DTO。调用方必须再检查顶层类型、字段白名单及所属业务规则，不能无检查类型断言；Runtime还须先施加自己的64 KiB限额 |
| `(*Validator).ValidateConfig(raw []byte) error` | 最多8 MiB完整Config JSON；成功表示通过严格JSON、共享Config Schema和原Prepare补充规则，原输入不改写 | 由NewValidator构造后调用；保存原始字节或构造明确规范形式由内核负责，此入口不生成摘要、不默认授权部署 |

固定错误沿原实现：`invalid_json`表示坏JSON、重复键、非法UTF-8、未配对UTF16转义或尾随；`number_invalid`表示非数学整数或超安全整数；`budget_exceeded`表示原始输入／数字资源／文件字节预算超限；`payload_invalid`表示Config Schema、重复路径、路径安全或缺引用；`encoding_invalid`表示非规范Base64或文件不是UTF-8。错误不包含用户输入、文件正文、Schema底层详情或秘密。构造第三份Config规则失败只返回固定`bundled_config_compile_failed`；已有构造失败分类保留。多种错误并存时不保证对象遍历检查顺序，但不接受任何部分结果。

## 2. 严格JSON与本轮收紧

公共StrictJSON先检查8 MiB字节上限，再复用现有strictJSON。原有单值、无重复键、无尾随内容、有效UTF-8、递归深度10000、数字token≤128字节、显式指数绝对值≤128及±9007199254740991精确整数限制不变。1、1.0和1e0均返回等价十进制json.Number，不经过float64；JSON中的null仍是nil，业务是否允许null由调用方的DTO／Schema决定。边界8 MiB包含有效JSON外围空白，不截断后再受理。

本轮明确收紧旧消息和内层FrozenPayload：encoding/json原先会把未配对的`\uD800`—`\uDFFF`静默转为替换字符；现在strictJSON在解码前检查原始JSON字符串的Unicode转义。高代理项必须紧接一个低代理项转义；孤立低代理项、错误顺序、高代理项后隔普通字符／转义反斜杠均拒绝。合法代理对解成原码点，字面反斜杠后的`uD800`文本不误判；直接出现合法U+FFFD与`\uFFFD`不被当成解析修复。这一规则同时覆盖对象键和值，键按解码后内容检查重复，不进行Unicode归一化。

原始字节扫描只负责代理项配对和转义四位十六进制完整性；通用JSON词法／语法仍由原Decoder承担，避免实现第二个JSON解析器。扫描为O(n)时间、O(1)额外空间。调用方得到树前，严格解析已完全成功；对象树中的任何类型断言都必须建立在相应Schema或显式类型检查之后。

## 3. Config复用与内部协作

NewValidator继续离线加载同一嵌入Schema、拒绝外部资源并保持编译后不可变，另外编译`$defs/Config`为第三份规则。固定Base64线性匹配器以及其余ECMAScript100 ms策略沿374016e，不改变Schema、模式语义或旧消息8 MiB限额。

直接Config路径为：8 MiB检查 → strictJSON → Config Schema → 已验证对象 → 单一补充校验函数。原prepare路径为：原外层检查 → Base64解包 → strictJSON → FrozenPayload Schema → 外内目标关联 → 同一Config补充校验函数。因此Config字段、类型、数组／环境数量及未知字段仍只由共享Schema定义；补充函数只保存既有职责：安全相对路径、UTF-8路径字节≤240、文件路径唯一、严格Base64往返、文件内容UTF-8、每文件≤1 MiB、全部原文件≤4 MiB，以及compose/env引用都命中文件集合。

重要内部对象：`Validator.config`持有离线编译后的Config规则；每次解析树只在本次调用内存在；补充校验的paths集合仅保存本包文件路径，total累计解码后的原始字节，不把Base64文本大小误作4 MiB限制。文件顺序、内容换行、数组次序、环境值和Unicode保持原输入语义。Config中环境变量名恰好为input_digest或payload_b64也仍是用户环境变量，不调用外层消息字段的checkEncodedValues遍历。

本模块无持久状态、事务、锁、后台线程或关闭入口；Validator构造后可被并发调用，各请求使用独立临时数据。无context参数和硬响应时限承诺；HTTP／Runtime在读取阶段自行限制来源、大小与时限。失败、取消读取或重试不产生业务副作用，也没有恢复或自动重试行为。同一不可变输入在相同规则下给出相同有效性判断，不把校验当成操作幂等／授权事实。

## 4. 验证与维护路径

修改JSON规则须同时核对HTTP、运行配置、服务请求摘要和原Agent消息；尤其不能先经宽松json.Unmarshal丢掉重复键／未配对代理项后，再调用StrictJSON宣称检查了原输入。修改Config规则先检查共享Schema；自定义文件预算只维护一份补充函数，直接Config与prepare必须一致。改动数字解析不得引入float64舍入；改动字符串扫描须覆盖转义反斜杠、对象键、边界代理项与合法代理对。错误日志只能记录固定分类。

正常例：HTTP收到含合法代理对的环境值，StrictJSON返回同一码点；ValidateConfig核验原Config后内核才能决定规范存储。异常例：两个字段分别拼写为字面emoji键和其代理对转义，严格解析把它们识别为重复键并拒绝。另一个例子是文件看似有合法Base64，但pad bits非规范，直接Config和prepare均由同一补充检查拒绝，不能因为Schema已通过就保存。

实现后维护入口与逐项证据在[实施记录](../implementation/m1-protocol-input.md)回填；本共享输入模块完成不等于HTTP、Manager服务或产品发布完成。

## 5. 实现后的接手定位（2026-11-12）

[输入入口](../../internal/protocol/input.go)包含StrictJSON、ValidateConfig及原始转义扫描；[现有校验器](../../internal/protocol/validator.go)只新增Config规则编译、在strictJSON之前核验代理项，并将Prepare的补充检查提取为validateConfigFields。没有复制Config字段定义，也没有改374016e的Base64匹配器。StrictJSON只限制通用输入树，业务消费者仍须检查所用DTO；ValidateConfig先通过Config规则才调用包含具体类型断言的共享补充函数。

[输入回归](../../internal/protocol/input_test.go)从同一Config构造直接入口与prepare，25种组合对照有效性；另覆盖未配对代理项、合法补充平面字符、解码后重复键、8 MiB原始界限、数字及并发共享。排障时先区分JSON字节／Unicode／数字错误、Config结构错误、路径／引用错误和文件编码／字节预算错误，不把所有失败简化为Schema不通过。详尽逐项状态和范围限制保留在[实施记录](../implementation/m1-protocol-input.md)。
