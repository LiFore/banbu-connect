// 用虚拟节点检查 script.js：node check.js [mihomo 路径]
// 给出 mihomo 路径时会额外运行 `mihomo -t`（需要联网下载 geo 数据，不启动 TUN）。
const assert = require("assert");
const fs = require("fs");
const os = require("os");
const path = require("path");
const vm = require("vm");
const { spawnSync } = require("child_process");

const source = fs.readFileSync(path.join(__dirname, "script.js"), "utf8");
const mihomo = process.argv[2];
const BUILTIN = new Set(["DIRECT", "REJECT"]);
const STRICT_GROUPS = ["Claude", "ChatGPT"];
const PROTECTED = JSON.parse(fs.readFileSync(path.join(__dirname, "..", "guard", "protected.json"), "utf8"));
const DUMMY_ISP = `const ISP_PROXIES = [{ name: "dummy-isp_proxy", type: "http", server: "isp.example.test", port: 8080 }];`;

const SCENARIOS = {
  "重命名后的多订阅": { nodes: ["香港 - 01 - yhyun", "台湾 - 01 - jsyun", "日本 - 01 - jsyun", "美国 - 01 - lmyun", "新加坡 - 01 - efanyun"] },
  "没有烟火云": { nodes: ["香港 - 01 - jsyun", "日本 - 01 - jsyun", "美国 - 01 - lmyun"] },
  "机场原始名称": { nodes: ["🇭🇰 HK-IPLC-01", "🇯🇵 Japan 02", "🇺🇸 US Premium 03", "AUS Sydney 01", "剩余流量：10GB", "套餐到期：2027-01-01"] },
  "只有信息节点": { nodes: ["剩余流量：10GB"] },
  "带 ISP 节点": { nodes: ["香港 - 01 - jsyun", "美国 - 01 - lmyun"], isp: true },
};

function generate({ nodes, isp }) {
  const code = isp ? source.replace("const ISP_PROXIES = [];", DUMMY_ISP) : source;
  const ctx = {};
  vm.createContext(ctx);
  vm.runInContext(code, ctx);
  const proxies = nodes.map((name, i) => ({
    name, type: "ss", server: `198.51.100.${i + 1}`, port: 8388, cipher: "aes-128-gcm", password: "dummy",
  }));
  return JSON.parse(JSON.stringify(ctx.main({ proxies })));
}

function pathsToDirect(config, start) {
  const groups = new Map(config["proxy-groups"].map((g) => [g.name, g]));
  const found = [];
  const walk = (name, trail) => {
    if (name === "DIRECT") found.push([...trail, name].join(" → "));
    if (trail.includes(name) || !groups.has(name)) return;
    for (const member of groups.get(name).proxies) walk(member, [...trail, name]);
  };
  walk(start, []);
  return found;
}

