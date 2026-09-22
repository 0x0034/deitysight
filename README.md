# Deitysight

Linux 主机调查证据采集 agent，**强依赖 atop 2.7.1**。CPU、IO、内存、网络场景共用一个持续采样窗口；agent 只整理证据，不计算利用率、不排名、不诊断，分析由 server 完成。支持 Linux amd64/arm64，构建需要 Go 1.25+。

```sh
make verify
```

`make build` 通过 `-ldflags -X` 注入 `git describe --tags --always --dirty` 的版本描述；没有 Git 信息时使用 `dev`。发布构建可显式指定版本：

```sh
make build VERSION=v1.1.0
# 直接使用 Go 构建时：
go build -ldflags "-X github.com/0x0034/deitysight/internal/agent.Version=v1.1.0" -o dist/deitysight ./cmd/deitysight
```

直接构建且未注入时版本为 `dev`；运行 `deitysight -version` 查看二进制版本。

部署使用专用非 root `deitysight` 账号。该 UID 只能运行 agent 和采集子进程，不能复用于业务进程。安装固定版本 atop 的普通可执行文件（不设置 setuid、不启用会计或探针），并确认 `atop -V` 为 2.7.1。不同版本不会自动兼容。

```sh
useradd --system --no-create-home --shell /sbin/nologin deitysight
install -m 0755 dist/deitysight-linux-arm64 /usr/local/bin/deitysight
install -d -m 0750 -o root -g deitysight /etc/deitysight
install -m 0640 -o root -g deitysight configs/agent.example.yaml /etc/deitysight/agent.yaml
# 编辑配置，替换 token，并设置正确的 atop binary。
install -m 0644 deploy/deitysight.service /etc/systemd/system/deitysight.service
systemctl daemon-reload
systemctl enable --now deitysight
```

按机器架构选择安装文件。升级已有 root 部署前先停止服务，将专用存储目录及已有文件的所有者改为 `deitysight:deitysight`，配置保留 root 所有、deitysight 组可读；再替换服务单元。已有归档保持原内容，v1 下载不受格式升级影响。新任务只产生 schema v2，不混入旧版后台历史。

systemd 将 agent 和 atop 合计限制为 1 CPU、1 GiB 内存，磁盘默认预算 1 GiB。服务仅授予 `CAP_DAC_READ_SEARCH`、`CAP_SYS_PTRACE`，不授予 `CAP_KILL`。二进制安装 TSYNC seccomp，禁止 ptrace、进程内存操作、动态探针和外部线程信号；仅允许用于探测的 signal 0 及用于回收的 SIGKILL，再由内核 UID 权限检查隔离业务进程。root 或超出允许范围的能力集会被拒绝。文件系统只读保护依赖服务单元，直接运行二进制不会创建这些挂载限制。

所有接口均需 Bearer token。默认仅监听 `127.0.0.1:19100`；远程访问可改为管理网地址。配置启动时读取，修改后重启。配置文件、存储路径和旧版迁移字段见 [配置说明](configs/README.md)。

```sh
curl --fail -H "Authorization: Bearer $DEITYSIGHT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"request_id":"investigation-0001","window_seconds":30,"step_seconds":5,"scenes":["cpu","io","mem","network"],"include_threads":false}' \
  http://127.0.0.1:19100/v1/tasks

curl --fail -H "Authorization: Bearer $DEITYSIGHT_TOKEN" \
  http://127.0.0.1:19100/v1/tasks/TASK_ID

curl --fail -H "Authorization: Bearer $DEITYSIGHT_TOKEN" \
  -o result.tar.gz http://127.0.0.1:19100/v1/tasks/TASK_ID/result
```

`scenes` 省略时选全部四类；不接受空数组、重复项或未知场景。默认保存全部可见进程，`include_threads=true` 才保留线程记录，不做 Top N。该过滤不能降低 atop 内部扫描线程的成本。幂等键仍为 `request_id`，场景顺序不影响幂等匹配；参数变化返回 409，同一时间只接受一个任务。

