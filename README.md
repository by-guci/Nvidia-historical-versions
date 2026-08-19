# Nvidia historical versions

NVIDIA 历史驱动检索 —— 一个基于 Go 的轻量级驱动检索网站，帮你找回那些被 NVIDIA 官网"淹没"的历史驱动版本。

> 新驱动不一定更好。游戏卡顿、性能回退、兼容性问题时，回退到旧驱动往往是首选方案，但 NVIDIA 官网的历史版本很难直接找到。这个项目把 NVIDIA 官方的驱动数据抓取下来，做成一个快速检索网站。

## 🖥️ 在线体验

🔗 **[https://nvidia.openxer.com](https://nvidia.openxer.com)** —— 直接访问即可检索、筛选、下载 NVIDIA 历史驱动。

## ✨ 功能特性

- **历史驱动检索**：收录 NVIDIA 官方驱动数据（最早可追溯到 2016 年），按驱动名称、版本号、操作系统、语言精确筛选
- **一键直达官方下载**：每条记录都附 NVIDIA 官方下载链接（download.nvidia.com），不经过任何中转
- **中文优先**：默认收录简体中文、繁体中文、英文（US）三种语言版本
- **自动更新**：内置更新器定期从 NVIDIA GeForce Service Toolkit API 同步最新驱动，数据保持新鲜
- **轻量部署**：纯 Go 单二进制 + 静态文件，Docker 镜像仅约 15MB，`docker compose up` 即可启动
- **数据分片存储**：驱动数据按 JSON 分片存储并索引，无需数据库，易于备份和迁移

## 🚀 快速开始

```bash
# Docker Compose 一键启动（Web 服务 + 定时更新器）
docker compose up -d --build

# 或本地直接运行
go run .
# 访问 http://localhost:8090
```

> **⚠️ 部署注意（重要）**：两个容器均以 UID 1000（appuser）运行，需要能写入宿主机挂载的 `static/data` 目录。如果 updater 报 `permission denied`，在服务器上执行：
>
> ```bash
> # 将数据目录属主改为 UID 1000（与容器内 appuser 一致）
> sudo chown -R 1000:1000 static/data
> docker compose up -d --build
> ```
>
> 若你的服务器 UID 1000 已被其他用户占用，可改为其他 UID（如 1001），但需同步修改 `Dockerfile` 中 `adduser -u` 和 `docker-compose.yml` 中 `user:` 两处。

手动更新驱动数据：

```bash
go run ./cmd/updater -data ./static/data -scan 5000
```

## 🔧 技术栈

| 组件 | 说明 |
| --- | --- |
| 后端 | Go 1.23，标准库 net/http，无第三方依赖 |
| 前端 | 原生 HTML / CSS / JavaScript |
| 数据 | JSON 分片存储 + 索引清单（index.json） |
| 部署 | Docker 多阶段构建，Alpine 基础镜像，非 root 运行 |

## 📁 项目结构

```
nvidia-historical-versions/
├── main.go                  # Web 服务器主程序：API 路由、驱动检索、分页、静态文件服务
├── clicks.go                # 下载点击计数：限流、去重、持久化（30 秒落盘）
├── main_test.go             # 服务器端测试（检索、分页、API）
├── go.mod                   # Go 模块定义（module: nvidia-driver-search, go 1.23）
├── Dockerfile               # Docker 多阶段构建：编译 + Alpine 运行镜像（约 15MB）
├── docker-compose.yml       # 一键编排：Web 服务 + 定时更新器（每周日 03:00）
├── LICENSE                  # MIT 许可证
├── README.md                # 项目文档
├── .gitignore               # Git 忽略规则
│
├── cmd/
│   └── updater/
│       ├── main.go          # 数据更新器：从 NVIDIA API 抓取驱动，写入 JSON 分片
│       └── main_test.go     # 更新器测试
│
└── static/                  # 前端静态资源（由 Web 服务器直接托管）
    ├── index.html           # 单页应用入口：搜索框、筛选、结果列表、分页
    ├── styles.css           # 页面样式（437 行）
    ├── app.js               # 前端逻辑：调用 /api 接口、渲染结果、上报点击
    ├── assets/              # 站点图标（favicon）
    └── data/                # 驱动数据（JSON 分片 + 索引 + 前端兜底数据）
        ├── index.json       # 分片索引清单
        ├── drivers-001~020.json  # 驱动数据分片（共 20 个）
        └── local-drivers.js # 本地驱动数据（前端兜底，无需后端也能展示）
```

## 📡 API

| 接口 | 说明 |
| --- | --- |
| `GET /api/drivers` | 驱动检索（支持关键词、版本、系统、语言、分页） |
| `GET /api/options` | 可选的操作系统 / 语言筛选项 |
| `GET /api/clicks` | 下载点击量统计 |
| `GET /health` | 健康检查 |

## 🗂️ 数据

驱动数据存放在 `static/data/`：

- `drivers-*.json`：驱动数据分片
- `index.json`：分片索引清单
- `local-drivers.js`：本地驱动数据（前端兜底）

数据来源：NVIDIA GeForce Service Toolkit API（`gfwsl.geforce.cn`），仅收录官方发布的驱动信息。

## 📄 许可

本项目代码采用 MIT 许可证，驱动数据版权归 NVIDIA 所有。
