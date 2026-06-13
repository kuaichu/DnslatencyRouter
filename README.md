# DNS Latency Router

周期性解析目标域名的全部候选 IP，按延迟、抖动、丢包率综合评分，结合切换阈值与稳定窗口，自动把 Cloudflare DNS A 记录切到更稳的节点。自带一个偏运维控制台风格的 Web 仪表盘。

这个项目特别适合代理场景：客户端连接 `custom_domain`，TLS SNI 覆写为 `target_domain`，这样入口域名不变，但底层会持续指向当前更优的出口 IP。

## 核心能力

- 多 DNS 解析目标域名，尽量发现更多真实候选 IP
- 可按运营商策略选择解析 DNS 池，`auto` 会根据探测源自动推断联通/电信/移动
- 支持 `ICMP` / `TCP` 两种探测方式
- 每轮可多次探测同一 IP，计算平均延迟、抖动、丢包率与综合分
- 不是看到更低延迟就立即切换，而是带有 `阈值 + 稳定时长` 的防抖策略
- 支持按本地时间窗口对指定 IDC / ISP 动态加权，避免深夜抖动节点误切换
- 支持多机场入口：每个机场可配置独立入口域名、缩写、探测源与运营商策略
- 支持按 IP 归属地拆分地区记录，例如 `airport-a-hk.example.net`、`airport-a-my.example.net`
- 支持主控 + Agent 子机模式：电信、联通、移动机器可在各自网络里探测并上报结果
- Cloudflare API 支持走代理
- Web 仪表盘支持在线修改主要配置并即时生效
- Web 仪表盘内置全球 SVG 国旗资源，地区卡片不依赖系统 Emoji 或外部 CDN
- 日志、检测历史、IP 样本会持久化保存最近 7 天
- 支持 IP 生命周期管理：游离 IP 不会立刻删除，而是降级展示并在窗口期后自动淘汰

## 工作原理

```text
                         ┌──────────────────────────────┐
                         │  dns-latency-router          │
                         │                              │
                         │  1. 按运营商策略解析目标域名   │
                         │     → 收集全部候选 IP        │
                         │                              │
                         │  2. 每个 IP 多次探测          │
                         │     → 延迟 / 抖动 / 丢包率   │
                         │     → 计算综合评分           │
                         │                              │
                         │  3. 比较当前记录与候选节点     │
                         │     → 满足阈值与稳定窗口     │
                         │     → 再更新 Cloudflare      │
                         │                              │
                         │  4. 写入历史 / 日志 / 样本    │
                         │     → 刷新 Web 仪表盘        │
                         └──────────┬───────────────────┘
                                    │
                    ┌───────────────┴───────────────┐
                    ▼                               ▼
          Cloudflare DNS API               Web 仪表盘 :19198
     (你的自定义域名的 A 记录)          (状态 / 图表 / 日志 / 管理)
                                    │
                                    ▼
                  ┌──────────────────────────────────┐
                  │  代理客户端 (Clash / Sing-box)    │
                  │  连接 custom_domain               │
                  │  SNI → target_domain              │
                  └──────────────────────────────────┘
```

## 选路策略

### 运营商解析策略

`carrier` 控制解析阶段使用哪组 DNS 池：

- `auto`: 默认值，根据 `probe_source` 里的“联通 / 电信 / 移动”自动推断
- `unicom`: 优先使用联通视角的 DNS 池
- `telecom`: 优先使用电信视角的 DNS 池
- `mobile`: 优先使用移动视角的 DNS 池
- `all`: 使用 `dns_servers` 中配置的全量解析池

这个策略影响“发现哪些候选 IP”。最终是否切换，仍然由本机对这些 IP 的延迟、抖动、丢包率综合评分决定。

> 注意：当前 Cloudflare 单条 A 记录只能全局指向一个 IP。这里的运营商策略是“按本探测机视角选最优 IP”，不是针对不同访客运营商分别返回不同 IP 的智能 DNS。

### 主控 / Agent 模式

当需要同时接入联通、电信、移动等不同运营商出口时，可以把一台机器作为主控，其他运营商机器作为 Agent 子机。Agent 不暴露端口，只主动访问主控：

```text
电信 Agent  ─┐
联通 Agent  ─┼─>  主控 / Web / Cloudflare 更新
移动 Agent  ─┘
```

主控负责：

- 下发机场入口域名和探测参数
- 接收各 Agent 的解析、延迟、抖动、丢包样本
- 按 `carrier_records` 为不同运营商维护独立 Cloudflare A 记录
- 保留原有全局 `entry_record` 和地区 `region_records` 逻辑

