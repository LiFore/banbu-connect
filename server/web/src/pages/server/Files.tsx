import { useCallback, useEffect, useRef, useState } from 'react'
import { App, Button, Dropdown, Empty, Form, Input, Modal, Progress, Segmented, Skeleton, Space, Switch, Tag, Tooltip, Upload } from 'antd'
import {
  ArrowDownOutlined,
  ArrowUpOutlined,
  CloudServerOutlined,
  DatabaseOutlined,
  DeleteOutlined,
  DownloadOutlined,
  EditOutlined,
  HolderOutlined,
  InboxOutlined,
  MoreOutlined,
  PushpinFilled,
  PushpinOutlined,
  ReadOutlined,
  SwapOutlined,
  UploadOutlined,
} from '@ant-design/icons'
import { serverApi, type DriveFile, type StorageConfig } from '../../serverApi'
import { formatBytes, PageHeader, RowAction } from '../common'
import Markdown from '../markdown/Markdown'
import FileIcon from '../files/FileIcon'
import StorageSettings from './StorageSettings'
import { UploadAbortedError, uploadDriveFile } from './fileUpload'

// 文件（云盘，S1.11）：上传文件给所有客户端下载。客户端「下载」页按这里的顺序显示：置顶的在前，组内可拖拽排序。
// 说明用简短的 Markdown；长内容上传 .md 文件，客户端可以直接阅读。

const MAX_DESC = 2000

