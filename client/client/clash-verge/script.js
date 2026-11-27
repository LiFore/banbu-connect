// Clash Verge Rev 全局扩展脚本（单机版），由 sub-store/script.js 整理而来。
//
// ISP 代理：只在 Clash Verge 的脚本编辑器里填写 ISP_PROXIES，不要把凭据提交进仓库。
// 节点名须包含 isp_proxy；dialer-proxy 由脚本补上。没有 ISP 节点时留空即可。
// 修改后运行 `node check.js [mihomo 路径]` 检查。
const ISP_PROXIES = [];

// Kill Switch 的 DNS 入口（client/guard）。Windows 的 NRPT 只能用 53 端口；
// macOS 改成 127.0.0.1:1053。
const DNS_LISTEN = "127.0.0.2:53";

const proxyName = "代理模式";
const CHAIN_GROUP = "链式拨号代理组";
const TEST_URL = "http://www.gstatic.com/generate_204";
const ICON_BASE = "https://fastly.jsdelivr.net/gh/clash-verge-rev/clash-verge-rev.github.io@main/docs/assets/icons/";
const LOYALSOLDIER = "https://cdn.jsdelivr.net/gh/Loyalsoldier/clash-rules@release/";
const BLACKMATRIX = "https://cdn.jsdelivr.net/gh/blackmatrix7/ios_rule_script@master/rule/Clash/";
const GEO_BASE = "https://testingcf.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@release/";

// 地区代码前后不能是字母，避免 US 命中 AUS，同时允许 HK01 这样的写法。
const code = (c) => `(^|[^A-Za-z])${c}([^A-Za-z]|$)`;
const REGIONS = [
  { key: "HK", flag: "hk", regex: new RegExp(`香港|${code("HK")}|Hong ?Kong|🇭🇰`) },
  { key: "TW", flag: "tw", regex: new RegExp(`台湾|臺灣|${code("TW")}|Taiwan|🇹🇼`) },
  { key: "SG", flag: "sg", regex: new RegExp(`新加坡|狮城|${code("SG")}|Singapore|🇸🇬`) },
  { key: "JP", flag: "jp", regex: new RegExp(`日本|${code("JP")}|Japan|🇯🇵`) },
  { key: "US", flag: "us", regex: new RegExp(`美国|${code("US")}|${code("USA")}|United States|🇺🇸`) },
];
const INFO_NODE = /剩余流量|下次重置|套餐到期|到期时间|官网|主页|没节点/;
const SUB_DISPLAY_NAMES = { jsyun: "极速云", efanyun: "饿饭云", lmyun: "老猫云", yhyun: "烟火云CN2" };

function main(params) {
  if (!params.proxies) return params;
  params.proxies = params.proxies.filter((p) => !INFO_NODE.test(p.name || ""));
  overwriteRules(params);
  overwriteProxyGroups(params);
  overwriteDns(params);
  return params;
}

