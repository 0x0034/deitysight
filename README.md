# Deitysight

Linux 主机只读采集 agent。保留 CPU、内存、swap、I/O、PSI、可见进程/线程与 cgroup 原始信息，供外部 server 分析。项目不包含 server，也不调用大模型。

需要 Go 1.25+ 构建，支持 Linux amd64/arm64，运行身份为 root。

```sh
make verify
```

将对应架构的 `dist/deitysight-linux-*` 安装为 `/usr/local/bin/deitysight`，复制 `configs/agent.example.yaml` 为 `/etc/deitysight/agent.yaml`，设置真实 token，配置文件权限设为 `0600`。手动启动：

```sh
/usr/local/bin/deitysight --config /etc/deitysight/agent.yaml
```

systemd 模板位于 `deploy/deitysight.service`。修改存储路径时同时调整 `ReadWritePaths` 与 `StateDirectory`；安装单元后执行 `systemctl daemon-reload`、`systemctl enable --now deitysight`。默认使用 0.2 核 CPU 配额、256 MiB 内存上限。已在 systemd 252 测试模板；其他版本需按验证文档检查。

二进制启动时安装作用于所有线程的 seccomp 过滤器，禁止跨进程信号、ptrace、进程内存系统调用及动态探针，保留 Go 运行时的自身线程信号。内核必须支持 seccomp TSYNC，不支持时拒绝启动。文件系统保护由 systemd 模板提供；手动运行不会自动创建只读挂载命名空间。

配置启动时加载。默认监听 `127.0.0.1:19100`，需要远程 server 访问时改为管理网地址。HTTP 使用 Bearer token，按已确认设计不提供 TLS。示例 token 会被启动校验拒绝。

`background.enabled: false` 时仅按需采集；设为 `true` 后以 `background.step` 低频采样。任务保存自身的历史副本。数据写入 `storage.path`，结果保留 24 小时，任务与幂等记录保留 7 天；预算计入原始文件、归档、临时文件和下载中的文件。

调用示例（`DEITYSIGHT_TOKEN` 由调用者设置，不是 agent 的配置入口）：

```sh
curl -H "Authorization: Bearer $DEITYSIGHT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"request_id":"investigation-0001","window_seconds":30,"step_seconds":5}' \
  http://127.0.0.1:19100/v1/tasks

curl -H "Authorization: Bearer $DEITYSIGHT_TOKEN" \
  http://127.0.0.1:19100/v1/tasks/TASK_ID

curl --fail -H "Authorization: Bearer $DEITYSIGHT_TOKEN" \
  -o result.tar.gz http://127.0.0.1:19100/v1/tasks/TASK_ID/result
```

窗口/步长可省略，默认 30s/5s，包含起点和终点（共 7 轮）。同一 `request_id` 重试返回原任务；不同参数返回 409。同一时间只接受一个按需任务。所有接口（含 `/v1/health`）均需鉴权。

结果为 `tar.gz`：`manifest.json`、`samples.jsonl`、`errors.jsonl`，开启后台采样时还包含 `history.jsonl`。manifest 提供身份、有效参数、时间覆盖、计数语义和数据文件 SHA-256；归档本身 SHA-256 通过 ETag 返回。源数据非 UTF-8 时使用 Base64。

每轮有软时间预算，每数据源有字节上限。权限限制、进程退出、身份变化、超时、截断与丢点均作为缺失报告，不会补成零或推断根因。`completed` 表示本次计划采集未报告缺失，不保证内核暴露了所有主机对象。容器部署仅能看到所在命名空间，因此建议在宿主机部署。

采集仅访问固定内核数据源；不读取命令行参数、环境变量、进程内存或业务文件正文，不执行 shell/外部命令。进程聚合计数与线程计数不能累加，线程内存共享，`wchan=0` 也不能证明没有等待。任务因 agent 重启而中断时只恢复已有证据，不继续补采。元数据损坏会暂停新任务准入。

详细协议与边界见 [技术设计](docs/design/agent-technical-design.md)、[数据源说明](docs/design/linux-data-sources.md)；验证证据见 [实施验证](docs/implementation-validation.md)。
