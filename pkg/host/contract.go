package host

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件是契约兼容判定的**唯一事实源**（ADR-0007）。
//
// 判据从「api_version 与核心完全相等」换成「插件实际用到的那部分契约没变」：
// 插件用工具静态生成自己的使用面（Manifest.Uses），核心维护下面这张变更
// 分级表，两者求交即可判定。整体相等退化为「没有声明使用面」时的兜底，
// 而不是唯一路径。
//
// 为什么成员名是字符串而不是枚举常量：88 个成员就要 88 个导出符号，污染
// 契约包命名空间；而判定只有一处、拼错会被 TestContractSurfaceMembersExist
// 抓住。为什么不用结构化的 {Type, Member}：那只是把拼错的机会从一处变两处。

// ChangeClass 是一条契约变更对**按旧版编译的插件**的影响类别。
type ChangeClass string

const (
	// Additive 是旧插件观测不到的变更：新增字段、新增方法、新增哨兵。
	// 判定时整条跳过——纯增量逼所有插件改字符串重编译，正是要消灭的行政成本。
	Additive ChangeClass = "additive"
	// Behavioral 是签名一个字都没变、可观察语义却变了的变更（v10 的
	// Params.Str 空串语义）。这类变更没有编译期信号，是静默错行为的来源，
	// 所以必须逐条记在这里，不能靠"签名没变"推断安全。
	Behavioral ChangeClass = "behavioral"
	// Breaking 是删改签名、删字段、改线格式或包位置。
	Breaking ChangeClass = "breaking"
)

// Change 是分级表的一行：某个版本上、影响某些成员的、某一类变更。
//
// 一个版本可能同时有 breaking 与 behavioral 两类变更（v8 就是：Yield 取代
// Broadcast 是破坏性、Command/Args 改由核心填好是行为性）。合成一行会把
// "只是语义变了"的成员按破坏性一刀切，所以类别必须与成员绑定，按行记。
type Change struct {
	// Version 是这次变更落地的契约版本。
	Version int
	Class   ChangeClass
	// Surface 是受影响的成员，**成员级**（"Params.Str"）而不是符号级
	// （"Params"）：torn 不碰 Params.Str，按符号级判会把它一起拒掉，
	// 等于把要消灭的误判换个地方复现。
	//
	// 同一版本改了多个成员时用逗号分隔（"Event.Command,Event.Args"）；
	// 只有一个条目用 "*"：表示全局变更（v12 的包外置与线协议），
	// 没有任何使用面能让它变得安全。
	Surface string
}

// contractChanges 是 4~13 的完整变更表，按版本升序。
//
// 写这一行的判据只有一条：**这次改动会不会让一个没改代码、按旧版编译的
// 插件产生不同的可观察行为**。会，就是 behavioral 或 breaking；不会，就是
// additive。判断不了的时候按更严重的那档记——拒载是可观测的、能照日志改的，
// 静默错行为不是。
//
// 删除类变更（v8 把 Manifest.Broadcast 换成 Yield）无法用成员名表达：被删的
// 成员已经不在契约包里，写进来会立刻被 TestContractSurfaceMembersExist 判红。
// 这类变更的兜底是 v12 的全局 "*"——所有按 ≤11 编译的插件都在那里被统一拒载，
// 不必逐条记。
var contractChanges = []Change{
	// v4：任务支持会话维度，JobRunner 可能收到非空的 Peer。
	{Version: 4, Class: Additive, Surface: "JobSpec.PerPeer"},

	// v5：Host 新增 Renderer。
	{Version: 5, Class: Additive, Surface: "Host.Renderer"},

	// v6：HTTP 新增三个方法。
	{Version: 6, Class: Additive, Surface: "HTTP.GetJSONWithHeader,HTTP.GetJSONOnce,HTTP.WithTimeout"},

	// v7：非 2xx 的错误类型从 httpx 的换成 *HTTPStatusError——签名不变，
	// 但按错误串/类型判断的插件会换一条分支走。
	{Version: 7, Class: Behavioral, Surface: "HTTP.GetJSON,HTTP.PostJSON,HTTP.GetJSONWithHeader,HTTP.GetJSONOnce"},

	// v8：Yield 取代 Broadcast（删除类，见上），且 Command/Args 改由核心填好
	// ——插件自己 CutPrefix 的逻辑会与核心填的值重叠。
	{Version: 8, Class: Behavioral, Surface: "Event.Command,Event.Args"},

	// v9：Host 新增 Session，Event 新增 Session 字段。
	{Version: 9, Class: Additive, Surface: "Host.Session,Event.Session"},

	// v10：Params.Str 不再把「键存在但值为空串」当未设置；QueryStore.Query
	// 的只读改由 SQLite 连接保证，且不再自动追加 LIMIT。
	{Version: 10, Class: Behavioral, Surface: "Params.Str,QueryStore.Query"},

	// v11：导出三个主动消息失败哨兵。
	{Version: 11, Class: Additive, Surface: "ErrActiveQuota,ErrActiveDisabled,ErrRateLimited"},

	// v12：契约包从 internal/host 外置为 pkg/host，载体从编译期接口变成
	// 线协议。全局变更，没有使用面能让它变得安全。
	{Version: 12, Class: Breaking, Surface: "*"},

	// v13：Manifest 新增两项——Upstream（零值仍是「以核心仓 index.json 为准」）
	// 与 Uses（插件自报的使用面，#56）。两项都是纯增量：按 12 编译的插件根本没有
	// 这两个字段，判定整条跳过。记进来的作用只是让表覆盖本版的全部契约变更。
	{Version: 13, Class: Additive, Surface: "Manifest.Upstream,Manifest.Uses"},
}

