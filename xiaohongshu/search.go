package xiaohongshu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/xpzouying/xiaohongshu-mcp/configs"
	"github.com/xpzouying/xiaohongshu-mcp/errors"
)

type SearchResult struct {
	Search struct {
		Feeds FeedsValue `json:"feeds"`
	} `json:"search"`
}

// FilterOption 筛选选项结构体
type FilterOption struct {
	SortBy      string `json:"sort_by,omitempty" jsonschema:"排序依据: 综合|最新|最多点赞|最多评论|最多收藏,默认为'综合'"`
	NoteType    string `json:"note_type,omitempty" jsonschema:"笔记类型: 不限|视频|图文,默认为'不限'"`
	PublishTime string `json:"publish_time,omitempty" jsonschema:"发布时间: 不限|一天内|一周内|半年内,默认为'不限'"`
	SearchScope string `json:"search_scope,omitempty" jsonschema:"搜索范围: 不限|已看过|未看过|已关注,默认为'不限'"`
	Location    string `json:"location,omitempty" jsonschema:"位置距离: 不限|同城|附近,默认为'不限'"`
}

// filterGroup 面板上的一个筛选组：标签是什么、对应入参的哪个字段、允许哪些取值。
//
// 组和选项一律按文本定位，不用序号。面板里每个选项除真实节点外还有一份几乎透明的
// 诱饵节点（见 findFilterOption），二者交错排列，按 nth-child 取会落在诱饵上。
type filterGroup struct {
	label        string                    // 面板上这一组的标签文本
	defaultValue string                    // 页面初始选中的值，不需要重复点击
	pick         func(FilterOption) string // 从入参里取这一组的值
	allowed      []string                  // 合法取值；在打开页面之前就能挡掉写错的值
}

var filterGroups = []filterGroup{
	{"排序依据", "综合", func(f FilterOption) string { return f.SortBy },
		[]string{"综合", "最新", "最多点赞", "最多评论", "最多收藏"}},
	{"笔记类型", "不限", func(f FilterOption) string { return f.NoteType },
		[]string{"不限", "视频", "图文"}},
	{"发布时间", "不限", func(f FilterOption) string { return f.PublishTime },
		[]string{"不限", "一天内", "一周内", "半年内"}},
	{"搜索范围", "不限", func(f FilterOption) string { return f.SearchScope },
		[]string{"不限", "已看过", "未看过", "已关注"}},
	{"位置距离", "不限", func(f FilterOption) string { return f.Location },
		[]string{"不限", "同城", "附近"}},
}

// pendingFilter 一个待应用的筛选项。
type pendingFilter struct {
	group     string // 组标签
	option    string // 选项文本
	groupIdx  int    // 组在页面 activeFilters 里的下标（与 filterGroups 顺序一致）
	optionIdx int    // 选项在该组里的下标（与 allowed 顺序一致）
}

// collectFilters 把入参整理成待应用的筛选项，同一组以最后一个非空值为准，等于默认值的不点。
//
// 校验放在这里是为了在打开浏览器之前就挡掉写错的值：否则要等导航、悬停、
// 在面板里找不到之后才能报错，等于为了说一句"你写错了"先向平台发一次请求。
func collectFilters(filters []FilterOption) ([]pendingFilter, error) {
	selected := make(map[string]string, len(filterGroups))

	for _, f := range filters {
		for _, g := range filterGroups {
			value := g.pick(f)
			if value == "" {
				continue
			}
			if !slices.Contains(g.allowed, value) {
				return nil, fmt.Errorf("%s 不支持 %q，可选：%s",
					g.label, value, strings.Join(g.allowed, "、"))
			}
			selected[g.label] = value
		}
	}

	var pending []pendingFilter
	for gi, g := range filterGroups {
		value, ok := selected[g.label]
		if !ok || value == g.defaultValue {
			continue
		}
		pending = append(pending, pendingFilter{
			group: g.label, option: value, groupIdx: gi, optionIdx: slices.Index(g.allowed, value),
		})
	}

	return pending, nil
}

type SearchAction struct {
	page *rod.Page
}

func NewSearchAction(page *rod.Page) *SearchAction {
	pp := page.Timeout(60 * time.Second)

	return &SearchAction{page: pp}
}

