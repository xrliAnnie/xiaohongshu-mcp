---
date: 2026-04-06
topic: "收藏内容与收藏夹浏览功能"
status: draft
depends_on: "exploration/new/saved-content-collections.md"
---

# Research: 收藏内容与收藏夹浏览

## Key Findings

### 1. 代码架构模式（已验证）

项目遵循统一的四层架构：

```
mcp_server.go (Args 结构体 + 工具注册)
  → mcp_handlers.go (handler 函数)
    → service.go (Service 方法: 创建 browser/page, 调用 Action)
      → xiaohongshu/*.go (Action 结构体: go-rod 操作 + __INITIAL_STATE__ 读取)
```

每个 Action 都通过 `newBrowser() → page → Action → method → response` 模式工作。

### 2. `__INITIAL_STATE__` 数据模型

已知的 state 路径：

| 功能 | State Key | 数据格式 |
|------|-----------|---------|
| 首页 Feed | `feed.feeds._value` | `[]Feed` |
| 搜索结果 | `search.feeds._value` | `[]Feed` |
| 用户信息 | `user.userPageData._value` | `{basicInfo, interactions}` |
| 用户笔记 | `user.notes._value` | `[][]Feed`（双重数组） |
| 笔记详情 | `note.noteDetailMap` | `map[feedID]{note, comments}` |

关键发现：所有 state 值都使用 `.value` 或 `._value` 两种 accessor 模式（getter vs 内部字段）。

### 3. 翻页复用可行性分析

`search_pagination.go` 中的 `scrollAndCollectFeeds()` 函数：
- 核心逻辑完全通用：滚动、去重、停滞检测
- **唯一耦合点**：`readFeedsJS` 常量，硬编码读取 `search.feeds`
- **解耦方案**：提取 `scrollAndCollectFeedsWithReader(page, initialFeeds, limit, readJS)` 函数，将 JS 表达式参数化
- 原函数保持不变，调用新函数传入 `readFeedsJS`

### 4. 获取当前用户 ID 的方式

收藏页 URL 需要 user_id。两种获取方式：

| 方式 | 实现 | 优缺点 |
|------|------|--------|
| 侧边栏导航 | `navigate.ToProfilePage()` → 从 URL 提取 | 复用现有代码，可靠 |
| State 读取 | 从 `__INITIAL_STATE__` 读取 | 需要先导航到任意页面 |

**推荐方式 A（侧边栏导航）**：
1. 导航到个人主页（复用 `NavigateAction.ToProfilePage`）
2. 从 URL 正则提取 user_id: `/user/profile/([a-f0-9]+)`
3. 构造收藏页 URL 并二次导航

## Open Questions 回答

### Q1: `__INITIAL_STATE__` 中收藏数据的确切 key 是什么？

**推测（需运行时验证）**：

基于 `user.notes` 的命名模式，收藏数据大概率在以下位置：
- `user.collect._value` — 收藏的笔记列表
- `user.collectFolders._value` — 收藏夹文件夹列表

替代可能的 key 名：
- `user.collections`, `user.boards`, `user.favorites`

**设计策略**：代码中按优先级尝试多个 key，找到有效数据即停止。用 `page.MustEval` 打印完整 `user` 对象的 keys 来确认。

```javascript
// 运行时探测 JS
() => {
  const user = window.__INITIAL_STATE__?.user;
  if (user) return JSON.stringify(Object.keys(user));
  return "";
}
```

**实现中的降级策略**：首次运行时，如果预设的 key 都找不到数据，打印所有 `user` 下的 keys 到日志，方便调试。

### Q2: 收藏夹列表在哪个 URL / state key 下？

**推测**：

URL 模式：
- 收藏 tab: `https://www.xiaohongshu.com/user/profile/{user_id}?tab=collect`
- 特定收藏夹: `https://www.xiaohongshu.com/user/profile/{user_id}/collect/{collection_id}`

收藏夹列表数据的获取有两种可能：
1. 在 profile 的 collect tab 页面的 `__INITIAL_STATE__` 中（与收藏内容一起加载）
2. 通过点击收藏 tab 后触发的数据加载

**实现方案**：导航到 `?tab=collect` 页面后，探测 `__INITIAL_STATE__` 中的 user 子键。

### Q3: 收藏内容的数据结构和 Feed 是否一致？

**高概率一致**。理由：
1. 小红书的笔记卡片在首页、搜索、用户主页都使用同一个 `Feed` 结构
2. 收藏的本质是"笔记引用"，UI 展示和首页/搜索完全一样（卡片样式、封面、标题、互动信息）
3. `__INITIAL_STATE__` 中其他所有笔记列表都使用 `Feed` 类型

**风险**：收藏列表中可能有额外字段（如收藏时间 `collectTime`），但核心 Feed 字段应该一致。如果有差异，可以在 `Feed` 结构体上加 `json:",omitempty"` 字段，不影响现有功能。

