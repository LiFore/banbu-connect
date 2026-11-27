import { useEffect, useState } from 'react'
import { Alert, App, AutoComplete, Button, Divider, Form, Input, Modal, Space, Switch, Typography } from 'antd'
import { CheckCircleFilled, CloudSyncOutlined, WarningFilled } from '@ant-design/icons'
import { serverApi, type StorageConfig, type StorageTest } from '../../serverApi'

// 存储设置（S1.11）：阿里云 OSS。启用后新上传的更新包和云盘文件存到 OSS，客户端经签名地址直接从 OSS 下载；
// 未启用时存在管理服务的数据目录。已有文件可一键迁移到 OSS。

const regions = [
  ['cn-hangzhou', '华东 1（杭州）'],
  ['cn-shanghai', '华东 2（上海）'],
  ['cn-qingdao', '华北 1（青岛）'],
  ['cn-beijing', '华北 2（北京）'],
  ['cn-zhangjiakou', '华北 3（张家口）'],
  ['cn-shenzhen', '华南 1（深圳）'],
  ['cn-guangzhou', '华南 3（广州）'],
  ['cn-chengdu', '西南 1（成都）'],
  ['cn-hongkong', '中国香港'],
  ['ap-southeast-1', '新加坡'],
  ['us-west-1', '美国（硅谷）'],
].map(([value, label]) => ({ value, label: `${value} · ${label}` }))

type FormValues = Pick<StorageConfig, 'enabled' | 'region' | 'endpoint' | 'bucket' | 'access_key_id' | 'access_key_secret' | 'prefix'>