Agent 负责：

- 在本机运营商网络下解析机场入口域名
- 对解析出的 IP 做 ICMP / TCP 探测
- 把结果上报给主控

主控配置示例：

```yaml
node_role: controller

agent:
  token: "change-this-long-random-token"
  report_ttl_seconds: 900

agents:
  - id: "cu-ningbo-01"
    name: "宁波联通 01"
    probe_source: "宁波联通"
    carrier: "unicom"
  - id: "ct-ningbo-01"
    name: "宁波电信 01"
    probe_source: "宁波电信"
    carrier: "telecom"

base_domain: "example.net"

airport_profiles:
  - id: "sntp"
    name: "守候网络"
    slug: "sntp"
    target_domains:
      - "2310lines.sntp.fun"
    probe_source: "宁波联通"
    carrier: "auto"
    entry_record:
      label: "全局最快"
      custom_domain: "sntp-entry.example.net"
    carrier_records:
      unicom:
        label: "联通最快"
        custom_domain: "sntp-unicom.example.net"
      telecom:
        label: "电信最快"
        custom_domain: "sntp-telecom.example.net"
```

电信 Agent 子机配置示例：

```yaml
node_role: agent

agent:
  id: "ct-ningbo-01"
  name: "宁波电信 01"
  controller_url: "http://10.0.0.234:19198"
  token: "change-this-long-random-token"
  probe_source: "宁波电信"
  carrier: "telecom"
  report_interval_seconds: 300

ping_mode: icmp
ping_port: 443
ping_timeout_seconds: 5
ping_attempts: 4
dns_servers:
  - 114.114.114.114
  - 223.5.5.5
  - 119.29.29.29
```

主控和 Agent 现在是两个独立二进制：`dns-latency-router` 只运行主控，`dns-latency-router-agent` 只运行 Agent。Agent 配置必须写 `node_role: agent`，且不需要 Cloudflare Token，它只拉任务和上报探测结果。

主控里的 `agents` 是预期在线的 Agent 清单。Web 仪表盘会显示本地主控节点以及这些 Agent 的在线、过期、离线状态；没有写进清单但实际发来上报的临时 Agent 也会显示出来。后台管理里的 `Agent 探针` 页可以维护通信 Token、离线判定 TTL，以及联通/电信/移动子机清单。`agent.report_ttl_seconds` 控制远端 Agent 多久没上报后从在线变为过期。

也可以像 Nezha 探针一样使用一键安装：在后台 `Agent 探针` 页先保存通信 Token，然后复制页面里的安装命令到电信/移动机器上用 root 执行。安装脚本会从主控下载 `dns-latency-router-agent`、写入 `agent.yaml`、创建 systemd 服务并自动连接主控；首次上报后会自动出现在主控列表里，再在后台修改地区/探测源和运营商即可。

默认不是“最低 Ping 获胜”，而是综合以下指标：

- 平均延迟
- 抖动
- 丢包率
- 低于阈值的异常延迟过滤

每个成功探测结果都会得到一个综合分，分数越低越好。默认权重大致偏向：

- 延迟优先
- 对抖动有一定惩罚
- 对丢包率更敏感

此外，为了避免频繁来回切换，系统还会启用两层保护：

- `switch_improvement_percent`
  - 新 IP 至少比当前 IP 好这么多，才有资格成为候选
- `switch_stable_seconds`
  - 候选节点必须持续稳定一段时间，才真正调用 Cloudflare API 切换

### 时间窗口权重

如果你已经知道某类节点在某个时间段容易抖动，可以让系统在这段时间里自动给它“加惩罚分”。

典型例子：

- 白天 `Google LLC` 的 Anycast 入口很快
- 但凌晨 `0:00 - 5:00` 容易闪烁、抖动变大
- 这时你希望系统宁可保守选择 `Amazon 香港` 这类更稳的节点

可用字段：

- `time_penalty_start_hour`
- `time_penalty_end_hour`
- `time_penalty_score`
- `time_penalty_org_keywords`

逻辑是：

- 如果当前本地时间落在指定窗口内
- 且该 IP 的 ISP / IDC 名称命中关键词
- 就在原有综合分上再追加一段惩罚分

例如默认值：

- `0 - 5` 点
- 对 `Google LLC`
- 追加 `60` 分

这样即使某个 Google 节点瞬时 RTT 只有 `31ms`，在深夜窗口里也会被主动降权，从而更偏向真正稳定的线路。

