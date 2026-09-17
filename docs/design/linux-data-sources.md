# Linux 数据源核查与采集白名单

核查日期：2026-09-18。本文件依据 Linux 内核文档、man-pages、内核实现及 systemd 官方文档，
区分已核查的接口语义与本项目的实现方案。白名单与部署方案随
[技术设计](agent-technical-design.md) 于 2026-09-18 整体确认。
已在 Linux 容器执行真实采集和 systemd 集成验证，详见 [实施验证](../implementation-validation.md)。
这些结果不代表所有内核版本均提供相同字段。

## 1. 主机与进程数据源

只打开下面列出的状态和计数接口，按实际可用性记录；不递归复制整个 procfs 或 sysfs。

| 范围 | 读取的接口 | 用途及限制 |
| --- | --- | --- |
| 主机 CPU 与任务状态 | `/proc/stat`、`/proc/loadavg`、`/proc/uptime` | CPU 原始计数、运行/阻塞任务数量、负载与时间基准；不是具体进程的根因结论 |
| 主机内存 | `/proc/meminfo`、`/proc/vmstat` | 内存、回收、换页与缺页相关原始数据，保留单位 |
| 主机 I/O | `/proc/diskstats` | 设备级原始计数；不据此直接建立某个进程到设备的因果关系 |
| 资源压力 | `/proc/pressure/cpu`、`/proc/pressure/memory`、`/proc/pressure/io` | PSI 是否存在取决于内核功能和配置，缺失不能用零替代 |
| 进程 | `/proc/<pid>/stat`、`status`、`io`、`cgroup`、`wchan` | 身份、状态、资源计数、cgroup 归属与等待信息 |
| 线程 | `/proc/<pid>/task/<tid>/stat`、`status`、`io`、`wchan`、`cgroup` | 保留线程作用域和共享属性说明，不能把所有数值都当成线程独占资源 |
| 挂载和标识 | `/proc/self/mountinfo`、`/proc/sys/kernel/random/boot_id` | 解析当前挂载视图中的 cgroup 层级及主机启动身份 |

主机元信息另记录内核版本、架构、页大小与用户态时钟节拍，不能把内核 HZ 与进程计数的单位混为一谈。
CPU 和 I/O 累计值在 agent 端不求使用率；server 必须依据实际时间和各数据源语义解释。

上述最小白名单之外，命令行参数、smaps 详细页映射、文件描述符目标、内核栈、日志等均不默认纳入。
不读取进程内存、环境变量或业务文件正文，后续增加任何源都必须明确其用途和开销。

## 2. 已核查的进程与线程语义

### CPU 和共享内存不能重复累加

`/proc/<pid>/task/<tid>/` 中某些文件与进程级文件报告相同的共享属性，
其他文件则包含线程独立的状态。它们不是一组可直接相加的同质指标。

内核的进程级 stat 路径使用线程组 CPU 汇总，线程 stat 路径使用对应线程的 CPU 计数。
进程级 CPU 再加各线程 CPU 会重复计算。线程共享地址空间，线程 status 中的内存数值
也不能当作各线程独占内存再相加。原始记录必须标明 process/thread 作用域。

### I/O 存在不同计数口径

`/proc/<pid>/io` 的进程级口径包含线程组，并可能包含已经等待回收的子进程累计量；
线程级接口具有不同的计数作用域。因此既不能简单把进程与线程的 I/O 再相加，
也不能把进程级累计值当成这个进程当前独立的磁盘吞吐。

`rchar/wchar` 是相关系统调用层面的字节计数，`read_bytes/write_bytes` 是存储层相关计数。
缓存等因素使二者含义不同，文件系统、内核和计数更新方式也可能影响解释。
读取 io 受到 `PTRACE_MODE_READ_FSCREDS` 权限检查；root 与服务沙箱的组合仍需要验证。

### PID 和等待信息

stat 的启动时间字段与 boot_id、PID 共同用于避免把复用的 PID 当成同一对象。
采集不是原子快照；进程退出或身份改变时必须明确标记，不能拼接不同生命周期的数据。
这个组合是工程关联标识，不是内核承诺绝对无碰撞的永久身份；启动计数单位为用户态时钟节拍。
RSS 的单位为页，手册说明其数值可能不精确，原始数据完整不等于获得了精确的物理内存归属。
主线程退出后，task 目录的内容可能不可用，必须记录线程枚举限制。

`wchan` 返回 0 具有歧义：可能未取得等待地址，也可能受权限或符号解析限制。
不能把 0 解释为“没有等待”，也不能仅凭 D 状态认定正在等待本地磁盘。
这些读取不会附加调试器，但读取权限与内核配置依然影响其可见性。
stat 中标为 `[PT]` 的字段也可能在访问检查未通过时显示零，应附带来源语义，不能将原文中每个零都当作正常观测。

## 3. cgroup 数据源

根据 mountinfo 和进程 cgroup 记录选择实际挂载层级，不能把 v1、v2 路径硬套到同一种布局。
读取实际关联的 cgroup，并沿其可见祖先链收集相关限制，避免只看到叶节点的无限额设置。
相同 cgroup 在一轮内只读取一次，通过引用关联多个进程。