func (s *SearchAction) Search(ctx context.Context, keyword string, limit int, filters ...FilterOption) ([]Feed, error) {
	// 先校验筛选取值，必须在导航之前：写错的值不该先向平台发一次请求再报错。
	pending, err := collectFilters(filters)
	if err != nil {
		return nil, err
	}

	page := s.page.Context(ctx).Timeout(60 * time.Second)

	searchURL := makeSearchURL(keyword)
	page.MustNavigate(searchURL)
	page.MustWaitStable()

	page.MustWait(`() => window.__INITIAL_STATE__ !== undefined`)

	if len(pending) > 0 {
		// 悬停在筛选按钮上展开面板
		filterButton := page.MustElement(`div.filter`)
		filterButton.MustHover()

		// 等待筛选面板出现
		page.MustWait(`() => document.querySelector('div.filter-panel') !== null`)

		for _, pf := range pending {
			option, err := findFilterOption(page, pf)
			if err != nil {
				return nil, err
			}

			// 每点一项都等页面确认这一项已生效、结果加载完成，再点下一项：
			// 站点点完先清空 feeds 再灌入新数据，连点会让下一次点击落在重排中的面板上。
			if err := option.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return nil, fmt.Errorf("点击筛选选项「%s」失败: %w", pf.option, err)
			}
			if err := waitFilterApplied(page, pf.groupIdx, pf.optionIdx, filterResultTimeout); err != nil {
				return nil, fmt.Errorf("筛选「%s」未生效: %w", pf.option, err)
			}
		}
	}

	result := page.MustEval(readFeedsJS).String()

	if result == "" {
		return nil, errors.ErrNoFeeds
	}

	var feeds []Feed
	if err := json.Unmarshal([]byte(result), &feeds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal feeds: %w", err)
	}

	// 根据 limit 截断或翻页加载更多结果
	if limit > 0 {
		if len(feeds) >= limit {
			return feeds[:limit], nil
		}
		feeds = scrollAndCollectFeeds(page, feeds, limit)
	}

	return feeds, nil
}

// filterResultTimeout 点完筛选项后等结果换一批的上限。
const filterResultTimeout = 15 * time.Second

// minOptionOpacity 不透明度低于此值的节点不作为点击目标。
const minOptionOpacity = 0.1

// effectiveOpacityJS 算节点自身连同各级祖先的不透明度乘积。
// 只看自身 opacity 不够：祖先透明时子节点仍报 1。
const effectiveOpacityJS = `() => {
	let o = 1;
	for (let el = this; el && el.nodeType === 1; el = el.parentElement) {
		const v = parseFloat(getComputedStyle(el).opacity);
		if (!Number.isNaN(v)) o *= v;
	}
	return o;
}`

// optionVisible 判断筛选选项节点是不是真实可见的那一个：
// rod 的 Visible 排除 display:none / visibility:hidden / 零尺寸，再加上有效不透明度。
// 读不到状态就返回错误，由调用方上报，不能默认当作可见（那样会失去排除诱饵的依据）。
func optionVisible(opt *rod.Element) (bool, error) {
	visible, err := opt.Visible()
	if err != nil {
		return false, err
	}
	if !visible {
		return false, nil
	}
	res, err := opt.Eval(effectiveOpacityJS)
	if err != nil {
		return false, err
	}
	return res.Value.Num() >= minOptionOpacity, nil
}

// filterAppliedJS 读页面自己记录的筛选状态（__INITIAL_STATE__.search）：
// activeFilters[组] 是该组当前选中的选项下标，state 是 "loading" / "success"。
// 实测点击的那一刻 activeFilters 更新、state 变 loading 且 feeds 清空，
// 请求完成后 state 回到 success 并填入新结果。
const filterAppliedJS = `(group, option) => {
	const s = window.__INITIAL_STATE__?.search;
	if (!s) return false;
	const u = v => (v && typeof v === "object" && ("_value" in v || "value" in v)) ? (v.value !== undefined ? v.value : v._value) : v;
	const active = u(s.activeFilters);
	return Array.isArray(active) && active[group] === option && u(s.state) === "success";
}`

