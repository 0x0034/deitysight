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
