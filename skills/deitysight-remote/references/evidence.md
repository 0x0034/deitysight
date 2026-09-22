# atop 证据核验与解释

本规则适用于 schema v2、atop 2.7.1。先看 manifest.schema_version、host.atop_version 和 source_semantics；旧版 v1、未知版本或原生 raw 包需要相应格式说明，不能套用 v2 规则。当前 agent 新任务输出 parseable JSONL，不输出原生 atop `-w` raw。

## 校验归档

1. 下载前读取已知 size 并检查本地剩余空间，流式下载。先比对完整归档字节长度及独立提供的 SHA-256；摘要不符时停止用该包下结论。
2. 流式列举 tar.gz，只读取 manifest.json、samples.jsonl、errors.jsonl 和可选 history.jsonl。拒绝路径穿越、绝对路径、重复成员名、符号/硬链接及设备类型，避免无条件 extractall。
3. 核对 manifest.files 中各文件的实际长度和 SHA-256；这些摘要验证内部一致性，不能独立证明无外层摘要的包来自预期主机。
4. 核对 task_id/request_id、agent_id、host.hostname、场景及 actual timestamps。与用户指定对象不一致时停止对请求目标的归因，报告身份差异；未提供预期身份则注明核验范围。时间遵循被调查主机的时钟，明确标注时区和可能的时钟偏差。

操作前设置调用端资源预算。用户没有指定时，可采用压缩包 256 MiB、解压总量 2 GiB、单成员 1 GiB、成员数 16 的保守默认值；这些不是 agent 服务端限制。结合声明大小和本地空间可明确调整预算。下载与解压均按实际字节持续计数，不能只信 Content-Length 或 tar 头；超额时停止并报告，不把截断内容当完整证据。只分析允许成员，未消费的其他成员也计入解压总量，避免绕过预算。

将 request.json、去掉签名 URL 的任务摘要、原始归档、计算脚本和分析报告放在本次独立目录；目录只供当前用户访问。完整 task 响应可能包含预签名凭证，不应直接作为长期日志保存。用户明确要求“重新分析”时，不从旧报告选择嫌疑进程或拼入旧观测值。

## 选择可用帧

`samples.jsonl` 是记录流，不是一行一个完整 snapshot。关键字段：

| 字段 | 用法 |
| --- | --- |
| schema_version / kind / source | v2 source 为 atop/CPU、atop/PRD 等；frame_end 的 source 为 atop/SEP |
| sample_id | 同一帧的关联 ID |
| epoch / interval_seconds | atop 观测时刻与实际区间秒数 |
| baseline | RESET 基线，通常包含开机以来累计量 |
| complete | 单条记录或帧提交标记的完整性；不能单靠某条 source 的 true 判断整帧完整 |
| supported | false 代表能力不可用，输出零不能当测得值 |
| scope / object | process/thread 与 PID、TID 等身份；启动时间通常来自同帧 PRG |
| content_encoding / content | 解码后得到原始 parseable 行；v2 通常为 utf8 |

按流顺序临时收集同一 sample_id 的 source；遇到该 ID 的 `kind=frame_end && complete=true` 才提交整帧。核对同帧 epoch、interval_seconds、hostname 一致；丢弃没有 SEP 的尾帧或 ID 不匹配的数据，保留缺失记录。将计算出的完整帧数与 task.sampled_points 对照，不符时说明差异。

RESET 帧不参与速率或本次区间计数求和。RSS、free 等即时值可用作基线端点。后续帧已是 atop 区间值，直接除以该帧的真实 interval_seconds；不要再差分这些区间计数，也不要除以请求中的 step_seconds。只有累计计数确实由相应格式定义为累计时才差分。

history.jsonl 可能由多个独立会话组成，存在 RESET 与中断空隙；按会话、基线和时间单独处理。不能把两个不连续 task 或历史片段拼成连续趋势。没有完整非基线帧时只能报告即时状态。一个可信的完整非基线帧已经包含区间计数，可计算该区间速率，但不足以建立持续趋势；需要累计值差分或端点增长时才要求有效的两端点。