## Web 仪表盘

浏览器打开 `http://<你的服务器 IP>:19198`。

### 看板模块

- **主看板**
  - 目标域名
  - 当前最优 IP
  - 当前延迟圆环
  - 下次检测倒计时
  - 本轮解析结果数量
  - 探测源说明
  - 当前运营商解析策略
- **延迟历史**
  - 默认显示平均延迟最低的活跃 IP
  - 可手动点击不同 IP，切换查看它自己的延迟曲线
  - 右上角显示当前选中 IP 的样本条数
- **IP 表现**
  - 每个 IP 都会显示：
    - 归属地 / 运营商标签
    - 平均延迟
    - 平均抖动
    - 平均丢包
    - 成功率
    - 最快延迟
    - 最佳综合分
    - `n=样本数`
  - 已从最新 DNS 结果消失的 IP 会进入“游离态”
    - 卡片置灰、降权
    - 标记为“已从 DNS 移除”
    - 自动沉底排序
- **实时日志**
  - 支持 `全部 / 检测 / 决策 / 系统` 过滤
  - 以“检测周期”为单位分组显示
  - 每轮有摘要行，便于快速看结论
  - 旧轮次自动折叠
  - 支持自动跟随 / 锁定查看历史
  - 前端会过滤终端噪声，比如 `press Ctrl+C`、`next check in`、`received interrupt`
- **管理设置**
  - 单机场模式下可编辑基础域名、探测参数和路由算法
  - 多机场模式下以“机场入口”为主，按机场配置入口域名、缩写、探测源和运营商策略
  - 分成四个标签页：
    - 基础设置
    - 机场入口
    - 探测配置
    - 路由算法
  - 保存后会同时：
    - 写回 `config.yaml`
    - 更新内存中的运行参数
    - 下一轮立即按新参数工作

### UI 设计说明

当前看板不是简单表格，而是偏 `Vercel / Linear / Cloudflare 控制台` 风格的暗色运维面板：

- 深色玻璃感背景与低对比网格底纹
- 关键信息用大号等宽字体突出
- 主色为绿色青蓝系，表示“连通、稳定、在线”
- 重要信息前置，次级信息做低侵略度弱化
- 日志区做成嵌入式终端风格，便于长时间盯盘
- 地区解析卡片使用内置 SVG 国旗、地区名和弱化缩写形成视觉锚点
- 全局最快记录作为通栏汇总卡展示，子区域卡片只保留精简候选数，生命周期信息通过悬浮提示查看
- 移动端单列布局，核心卡片和延迟圆环可正常阅读

### 内置国旗资源

地区卡片使用本地 SVG 国旗，不依赖系统 Emoji 渲染，也不访问外部 CDN。资源来自 `flag-icons@7.5.0` 的 `flags/4x3`，按 MIT License 使用，授权文件保存在：

```text
internal/web/assets/flags/LICENSE.flag-icons
```

这些 SVG 会通过 Go `embed` 打进最终二进制，并由本地静态路径提供：

```text
/assets/flags/cn.svg
/assets/flags/hk.svg
/assets/flags/my.svg
/assets/flags/jp.svg
```

前端会根据地区代码自动选择对应 SVG。已做兼容：

- `entry` → `un`
- `uk` → `gb`
- `mac` → `mo`

## 数据保留与生命周期

系统会把运行数据持久化到 `data/` 目录，默认保留最近 7 天：

- `runtime-logs.jsonl`
- `runtime-history.json`
- `runtime-samples.json`

当前策略：

- 日志、检测历史、IP 样本都会按 7 天窗口裁剪
- 同时也有固定上限，避免无限膨胀
  - `logs`: 2000
  - `history`: 2000
  - `samples`: 2000
- 某个 IP 如果暂时不在最新 DNS 结果里：
  - 不会立刻删掉
  - 会变成游离态保留在 IP 表现面板
  - 后台不再继续对它做新一轮探测
  - 超过统计窗口后，它的样本会自然被清理掉

## 配置

编辑 `config.yaml`：

```yaml
cloudflare:
  api_token: "your-api-token"
  zone_id: "your-zone-id"
  record_id: "your-record-id"

target_domain: "api.openai.com"
custom_domain: "your-proxy.example.com"
probe_source: "城市运营商"
carrier: "auto"

check_interval: 300
proxy_url: "http://127.0.0.1:7890"
ping_mode: "icmp"
ping_port: 443
ping_timeout_seconds: 5
ping_attempts: 4
ping_min_threshold_ms: 1

selection_latency_weight: 1.0
selection_jitter_weight: 0.35
selection_loss_weight: 4.0
switch_improvement_percent: 15
switch_stable_seconds: 120
time_penalty_start_hour: 0
time_penalty_end_hour: 5
time_penalty_score: 60
time_penalty_org_keywords: "Google LLC"

web_port: 19198

dns_servers:
  - 114.114.114.114
  - 223.5.5.5
  - 119.29.29.29
  - 180.76.76.76
  - 8.8.8.8
```