function overwriteRules(params) {
  const customRules = [
    "DST-PORT,19302,REJECT",
    "DOMAIN-KEYWORD,thinkphp1,REJECT",
    "DOMAIN-KEYWORD,adobe,REJECT",

    // QUIC 阻断必须排在 Claude 的普通规则之前，否则永远匹配不到。
    // TODO(verify:V11) 只检查了规则顺序，没有发过真实 UDP。
    "AND,((DOMAIN-KEYWORD,anthropic),(NETWORK,udp)),REJECT",
    "AND,((DOMAIN-KEYWORD,claude),(NETWORK,udp)),REJECT",
    "AND,((DOMAIN-SUFFIX,clau.de),(NETWORK,udp)),REJECT",

    "DOMAIN-SUFFIX,ping0.cc,Claude",
    "DOMAIN-SUFFIX,claude.com,Claude",
    "DOMAIN-SUFFIX,claude.ai,Claude",
    "DOMAIN-SUFFIX,clau.de,Claude",
    "DOMAIN-SUFFIX,chatgpt.com,ChatGPT",
    "DOMAIN-SUFFIX,openai.com,ChatGPT",
    "DOMAIN-SUFFIX,oaistatic.com,ChatGPT",
    "DOMAIN-SUFFIX,oaiusercontent.com,ChatGPT",
    "DOMAIN-KEYWORD,anthropic,Claude",
    "DOMAIN-KEYWORD,claude,Claude",
    "DOMAIN,servd-anthropic-website.b-cdn.net,Claude",
    "DOMAIN-SUFFIX,anthropic.com,Claude",
    "DOMAIN-SUFFIX,claudeusercontent.com,Claude",
    "DOMAIN-SUFFIX,claudemcpclient.com,Claude",

    // 严格服务的规则集排在所有直连规则之前。
    "RULE-SET,openai,ChatGPT",
    "RULE-SET,claude,Claude",
  ];

  // IP 类规则统一 no-resolve：不为了匹配规则而在本地解析域名，避免 DNS 泄漏。
  params.rules = [
    ...customRules,
    // 内核下载 geo 数据也走自身规则；固定直连，否则节点不可用时下载失败，依赖 ASN 的严格规则会被跳过。
    // TODO(verify:V6) jsDelivr 的直连可达性是在软路由后面测的，无软路由环境需要复核。
    "DOMAIN,testingcf.jsdelivr.net,DIRECT",
    "RULE-SET,reject,广告拦截",
    "RULE-SET,direct,DIRECT",
    "RULE-SET,private,DIRECT",
    "RULE-SET,cncidr,DIRECT,no-resolve",
    "RULE-SET,lancidr,DIRECT,no-resolve",
    "GEOIP,LAN,DIRECT,no-resolve",
    "GEOIP,CN,DIRECT,no-resolve",
    "RULE-SET,applications,DIRECT",
    "RULE-SET,spotify,Spotify",
    "RULE-SET,telegramcidr,电报消息,no-resolve",
    "RULE-SET,tld-not-cn," + proxyName,
    "RULE-SET,google," + proxyName,
    "RULE-SET,icloud," + proxyName,
    "RULE-SET,apple," + proxyName,
    "RULE-SET,gfw," + proxyName,
    "RULE-SET,greatfire," + proxyName,
    "RULE-SET,proxy," + proxyName,
    "MATCH,漏网之鱼",
  ];

  const provider = (name, behavior, url) => ({
    type: "http",
    behavior,
    url,
    path: `./ruleset/${name}.yaml`,
    interval: 86400,
  });
  const providers = {};
  for (const name of ["reject", "icloud", "apple", "google", "proxy", "direct", "private", "gfw", "greatfire", "tld-not-cn"]) {
    providers[name] = provider(name, "domain", `${LOYALSOLDIER}${name}.txt`);
  }
  for (const name of ["telegramcidr", "cncidr", "lancidr"]) {
    providers[name] = provider(name, "ipcidr", `${LOYALSOLDIER}${name}.txt`);
  }
  providers.applications = provider("applications", "classical", `${LOYALSOLDIER}applications.txt`);
  providers.openai = provider("openai", "classical", `${BLACKMATRIX}OpenAI/OpenAI.yaml`);
  providers.claude = provider("claude", "classical", `${BLACKMATRIX}Claude/Claude.yaml`);
  providers.spotify = provider("spotify", "classical", `${BLACKMATRIX}Spotify/Spotify.yaml`);
  params["rule-providers"] = providers;
}

