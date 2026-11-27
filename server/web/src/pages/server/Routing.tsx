import { useSearchParams } from 'react-router-dom'
import { Segmented } from 'antd'
import Chains from './Chains'
import Routes from './Routes'

type Tab = 'routes' | 'chains'

// 「线路」页：线路、链式代理合在一页（规则在「方案」页，ADR-064），用 ?tab=chains 记住当前选中项（旧地址 /chains 也跳到这里）。
// 「线路」一直挂载、未选中时隐藏：切到链式代理再切回来，还没保存的修改不会丢。
// 链式代理选中时才挂载：每次切过去都重新读节点和代理组（可能刚在线路里新增了代理组）。
// 页头由各自组件出（标题和操作不同），切换控件放进页头操作区，不再多出一个大标题。
export default function Routing() {
  const [params, setParams] = useSearchParams()
  const tab: Tab = params.get('tab') === 'chains' ? 'chains' : 'routes'

  const switcher = (
    <Segmented<Tab>
      value={tab}
      onChange={(t) => setParams(t === 'chains' ? { tab: 'chains' } : {}, { replace: true })}
      options={[
        { label: '线路', value: 'routes' },
        { label: '链式代理', value: 'chains' },
      ]}
    />
  )

  // 隐藏用外层普通 div 的 hidden，组件根节点自带的 display 样式不会盖掉它。
  return (
    <>
      <div hidden={tab !== 'routes'}>
        <Routes switcher={switcher} />
      </div>
      {tab === 'chains' && <Chains switcher={switcher} />}
    </>
  )
}
