import type { RuleSet } from '../../serverApi'

// 规则集目录（M5-04，ADR-064 扩充）：Loyalsoldier/clash-rules、blackmatrix7/ios_rule_script、MetaCubeX/meta-rules-dat 的常用条目。
// 地址用 jsDelivr；客户端设置了下载镜像时会改写到镜像（M7-04）。地址在 2026-11-23 逐个核实可下载。
const ls = 'https://cdn.jsdelivr.net/gh/Loyalsoldier/clash-rules@release/'
const bm = 'https://cdn.jsdelivr.net/gh/blackmatrix7/ios_rule_script@master/rule/Clash/'
const mc = 'https://cdn.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta/geo/'

export type CatalogSource = 'Loyalsoldier' | 'blackmatrix7' | 'MetaCubeX'
export type CatalogCategory = 'AI' | '基础' | '国内' | '开发与办公' | '社交' | '影音' | '游戏' | '其他'
export const CATEGORIES: CatalogCategory[] = ['AI', '基础', '国内', '开发与办公', '社交', '影音', '游戏', '其他']

export interface CatalogEntry {
  source: CatalogSource
  category: CatalogCategory
  name: string
  label: string
  ruleSet: Omit<RuleSet, 'target'>
  target?: string // 建议的目标
  large?: boolean // 条目很多（数万条以上），客户端内存占用较大
}

const l = (
  category: CatalogCategory,
  name: string,
  label: string,
  behavior: 'domain' | 'ipcidr' | 'classical',
  target?: string,
  large?: boolean,
): CatalogEntry => ({
  source: 'Loyalsoldier',
  category,
  name,
  label,
  target,
  large,
  ruleSet: { name, behavior, url: `${ls}${name}.txt`, ...(behavior === 'ipcidr' ? { no_resolve: true } : {}) },
})
const b = (category: CatalogCategory, name: string, label: string, target?: string, large?: boolean): CatalogEntry => ({
  source: 'blackmatrix7',
  category,
  name: name.toLowerCase(),
  label,
  target,
  large,
  ruleSet: { name: name.toLowerCase(), behavior: 'classical', url: `${bm}${name}/${name}.yaml` },
})
// MetaCubeX：geosite 是域名列表，geoip 是 IP 段；名称加前缀，避免和其他来源重名。
const site = (category: CatalogCategory, file: string, label: string, target?: string, large?: boolean): CatalogEntry => {
  const name = `geosite-${file.replace('!', 'not-')}`
  return {
    source: 'MetaCubeX',
    category,
    name,
    label,
    target,
    large,
    ruleSet: { name, behavior: 'domain', url: `${mc}geosite/${file}.yaml` },
  }
}
const ip = (category: CatalogCategory, file: string, label: string, target?: string): CatalogEntry => {
  const name = `geoip-${file}`
  return {
    source: 'MetaCubeX',
    category,
    name,
    label,
    target,
    ruleSet: { name, behavior: 'ipcidr', url: `${mc}geoip/${file}.yaml`, no_resolve: true },
  }
}

