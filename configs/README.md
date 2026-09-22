# 配置与 v2 迁移

复制 [agent.example.yaml](agent.example.yaml)，替换 token，选择管理网监听地址和 atop 的安装路径。配置使用严格 YAML 字段检查；启动时加载，修改后重启。所有 HTTP 接口均需 Bearer token，当前不提供 TLS。

配置文件建议 `root:deitysight 0640`，其父目录 `0750`，存储目录由专用账号拥有、权限 `0700`。使用 [systemd 单元](../deploy/deitysight.service)运行；更改存储路径时同步调整 `StateDirectory` 和 `ReadWritePaths`。不要与其他实例共享目录，不要让业务进程使用 deitysight UID。

| 字段 | 默认值 | 含义 |
| --- | --- | --- |
| http.listen | 127.0.0.1:19100 | 远程 server 调用时改为管理网地址 |
| http.token | 必填 | 不接受空值、首尾空白、换行或 REPLACE_WITH 占位符 |
| storage.path | /var/lib/deitysight | 专用绝对目录，任务、历史、归档、临时数据共用预算 |
| storage.result_retention | 24h | 归档及采集附件保留时间 |
| storage.task_retention | 168h | 任务和幂等记录保留时间，不短于归档保留时间 |
| storage.max_bytes | 1073741824 | 总存储预算，包含临时文件和下载租约 |
| storage.min_free_bytes | 1073741824 | 文件系统最低可用空间 |
| background.enabled | false | 可选后台历史；按需任务优先，切换存在间隙 |
| background.step | 30s | 后台 atop 步长，使用整秒 |
| background.retention | 10m | 历史保留时长及一个后台会话的最长计划窗口 |
| sampling.default_window | 30s | 默认任务窗口 |
| sampling.default_step | 5s | 默认任务步长 |
| sampling.max_window | 300s | 任务窗口上限 |
| sampling.min_step | 1s | 最短步长 |
| sampling.max_points | 301 | 最多计划帧数，包括 RESET；非整除窗口向上取整 |
| sampling.max_source_bytes | 1048576 | **单行**缓冲上限，范围 256 字节–16 MiB；大帧逐行写入，不整体缓冲 |
| atop.enabled | true | false 时健康 503，保留查询/下载，拒绝新任务 |
| atop.mode | parseable | v2 唯一可采集模式 |
| atop.binary | /usr/bin/atop | 仅允许 /bin/atop、/usr/bin/atop、/usr/sbin/atop、/usr/local/bin/atop |
| atop.startup_grace | 10s | 启动宽限，允许 0–60s |
| atop.finish_grace | 5s | 收尾宽限，允许 0–60s |

任务窗口与步长必须是正整数秒。实际区间由 atop 输出决定；计划帧数是 `ceil(window / step) + 1`，最后一帧可能在请求窗口之后，仍受总超时约束。线程很多或 CPU 限流时会减少完整帧，不无限等待补齐。结果的 epoch、interval_seconds、baseline、frame_end 必须共同解读。

旧字段 `sampling.round_timeout`、`atop.interval`、`atop.path` 仍可解析以方便迁移，但不控制 v2 采样；窗口/步长来自任务或后台设置，总超时来自窗口及两项 grace。删除这些旧字段可减少误解。旧 `mode: raw` 不再采集，必须改为 `parseable` 才能接受新任务；旧归档不会被重新脱敏或改写。

atop 探测只在启动执行一次，修复缺失、版本或权限后重启 agent。只支持官方 atop 2.7.1，设置 `ATOPACCT=''` 关闭会计，不执行 shell、不接受额外命令参数。命令行在写入任何文件前移除；不保存 stderr。

升级步骤：停止旧服务，安装已固定版本的 atop 和新 agent，调整配置和存储目录权限，更新服务单元，再启动。不要直接以 root 运行新 agent。服务共享 1 CPU、1 GiB 内存上限，磁盘默认 1 GiB；该预算需要按实际进程/线程规模校准。

## S3 结果转存

S3 为可选功能，默认关闭。启用后，**新接收的 task** 在本地归档完成后异步上传；用户或中间 server 仍通过原 HTTP 接口提交和查询任务。

```yaml
s3:
  enabled: true
  endpoint: "https://s3.example.com"
  region: "us-east-1"
  bucket: "deitysight-results"
  prefix: "deitysight"
  force_path_style: true
  access_key_id: "REPLACE_WITH_ACCESS_KEY_ID"
  secret_access_key: "REPLACE_WITH_SECRET_ACCESS_KEY"
  session_token: ""
  presign_ttl: "1h"
  upload_timeout: "2m"
  retry_initial: "5s"
  retry_max: "5m"
```