export default function Files() {
  const { message, modal } = App.useApp()
  const [files, setFiles] = useState<DriveFile[]>([])
  const [loading, setLoading] = useState(true)
  const [storage, setStorage] = useState<StorageConfig>()
  const [storageOpen, setStorageOpen] = useState(false)
  const [upload, setUpload] = useState<{ replace?: DriveFile }>()
  const [edit, setEdit] = useState<DriveFile>()
  const [reading, setReading] = useState<{ file: DriveFile; content?: string }>()
  const [drag, setDrag] = useState<{ id: number; over?: number }>()

  const reload = useCallback(
    () =>
      serverApi
        .files()
        .then(setFiles)
        .catch((e: Error) => message.error(e.message))
        .finally(() => setLoading(false)),
    [message],
  )

  useEffect(() => {
    void reload()
    serverApi.storage().then(setStorage, () => undefined)
  }, [reload])

  const act = (f: () => Promise<unknown>, ok?: string) =>
    f()
      .then(() => {
        if (ok) void message.success(ok)
      })
      .catch((e: Error) => message.error(e.message))
      .finally(reload)

  const pinned = files.filter((f) => f.pinned)
  const others = files.filter((f) => !f.pinned)

  // 在同一分组内把 id 移到 target 的位置，提交这个分组的完整顺序。
  const move = (id: number, target: number) => {
    const group = files.find((f) => f.id === id)?.pinned ? pinned : others
    const from = group.findIndex((f) => f.id === id)
    const to = group.findIndex((f) => f.id === target)
    if (from < 0 || to < 0 || from === to) return
    const next = [...group]
    const [it] = next.splice(from, 1)
    next.splice(to, 0, it)
    setFiles(group === pinned ? [...next, ...others] : [...pinned, ...next]) // 先在页面上换好，再提交
    void act(() => serverApi.reorderFiles(next.map((f) => f.id)))
  }
  const step = (f: DriveFile, d: -1 | 1) => {
    const group = f.pinned ? pinned : others
    const i = group.findIndex((x) => x.id === f.id)
    const t = group[i + d]
    if (t) move(f.id, t.id)
  }

  const openReader = (f: DriveFile) => {
    setReading({ file: f })
    serverApi.fileContent(f.id).then(
      (r) => setReading({ file: f, content: r.content }),
      (e: Error) => {
        message.error(e.message)
        setReading(undefined)
      },
    )
  }

  const row = (f: DriveFile, group: DriveFile[]) => {
    const i = group.indexOf(f)
    const dragging = drag?.id === f.id
    const over = drag && drag.over === f.id && drag.id !== f.id
    return (
      <div
        key={f.id}
        className={`drive-row${f.published ? '' : ' unpublished'}${dragging ? ' dragging' : ''}${over ? ' drop-target' : ''}`}
        onDragOver={(e) => {
          if (!drag || group.every((x) => x.id !== drag.id)) return // 只能在同一分组内拖
          e.preventDefault()
          if (drag.over !== f.id) setDrag({ ...drag, over: f.id })
        }}
        onDrop={(e) => {
          e.preventDefault()
          if (drag) move(drag.id, f.id)
          setDrag(undefined)
        }}
      >
        <span
          className="drive-handle"
          draggable
          title="拖动排序"
          aria-label="拖动排序"
          onDragStart={(e) => {
            e.dataTransfer.effectAllowed = 'move'
            e.dataTransfer.setData('text/plain', String(f.id))
            setDrag({ id: f.id })
          }}
          onDragEnd={() => setDrag(undefined)}
        >
          <HolderOutlined />
        </span>
        <FileIcon name={f.file_name} />
        <div className="drive-main">
          <div className="drive-title">
            <span className="drive-name selectable">{f.name}</span>
            {f.pinned && (
              <Tag color="gold" bordered={false} icon={<PushpinFilled />}>
                置顶
              </Tag>
            )}
            {!f.published && <Tag bordered={false}>未发布</Tag>}
            {f.markdown && <Tag bordered={false}>Markdown</Tag>}
          </div>
          <div className="drive-meta">
            <span className="selectable" title={f.file_name}>
              {f.file_name}
            </span>
            <span>{formatBytes(f.size)}</span>
            <span>更新于 {new Date(f.updated_at).toLocaleString()}</span>
            <span>{f.storage === 'oss' ? 'OSS' : '管理服务'}</span>
          </div>
          {f.description && (
            <div className="drive-desc">
              <Markdown source={f.description} compact />
            </div>
          )}
        </div>
        <div className="drive-actions">
          <Switch
            size="small"
            aria-label={`发布 ${f.name}`}
            checkedChildren="已发布"
            unCheckedChildren="未发布"
            checked={f.published}
            onChange={(on) => act(() => serverApi.updateFile(f.id, { published: on }))}
          />
          <RowAction
            title={f.pinned ? '取消置顶' : '置顶'}
            icon={f.pinned ? <PushpinFilled /> : <PushpinOutlined />}
            onClick={() => act(() => serverApi.updateFile(f.id, { pinned: !f.pinned }))}
          />
          <RowAction title="编辑" icon={<EditOutlined />} onClick={() => setEdit(f)} />
          {f.markdown && <RowAction title="阅读" icon={<ReadOutlined />} onClick={() => openReader(f)} />}
          <Tooltip title="下载">
            <Button size="small" type="text" aria-label="下载" icon={<DownloadOutlined />} href={serverApi.fileDownloadURL(f.id)} />
          </Tooltip>
          <Dropdown
            trigger={['click']}
            menu={{
              items: [
                { key: 'up', icon: <ArrowUpOutlined />, label: '上移', disabled: i <= 0 },
                { key: 'down', icon: <ArrowDownOutlined />, label: '下移', disabled: i >= group.length - 1 },
                { key: 'replace', icon: <SwapOutlined />, label: '替换文件' },
                { type: 'divider' },
                { key: 'delete', icon: <DeleteOutlined />, label: '删除', danger: true },
              ],
              onClick: ({ key }) => {
                if (key === 'up') step(f, -1)
                if (key === 'down') step(f, 1)
                if (key === 'replace') setUpload({ replace: f })
                if (key === 'delete')
                  modal.confirm({
                    title: `删除「${f.name}」？`,
                    content: '文件会从存储里删除，客户端将看不到它。',
                    okText: '删除',
                    okButtonProps: { danger: true },
                    cancelText: '取消',
                    onOk: () => act(() => serverApi.deleteFile(f.id), '已删除'),
                  })
              },
            }}
          >
            <Button size="small" type="text" aria-label="更多操作" icon={<MoreOutlined />} />
          </Dropdown>
        </div>
      </div>
    )
  }

  return (
    <Space orientation="vertical" size={16} style={{ width: '100%' }}>
      <PageHeader
        title="文件"
        count={files.length}
        actions={
          <>
            <Button icon={<DatabaseOutlined />} onClick={() => setStorageOpen(true)}>
              存储设置
            </Button>
            <Button type="primary" icon={<UploadOutlined />} onClick={() => setUpload({})}>
              上传文件
            </Button>
          </>
        }
      />
      {storage && (
        <div className={`drive-storage${storage.active === 'oss' ? ' oss' : ''}`}>
          {storage.active === 'oss' ? <DatabaseOutlined /> : <CloudServerOutlined />}
          <span>
            {storage.active === 'oss'
              ? `新文件存到阿里云 OSS（${storage.bucket} · ${storage.region}），客户端直接从 OSS 下载`
              : '新文件存在管理服务的数据目录，客户端经管理服务下载。配置阿里云 OSS 后可以减轻服务器带宽。'}
          </span>
          <Button size="small" type="link" onClick={() => setStorageOpen(true)}>
            {storage.active === 'oss' ? '查看' : '配置 OSS'}
          </Button>
        </div>
      )}

      {loading ? (
        <Skeleton active paragraph={{ rows: 6 }} />
      ) : files.length === 0 ? (
        <Empty description="还没有文件。上传后，已发布的文件会出现在所有客户端的「下载」页。" image={Empty.PRESENTED_IMAGE_SIMPLE}>
          <Button type="primary" icon={<UploadOutlined />} onClick={() => setUpload({})}>
            上传文件
          </Button>
        </Empty>
      ) : (
        <div className="drive-list">
          {pinned.length > 0 && <div className="drive-group-label">置顶</div>}
          {pinned.map((f) => row(f, pinned))}
          {pinned.length > 0 && others.length > 0 && <div className="drive-group-label">其他文件</div>}
          {others.map((f) => row(f, others))}
          <div className="hint">拖动左侧的把手调整顺序（置顶和其他文件各自排序）。客户端按同样的顺序显示。</div>
        </div>
      )}

      {/* 弹窗按需挂载：每次打开都是干净的状态 */}
      {storageOpen && <StorageSettings open onClose={() => setStorageOpen(false)} onSaved={setStorage} />}
      {upload && (
        <UploadModal
          open
          replace={upload.replace}
          onClose={() => setUpload(undefined)}
          onDone={() => {
            setUpload(undefined)
            void reload()
          }}
        />
      )}
      <EditModal
        file={edit}
        onClose={() => setEdit(undefined)}
        onSaved={() => {
          setEdit(undefined)
          void reload()
        }}
      />
      <Modal
        title={reading?.file.name}
        open={!!reading}
        onCancel={() => setReading(undefined)}
        footer={null}
        width={820}
        destroyOnHidden
        className="markdown-reader-modal"
      >
        {reading?.content === undefined ? <Skeleton active paragraph={{ rows: 8 }} /> : <Markdown source={reading.content} />}
      </Modal>
    </Space>
  )
}