### 多机场 / 多地区入口

如果要让不同机场、不同地区分别更新到独立子域名，可以保留全局 Cloudflare token 和 `zone_id`，然后配置 `airport_profiles`。当某个地区没有显式写 `custom_domain` 时，会按 `{slug}-{region}.{base_domain}` 自动生成。

```yaml
cloudflare:
  api_token: "your-api-token"
  zone_id: "your-zone-id"

base_domain: "example.net"

airport_profiles:
  - id: "airport-a"
    name: "机场 A"
    slug: "airport-a"
    target_domains:
      - "entry-a-1.example.com"
      - "entry-a-2.example.com"
    probe_source: "城市运营商"
    carrier: "auto"
    entry_record:
      label: "全局最快"
      record_id: "cloudflare-record-id-for-airport-a-entry"
    region_records:
      hk:
        label: "香港"
        record_id: "cloudflare-record-id-for-airport-a-hk"
      my:
        label: "马来西亚"
        record_id: "cloudflare-record-id-for-airport-a-my"

  - id: "airport-b"
    name: "机场 B"
    slug: "airport-b"
    target_domains:
      - "entry-b.example.com"
    entry_record:
      record_id: "cloudflare-record-id-for-airport-b-entry"
    region_records:
      hk:
        label: "香港"
        record_id: "cloudflare-record-id-for-airport-b-hk"
```

上面的配置会自动对应这些域名：

- `airport-a-hk.example.net`
- `airport-a-my.example.net`
- `airport-a-entry.example.net`
- `airport-b-entry.example.net`
- `airport-b-hk.example.net`

每轮检测会先解析机场入口域名；如果一个机场配置了多个 `target_domains`，会把这些域名解析出的 IP 合并去重后统一探测。随后再按 IP 归属地分组；例如解析出 3 个香港 IP 和 1 个马来西亚 IP 时，系统会在香港组内选最优 IP 更新 `airport-a-hk.example.net`，马来西亚组只有一个健康 IP 时则直接更新 `airport-a-my.example.net`。`entry_record` 会从该机场解析出的全部健康 IP 里选综合最优，更新到 `airport-a-entry.example.net`。每个机场和地区都有独立的稳定窗口，互不影响。

当前内置地区代码包括：

- `hk`: 香港
- `my`: 马来西亚
- `sg`: 新加坡
- `jp`: 日本
- `tw`: 台湾
- `mo`: 澳门
- `cn`: 中国大陆
- `us`: 美国

### 字段说明

| 字段 | 说明 |
|------|------|
| `target_domain` | 需要真实探测的目标域名 |
| `custom_domain` | Cloudflare 上由本工具动态更新的 A 记录 |
| `probe_source` | 探测机所在网络/机房说明，展示在主看板 |
| `carrier` | 运营商解析策略：`auto` / `unicom` / `telecom` / `mobile` / `all` |
| `check_interval` | 两轮检测之间的间隔，单位秒 |
| `proxy_url` | Cloudflare API 请求使用的代理，可为 HTTP 或 SOCKS5 |
| `ping_mode` | `icmp` 或 `tcp` |
| `ping_port` | `tcp` 模式下探测的目标端口 |
| `ping_timeout_seconds` | 单次探测超时 |
| `ping_attempts` | 每轮对每个 IP 的探测次数 |
| `ping_min_threshold_ms` | 过低延迟过滤阈值，避免本地回环等假结果 |
| `selection_latency_weight` | 延迟权重 |
| `selection_jitter_weight` | 抖动权重 |
| `selection_loss_weight` | 丢包权重 |
| `switch_improvement_percent` | 新 IP 至少比当前 IP 好多少百分比才考虑切换 |
| `switch_stable_seconds` | 候选节点需稳定多久才真正切换 |
| `time_penalty_start_hour` | 时间窗口惩罚的开始小时，按探测机本地时间 |
| `time_penalty_end_hour` | 时间窗口惩罚的结束小时，支持跨天 |
| `time_penalty_score` | 命中时间窗口和目标厂商后追加的惩罚分 |
| `time_penalty_org_keywords` | 逗号分隔的 ISP / IDC 关键词，如 `Google LLC, Google Cloud` |
| `web_port` | Web 仪表盘端口，`0` 表示禁用 |