## 进程身份与统计口径

默认按同帧 PRG 的 PID、start_time_epoch 关联 PRC/PRM/PRD/PRN。只凭 PID 或进程名无法安全跨重启/复用关联。当前没有 host boot ID 或完整 cgroup/Pod 归属，容器短标识不是 Pod 名。

汇总进程时只使用 scope=process/is_process=y；线程用于钻取，不与进程聚合量再次相加。进程名及 `()` 包围的字段可能含空格和括号，解析 PRG/其他进程标签时不能简单对整行 split 后使用固定偏移。PRG 命令行已被替换为 `()`，不能据此还原可执行路径或业务参数。

需要字段序号时核对官方固定版本的 [parseable.c](https://github.com/Atoptool/atop/blob/v2.7.1/parseable.c) 和 [atop.1](https://github.com/Atoptool/atop/blob/v2.7.1/man/atop.1)。这是格式参考，不是额外主机数据源；不能套用最新版 atop 的列位置。

## 分场景解释

| 场景 | 标签与计算 | 结论边界 |
| --- | --- | --- |
| CPU | CPU/cpu 的时间使用 hertz；PRC 的 `(utime+stime)/hertz/实际秒数` 为平均占用核数；乘 100% 是“单核=100%”口径。CPL 提供负载值 | 整机占用与单进程单核口径分开；guest 时间可能已包含在 user，避免重复计入。进程 runqueue delay 为纳秒，不能当 CPU 执行时间 |
| I/O | DSK busy 是毫秒，`busy_ms/(秒数*1000)*100%`；DSK/PRD 扇区均为 512 字节；区间扇区直接换算吞吐 | PRD 是进程总体 I/O，可能包含回收子进程计数，不是按设备拆分；busy 高不等于已证明延迟瓶颈；本版本没有独立读写 await 或平均队列 |
| 内存 | MEM/SWP/PAG 根据各行 pagesize 换算；PRM 内存量为 KiB，RSS 是即时值，growth/faults 为区间值 | free 不是 MemAvailable；RSS 共享不可简单相加；PSS 不可用；短窗口上涨不能直接判定泄漏，结合进程启动时间 |
| 网络 | NET 接口字节按实际区间换算；仅在 PRN supported=true 时分析进程网络 | bond、成员网卡、macvlan/虚拟接口可能重复计数；协议层数据不能代表所有容器网络命名空间；缺少 PRN 时无法进程归因 |

跨多帧求平均速率，用区间计数总和除以有效秒数总和，不直接平均不同长度区间的速率。CPU 总体占用用对应 tick 总量加权；说明是否将 iowait/steal 包含在所称“忙碌”中。

额外易误读项：

- PSI supported=false 不能解释为无压力；基础证据完整但可选能力缺失的 task 仍可能 completed。
- PRG 的 nthrslpu 在 atop 2.7.1 中包含 D 与 I（空闲内核线程），不是纯“等待磁盘线程数”。
- PAG pgscans 聚合 pgscan_*，可能包含重叠计数；不能当成扫描的唯一页数。负值能力哨兵不当作正常零。
- 没有进程会计，采样间退出的短命进程可能缺失；include_threads=false 只减少输出，不能消除 atop 内部扫描线程开销。
- 调查本身消耗 CPU、内存和写入；识别 atop、deitysight 进程，避免当成原业务异常。不要凭它们的 CPU 占用自行证明 cgroup 配额限流。

## 报告的证据边界

给出实际主机、时间、完整区间数和任务状态，再呈现各资源的观测与相关进程。保留关键计数/单位、计算口径、PID+启动身份以及可见容器标识，提供归档和可复核计算脚本路径。

将“主要读进程”与“为什么大量读取”分开：只有 atop 时通常不能确认文件路径、读取位点、业务重扫、Pod 归属、完整 cgroup 限额、GC 或网络请求来源。针对未知项说明还需什么证据，是否继续采集由用户任务范围决定；不自动登录主机或接入监控、日志平台补齐。
