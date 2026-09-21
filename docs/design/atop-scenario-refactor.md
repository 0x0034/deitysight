# 以 atop 为主的场景采集改造

状态：Q1–Q17 已确认，2026-09-22；实现与回归结果见 docs/atop-validation.md。本设计替代旧版 proc/cgroup 加可选 atop 的采集方案。

## 已明确方向

本轮目标仓库为 deitysight。用户要求修改现有实现，强依赖 atop 获取调查证据，并按 CPU、I/O、内存、网络拆解场景。场景术语见根目录 CONTEXT.md。

## 现场证据

- 现有全量 proc/线程采集在约 8 万个调度实体的主机上，每轮 2 秒预算耗尽，仅覆盖低 PID 内核线程及 systemd，业务进程未被覆盖。
- 开启 atop 后，远端任务停在第一轮，健康接口返回 collector_stalled=true。本地 Linux 隔离复现确认 agent 的 seccomp 阻止 CommandContext 终止自己的超时子进程；这证明超时回收缺陷，不证明远端 atop 迟迟不退出的具体原因。
- 独立 atop -P DSK,PRG,PRD 5 13 采集产生 13 个完整帧。2026-09-21 23:31:01–23:32:18（北京时间），排除首帧 RESET 后有 12 个区间，共 77 秒。
- 该文件定位 databus PID 287969 为主要存储读取来源，约 2521.43 MiB/s，与同期 sda 2541.86 MiB/s 高度吻合。证明 atop 证据具备进程归因价值，但不证明更早告警时段或业务行为的正常性。
- 文件约 274 MiB，单帧约 20.78–22.11 MiB，含约 7.7 万条线程记录。当前单源默认 1 MiB、配置最大 16 MiB，不能直接容纳这种帧。
- 现场 DSK 输出不含计算平均读写延迟和平均队列长度所需的全部字段，需按实际 atop 版本核查能力。

## 已核实的实现缺口

- 每轮重新启动 atop，样本数固定为 1，需要正确识别启动以来累计帧，不能将其当作一个步长内的增量。
- parseable runner 先用 bytes.Buffer 收集全部 stdout/stderr，返回后才执行单源截断，不能约束采集过程中的内存占用。
- atop 成功记录未设置 kind=source，但任务成功源计数依赖该值；改为主数据源后必须统一记录与任务终态语义。
- 子进程取消策略与禁止进程控制的安全边界需要共同设计：被调查业务进程与 agent 自己创建的采集子进程应明确区分。

## 已确认决策

- Q1：agent 不计算指标、不排名、不做归因；负责采集、整理与完整性报告，server 承担后续分析。沿用既有 agent/server 职责边界。

- Q2：采集证据只使用 atop 输出，不额外读取 proc、sysfs 或 cgroup 文件补充调查数据；不可用信息明确报告，不能当成零。
- Q3：单任务允许选择多个场景，共享一次窗口采样。
- Q4：atop 缺失或版本不支持时保留 HTTP 查询及旧结果下载，健康状态不可就绪，拒绝新采集；不退回原 proc 采集。
- Q5：固定使用 atop 2.7.1。采用该版本支持的 parseable 输出，不使用 JSON。按 v2.7.1 实际能力验收，不承诺独立读写 await、当前未完成 I/O 数或平均队列长度。

- Q6：默认保存全部可见进程，线程明细由任务显式开启；不按 Top N 筛选。输出裁剪不等于减少 atop 内部线程扫描成本。
- Q7：允许 agent 做格式解析与字段裁剪，删除 PRG 命令行字段后再持久化，保留进程身份、名称及可用归属；不计算资源指标。
- Q8：接受网络场景只提供主机网络数据；进程网络只在管理员预部署且与 atop 2.7.1 兼容的插件可用时提供。agent 不安装或加载探针，缺失能力明确报告。
- Q9：默认设置 ATOPACCT='' 禁用进程会计，避免自动启用系统会计；明确采样间短命进程可能不可见。
- Q10：允许启停和回收 agent 自己创建的采集子进程，继续禁止控制被调查的业务进程；保留隔离与权限约束，不整体关闭沙箱。

## 官方能力核查（版本能力依据）

核查来源：Atoptool/atop 官方版本源码与 man/atop.1。

- v2.7.1 无 JSON，v2.8.0 开始有 -J；本项目已选 v2.7.1，采用 parseable 输出。
- parseable 使用 RESET 标记累计首帧、SEP 标记帧结束。v2.13 JSON 输出没有同样的 RESET 标志。
- v2.7.1 DSK 无 in-flight/平均队列字段，v2.8.0 增加；v2.13 的 parseable/JSON 仍不提供独立读写 await 所需字段。busy 时间除 I/O 次数不是 r_await/w_await。
- PRG/PRD 等 parseable 输出遍历 taskall。不能用交互显示开关 -y 承诺只输出进程；流式过滤 is_process 可缩小证据包，但不消除 atop 内部线程扫描。
- PRG 输出命令行，标签选择不提供字段级删除；-Z 仅改变空格表示，不是脱敏。
- 没有网络插件时 PRN 进程计数不可解释为有效零值；NET 可提供主机接口/协议栈数据。v2.12/v2.13 需 -K 检测网络插件，但本项目已选 v2.7.1，不套用这些版本的参数或插件支持承诺。读取预部署 netatop-bpf 使用本地 socket，传统 netatop 涉及 raw socket 权限；插件支持须在固定版本真实验收。
- ATOPACCT 为空字符串可禁用进程会计，避免 root atop 自动启用系统会计；代价是采样间退出的短命进程可见性受限。

