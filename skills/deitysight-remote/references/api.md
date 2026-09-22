# 远程接口与结果获取

适用于当前 `/v1` 接口及可选 S3 转存。部署可能尚未升级；以实际 JSON 字段和 manifest 为准，不能只凭 agent.version 推断所有可选能力已开启。

## 连接与鉴权

`DEITYSIGHT_ENDPOINT` 是用户提供的 agent origin，例如 `http://agent.example.internal:19100`，不含用户名、密码或 token 查询参数。agent 原生使用 HTTP；只有用户部署了 HTTPS 入口时才使用对应 HTTPS URL。此处示例域名不是真实主机。

所有 agent API 都需要 `Authorization: Bearer <token>`。使用工具的受保护 header 机制，或在调用进程内部读取环境变量/受限文件；日志只展示无令牌模板。不要将 token 或 S3 URL 展开成 curl 等外部程序的命令参数，因为关闭 xtrace 也不能防止本机 argv 暴露。

以下 Python 3 标准库示例仅查询健康状态，凭据在进程内部读取，不进入外部命令参数。环境变量应通过调用环境安全提供：

```python
import json
import os
import urllib.error
import urllib.request

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None

endpoint = os.environ["DEITYSIGHT_ENDPOINT"].rstrip("/")
token = os.environ["DEITYSIGHT_TOKEN"]
opener = urllib.request.build_opener(NoRedirect())

def api_json(method, path, payload=None):
    request = urllib.request.Request(
        endpoint + path,
        data=json.dumps(payload).encode() if payload is not None else None,
        method=method,
        headers={"Authorization": "Bearer " + token,
                 "Content-Type": "application/json"},
    )
    try:
        response = opener.open(request, timeout=15)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.code, json.load(response)

status, health = api_json("GET", "/v1/health")
print(status, health)
```

为请求设置连接/读取超时，读取 HTTP 状态与 JSON 错误正文。对带鉴权的请求不盲目跟随重定向；不能把 agent token 转发给另一 origin 或 S3。

## 四个接口

| 方法与路径 | 用途 |
| --- | --- |
| GET `/v1/health` | 查询 agent/atop、存储、执行器是否能接受新任务 |
| POST `/v1/tasks` | 提交采集请求，202 新建，200 幂等重放 |
| GET `/v1/tasks/{task_id}` | 查询采样、结果和可选 S3 状态 |
| GET `/v1/tasks/{task_id}/result` | 下载原始 tar.gz；仍是本地下载，不重定向到 S3 |

没有 list-tasks、取消、删除、修改配置、独立触发上传或查询完整配置的 API。已有 task_id 在 health 503 时仍可尝试查询和下载。`health.accepting_tasks=false` 不总是 HTTP 503：已有任务或磁盘不足时也可能返回 HTTP 200。

提交示例；先生成本次唯一的 request_id，再将该 JSON 保存为不含凭据的 `request.json`：

```json
{
  "request_id": "investigation-<本次唯一ID>",
  "window_seconds": 30,
  "step_seconds": 5,
  "scenes": ["cpu", "io", "mem", "network"],
  "include_threads": false
}
```

授权新采集后，可在上述同一 Python 调用脚本中使用：

```python
with open("request.json", encoding="utf-8") as file:
    payload = json.load(file)
status, task = api_json("POST", "/v1/tasks", payload)
print(status, {key: task.get(key) for key in ("task_id", "state", "reused", "error")})
```

| 字段 | 约束 |
| --- | --- |
| request_id | 必填，1–128 字节 UTF-8；同一逻辑请求的网络重试始终复用 |
| window_seconds | 可省略，正整数秒；部署默认 30，上限默认 300 |
| step_seconds | 可省略，正整数秒，不大于窗口；部署默认 5、下限默认 1 |
| scenes | 可省略，默认四类；只接受 cpu/io/mem/network，显式空数组、重复项和未知值无效 |
| include_threads | 可省略，默认 false；保留全部可见进程，true 才额外保留线程记录 |

默认最大计划帧数 301；`ceil(window/step)+1` 包含 RESET 基线，非整除窗口末帧可能在窗口之后。请求体最多 8 KiB；未知字段、重复 JSON 键、null 和非整数时间均无效。没有 S3、atop 参数或命令参数的 task 覆盖字段。

相同 request_id 在任务记录保留期内复用原任务；省略的参数沿用首次有效值，显式改变窗口、步长、场景或线程开关返回冲突。终态后的网络重试不产生新调查。查询超过任务保留期后，原 ID 的幂等保证已结束，不要未经判断就重发 POST。

若首次请求是否到达尚不确定，省略参数可能创建使用部署默认值的新任务；必须恢复原请求体，不能用“省略即可沿用”绕过缺失的原参数。

## 状态与错误