// 说明编辑：写 / 预览切换，字数限制。
function DescriptionField({ value, onChange }: { value?: string; onChange?: (v: string) => void }) {
  const [mode, setMode] = useState<'edit' | 'preview'>('edit')
  return (
    <div className="desc-field">
      <Segmented
        size="small"
        value={mode}
        onChange={(v) => setMode(v as typeof mode)}
        options={[
          { value: 'edit', label: '编辑' },
          { value: 'preview', label: '预览' },
        ]}
      />
      {mode === 'edit' ? (
        <Input.TextArea
          value={value}
          onChange={(e) => onChange?.(e.target.value)}
          autoSize={{ minRows: 3, maxRows: 10 }}
          maxLength={MAX_DESC}
          showCount
          placeholder="简短说明，支持 Markdown：**加粗**、[链接](https://…)、列表等。长内容请上传 .md 文件。"
        />
      ) : (
        <div className="desc-preview">{value?.trim() ? <Markdown source={value} compact /> : <span className="hint">没有说明</span>}</div>
      )}
    </div>
  )
}

interface UploadValues {
  name: string
  description: string
  pinned: boolean
  published: boolean
}

function UploadModal({ open, replace, onClose, onDone }: { open: boolean; replace?: DriveFile; onClose: () => void; onDone: () => void }) {
  const { message } = App.useApp()
  const [form] = Form.useForm<UploadValues>()
  const [file, setFile] = useState<File>()
  const [progress, setProgress] = useState<{ loaded: number; total: number; speed: number }>()
  const abort = useRef<AbortController>(undefined)

  const start = async () => {
    if (!file) {
      message.error('请选择文件')
      return
    }
    const v = replace ? ({} as UploadValues) : await form.validateFields()
    const ctl = new AbortController()
    abort.current = ctl
    const began = Date.now()
    setProgress({ loaded: 0, total: file.size, speed: 0 })
    try {
      await uploadDriveFile(file, {
        replaceId: replace?.id,
        name: v.name?.trim() || file.name,
        description: v.description ?? '',
        pinned: v.pinned,
        published: v.published,
        signal: ctl.signal,
        onProgress: (loaded, total) => setProgress({ loaded, total, speed: loaded / Math.max(0.5, (Date.now() - began) / 1000) }),
      })
      message.success(replace ? '已替换文件' : '已上传')
      onDone()
    } catch (e) {
      if (!(e instanceof UploadAbortedError)) message.error((e as Error).message)
      setProgress(undefined)
    } finally {
      abort.current = undefined
    }
  }

  const uploading = !!progress
  const close = () => {
    abort.current?.abort()
    onClose()
  }

  return (
    <Modal
      title={replace ? `替换「${replace.name}」的文件` : '上传文件'}
      open={open}
      onCancel={close}
      maskClosable={!uploading}
      width={620}
      destroyOnHidden
      footer={
        <Space>
          <Button onClick={close}>{uploading ? '取消上传' : '取消'}</Button>
          <Button type="primary" icon={<UploadOutlined />} onClick={start} loading={uploading} disabled={!file}>
            {replace ? '上传并替换' : '上传'}
          </Button>
        </Space>
      }
    >
      <Space orientation="vertical" size={12} style={{ width: '100%' }}>
        <Upload.Dragger
          multiple={false}
          showUploadList={false}
          disabled={uploading}
          beforeUpload={(f) => {
            setFile(f)
            if (!replace && !form.getFieldValue('name')) form.setFieldValue('name', f.name.replace(/\.(md|markdown)$/i, ''))
            return false
          }}
        >
          <p className="ant-upload-drag-icon">
            <InboxOutlined />
          </p>
          {file ? (
            <p className="ant-upload-text">
              {file.name}（{formatBytes(file.size)}）
            </p>
          ) : (
            <p className="ant-upload-text">点击或拖入文件</p>
          )}
          <p className="ant-upload-hint">不限大小，大文件分片直传。.md 文件客户端可以直接阅读。</p>
        </Upload.Dragger>
        {replace && <span className="hint">替换后保留标题、说明、置顶和顺序，旧文件删除。</span>}
        {!replace && (
          <Form form={form} layout="vertical" requiredMark={false} initialValues={{ pinned: false, published: true, description: '' }}>
            <Form.Item name="name" label="标题" rules={[{ required: true, message: '填写标题' }, { max: 200 }]}>
              <Input placeholder="客户端显示的名称" />
            </Form.Item>
            <Form.Item name="description" label="说明（可选）">
              <DescriptionField />
            </Form.Item>
            <Space size={24}>
              <Form.Item name="published" label="发布" valuePropName="checked" tooltip="发布后客户端才看得到" style={{ marginBottom: 0 }}>
                <Switch />
              </Form.Item>
              <Form.Item name="pinned" label="置顶" valuePropName="checked" style={{ marginBottom: 0 }}>
                <Switch />
              </Form.Item>
            </Space>
          </Form>
        )}
        {progress && (
          <div>
            <Progress percent={Math.floor((progress.loaded / Math.max(1, progress.total)) * 100)} status="active" />
            <span className="hint">
              {formatBytes(progress.loaded)} / {formatBytes(progress.total)} · {formatBytes(progress.speed)}/s
            </span>
          </div>
        )}
      </Space>
    </Modal>
  )
}