| 字段 | 默认值 | 约束与作用 |
| --- | --- | --- |
| s3.enabled | false | 是否接收新转存意图并运行上传；关闭后已有待办暂停，但本地截止时间不延长 |
| s3.endpoint | 无 | 启用时必填 HTTPS 服务根地址；不接受用户信息、路径前缀、查询或片段；必须通过 TLS 证书校验 |
| s3.region | 无 | 必填签名区域，按服务实际设置；示例 us-east-1 不是所有服务的正确值 |
| s3.bucket | 无 | 预先创建的私有 bucket；启用时必填，遵循 3–63 字符 DNS 桶名规则 |
| s3.prefix | deitysight | 对象前缀，可空；不接受绝对路径、父目录穿越、重复分隔符、控制字符，最多 512 字节 |
| s3.force_path_style | true | true 使用 endpoint/bucket/key；false 使用 bucket.endpoint/key |
| s3.access_key_id | 无 | 显式配置的上传/签名身份；不会自动读取其他 AWS 凭据来源 |
| s3.secret_access_key | 无 | 启用时必填，不接受占位符或控制字符；不写入结果、响应或日志 |
| s3.session_token | 空 | 可选临时凭据 token；链接可能因凭据提前到期而早于 URL 标注时间失效 |
| s3.presign_ttl | 1h | 正整数秒，最多 7 天；GET task 时签发新链接，实际有效期还受凭据/桶策略影响 |
| s3.upload_timeout | 2m | 单次尝试预算，必须为正；还受本地结果剩余保留时间约束 |
| s3.retry_initial | 5s | 初始退避间隔，必须为正 |
| s3.retry_max | 5m | 最大退避间隔，不小于 retry_initial；加入抖动避免同时重试 |

上传对象为原有最终 `tar.gz`，包含 partial/interrupted 结果和可下载的 failed 错误证据包。对象名为 `<prefix>/<agent_id>/<task_id>/<归档SHA-256>.tar.gz`，重试沿用该 key。启用版本控制的 bucket 仍可能保留重复写入的版本，需自行设置生命周期。

单独的 worker 以单并发上传，超过 64 MiB 使用顺序 multipart（默认 8 MiB 分片，较大文件自动调整），不将整包放入内存。上传过程中本地文件持有租约、始终计入存储预算。认证失败或网络故障不改变 task 的采集结局，查询 `result.s3` 获取实际转存状态。

| result.s3.state | 含义 |
| --- | --- |
| waiting_result | 等待 task 完成本地归档 |
| pending / uploading | 等待上传 / 正在上传及核验 |
| retry_wait | 等待 next_attempt_at，last_error_code 给出脱敏原因 |
| uploaded | 已上传并提交成功记录，可生成 url 与 url_expires_at |
| expired | 本地结果保留期已过，停止上传重试 |
| unavailable | 没有归档，或本地文件缺失、完整性不匹配 |

`paused: true` 表示开关关闭或当前 endpoint/region/bucket/prefix/寻址模式与该任务的原目标不匹配。恢复匹配配置并重启后可继续未到期待办；更换凭据不改变对象身份。已有归档不会自动批量补传，重复 request_id 也不会为旧任务追加上传意图。

上传成功后本地仍默认保留 24 小时。到期仍未上传成功则停止重试并清理，可能失去唯一副本；保留期不会因为 S3 不可达而自动延长。上传成功的本地文件到期后，任务记录有效期内（默认 7 天）仍可查询新链接，原本地下载接口仍返回 410；任务记录过期后返回 404。关闭 S3 后仍可用匹配且完整的签名配置为已上传对象生成链接。

S3 对象由 bucket 生命周期管理，本地 TTL 不删除远端对象。`uploaded` 是曾经成功上传的记录，不保证对象未被外部删除。预签名 URL 只在 API 响应中生成，不保存在 task.json、归档或日志中；签名失败会省略 URL 并提供 url_error_code。

需要目标前缀的 PutObject/GetObject（包括 HEAD 和预签名下载）权限，以及 multipart 的上传、中止和 ListMultipartUploadParts 权限。不需要 DeleteObject。HEAD 在缺少 ListBucket 时可能以 403 表示对象不存在，agent 会将其作为权限错误处理；按部署环境授予适当的受限 ListBucket 可明确区分不存在对象。开启桶的 AbortIncompleteMultipartUpload 生命周期清理，以兜底进程崩溃后遗留的分片；该规则不等同于已完成对象的保留规则。

上传请求附带 checksum，并核验远端长度与自定义 SHA-256 元信息；S3 ETag 和 multipart composite checksum 均不作为完整文件 SHA-256。下载方可使用响应的 sha256 对实际下载字节重新校验。服务需兼容条件写入 `If-None-Match: *` 及所用 checksum/multipart API，不支持时显式失败，不静默放弃校验或覆盖保护。

保持配置文件最小读取权限，放行到 endpoint 的 DNS/HTTPS；企业 CA 加入服务的可信证书环境，不能关闭证书验证。endpoint 也是签名链接使用的域名，不支持签名后替换域名或独立下载代理。实际服务联调信息和凭据不进入仓库。
