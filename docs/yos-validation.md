# YOS 适配验证

2026-09-22。基于用户提供的内部 YOS 用户文档、Java SDK 示例和已分配 namespace 做回归；标准 S3 的历史验证见 [S3 验证](s3-validation.md)。本次没有修改线上部署或采集真实主机数据。

## 协议依据与实现

初始网关回归确认：普通 PUT/GET 正常，HEAD 没有返回自定义 SHA-256/agent/task 元信息，重复 `If-None-Match: *` PUT 返回 200，COS presign 接口返回可下载的 HTTPS 链接。网关 HTTPS 证书域名不匹配，文档提供 HTTP 地址；UFile presign 返回 HTTP 链接。

据此新增独立 YOS provider：显式允许内部 HTTP、完整 namespace 路径、按任务生成短对象名、单次流式 PUT、上传前后实际内容核验、COS 专用签名接口。未降低标准 S3 的条件写、元数据校验或 TLS 要求。行为和边界以 [配置说明](../configs/README.md#yos-结果转存) 和 [ADR-0004](adr/0004-yos-result-transfer.md) 为准。

## 已完成检查

| 检查 | 结果 |
| --- | --- |
| 全项目竞态与覆盖率测试（本机 darwin arm64） | 通过；agent 83.4%，总计 82.2% |
| 原 S3 与 YOS 联合回归 | 通过 |
| Linux Go 1.25 联合回归 | 无外网、源码只读挂载的容器内通过，包含 YOS 大文件检查；不依赖真实网关 |
| go vet ./... | 通过 |
| Linux amd64/arm64 构建 | 通过 |
| git diff --check | 通过 |
| 65 MiB 合成文件 | 本地 HTTP 协议测试通过；一次带 Content-Length 的 PUT，无 multipart，流式读回摘要一致 |
| 内部 YOS 实际任务转存 | 通过；合成采集器经真实 agent worker 上传，COS HTTPS 下载长度与完整 SHA-256 一致 |

覆盖：namespace/路径与配置校验、标准 S3 旧目标兼容、重启后校验远端并避免重复上传、同长度篡改后拒绝发布、已有冲突对象拒绝覆盖、权限/限流/服务错误脱敏、取消、签名接口失败、目标变化、本地过期后继续签名、大小限制、拒绝不可信 TLS、网关重定向及错误签名响应。

签名验证覆盖不安全协议、非 COS 域名、错误对象路径、userinfo/端口/片段、异常有效期、重复或缺失签名、损坏/超大 XML。真实网关时钟比本地约快 2 秒；加入最多 30 秒起始时间容差后，真实签名下载通过，过期时间仍要求与请求精确一致。XML 实体解码后保持原始 URL；COS 时间字段中的分号仅在验证查询参数时转义。

## 内部实测与复现

成功实测上传了一个 1,981 字节合成任务归档，COS 下载摘要为 `ed958d4708e05feaa4cfc449c3aeabafa169d1aa47a7fbb2899b2b4d318241b4`。该值仅记录本次运行，后续任务的时间与身份不同，摘要也不同。测试没有输出签名 URL，没有使用真实主机证据；每轮只删除自己的唯一对象，之后 HEAD 验证为 404。agent 生产代码不执行删除。

在允许写入并清理合成对象的已分配测试 namespace 中运行：

```sh
DEITYSIGHT_TEST_YOS_ENDPOINT=http://yos.example.internal \
DEITYSIGHT_TEST_YOS_NAMESPACE=team/qa \
DEITYSIGHT_TEST_YOS_ALLOW_HTTP=true \
go test -tags=yosintegration ./internal/agent -run '^TestYOSLiveTransfer$' -v -count=1
```

测试走当前配置、异步转存、签名及下载流程，使用合成 collector；不会启动 atop，也不证明线上 atop 部署已更新。真实网关仅验证小包，65 MiB 流式传输在本地协议服务验证；没有宣称真实网关已完成 1 GB 压测或跨云故障验证。YOS 不提供原子条件写保证，测试不声称消除了并发写竞争。

协议来源：内部 YOS [用户文档](https://paas.qima-inc.com/docs/yos/user.html)、[概述](https://paas.qima-inc.com/docs/yos/)、[Java 示例](https://paas.qima-inc.com/docs/yos/java-sdk.html)。

## 追加专项回归：1 GB 边界

2026-09-22 再次执行 `go test -race ./internal/agent -run '^TestYOS' -v -count=1`，9 组顶层测试及其子用例全部通过（6.221s）。`go vet -tags=yosintegration ./...` 与 diff 检查通过。本轮补充精确字节边界测试，没有修改生产逻辑。

| 最终归档大小 | 本地准入测试结果 |
| --- | --- |
| 999,999,999 字节 | pending，允许单次 PUT |
| 1,000,000,000 字节 | pending，允许单次 PUT |
| 1,000,000,001 字节 | unavailable / yos_object_too_large，零上传请求，worker 不选择该任务上传 |

边界测试还验证采集状态保持 completed、本地结果可用标记与 URL 保留、拒绝状态能够正确序列化并通过恢复元数据校验。测试使用稀疏文件和模拟 HTTP transport，验证的是 agent 准入与状态契约，**没有传输 1 GB 数据，也不证明真实网关的容量上限**。本地 65 MiB 实际流式 PUT/读回、原归档下载、重启恢复、对象冲突和签名异常测试也重新通过。

真实内部 YOS 联调独立重跑通过（2.41s）：合成任务归档 1,981 字节，经 COS HTTPS 下载后的 SHA-256 为 `28e9b9eb7a19debf312980781e598698d5bbda431cde054bfd7b420eb84a52ac`，与本地一致。测试结束删除本次唯一对象，HEAD 确认 404；未修改部署或上传真实主机证据。