export const CATALOG: CatalogEntry[] = [
  // AI
  b('AI', 'OpenAI', 'OpenAI / ChatGPT', 'ChatGPT'),
  b('AI', 'Claude', 'Claude', 'Claude'),
  b('AI', 'Anthropic', 'Anthropic', 'Claude'),
  b('AI', 'Gemini', 'Gemini'),
  b('AI', 'Copilot', 'Copilot'),
  site('AI', 'openai', 'OpenAI（geosite）', 'ChatGPT'),
  site('AI', 'anthropic', 'Anthropic（geosite）', 'Claude'),
  site('AI', 'google-gemini', 'Gemini（geosite）'),
  site('AI', 'perplexity', 'Perplexity'),
  site('AI', 'xai', 'xAI / Grok'),
  site('AI', 'cursor', 'Cursor'),
  site('AI', 'category-ai-!cn', '境外 AI 服务合集'),
  // 基础
  l('基础', 'reject', '广告拦截', 'domain', 'REJECT'),
  l('基础', 'private', '私有网络域名', 'domain', 'DIRECT'),
  l('基础', 'lancidr', '局域网 IP 段', 'ipcidr', 'DIRECT'),
  l('基础', 'applications', '需直连的程序', 'classical', 'DIRECT'),
  l('基础', 'proxy', '代理域名', 'domain', undefined, true),
  l('基础', 'gfw', 'GFWList', 'domain'),
  l('基础', 'greatfire', 'GreatFire', 'domain'),
  l('基础', 'tld-not-cn', '非中国顶级域名', 'domain'),
  b('基础', 'Lan', '局域网', 'DIRECT'),
  b('基础', 'AdvertisingLite', '广告拦截（精简）', 'REJECT'),
  b('基础', 'Advertising', '广告拦截（完整）', 'REJECT', true),
  b('基础', 'Global', '境外常用服务', undefined, true),
  site('基础', 'private', '私有网络域名（geosite）', 'DIRECT'),
  ip('基础', 'private', '私有网络 IP 段', 'DIRECT'),
  site('基础', 'category-ads-all', '广告域名合集', 'REJECT', true),
  site('基础', 'geolocation-!cn', '境外域名合集', undefined, true),
  // 国内
  l('国内', 'direct', '国内直连域名', 'domain', 'DIRECT', true),
  l('国内', 'cncidr', '国内 IP 段', 'ipcidr', 'DIRECT'),
  l('国内', 'icloud', 'iCloud', 'domain'),
  l('国内', 'apple', 'Apple', 'domain'),
  b('国内', 'China', '国内服务', 'DIRECT'),
  b('国内', 'ChinaMax', '国内服务（完整）', 'DIRECT', true),
  b('国内', 'Tencent', '腾讯', 'DIRECT'),
  b('国内', 'WeChat', '微信', 'DIRECT'),
  b('国内', 'Alibaba', '阿里巴巴', 'DIRECT'),
  b('国内', 'Baidu', '百度', 'DIRECT'),
  b('国内', 'NetEase', '网易', 'DIRECT'),
  b('国内', 'BiliBili', '哔哩哔哩', 'DIRECT'),
  b('国内', 'DouYin', '抖音', 'DIRECT'),
  site('国内', 'cn', '国内域名（geosite）', 'DIRECT', true),
  ip('国内', 'cn', '国内 IP 段（geoip）', 'DIRECT'),
  // 开发与办公
  l('开发与办公', 'google', 'Google', 'domain'),
  b('开发与办公', 'GitHub', 'GitHub'),
  b('开发与办公', 'GitLab', 'GitLab'),
  b('开发与办公', 'Docker', 'Docker'),
  b('开发与办公', 'Python', 'Python / PyPI'),
  b('开发与办公', 'Jetbrains', 'JetBrains'),
  b('开发与办公', 'Vercel', 'Vercel'),
  b('开发与办公', 'Cloudflare', 'Cloudflare'),
  b('开发与办公', 'Microsoft', 'Microsoft'),
  b('开发与办公', 'Bing', 'Bing'),
  b('开发与办公', 'OneDrive', 'OneDrive'),
  b('开发与办公', 'Dropbox', 'Dropbox'),
  b('开发与办公', 'Notion', 'Notion'),
  b('开发与办公', 'Figma', 'Figma'),
  b('开发与办公', 'Slack', 'Slack'),
  b('开发与办公', 'Speedtest', 'Speedtest'),
  site('开发与办公', 'github', 'GitHub（geosite）'),
  site('开发与办公', 'microsoft', 'Microsoft（geosite）'),
  site('开发与办公', 'apple', 'Apple（geosite）'),
  site('开发与办公', 'category-dev', '开发者服务合集'),
  ip('开发与办公', 'google', 'Google IP 段'),
  // 社交
  l('社交', 'telegramcidr', 'Telegram IP 段', 'ipcidr'),
  b('社交', 'Telegram', 'Telegram'),
  b('社交', 'Twitter', 'Twitter / X'),
  b('社交', 'Discord', 'Discord'),
  b('社交', 'Facebook', 'Facebook'),
  b('社交', 'Instagram', 'Instagram'),
  b('社交', 'Line', 'LINE'),
  b('社交', 'LinkedIn', 'LinkedIn'),
  b('社交', 'Reddit', 'Reddit'),
  b('社交', 'Wikipedia', 'Wikipedia'),
  site('社交', 'telegram', 'Telegram（geosite）'),
  ip('社交', 'telegram', 'Telegram IP 段（geoip）'),
  site('社交', 'twitter', 'Twitter（geosite）'),
  site('社交', 'discord', 'Discord（geosite）'),
  // 影音
  b('影音', 'YouTube', 'YouTube'),
  b('影音', 'Netflix', 'Netflix'),
  b('影音', 'Disney', 'Disney+'),
  b('影音', 'HBO', 'HBO'),
  b('影音', 'Hulu', 'Hulu'),
  b('影音', 'PrimeVideo', 'Prime Video'),
  b('影音', 'Spotify', 'Spotify'),
  b('影音', 'AppleMusic', 'Apple Music'),
  b('影音', 'TikTok', 'TikTok'),
  b('影音', 'Twitch', 'Twitch'),
  b('影音', 'Bahamut', '巴哈姆特'),
  b('影音', 'Pixiv', 'Pixiv'),
  b('影音', 'Emby', 'Emby'),
  site('影音', 'youtube', 'YouTube（geosite）'),
  site('影音', 'netflix', 'Netflix（geosite）'),
  ip('影音', 'netflix', 'Netflix IP 段'),
  site('影音', 'spotify', 'Spotify（geosite）'),
  // 游戏
  b('游戏', 'Steam', 'Steam'),
  b('游戏', 'Epic', 'Epic Games'),
  b('游戏', 'EA', 'EA'),
  b('游戏', 'PlayStation', 'PlayStation'),
  b('游戏', 'Xbox', 'Xbox'),
  b('游戏', 'Nintendo', 'Nintendo'),
  site('游戏', 'steam', 'Steam（geosite）'),
  // 其他
  b('其他', 'Amazon', 'Amazon'),
  b('其他', 'PayPal', 'PayPal'),
  b('其他', 'Binance', 'Binance'),
  b('其他', 'Crypto', '加密货币'),
]