export default function StorageSettings({ open, onClose, onSaved }: { open: boolean; onClose: () => void; onSaved: (c: StorageConfig) => void }) {
  const { message } = App.useApp()
  const [form] = Form.useForm<FormValues>()
  const [config, setConfig] = useState<StorageConfig>()
  const [test, setTest] = useState<StorageTest>()
  const [busy, setBusy] = useState<'' | 'test' | 'save' | 'cors' | 'migrate'>('')

  // 每次打开时重新挂载（见 Files.tsx），这里只在挂载时读一次配置。
  useEffect(() => {
    serverApi.storage().then(
      (c) => {
        setConfig(c)
        form.setFieldsValue({ ...c, access_key_secret: '' })
      },
      (e: Error) => message.error(e.message),
    )
  }, [form, message])

  const run = async (kind: typeof busy, f: () => Promise<void>) => {
    setBusy(kind)
    try {
      await f()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setBusy('')
    }
  }

  const doTest = () =>
    run('test', async () => {
      const v = await form.validateFields()
      setTest(await serverApi.testStorage(v))
    })

  const save = () =>
    run('save', async () => {
      const v = await form.validateFields()
      const c = await serverApi.saveStorage(v)
      setConfig(c)
      form.setFieldValue('access_key_secret', '')
      onSaved(c)
      message.success(c.enabled ? '已保存，新上传的文件将存到 OSS' : '已保存')
    })

  const fixCORS = () =>
    run('cors', async () => {
      await serverApi.fixStorageCORS()
      message.success('已添加跨域规则')
      setTest(await serverApi.testStorage(form.getFieldsValue()))
    })

  const migrate = () =>
    run('migrate', async () => {
      const r = await serverApi.migrateStorage()
      const c = await serverApi.storage()
      setConfig(c)
      onSaved(c)
      if (r.errors.length) {
        Modal.warning({ title: `已迁移 ${r.releases} 个更新包、${r.files} 个文件，${r.errors.length} 个失败`, content: r.errors.join('\n') })
      } else {
        message.success(`已迁移 ${r.releases} 个更新包、${r.files} 个文件`)
      }
    })

  const localCount = (config?.local_releases ?? 0) + (config?.local_files ?? 0)

  return (
    <Modal
      title="存储设置"
      open={open}
      onCancel={onClose}
      width={620}
      destroyOnHidden
      footer={
        <Space>
          <Button onClick={doTest} loading={busy === 'test'}>
            测试连接
          </Button>
          <Button type="primary" onClick={save} loading={busy === 'save'}>
            保存
          </Button>
        </Space>
      }
    >
      <Space orientation="vertical" size={12} style={{ width: '100%' }}>
        <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>
          启用阿里云 OSS 后，新上传的客户端更新包和文件都存到 OSS（私有读写），客户端用临时签名地址直接从 OSS 下载，不占管理服务的带宽。
          已存在的文件仍从原处读取，可以在下方迁移。
        </Typography.Paragraph>
        <Form form={form} layout="vertical" requiredMark={false} className="storage-form">
          <Form.Item name="enabled" label="使用 OSS 存储新文件" valuePropName="checked">
            <Switch />
          </Form.Item>
          <div className="storage-grid">
            <Form.Item name="region" label="地域" rules={[{ required: true, message: '如 cn-hangzhou' }]}>
              <AutoComplete options={regions} placeholder="cn-hangzhou" filterOption={(i, o) => (o?.value ?? '').includes(i)} />
            </Form.Item>
            <Form.Item name="bucket" label="Bucket" rules={[{ required: true, message: '填写 Bucket 名称' }]}>
              <Input placeholder="banbu-files" autoComplete="off" />
            </Form.Item>
            <Form.Item name="access_key_id" label="AccessKey ID" rules={[{ required: true, message: '填写 AccessKey ID' }]}>
              <Input autoComplete="off" />
            </Form.Item>
            <Form.Item
              name="access_key_secret"
              label="AccessKey Secret"
              rules={[{ required: !config?.has_secret, message: '填写 AccessKey Secret' }]}
            >
              <Input.Password placeholder={config?.has_secret ? '已保存，留空不改' : ''} autoComplete="new-password" />
            </Form.Item>
            <Form.Item name="prefix" label="路径前缀（可选）" tooltip="文件放在桶里的这个目录下，如 banbu">
              <Input placeholder="banbu" autoComplete="off" />
            </Form.Item>
            <Form.Item name="endpoint" label="Endpoint（可选）" tooltip="留空时按地域使用公网地址 oss-<地域>.aliyuncs.com">
              <Input placeholder="oss-cn-hangzhou.aliyuncs.com" autoComplete="off" />
            </Form.Item>
          </div>
        </Form>
        <Typography.Text type="secondary" className="hint">
          建议用 RAM 子账号，只授权这个桶的 PutObject、GetObject、DeleteObject、分片上传相关权限，以及 GetBucketCors、PutBucketCors（用于自动配置跨域）。
        </Typography.Text>

        {test && (
          <div className="storage-test">
            {test.ok ? (
              <div className="storage-test-row ok">
                <CheckCircleFilled /> 读写正常
              </div>
            ) : (
              <div className="storage-test-row bad">
                <WarningFilled /> {test.error}
              </div>
            )}
            {test.ok &&
              (test.cors_ready ? (
                <div className="storage-test-row ok">
                  <CheckCircleFilled /> 跨域规则允许本页面直传
                </div>
              ) : (
                <div className="storage-test-row warn">
                  <WarningFilled />
                  <span>
                    {test.cors_error
                      ? `读不到跨域规则：${test.cors_error}`
                      : '桶的跨域（CORS）规则还不允许本页面直传文件，上传云盘文件会失败。'}
                  </span>
                  <Button size="small" onClick={fixCORS} loading={busy === 'cors'} disabled={!config?.has_secret}>
                    自动配置跨域
                  </Button>
                </div>
              ))}
            {test.ok && !test.cors_ready && !config?.has_secret && <span className="hint">先保存配置，再自动配置跨域。</span>}
          </div>
        )}

        {config?.active === 'oss' && localCount > 0 && (
          <>
            <Divider style={{ margin: '4px 0' }} />
            <Alert
              type="info"
              showIcon
              title={`还有 ${config.local_releases} 个更新包、${config.local_files} 个文件存在管理服务上`}
              description="迁移后从 OSS 下载，本机副本删除。文件大时需要一些时间，期间请不要关闭页面。"
              action={
                <Button size="small" icon={<CloudSyncOutlined />} onClick={migrate} loading={busy === 'migrate'}>
                  迁移到 OSS
                </Button>
              }
            />
          </>
        )}
      </Space>
    </Modal>
  )
}
