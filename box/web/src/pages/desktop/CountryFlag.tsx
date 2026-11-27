import { useEffect, useState } from 'react'
import { Tooltip } from 'antd'

// 出口的国家 / 地区旗帜：flag-icons（MIT）的 4:3 SVG。只打包常见出口地的旗帜（懒加载，用到才取），
// 其余显示两位代码，避免把 270 多面旗帜都编进客户端和路由盒的二进制。
const flags = import.meta.glob<string>(
  '../../../node_modules/flag-icons/flags/4x3/{us,ca,br,ar,cl,gb,ie,fr,de,nl,be,lu,ch,at,it,pt,se,no,fi,dk,is,pl,cz,hu,ro,bg,gr,tr,ua,ru,il,ae,sa,qa,in,pk,bd,th,vn,my,sg,id,ph,kh,hk,mo,tw,cn,jp,kr,mn,au,nz,za,eg,ng,ke,kz}.svg',
  { query: '?url', import: 'default' },
)

const cache = new Map<string, string>()

const loader = (code: string) => flags[`../../../node_modules/flag-icons/flags/4x3/${code}.svg`]

let names: Intl.DisplayNames | undefined
const regionName = (code: string) => {
  try {
    names ??= new Intl.DisplayNames(['zh-CN'], { type: 'region' })
    return names.of(code) ?? code
  } catch {
    return code
  }
}

export function countryName(code?: string) {
  return code ? regionName(code.toUpperCase()) : ''
}

// 旗帜 + 可选的国家 / 地区名称；没有代码时不显示。
export default function CountryFlag({ code, showName = false }: { code?: string; showName?: boolean }) {
  const cc = (code ?? '').toLowerCase()
  // 取到的地址按代码缓存在模块里，同一面旗帜只加载一次。
  const [loaded, setLoaded] = useState<{ cc: string; src: string }>()
  useEffect(() => {
    let live = true
    const load = loader(cc)
    if (load && !cache.has(cc)) void load().then((u) => (cache.set(cc, u), live && setLoaded({ cc, src: u })), () => undefined)
    return () => {
      live = false
    }
  }, [cc])
  const src = cache.get(cc) ?? (loaded?.cc === cc ? loaded.src : undefined)
  if (!/^[a-z]{2}$/.test(cc)) return null
  const name = regionName(cc.toUpperCase())
  const badge = src ? <img className="country-flag" src={src} alt={name} draggable={false} /> : <span className="country-flag country-flag-code">{cc.toUpperCase()}</span>
  return (
    <Tooltip title={showName ? undefined : `${name}（${cc.toUpperCase()}）`}>
      <span className="country-flag-wrap">
        {badge}
        {showName && <span>{name}</span>}
      </span>
    </Tooltip>
  )
}
