# S3 结果转存实施验证

日期：2026-09-22。实现依据为 [已确认设计](design/s3-remote-storage.md) 和 [ADR-0003](adr/0003-asynchronous-result-transfer.md)。本轮仅修改 agent 仓库并使用本地测试服务，没有访问生产 S3、上传真实主机归档或修改线上部署。

## 实现与检查

新增默认关闭的 S3 配置、持久化转存意图、独立单并发上传 worker、重试与重启恢复、result.s3 查询响应及按需生成的预签名链接。上传重试不改变采集状态，原本地 HTTP 下载和过期语义保持兼容。

使用 AWS SDK for Go v2：S3 v1.97.3、aws v1.41.5、eventstream v1.7.8、smithy v1.24.2，依赖已 vendor。初选旧 SDK 被 govulncheck 检出 GO-2026-5764，已升级至修复版本，重新扫描没有发现漏洞。

本地 Go 1.27.1 / darwin arm64：

| 检查 | 结果 |
| --- | --- |
| 完整竞态与覆盖率测试 | 通过；agent 包 82.3%，全项目可编译代码 81.1% |
| go vet ./... | 通过 |
| go mod verify | 通过 |
| govulncheck ./... | 升级后通过，无已知漏洞报告 |
| Linux amd64/arm64 静态构建 | 通过 |
| git diff --check | 通过 |

覆盖的转存场景包括正常上传、重放幂等、后台上传期间继续采样、失败后重试、关闭/重开、目标变更、凭据轮换、采集期间重启生成 interrupted、failed 错误证据包、远端已上传但本地成功记录丢失、损坏或缺失本地文件、对象冲突、HEAD 403、元数据损坏、签名失败及本地 TTL 到期取消上传。

Linux 使用 Go 1.25 / Alpine 3.23 镜像，主采集包全套测试通过，覆盖 81.9%。原有 CLI/沙箱降权测试首次因 Go 临时目录不可遍历而失败；将测试二进制编译到容器中可遍历的 `/tmp/deitysight-test-binaries` 后，两包均通过，无需修改程序或放开沙箱。该环境没有 atop 2.7.1，CLI 验证覆盖缺少 atop 时的查询/准入和退出行为；真实 atop 场景回归仍见既有 atop 验证记录。

HTTPS 协议测试验证 SDK 产生真实 HTTP 请求、checksum 请求头、原字节传输、对象元信息核对、条件写入、防止覆盖、multipart checkpoint 与中止。不可信 TLS 证书明确失败。API 只返回脱敏错误码，持久化元数据和归档不含访问凭据及预签名 URL。

## 独立 MinIO 实测

本地一次性容器镜像：`quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z`，镜像摘要 `sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e`。仅发布到 127.0.0.1 随机端口，启用 HTTPS；测试客户端使用显式可信测试证书，没有关闭证书验证。内存 512 MiB、CPU 1 核、临时数据盘 256 MiB，凭据仅用于该容器。

通过 `go test -tags=s3integration ./internal/agent -run TestS3LiveHTTPS -v` 实测：

- 新 task 本地归档完成后上传到测试 bucket，返回可实际下载的预签名链接；下载长度和 SHA-256 与本地结果一致。
- 将链接签名篡改为错误值，MinIO 返回 403，验证不是仅检查 URL 字段形状。
- 70 MiB（73,400,320 字节）文件触发生产 multipart 阈值，分片顺序上传；HEAD 身份/长度/摘要声明一致，实际下载后完整 SHA-256 再次一致。
- 再次尝试写入同一 key 时，条件写保护拒绝覆盖，未完成 multipart 被中止。
- 上述大文件上传、下载校验与冲突检查在本次本地环境合计约 0.74 秒；这只是本地功能验证，不代表生产网络性能。

完整可复现脚本：

```sh
sh integration/run-s3-check.sh
```

脚本创建唯一的临时测试目录与 bucket，测试结束删除自己的容器和容器测试对象；两天有效的测试证书留在脚本打印的临时目录。不要把带 `s3integration` 标签的测试指向生产服务或业务 bucket。

## 语义边界

- 对象 HEAD 中自定义 SHA-256 是声明信息；本次 MinIO 测试另行下载并重算，才验证完整内容一致。生产上传不会每次下载整个对象。multipart composite checksum、ETag 不作为完整归档 SHA-256。
- 重启恢复的是待办；旧 multipart 会话先中止，再从头传输，不承诺分片级断点续传。上传 ID 尚未成功持久化即崩溃的残留由 bucket 的未完成 multipart 生命周期兜底。
- 临时凭据到期、策略变化或外部删除对象可使链接提前失效。uploaded 表示成功记录，不是每次查询都实时检查对象存在。
- 只验证了本地 MinIO 和可控 HTTPS 协议服务；实际目标服务、endpoint、bucket 与凭据尚未提供，不能声称真实目标服务或所有 S3 兼容实现已完成联调。
- agent 不创建生产 bucket、配置生命周期或删除已完成远端对象。桶权限、CA、网络及生命周期由部署方准备。
