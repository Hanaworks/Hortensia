# M1共享输入校验实施与验收

2026-11-12编码前基线，起点374016e。当前I01—I05及I06本地检查通过；消费者集成与整仓统一验收尚未由本子任务验证。下表保留编码前初始状态，结果见末节。[模块规格](../specifications/m1-protocol-input.md)在编码前定义入口、预算、JSON收紧与Config复用边界。仅修改`internal/protocol`及本模块说明，contracts只读；HTTP、Runtime及服务内核由各自模块接入。

## 实施顺序

1. 保存本页与规格，先新增失败测试：未配对UTF16曾被替换接受、新公共API缺失。
2. 新增无副作用的代理项扫描并复用strictJSON；公开StrictJSON施加8 MiB上限，保留原数字／重复键规则。
3. NewValidator编译现有Config定义，直接入口完成Schema后才断言对象；把prepare补充校验提取为唯一函数，两条路径复用。
4. 定向验证各边界与原协议回归，记录RED／GREEN、环境和限制；主代理另做整仓统一验证及消费者集成，不以本包通过替代服务可用。

| 编号 | 验收条件、观察与完成标准 | 初始状态 |
| --- | --- | --- |
| I01 | 严格解析拒绝孤立高／低、颠倒、跨字符、转义反斜杠分隔的代理项；合法pair、直接Unicode、合法U+FFFD及字面反斜杠文本保持；键和值都覆盖，失败不返回部分树 | 未验证 |
| I02 | StrictJSON在8 MiB边界可解析、超1字节拒绝；重复解码后同名键、尾随、BOM、坏UTF8、深度、数字token／指数预算拒绝；精确安全整数1.0／1e0规范化，分数／溢出不舍入接受 | 未验证 |
| I03 | ValidateConfig接受共享Schema有效对象，拒绝错类型／null／未知键／缺必填；环境值不被误当协议字段，输入字节和数组内容不改写 | 未验证 |
| I04 | 直接Config与原Prepare对文件／总量、规范Base64、UTF8、路径字节／安全性、重复文件、缺引用及数组／环境数量给出相同有效性；总4 MiB和1字节超额都有实际断言 | 未验证 |
| I05 | 旧Agent信封及FrozenPayload同步拒绝未配对Unicode转义，合法pair仍可验证；Base64修复和其他旧协议回归不退化，固定错误不泄漏正文 | 未验证 |
| I06 | 并发调用共享Validator只使用独立解析数据；单包test/vet通过。整仓test/vet/build、HTTP／Runtime／内核集成由主代理统一验收并单独记录 | 未验证 |

环境为本机Go及go.mod锁定库；实际版本交付时复读。无需数据库、Docker、TLS或外部网络。命令为`go test ./internal/protocol -count=1`及`go vet ./internal/protocol`；按新增测试命名可定向重现。I01—I05及I06本地检查通过才能标记共享输入切片完成；整个服务与第一版交付仍依赖消费者的真实接入与验收。

## 实施结果与交接（2026-11-12）

RED先于生产修改：`TestSharedInputRejectsUnpairedSurrogatesBeforeReplacement`的6类输入实际被旧strictJSON替换为U+FFFD并接受，测试失败；随后新增公共入口测试实际因StrictJSON／ValidateConfig未定义编译失败。实现后不改这些拒绝条件，也未修改共享Schema或旧预算。

| 编号 | 结果 | 实际证据 |
| --- | --- | --- |
| I01 | 通过 | 孤立高／低、低高顺序、跨字符、字面反斜杠分隔、对象键均拒绝；合法emoji及U+10000／U+10FFFF代理对、U+FFFD和字面反斜杠文本保持。错误返回nil树 |
| I02 | 通过 | 精确8 MiB含空白JSON可解析，超1字节为budget_exceeded；BOM、坏UTF8、尾随、解码后同名键、10001层嵌套及数字边界拒绝；1.0／1e0返回json.Number("1")，非精确整数不被舍入接受 |
| I03 | 通过 | 错顶层类型、null、未知／缺失／错误字段及原始坏JSON被拒绝；环境名input_digest／payload_b64作为普通环境保留；校验未改变输入字节 |
| I04 | 通过 | 25种直接Config／Prepare对照覆盖单文件与4 MiB总量及各超1字节、Base64／UTF8、路径／引用、240与243字节路径、128／129文件、1024／1025环境值及数字／类型错误 |
| I05 | 通过 | 旧result信封与内层FrozenPayload分别验证未配对转义拒绝、合法emoji代理对通过；原协议与Base64修复测试全部继续通过，错误仍是固定分类 |
| I06 | 本地通过；集成未验证 | 同一Validator在8个goroutine各8轮直接Config与坏Unicode校验中保持一致；本包完整test与vet通过。未代替HTTP／Runtime／服务内核接入及全仓验收 |

命令证据：`go test ./internal/protocol -run '^TestSharedInput' -count=1 -v`通过（包1.024秒）；`go test ./internal/protocol -count=1`通过（包3.316秒）；`go vet ./internal/protocol`退出0。运行环境为Go 1.27.0 windows/amd64。源码改动为新增input.go／input_test.go及validator.go的共同入口；contracts、原validator_test.go和已验收base64_pattern_test.go未改，未执行Git提交。

消费者需从原始字节调用StrictJSON，不能先做宽松解码再重编码交给它；Runtime应先落实自己的64 KiB输入限制。输入树中的json.Number是精确规范整数，业务类型、未知字段、权限、事务和请求摘要仍由对应消费者负责。下一依赖是消费者真实接线及主代理统一回归，本模块完成不等于服务或首版可发布。