function EditModal({ file, onClose, onSaved }: { file?: DriveFile; onClose: () => void; onSaved: () => void }) {
  const { message } = App.useApp()
  const [form] = Form.useForm<{ name: string; file_name: string; description: string }>()
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (file) form.setFieldsValue({ name: file.name, file_name: file.file_name, description: file.description })
  }, [file, form])

  const save = async () => {
    if (!file) return
    const v = await form.validateFields()
    setSaving(true)
    try {
      await serverApi.updateFile(file.id, v)
      message.success('已保存')
      onSaved()
    } catch (e) {
      message.error((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal title="编辑文件" open={!!file} onCancel={onClose} onOk={save} okText="保存" cancelText="取消" confirmLoading={saving} width={620} destroyOnHidden>
      <Form form={form} layout="vertical" requiredMark={false}>
        <Form.Item name="name" label="标题" rules={[{ required: true, message: '填写标题' }, { max: 200 }]}>
          <Input />
        </Form.Item>
        <Form.Item name="file_name" label="下载文件名" rules={[{ required: true, message: '填写文件名' }]} tooltip="客户端下载后保存的文件名">
          <Input />
        </Form.Item>
        <Form.Item name="description" label="说明">
          <DescriptionField />
        </Form.Item>
      </Form>
    </Modal>
  )
}
