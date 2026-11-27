package sub

import (
	"sort"
	"strings"
)

// RegionOther 是 KeepUnmatched 时无法识别地区的节点归属。
const RegionOther = "其他"

type region struct {
	Name     string
	Flag     string
	Keywords []string
}

// regions 迁移自 sub-store/operate.js（M2-06）。
// 不使用 2 字母 ISO 代码做关键词，避免与流量等文本误匹配。
var regions = []region{
	// 亚洲
	{"香港", "🇭🇰", []string{"香港", "HongKong", "Hong Kong"}},
	{"日本", "🇯🇵", []string{"日本", "Japan"}},
	{"新加坡", "🇸🇬", []string{"新加坡", "Singapore"}},
	{"台湾", "🇹🇼", []string{"台湾", "臺灣", "Taiwan"}},
	{"韩国", "🇰🇷", []string{"韩国", "韓國", "Korea"}},
	{"泰国", "🇹🇭", []string{"泰国", "Thailand"}},
	{"越南", "🇻🇳", []string{"越南", "Vietnam"}},
	{"印度尼西亚", "🇮🇩", []string{"印度尼西亚", "印尼", "Indonesia"}},
	{"印度", "🇮🇳", []string{"印度", "India"}},
	{"菲律宾", "🇵🇭", []string{"菲律宾", "Philippines"}},
	{"马来西亚", "🇲🇾", []string{"马来西亚", "Malaysia"}},
	{"柬埔寨", "🇰🇭", []string{"柬埔寨", "Cambodia"}},
	{"缅甸", "🇲🇲", []string{"缅甸", "Myanmar"}},
	{"老挝", "🇱🇦", []string{"老挝", "Laos"}},
	{"哈萨克斯坦", "🇰🇿", []string{"哈萨克斯坦", "Kazakhstan"}},
	{"巴基斯坦", "🇵🇰", []string{"巴基斯坦", "Pakistan"}},
	{"孟加拉国", "🇧🇩", []string{"孟加拉", "Bangladesh"}},
	{"蒙古", "🇲🇳", []string{"蒙古", "Mongolia"}},
	{"尼泊尔", "🇳🇵", []string{"尼泊尔", "Nepal"}},
	// 中东
	{"迪拜", "🇦🇪", []string{"迪拜", "阿联酋", "Dubai"}},
	{"以色列", "🇮🇱", []string{"以色列", "Israel"}},
	{"沙特阿拉伯", "🇸🇦", []string{"沙特", "Saudi"}},
	{"卡塔尔", "🇶🇦", []string{"卡塔尔", "Qatar"}},
	{"伊朗", "🇮🇷", []string{"伊朗", "Iran"}},
	// 北美
	{"美国", "🇺🇸", []string{"美国", "United States"}},
	{"加拿大", "🇨🇦", []string{"加拿大", "Canada"}},
	{"墨西哥", "🇲🇽", []string{"墨西哥", "Mexico"}},
	// 南美
	{"巴西", "🇧🇷", []string{"巴西", "Brazil"}},
	{"阿根廷", "🇦🇷", []string{"阿根廷", "Argentina"}},
	{"智利", "🇨🇱", []string{"智利", "Chile"}},
	{"哥伦比亚", "🇨🇴", []string{"哥伦比亚", "Colombia"}},
	{"秘鲁", "🇵🇪", []string{"秘鲁", "Peru"}},
	// 西欧 / 中欧
	{"英国", "🇬🇧", []string{"英国", "United Kingdom"}},
	{"法国", "🇫🇷", []string{"法国", "France"}},
	{"德国", "🇩🇪", []string{"德国", "Germany"}},
	{"意大利", "🇮🇹", []string{"意大利", "Italy"}},
	{"西班牙", "🇪🇸", []string{"西班牙", "Spain"}},
	{"葡萄牙", "🇵🇹", []string{"葡萄牙", "Portugal"}},
	{"荷兰", "🇳🇱", []string{"荷兰", "Netherlands"}},
	{"比利时", "🇧🇪", []string{"比利时", "Belgium"}},
	{"瑞士", "🇨🇭", []string{"瑞士", "Switzerland"}},
	{"奥地利", "🇦🇹", []string{"奥地利", "Austria"}},
	{"爱尔兰", "🇮🇪", []string{"爱尔兰", "Ireland"}},
	{"卢森堡", "🇱🇺", []string{"卢森堡", "Luxembourg"}},
	// 北欧
	{"瑞典", "🇸🇪", []string{"瑞典", "Sweden"}},
	{"挪威", "🇳🇴", []string{"挪威", "Norway"}},
	{"芬兰", "🇫🇮", []string{"芬兰", "Finland"}},
	{"丹麦", "🇩🇰", []string{"丹麦", "Denmark"}},
	{"冰岛", "🇮🇸", []string{"冰岛", "Iceland"}},
	// 东欧 / 巴尔干
	{"俄罗斯", "🇷🇺", []string{"俄罗斯", "Russia"}},
	{"乌克兰", "🇺🇦", []string{"乌克兰", "Ukraine"}},
	{"波兰", "🇵🇱", []string{"波兰", "Poland"}},
	{"捷克", "🇨🇿", []string{"捷克", "Czech"}},
	{"斯洛伐克", "🇸🇰", []string{"斯洛伐克", "Slovakia"}},
	{"匈牙利", "🇭🇺", []string{"匈牙利", "Hungary"}},
	{"罗马尼亚", "🇷🇴", []string{"罗马尼亚", "Romania"}},
	{"保加利亚", "🇧🇬", []string{"保加利亚", "Bulgaria"}},
	{"摩尔多瓦", "🇲🇩", []string{"摩尔多瓦", "Moldova"}},
	{"立陶宛", "🇱🇹", []string{"立陶宛", "Lithuania"}},
	{"拉脱维亚", "🇱🇻", []string{"拉脱维亚", "Latvia"}},
	{"爱沙尼亚", "🇪🇪", []string{"爱沙尼亚", "Estonia"}},
	{"塞尔维亚", "🇷🇸", []string{"塞尔维亚", "Serbia"}},
	{"克罗地亚", "🇭🇷", []string{"克罗地亚", "Croatia"}},
	{"斯洛文尼亚", "🇸🇮", []string{"斯洛文尼亚", "Slovenia"}},
	{"北马其顿", "🇲🇰", []string{"北马其顿", "马其顿", "Macedonia"}},
	{"希腊", "🇬🇷", []string{"希腊", "Greece"}},
	{"土耳其", "🇹🇷", []string{"土耳其", "Turkey"}},
	// 大洋洲
	{"澳大利亚", "🇦🇺", []string{"澳大利亚", "澳洲", "Australia"}},
	{"新西兰", "🇳🇿", []string{"新西兰", "New Zealand"}},
	// 非洲
	{"南非", "🇿🇦", []string{"南非", "South Africa"}},
	{"埃及", "🇪🇬", []string{"埃及", "Egypt"}},
	{"尼日利亚", "🇳🇬", []string{"尼日利亚", "Nigeria"}},
}

type keywordEntry struct {
	keyword, region string
}

// keywordIndex 按关键词长度降序，保证“印度尼西亚”先于“印度”匹配。
var keywordIndex = func() []keywordEntry {
	var idx []keywordEntry
	for _, r := range regions {
		for _, k := range r.Keywords {
			idx = append(idx, keywordEntry{k, r.Name})
		}
	}
	sort.SliceStable(idx, func(i, j int) bool {
		return len([]rune(idx[i].keyword)) > len([]rune(idx[j].keyword))
	})
	return idx
}()

// MatchRegion 按节点名识别地区：旗帜优先（无子串歧义），其次关键词。未命中返回空串。
func MatchRegion(name string) string {
	for _, r := range regions {
		if strings.Contains(name, r.Flag) {
			return r.Name
		}
	}
	for _, e := range keywordIndex {
		if strings.Contains(name, e.keyword) {
			return e.region
		}
	}
	return ""
}

// RegionNames 返回内置地区名列表（供前端筛选）。
func RegionNames() []string {
	names := make([]string, len(regions))
	for i, r := range regions {
		names[i] = r.Name
	}
	return names
}
