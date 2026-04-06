---
date: 2026-04-05
topic: "收藏内容与收藏夹浏览功能"
status: draft
---

# Exploration: 收藏内容与收藏夹浏览

## Problem Statement

当前 MCP 只有 `favorite_feed`（收藏/取消收藏单篇笔记），是纯 write-only 操作。
用户需要：
1. 查看自己全部收藏内容
2. 列出所有收藏夹（Collection）
3. 读取特定收藏夹内的笔记并进行分析

## 现有代码分析

### 已有模式（可复用）

| 模式 | 文件 | 做了什么 |
|------|------|---------|
| 用户主页 | `user_profile.go` | 导航到 profile URL → 读 `__INITIAL_STATE__.user` → 提取数据 |
| 搜索翻页 | `search_pagination.go` | 滚动 → 重读 state → 按 ID 去重 → 停滞检测 |
| 侧边栏导航 | `navigate.go` | 导航到 explore → 点击侧边栏链接 → 进入子页面 |

### 关键发现

1. **`__INITIAL_STATE__` 是主要数据源** — 所有现有功能都从这里读数据，不调 API
2. **翻页模式已验证** — `search_pagination.go` 的滚动去重模式可直接复用
3. **Feed 结构可复用** — 收藏的笔记本质上也是 Feed，数据结构应该一致
4. **每个请求独立 browser** — 无状态，无共享问题

## 小红书收藏页结构（待验证）

### URL 模式

```
用户主页:          https://www.xiaohongshu.com/user/profile/{user_id}
收藏 tab:          https://www.xiaohongshu.com/user/profile/{user_id}?tab=collect
特定收藏夹:         https://www.xiaohongshu.com/user/profile/{user_id}/collect/{collection_id}
```

### 预期 `__INITIAL_STATE__` 结构（需 DOM 验证）

```
window.__INITIAL_STATE__
├── user
│   ├── userPageData     (已知: 用户基本信息)
│   ├── notes            (已知: 用户笔记列表)
│   ├── collect          (预期: 收藏的笔记列表)
│   └── collectFolders   (预期: 收藏夹列表)
```

## 技术方案选项

### 方案 A: 基于 `__INITIAL_STATE__` 读取（推荐）

与现有代码一致的模式：
1. 导航到用户收藏页 URL
2. 读取 `__INITIAL_STATE__` 中的收藏数据
3. 需要翻页时复用 scroll + 去重模式

**优点**: 与项目现有模式完全一致，代码量小，维护简单
**风险**: 收藏夹的 state key 名称需要实际验证

### 方案 B: 基于 DOM 解析

直接从页面 DOM 元素中提取收藏卡片信息。

**优点**: 不依赖 `__INITIAL_STATE__` 的具体结构
**缺点**: DOM 结构容易变化，解析复杂，与项目现有模式不一致

**结论: 选方案 A**，与项目 90% 的代码保持一致。

## 新增 MCP 工具设计（初步）

| 工具名 | 参数 | 返回 | 说明 |
|--------|------|------|------|
| `list_collections` | 无（读当前登录用户） | 收藏夹列表 (name, id, count) | 列出所有收藏夹 |
| `get_collection_content` | `collection_id`, `limit` | Feed 列表 | 读取特定收藏夹内容，支持翻页 |
| `list_saved_content` | `limit` | Feed 列表 | 读取全部收藏（不分收藏夹），支持翻页 |

## 新增文件结构（预估）

```
xiaohongshu/
├── saved_content.go           # NEW: 收藏内容浏览逻辑
├── saved_content_types.go     # NEW: Collection 等数据结构（如果 state 结构不同于 Feed）
├── search_pagination.go       # 复用: scrollAndCollectFeeds 可能需要泛化
```

现有文件改动（最小侵入，同翻页功能策略）：
- `mcp_server.go` — 注册新工具 + 参数结构体
- `mcp_handlers.go` — handler 函数
- `service.go` — service 方法

## Open Questions（需 Research 阶段解决）

1. **`__INITIAL_STATE__` 中收藏数据的确切 key 是什么？** — 需要打开浏览器实际查看
2. **收藏夹列表在哪个 URL / state key 下？** — 可能在 profile 页，也可能需要单独页面
3. **收藏内容的数据结构和 Feed 是否一致？** — 如果一致可以直接复用 types
4. **未登录用户的收藏是否可见？** — 可能只有自己能看到自己的收藏
5. **翻页机制是否和搜索一样（滚动触发懒加载）？** — 大概率是

## Next Step

进入 Research 阶段：用浏览器打开小红书收藏页，用 Chrome DevTools 查看 `__INITIAL_STATE__` 的实际结构，回答上面的 5 个 Open Questions。
