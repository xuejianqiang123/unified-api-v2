// panel_ui_test.go 面板静态页面的引用一致性守卫。
//
// 背景：panelweb/index.html 由 //go:embed 编进二进制，JS 语法错误或「删了元素忘了删引用」
// 不会让 go build 失败，只会在浏览器里静默报错——用户看到的就是「页面没整改成功」。
// 这里把能自动判死的两类问题做成回归判据：
//  1. JS 里 getElementById 的每个字面量 id 都必须在 HTML 中真实存在；
//  2. 概览/请求日志的关键信息块必须存在于页面，同时被用户明确要求删掉的旧块不得回流。
package server

import (
	"regexp"
	"strings"
	"testing"
)

var (
	reGetByIDLit = regexp.MustCompile(`getElementById\('([A-Za-z0-9_-]+)'\)`)
	reHTMLIDAttr = regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)
)

func TestPanelHTMLReferencedIDsExist(t *testing.T) {
	html := string(panelIndexHTML)
	ids := map[string]bool{}
	for _, m := range reHTMLIDAttr.FindAllStringSubmatch(html, -1) {
		ids[m[1]] = true
	}
	var missing []string
	for _, m := range reGetByIDLit.FindAllStringSubmatch(html, -1) {
		if !ids[m[1]] {
			missing = append(missing, m[1])
		}
	}
	if len(missing) > 0 {
		t.Fatalf("JS 引用了不存在的元素 id（运行时会静默失败）: %v", missing)
	}
}

func TestPanelUIIsnapshot(t *testing.T) {
	html := string(panelIndexHTML)
	// 用户 2026-11-21 明确要求的内容必须在页面里。
	want := []string{
		"实时使用",                               // 概览：替代原「账号健康摘要」
		"优先级控制",                              // Provider 行内入口 + 弹窗
		"renderRecent(",                      // 请求流水渲染
		"最近请求",                               // 请求日志页首屏
		"按模型汇总",                              // 日志页：模型维度
		"按账号汇总",                              // 账号页：管理与运行合表后的副标题（原号池页标题，合并后保留）
		"客户端接入",                              // 接入页：凭证块（原「密钥」页）
		"全局别名表",                              // 接入页：别名表（原「模型别名」页）
		"对话测试台",                              // 概览页：原「测试台」页并入
		`id="task-checkin"`,                  // 概览页：原「任务」页签到表并入
		"导入本机 Trae",                          // 账号页：从本机客户端读凭证入池（不跳外部控制台）
		"导入本机豆包",                             // 账号页：从豆包客户端读 cookie 入池
		"importDoubaoLocal(",                 // 账号页：对应处理器必须接线
		"removeDoubaoAcc(",                   // 账号页：豆包账号彻底删除（含 accounts.json）
		"accounts/db/remove",                 // 账号页：删除端点必须接线
		"trae/accounts/add",                  // 账号页：手动粘贴 JWT 兜底
		"当前状态",                               // 概览：Trae 无签到历史，时间列显式标注为当前状态
		`<option value="trae">Trae</option>`, // 日志页：Provider 筛选含 Trae
		"ov-providers-hint",                  // 概览：选号模式并入说明行
		`id="acc-mode-btn"`,                  // 账号页：选号模式开关（原概览「账号优先级」列承载）
		"cycleMode(",                         // 账号页：选号模式切换处理器
		"updateModeBtn(",                     // 账号页：按钮文案随真实模式刷新
		"onProvDrop(",                        // 概览：Provider 行拖拽排序
		"trae/accounts/reorder",              // 账号页：Trae 拖拽排序的代理端点
	}
	for _, w := range want {
		if !strings.Contains(html, w) {
			t.Errorf("页面缺少必需内容 %q", w)
		}
	}
	// 被明确要求删除的旧内容不得回流。
	notWant := []string{
		`id="ov-account-summary"`, // 账号健康摘要整块
		"<th>别名数</th>",            // Provider 表的别名数列
		// 2026-11-21 用户要求「账号 / 号池」合并为一页，以下结构不得回归：
		`data-v="pool"`, // 独立「号池」导航与视图
		`id="pool-acc"`, // 号池独立表
		`id="acc-wb"`,   // 分表（WorkBuddy）
		`id="acc-db"`,   // 分表（豆包）
		// 2026-11-21 用户要求「密钥 / 模型别名」合并为一页「接入」，以下结构不得回归：
		`data-v="keys"`,   // 独立「密钥」导航与视图
		`data-v="models"`, // 独立「模型别名」导航与视图
		// 2026-11-21 用户要求「测试台 / 任务」并入概览，且概览只留两张健康卡：
		`data-v="test"`,   // 独立「测试台」导航与视图
		`data-v="tasks"`,  // 独立「任务」导航与视图
		"alias_breakdown", // 已删「全局别名」卡片的数据字段（改用此判据，避免误伤接入页「全局别名表」）
		"需关注",             // 已删的第二张卡片标题
		// 2026-11-22 用户质疑「设置页存在的意义」：只读 JSON 快照与别处重复，已删除。
		`data-v="settings"`,
		`id="settings-json"`,
		"setProviderFirst", // 已删的「豆包优先/WorkBuddy 优先」按钮处理器（拖拽替代）
		"账号优先级",            // 已删的概览列（操作都在账号页）
		"openPrio(",        // 已删的优先级控制弹窗（拖拽替代）
		"setMode(",         // 已删的模式切换（改由 cycleMode 承担）
		`id="ov-porder"`,   // 已删的「X 优先」徽标（与顺序列重复）
		`id="ov-mode"`,     // 已删的「选号：优先级」徽标（并入说明行）
		"运行配置快照",
	}
	for _, n := range notWant {
		if strings.Contains(html, n) {
			t.Errorf("页面仍含已删除内容 %q", n)
		}
	}
}

