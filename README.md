# DNS Latency Router

周期性探测目标域名的所有解析 IP，找到延迟最低的那个，自动更新 Cloudflare DNS A 记录指向最快 IP。自带 Web 仪表盘。

专为代理客户端（Clash、Sing-box）设计：客户端连接自定义域名，TLS SNI 覆写为目标域名，实现动态路由到最优 IP。

## 工作原理

```
                         ┌──────────────────────────────┐
                         │  dns-latency-router           │
                         │                              │
                         │  1. 多DNS解析目标域名          │
                         │     (电信/阿里/DNSPod等)       │
                         │     → 发现所有可能的 IP        │
                         │                              │
                         │  2. 本地TCP ping所有 IP        │
                         │     (并发, 5s超时)             │
                         │     → 选延迟最低的 IP          │
                         │                              │
                         │  3. 更新Cloudflare A记录       │
                         │     → 指向最快 IP              │
                         │                              │
                         │  4. 休眠5分钟, 重复            │
                         └──────────┬───────────────────┘
                                    │
                    ┌───────────────┴───────────────┐
                    ▼                               ▼
          Cloudflare DNS API               Web 仪表盘 :19198
     (你的自定义域名的 A 记录)          (实时状态 / 图表 / 管理)
                                    │
                                    ▼
                  ┌──────────────────────────────────┐
                  │  代理客户端 (Clash/Sing-box)       │
                  │  连接 custom_domain               │
                  │  SNI → target_domain              │
                  └──────────────────────────────────┘
```

## Web 仪表盘

浏览器打开 `http://<IP>:19198`：

- **状态卡片** — 目标域名、当前最优 IP、延迟、下次检测倒计时
- **延迟历史曲线图** — 平滑贝塞尔曲线，鼠标悬停显示详情（时间、延迟、IP）
- **实时日志** — SSE 推送，自动滚动
- **管理面板** — 点击右上角 ⚙，在线修改目标域名和自定义域名，保存后自动写入 `config.yaml` 并即时生效

## 配置

编辑 `config.yaml`：

```yaml
cloudflare:
  api_token: "your-api-token"     # Cloudflare API Token (Zone:DNS:Edit)
  zone_id: "your-zone-id"         # Cloudflare 区域 ID
  record_id: "your-record-id"     # DNS 记录 ID

target_domain: "api.openai.com"        # 要探测的目标域名
custom_domain: "your-proxy.example.com"  # 你的代理域名 (A 记录)

check_interval: 300        # 探测间隔 (秒)
ping_port: 443             # TCP ping 端口
ping_timeout_seconds: 5    # 单次 ping 超时

web_port: 19198            # Web 仪表盘端口 (0 = 禁用)

dns_servers:               # 多运营商 DNS 解析
  - 114.114.114.114        # 电信
  - 223.5.5.5              # 阿里云
  - 119.29.29.29           # DNSPod (腾讯)
  - 180.76.76.76           # 百度
  - 8.8.8.8                # Google
```

### 获取 Cloudflare 参数

| 参数 | 获取方式 |
|------|---------|
| Zone ID | Cloudflare 仪表盘 → Overview → 右侧 API 区域 |
| API Token | https://dash.cloudflare.com/profile/api-tokens → 创建 Token (Zone:DNS:Edit) |
| Record ID | `curl -H "Authorization: Bearer <token>" "https://api.cloudflare.com/client/v4/zones/<zone_id>/dns_records"` |

> **Record ID 说明**: 如果删除并重建了 DNS 记录，Record ID 会变，需要重新获取。

## 编译 & 运行

### 一键构建（Linux + Windows）

```bash
chmod +x build.sh
./build.sh
```

### Windows

```powershell
cd dns-latency-router
go mod tidy
go build -o dns-latency-router.exe .
.\dns-latency-router.exe
```

### Linux + PM2 进程守护

```bash
# 1. 构建 Linux 版本
./build.sh

# 2. 部署
mkdir -p logs
pm2 start ecosystem.config.js
pm2 save

# 3. 常用命令
pm2 status                      # 查看状态
pm2 logs dns-latency-router     # 查看日志
pm2 restart dns-latency-router  # 重启
pm2 stop dns-latency-router     # 停止
pm2 delete dns-latency-router   # 删除
```

### 自定义配置路径

```bash
./dns-latency-router path/to/config.yaml
```

运行后立即执行一次探测，之后按 `check_interval` 循环。按 `Ctrl+C` 停止。

## 代理客户端配置

核心技巧：客户端连接 `custom_domain`（解析到最快 IP），TLS SNI 设置为 `target_domain`。

### Clash

```yaml
proxies:
  - name: "fast-ai"
    type: ss
    server: your-proxy.example.com   # custom_domain
    port: 443
    sni: api.openai.com              # target_domain (SNI 覆写)
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

## 注意事项

- 如果目标域名当前只有一个 IP，多 DNS 解析也不会发现更多——这个机制是**为未来目标域名切到多 IP（如 CDN）时自动生效**而设计的
- 工具只在 IP 变化时才会调用 Cloudflare API（内部做了比对，IP 没变不会重复请求）
- 通过 Web 管理面板修改域名后，会自动写入 `config.yaml` 持久化，重启不丢失
- Cloudflare 免费计划速率限制 1200 请求/5 分钟，此工具每周期仅 1 个请求