参考：

- https://github.com/Atoptool/atop/blob/v2.13.0/man/atop.1
- https://github.com/Atoptool/atop/blob/v2.13.0/parseable.c
- https://github.com/Atoptool/atop/blob/v2.13.0/json.c
- https://github.com/Atoptool/atop/blob/v2.13.0/netatopbpfif.c
- https://github.com/Atoptool/atop/blob/v2.7.1/parseable.c
- https://github.com/Atoptool/atop/blob/v2.8.0/parseable.c

## 后续已确认决策

- Q11：四场景标准证据范围；scenes 多选，省略时全部四类；include_threads 默认 false；默认不开启 PSS 扫描。
- Q12：新结果采用 schema v2、保留已有任务与旧结果下载；逐帧保存经命令行裁剪的 atop 字段原值，不继续生成含命令行的原生 raw。
- Q13：一个持续 atop 会话，有界窗口与有限启动/收尾宽限；实际时间、累计首帧与缺失明确报告，不无限等待凑齐帧数。
- Q14：保留可选后台 atop 历史，默认关闭；按需任务优先，单采集会话，切换间隙明确报告。
- Q15：能力不可用单独声明；基础证据完整可 completed，执行丢失为 partial，无可用证据为 failed。
- Q16：允许将 agent 与 atop 合计的初始部署预算调整为 1 核 CPU、1 GiB 内存，再经实测校准；磁盘沿用现有预算。

## 已授权的场景回归环境

用户于本轮明确授权通过 ssh root@R9 使用该虚拟机进行场景回归。已成功连接并完成只读基线核查：Rocky Linux 9.5、Linux 5.10.134-18.an8.aarch64、arm64、4 个逻辑 CPU、约 7.5 GiB 内存；核查时可用内存约 1.7 GiB，/tmp 所在文件系统可用约 89 GiB。

初次核查时未安装 atop，未发现 stress-ng/fio/iperf3/Go，已安装 python3；deitysight systemd 服务不存在。后续回归将准备固定 atop 2.7.1 与待验收 agent，使用有时限、资源受控的 CPU/I/O/内存/本地网络负载；不把该虚拟机的基础功能回归当作约 8 万线程生产规模的性能验收。Q11–Q16 已确认；该虚拟机仅用于受控回归。

## atop 2.7.1 追加核查

- 已确认该版本标签包含 PSI，具有独立 y/n 支持标志；CPU some、内存与 I/O some/full 可按实际支持报告，不能把不支持时的零值解释为无压力。
- 该版本仅核实传统 netatop 路径，不支持 netatop-bpf；不能套用新版 -K 参数。netatopd 存在时退出流程会尝试对既有 daemon 发送 SIGHUP，这超出已批准的自身子进程控制范围，沙箱必须继续阻止，并验证退出不会受阻。
- 该版本基于 alarm 触发采样，不是每次扫描后固定再睡 step 秒；延迟可能来自扫描、调度、限流与输出背压。实际窗口与帧数契约遵循已确认的 Q13。

补充来源：https://github.com/Atoptool/atop/blob/v2.7.1/atop.c ，https://github.com/Atoptool/atop/blob/v2.7.1/netatopif.c ，https://github.com/Atoptool/atop/blob/v2.7.1/acctproc.c 。

## Q17：专用非 root 身份（已确认）

使用专用 deitysight 账号，禁止任何业务进程复用此 UID。agent 与 atop 仅持有 CAP_DAC_READ_SEARCH、CAP_SYS_PTRACE；不授予 CAP_KILL、CAP_SETUID 或 CAP_NET_RAW。seccomp 保留 ptrace、外部线程信号、动态探针与进程内存系统调用限制，仅放行 signal 0（Go pidfd 能力检测）及 SIGKILL（回收采集子进程）。内核 UID 权限检查阻止向其他账号业务进程发信号；相同 UID 的其他 agent 实例在同一信号权限域内，因此该账号只能运行采集器。

systemd 提供只读挂载、NoNewPrivileges、capability bounding/ambient 集与 1 CPU/1 GiB 共享资源预算。root 身份、UID 不一致或能力集不符合约束时拒绝启动。传统 netatop 插件可能因 raw socket 权限约束不可用，不自动放宽权限或向其 daemon 发信号。

运行时不回退至 proc/cgroup 采集；旧归档按原格式继续下载。旧后台历史不会混入 v2 新任务。旧 raw 配置可保持诊断接口，但不接受新任务；迁移时必须设为 parseable。