// waitFilterApplied 等页面确认「这一组选中了这一项，且这次请求已完成」。
//
// 点完筛选项之后不能立刻读结果：feeds 先被清空再灌入，读早了拿到空或筛选前那一批。
// 这里不靠「结果 id 变没变」去猜：条件含具体的组和选项下标，所以上一项的结果晚到、
// 或结果集恰好没变，都不会误判；超时就是页面真的没进入「已选中且加载完成」，如实报错。
//
// 等待用 page.Timeout 派生出带上限的 page：上限同时约束单次 Eval，且仍是调用方 ctx 的子 ctx，
// 调用方取消时立刻返回。页面里读不到这些状态字段时同样超时报错，不会当成成功。
func waitFilterApplied(page *rod.Page, group, option int, timeout time.Duration) error {
	wp := page.Timeout(timeout)
	for {
		if res, err := wp.Eval(filterAppliedJS, group, option); err == nil && res.Value.Bool() {
			return nil
		}
		select {
		case <-wp.GetContext().Done():
			return fmt.Errorf("%s 内页面没有进入「该项已选中且结果加载完成」的状态: %w", timeout, wp.GetContext().Err())
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// findFilterOption 在筛选面板里定位一个选项：按标签找到组，再在组内按文本找选项。
//
// 全程不用序号。2026-11 起面板里每个选项有两份：真实节点，以及一份
// button-hp-installed / aria-hidden / opacity≈0 / z-index:-1 的诱饵节点（与真实节点交错排列）。
// 早前用 div.tags:nth-child(N) 会落在诱饵上，rod 判定它被真实节点遮挡，等满 60s 超时；
// 即使点中了，点诱饵也是明显的自动化信号。所以同一文本只取不透明度正常的那个。
//
// 作用域必须限定在 div.filter-panel 内且只认 div.tags：页面别处存在同文本的
// 可见元素（顶部频道栏的「图文」「视频」、标签「综合」），放宽会点错地方。
func findFilterOption(page *rod.Page, pf pendingFilter) (*rod.Element, error) {
	groups, err := page.Elements("div.filter-panel div.filters")
	if err != nil {
		return nil, fmt.Errorf("读取筛选面板失败: %w", err)
	}

	for _, group := range groups {
		// 组标签是 div.filters 下的直接子 span
		hasLabel, label, err := group.Has(":scope > span")
		if err != nil {
			return nil, fmt.Errorf("读取筛选组标签失败: %w", err)
		}
		if !hasLabel {
			continue
		}
		text, err := label.Text()
		if err != nil {
			return nil, fmt.Errorf("读取筛选组标签失败: %w", err)
		}
		if strings.TrimSpace(text) != pf.group {
			continue
		}

		options, err := group.Elements("div.tags")
		if err != nil {
			return nil, fmt.Errorf("读取「%s」的选项失败: %w", pf.group, err)
		}

		var available []string
		matched := false
		for _, opt := range options {
			t, err := opt.Text()
			if err != nil {
				return nil, fmt.Errorf("读取「%s」的选项文本失败: %w", pf.group, err)
			}
			t = strings.TrimSpace(t)
			if !slices.Contains(available, t) {
				available = append(available, t)
			}
			if t != pf.option {
				continue
			}

			matched = true
			visible, err := optionVisible(opt)
			if err != nil {
				return nil, fmt.Errorf("读取「%s」的选项「%s」状态失败: %w", pf.group, pf.option, err)
			}
			if visible {
				return opt, nil
			}
		}
		if matched {
			return nil, fmt.Errorf("「%s」里的选项「%s」当前不可见", pf.group, pf.option)
		}
		return nil, fmt.Errorf("「%s」里没有选项「%s」，页面上是：%s",
			pf.group, pf.option, strings.Join(available, "、"))
	}

	return nil, fmt.Errorf("筛选面板里没有「%s」这一组", pf.group)
}

func makeSearchURL(keyword string) string {

	values := url.Values{}
	values.Set("keyword", keyword)
	values.Set("source", "web_explore_feed")

	//https://www.xiaohongshu.com/search_result?keyword=%25E7%258E%258B%25E5%25AD%2590&source=web_search_result_notes
	//https://www.xiaohongshu.com/search_result?keyword=%25E7%258E%258B%25E5%25AD%2590&source=web_explore_feed
	return fmt.Sprintf(configs.BaseURL()+"/search_result?%s", values.Encode())
}
