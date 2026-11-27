import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { App, Button, Drawer, Empty, Input, Progress, Skeleton, Tag, Tooltip } from 'antd'
import {
  CloseOutlined,
  DownloadOutlined,
  ExportOutlined,
  FolderOpenOutlined,
  PushpinFilled,
  ReadOutlined,
  ReloadOutlined,
  SearchOutlined,
} from '@ant-design/icons'
import { api, type FileDownload, type RemoteFile } from '../../api'
import { formatBytes, PageHeader } from '../common'
import FileIcon from '../files/FileIcon'
import Markdown from '../markdown/Markdown'

// 下载页（S1.11）：管理服务上传的文件，置顶的在前，按管理端的顺序。
// 下载由托盘程序在用户会话里保存到「下载」目录（后台服务以系统权限运行，不替用户写文件），这里显示进度。
// Markdown 文件可以直接阅读。

const active = (d?: FileDownload) => d?.status === 'pending' || d?.status === 'running'

export default function Downloads() {
  const { message } = App.useApp()
  const navigate = useNavigate()
  const [files, setFiles] = useState<RemoteFile[]>()
  const [bound, setBound] = useState<boolean>()
  const [error, setError] = useState('')
  const [downloads, setDownloads] = useState<FileDownload[]>([])
  const [query, setQuery] = useState('')
  const [reading, setReading] = useState<{ file: RemoteFile; content?: string; error?: string }>()

  const loadFiles = useCallback(
    () =>
      api.server
        .status()
        .then((s) => {
          setBound(s.bound)
          return s.bound ? api.files.list().then((list) => setFiles(list)) : undefined
        })
        .then(() => setError(''))
        .catch((e: Error) => setError(e.message)),
    [],
  )

  const [now, setNow] = useState(0)
  const loadDownloads = useCallback(
    () =>
      api.files.downloads().then((list) => {
        setDownloads(list)
        setNow(Date.now())
      }, () => undefined),
    [],
  )

  useEffect(() => {
    void loadFiles()
    void loadDownloads()
    const t = setInterval(loadFiles, 60000)
    return () => clearInterval(t)
  }, [loadFiles, loadDownloads])

  // 有进行中的下载时每秒刷新进度，否则 5 秒一次（托盘程序可能刚完成或失败）。
  const busy = downloads.some(active)
  useEffect(() => {
    const t = setInterval(loadDownloads, busy ? 1000 : 5000)
    return () => clearInterval(t)
  }, [busy, loadDownloads])

  // 每个文件最近一次下载。
  const latest = useMemo(() => {
    const m = new Map<number, FileDownload>()
    for (const d of downloads) if (!m.has(d.file_id)) m.set(d.file_id, d)
    return m
  }, [downloads])

  const act = (f: () => Promise<unknown>) =>
    f()
      .catch((e: Error) => message.error(e.message))
      .finally(loadDownloads)

  const read = (file: RemoteFile) => {
    setReading({ file })
    api.files.content(file.id).then(
      (r) => setReading({ file, content: r.content }),
      (e: Error) => setReading({ file, error: e.message }),
    )
  }

  const q = query.trim().toLowerCase()
  const shown = (files ?? []).filter(
    (f) => !q || f.name.toLowerCase().includes(q) || f.file_name.toLowerCase().includes(q) || f.description.toLowerCase().includes(q),
  )

  const action = (f: RemoteFile) => {
    const d = latest.get(f.id)
    if (active(d)) {
      const pct = d!.total > 0 ? Math.floor((d!.downloaded / d!.total) * 100) : 0
      const waiting = d!.status === 'pending' && now - new Date(d!.created_at).getTime() > 8000
      return (
        <div className="dl-progress">
          <Progress percent={pct} size="small" status="active" showInfo={false} />
          <span className="hint">
            {d!.status === 'pending' ? (waiting ? '等待托盘程序…请确认 Banbu Connect 托盘程序在运行' : '准备下载…') : `${formatBytes(d!.downloaded)} / ${formatBytes(d!.total)}`}
          </span>
          <Tooltip title="取消">
            <Button size="small" type="text" aria-label="取消下载" icon={<CloseOutlined />} onClick={() => act(() => api.files.cancel(d!.id))} />
          </Tooltip>
        </div>
      )
    }
    if (d?.status === 'done') {
      return (
        <div className="dl-actions">
          <Button size="small" icon={<ExportOutlined />} onClick={() => act(() => api.files.open(d.id))}>
            打开
          </Button>
          <Tooltip title="在文件夹中显示">
            <Button size="small" aria-label="在文件夹中显示" icon={<FolderOpenOutlined />} onClick={() => act(() => api.files.reveal(d.id))} />
          </Tooltip>
          <Tooltip title="重新下载">
            <Button size="small" type="text" aria-label="重新下载" icon={<DownloadOutlined />} onClick={() => act(() => api.files.download(f.id))} />
          </Tooltip>
        </div>
      )
    }
    return (
      <div className="dl-actions">
        {f.markdown && (
          <Button size="small" icon={<ReadOutlined />} onClick={() => read(f)}>
            阅读
          </Button>
        )}
        <Button size="small" type={f.markdown ? 'default' : 'primary'} icon={<DownloadOutlined />} onClick={() => act(() => api.files.download(f.id))}>
          下载
        </Button>
      </div>
    )
  }

  const status = (f: RemoteFile) => {
    const d = latest.get(f.id)
    if (d?.status === 'failed') return <span className="dl-status bad">下载失败：{d.error}</span>
    if (d?.status === 'done' && d.path)
      return (
        <span className="dl-status ok selectable" title={d.path}>
          已下载到 {d.path}
        </span>
      )
    return null
  }

  let body
  if (bound === false) {
    body = (
      <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有绑定管理服务，绑定后可以下载管理员提供的文件">
        <Button onClick={() => navigate('/advanced/server')}>去绑定</Button>
      </Empty>
    )
  } else if (error && !files) {
    body = (
      <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={`读取文件列表失败：${error}`}>
        <Button icon={<ReloadOutlined />} onClick={() => void loadFiles()}>
          重试
        </Button>
      </Empty>
    )
  } else if (!files) {
    body = <Skeleton active paragraph={{ rows: 6 }} />
  } else if (files.length === 0) {
    body = <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="管理员还没有提供文件" />
  } else if (shown.length === 0) {
    body = <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="没有找到匹配的文件" />
  } else {
    body = (
      <div className="dl-list">
        {shown.map((f) => (
          <div key={f.id} className={`dl-item${f.pinned ? ' pinned' : ''}`}>
            <FileIcon name={f.file_name} />
            <div className="dl-main">
              <div className="dl-title">
                <span className="dl-name" title={f.name}>
                  {f.name}
                </span>
                {f.pinned && (
                  <Tag color="gold" bordered={false} icon={<PushpinFilled />}>
                    置顶
                  </Tag>
                )}
              </div>
              <div className="dl-meta">
                <span title={f.file_name}>{f.file_name}</span>
                <span>{formatBytes(f.size)}</span>
                <span>{new Date(f.updated_at).toLocaleDateString()}</span>
              </div>
              {f.description && (
                <div className="dl-desc">
                  <Markdown source={f.description} compact />
                </div>
              )}
              {status(f)}
            </div>
            {action(f)}
          </div>
        ))}
      </div>
    )
  }

  return (
    <div className="desktop-stack">
      <PageHeader
        title="下载"
        count={files?.length}
        actions={
          <>
            {(files?.length ?? 0) > 5 && (
              <Input
                allowClear
                size="small"
                prefix={<SearchOutlined />}
                placeholder="搜索文件"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                style={{ width: 180 }}
              />
            )}
            <Tooltip title="刷新">
              <Button aria-label="刷新" icon={<ReloadOutlined />} onClick={() => void loadFiles()} />
            </Tooltip>
          </>
        }
      />
      {body}
      <Drawer
        title={reading?.file.name}
        open={!!reading}
        onClose={() => setReading(undefined)}
        size="large"
        destroyOnHidden
        className="markdown-reader"
        extra={
          reading && (
            <Button size="small" icon={<DownloadOutlined />} onClick={() => act(() => api.files.download(reading.file.id))}>
              下载
            </Button>
          )
        }
      >
        {reading?.error ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={reading.error} />
        ) : reading?.content === undefined ? (
          <Skeleton active paragraph={{ rows: 10 }} />
        ) : (
          <Markdown source={reading.content} />
        )}
      </Drawer>
    </div>
  )
}
