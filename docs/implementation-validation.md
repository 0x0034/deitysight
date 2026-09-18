# 首版实施验证

验证日期：2026-09-18（Asia/Shanghai）。项目交付 agent，不包含 server 或模型调用。

## 已实现

- HTTP + YAML token；异步提交、状态查询、健康检查、归档下载。
- 默认 30s 窗口、5s 步长，含起点和终点；固定时序、软超时、丢点记录、单个按需任务。
- 可配置后台采样；串行执行、按需优先、历史冻结、跨重启 boot_id 归属。
- 主机、所有可枚举进程/线程、相关 cgroup 及可见祖先的原始白名单数据；不做 Top N、排名或根因分析。
- 可选 atop parseable/raw 快照；固定可执行路径和参数，agent 不执行 shell，atop 输出只作为辅助原始记录。
- atop 仅在按需 task 的采样轮次启动；task 的有效 `step_seconds` 覆盖 atop 默认间隔，后台采样不会启动 atop。
- 专用目录、独占锁、原子元数据、流式归档、SHA-256、幂等恢复、TTL、下载租约与配额。
- 中断恢复、损坏 JSONL 原件保留、损坏元数据阻止新任务、结果损坏拒绝下载。
- root systemd 模板；只读文件系统保护，二进制 seccomp TSYNC 禁止进程控制与探针。

## 自动化检查

本地 Go 1.27.1 / darwin arm64；Linux 使用现有 Go 1.25 Alpine 3.23 镜像。

```sh
go test ./...
go test -race ./...
go vet ./...
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
go mod verify
govulncheck ./...
make build
```

竞态检查、静态检查、模块校验均通过。Go 官方漏洞检查未发现可达已知漏洞。
Linux amd64、arm64 构建通过。最终本地语句覆盖率：全项目 81.6%，核心 agent 包 83.6%；
CLI 与 seccomp 的 Linux 子进程测试也采集覆盖率（本地 macOS 不运行这些 Linux 路径）。
最终 Linux 测试的包覆盖率为 agent 83.6%、CLI 73.2%、seccomp 75.0%。

回归测试覆盖：严格 JSON/YAML、鉴权、默认/非整除计划、并发准入、幂等与配置变化、
超时/丢点、后台采样不中断、归档内容与校验和、过期下载、下载保留与清理恢复、
元数据冲突、损坏尾行、字节截断、非 UTF-8、符号链接、PID 身份变化、线程独立 cgroup、
cgroup v1/v2 目录夹具、父级身份及读取去重、存储预算与打包预留、空间不足。

## 真实 Linux 与 systemd

环境：OrbStack Linux `7.0.14-orbstack-00380-ga7e0a2dc9535`；
Rocky Linux 9 容器中的 systemd 252。容器无网络，项目只读挂载；需要容器特权以测试 systemd 挂载保护。
原生 arm64 agent 与测试探针通过；Rocky 用户空间本身为 amd64 仿真。

验证脚本只用于一次性测试容器/虚拟机，不能直接在业务主机运行：

```sh
# 在项目目录构建，再将项目只读挂载为测试容器的 /src
make build
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags=integration -o dist/sandboxprobe ./integration/sandboxprobe
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags=integration -o dist/workload ./integration/workload
# 以下在测试容器中执行
DEITYSIGHT_TEST_ARCH=arm64 sh /src/integration/systemd-check.sh
sh /src/integration/sandbox-check.sh
python3 /src/integration/load-check.py
python3 /src/integration/recovery-check.py
```

已观测到：

- 真实 `/proc` 读取包含 host/process/thread/cgroup；基础容器扫描约 1.6ms、原文约 53KB。此数值不代表大型宿主机。
- 服务实际生效的 `CPUQuotaPerSecUSec=200ms`、`MemoryMax=268435456`。
- 普通 2s/1s 任务完成 3 个采样点，返回可下载 `partial`；缺失接口和退出进程均有记录。
- 与服务相同配置的探针不能写配置、`/proc/sys`、`/sys` 和普通 `/tmp`；可写专用存储目录。
- 外部 `kill`/`tgkill` 与 ptrace 被拒绝；自身线程信号可用；可读取其他 UID 进程的 `io`。
- 100 个测试进程，每个额外锁定 8 个线程；6s/2s 任务观测到 99 个 PID、1,204 个 TID。
  在 20% CPU 配额下实际完成 2 轮、丢失 2 点、报告 2 次软超时；两轮分别 5,760/6,629 条记录。
  归档 419,108 字节，结束时内存约 26 MiB。归档 ETag 和内部数据文件 SHA-256 均校验通过。
  内存数值是结束时快照，不是峰值；可见进程数量和缺失比例会随机器状态变化。
- SIGKILL 发生在采样期间，另注入不完整尾行和未提交候选归档：重启后任务为 `interrupted`，
  保留 request_id、隔离损坏原件、重新生成可下载归档，不把候选文件当作完成结果，不补采。
- 将独立 8 MiB tmpfs 填至真实 ENOSPC：任务停止采样，无法打包时为 `failed` 且结果不可用，
  健康状态报告存储不可用；释放测试占用后可提交新任务。完全不可写时终态可能仅存在内存，符合恢复契约。

## 验证边界

- 原生 amd64 主机尚未实测。此环境的 amd64 仿真执行器对 seccomp TSYNC 返回 EINVAL；
  最终二进制因此拒绝启动。没有为兼容仿真而关闭过滤器。
- cgroup v1 已通过目录夹具测试；当前实际内核环境为 v2，未运行真实 v1 宿主机验收。
- 本开发环境未安装 atop，因此真实 atop 二进制调用测试被跳过；固定参数、白名单、失败和输出保留由单元测试覆盖。目标机启用前应先安装并单独运行 `atop -P ALL 1 1` 验证版本支持 parseable 输出。
- 存储只读故障的在线重新挂载被测试内核以 EBUSY 拒绝；实际 ENOSPC 路径已验证。
- 未建立所有发行版、内核、文件系统的兼容矩阵；未覆盖每一个 fsync/rename 指令点的掉电故障。
- 读取预算是软预算；单次内核读取无法由 Go context 强制中断。卡住时拒绝新任务，systemd 最终停止超时可终止进程。
- 数据量大时会返回缺失。默认配额不承诺完整扫描任意规模主机；须根据任务结果中的实际覆盖率调整预算。
- systemd 与 seccomp 提供分层限制，不能据此宣称对任意内核漏洞或任意新挂载传播具有完备隔离。