### 管理设置中的分组

- **基础设置**
  - `target_domain`
  - `custom_domain`
  - `probe_source`
  - `carrier`
  - `check_interval`
- **机场入口**
  - `base_domain`
  - `airport_profiles[].name`
  - `airport_profiles[].slug`
  - `airport_profiles[].target_domains`
  - `airport_profiles[].probe_source`
  - `airport_profiles[].carrier`
- **探测配置**
  - `ping_mode`
  - `ping_port`
  - `ping_attempts`
- **路由算法**
  - `selection_latency_weight`
  - `selection_jitter_weight`
  - `selection_loss_weight`
  - `switch_improvement_percent`
  - `switch_stable_seconds`
  - `time_penalty_start_hour`
  - `time_penalty_end_hour`
  - `time_penalty_score`
  - `time_penalty_org_keywords`

## 获取 Cloudflare 参数

| 参数 | 获取方式 |
|------|---------|
| Zone ID | Cloudflare 仪表盘 → Overview |
| API Token | [Cloudflare API Tokens](https://dash.cloudflare.com/profile/api-tokens) |
| Record ID | `curl -H "Authorization: Bearer <token>" "https://api.cloudflare.com/client/v4/zones/<zone_id>/dns_records"` |

> 如果删除并重建了 DNS 记录，`record_id` 会变，需要重新获取。

## 编译与运行

### 一键构建

```bash
chmod +x build.sh
./build.sh
```

### Windows

```powershell
go mod tidy
go build -o dns-latency-router.exe .
go build -o dns-latency-router-agent.exe ./cmd/dlr-agent
.\dns-latency-router.exe
```

### Linux + PM2

```bash
./build.sh
mkdir -p logs
pm2 start ecosystem.config.js
pm2 save
```

常用命令：

```bash
pm2 status
pm2 logs dns-latency-router
pm2 restart dns-latency-router
pm2 stop dns-latency-router
pm2 delete dns-latency-router
```

### 自定义配置路径

```bash
./dns-latency-router /path/to/config.yaml
./dns-latency-router-agent /path/to/agent.yaml
```

### Release 构建产物

正式 Release 通常提供四个 amd64 产物：

- `dns-latency-router-windows-amd64.exe`
- `dns-latency-router-linux-amd64`
- `dns-latency-router-agent-windows-amd64.exe`
- `dns-latency-router-agent-linux-amd64`

主控二进制包含嵌入式 Web 仪表盘和本地 SVG 国旗资源；Agent 二进制不包含 Web UI，只负责探测和上报。

## 代理客户端配置示例

核心技巧：客户端连接 `custom_domain`，但 TLS SNI 填 `target_domain`。

### Clash

```yaml
proxies:
  - name: "fast-ai"
    type: ss
    server: your-proxy.example.com
    port: 443
    sni: api.openai.com
```

### Sing-box

```json
{
  "outbounds": [{
    "type": "shadowsocks",
    "server": "your-proxy.example.com",
    "server_port": 443,
    "sni": "api.openai.com"
  }]
}
```

## 常见问题

### 1. Token 能 GET 但 PATCH 失败

通常是 Token 只有 `DNS:Read`，没有 `DNS:Edit`。

### 2. Cloudflare API 报 EOF 或网络错误

优先检查：

- 服务器是否能直连外网
- `proxy_url` 指向的代理是否可用
- 代理节点本身是否已经失效

### 3. 页面一直显示“加载中”

新版本前端已经改成短轮询，不再依赖长连接常驻刷新；如果仍异常，通常是反向代理或浏览器缓存问题，强刷页面即可。

### 4. 为什么有些 IP 变灰了

这是“游离态”：

- 它们曾经在 DNS 结果里出现过
- 但当前最新一轮解析里已经不在
- 系统会保留它们最近 7 天的历史表现，便于回看

### 5. 为什么日志不会无限增长

日志、历史、IP 样本都只保留最近 7 天，且有固定条数上限，用来防止内存和 UI 无限膨胀。

## 适用场景

- 目标域名背后是多 IP / CDN / Anycast 入口
- 你想长期观察不同出口 IP 的本地表现差异
- 你需要一个适合盯盘的简洁 Web 控制台
- 你希望切换逻辑更保守，避免“今天这个快一点、下一轮又换回去”
