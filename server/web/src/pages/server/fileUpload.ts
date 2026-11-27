import { serverApi, type DriveFile, type SignedURL } from '../../serverApi'

// 云盘文件上传（S1.11）：向管理服务要上传计划，浏览器把文件切片后直接 PUT 到签名地址（OSS 或管理服务自己），
// 同时传 3 片，每片失败重试 2 次；全部传完后提交各片的 ETag，由管理服务合并。失败或取消时放弃整个上传。

export interface UploadOptions {
  replaceId?: number
  name?: string
  description?: string
  pinned?: boolean
  published?: boolean
  onProgress?: (loaded: number, total: number) => void
  signal?: AbortSignal
}

const CONCURRENCY = 3
const RETRIES = 2

export class UploadAbortedError extends Error {
  constructor() {
    super('已取消上传')
  }
}

function putPart(target: SignedURL, body: Blob, onLoaded: (n: number) => void, signal?: AbortSignal): Promise<string> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('PUT', target.url)
    for (const [k, v] of Object.entries(target.headers ?? {})) {
      // 浏览器自己设置的头不能也不必再设。
      if (!['host', 'content-length'].includes(k.toLowerCase())) xhr.setRequestHeader(k, v)
    }
    xhr.upload.onprogress = (e) => onLoaded(e.loaded)
    xhr.onload = () => {
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(new Error(`分片上传失败（HTTP ${xhr.status}）${xhr.responseText ? '：' + xhr.responseText.slice(0, 200) : ''}`))
        return
      }
      const etag = xhr.getResponseHeader('ETag')
      if (!etag) {
        reject(new Error('读不到分片的 ETag：OSS 桶的跨域（CORS）规则没有暴露 ETag，请在「存储设置」里点「自动配置跨域」'))
        return
      }
      resolve(etag)
    }
    xhr.onerror = () =>
      reject(new Error('分片上传失败：网络错误。存在 OSS 时多半是桶的跨域（CORS）规则不允许本页面，请在「存储设置」里检查'))
    const abort = () => xhr.abort()
    xhr.onabort = () => reject(new UploadAbortedError())
    signal?.addEventListener('abort', abort, { once: true })
    xhr.send(body)
  })
}

export async function uploadDriveFile(file: File, opts: UploadOptions = {}): Promise<DriveFile> {
  const plan = await serverApi.startUpload(file.name, file.size, opts.replaceId ?? 0)
  const loaded = new Array<number>(plan.parts.length).fill(0)
  const report = () => opts.onProgress?.(Math.min(file.size, loaded.reduce((a, b) => a + b, 0)), file.size)
  const etags = new Array<string>(plan.parts.length)
  // 一片彻底失败或用户取消时，停下其他正在传的分片。
  const ctl = new AbortController()
  opts.signal?.addEventListener('abort', () => ctl.abort(), { once: true })
  let next = 0
  const worker = async () => {
    while (next < plan.parts.length) {
      const i = next++
      const start = i * plan.part_size
      const blob = file.slice(start, Math.min(start + plan.part_size, file.size))
      for (let attempt = 0; ; attempt++) {
        if (ctl.signal.aborted) throw new UploadAbortedError()
        try {
          etags[i] = await putPart(plan.parts[i], blob, (n) => {
            loaded[i] = n
            report()
          }, ctl.signal)
          loaded[i] = blob.size
          report()
          break
        } catch (e) {
          loaded[i] = 0
          if (e instanceof UploadAbortedError || attempt >= RETRIES) {
            ctl.abort()
            throw e
          }
        }
      }
    }
  }
  try {
    // 先等所有分片停下，报第一个真正的错误（不是被连带取消的）。
    const results = await Promise.allSettled(Array.from({ length: Math.min(CONCURRENCY, plan.parts.length) }, worker))
    const failed = results.filter((r): r is PromiseRejectedResult => r.status === 'rejected').map((r) => r.reason as Error)
    if (failed.length) throw opts.signal?.aborted ? new UploadAbortedError() : (failed.find((e) => !(e instanceof UploadAbortedError)) ?? failed[0])
    return await serverApi.completeUpload(plan.id, {
      parts: etags.map((etag, i) => ({ number: i + 1, etag })),
      name: opts.name,
      description: opts.description,
      pinned: opts.pinned,
      published: opts.published,
    })
  } catch (e) {
    void serverApi.abortUpload(plan.id).catch(() => undefined)
    throw e
  }
}