// CheckCompat 判断一个按 builtAgainst 编译、声明了 uses 使用面的插件能否被
// 本核心加载。返回 nil 表示可以加载；否则返回的错误**点名具体成员**，
// 让启动日志能直接照抄成"要改什么"，而不是一句"版本不等"。
//
// 算法（ADR-0007 决策 7）：
//
//  1. 插件编译时所对的版本必须 ≤ 核心版本，否则一律拒——向前兼容不做；
//  2. 取 (builtAgainst, APIVersion] 区间内的变更，additive 的跳过；
//  3. 其余每条，成员与使用面相交即拒；成员为 "*" 的一律拒；
//  4. 使用面为空表示依赖每个成员，退化为整体相等（fail-closed）。
//
// 第 4 条的方向不能反：若把空使用面解释成"不依赖任何成员"，生成器一挂就
// 等于所有插件放行，语义变更会静默穿透——比拒载坏得多。
func CheckCompat(builtAgainst string, uses []string) error {
	cur, err := parseContractVersion(APIVersion)
	if err != nil {
		// 本包的常量写错属于代码缺陷，不是插件的错；照原样报出来即可。
		return fmt.Errorf("本核心的契约版本 %q 无法解析: %w", APIVersion, err)
	}
	built, err := parseContractVersion(builtAgainst)
	if err != nil {
		return fmt.Errorf("插件声明的 api_version=%q 不是版本号，无法判定兼容性（本核心 %d）",
			builtAgainst, cur)
	}
	if built > cur {
		// 保留 "api_version" 这个词面：现场排障时它是 grep 的锚点
		// （internal/pluginproc 的用例也按它断言）。
		return fmt.Errorf("插件声明的 api_version=%d 比本核心的 %d 新：向前兼容不做（ADR-0007）",
			built, cur)
	}

	// 空使用面 = 依赖每个成员，退化为「必须与核心同版本」。这条必须在
	// 分级表之前判：纯增量变更对「什么都没声明」的插件也仍然是拒载理由，
	// 因为它没有给出任何"我不用那一块"的证据。
	if len(uses) == 0 {
		if built == cur {
			return nil
		}
		ch, ok := firstNonAdditive(built, cur)
		if !ok {
			return fmt.Errorf("插件按 api %d 编译且未声明使用面（按依赖全部成员处理），"+
				"而本核心是 %d：空使用面要求 api_version 与核心相等", built, cur)
		}
		if ch.Surface == "*" {
			return fmt.Errorf("插件按 api %d 编译且未声明使用面，而 v%d 是全局变更（%s）",
				built, ch.Version, classLabel(ch.Class))
		}
		return fmt.Errorf("插件按 api %d 编译且未声明使用面（按依赖全部成员处理），"+
			"而 %s 在 v%d 有%s变更", built, firstMember(ch.Surface), ch.Version, classLabel(ch.Class))
	}

	for _, ch := range contractChanges {
		// 区间是左开右闭：插件编译时已经对的那一版，它当然知道。
		if ch.Version <= built || ch.Version > cur || ch.Class == Additive {
			continue
		}
		if ch.Surface == "*" {
			return fmt.Errorf("插件按 api %d 编译，而 v%d 是全局变更（%s）：无法按使用面放行",
				built, ch.Version, classLabel(ch.Class))
		}
		for _, member := range strings.Split(ch.Surface, ",") {
			member = strings.TrimSpace(member)
			if member == "" {
				continue
			}
			if usesMember(uses, member) {
				return fmt.Errorf("插件按 api %d 编译且用到 %s，而该成员在 v%d 有%s变更",
					built, member, ch.Version, classLabel(ch.Class))
			}
		}
	}
	return nil
}

// firstNonAdditive 返回 (built, cur] 区间内第一条非增量变更。
// 它只用于给「未声明使用面」的拒载找一个能照抄的理由——那种情况下判定结果
// 已经定了（拒载），差别只在日志能不能点名。
func firstNonAdditive(built, cur int) (Change, bool) {
	for _, ch := range contractChanges {
		if ch.Version > built && ch.Version <= cur && ch.Class != Additive {
			return ch, true
		}
	}
	return Change{}, false
}

// firstMember 取 Surface 的第一个成员名，用于只报一条的场合。
func firstMember(surface string) string {
	for _, member := range strings.Split(surface, ",") {
		if member = strings.TrimSpace(member); member != "" {
			return member
		}
	}
	return surface
}

// usesMember 判断使用面里有没有这个成员。
func usesMember(uses []string, member string) bool {
	for _, u := range uses {
		if u == member {
			return true
		}
	}
	return false
}

// classLabel 是给日志用的类别说法，不是给判别用的（判别一律比 ChangeClass）。
func classLabel(c ChangeClass) string {
	switch c {
	case Behavioral:
		return "行为（签名不变、语义改变）"
	case Breaking:
		return "破坏性"
	case Additive:
		return "纯增量"
	}
	return string(c)
}

// parseContractVersion 把 "13" 这样的字面量解成整数。
//
// 不做 "13.0"、"v13"、区间表达式之类的宽容解析：api_version 是插件手写或
// 由 host.APIVersion 带过来的单个整数，多一种可接受的写法就多一条判据分叉。
func parseContractVersion(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}