| HTTP / 错误码 | 调用行为 |
| --- | --- |
| 401 unauthorized | 停止认证重试，核对 endpoint 与令牌来源；不猜 token |
| 400 invalid_request / 413 request_too_large | 检查字段、单位和部署限制；不把参数错误当成主机异常 |
| 409 request_conflict | 原 ID 已对应不同参数；要接续则用原参数，要新调查才生成新 ID |
| 409 agent_busy | 未接受新任务；不取消或接管别人的 task。用户要求等待时，在有界预算内用同一请求重试 |
| 409 result_not_ready | 查询同一 task，等待采样/打包结束 |
| 409 result_unavailable | 没有可下载的本地归档；检查 errors 和独立的 S3 结果 |
| 410 result_expired | 本地结果已过期；检查已上传的 S3 链接，不重新采样来伪装旧结果 |
| 404 task_not_found | ID 不存在或任务记录过期；核对 ID，保留“无法查询”事实 |
| 503 atop_unavailable / agent_unavailable | 新采集不可执行；报告健康原因，已有结果路径仍可用 |
| 507 storage_unavailable | agent 本地存储不足/不可写；不代替用户删数据或改配额 |

错误正文：`{"error":{"code":"...","message":"..."}}`。网络失败后结果不确定时保留原 request_id。连续有限重试仍失败，则交付 request_id/task_id 与阻塞点；只重试用户授权的操作。

task.state 的 running 可能处于 sampling 或 packaging；completed、partial、failed、interrupted 均为采集终态。能否下载取决于 result.available，不能只看 state。记录 planned_points、sampled_points、missed_points、errors、capabilities、limitations 和真实时间字段。

## 本地下载

固定请求 agent 的 `/v1/tasks/{task_id}/result`，避免将未经检查的任意 result.url 与 Bearer token 拼接。

使用带原 agent Bearer header 的 HTTP GET，将二进制响应流式写入本次工作目录的 result.tar.gz，不调用上例的 JSON 解码函数。依据已知大小设置本次下载时限（可先用 120 秒）和实际字节预算，避免无限等待或将完整包加载进内存。

下载完成验证 `result.size`、`result.sha256`；本地接口 ETag 是归档 SHA-256。超时预算可按已知包大小合理调整，下载中断时仍重试同一结果。核验步骤见 [evidence.md](evidence.md)。

## S3 链接与下载

S3 由 agent 配置决定，调用方不需要 access_key_id/secret_access_key。`result.s3` 缺失表示该任务没有转存记录或部署尚不支持该字段，不要假设会自动补传旧任务。

`result.s3.provider: yos` 表示使用内部 YOS，沿用同一状态和下载流程；provider 缺失时是标准 S3。YOS 的 bucket 可能是含斜线的 namespace/key，返回的 url 是 COS HTTPS 直链，按原值下载即可。`allow_http: true` 只描述 agent 到内部网关的配置；下载方仍验证返回链接的 HTTPS 证书。对象 key 是不透明标识，YOS 文件名中的摘要不能替代 result.sha256。`yos_object_too_large` 表示归档超出 YOS 单包限制，此次转存 unavailable，可在本地结果有效期内下载原归档。

| result.s3 | 含义与行为 |
| --- | --- |
| state=waiting_result | 等待本地最终归档 |
| pending / uploading | 等待或进行上传；独立于采集状态 |
| retry_wait | 按 next_attempt_at 观察，last_error_code 是脱敏原因；无需重发采集任务 |
| uploaded 且 url 存在 | 已确认转存，使用该完整 URL 下载 |
| uploaded 但无 url | 查看 url_error_code，可能是签名配置缺失或目标变更；不自行拼接公共 URL |
| expired / unavailable | 未在期限内上传成功或源归档不可用；解释结果缺口 |
| paused=true | 开关关闭或目标配置不匹配；重复查询不会解除暂停 |

一次新查询确认 `s3_target_changed` 或 `signing_unavailable` 后，停止自动轮询该配置阻塞，报告具体原因；继续等待不会自动修复配置。

成功结果还包含 bucket、key、sha256、size、uploaded_at、url_expires_at。URL 在查询时生成，默认有效 1 小时；临时凭据或外部策略可能令它更早失效。uploaded 只证明曾经成功上传，对象可能已被 bucket 生命周期或管理员删除。

S3 下载使用独立 HTTP GET，不携带 agent Bearer token，也不需要本地 AWS 配置。程序内部从 task 响应或受保护变量中读取完整 URL，按 HTTPS 正常校验证书；流式保存到本次工作目录并执行与本地下载相同的时限和实际字节预算。不要通过 `curl "$S3_URL"` 将签名凭证暴露在 argv。

签名失败/过期时，若能访问 agent 且任务记录未过期，重新 GET 原 task 取得新链接；办公侧不能访问 agent 时，由能访问的一方提供新链接。不要将 S3 403 一概归因于过期，也不要修改已签名 URL 的域名/路径/参数。

本地归档默认保留 24h，任务记录默认 7 天，实际部署可调整。上传成功后，本地 410 与 S3 可用可以同时成立；任务记录过期后 agent 不再刷新链接。S3 保留由 bucket 管理。下载后用 s3.sha256/size 或同一 task 的 result.sha256/size 校验，S3 ETag（包括 multipart ETag）不是完整文件 SHA-256。

若用户仅提供 S3 链接，没有 task 响应/外部摘要，可以下载并核验归档内部清单，但必须区分“内部清单一致”与“外层包已用独立摘要验证”。