// TestPanelDragRowEmitsNoTrailingAngle 守卫表格行拼装的结构正确性。
//
// 背景（2026-11-21 用户反馈红框）：dragRow 曾返回 ' ... title="拖拽调整优先级">'
// ——自带一个结尾 '>'，而调用方写的是 '<tr'+dragRow(...)+'>'，于是每行多出一个
// 文本节点。HTML 表格模型会把 <tr> 内的裸文本提升（foster parent）到表格外，
// 页面上表现为表格左侧冒出一串 '>' 残字，且不报任何错误。
//
// 判据：dragRow 的返回串不得以 '>' 结束；调用点必须仍是 '<tr'+dragRow(...)+'>'。
// 任一条不成立即失败——这不是风格检查，是渲染正确性。
func TestPanelDragRowEmitsNoTrailingAngle(t *testing.T) {
	html := string(panelIndexHTML)
	i := strings.Index(html, "function dragRow(")
	if i < 0 {
		t.Fatal("未找到 dragRow 定义")
	}
	j := strings.Index(html[i:], "\n}")
	if j < 0 {
		t.Fatal("dragRow 定义未闭合")
	}
	body := html[i : i+j+2]

	if strings.Contains(body, `title="拖拽调整优先级">'`) {
		t.Fatal("dragRow 返回值末尾多了一个 '>'：调用方 '<tr'+dragRow(...)+'>' 会在每行多出文本节点，" +
			"浏览器把表格内裸文本提升到表格外，页面出现 '>' 残字")
	}
	if !strings.Contains(body, `title="拖拽调整优先级"'`) {
		t.Fatal("dragRow 返回串形态已变化（未找到 title 结尾的单引号），需同步本守卫")
	}
	if !strings.Contains(html, `'<tr'+dragRow(`) {
		t.Fatal("dragRow 调用点写法已变化（未找到 '<tr'+dragRow(），需同步本守卫")
	}
}
