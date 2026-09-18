# Deitysight 配置说明

`agent.example.yaml` 是可复制的配置模板。配置在 agent 启动时读取一次，运行期间不会热加载；修改后需要重启服务。YAML 使用严格字段校验，拼写未知字段会导致启动失败。

部署步骤：

```sh
install -d -m 0700 /etc/deitysight
install -m 0600 configs/agent.example.yaml /etc/deitysight/agent.yaml
vi /etc/deitysight/agent.yaml
/usr/local/bin/deitysight --config /etc/deitysight/agent.yaml
```

必须把 `http.token` 替换为真实 token。示例占位符会被拒绝；token 不会写入日志、任务元数据或结果包。

## 完整配置

```yaml
http:
  listen: "127.0.0.1:19100"
  token: "REPLACE_WITH_DEPLOYMENT_SECRET"
storage:
  path: "/var/lib/deitysight"
  result_retention: "24h"
  task_retention: "168h"
  max_bytes: 1073741824
  min_free_bytes: 1073741824
background:
  enabled: false
  step: "30s"
  retention: "10m"
sampling:
  default_window: "30s"
  default_step: "5s"
  max_window: "300s"
  min_step: "1s"
  max_points: 301
  round_timeout: "2s"
  max_source_bytes: 1048576
atop:
  enabled: false
  mode: "parseable"
  binary: "/usr/bin/atop"
  path: "/var/lib/deitysight/atop"
  interval: "1s"
```

## 字段说明

### `http`

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `127.0.0.1:19100` | 监听地址，必须是合法的 `host:port`。远程 server 访问时改为管理网地址。当前协议使用 HTTP。 |
| `token` | 无 | Bearer token。不能为空、不能带首尾空格或换行、不能保留 `REPLACE_WITH` 占位符。所有接口，包括 `/v1/health`，都要求鉴权。 |

### `storage`

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `path` | `/var/lib/deitysight` | agent 专用的绝对目录。不能使用 `/`、`/var`、`/tmp`、`/etc`、`/proc`、`/sys` 等宽泛或系统控制目录。agent 会创建目录并使用 `0700`/`0600` 权限。 |
| `result_retention` | `24h` | 任务结果归档保留时间。过期后删除归档和采样附件，但任务元数据仍可查询。 |
| `task_retention` | `168h` | 任务元数据和 request_id 幂等记录保留时间，不能短于 `result_retention`。 |
| `max_bytes` | `1073741824` | 存储预算上限，单位字节。原始 JSONL、后台历史、归档、临时文件和下载中的文件都计入。 |
| `min_free_bytes` | `1073741824` | 所在文件系统最低剩余空间，单位字节。低于该值时拒绝新任务或停止继续写入。 |

存储目录只能由一个 agent 实例使用。目录锁、任务元数据和结果文件都会持久化；不要把多个 agent 配置到同一个目录。

### `background`

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `false` | 是否持续进行低频后台采样。关闭时只在 server 提交任务后采样。 |
| `step` | `30s` | 后台采样间隔，必须为正时长。 |
| `retention` | `10m` | 后台历史保留窗口。按需任务开始时冻结可用历史并复制到任务归档。 |

后台采样和按需采样共用一个串行采集器；按需任务优先，不会并行启动第二轮采集。

### `sampling`

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `default_window` | `30s` | 请求省略窗口时使用的采样窗口。必须是整秒，不能超过 `max_window`。 |
| `default_step` | `5s` | 请求省略步长时使用的采样步长。必须是整秒，且不小于 `min_step`。 |
| `max_window` | `300s` | 请求允许的最大窗口。 |
| `min_step` | `1s` | 请求允许的最小步长。 |
| `max_points` | `301` | 单个任务最多计划采样点数。计划包含窗口起点和终点；例如 `30s/5s` 为 7 点。 |
| `round_timeout` | `2s` | 每轮采集软超时；实际使用 `min(round_timeout, step)`。超时会记录错误或丢点，不伪造零值。 |
| `max_source_bytes` | `1048576` | 单个数据源最大保留字节数，单位字节，最大允许 16 MiB。超出部分截断并标记不完整。 |

server 可以在任务请求中传入 `window_seconds` 和 `step_seconds` 覆盖默认值，但仍受上述限制约束。窗口和步长必须为正整数秒。

### `atop`

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `false` | 是否启用 atop 辅助快照。默认关闭，不影响 proc/cgroup 原始采集。 |
| `mode` | `parseable` | `parseable` 保存 `-P ALL` 文本输出；`raw` 使用 `-w` 保存 atop 原生二进制快照，并以 Base64 封装。 |
| `binary` | `/usr/bin/atop` | 只允许 `/bin/atop`、`/usr/bin/atop`、`/usr/sbin/atop`、`/usr/local/bin/atop`。不允许任意可执行文件路径。 |
| `path` | 空（使用 `storage.path`） | 仅 raw 模式使用的临时输出目录，必须位于 `storage.path` 内。每轮读取后删除临时文件，不作为长期 atop 日志目录。 |
| `interval` | `1s` | 无 task 上下文时的默认间隔。正常 HTTP task 采集时，atop 间隔始终使用该 task 的有效 `step_seconds`，因此 task 参数优先。 |

parseable 模式每轮直接执行固定调用：

```text
<binary> -P ALL <task_step_seconds> 1
```

agent 不经过 shell，也不接受配置中的额外命令参数。atop 只随按需 task 的采样轮次启动，后台采样不会启动 atop。task 的有效 `step_seconds` 覆盖 `atop.interval`；窗口决定 task 有多少轮 atop 记录。标准输出按 `/atop/parseable` 原样写入任务结果；退出失败、stderr、超时、空输出和截断都会单独记录。atop 是辅助数据源，不能替代 `/proc`、线程和 cgroup 采集。

raw 模式每轮执行等价于：

```text
<binary> -w <temporary-file-under-atop.path> <task_step_seconds> 1
```

原生二进制内容按 `/atop/raw` 保存为 Base64；临时文件在读取后删除，内容大小受 `sampling.max_source_bytes` 限制，最终 JSONL 与归档会计入存储预算。

启用前建议在目标 Linux 主机确认 atop 已安装且支持 parseable 输出：

```sh
/usr/bin/atop -P ALL 1 1
```

## 推荐配置

只需要按请求采集时保持默认配置：

```yaml
background:
  enabled: false
atop:
  enabled: false
```

需要低频历史和 atop 辅助快照时：

```yaml
background:
  enabled: true
  step: "30s"
  retention: "10m"
atop:
  enabled: true
  mode: "raw"
  binary: "/usr/bin/atop"
  path: "/var/lib/deitysight/atop"
  interval: "1s"
```

增大窗口、缩短步长或启用 atop 都会增加 CPU、磁盘和归档大小。建议先观察结果中的 `sampled_points`、`missed_points`、错误摘要和 manifest 的 source coverage，再调整预算。