| 控制器 | cgroup v2 | cgroup v1 |
| --- | --- | --- |
| CPU 使用和限流 | `cpu.stat`、`cpu.max`、`cpu.weight` | `cpuacct.usage`、`cpuacct.stat`、`cpu.stat`、`cpu.cfs_quota_us`、`cpu.cfs_period_us`、`cpu.shares` |
| CPU 集合 | `cpuset.cpus.effective` | `cpuset.cpus`，有效集合文件按可用性读取 |
| 内存 | `memory.current`、`memory.stat`、`memory.max`、`memory.high`、`memory.events` | `memory.usage_in_bytes`、`memory.limit_in_bytes`、`memory.stat`、`memory.failcnt` |
| I/O | `io.stat`、`io.max` | `blkio.throttle.io_service_bytes`、`blkio.throttle.io_serviced`、`blkio.throttle.read_bps_device`、`blkio.throttle.write_bps_device` |
| 压力和成员关系 | 可用时读取 `cpu.pressure`、`memory.pressure`、`io.pressure`、`cgroup.events` | 控制器与内核实现不同，不伪造 v2 对应字段 |

这是按实际适用性读取的白名单，不代表每个目录都有这些文件。具体内核版本、根 cgroup、控制器启用情况、
块设备和 I/O 策略都会影响文件及统计可用性；不支持、无权限与对象消失需分别记录。

v1 `cpuacct.usage` 单位为纳秒；不能与其他 CPU 计数无条件混用。
具有层级统计语义的 cgroup 字段会包含后代，父子 cgroup 不应直接累加。
同样也不能把 cgroup 总量再与成员进程总量相加；部分字段的 local 与 hierarchical 语义必须分别解释。

## 4. systemd 沙箱的可见性与只读约束

已核查的关键限制：

- `ProtectSystem=strict` 不自动覆盖 `/dev`、`/proc`、`/sys`，不能单靠它保证内核接口只读。
- `ReadOnlyPaths=` 可保留读取并限制写入，但官方文档说明了子挂载传播等限制；
  具有足够特权的进程也可能撤销挂载保护，需要限制挂载能力和相关系统调用。
- `ProtectProc=invisible/noaccess` 会限制跨用户进程可见性或访问；root 和相关 capability 可能存在例外。
  不能依赖这些例外来保证采集完整性，也不能无提示地隐藏其他用户的进程。
- `ProcSubset=pid` 会隐藏非进程的 procfs 接口，与主机级资源采集冲突；本项目应保留 `ProcSubset=all`。

本项目在服务模板中采用 `ProtectSystem=strict`，仅对专用存储目录提供写入例外，
并为 `/proc`、`/sys` 选择经过验证的只读保护，配合 `NoNewPrivileges`、capability 与 syscall 限制。
不以 `ProtectProc` 的跨用户隐藏模式作为完整采集器的默认配置。

跨用户读取 procfs 某些字段可能需要与 ptrace 权限检查相关的 capability；这不意味着允许附加调试。
权限、文件系统保护与禁止进程控制必须分别验证。禁止 ptrace、目标进程内存写入、信号控制和探针
所需的过滤规则必须兼容 Go 运行时自身行为，不能直接宣称一个 capability 列表就提供全部保证。

部署验收需要同时验证：能够读取允许的数据、不能修改目标配置/进程/内核控制入口、
只能在专用目录保存自身数据，以及服务重启后可恢复已有任务。
实施验证已覆盖固定配置下的可见性、写入限制、进程控制限制和重启恢复；
不能将单个环境的结果当作所有发行版或内核的隔离保证。

## 5. 参考来源

本轮实际读取并用于上述核心结论的来源如下。部分 docs.kernel.org 页面下载超时，改读 Linux 仓库中的原始文档。

- proc_pid_task(5)：https://man7.org/linux/man-pages/man5/proc_pid_task.5.html
- proc_pid_stat(5)：https://man7.org/linux/man-pages/man5/proc_pid_stat.5.html
- proc_pid_io(5)：https://man7.org/linux/man-pages/man5/proc_pid_io.5.html
- 进程/线程 stat 实现：https://raw.githubusercontent.com/torvalds/linux/master/fs/proc/array.c
- io 与 wchan 实现：https://raw.githubusercontent.com/torvalds/linux/master/fs/proc/base.c
- cgroup v2：https://raw.githubusercontent.com/torvalds/linux/master/Documentation/admin-guide/cgroup-v2.rst
- cgroup v1 CPU accounting：https://raw.githubusercontent.com/torvalds/linux/master/Documentation/admin-guide/cgroup-v1/cpuacct.rst
- cgroup v1 memory：https://raw.githubusercontent.com/torvalds/linux/master/Documentation/admin-guide/cgroup-v1/memory.rst
- cgroup v1 block I/O：https://raw.githubusercontent.com/torvalds/linux/master/Documentation/admin-guide/cgroup-v1/blkio-controller.rst
- CFS bandwidth：https://raw.githubusercontent.com/torvalds/linux/master/Documentation/scheduler/sched-bwc.rst
- systemd.exec：https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html
- systemd.exec 原文：https://raw.githubusercontent.com/systemd/systemd/main/man/systemd.exec.xml

PSI 详细字段、status 全字段、v1 cpuset 全部文件及 systemd 最低版本兼容矩阵未在本轮完整核查，
白名单中的这些项需要在实现时继续核对；参考入口为 https://docs.kernel.org/filesystems/proc.html
和 https://docs.kernel.org/accounting/psi.html 。
源码链接指向滚动分支；实现时应以目标内核及系统配置做集成验证，不能将最新文档当作所有发行版的保证。