默认窗口 30s、步长 5s，计划包含 RESET 基线及 6 个区间帧。实际时刻与间隔以 atop 输出为准，延迟时不会无限等到凑齐帧数。一个任务只启动一个 atop 会话，默认最多等待窗口加 10s 启动、5s 收尾宽限。子进程被取消后回收，后续任务可以继续执行。

结果为 `tar.gz`，包含 `manifest.json`、`samples.jsonl`、`errors.jsonl`，启用后台历史时另有 `history.jsonl`。manifest 和下载 ETag 提供 SHA-256。**只使用有对应 `frame_end`（SEP）的记录**；尾部未提交帧不能作为完整证据。首帧 `baseline=true` 是启动以来累计量，后续帧是 atop 的区间值，不能再次差分。PRG 命令行在任何落盘前替换为 `()`；不保留原生 raw 或 stderr。

| 场景 | 证据 | 边界 |
| --- | --- | --- |
| CPU | CPU、每核 cpu、CPL、PRC、PSI | server 根据 hertz 和实际 interval 解读 CPU 时间 |
| IO | DSK、LVM、MDD、PRD、PSI | 无独立读写 await、平均队列；进程 I/O 不是按设备拆分 |
| MEM | MEM、SWP、PAG、PRM、PSI | 不采 PSS；共享内存使进程 RSS 不可简单相加 |
| network | NET、PRN | 没有可用插件时只能提供主机/接口数据，进程计数不可当成零 |

`capabilities` 和每条记录的 `supported` 区分能力不可用与实际零值。基础证据完整、但可选能力缺失时仍可 `completed`；超时、损坏、截断或丢帧为 `partial`，没有可用完整帧为 `failed`。atop 缺失、禁用、配置 raw 或版本不支持时，`/v1/health` 返回 503，新任务被拒绝；任务查询和旧归档下载继续可用。安装或修复 atop 后重启 agent 重新探测。

`ATOPACCT=''` 禁用系统进程会计，采样间退出的短命进程可能不可见。agent 不安装/加载网络探针；传统 netatop 可能因服务权限不可用，不自动扩权。PRG 仅提供 atop 可见的容器标识，不包含完整 cgroup、Pod 归属或 host boot ID。

后台默认关闭；开启后以默认 30s 步长保留 10m 证据。按需任务取消后台会话并冻结完整历史帧，结束后重开后台会话。历史有会话和任务造成的空隙，`history_reason`、基线和时间戳明确其非连续性，不承诺全窗口覆盖。

可选启用 **S3 结果转存**：配置 `s3.enabled: true` 及 HTTPS endpoint、region、bucket 和上传凭据后，新任务的最终归档会异步上传，任务查询的 `result.s3` 返回状态和默认 1 小时有效的预签名下载链接。采集状态与上传状态独立，本地下载继续可用；网络失败在本地结果保留期内自动重试，重启后恢复。旧任务不自动补传，远端对象由 bucket 生命周期管理。完整字段、权限及过期语义见 [S3 配置说明](configs/README.md#s3-结果转存)，测试证据见 [S3 验证](docs/s3-validation.md)。

内部 YOS 使用 `s3.provider: yos`，将分配的 `namespace/key` 整体配置为 bucket。适配支持显式允许内部 HTTP 网关、单次流式上传与完整读回校验，并返回 COS HTTPS 下载链接；当前归档上限 1 GB，不使用 multipart。配置片段、额外流量和一致性边界见 [YOS 配置说明](configs/README.md#yos-结果转存)。

设计依据见 [场景改造](docs/design/atop-scenario-refactor.md)，实测结果与限制见 [atop 验证](docs/atop-validation.md)。旧版技术设计和验证文件仅描述 schema v1。

远程调用方可使用 [deitysight-remote skill](skills/deitysight-remote/SKILL.md)，其中包含 HTTP 调用、幂等重试、S3 链接获取和 atop 证据解释流程。将整个 `skills/deitysight-remote/` 目录复制到调用端的 skills 目录（例如 `~/.codex/skills/`）后，可通过 `$deitysight-remote` 使用；具体目标地址和凭据由调用者提供，skill 中不包含固定主机或真实秘密。