function overwriteProxyGroups(params) {
  // TODO(verify:V13) 链式代理只用虚拟节点测过，没有走过真实链路。
  params.proxies.push(...ISP_PROXIES.map((p) => ({ ...p, "dialer-proxy": CHAIN_GROUP })));

  const isIsp = (p) => (p.name || "").includes("isp_proxy");
  const ispNames = params.proxies.filter(isIsp).map((p) => p.name);
  const normal = params.proxies.filter((p) => !isIsp(p)).map((p) => p.name);
  // Mihomo 不接受空的 proxies；没有可用节点时失败即阻断，绝不回退到 DIRECT。
  const orReject = (list) => (list.length > 0 ? list : ["REJECT"]);
  const matching = (regex) => normal.filter((name) => regex.test(name));
  const urlTest = (name, proxies) => ({
    name,
    type: "url-test",
    url: TEST_URL,
    interval: 300,
    tolerance: 50,
    proxies,
    hidden: true,
  });
  const loadBalance = (name, strategy, icon) => ({
    name,
    type: "load-balance",
    url: TEST_URL,
    icon: ICON_BASE + icon,
    interval: 300,
    "max-failed-times": 3,
    strategy,
    lazy: true,
    proxies: orReject(normal),
  });

  // 地区组：没有匹配节点的地区不生成，引用方只引用实际存在的组。
  const regional = REGIONS.map((r) => ({ ...r, proxies: matching(r.regex) })).filter((r) => r.proxies.length > 0);
  const regionAuto = regional.map((r) => urlTest(`${r.key} - 自动选择`, r.proxies));
  const others = normal.filter((name) => !/游戏|关注/.test(name));
  if (others.length > 0) regionAuto.push(urlTest("其它 - 自动选择", others));
  const regionManual = regional.map((r) => ({
    name: `${r.key} - 手工选择`,
    type: "select",
    proxies: r.proxies,
    icon: `${ICON_BASE}flags/${r.flag}.svg`,
  }));

  // 按订阅分组：依赖「地区 - 序号 - 订阅名」格式，其它命名的节点不参与。
  // TODO(verify:V14) 多订阅要靠 Sub-Store 合并和重命名，未在 Clash Verge 里验证。
  const bySub = {};
  for (const name of normal) {
    const parts = name.split(" - ");
    const sub = parts.length >= 2 ? parts[parts.length - 1].trim() : "";
    if (sub) (bySub[sub] = bySub[sub] || []).push(name);
  }
  const subAuto = Object.entries(bySub).map(([sub, proxies]) =>
    urlTest(`${SUB_DISPLAY_NAMES[sub] || sub}自动选择组`, proxies)
  );

  const autoNames = [...regionAuto, ...subAuto].map((g) => g.name);
  const regionChoices = [...regionAuto, ...regionManual].map((g) => g.name);
  const ispGroup = ispNames.length > 0
    ? [{ name: "ISP代理 - 手工选择", type: "select", proxies: ispNames, icon: `${ICON_BASE}flags/speed.svg` }]
    : [];
  const chainGroup = ispNames.length > 0
    ? [{ name: CHAIN_GROUP, type: "select", proxies: orReject([...subAuto.map((g) => g.name), ...normal]), icon: `${ICON_BASE}ambulance.svg` }]
    : [];
  // 严格成员：Claude、ChatGPT 的任何路径都不能到达 DIRECT，因此不引用含 DIRECT 的「代理模式」。
  const strict = [...ispGroup.map((g) => g.name), "自动选择", "手动选择"];
  const yanhuo = subAuto.some((g) => g.name === "烟火云CN2自动选择组") ? ["烟火云CN2自动选择组"] : [];

  params["proxy-groups"] = [
    {
      name: proxyName,
      type: "select",
      icon: `${ICON_BASE}adjust.svg`,
      proxies: ["自动选择", "手动选择", "负载均衡 (散列)", "负载均衡 (轮询)", "DIRECT"],
    },
    { name: "手动选择", type: "select", icon: `${ICON_BASE}link.svg`, proxies: orReject(normal) },
    { name: "自动选择", type: "select", icon: `${ICON_BASE}speed.svg`, proxies: [...autoNames, "ALL - 自动选择"] },
    loadBalance("负载均衡 (散列)", "consistent-hashing", "balance.svg"),
    loadBalance("负载均衡 (轮询)", "round-robin", "merry_go.svg"),
    urlTest("ALL - 自动选择", orReject(normal)),
    ...chainGroup,
    { name: "电报消息", type: "select", proxies: [proxyName, ...regionChoices], icon: `${ICON_BASE}telegram.svg` },
    { name: "ChatGPT", type: "select", proxies: [...strict, ...yanhuo], icon: `${ICON_BASE}chatgpt.svg` },
    { name: "Claude", type: "select", proxies: strict, icon: `${ICON_BASE}claude.svg` },
    {
      name: "Spotify",
      type: "select",
      proxies: [proxyName, ...regionChoices],
      icon: "https://storage.googleapis.com/spotifynewsroom-jp.appspot.com/1/2020/12/Spotify_Icon_CMYK_Green.png",
    },
    { name: "漏网之鱼", type: "select", proxies: ["DIRECT", proxyName], icon: `${ICON_BASE}fish.svg` },
    { name: "广告拦截", type: "select", proxies: ["REJECT", "DIRECT", proxyName], icon: `${ICON_BASE}block.svg` },
    ...regionAuto,
    ...subAuto,
    ...regionManual,
    ...ispGroup,
  ];
}

function overwriteDns(params) {
  const cnDnsList = ["218.2.2.2", "https://223.5.5.5/dns-query", "https://1.12.12.12/dns-query"];
  // TODO(verify:V6) 1.1.1.1 的连通性是在软路由后面测的，无软路由环境需要复核。
  const trustDnsList = ["tls://1.1.1.1:853", "https://1.1.1.1/dns-query"];
  const nameserverPolicy = {
    "geosite:cn": cnDnsList,
    "geosite:geolocation-!cn": trustDnsList,
  };

  // 域名形式的节点地址走国内 DNS；IP 形式不需要解析。
  const isIp = (host) => /^\d{1,3}(\.\d{1,3}){3}$/.test(host) || host.includes(":");
  for (const proxy of params.proxies) {
    if (proxy.server && !isIp(proxy.server)) nameserverPolicy[proxy.server] = cnDnsList;
  }

  const { fallback, "fallback-filter": fallbackFilter, ...baseDns } = params.dns || {};
  params.dns = {
    ...baseDns,
    enable: true,
    listen: DNS_LISTEN,
    "enhanced-mode": "fake-ip",
    "fake-ip-range": "198.18.0.1/16",
    "fake-ip-filter": ["*.lan", "*.local", "+.pool.ntp.org", "time.*.com", "+.msftconnecttest.com", "+.msftncsi.com"],
    "prefer-h3": false,
    "default-nameserver": cnDnsList,
    nameserver: trustDnsList,
    "nameserver-policy": nameserverPolicy,
  };

  Object.assign(params, {
    "unified-delay": true,
    "tcp-concurrent": true,
    profile: { "store-selected": true, "store-fake-ip": true },
    sniffer: {
      enable: true,
      sniff: {
        TLS: { ports: [443, 8443] },
        QUIC: { ports: [443, 8443] },
        HTTP: { ports: [80, "8080-8880"], "override-destination": true },
      },
    },
    "geodata-mode": true,
    "geox-url": {
      geoip: `${GEO_BASE}geoip-lite.dat`,
      geosite: `${GEO_BASE}geosite.dat`,
      mmdb: `${GEO_BASE}country-lite.mmdb`,
      asn: `${GEO_BASE}GeoLite2-ASN.mmdb`,
    },
  });
}