function checkConfig(config) {
  const groups = new Map(config["proxy-groups"].map((g) => [g.name, g]));
  const proxyNames = new Set(config.proxies.map((p) => p.name));
  const exists = (ref) => BUILTIN.has(ref) || groups.has(ref) || proxyNames.has(ref);

  for (const g of config["proxy-groups"]) {
    assert(g.proxies.length > 0, `空组：${g.name}`);
    for (const member of g.proxies) assert(exists(member), `${g.name} 引用了不存在的 ${member}`);
  }
  for (const p of config.proxies) {
    if (p["dialer-proxy"]) assert(groups.has(p["dialer-proxy"]), `${p.name} 的前置不存在`);
    assert(!/剩余流量|套餐到期/.test(p.name), `信息节点未过滤：${p.name}`);
  }
  for (const name of STRICT_GROUPS) {
    assert.deepStrictEqual(pathsToDirect(config, name), [], `严格组可达 DIRECT`);
  }

  const rules = config.rules;
  const index = (prefix) => rules.findIndex((r) => r.startsWith(prefix));
  for (const r of rules) {
    const target = r.startsWith("AND,") ? r.slice(r.lastIndexOf(",") + 1) : r.split(",")[r.startsWith("MATCH") ? 1 : 2];
    assert(exists(target), `规则目标不存在：${r}`);
    if (r.startsWith("RULE-SET,")) assert(config["rule-providers"][r.split(",")[1]], `规则集不存在：${r}`);
  }
  for (const keyword of ["anthropic", "claude"]) {
    const block = index(`AND,((DOMAIN-KEYWORD,${keyword})`);
    assert(block >= 0 && block < index(`DOMAIN-KEYWORD,${keyword},`), `${keyword} 的 QUIC 阻断不在普通规则之前`);
  }
  const firstDirect = rules.findIndex((r) => r.endsWith(",DIRECT") || r.includes(",DIRECT,"));
  for (const strictRule of ["RULE-SET,openai,", "RULE-SET,claude,"]) {
    assert(index(strictRule) < firstDirect, `${strictRule} 排在直连规则之后`);
  }
  // 受保护域名拿到 fake-ip 后全靠规则分流：没有严格规则的话，内核会替它直连。
  for (const suffix of PROTECTED.suffixes) {
    const rule = rules.findIndex((r) => STRICT_GROUPS.some((g) => r === `DOMAIN-SUFFIX,${suffix},${g}`));
    assert(rule >= 0 && rule < firstDirect, `受保护域名 ${suffix} 缺少直连规则之前的严格规则`);
    assert(!config.dns["fake-ip-filter"].some((f) => f.includes(suffix)), `受保护域名 ${suffix} 被排除在 fake-ip 之外`);
  }
  assert.strictEqual(config.dns["enhanced-mode"], "fake-ip");
  assert(config.dns.listen, "缺少 dns.listen");

  assert.strictEqual(rules.filter((r) => r.startsWith("MATCH,")).length, 1);
  assert.strictEqual(rules[rules.length - 1].split(",")[0], "MATCH");

  assert(!("fallback" in config.dns) && !("fallback-filter" in config.dns), "仍在使用旧式 fallback");
  assert(!config.dns["nameserver-policy"]["geosite:cn"].some((s) => s.includes("8.8.8.8")), "国内 DNS 含境外上游");
}

function runMihomoTest(name, config) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "verge-check-"));
  const tested = {
    ...config,
    "mixed-port": 17890,
    tun: { enable: true, stack: "mixed", "auto-route": true, "auto-detect-interface": true, "dns-hijack": ["any:53"] },
  };
  const file = path.join(dir, "config.yaml");
  fs.writeFileSync(file, JSON.stringify(tested, null, 1));
  const result = spawnSync(mihomo, ["-t", "-d", dir, "-f", file], { encoding: "utf8", timeout: 180000 });
  fs.rmSync(dir, { recursive: true, force: true });
  assert.strictEqual(result.status, 0, `mihomo -t 失败（${name}）：\n${(result.stdout + result.stderr).slice(-800)}`);
}

// 仓库里的脚本不能带凭据。
assert(source.includes("const ISP_PROXIES = [];"), "ISP_PROXIES 必须留空后才能提交");

const regions = generate(SCENARIOS["机场原始名称"])["proxy-groups"].map((g) => g.name);
assert(!regions.includes("TW - 自动选择"), "没有台湾节点时不应生成台湾组");
const auto = generate(SCENARIOS["重命名后的多订阅"])["proxy-groups"].find((g) => g.name === "HK - 自动选择");
assert.deepStrictEqual(auto.proxies, ["香港 - 01 - yhyun"], "地区组未匹配到重命名后的节点");
const us = generate(SCENARIOS["机场原始名称"])["proxy-groups"].find((g) => g.name === "US - 自动选择");
assert.deepStrictEqual(us.proxies, ["🇺🇸 US Premium 03"], "US 不应命中 AUS");

let failed = 0;
for (const [name, scenario] of Object.entries(SCENARIOS)) {
  try {
    const config = generate(scenario);
    checkConfig(config);
    if (mihomo) runMihomoTest(name, config);
    console.log(`PASS  ${name}`);
  } catch (err) {
    failed++;
    console.log(`FAIL  ${name}: ${err.message}`);
  }
}
process.exit(failed ? 1 : 0);