### Q4: 未登录用户的收藏是否可见？

**仅自己可见**。理由：
1. 小红书收藏是私密功能，其他用户无法看到你的收藏
2. 因此 3 个新工具都只能操作**当前登录用户**的收藏
3. 不需要 `user_id` 和 `xsec_token` 参数（与 `list_feeds` 一样，无参数）
4. 必须处理未登录场景——返回明确错误提示

**对工具设计的影响**：
- `list_collections`: 无参数（读当前用户）
- `list_saved_content`: 仅需 `limit` 参数
- `get_collection_content`: 需要 `collection_id` + `limit`

### Q5: 翻页机制是否和搜索一样（滚动触发懒加载）？

**大概率是**。理由：
1. 小红书所有列表页面（首页、搜索、用户笔记、收藏）都使用瀑布流布局
2. 瀑布流的标准模式就是滚动触发懒加载
3. `__INITIAL_STATE__` 会随滚动更新（搜索结果已验证此行为）

**可直接复用 `scrollAndCollectFeeds` 的翻页逻辑**，仅需替换 JS 读取表达式。

## Implementation Details

### go-rod API 使用计划

| 操作 | go-rod API | 说明 |
|------|-----------|------|
| 导航到收藏页 | `page.MustNavigate(url)` | 构造 URL 直接导航 |
| 等待页面稳定 | `page.MustWaitStable()` | 等待异步数据加载 |
| 等待 state 加载 | `page.MustWait(js)` | 等待 `__INITIAL_STATE__` 可用 |
| 读取 state 数据 | `page.MustEval(js)` | JS 注入读取（必须，go-rod 无法直接读 JS 变量） |
| 滚动翻页 | `page.Eval(scrollJS)` | 复用现有滚动模式 |
| 点击收藏 tab | `page.MustElement(selector).MustClick()` | go-rod 原生点击（如需要 tab 切换） |
| 提取 URL | `page.MustInfo().URL` | 从 URL 提取 user_id |

**JS 注入说明**：读取 `__INITIAL_STATE__` 必须使用 `page.MustEval` / `page.Eval`，这是因为 `__INITIAL_STATE__` 是 JS 全局变量，go-rod 没有原生 API 可以直接访问。项目中所有现有功能都使用此模式，这是合理且必要的 JS 注入。

### 新增 JS 读取表达式

```javascript
// 读取收藏内容 (list_saved_content / get_collection_content 翻页用)
const readCollectFeedsJS = `() => {
  const user = window.__INITIAL_STATE__?.user;
  if (!user) return "";
  // 尝试多个可能的 key
  const collect = user.collect || user.collections || user.collectNotes;
  if (!collect) return "";
  const data = collect.value !== undefined ? collect.value : collect._value;
  return data ? JSON.stringify(data) : "";
}`;

// 读取收藏夹列表
const readCollectFoldersJS = `() => {
  const user = window.__INITIAL_STATE__?.user;
  if (!user) return "";
  const folders = user.collectFolders || user.boards || user.collectionFolders;
  if (!folders) return "";
  const data = folders.value !== undefined ? folders.value : folders._value;
  return data ? JSON.stringify(data) : "";
}`;

// 探测 user state 下的所有 keys（调试用）
const debugUserKeysJS = `() => {
  const user = window.__INITIAL_STATE__?.user;
  if (!user) return "";
  return JSON.stringify(Object.keys(user));
}`;
```

### 文件结构

```
xiaohongshu/
├── saved_content.go          # NEW: SavedContentAction + 3 个方法
├── search_pagination.go      # MODIFY: 提取通用翻页函数

mcp_server.go                 # MODIFY: 3 个 Args 结构体 + 3 个工具注册
mcp_handlers.go               # MODIFY: 3 个 handler
service.go                    # MODIFY: 3 个 service 方法
```

## Edge Cases

1. **未登录** — 侧边栏导航会失败，需要捕获并返回"请先登录"错误
2. **无收藏** — `__INITIAL_STATE__` 中收藏列表为空或 key 不存在，返回空列表而非错误
3. **收藏夹为空** — 特定收藏夹中无内容，返回空列表
4. **state key 名称猜测错误** — 首次运行时若预设 key 都不匹配，打印调试信息到日志
5. **收藏内容被删除** — 收藏列表中的笔记可能已被删除，此时 Feed 数据中可能有标记或缺失字段
6. **双重数组** — `user.notes` 使用 `[][]Feed` 格式，收藏可能也是，需要兼容处理

## Dependencies

- 无新增外部依赖
- 复用项目现有的 go-rod、logrus、retry-go

## Estimated Complexity: medium

核心逻辑与现有代码高度一致，主要不确定性在 `__INITIAL_STATE__` 的具体 key 名称。实现后需要运行时验证并可能需要调整 key 名。
