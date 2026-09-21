# atop 场景改造验收

2026-09-22，Q1–Q17 已确认。实现版本 2.0.0，结果协议 schema v2。agent 只采集并组织证据，验收脚本在 agent 外计算断言。

## 环境与准备

用户授权 `ssh root@R9` 进行场景回归。测试机 Rocky Linux 9.5、Linux 5.10.134-18.an8.aarch64、4 CPU、约 7.5 GiB 内存。机器时间为 2026-07-30，未修改其时钟；归档时间遵循主机时间，不能与此前生产告警时间直接关联。

从 Atoptool 官方 v2.7.1 源码构建，安装普通二进制到 `/usr/local/bin/atop`。未启动 atop 服务、进程会计或网络插件。准备构建环境时安装 ncurses-devel、glib2-devel，包管理器同时更新了部分 glib、util-linux、ncurses、SELinux 依赖；不属于 agent 自动安装功能。

测试实例为 `deitysight-regression.service`，配置和脚本在 `/opt/deitysight-regression`，存储在 `/var/lib/deitysight-regression`，仅监听 `127.0.0.1:19100`。使用专用非 root 账号及正式服务模板的沙箱、1 CPU/1 GiB 上限。测试结束停止服务，保留归档与脚本供复核。

## 自动化验证

- `go test -race -coverprofile=... ./...`：通过；主采集包约 82.5%，本地所有可编译代码总覆盖约 80.9%。macOS 不执行 Linux seccomp 路径。
- `go vet ./...`、`git diff --check`：通过。
- `make build`：Linux amd64、arm64 均通过；R9 实际运行 arm64 构建。
- R9 Linux 测试二进制：agent 全套、CLI 启停和沙箱测试通过；Linux agent 覆盖约 82.4%。amd64 仅交叉编译，未在 amd64 内核执行 seccomp 回归。
- 解析：RESET/SEP、线程筛选、命令行脱敏、恶意括号、超过 1 MiB 的帧逐行处理、缺少必需标签、坏输入与持久化失败。
- 窗口：只启动一次会话、超时为 partial、回收后可接受下一任务、无 atop 时健康/准入 503。
- 持久化：未提交尾帧不进入覆盖统计；损坏行、错误 sample_id、未写完的 SEP 行不能在恢复时提交；新历史不混入 v1 证据。

## R9 真实负载

执行 `integration/atop-scenes-check.py`。CPU 为单进程有界计算，IO 为专用 4 MiB 文件的限速 O_DIRECT 写入并 fsync，MEM 为 64 MiB 触页，网络只在 loopback 传输。负载有时限，退出后清理自身进程和临时文件。

| 请求 | 完整帧 | 源记录 | 归档字节 | 结果 |
| --- | ---: | ---: | ---: | --- |
| cpu | 5 | 3165 | 100794 | completed，目标 PID 的 CPU 时间增加 |
| io | 5 | 3150 | 96733 | completed，目标 PID 的写扇区计数增加 |
| mem | 5 | 3150 | 105764 | completed，目标 PID RSS 大于 60 MiB |
| network | 5 | 3145 | 95602 | completed，loopback 收发证据存在 |
| 四场景共享 | 5 | 7980 | 244532 | completed，同窗保留四类证据 |

每次为 4s/1s；所有归档校验 SHA-256，重复 request_id 返回相同任务。默认不保留线程，PRG 中不出现测试命令行标记。R9 未启用 PSI 和 netatop，能力返回 false；进程 I/O 支持为 true。不能把未启用能力的数值解释成零。

`integration/atop-lifecycle-check.py` 验证：后台会话可被任务抢占，完整历史保留；显式启用线程后存在 thread 记录；重启后任务为 interrupted 并可下载已有完整帧；禁用 atop 后 HTTP 保留、新任务 503、此前归档仍可下载。历史有间隙，不能按连续时间序列理解。

## 进程隔离

在 R9 复现旧沙箱超时后无法终止 `/bin/sleep` 的问题：100ms 取消仍等待约 10s。新沙箱约 100ms 完成取消和回收。验证跨 UID kill、pidfd 信号及 `/proc/PID` 目录 FD 信号被拒绝，自身 Go 线程信号可用。

同时验证阻止 pidfd_getfd，拒绝 real/effective/saved UID 不一致的身份；不允许保留 saved-root 身份绕过非 root 条件。专用 UID 下其他 agent 实例属于同一信号权限域，禁止业务共享该 UID。没有关闭 seccomp 或授予 CAP_KILL。

## 实际限制

R9 是功能与安全回归，不代表约 8 万线程生产规模的性能验收。检查到服务资源上限为 1 CPU/1 GiB，重启后的空闲 MemoryCurrent 约 4.5 MiB；该数字不是采集中内存峰值，不用于证明生产预算足够。

不采 host boot ID、完整 cgroup/Pod 映射、PSS、独立读写 await 或平均队列长度。不启用进程会计，因此可能漏掉采样间退出的短命进程。传统 netatop 在当前权限下可能不可用，未验证预部署插件的成功路径；不会为插件增加 raw socket 权限或发送管理信号。

仅改变 deitysight 代码及授权 R9 测试实例；没有修改此前的 10.215.32.10 生产部署。
